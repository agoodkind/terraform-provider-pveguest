// Package transport runs commands inside Proxmox guests through the Proxmox VE
// API.
package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Kind selects the API that runs commands in a guest.
type Kind string

const (
	// KindLXC is a container. The exec API of the container runs commands as
	// root.
	KindLXC Kind = "lxc"

	// KindQEMU is a VM. The QEMU guest agent API runs commands, which requires
	// a running guest agent in the VM.
	KindQEMU Kind = "qemu"
)

const (
	defaultTimeoutSeconds = 120

	// The container exec API accepts at most 3600 seconds.
	maximumTimeoutSeconds = 3600

	// The container exec API accepts input-data of at most 131072 base64
	// characters, which is 98304 raw bytes.
	lxcStdinLimit = 96 * 1024

	// The exec call returns at once, and the guest command can run for its
	// timeout. The poll ends this long after the timeout.
	pollTimeoutMargin = 15 * time.Second

	pollInitialDelay = 200 * time.Millisecond
	pollMaximumDelay = 2 * time.Second
)

// Guest identifies one container or VM by the node that runs it, its Proxmox
// ID, and its kind.
type Guest struct {
	Node string
	VMID int64
	Kind Kind
}

// String formats the guest identity for error messages.
func (guest Guest) String() string {
	return fmt.Sprintf("guest node=%q vmid=%d kind=%s", guest.Node, guest.VMID, guest.Kind)
}

// apiPath returns the API path of an operation on the guest. The path uses the
// Proxmox node name, which can differ from the node key of the guest.
func (guest Guest) apiPath(nodeName string, operation string) string {
	node := url.PathEscape(nodeName)
	vmid := strconv.FormatInt(guest.VMID, 10)
	return "/api2/json/nodes/" + node + "/" + string(guest.Kind) + "/" + vmid + "/" + operation
}

// StdinLimit returns the largest standard input in bytes that one command
// accepts for the kind.
func (kind Kind) StdinLimit() int {
	return lxcStdinLimit
}

// SplitStdin cuts content into pieces that one command accepts as standard
// input.
func SplitStdin(kind Kind, content []byte) [][]byte {
	limit := kind.StdinLimit()
	var pieces [][]byte
	remaining := content
	for len(remaining) > limit {
		pieces = append(pieces, remaining[:limit])
		remaining = remaining[limit:]
	}
	return append(pieces, remaining)
}

// Command is one program run inside a guest. Argv[0] is the program, Stdin is
// the complete standard input, and a zero TimeoutSeconds selects the default
// timeout. Stdin must not exceed the StdinLimit of the guest kind.
type Command struct {
	Argv           []string
	Stdin          []byte
	TimeoutSeconds int
}

// UnreachableError reports that the API could not run a command in the
// guest: a stopped container, a stopped VM, or a VM without a running guest
// agent.
type UnreachableError struct {
	guest  Guest
	detail string
}

// Error returns a message with the guest and the detail from the Proxmox API.
func (unreachable *UnreachableError) Error() string {
	return fmt.Sprintf("%s is unreachable: %s", unreachable.guest, unreachable.detail)
}

var unreachablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`container '\d+' not running!`),
	regexp.MustCompile(`Configuration file '[^']*' does not exist`),
	regexp.MustCompile(`VM \d+ is not running`),
	regexp.MustCompile(`QEMU guest agent is not running`),
	regexp.MustCompile(`No QEMU guest agent configured`),
}

type execStartBody struct {
	Command   []string `json:"command"`
	InputData *string  `json:"input-data,omitempty"`
	Timeout   *int     `json:"timeout,omitempty"`
	Retention string   `json:"result-retention"`
}

type execStartResponse struct {
	PID int64 `json:"pid"`
}

type execAckBody struct {
	PID int64 `json:"pid"`
}

type resultReadError struct {
	cause error
}

func (failure *resultReadError) Error() string {
	return failure.cause.Error()
}

func (failure *resultReadError) Unwrap() error {
	return failure.cause
}

// flag reads a boolean that the API prints as 1, 0, true, or false.
type flag bool

func (value *flag) UnmarshalJSON(document []byte) error {
	text := string(document)
	parsed, err := strconv.ParseBool(text)
	if err != nil {
		return errors.New("exec-status contains an invalid boolean")
	}
	*value = flag(parsed)
	return nil
}

type execStatus struct {
	Exited       *flag   `json:"exited"`
	ExitCode     *int    `json:"exitcode"`
	OutData      *string `json:"out-data"`
	ErrData      *string `json:"err-data"`
	OutTruncated flag    `json:"out-truncated"`
	ErrTruncated flag    `json:"err-truncated"`
	TimedOut     flag    `json:"timed-out"`
}

// Run starts the command in the guest, polls until it exits, and returns its
// output and exit status. A nonzero exit status is part of the Result. Each
// command reserves an exec slot before the POST exec request and releases the
// slot after the command exits or fails. The slot covers the POST exec request
// and every exec-status poll.
func (client *Client) Run(ctx context.Context, guest Guest, command Command) (Result, error) {
	return client.run(ctx, guest, command)
}

func (client *Client) run(ctx context.Context, guest Guest, command Command) (Result, error) {
	connection, timeout, err := client.prepare(guest, command)
	if err != nil {
		return Result{}, err
	}

	err = connection.acquireExecSlot(ctx, guest)
	if err != nil {
		return Result{}, err
	}
	defer connection.releaseExecSlot()

	processID, err := connection.startExec(ctx, guest, command, timeout)
	if err != nil {
		return Result{}, err
	}
	status, err := connection.waitForExit(ctx, guest, command, processID, timeout)
	if err != nil {
		return Result{}, err
	}
	result, err := decodeExecResult(guest, status)
	if err != nil {
		return Result{}, err
	}
	if err := connection.acknowledgeExec(ctx, guest, processID); err != nil {
		failureClass := "api"
		if isConnectionClosed(err) {
			failureClass = "connection"
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failureClass = "context"
		}
		slog.WarnContext(ctx, "command result acknowledgment failed",
			"guest", guest.String(), "pid", processID, "class", failureClass)
	}
	if err := resultError(guest, command, status, timeout); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (client *Client) prepare(guest Guest, command Command) (*nodeConnection, int, error) {
	if len(command.Argv) == 0 {
		return nil, 0, fmt.Errorf("%s: empty command", guest)
	}
	if guest.Kind != KindLXC && guest.Kind != KindQEMU {
		return nil, 0, fmt.Errorf("%s: unknown kind", guest)
	}
	connection, found := client.nodes[guest.Node]
	if !found {
		return nil, 0, fmt.Errorf("%s: node is not in the provider nodes map", guest)
	}
	timeout := defaultTimeoutSeconds
	if command.TimeoutSeconds > 0 {
		timeout = command.TimeoutSeconds
	}
	if timeout > maximumTimeoutSeconds {
		return nil, 0, fmt.Errorf(
			"%s: timeout of %d seconds exceeds the limit of %d seconds",
			guest, timeout, maximumTimeoutSeconds,
		)
	}
	limit := guest.Kind.StdinLimit()
	if len(command.Stdin) > limit {
		return nil, 0, fmt.Errorf(
			"%s: stdin of %d bytes exceeds the limit of %d bytes for one command",
			guest, len(command.Stdin), limit,
		)
	}
	return connection, timeout, nil
}

func (connection *nodeConnection) startExec(
	ctx context.Context,
	guest Guest,
	command Command,
	timeout int,
) (int64, error) {
	body := execStartBody{Command: command.Argv, InputData: nil, Timeout: &timeout, Retention: "acknowledged"}
	if len(command.Stdin) > 0 {
		input := base64.StdEncoding.EncodeToString(command.Stdin)
		body.InputData = &input
	}

	document, err := encodeJSON(&body)
	if err != nil {
		return 0, fmt.Errorf("%s: POST exec: %w", guest, err)
	}
	data, err := connection.call(ctx, guest, apiRequest{
		method:    http.MethodPost,
		operation: execStartOperation,
		query:     nil,
		body:      document,
	})
	if err != nil {
		return 0, err
	}
	var started execStartResponse
	if err := json.Unmarshal(data, &started); err != nil {
		slog.ErrorContext(ctx, "exec response has no pid", "guest", guest.String(), "err", err)
		return 0, fmt.Errorf("%s: exec response has no pid: %w", guest, err)
	}
	if started.PID <= 0 {
		return 0, fmt.Errorf("%s: exec response has an invalid pid; guest command may have started", guest)
	}
	return started.PID, nil
}

// waitForExit polls exec-status with a delay that doubles from 200 ms to 2 s.
// Completed results remain available until acknowledgment or expiry.
func (connection *nodeConnection) waitForExit(
	ctx context.Context,
	guest Guest,
	command Command,
	processID int64,
	timeout int,
) (execStatus, error) {
	waitContext, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+pollTimeoutMargin)
	defer cancel()

	query := url.Values{"pid": {strconv.FormatInt(processID, 10)}}
	delay := pollInitialDelay
	for {
		data, err := connection.readExecStatus(waitContext, guest, apiRequest{
			method:    http.MethodGet,
			operation: "exec-status",
			query:     query,
			body:      nil,
		})
		if err != nil {
			return execStatus{}, err
		}
		var status execStatus
		if err := json.Unmarshal(data, &status); err != nil {
			slog.ErrorContext(ctx, "exec-status response is malformed", "guest", guest.String(), "err", err)
			return execStatus{}, fmt.Errorf("%s: parse exec-status response: %w", guest, err)
		}
		if status.Exited == nil {
			return execStatus{}, fmt.Errorf("%s: exec-status response omits the exited flag", guest)
		}
		if *status.Exited {
			return status, nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-waitContext.Done():
			timer.Stop()
			slog.ErrorContext(ctx, "wait for command exit ended", "guest", guest.String(), "err", waitContext.Err())
			return execStatus{}, fmt.Errorf(
				"%s: command %s did not exit before the wait ended: %w",
				guest, QuoteCommand(command.Argv), waitContext.Err(),
			)
		case <-timer.C:
		}
		delay = min(delay*2, pollMaximumDelay)
	}
}

func (connection *nodeConnection) readExecStatus(
	ctx context.Context,
	guest Guest,
	request apiRequest,
) (json.RawMessage, error) {
	request.retryAtCaller = true
	delay := retryInitialDelay
	for attempt := 1; ; attempt++ {
		data, err := connection.call(ctx, guest, request)
		if err == nil {
			return data, nil
		}
		var closed *closedConnectionError
		if !errors.As(err, &closed) {
			return nil, err
		}
		if attempt >= maxReadAttempts {
			return nil, &resultReadError{cause: fmt.Errorf("%s: command result read failed after %d attempts; pid %s requires another status read: %w",
				guest, attempt, request.query.Get("pid"), err)}
		}
		slog.WarnContext(ctx, "retry command result read after a closed connection",
			"guest", guest.String(), "attempt", attempt)
		if err := waitReadCommandRetry(ctx, delay); err != nil {
			return nil, err
		}
		delay *= retryDelayFactor
	}
}

func (connection *nodeConnection) acknowledgeExec(ctx context.Context, guest Guest, processID int64) error {
	document, err := encodeJSON(&execAckBody{PID: processID})
	if err != nil {
		return err
	}
	_, err = connection.call(ctx, guest, apiRequest{
		method: http.MethodPost, operation: "exec-ack", body: document,
	})
	return err
}

func resultError(guest Guest, command Command, status execStatus, timeout int) error {
	quoted := QuoteCommand(command.Argv)
	if status.TimedOut {
		return fmt.Errorf("%s: command %s timed out after %d seconds", guest, quoted, timeout)
	}
	if status.OutTruncated || status.ErrTruncated {
		return fmt.Errorf("%s: the output of command %s exceeded the capture limit", guest, quoted)
	}
	return nil
}

func decodeExecResult(guest Guest, status execStatus) (Result, error) {
	if status.ExitCode == nil {
		return Result{}, fmt.Errorf("%s: command exited without an exit code", guest)
	}
	if status.OutData == nil || status.ErrData == nil {
		return Result{}, fmt.Errorf("%s: completed command result omits an output stream", guest)
	}

	stdout, err := decodeStream(guest, *status.OutData)
	if err != nil {
		return Result{}, err
	}
	stderr, err := decodeStream(guest, *status.ErrData)
	if err != nil {
		return Result{}, err
	}
	return Result{Stdout: stdout, Stderr: stderr, ExitCode: *status.ExitCode}, nil
}

func decodeStream(guest Guest, data string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		slog.Error("exec-status stream is not base64", "guest", guest.String(), "err", err)
		return nil, fmt.Errorf("%s: decode exec-status stream: %w", guest, err)
	}
	return decoded, nil
}
