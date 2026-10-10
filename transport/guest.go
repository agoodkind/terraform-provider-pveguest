// Package transport runs commands inside Proxmox guests through the Proxmox VE
// API.
package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"
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

	// The QEMU guest agent API accepts input-data of at most 65536 characters
	// in a request body of at most 64 KiB. The limit leaves room for JSON
	// escapes.
	qemuStdinLimit = 16 * 1024

	// The exec call returns at once, and the guest command can run for its
	// timeout. The poll ends this long after the timeout.
	pollTimeoutMargin = 15 * time.Second

	pollInitialDelay = 200 * time.Millisecond
	pollMaximumDelay = 2 * time.Second

	// A command killed by a signal has the exit status 128 plus the signal.
	signalExitCodeBase = 128
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
// Proxmox node name, which can differ from the node key of the guest. The QEMU
// guest agent operations live under agent.
func (guest Guest) apiPath(nodeName string, operation string) string {
	node := url.PathEscape(nodeName)
	vmid := strconv.FormatInt(guest.VMID, 10)
	if guest.Kind == KindQEMU {
		return "/api2/json/nodes/" + node + "/qemu/" + vmid + "/agent/" + operation
	}
	return "/api2/json/nodes/" + node + "/lxc/" + vmid + "/" + operation
}

// StdinLimit returns the largest standard input in bytes that one command
// accepts for the kind.
func (kind Kind) StdinLimit() int {
	if kind == KindQEMU {
		return qemuStdinLimit
	}
	return lxcStdinLimit
}

// SplitStdin cuts content into pieces that one command accepts as standard
// input. The pieces of QEMU content end at character boundaries, because the
// guest agent API takes text.
func SplitStdin(kind Kind, content []byte) [][]byte {
	limit := kind.StdinLimit()
	var pieces [][]byte
	remaining := content
	for len(remaining) > limit {
		end := limit
		if kind == KindQEMU {
			for end > 0 && !utf8.RuneStart(remaining[end]) {
				end--
			}
		}
		pieces = append(pieces, remaining[:end])
		remaining = remaining[end:]
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
}

type execStartResponse struct {
	PID int64 `json:"pid"`
}

// flag reads a boolean that the API prints as 1, 0, true, or false.
type flag bool

func (value *flag) UnmarshalJSON(document []byte) error {
	text := string(document)
	if text == "null" {
		return nil
	}
	parsed, err := strconv.ParseBool(text)
	if err != nil {
		return fmt.Errorf("unexpected boolean value %s: %w", text, err)
	}
	*value = flag(parsed)
	return nil
}

type execStatus struct {
	Exited       flag   `json:"exited"`
	ExitCode     *int   `json:"exitcode"`
	Signal       *int   `json:"signal"`
	OutData      string `json:"out-data"`
	ErrData      string `json:"err-data"`
	OutTruncated flag   `json:"out-truncated"`
	ErrTruncated flag   `json:"err-truncated"`
	TimedOut     flag   `json:"timed-out"`
}

// exitCode returns the exit code, or 128 plus the signal number for a command
// that a signal ended.
func (status execStatus) exitCode() (int, bool) {
	if status.ExitCode != nil {
		return *status.ExitCode, true
	}
	if status.Signal != nil {
		return signalExitCodeBase + *status.Signal, true
	}
	return 0, false
}

// Run starts the command in the guest, polls until it exits, and returns its
// output and exit status. A nonzero exit status is part of the Result.
func (client *Client) Run(ctx context.Context, guest Guest, command Command) (Result, error) {
	connection, timeout, err := client.prepare(guest, command)
	if err != nil {
		return Result{}, err
	}

	processID, err := connection.startExec(ctx, guest, command, timeout)
	if err != nil {
		return Result{}, err
	}
	status, err := connection.waitForExit(ctx, guest, command, processID, timeout)
	if err != nil {
		return Result{}, err
	}
	return resultOf(guest, command, status, timeout)
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
	if guest.Kind == KindQEMU && !utf8.Valid(command.Stdin) {
		return nil, 0, fmt.Errorf("%s: stdin is not valid UTF-8, which the QEMU guest agent API requires", guest)
	}
	return connection, timeout, nil
}

func (connection *nodeConnection) startExec(
	ctx context.Context,
	guest Guest,
	command Command,
	timeout int,
) (int64, error) {
	body := execStartBody{Command: command.Argv}
	if len(command.Stdin) > 0 {
		var input string
		if guest.Kind == KindQEMU {
			input = string(command.Stdin)
		} else {
			input = base64.StdEncoding.EncodeToString(command.Stdin)
		}
		body.InputData = &input
	}
	if guest.Kind == KindLXC {
		body.Timeout = &timeout
	}

	document, err := encodeJSON(&body)
	if err != nil {
		return 0, fmt.Errorf("%s: POST exec: %w", guest, err)
	}
	data, err := connection.call(ctx, guest, apiRequest{
		method:    http.MethodPost,
		operation: "exec",
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
	return started.PID, nil
}

// waitForExit polls exec-status with a delay that doubles from 200 ms to 2 s.
// The first read after the exit returns the result, and the API deletes it.
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
		data, err := connection.call(waitContext, guest, apiRequest{
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
		if status.Exited {
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

func resultOf(guest Guest, command Command, status execStatus, timeout int) (Result, error) {
	quoted := QuoteCommand(command.Argv)
	if status.TimedOut {
		return Result{}, fmt.Errorf("%s: command %s timed out after %d seconds", guest, quoted, timeout)
	}
	if status.OutTruncated || status.ErrTruncated {
		return Result{}, fmt.Errorf("%s: the output of command %s exceeded the capture limit", guest, quoted)
	}

	exitCode, hasExitCode := status.exitCode()
	if !hasExitCode {
		return Result{}, fmt.Errorf("%s: command %s exited without an exit code", guest, quoted)
	}

	stdout, err := decodeStream(guest, status.OutData)
	if err != nil {
		return Result{}, err
	}
	stderr, err := decodeStream(guest, status.ErrData)
	if err != nil {
		return Result{}, err
	}
	return Result{Stdout: stdout, Stderr: stderr, ExitCode: exitCode}, nil
}

// The container exec API returns base64. The QEMU guest agent API decodes the
// streams before it returns them.
func decodeStream(guest Guest, data string) ([]byte, error) {
	if guest.Kind == KindQEMU {
		return []byte(data), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		slog.Error("exec-status stream is not base64", "guest", guest.String(), "err", err)
		return nil, fmt.Errorf("%s: decode exec-status stream: %w", guest, err)
	}
	return decoded, nil
}
