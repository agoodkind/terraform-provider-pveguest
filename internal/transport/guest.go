package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Kind string

const (
	KindLXC  Kind = "lxc"
	KindQEMU Kind = "qemu"

	DefaultTimeoutSeconds = 120

	// qm guest exec rejects more than 1 MiB on stdin.
	QEMUStdinLimit = 1024 * 1024

	// The SSH session outlives the guest agent timeout by this margin, so
	// qm reports the timeout itself.
	sessionTimeoutMargin = 15 * time.Second
)

// Guest identifies one container or VM on one hypervisor.
type Guest struct {
	Node string
	VMID int64
	Kind Kind
}

func (guest Guest) String() string {
	return fmt.Sprintf("guest node=%q vmid=%d kind=%s", guest.Node, guest.VMID, guest.Kind)
}

// Command is one program invocation inside a guest.
type Command struct {
	Argv           []string
	Stdin          []byte
	TimeoutSeconds int
}

// UnreachableError reports that the hypervisor could not run a command in
// the guest: a stopped container, a stopped VM, or a VM without a running
// guest agent.
type UnreachableError struct {
	Guest  Guest
	Detail string
}

func (unreachable *UnreachableError) Error() string {
	return fmt.Sprintf("%s is unreachable: %s", unreachable.Guest, unreachable.Detail)
}

var lxcUnreachablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^container '\d+' not running!$`),
	regexp.MustCompile(`(?m)^Configuration file '[^']*' does not exist$`),
	regexp.MustCompile(`(?m)^lxc-attach: \d+: `),
}

// HostArgv returns the argument vector that the hypervisor runs for the
// command.
func HostArgv(guest Guest, command Command) ([]string, error) {
	if len(command.Argv) == 0 {
		return nil, fmt.Errorf("empty command for %s", guest)
	}
	vmid := strconv.FormatInt(guest.VMID, 10)
	var hostArgv []string
	switch guest.Kind {
	case KindLXC:
		hostArgv = []string{"pct", "exec", vmid, "--"}
	case KindQEMU:
		timeout := strconv.Itoa(timeoutSeconds(command))
		hostArgv = []string{"qm", "guest", "exec", vmid, "--pass-stdin", "1", "--timeout", timeout, "--"}
	default:
		return nil, fmt.Errorf("unknown kind %q for %s", guest.Kind, guest)
	}
	return append(hostArgv, command.Argv...), nil
}

func timeoutSeconds(command Command) int {
	if command.TimeoutSeconds > 0 {
		return command.TimeoutSeconds
	}
	return DefaultTimeoutSeconds
}

// Run runs the command inside the guest and returns the output and exit
// status of the inner command. Every error message includes the guest.
func (pool *Pool) Run(ctx context.Context, guest Guest, command Command) (Result, error) {
	hostArgv, err := HostArgv(guest, command)
	if err != nil {
		return Result{}, err
	}
	if guest.Kind == KindQEMU && len(command.Stdin) > QEMUStdinLimit {
		return Result{}, fmt.Errorf(
			"%s: stdin of %d bytes exceeds the %d byte limit of qm guest exec",
			guest, len(command.Stdin), QEMUStdinLimit,
		)
	}

	sessionTimeout := time.Duration(timeoutSeconds(command))*time.Second + sessionTimeoutMargin
	sessionContext, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	hostResult, err := pool.RunOnNode(sessionContext, guest.Node, QuoteCommand(hostArgv), command.Stdin)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", guest, err)
	}
	if guest.Kind == KindQEMU {
		return interpretQEMUResult(guest, hostResult)
	}
	return interpretLXCResult(guest, hostResult)
}

func interpretLXCResult(guest Guest, hostResult Result) (Result, error) {
	if hostResult.ExitCode == 0 {
		return hostResult, nil
	}
	for _, pattern := range lxcUnreachablePatterns {
		if pattern.Match(hostResult.Stderr) {
			return Result{}, &UnreachableError{Guest: guest, Detail: trimmedText(hostResult.Stderr)}
		}
	}
	return hostResult, nil
}

type qemuExecOutput struct {
	Exited       json.RawMessage `json:"exited"`
	ExitCode     *int            `json:"exitcode"`
	OutData      string          `json:"out-data"`
	ErrData      string          `json:"err-data"`
	OutTruncated json.RawMessage `json:"out-truncated"`
	ErrTruncated json.RawMessage `json:"err-truncated"`
}

func interpretQEMUResult(guest Guest, hostResult Result) (Result, error) {
	// qm exits nonzero only when it could not run the command: the VM is
	// stopped, has no agent, or the agent does not answer. The status of
	// the inner command is in the JSON document.
	if hostResult.ExitCode != 0 {
		return Result{}, &UnreachableError{Guest: guest, Detail: trimmedText(hostResult.Stderr)}
	}
	return ParseQEMUExecOutput(guest, hostResult.Stdout)
}

// ParseQEMUExecOutput converts the JSON document printed by qm guest exec.
func ParseQEMUExecOutput(guest Guest, document []byte) (Result, error) {
	var output qemuExecOutput
	if err := json.Unmarshal(document, &output); err != nil {
		return Result{}, fmt.Errorf("%s: parse qm guest exec output: %w", guest, err)
	}
	if !jsonTruthy(output.Exited) || output.ExitCode == nil {
		return Result{}, fmt.Errorf("%s: command did not finish within the guest agent timeout", guest)
	}
	if jsonTruthy(output.OutTruncated) || jsonTruthy(output.ErrTruncated) {
		return Result{}, fmt.Errorf("%s: the guest agent truncated the command output", guest)
	}
	return Result{
		Stdout:   []byte(output.OutData),
		Stderr:   []byte(output.ErrData),
		ExitCode: *output.ExitCode,
	}, nil
}

// jsonTruthy accepts both encodings that Proxmox versions print for a
// boolean: 1 and true.
func jsonTruthy(value json.RawMessage) bool {
	text := string(bytes.TrimSpace(value))
	return text == "1" || text == "true"
}

func trimmedText(output []byte) string {
	return strings.TrimSpace(string(output))
}
