package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	firstMarker  = "pveguest-acceptance-marker-one"
	secondMarker = "pveguest-acceptance-marker-two"
)

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func expectAction(address string, action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, action)},
	}
}

func checkGuest(assertion func() error) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		return assertion()
	}
}

func (g *testGuest) expectFileContent(path string, want string) func() error {
	return func() error {
		got := g.readFile(path)
		if got != want {
			return fmt.Errorf("%s contains %q, want %q", path, got, want)
		}
		return nil
	}
}

func (g *testGuest) fileConfig(path string, content string, extra string) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_file" "test" {
  node    = local.node
  vmid    = local.vmid
  kind    = local.kind
  path    = %q
  content = %q
  %s
}
`, path, content, extra)
}

// Every apply step ends with a plan that the harness requires to be empty.
// That plan is the AC1 check in each test of this file.

func TestAccFile(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	path := testDirectory + "/plain file.conf"
	content := "alpha = 1\nquote = 'single' \"double\"\n"
	config := guest.fileConfig(path, content, "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: checkGuest(func() error {
			if guest.exists(path) {
				return fmt.Errorf("%s still exists after destroy", path)
			}
			return nil
		}),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_file.test", "sha256", sha256Hex(content)),
					resource.TestCheckResourceAttr("pveguest_file.test", "mode", "0644"),
					resource.TestCheckResourceAttr("pveguest_file.test", "owner", "root"),
					checkGuest(guest.expectFileContent(path, content)),
					checkGuest(func() error {
						status := strings.TrimSpace(guest.mustRun("stat", "-c", "%a %U %G", "--", path))
						if status != "644 root root" {
							return fmt.Errorf("stat reports %q", status)
						}
						return nil
					}),
				),
			},
			{
				// AC3, first half: a hand edit produces a planned rewrite.
				PreConfig:        func() { guest.writeFile(path, "edited by hand\n") },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_file.test", plancheck.ResourceActionUpdate),
				Check:            checkGuest(guest.expectFileContent(path, content)),
			},
			{
				// A missing file is a normal Read result: the plan creates it.
				PreConfig:        func() { guest.mustRun("rm", "-f", "--", path) },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_file.test", plancheck.ResourceActionCreate),
				Check:            checkGuest(guest.expectFileContent(path, content)),
			},
			{
				PreConfig:        func() { guest.mustRun("chmod", "0666", "--", path) },
				Config:           guest.fileConfig(path, content, `mode = "0600"`),
				ConfigPlanChecks: expectAction("pveguest_file.test", plancheck.ResourceActionUpdate),
				Check: checkGuest(func() error {
					mode := strings.TrimSpace(guest.mustRun("stat", "-c", "%a", "--", path))
					if mode != "600" {
						return fmt.Errorf("mode is %s, want 600", mode)
					}
					return nil
				}),
			},
		},
	})
}

func TestAccFileValidate(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	path := testDirectory + "/validated.conf"
	validate := `validate = "grep -q good %s"`
	goodContent := "good\n"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: guest.fileConfig(path, goodContent, validate),
				Check:  checkGuest(guest.expectFileContent(path, goodContent)),
			},
			{
				// AC6: a failing validate command fails the apply. OpenTofu
				// wraps the diagnostic text, and each gap in the pattern
				// matches a line break.
				Config:      guest.fileConfig(path, "bad\n", validate),
				ExpectError: regexp.MustCompile(`validate\s+command\s+for\s+\S+\s+exited\s+with\s+status\s+1`),
			},
			{
				// AC6: the previous file is unchanged, and the temporary
				// file is gone.
				PreConfig: func() {
					if err := guest.expectFileContent(path, goodContent)(); err != nil {
						t.Fatal(err)
					}
					listing := strings.TrimSpace(guest.mustRun("ls", "-A", "--", testDirectory))
					if listing != "validated.conf" {
						t.Fatalf("directory contains %q after the failed apply", listing)
					}
				},
				Config: guest.fileConfig(path, goodContent, validate),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
		},
	})
}

func (g *testGuest) writeOnlyConfig(path string, secret string, version int) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_file" "secret" {
  node               = local.node
  vmid               = local.vmid
  kind               = local.kind
  path               = %q
  content_wo         = %q
  content_wo_version = %d
  mode               = "0600"
}
`, path, secret, version)
}

func checkStateOmits(secret string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		for address, resourceState := range state.RootModule().Resources {
			for name, value := range resourceState.Primary.Attributes {
				if strings.Contains(value, secret) {
					return fmt.Errorf("state attribute %s.%s contains the write-only content", address, name)
				}
			}
		}
		return nil
	}
}

func TestAccFileWriteOnly(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	path := testDirectory + "/secret.conf"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: guest.writeOnlyConfig(path, firstMarker, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkGuest(guest.expectFileContent(path, firstMarker)),
					checkStateOmits(firstMarker),
					resource.TestCheckNoResourceAttr("pveguest_file.secret", "content_wo"),
					resource.TestCheckResourceAttr("pveguest_file.secret", "sha256", sha256Hex(firstMarker)),
				),
			},
			{
				// New content with the same version writes nothing.
				Config: guest.writeOnlyConfig(path, secondMarker, 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: checkGuest(guest.expectFileContent(path, firstMarker)),
			},
			{
				Config:           guest.writeOnlyConfig(path, secondMarker, 2),
				ConfigPlanChecks: expectAction("pveguest_file.secret", plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkGuest(guest.expectFileContent(path, secondMarker)),
					checkStateOmits(secondMarker),
				),
			},
			{
				// Read compares the guest hash with the hash of the last
				// apply: a hand edit produces a planned rewrite.
				PreConfig:        func() { guest.writeFile(path, "edited by hand\n") },
				Config:           guest.writeOnlyConfig(path, secondMarker, 2),
				ConfigPlanChecks: expectAction("pveguest_file.secret", plancheck.ResourceActionUpdate),
				Check:            checkGuest(guest.expectFileContent(path, secondMarker)),
			},
		},
	})
}

func (g *testGuest) linkConfig(path string, target string) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_link" "test" {
  node   = local.node
  vmid   = local.vmid
  kind   = local.kind
  path   = %q
  target = %q
}
`, path, target)
}

func TestAccLink(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	path := testDirectory + "/link"
	firstTarget := testDirectory + "/first target"
	secondTarget := "relative/second"
	expectTarget := func(want string) func() error {
		return func() error {
			got := strings.TrimSuffix(guest.mustRun("readlink", "--", path), "\n")
			if got != want {
				return fmt.Errorf("link stores %q, want %q", got, want)
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: checkGuest(func() error {
			if guest.exists(path) {
				return fmt.Errorf("%s still exists after destroy", path)
			}
			return nil
		}),
		Steps: []resource.TestStep{
			{
				Config: guest.linkConfig(path, firstTarget),
				Check:  checkGuest(expectTarget(firstTarget)),
			},
			{
				PreConfig:        func() { guest.mustRun("ln", "-sfT", "--", "/nonexistent", path) },
				Config:           guest.linkConfig(path, firstTarget),
				ConfigPlanChecks: expectAction("pveguest_link.test", plancheck.ResourceActionUpdate),
				Check:            checkGuest(expectTarget(firstTarget)),
			},
			{
				Config:           guest.linkConfig(path, secondTarget),
				ConfigPlanChecks: expectAction("pveguest_link.test", plancheck.ResourceActionUpdate),
				Check:            checkGuest(expectTarget(secondTarget)),
			},
			{
				PreConfig:        func() { guest.mustRun("rm", "-f", "--", path) },
				Config:           guest.linkConfig(path, secondTarget),
				ConfigPlanChecks: expectAction("pveguest_link.test", plancheck.ResourceActionCreate),
				Check:            checkGuest(expectTarget(secondTarget)),
			},
		},
	})
}

func TestAccAptPackages(t *testing.T) {
	guest := newTestGuest(t)
	t.Cleanup(guest.purgeTestPackage)
	config := guest.providerBlock() + fmt.Sprintf(`
resource "pveguest_apt_packages" "test" {
  node     = local.node
  vmid     = local.vmid
  kind     = local.kind
  packages = [%q]
}
`, testPackage)
	expectInstalled := checkGuest(func() error {
		if !guest.packageInstalled(testPackage) {
			return fmt.Errorf("package %s is not installed", testPackage)
		}
		return nil
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		// Destroy does not remove packages.
		CheckDestroy: expectInstalled,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  expectInstalled,
			},
			{
				// AC2: a package removed by hand produces a planned install.
				PreConfig:        func() { guest.mustRun("apt-get", "remove", "-y", "--", testPackage) },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_apt_packages.test", plancheck.ResourceActionUpdate),
				Check:            expectInstalled,
			},
		},
	})
}

func TestAccSystemdUnit(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	guest.useTestUnits()
	triggerPath := testDirectory + "/trigger.conf"
	triggerContent := "trigger = 1\n"
	config := guest.fileConfig(triggerPath, triggerContent, "") + fmt.Sprintf(`
resource "pveguest_systemd_unit" "timer" {
  node    = local.node
  vmid    = local.vmid
  kind    = local.kind
  name    = %q
  enabled = true
  active  = true
  restart_on = {
    trigger = pveguest_file.test.write_id
  }
}
`, testTimer)

	activeSince := func() string {
		output := guest.mustRun("systemctl", "show", "--property", "ActiveEnterTimestampMonotonic", "--value", "--", testTimer)
		return strings.TrimSpace(output)
	}
	expectEnabledAndActive := checkGuest(func() error {
		enabled := guest.systemctlState("is-enabled", testTimer)
		active := guest.systemctlState("is-active", testTimer)
		if enabled != "enabled" || active != "active" {
			return fmt.Errorf("timer is %s and %s, want enabled and active", enabled, active)
		}
		return nil
	})
	var activeSinceBeforeEdit string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  expectEnabledAndActive,
			},
			{
				// AC3: a hand edit of the file produces a planned rewrite,
				// and the apply restarts the unit that lists the file.
				PreConfig: func() {
					activeSinceBeforeEdit = activeSince()
					guest.writeFile(triggerPath, "edited by hand\n")
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("pveguest_file.test", plancheck.ResourceActionUpdate),
						plancheck.ExpectResourceAction("pveguest_systemd_unit.timer", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					checkGuest(guest.expectFileContent(triggerPath, triggerContent)),
					expectEnabledAndActive,
					checkGuest(func() error {
						activeSinceAfterApply := activeSince()
						if activeSinceAfterApply == activeSinceBeforeEdit {
							return fmt.Errorf("the timer was not restarted: active since %s", activeSinceAfterApply)
						}
						return nil
					}),
				),
			},
			{
				// An apply without changes does not restart the unit.
				PreConfig: func() { activeSinceBeforeEdit = activeSince() },
				Config:    config,
				Check: checkGuest(func() error {
					if activeSince() != activeSinceBeforeEdit {
						return fmt.Errorf("the timer was restarted by an apply without changes")
					}
					return nil
				}),
			},
			{
				// AC4: a timer disabled by hand produces a planned enable.
				PreConfig:        func() { guest.mustRun("systemctl", "disable", "--", testTimer) },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_systemd_unit.timer", plancheck.ResourceActionUpdate),
				Check:            expectEnabledAndActive,
			},
			{
				// A unit stopped by hand produces a planned start.
				PreConfig:        func() { guest.mustRun("systemctl", "stop", "--", testTimer) },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_systemd_unit.timer", plancheck.ResourceActionUpdate),
				Check:            expectEnabledAndActive,
			},
			{
				// A missing unit is a normal Read result: the plan succeeds
				// and creates the resource.
				PreConfig: func() {
					guest.mustRun("systemctl", "disable", "--now", "--", testTimer)
					guest.mustRun("rm", "-f", "--", unitDirectory+"/"+testTimer)
					guest.mustRun("systemctl", "daemon-reload")
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PostApplyPostRefresh: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("pveguest_systemd_unit.timer", plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction("pveguest_file.test", plancheck.ResourceActionNoop),
					},
				},
			},
		},
	})
}
