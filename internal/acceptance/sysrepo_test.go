package acceptance_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const (
	testSysrepoModule   = "pveguest-acc"
	testSysrepoFirst    = "2026-10-05"
	testSysrepoSecond   = "2026-10-06"
	testSysrepoFeature  = "extra"
	testSysrepoFirstXML = `<settings xmlns="urn:pveguest:acc"><name>alpha</name></settings>`
	testSysrepoEditXML  = `<settings xmlns="urn:pveguest:acc"><note>added by hand</note></settings>`
)

// The module file for a revision. The second revision adds one leaf.
func testSysrepoYang(revision string) string {
	extraLeaf := ""
	if revision == testSysrepoSecond {
		extraLeaf = "    leaf level { type uint8; }\n"
	}
	return fmt.Sprintf(`module %s {
  yang-version 1.1;
  namespace "urn:pveguest:acc";
  prefix pga;
  revision %s;
  feature %s;
  container settings {
    leaf name { type string; }
    leaf note { type string; }
%s  }
}
`, testSysrepoModule, revision, testSysrepoFeature, extraLeaf)
}

func testSysrepoModulePath(revision string) string {
	return fmt.Sprintf("%s/%s@%s.yang", testDirectory, testSysrepoModule, revision)
}

// useSysrepo skips the test when the guest has no sysrepoctl, and removes the
// test module and its data after the test.
func (g *testGuest) useSysrepo() {
	g.t.Helper()
	probe := g.run(nil, "sh", "-c", "command -v sysrepoctl && command -v sysrepocfg")
	if probe.ExitCode != 0 {
		g.t.Skipf("%s has no sysrepoctl and sysrepocfg in its PATH", g.guest)
	}
	g.t.Cleanup(func() {
		// The uninstall exits with a nonzero status for a module that the
		// test already removed.
		g.run(nil, "sysrepoctl", "--uninstall", testSysrepoModule)
	})
}

func (g *testGuest) sysrepoModuleRow() string {
	g.t.Helper()
	for line := range strings.SplitSeq(g.mustRun("sysrepoctl", "--list"), "\n") {
		if strings.HasPrefix(line, testSysrepoModule+" ") {
			return line
		}
	}
	return ""
}

func (g *testGuest) sysrepoExport() string {
	g.t.Helper()
	return g.mustRun(
		"sysrepocfg", "--export", "--datastore", "running", "--module", testSysrepoModule, "--format", "xml",
	)
}

func (g *testGuest) sysrepoConfig(revision string, dataContent string) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_file" "module" {
  node    = local.node
  vmid    = local.vmid
  kind    = local.kind
  path    = %q
  content = %q
}

resource "pveguest_sysrepo_module" "test" {
  node     = local.node
  vmid     = local.vmid
  kind     = local.kind
  path     = pveguest_file.module.path
  features = [%q]
  update   = true
}

resource "pveguest_sysrepo_data" "test" {
  node      = local.node
  vmid      = local.vmid
  kind      = local.kind
  datastore = "running"
  module    = pveguest_sysrepo_module.test.module
  content   = %q
}
`, testSysrepoModulePath(revision), testSysrepoYang(revision), testSysrepoFeature, dataContent)
}

func (g *testGuest) sysrepoModuleOnlyConfig(revision string) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_file" "module" {
  node    = local.node
  vmid    = local.vmid
  kind    = local.kind
  path    = %q
  content = %q
}

resource "pveguest_sysrepo_module" "test" {
  node = local.node
  vmid = local.vmid
  kind = local.kind
  path = pveguest_file.module.path
}
`, testSysrepoModulePath(revision), testSysrepoYang(revision))
}

func TestAccSysrepoModuleWithoutUpdateKeepsInstalledRevision(t *testing.T) {
	guest := newTestGuest(t)
	guest.useSysrepo()
	guest.useTestDirectory()

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					guest.writeFile(testSysrepoModulePath(testSysrepoSecond), testSysrepoYang(testSysrepoSecond))
					guest.mustRun(
						"sysrepoctl", "--install", testSysrepoModulePath(testSysrepoSecond),
						"--search-dirs", testDirectory,
					)
				},
				Config: guest.sysrepoModuleOnlyConfig(testSysrepoFirst),
				Check: checkGuest(func() error {
					row := guest.sysrepoModuleRow()
					if !strings.Contains(row, testSysrepoSecond) {
						return fmt.Errorf("module row is %q", row)
					}
					return nil
				}),
			},
			{
				Config:   guest.sysrepoModuleOnlyConfig(testSysrepoFirst),
				PlanOnly: true,
			},
		},
	})
}

func TestAccSysrepoModuleAndData(t *testing.T) {
	guest := newTestGuest(t)
	guest.useSysrepo()
	guest.useTestDirectory()

	moduleAddress := "pveguest_sysrepo_module.test"
	dataAddress := "pveguest_sysrepo_data.test"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: checkGuest(func() error {
			if row := guest.sysrepoModuleRow(); row != "" {
				return fmt.Errorf("module row %q remains after destroy", row)
			}
			return nil
		}),
		Steps: []resource.TestStep{
			{
				Config: guest.sysrepoConfig(testSysrepoFirst, testSysrepoFirstXML),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(moduleAddress, "module", testSysrepoModule),
					resource.TestCheckResourceAttr(moduleAddress, "revision", testSysrepoFirst),
					resource.TestCheckResourceAttr(moduleAddress, "features.#", "1"),
					checkGuest(func() error {
						row := guest.sysrepoModuleRow()
						if !strings.Contains(row, testSysrepoFirst) || !strings.Contains(row, testSysrepoFeature) {
							return fmt.Errorf("module row is %q", row)
						}
						return nil
					}),
					checkGuest(func() error {
						export := guest.sysrepoExport()
						if !strings.Contains(export, "<name>alpha</name>") {
							return fmt.Errorf("export is %q", export)
						}
						return nil
					}),
				),
			},
			{
				// A node that the content omits is a difference. The apply removes it.
				PreConfig: func() {
					guest.writeFile(testDirectory+"/edit.xml", testSysrepoEditXML)
					guest.mustRun(
						"sysrepocfg", "--edit="+testDirectory+"/edit.xml", "--datastore", "running",
						"--module", testSysrepoModule, "--format", "xml",
					)
				},
				Config:           guest.sysrepoConfig(testSysrepoFirst, testSysrepoFirstXML),
				ConfigPlanChecks: expectAction(dataAddress, plancheck.ResourceActionUpdate),
				Check: checkGuest(func() error {
					export := guest.sysrepoExport()
					if !strings.Contains(export, "<name>alpha</name>") || strings.Contains(export, "<note>") {
						return fmt.Errorf("export is %q", export)
					}
					return nil
				}),
			},
			{
				// An uninstalled module is a normal Read result: the plan installs it again.
				PreConfig: func() {
					guest.mustRun("sysrepoctl", "--uninstall", testSysrepoModule)
				},
				Config:           guest.sysrepoConfig(testSysrepoFirst, testSysrepoFirstXML),
				ConfigPlanChecks: expectAction(moduleAddress, plancheck.ResourceActionCreate),
				Check: checkGuest(func() error {
					if row := guest.sysrepoModuleRow(); !strings.Contains(row, testSysrepoFirst) {
						return fmt.Errorf("module row is %q", row)
					}
					return nil
				}),
			},
			{
				// A file with a newer revision updates the installed module.
				Config:           guest.sysrepoConfig(testSysrepoSecond, testSysrepoFirstXML),
				ConfigPlanChecks: expectAction(moduleAddress, plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(moduleAddress, "revision", testSysrepoSecond),
					checkGuest(func() error {
						row := guest.sysrepoModuleRow()
						if !strings.Contains(row, testSysrepoSecond) || !strings.Contains(row, testSysrepoFeature) {
							return fmt.Errorf("module row is %q", row)
						}
						return nil
					}),
				),
			},
		},
	})
}
