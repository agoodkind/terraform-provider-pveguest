package acceptance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	testImportService     = "pveguest-acc-import.service"
	testImportServiceUnit = "[Unit]\nDescription=pveguest acceptance test import service\n\n[Service]\nType=simple\nExecStart=/bin/sleep 3600\n\n[Install]\nWantedBy=multi-user.target\n"
	importUnitAddress     = "pveguest_systemd_unit.imported"
	firstRestartValue     = "one"
	secondRestartValue    = "two"
)

// useImportUnit writes, enables, and starts the test unit outside the provider
// and removes it after the test.
func (g *testGuest) useImportUnit() {
	g.t.Helper()
	g.t.Cleanup(g.removeImportUnit)
	g.writeFile(unitDirectory+"/"+testImportService, testImportServiceUnit)
	g.mustRun("systemctl", "daemon-reload")
	g.mustRun("systemctl", "enable", "--now", "--", testImportService)
}

func (g *testGuest) removeImportUnit() {
	g.t.Helper()
	g.run(nil, "systemctl", "disable", "--", testImportService)
	g.run(nil, "systemctl", "stop", "--", testImportService)
	g.run(nil, "systemctl", "kill", "--signal=SIGKILL", "--", testImportService)
	g.mustRun(
		"rm", "-f", "--",
		unitDirectory+"/"+testImportService,
		unitDirectory+"/multi-user.target.wants/"+testImportService,
	)
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) importUnitID() string {
	return fmt.Sprintf("%s/%d/%s/%s", g.guest.Node, g.guest.VMID, g.guest.Kind, testImportService)
}

// importUnitMainPID returns the main process ID of the running import test
// unit.
func (g *testGuest) importUnitMainPID() string {
	g.t.Helper()
	result := g.runReadOnly("systemctl", "show", "--property=MainPID", "--value", "--", testImportService)
	mainPID := strings.TrimSpace(string(result.Stdout))
	if result.ExitCode != 0 || mainPID == "" || mainPID == "0" {
		g.t.Fatalf("%s main PID = %q, exit status = %d, stderr = %s",
			testImportService, mainPID, result.ExitCode, result.Stderr)
	}
	return mainPID
}

func (g *testGuest) expectImportUnitPID(want *string, same bool) resource.TestCheckFunc {
	return checkGuest(func() error {
		got := g.importUnitMainPID()
		if same && got != *want {
			return fmt.Errorf("%s main PID after apply = %s, want unchanged PID %s", testImportService, got, *want)
		}
		if !same && got == *want {
			return fmt.Errorf("%s main PID after apply = %s, want a different PID after restart", testImportService, got)
		}
		*want = got
		return nil
	})
}

func (g *testGuest) importUnitResource(restartValue string) string {
	return fmt.Sprintf(`
resource "pveguest_systemd_unit" "imported" {
  node    = %q
  vmid    = %d
  kind    = %q
  name    = %q
  enabled = true
  active  = true
  restart_on = {
    version = %q
  }
}
`, g.guest.Node, g.guest.VMID, string(g.guest.Kind), testImportService, restartValue)
}

func expectImportedAttributes(states []*terraform.InstanceState) error {
	if len(states) != 1 {
		return fmt.Errorf("import produced %d resources, want 1", len(states))
	}
	attributes := states[0].Attributes
	want := map[string]string{
		"name":             testImportService,
		"enabled":          "true",
		"active":           "true",
		"remove_on_delete": "false",
	}
	for name, value := range want {
		if attributes[name] != value {
			return fmt.Errorf("imported %s = %q, want %q", name, attributes[name], value)
		}
	}
	return nil
}

func TestAccSystemdUnitImport(t *testing.T) {
	t.Run("import command keeps the unit running", testSystemdImportCommand)
	t.Run("import block keeps the unit running", testSystemdImportBlock)
}

func testSystemdImportCommand(t *testing.T) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	mainPID := guest.importUnitMainPID()
	first := guest.providerBlock() + guest.importUnitResource(firstRestartValue)
	second := guest.providerBlock() + guest.importUnitResource(secondRestartValue)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectImportUnit(),
		Steps: []resource.TestStep{
			{
				Config:             first,
				ResourceName:       importUnitAddress,
				ImportState:        true,
				ImportStateId:      guest.importUnitID(),
				ImportStatePersist: true,
				ImportStateCheck:   expectImportedAttributes,
			},
			{
				Config:           first,
				ConfigPlanChecks: expectAction(importUnitAddress, plancheck.ResourceActionUpdate),
				Check:            guest.expectImportUnitPID(&mainPID, true),
			},
			{
				Config:           second,
				ConfigPlanChecks: expectAction(importUnitAddress, plancheck.ResourceActionUpdate),
				Check:            guest.expectImportUnitPID(&mainPID, false),
			},
		},
	})
}

func (g *testGuest) expectImportUnit() resource.TestCheckFunc {
	return checkGuest(func() error {
		gotEnabled := g.systemctlState("is-enabled", testImportService)
		gotActive := g.systemctlState("is-active", testImportService)
		if gotEnabled != "enabled" || gotActive != "active" {
			return fmt.Errorf("%s is %s and %s after destroy, want enabled and active",
				testImportService, gotEnabled, gotActive)
		}
		return nil
	})
}

func testSystemdImportBlock(t *testing.T) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	mainPID := guest.importUnitMainPID()
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}

	config := fmt.Sprintf(`
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
      %s
    }
  }
}

import {
  to = %s
  id = %q
}
`, providerSource, candidateInstalledVersion, guest.guest.Node, guest.nodeAttributes(),
		importUnitAddress, guest.importUnitID()) + guest.importUnitResource(firstRestartValue)

	workspace.writeConfig(config)
	workspace.mustRun("init", "-no-color")
	applyOutput := workspace.mustRun("apply", "-no-color", "-auto-approve")
	if !strings.Contains(applyOutput, "1 imported") {
		t.Fatalf("apply output does not report one imported resource:\n%s", applyOutput)
	}
	if err := guest.expectImportUnitPID(&mainPID, true)(nil); err != nil {
		t.Fatal(err)
	}

	output, exitCode := workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != 0 {
		t.Fatalf("plan after the import apply exited with status %d, want 0:\n%s", exitCode, output)
	}

	workspace.mustRun("destroy", "-no-color", "-auto-approve")
	if err := guest.expectImportUnit()(nil); err != nil {
		t.Fatal(err)
	}
}
