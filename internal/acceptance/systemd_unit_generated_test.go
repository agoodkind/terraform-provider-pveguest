package acceptance_test

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const (
	testSysVName        = "pveguest-acc-sysv"
	testSysVService     = testSysVName + ".service"
	testSysVScriptPath  = "/etc/init.d/" + testSysVName
	testSysVGenerated   = "/run/systemd/generator.late/" + testSysVService
	sysVUnitAddress     = "pveguest_systemd_unit.sysv"
	generatedFileState  = "generated"
	enabledFileState    = "enabled"
	disabledFileState   = "disabled"
	activeUnitState     = "active"
	sysVFirstRestart    = "one"
	sysVSecondRestart   = "two"
	sysVThirdRestart    = "three"
	invocationProperty  = "InvocationID"
	unitFileStateOption = "UnitFileState"
)

//go:embed testdata/pveguest-acc-sysv.sh
var testSysVScript string

// useSysVService installs, enables, and starts a SysV service with a generated
// unit. It checks the service state and registers cleanup for the test.
func (g *testGuest) useSysVService() {
	g.t.Helper()
	g.writeFile(testSysVScriptPath, testSysVScript)
	g.t.Cleanup(g.removeSysVService)
	g.mustRun("chmod", "0755", "--", testSysVScriptPath)
	g.mustRun("systemctl", "daemon-reload")
	g.mustRun("systemctl", "enable", "--", testSysVService)
	g.mustRun("systemctl", "start", "--", testSysVService)

	fileState := g.sysVProperty(unitFileStateOption)
	enabled := g.systemctlState("is-enabled", testSysVService)
	active := g.systemctlState("is-active", testSysVService)
	if fileState != generatedFileState || enabled != enabledFileState || active != activeUnitState {
		g.t.Fatalf("%s has %s %s, is-enabled %s, and is-active %s; the test requires %s, %s, and %s",
			testSysVService, unitFileStateOption, fileState, enabled, active,
			generatedFileState, enabledFileState, activeUnitState)
	}
}

func (g *testGuest) removeSysVService() {
	g.t.Helper()
	g.mustRun("systemctl", "stop", "--", testSysVService)
	g.mustRun("systemctl", "disable", "--", testSysVService)
	g.mustRun("rm", "-f", "--", testSysVScriptPath)
	g.mustRun("update-rc.d", testSysVName, "remove")
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) sysVProperty(property string) string {
	g.t.Helper()
	result := g.runReadOnly("systemctl", "show", "--property="+property, "--value", "--", testSysVService)
	value := strings.TrimSpace(string(result.Stdout))
	if result.ExitCode != 0 || value == "" {
		g.t.Fatalf("%s %s = %q, exit status = %d, stderr = %s",
			testSysVService, property, value, result.ExitCode, result.Stderr)
	}
	return value
}

// expectSysVService checks the generated unit's enabled and active states.
// It checks whether InvocationID changed according to wantRestart, then stores
// the current InvocationID in invocation.
func (g *testGuest) expectSysVService(wantEnabled string, invocation *string, wantRestart bool) resource.TestCheckFunc {
	return checkGuest(func() error {
		fileState := g.sysVProperty(unitFileStateOption)
		enabled := g.systemctlState("is-enabled", testSysVService)
		active := g.systemctlState("is-active", testSysVService)
		if fileState != generatedFileState || enabled != wantEnabled || active != activeUnitState {
			return fmt.Errorf("%s has %s %s, is-enabled %s, and is-active %s; want %s, %s, and %s",
				testSysVService, unitFileStateOption, fileState, enabled, active,
				generatedFileState, wantEnabled, activeUnitState)
		}
		got := g.sysVProperty(invocationProperty)
		if wantRestart && got == *invocation {
			return fmt.Errorf("%s %s after apply = %s, want a new value after restart",
				testSysVService, invocationProperty, got)
		}
		if !wantRestart && got != *invocation {
			return fmt.Errorf("%s %s after apply = %s, want unchanged value %s",
				testSysVService, invocationProperty, got, *invocation)
		}
		*invocation = got
		return nil
	})
}

func (g *testGuest) sysVUnitConfig(enabled bool, restartValue string) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_systemd_unit" "sysv" {
  node             = %q
  vmid             = %d
  kind             = %q
  name             = %q
  enabled          = %t
  active           = true
  remove_on_delete = false
  restart_on = {
    conf = %q
  }
}
`, g.guest.Node, g.guest.VMID, string(g.guest.Kind), testSysVService, enabled, restartValue)
}

func TestAccSystemdUnitGeneratedSysV(t *testing.T) {
	guest := newTestGuest(t)
	guest.useSysVService()
	invocation := guest.sysVProperty(invocationProperty)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectSysVService(enabledFileState, &invocation, false),
		Steps: []resource.TestStep{
			{
				Config:           guest.sysVUnitConfig(true, sysVFirstRestart),
				ConfigPlanChecks: expectAction(sysVUnitAddress, plancheck.ResourceActionCreate),
				Check:            guest.expectSysVService(enabledFileState, &invocation, true),
			},
			{
				Config:           guest.sysVUnitConfig(true, sysVSecondRestart),
				ConfigPlanChecks: expectAction(sysVUnitAddress, plancheck.ResourceActionUpdate),
				Check:            guest.expectSysVService(enabledFileState, &invocation, true),
			},
			{
				Config:           guest.sysVUnitConfig(false, sysVSecondRestart),
				ConfigPlanChecks: expectAction(sysVUnitAddress, plancheck.ResourceActionUpdate),
				Check:            guest.expectSysVService(disabledFileState, &invocation, false),
			},
			{
				Config:           guest.sysVUnitConfig(true, sysVThirdRestart),
				ConfigPlanChecks: expectAction(sysVUnitAddress, plancheck.ResourceActionUpdate),
				Check:            guest.expectSysVService(enabledFileState, &invocation, true),
			},
		},
	})
}
