package transport

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestQuoteCommandPassesArgumentsThroughShell(t *testing.T) {
	// The arguments pass through the same number of shells as in production:
	// the hypervisor login shell parses the command line once.
	argv := []string{
		"plain",
		"",
		"two words",
		"it's",
		`back\slash`,
		`"double"`,
		"$HOME `id` $(id)",
		"semi;colon && pipe | redirect > file",
		"line\nbreak",
		"tab\there",
		"*?[glob]",
		"~",
		"-leading-dash",
		"unicode é",
		"'''",
	}
	script := `for argument in "$@"; do printf '%s\0' "$argument"; done`
	commandLine := QuoteCommand(append([]string{"sh", "-c", script, "sh"}, argv...))

	output, err := exec.Command("sh", "-c", commandLine).Output()
	if err != nil {
		t.Fatalf("run quoted command: %v", err)
	}
	received := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if !reflect.DeepEqual(received, argv) {
		t.Fatalf("arguments changed in the shell:\n got %q\nwant %q", received, argv)
	}
}

func TestHostArgvLXC(t *testing.T) {
	guest := Guest{Node: "suburban", VMID: 224, Kind: KindLXC}
	hostArgv, err := HostArgv(guest, Command{Argv: []string{"stat", "--", "/etc/a file"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pct", "exec", "224", "--", "stat", "--", "/etc/a file"}
	if !reflect.DeepEqual(hostArgv, want) {
		t.Fatalf("got %q, want %q", hostArgv, want)
	}
	wantLine := `'pct' 'exec' '224' '--' 'stat' '--' '/etc/a file'`
	if QuoteCommand(hostArgv) != wantLine {
		t.Fatalf("got %s, want %s", QuoteCommand(hostArgv), wantLine)
	}
}

func TestHostArgvQEMU(t *testing.T) {
	guest := Guest{Node: "suburban", VMID: 113, Kind: KindQEMU}

	withTimeout, err := HostArgv(guest, Command{Argv: []string{"sh", "-c", "cat > \"$1\"", "sh", "/tmp/x"}, TimeoutSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"qm", "guest", "exec", "113", "--pass-stdin", "1", "--timeout", "600", "--",
		"sh", "-c", "cat > \"$1\"", "sh", "/tmp/x",
	}
	if !reflect.DeepEqual(withTimeout, want) {
		t.Fatalf("got %q, want %q", withTimeout, want)
	}

	defaultTimeout, err := HostArgv(guest, Command{Argv: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if defaultTimeout[7] != "120" {
		t.Fatalf("default timeout argument is %q, want 120", defaultTimeout[7])
	}
}

func TestHostArgvRejectsBadInput(t *testing.T) {
	if _, err := HostArgv(Guest{Node: "n", VMID: 1, Kind: KindLXC}, Command{}); err == nil {
		t.Fatal("empty argv was accepted")
	}
	if _, err := HostArgv(Guest{Node: "n", VMID: 1, Kind: "docker"}, Command{Argv: []string{"true"}}); err == nil {
		t.Fatal("unknown kind was accepted")
	}
}

func TestParseQEMUExecOutput(t *testing.T) {
	guest := Guest{Node: "suburban", VMID: 113, Kind: KindQEMU}

	success := `{"exitcode":0,"exited":1,"out-data":"line one\nline two\n"}`
	result, err := ParseQEMUExecOutput(guest, []byte(success))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "line one\nline two\n" || len(result.Stderr) != 0 {
		t.Fatalf("unexpected result %+v", result)
	}

	failure := `{
   "err-data" : "stat: cannot statx '/nope': No such file or directory\n",
   "exitcode" : 1,
   "exited" : true
}`
	result, err = ParseQEMUExecOutput(guest, []byte(failure))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 1 || !strings.Contains(string(result.Stderr), "No such file") {
		t.Fatalf("unexpected result %+v", result)
	}

	rejected := map[string]string{
		"timeout":   `{"pid":4242}`,
		"truncated": `{"exitcode":0,"exited":1,"out-data":"x","out-truncated":1}`,
		"not json":  `VM 113 is not running`,
	}
	for name, document := range rejected {
		_, err = ParseQEMUExecOutput(guest, []byte(document))
		if err == nil {
			t.Fatalf("%s: document was accepted", name)
		}
		if !strings.Contains(err.Error(), `node="suburban" vmid=113 kind=qemu`) {
			t.Fatalf("%s: error does not include the guest: %v", name, err)
		}
	}
}

func TestInterpretResultsReportUnreachableGuest(t *testing.T) {
	container := Guest{Node: "suburban", VMID: 224, Kind: KindLXC}
	_, err := interpretLXCResult(container, Result{ExitCode: 255, Stderr: []byte("container '224' not running!\n")})
	assertUnreachable(t, err, `node="suburban" vmid=224 kind=lxc`)

	_, err = interpretLXCResult(container, Result{
		ExitCode: 2,
		Stderr:   []byte("Configuration file 'nodes/suburban/lxc/224.conf' does not exist\n"),
	})
	assertUnreachable(t, err, `node="suburban" vmid=224 kind=lxc`)

	virtualMachine := Guest{Node: "suburban", VMID: 113, Kind: KindQEMU}
	_, err = interpretQEMUResult(virtualMachine, Result{ExitCode: 255, Stderr: []byte("QEMU guest agent is not running\n")})
	assertUnreachable(t, err, `node="suburban" vmid=113 kind=qemu`)
}

func TestInterpretLXCResultReturnsInnerExitStatus(t *testing.T) {
	container := Guest{Node: "suburban", VMID: 224, Kind: KindLXC}
	stderr := "stat: cannot statx '/root/container not running!': No such file or directory\n"
	result, err := interpretLXCResult(container, Result{ExitCode: 1, Stderr: []byte(stderr)})
	if err != nil {
		t.Fatalf("inner command failure was reported as a transport error: %v", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code %d, want 1", result.ExitCode)
	}
}

func assertUnreachable(t *testing.T, err error, guestText string) {
	t.Helper()
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("got %v, want an UnreachableError", err)
	}
	if !strings.Contains(err.Error(), guestText) {
		t.Fatalf("error %q does not include %q", err.Error(), guestText)
	}
}
