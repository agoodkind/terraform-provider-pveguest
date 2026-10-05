package transport_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

func TestQuoteCommandPassesArgumentsThroughShell(t *testing.T) {
	// One POSIX shell parses the quoted command line.
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
	commandLine := transport.QuoteCommand(append([]string{"sh", "-c", script, "sh"}, argv...))

	output, err := exec.Command("sh", "-c", commandLine).Output()
	if err != nil {
		t.Fatalf("run quoted command: %v", err)
	}
	received := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if !reflect.DeepEqual(received, argv) {
		t.Fatalf("arguments changed in the shell:\n got %q\nwant %q", received, argv)
	}
}
