package acceptance_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const (
	testRemovalService     = "pveguest-acc-removal.service"
	testRemovalServiceUnit = "[Unit]\nDescription=pveguest acceptance test removal service\n\n[Service]\nType=simple\nExecStart=/bin/sleep 3600\n\n[Install]\nWantedBy=multi-user.target\n"
	removalUnitAddress     = "pveguest_systemd_unit.removal"
	invalidTokenSuffix     = "-invalid"

	baselineProviderVersion   = "0.1.0"
	candidateInstalledVersion = "0.1.1"
	destroyPlanFileName       = "destroy.tfplan"

	testRemovalSlowStopUnit = "[Unit]\nDescription=pveguest acceptance test removal service with a slow stop\n\n[Service]\nType=simple\nExecStart=/bin/sleep 3600\nExecStop=/bin/sleep 60\nTimeoutStopSec=120\n\n[Install]\nWantedBy=multi-user.target\n"

	testRemovalRefuseStopUnit = "[Unit]\nDescription=pveguest acceptance test removal service that refuses a manual stop\nRefuseManualStop=yes\n\n[Service]\nType=simple\nExecStart=/bin/sleep 3600\n\n[Install]\nWantedBy=multi-user.target\n"

	deactivatingPollCount    = 40
	deactivatingPollInterval = 250 * time.Millisecond
)

func (g *testGuest) externalProviderConfig() string {
	return fmt.Sprintf(`
provider "pveguest" {
  nodes = {
    %q = {
      %s
    }
  }
  %s
}

resource "pveguest_systemd_unit" "removal" {
  node    = %q
  vmid    = %d
  kind    = %q
  name    = %q
  enabled = true
  active  = true
}
`, g.guest.Node, g.nodeAttributes(), g.controllerAttribute(),
		g.guest.Node, g.guest.VMID, string(g.guest.Kind), testRemovalService)
}

func (g *testGuest) useRemovalUnit() {
	g.t.Helper()
	g.useRemovalUnitContent(testRemovalServiceUnit)
}

func (g *testGuest) useRemovalUnitContent(content string) {
	g.t.Helper()
	g.t.Cleanup(g.removeRemovalUnit)
	g.writeFile(unitDirectory+"/"+testRemovalService, content)
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) requireDeactivating() {
	g.t.Helper()
	g.mustRun("systemctl", "stop", "--no-block", "--", testRemovalService)
	for range deactivatingPollCount {
		if g.systemctlState("is-active", testRemovalService) == "deactivating" {
			return
		}
		time.Sleep(deactivatingPollInterval)
	}
	g.t.Fatalf("%s did not report deactivating after systemctl stop --no-block", testRemovalService)
}

func (g *testGuest) removeRemovalUnit() {
	g.t.Helper()
	g.run(nil, "systemctl", "disable", "--", testRemovalService)
	g.run(nil, "systemctl", "stop", "--", testRemovalService)
	g.run(nil, "systemctl", "kill", "--signal=SIGKILL", "--", testRemovalService)
	g.mustRun(
		"rm", "-f", "--",
		unitDirectory+"/"+testRemovalService,
		unitDirectory+"/multi-user.target.wants/"+testRemovalService,
	)
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) removalUnitConfig(extra string) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_systemd_unit" "removal" {
  node    = local.node
  vmid    = local.vmid
  kind    = local.kind
  name    = %q
  enabled = true
  active  = true
  %s
}
`, testRemovalService, extra)
}

func (g *testGuest) expectRemovalUnit(enabled string, active ...string) resource.TestCheckFunc {
	return checkGuest(func() error {
		gotEnabled := g.systemctlState("is-enabled", testRemovalService)
		gotActive := g.systemctlState("is-active", testRemovalService)
		if gotEnabled != enabled || !slices.Contains(active, gotActive) {
			return fmt.Errorf("%s is %s and %s, want %s and one of %s",
				testRemovalService, gotEnabled, gotActive, enabled, strings.Join(active, ", "))
		}
		unitFile := unitDirectory + "/" + testRemovalService
		if !g.exists(unitFile) {
			return fmt.Errorf("%s does not exist", unitFile)
		}
		return nil
	})
}

func TestAccSystemdRemovalUpgrade(t *testing.T) {
	guest := newTestGuest(t)
	guest.useRemovalUnit()

	baseline := map[string]resource.ExternalProvider{
		"pveguest": {
			Source:            providerSource,
			VersionConstraint: baselineProviderVersion,
		},
	}

	resource.Test(t, resource.TestCase{
		CheckDestroy: guest.expectRemovalUnit("enabled", "active"),
		Steps: []resource.TestStep{
			{
				ExternalProviders: baseline,
				Config:            guest.externalProviderConfig(),
				Check:             guest.expectRemovalUnit("enabled", "active"),
			},
			{
				ProtoV6ProviderFactories: protoV6ProviderFactories,
				Config:                   guest.removalUnitConfig(""),
				ConfigPlanChecks:         expectAction(removalUnitAddress, plancheck.ResourceActionNoop),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(removalUnitAddress, "remove_on_delete", "false"),
					guest.expectRemovalUnit("enabled", "active"),
				),
			},
		},
	})
}

func TestAccSystemdRemovalPolicy(t *testing.T) {
	t.Run("default policy keeps the unit running", testSystemdDefaultPolicy)
	t.Run("remove_on_delete stops and disables the unit", testSystemdRemoveOnDelete)
	t.Run("unreadable guest keeps the resource in state", testSystemdUnreadableGuest)
	t.Run("pending stop finishes before state removal", testSystemdPendingStop)
	t.Run("refused stop keeps the resource in state", testSystemdRefusedStop)
}

func testSystemdRefusedStop(t *testing.T) {
	guest := newTestGuest(t)
	guest.useRemovalUnitContent(testRemovalRefuseStopUnit)
	config := guest.removalUnitConfig("remove_on_delete = true")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectRemovalUnit("disabled", "inactive", "failed"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  guest.expectRemovalUnit("enabled", "active"),
			},
			{
				Config:      config,
				Destroy:     true,
				ExpectError: regexp.MustCompile(`(?s)Delete\s+systemd\s+unit`),
			},
			{
				PreConfig: func() {
					guest.writeFile(unitDirectory+"/"+testRemovalService, testRemovalServiceUnit)
					guest.mustRun("systemctl", "daemon-reload")
				},
				Config:           config,
				ConfigPlanChecks: expectAction(removalUnitAddress, plancheck.ResourceActionNoop),
				Check:            guest.expectRemovalUnit("enabled", "active"),
			},
		},
	})
}

func testSystemdPendingStop(t *testing.T) {
	t.Run("enabled unit", func(t *testing.T) { runPendingStopVariant(t, true) })
	t.Run("disabled unit", func(t *testing.T) { runPendingStopVariant(t, false) })
}

func runPendingStopVariant(t *testing.T, enabled bool) {
	guest := newTestGuest(t)
	guest.useRemovalUnitContent(testRemovalSlowStopUnit)
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}

	wantEnabled := "disabled"
	if enabled {
		wantEnabled = "enabled"
	}

	workspace.writeConfig(guest.installedRemovalConfig(enabled))
	workspace.mustRun("init", "-no-color")
	workspace.mustRun("apply", "-no-color", "-auto-approve")
	if err := guest.expectRemovalUnit(wantEnabled, "active")(nil); err != nil {
		t.Fatal(err)
	}

	guest.requireDeactivating()
	started := time.Now()
	workspace.mustRun("destroy", "-no-color", "-auto-approve")
	destroyDuration := time.Since(started)
	activeState := guest.systemctlState("is-active", testRemovalService)
	t.Logf("destroy took %s and is-active reported %s", destroyDuration, activeState)
	if activeState != "inactive" && activeState != "failed" {
		t.Fatalf("%s is %s when destroy returns, want inactive or failed", testRemovalService, activeState)
	}
	if err := guest.expectRemovalUnit("disabled", "inactive", "failed")(nil); err != nil {
		t.Fatal(err)
	}
}

func testSystemdDefaultPolicy(t *testing.T) {
	guest := newTestGuest(t)
	guest.useRemovalUnit()
	config := guest.removalUnitConfig("")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectRemovalUnit("enabled", "active"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(removalUnitAddress, "remove_on_delete", "false"),
					guest.expectRemovalUnit("enabled", "active"),
				),
			},
		},
	})
}

func testSystemdRemoveOnDelete(t *testing.T) {
	guest := newTestGuest(t)
	guest.useRemovalUnit()
	config := guest.removalUnitConfig("remove_on_delete = true")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectRemovalUnit("disabled", "inactive", "failed"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(removalUnitAddress, "remove_on_delete", "true"),
					guest.expectRemovalUnit("enabled", "active"),
				),
			},
		},
	})
}

func (g *testGuest) installedRemovalConfig(enabled bool) string {
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
      %s
    }
  }
}

resource "pveguest_systemd_unit" "removal" {
  node             = %q
  vmid             = %d
  kind             = %q
  name             = %q
  enabled          = %t
  active           = true
  remove_on_delete = true
}
`, providerSource, candidateInstalledVersion, g.guest.Node, g.nodeAttributes(),
		g.guest.Node, g.guest.VMID, string(g.guest.Kind), testRemovalService, enabled)
}

func collapseWhitespace(output string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(output, "│", " ")), " ")
}

func testSystemdUnreadableGuest(t *testing.T) {
	guest := newTestGuest(t)
	guest.useRemovalUnit()
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}

	unreadable := *guest
	unreadable.node.APIToken += invalidTokenSuffix

	workspace.writeConfig(guest.installedRemovalConfig(true))
	workspace.mustRun("init", "-no-color")
	workspace.mustRun("apply", "-no-color", "-auto-approve")
	if err := guest.expectRemovalUnit("enabled", "active")(nil); err != nil {
		t.Fatal(err)
	}

	workspace.writeConfig(unreadable.installedRemovalConfig(true))
	workspace.mustRun("plan", "-destroy", "-refresh=false", "-no-color", "-out", destroyPlanFileName)
	output, exitCode := workspace.run("apply", "-no-color", destroyPlanFileName)
	if exitCode == 0 {
		t.Fatalf("applying the destroy plan with an invalid token exited with status 0:\n%s", output)
	}
	collapsed := collapseWhitespace(output)
	for _, want := range []string{"Delete systemd unit", "HTTP 401"} {
		if !strings.Contains(collapsed, want) {
			t.Fatalf("destroy output does not contain %q:\n%s", want, output)
		}
	}

	stateList := workspace.mustRun("state", "list")
	if !strings.Contains(stateList, removalUnitAddress) {
		t.Fatalf("state does not list %s after the failed destroy:\n%s", removalUnitAddress, stateList)
	}
	if err := guest.expectRemovalUnit("enabled", "active")(nil); err != nil {
		t.Fatal(err)
	}

	workspace.writeConfig(guest.installedRemovalConfig(true))
	workspace.mustRun("destroy", "-no-color", "-auto-approve")
	if err := guest.expectRemovalUnit("disabled", "inactive", "failed")(nil); err != nil {
		t.Fatal(err)
	}
}
