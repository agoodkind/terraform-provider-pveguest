package acceptance

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tofuPathVariable     = "TF_ACC_TERRAFORM_PATH"
	installedVersion     = "0.1.0"
	stateFileName        = "terraform.tfstate"
	planFileName         = "plan.tfplan"
	planChangesExitCode  = 2
	reattachVariableName = "TF_REATTACH_PROVIDERS"
	secretVariable       = "TF_VAR_secret"
)

// tofuWorkspace runs the tofu binary in one directory. The provider comes
// from the implied local mirror that make install fills, not from the test
// process.
type tofuWorkspace struct {
	t         *testing.T
	directory string
	binary    string
	// variableValue is the value of the ephemeral input variable. OpenTofu stores
	// no ephemeral value, and each command receives it again.
	variableValue string
}

func (w *tofuWorkspace) run(arguments ...string) (string, int) {
	w.t.Helper()
	command := exec.Command(w.binary, arguments...)
	command.Dir = w.directory
	command.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", reattachVariableName+"=", secretVariable+"="+w.variableValue)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if err == nil {
		return output.String(), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		w.t.Fatalf("tofu %s: %v", strings.Join(arguments, " "), err)
	}
	return output.String(), exitError.ExitCode()
}

func (w *tofuWorkspace) mustRun(arguments ...string) string {
	w.t.Helper()
	output, exitCode := w.run(arguments...)
	if exitCode != 0 {
		w.t.Fatalf("tofu %s exited with status %d:\n%s", strings.Join(arguments, " "), exitCode, output)
	}
	return output
}

func (w *tofuWorkspace) writeConfig(config string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.directory, "main.tf"), []byte(config), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *tofuWorkspace) requireOmits(fileName string, secret string) {
	w.t.Helper()
	content, err := os.ReadFile(filepath.Join(w.directory, fileName))
	if err != nil {
		w.t.Fatal(err)
	}
	if bytes.Contains(content, []byte(secret)) {
		w.t.Fatalf("%s contains the write-only content", fileName)
	}
}

// requirePlanArchiveOmits reads every member of the saved plan, which is a
// compressed archive, and fails when one contains the secret.
func (w *tofuWorkspace) requirePlanArchiveOmits(fileName string, secret string) {
	w.t.Helper()
	archive, err := zip.OpenReader(filepath.Join(w.directory, fileName))
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	for _, member := range archive.File {
		reader, err := member.Open()
		if err != nil {
			w.t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			w.t.Fatal(err)
		}
		if bytes.Contains(content, []byte(secret)) {
			w.t.Fatalf("plan member %s contains the write-only content", member.Name)
		}
	}
}

// TestAccInstalledProviderWriteOnly runs the tofu binary against the
// installed provider and reads the raw state file and the saved plan file,
// which the plugin test harness does not expose.
func TestAccInstalledProviderWriteOnly(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	path := testDirectory + "/installed-secret.conf"
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}

	config := func(version int) string {
		return fmt.Sprintf(`
terraform {
  required_providers {
    pveguest = {
      source  = %q
      version = %q
    }
  }
}

provider "pveguest" {
  nodes = {
    %q = {
      host = %q
    }
  }
}

variable "secret" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "pveguest_file" "secret" {
  node               = %q
  vmid               = %d
  kind               = %q
  path               = %q
  content_wo         = var.secret
  content_wo_version = %d
  mode               = "0600"
}
`, providerSource, installedVersion, guest.guest.Node, guest.host,
			guest.guest.Node, guest.guest.VMID, string(guest.guest.Kind), path, version)
	}

	workspace.variableValue = firstMarker
	workspace.writeConfig(config(1))
	workspace.mustRun("init", "-no-color")

	planOutput := workspace.mustRun("plan", "-no-color", "-out", planFileName)
	if strings.Contains(planOutput, firstMarker) {
		t.Fatal("the plan output contains the write-only content")
	}
	workspace.requirePlanArchiveOmits(planFileName, firstMarker)
	planJSON := workspace.mustRun("show", "-json", planFileName)
	if strings.Contains(planJSON, firstMarker) {
		t.Fatal("the JSON form of the plan contains the write-only content")
	}

	applyOutput := workspace.mustRun("apply", "-no-color", planFileName)
	if strings.Contains(applyOutput, firstMarker) {
		t.Fatal("the apply output contains the write-only content")
	}
	if err := guest.expectFileContent(path, firstMarker)(); err != nil {
		t.Fatal(err)
	}
	workspace.requireOmits(stateFileName, firstMarker)

	// AC1 through the installed binary: exit status 0 means no changes.
	output, exitCode := workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != 0 {
		t.Fatalf("plan after apply exited with status %d:\n%s", exitCode, output)
	}

	guest.writeFile(path, "edited by hand\n")
	output, exitCode = workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != planChangesExitCode {
		t.Fatalf("plan after a hand edit exited with status %d, want %d:\n%s", exitCode, planChangesExitCode, output)
	}

	workspace.variableValue = secondMarker
	workspace.writeConfig(config(2))
	applyOutput = workspace.mustRun("apply", "-no-color", "-auto-approve")
	if strings.Contains(applyOutput, secondMarker) {
		t.Fatal("the apply output contains the write-only content")
	}
	if err := guest.expectFileContent(path, secondMarker)(); err != nil {
		t.Fatal(err)
	}
	workspace.requireOmits(stateFileName, secondMarker)
	workspace.requireOmits(stateFileName, firstMarker)

	workspace.mustRun("destroy", "-no-color", "-auto-approve")
	if guest.exists(path) {
		t.Fatalf("%s still exists after destroy", path)
	}
}
