package acceptance_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	testImportService   = "pveguest-acc-import.service"
	testImportConfPath  = testDirectory + "/import.conf"
	importUnitAddress   = "pveguest_systemd_unit.imported"
	importFileAddress   = "pveguest_file.conf"
	unitImportIDFormat  = "<node>/<vmid>/<kind>/<name>"
	fileImportIDFormat  = "<node>/<vmid>/<kind>/<absolute path>"
	malformedImportID   = "node/101/lxc"
	firstRestartValue   = "one"
	adoptedConfContent  = "PVEGUEST_ACC_IMPORT=one\n"
	changedConfContent  = "PVEGUEST_ACC_IMPORT=two\n"
	laterConfContent    = "PVEGUEST_ACC_IMPORT=three\n"
	importedSummaryText = "2 imported"
)

// importedWriteIDPattern matches the write_id that Read sets after import.
var importedWriteIDPattern = regexp.MustCompile(`^import-[0-9a-f]{16}$`)

//go:embed testdata/pveguest-acc-import.service
var testImportServiceUnit string

// useImportUnit writes the configuration file and the unit, enables and starts
// the unit outside the provider, and removes both after the test.
func (g *testGuest) useImportUnit() {
	g.t.Helper()
	g.useTestDirectory()
	g.writeFile(testImportConfPath, adoptedConfContent)
	g.mustRun("chmod", "0644", "--", testImportConfPath)
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

func (g *testGuest) importFileID() string {
	return fmt.Sprintf("%s/%d/%s/%s", g.guest.Node, g.guest.VMID, g.guest.Kind, testImportConfPath)
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

// importUnitResource returns the unit resource. restartOn is the HCL
// expression of one restart_on value.
func (g *testGuest) importUnitResource(restartOn string) string {
	return fmt.Sprintf(`
resource "pveguest_systemd_unit" "imported" {
  node    = %q
  vmid    = %d
  kind    = %q
  name    = %q
  enabled = true
  active  = true
  restart_on = {
    conf = %s
  }
}
`, g.guest.Node, g.guest.VMID, string(g.guest.Kind), testImportService, restartOn)
}

// installedImportConfig returns a configuration for the installed provider
// with one import block for each address and ID pair.
func (g *testGuest) installedImportConfig(imports map[string]string) string {
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
`, providerSource, candidateInstalledVersion, g.guest.Node, g.nodeAttributes())
	for address, id := range imports {
		config += fmt.Sprintf(`
import {
  to = %s
  id = %q
}
`, address, id)
	}
	return config
}

// dependencyConfig imports the configuration file and the unit. The unit
// restart_on map uses the write_id of the file.
func (g *testGuest) dependencyConfig(content string) string {
	imports := map[string]string{importFileAddress: g.importFileID(), importUnitAddress: g.importUnitID()}
	return g.installedImportConfig(imports) + g.dependencyResources(content)
}

// dependencyResources returns a file resource with content and a unit resource
// with the file write_id in restart_on.
func (g *testGuest) dependencyResources(content string) string {
	return fmt.Sprintf(`
resource "pveguest_file" "conf" {
  node    = %q
  vmid    = %d
  kind    = %q
  path    = %q
  content = %q
}
`, g.guest.Node, g.guest.VMID, string(g.guest.Kind), testImportConfPath, content) +
		g.importUnitResource(importFileAddress+".write_id")
}

// fileWriteID returns the write_id of pveguest_file.conf from tofu show -json.
func (w *tofuWorkspace) fileWriteID() string {
	w.t.Helper()
	var shown shownState
	if err := json.Unmarshal([]byte(w.mustRun("show", "-json")), &shown); err != nil {
		w.t.Fatalf("json.Unmarshal returned an error for tofu show -json output: %v", err)
	}
	for _, resourceState := range shown.Values.RootModule.Resources {
		if resourceState.Address == importFileAddress {
			return resourceState.Values.WriteID
		}
	}
	w.t.Fatalf("state does not contain resource %s", importFileAddress)
	return ""
}

type shownState struct {
	Values struct {
		RootModule struct {
			Resources []struct {
				Address string `json:"address"`
				Values  struct {
					WriteID string `json:"write_id"`
				} `json:"values"`
			} `json:"resources"`
		} `json:"root_module"`
	} `json:"values"`
}

// expectImportedWriteID checks whether writeID has the import form requested by
// wantImported.
func expectImportedWriteID(writeID string, wantImported bool) error {
	isImported := importedWriteIDPattern.MatchString(writeID)
	if wantImported && !isImported {
		return fmt.Errorf("resource %s has write_id %q; expected import form %s", importFileAddress, writeID, importedWriteIDPattern)
	}
	if !wantImported && isImported {
		return fmt.Errorf("resource %s has write_id %q in import form after the rewrite", importFileAddress, writeID)
	}
	return nil
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
	t.Run("malformed import ID shows the format", testSystemdImportMalformedID)
	t.Run("import command keeps the unit running", testSystemdImportCommand)
	t.Run("literal restart_on value restarts the imported unit", testSystemdImportLiteralValue)
	t.Run("unchanged dependency keeps the unit running", testSystemdImportUnchangedDependency)
	t.Run("changed dependency restarts the unit", testSystemdImportChangedDependency)
}

func testSystemdImportMalformedID(t *testing.T) {
	guest := newTestGuest(t)
	cases := []struct {
		name string
		id   string
		want string
	}{
		{name: "missing unit", id: malformedImportID, want: unitImportIDFormat},
		{name: "extra segment", id: "node/101/lxc/unit.service/extra", want: unitImportIDFormat},
		{name: "invalid guest ID", id: "node/zero/lxc/unit.service", want: unitImportIDFormat},
		{name: "zero guest ID", id: "node/0/lxc/unit.service", want: unitImportIDFormat},
		{name: "invalid kind", id: "node/101/invalid/unit.service", want: "must be one of"},
		{name: "invalid unit", id: "node/101/lxc/unit", want: "must be a unit name with a suffix"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			imports := map[string]string{importUnitAddress: testCase.id}
			config := guest.installedImportConfig(imports) + guest.importUnitResource(fmt.Sprintf("%q", firstRestartValue))
			expectImportPlanError(t, config, testCase.want)
		})
	}
	t.Run("file import requires an absolute path", func(t *testing.T) {
		config := guest.dependencyConfig(adoptedConfContent)
		config = strings.ReplaceAll(config, guest.importFileID(), "node/101/lxc/relative.conf")
		expectImportPlanError(t, config, "must be an absolute path that ends in a file name")
	})
	t.Run("file import shows the format", func(t *testing.T) {
		config := guest.dependencyConfig(adoptedConfContent)
		config = strings.ReplaceAll(config, guest.importFileID(), malformedImportID)
		expectImportPlanError(t, config, fileImportIDFormat)
	})
}

func expectImportPlanError(t *testing.T, config string, want string) {
	t.Helper()
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}
	workspace.writeConfig(config)
	workspace.mustRun("init", "-no-color")

	output, exitCode := workspace.run("plan", "-no-color")
	if exitCode == 0 {
		t.Fatalf("plan with a malformed import ID exited with status 0:\n%s", output)
	}
	collapsed := collapseWhitespace(output)
	if !strings.Contains(collapsed, want) {
		t.Fatalf("plan output does not contain %q:\n%s", want, output)
	}
}

func testSystemdImportCommand(t *testing.T) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	mainPID := guest.importUnitMainPID()
	first := guest.providerBlock() + guest.dependencyResources(adoptedConfContent)
	second := guest.providerBlock() + guest.dependencyResources(changedConfContent)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectImportUnit(),
		Steps: []resource.TestStep{
			{
				Config:             first,
				ResourceName:       importFileAddress,
				ImportState:        true,
				ImportStateId:      guest.importFileID(),
				ImportStatePersist: true,
			},
			{
				Config:             first,
				ResourceName:       importUnitAddress,
				ImportState:        true,
				ImportStateId:      guest.importUnitID(),
				ImportStatePersist: true,
				ImportStateCheck:   expectImportedAttributes,
			},
			{
				Config: first,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(importFileAddress, plancheck.ResourceActionNoop),
						plancheck.ExpectResourceAction(importUnitAddress, plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(importFileAddress, "write_id", importedWriteIDPattern),
					guest.expectImportUnitPID(&mainPID, true),
				),
			},
			{
				Config:           second,
				ConfigPlanChecks: expectAction(importUnitAddress, plancheck.ResourceActionUpdate),
				Check:            guest.expectImportUnitPID(&mainPID, false),
			},
		},
	})
}

func testSystemdImportLiteralValue(t *testing.T) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	mainPID := guest.importUnitMainPID()
	config := guest.providerBlock() + guest.importUnitResource(fmt.Sprintf("%q", firstRestartValue))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectImportUnit(),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       importUnitAddress,
				ImportState:        true,
				ImportStateId:      guest.importUnitID(),
				ImportStatePersist: true,
				ImportStateCheck:   expectImportedAttributes,
			},
			{
				Config:           config,
				ConfigPlanChecks: expectAction(importUnitAddress, plancheck.ResourceActionUpdate),
				Check:            guest.expectImportUnitPID(&mainPID, false),
			},
		},
	})
}

func testSystemdImportUnchangedDependency(t *testing.T) {
	runDependencyImport(t, adoptedConfContent, false)
}

func testSystemdImportChangedDependency(t *testing.T) {
	runDependencyImport(t, changedConfContent, true)
}

// runDependencyImport imports the configuration file and the unit with import
// blocks and applies content as the declared file content.
func runDependencyImport(t *testing.T, content string, wantRestart bool) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	mainPID := guest.importUnitMainPID()
	inode := guest.importFileInode()
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}

	workspace.writeConfig(guest.dependencyConfig(content))
	workspace.mustRun("init", "-no-color")
	workspace.mustRun("plan", "-no-color", "-out", planFileName)
	applyOutput := workspace.mustRun("apply", "-no-color", planFileName)
	if !strings.Contains(applyOutput, importedSummaryText) {
		t.Fatalf("apply output does not report two imported resources:\n%s", applyOutput)
	}
	if err := guest.expectFileContent(testImportConfPath, content)(); err != nil {
		t.Fatal(err)
	}
	if err := guest.expectImportUnitPID(&mainPID, !wantRestart)(nil); err != nil {
		t.Fatal(err)
	}
	if !wantRestart && guest.importFileInode() != inode {
		t.Fatal("the unchanged import rewrote the configuration file")
	}
	if err := expectImportedWriteID(workspace.fileWriteID(), !wantRestart); err != nil {
		t.Fatal(err)
	}

	output, exitCode := workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != 0 {
		t.Fatalf("plan after the import apply exited with status %d, want 0:\n%s", exitCode, output)
	}

	workspace.writeConfig(guest.dependencyConfig(laterConfContent))
	workspace.mustRun("apply", "-no-color", "-auto-approve")
	if err := guest.expectImportUnitPID(&mainPID, false)(nil); err != nil {
		t.Fatal(err)
	}

	workspace.mustRun("destroy", "-no-color", "-auto-approve")
	if err := guest.expectImportUnit()(nil); err != nil {
		t.Fatal(err)
	}
}

func (g *testGuest) importFileInode() string {
	g.t.Helper()
	result := g.runReadOnly("stat", "--format=%i", "--", testImportConfPath)
	inode := strings.TrimSpace(string(result.Stdout))
	if result.ExitCode != 0 || inode == "" {
		g.t.Fatalf("configuration file inode = %q, exit status = %d, stderr = %s", inode, result.ExitCode, result.Stderr)
	}
	return inode
}
