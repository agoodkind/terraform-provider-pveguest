package provider

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestParseStatOutputPadsMode(t *testing.T) {
	cases := map[string]fileStatus{
		"644 root root":       {Mode: "0644", Owner: "root", Group: "root"},
		"4755 root staff":     {Mode: "4755", Owner: "root", Group: "staff"},
		"0 nobody nogroup":    {Mode: "0000", Owner: "nobody", Group: "nogroup"},
		"600 UNKNOWN UNKNOWN": {Mode: "0600", Owner: "UNKNOWN", Group: "UNKNOWN"},
	}
	for line, want := range cases {
		got, err := parseStatOutput(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if got != want {
			t.Fatalf("%q: got %+v, want %+v", line, got, want)
		}
	}
	if _, err := parseStatOutput("644 root"); err == nil {
		t.Fatal("a line with two fields was accepted")
	}
}

func TestParseSHA256SumOutput(t *testing.T) {
	hash := hashHex([]byte("hello\n"))
	if len(hash) != sha256HexLength {
		t.Fatalf("hashHex returned %d characters, want %d", len(hash), sha256HexLength)
	}

	accepted := []string{
		hash + "  /etc/motd",
		// sha256sum prints this form for a file name with a backslash.
		`\` + hash + `  /root/a\\b`,
	}
	for _, line := range accepted {
		got, err := parseSHA256SumOutput(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if got != hash {
			t.Fatalf("%q: got %s", line, got)
		}
	}

	rejected := []string{"", "short  /etc/motd", strings.Repeat("z", 64) + "  /etc/motd"}
	for _, line := range rejected {
		if _, err := parseSHA256SumOutput(line); err == nil {
			t.Fatalf("%q was accepted", line)
		}
	}
}

func TestValidateCommandLineQuotesTemporaryPath(t *testing.T) {
	temporaryPath := "/root/dir with space/.it's.conf.pveguest-00ff"
	commandLine := validateCommandLine(`set -- %s; echo "$1"`, temporaryPath)

	output, err := exec.Command("sh", "-c", commandLine).Output()
	if err != nil {
		t.Fatalf("run %q: %v", commandLine, err)
	}
	if strings.TrimSuffix(string(output), "\n") != temporaryPath {
		t.Fatalf("the shell received %q, want %q", output, temporaryPath)
	}
}

func TestTemporaryPathIsInDestinationDirectory(t *testing.T) {
	first, err := temporaryPathFor("/etc/ssh/sshd_config.d/10-base.conf")
	if err != nil {
		t.Fatal(err)
	}
	second, err := temporaryPathFor("/etc/ssh/sshd_config.d/10-base.conf")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/etc/ssh/sshd_config.d/.10-base.conf.pveguest-"
	if !strings.HasPrefix(first, prefix) || !strings.HasPrefix(second, prefix) {
		t.Fatalf("unexpected temporary paths %q and %q", first, second)
	}
	if first == second {
		t.Fatalf("two calls returned the same path %q", first)
	}
}

func TestParseDpkgQueryOutput(t *testing.T) {
	output := "curl\tii \nhello\trc \nvim\tun \nlibc6\tii \nlibc6\tii \nhalf\tiU \n"
	got := parseDpkgQueryOutput(output)
	want := map[string]bool{"curl": true, "libc6": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if missing := missingPackages([]string{"curl", "hello", "vim"}, got); !reflect.DeepEqual(missing, []string{"hello", "vim"}) {
		t.Fatalf("missing packages: %v", missing)
	}
}

func TestClassifyEnabledState(t *testing.T) {
	cases := map[string]enablement{
		"enabled":         enablementEnabled,
		"enabled-runtime": enablementEnabled,
		"static":          enablementFixed,
		"indirect":        enablementFixed,
		"generated":       enablementFixed,
		"disabled":        enablementDisabled,
		"masked":          enablementDisabled,
		"linked":          enablementDisabled,
	}
	for state, want := range cases {
		if got := classifyEnabledState(state); got != want {
			t.Fatalf("%s: got %d, want %d", state, got, want)
		}
	}
}

func TestIsActiveState(t *testing.T) {
	cases := map[string]bool{
		"active":       true,
		"activating":   true,
		"reloading":    true,
		"inactive":     false,
		"failed":       false,
		"deactivating": false,
	}
	for state, want := range cases {
		if got := isActiveState(state); got != want {
			t.Fatalf("%s: got %t, want %t", state, got, want)
		}
	}
}

func TestReportsMissingUnit(t *testing.T) {
	missing := "Failed to get unit file state for pveguest-acc.timer: No such file or directory\n"
	if !reportsMissingUnit(missing) {
		t.Fatal("the systemd message for a missing unit file was not recognized")
	}
	if reportsMissingUnit("Failed to connect to bus: Host is down\n") {
		t.Fatal("a bus failure was reported as a missing unit")
	}
}

func TestPackageAndUnitNamePatterns(t *testing.T) {
	for _, name := range []string{"hello", "libstdc++6", "qemu-guest-agent", "python3.11"} {
		if !packageNamePattern.MatchString(name) {
			t.Fatalf("package name %q was rejected", name)
		}
	}
	for _, name := range []string{"-o", "Hello", "a", "hello=1.0", "hello world", "hello:amd64"} {
		if packageNamePattern.MatchString(name) {
			t.Fatalf("package name %q was accepted", name)
		}
	}
	for _, name := range []string{"ssh.service", "getty@tty1.service", "apt-daily.timer", `dev-disk-by\x2duuid.swap`} {
		if !unitNamePattern.MatchString(name) {
			t.Fatalf("unit name %q was rejected", name)
		}
	}
	for _, name := range []string{"ssh", "--now", "ssh.service extra", "a/b.service"} {
		if unitNamePattern.MatchString(name) {
			t.Fatalf("unit name %q was accepted", name)
		}
	}
}
