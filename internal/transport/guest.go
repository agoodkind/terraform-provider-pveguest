// Package transport runs commands inside Proxmox guests through SSH sessions on
// the hypervisor.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind selects the hypervisor tool that runs commands in a guest.
type Kind string

const (
	// KindLXC is a container. The hypervisor runs commands with pct exec.
	KindLXC Kind = "lxc"

	// KindQEMU is a VM. The hypervisor runs commands with qm guest exec, which
	// needs a running guest agent in the VM.
	KindQEMU Kind = "qemu"
)

const (
	defaultTimeoutSeconds = 120

	// qemuStdinLimit is the largest stdin that qm guest exec accepts, in bytes.
	qemuStdinLimit = 1024 * 1024

	// The SSH timeout includes a margin for the guest-agent timeout response.
	sessionTimeoutMargin = 15 * time.Second
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

// Command is one program run inside a guest. Argv[0] is the program, Stdin is
// the complete standard input, and a zero TimeoutSeconds selects the default
// timeout.
type Command struct {
	Argv           []string
	Stdin          []byte
	TimeoutSeconds int
}

// unreachableError reports that the hypervisor could not run a command in
// the guest: a stopped container, a stopped VM, or a VM with a stopped
// guest agent.
type unreachableError struct {
	guest  Guest
	detail string
}

func (unreachable *unreachableError) Error() string {
	return fmt.Sprintf("%s is unreachable: %s", unreachable.guest, unreachable.detail)
}

var lxcUnreachablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^container '\d+' not running!$`),
	regexp.MustCompile(`(?m)^Configuration file '[^']*' does not exist$`),
	regexp.MustCompile(`(?m)^lxc-attach: \d+: `),
}

func hostArgv(guest Guest, command Command) ([]string, error) {
	if len(command.Argv) == 0 {
		return nil, fmt.Errorf("empty command for %s", guest)
	}
	vmid := strconv.FormatInt(guest.VMID, 10)
	var argv []string
	switch guest.Kind {
	case KindLXC:
		argv = []string{"pct", "exec", vmid, "--"}
	case KindQEMU:
		timeout := strconv.Itoa(timeoutSeconds(command))
		argv = []string{"qm", "guest", "exec", vmid, "--pass-stdin", "1", "--timeout", timeout, "--"}
	default:
		return nil, fmt.Errorf("unknown kind %q for %s", guest.Kind, guest)
	}
	return append(argv, command.Argv...), nil
}

func timeoutSeconds(command Command) int {
	if command.TimeoutSeconds > 0 {
		return command.TimeoutSeconds
	}
	return defaultTimeoutSeconds
}

// Run returns the guest command's output and status.
func (pool *Pool) Run(ctx context.Context, guest Guest, command Command) (Result, error) {
	argv, err := hostArgv(guest, command)
	if err != nil {
		return Result{}, err
	}
	if guest.Kind == KindQEMU && len(command.Stdin) > qemuStdinLimit {
		return Result{}, fmt.Errorf(
			"%s: stdin of %d bytes exceeds the %d byte limit of qm guest exec",
			guest, len(command.Stdin), qemuStdinLimit,
		)
	}

	sessionTimeout := time.Duration(timeoutSeconds(command))*time.Second + sessionTimeoutMargin
	sessionContext, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	hostResult, err := pool.runOnNode(sessionContext, guest.Node, QuoteCommand(argv), command.Stdin)
	if err != nil {
		slog.ErrorContext(
			ctx, "guest command failed on the hypervisor",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err,
		)
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
			return Result{}, &unreachableError{guest: guest, detail: trimmedText(hostResult.Stderr)}
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
	// qm guest exec can return status 0 when the guest program fails.
	// Read the guest program's status from JSON.
	if hostResult.ExitCode != 0 {
		return Result{}, &unreachableError{guest: guest, detail: trimmedText(hostResult.Stderr)}
	}
	return parseQEMUExecOutput(guest, hostResult.Stdout)
}

func parseQEMUExecOutput(guest Guest, document []byte) (Result, error) {
	var output qemuExecOutput
	if err := json.Unmarshal(document, &output); err != nil {
		slog.Error(
			"qm guest exec output is not the expected JSON document",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err,
		)
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
