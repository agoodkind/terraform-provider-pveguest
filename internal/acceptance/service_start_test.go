package acceptance_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const (
	// The Debian package at ships an enabled atd.service and does not affect other services.
	// The test purges at after it finishes.
	noStartPackage = "at"
	noStartUnit    = "atd.service"

	policyFilePath  = "/usr/sbin/policy-rc.d"
	installLockPath = "/run/pveguest-package-install.lock"

	// policyMarker must equal the marker line that the provider writes.
	policyMarker = "# The pveguest provider created this file for a package install."

	foreignPolicyContent = "#!/bin/sh\nexit 0\n"

	servicePackage    = "pveguest-acc-svc"
	serviceUnit       = "pveguest-acc-svc.service"
	serviceDebPath    = testDirectory + "/pveguest-acc-svc.deb"
	postinstSleepPath = testDirectory + "/postinst-sleep"

	killedApplySleepSeconds = 30

	guestWaitTimeout  = 3 * time.Minute
	guestPollInterval = 2 * time.Second

	lockHeldExitCode = 1
)

type serviceDebFile struct {
	source string
	target string
	mode   os.FileMode
}

func buildServiceDeb(t *testing.T, unsatisfiable bool) []byte {
	t.Helper()
	controlSource := "control"
	if unsatisfiable {
		controlSource = "control-unsatisfiable"
	}
	files := []serviceDebFile{
		{controlSource, "DEBIAN/control", 0o644},
		{"postinst", "DEBIAN/postinst", 0o755},
		{"prerm", "DEBIAN/prerm", 0o755},
		{"postrm", "DEBIAN/postrm", 0o755},
		{"pveguest-acc-svc.service", "usr/lib/systemd/system/pveguest-acc-svc.service", 0o644},
	}
	root := filepath.Join(t.TempDir(), "package")
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join("testdata", "svcdeb", file.source))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, file.target)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(t.TempDir(), servicePackage+".deb")
	command := exec.CommandContext(t.Context(), "dpkg-deb", "--root-owner-group", "--build", root, output)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dpkg-deb: %v: %s", err, combined)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// useServicePackage builds a test .deb file with an enabled pveguest-acc-svc.service
// unit, writes it to the guest, and purges the package after the test.
// The argument selects a variant with an unsatisfiable dependency.
func (g *testGuest) useServicePackage(unsatisfiable bool) {
	g.t.Helper()
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		g.t.Skip("dpkg-deb is missing on the controller, and the test builds its .deb file with dpkg-deb")
	}
	if g.packageInstalled(servicePackage) {
		g.t.Fatalf("package %s is already installed in the test guest; purge it before the test", servicePackage)
	}
	g.useTestDirectory()
	g.t.Cleanup(func() {
		g.run(nil, "systemctl", "stop", "--", serviceUnit)
		g.mustRun("apt-get", "purge", "-y", "--", servicePackage)
	})
	g.writeFile(serviceDebPath, string(buildServiceDeb(g.t, unsatisfiable)))
}

func (g *testGuest) debPackagesBlock(extra string) string {
	return fmt.Sprintf(`
resource "pveguest_deb_packages" "test" {
  node     = local.node
  vmid     = local.vmid
  kind     = local.kind
  packages = { %s = %q }
  %s
}
`, servicePackage, serviceDebPath, extra)
}

func (g *testGuest) debPackagesConfig(extra string) string {
	return g.providerBlock() + g.debPackagesBlock(extra)
}

func (g *testGuest) expectPackageInstalled(name string) resource.TestCheckFunc {
	return checkGuest(func() error {
		if !g.packageInstalled(name) {
			return fmt.Errorf("package %s is not installed", name)
		}
		return nil
	})
}

// expectUnitState compares the is-active state of the unit. An empty enabled
// argument logs the is-enabled state and does not compare it.
func (g *testGuest) expectUnitState(unit string, enabled string, active string) resource.TestCheckFunc {
	return checkGuest(func() error {
		state := g.systemctlState("is-enabled", unit)
		if enabled == "" {
			g.t.Logf("%s is-enabled state: %s", unit, state)
		} else if state != enabled {
			return fmt.Errorf("%s is %s, want %s", unit, state, enabled)
		}
		if state := g.systemctlState("is-active", unit); state != active {
			return fmt.Errorf("%s is %s, want %s", unit, state, active)
		}
		return nil
	})
}

func (g *testGuest) expectNoPolicyFile() resource.TestCheckFunc {
	return checkGuest(func() error {
		if g.exists(policyFilePath) {
			return fmt.Errorf("%s exists", policyFilePath)
		}
		return nil
	})
}

func (g *testGuest) waitFor(description string, condition func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(guestWaitTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out after %s waiting for %s", guestWaitTimeout, description)
		}
		time.Sleep(guestPollInterval)
	}
}

func TestAccAptPackagesNoServiceStart(t *testing.T) {
	guest := newTestGuest(t)
	if guest.packageInstalled(noStartPackage) {
		t.Fatalf("package %s is already installed in the test guest; purge it before the test", noStartPackage)
	}
	t.Cleanup(func() { guest.mustRun("apt-get", "purge", "-y", "--", noStartPackage) })
	config := guest.aptPackagesConfig([]string{noStartPackage}, "start_services = false")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_apt_packages.test", "start_services", "false"),
					guest.expectPackageInstalled(noStartPackage),
					// A default package install starts an enabled service. An enabled and
					// inactive unit shows that the install did not start the service.
					guest.expectUnitState(noStartUnit, "", "inactive"),
					guest.expectNoPolicyFile(),
				),
			},
		},
	})
}

func TestAccDebPackagesNoServiceStart(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(false)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: guest.debPackagesConfig("start_services = false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					guest.expectPackageInstalled(servicePackage),
					guest.expectUnitState(serviceUnit, "enabled", "inactive"),
					guest.expectNoPolicyFile(),
				),
			},
		},
	})
}

func TestAccDebPackagesStartServicesDefault(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(false)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: guest.debPackagesConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					guest.expectPackageInstalled(servicePackage),
					guest.expectUnitState(serviceUnit, "enabled", "active"),
					guest.expectNoPolicyFile(),
				),
			},
		},
	})
}

func TestAccDebPackagesFailedInstallRemovesPolicyFile(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(true)
	config := guest.debPackagesConfig("start_services = false")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`pveguest-acc-missing-dependency`),
			},
			{
				PreConfig: func() {
					if guest.exists(policyFilePath) {
						t.Fatalf("%s exists after the failed install", policyFilePath)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccDebPackagesRefuseForeignPolicyFile(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(false)
	if guest.exists(policyFilePath) {
		t.Fatalf("%s exists in the test guest; remove it before the test", policyFilePath)
	}
	t.Cleanup(func() { guest.mustRun("rm", "-f", "--", policyFilePath) })
	config := guest.debPackagesConfig("start_services = false")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					guest.writeFile(policyFilePath, foreignPolicyContent)
					guest.mustRun("chmod", "0755", "--", policyFilePath)
				},
				Config:      config,
				ExpectError: regexp.MustCompile(`policy-rc\.d`),
			},
			{
				PreConfig: func() {
					if got := guest.readFile(policyFilePath); got != foreignPolicyContent {
						t.Fatalf("%s contains %q after the refused apply, want %q", policyFilePath, got, foreignPolicyContent)
					}
					if guest.packageInstalled(servicePackage) {
						t.Fatalf("package %s is installed although the apply refused to run", servicePackage)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccDebPackagesRecoverMarkedPolicyFile(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(false)
	if guest.exists(policyFilePath) {
		t.Fatalf("%s exists in the test guest; remove it before the test", policyFilePath)
	}
	t.Cleanup(func() { guest.mustRun("rm", "-f", "--", policyFilePath) })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					guest.writeFile(policyFilePath, "#!/bin/sh\n"+policyMarker+"\nexit 101\n")
					guest.mustRun("chmod", "0755", "--", policyFilePath)
				},
				Config: guest.debPackagesConfig("start_services = false"),
				Check: resource.ComposeAggregateTestCheckFunc(
					guest.expectPackageInstalled(servicePackage),
					guest.expectUnitState(serviceUnit, "enabled", "inactive"),
					guest.expectNoPolicyFile(),
				),
			},
		},
	})
}

func TestAccPackagesApplyTogetherWithoutServiceStart(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(false)
	if guest.packageInstalled(noStartPackage) {
		t.Fatalf("package %s is already installed in the test guest; purge it before the test", noStartPackage)
	}
	t.Cleanup(func() { guest.mustRun("apt-get", "purge", "-y", "--", noStartPackage) })
	aptBlock := strings.TrimPrefix(
		guest.aptPackagesConfig([]string{noStartPackage}, "start_services = false"),
		guest.providerBlock(),
	)
	config := guest.providerBlock() + aptBlock + guest.debPackagesBlock("start_services = false")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					guest.expectPackageInstalled(noStartPackage),
					guest.expectPackageInstalled(servicePackage),
					guest.expectUnitState(noStartUnit, "", "inactive"),
					guest.expectUnitState(serviceUnit, "enabled", "inactive"),
					guest.expectNoPolicyFile(),
				),
			},
			{
				Config:           config,
				PlanOnly:         true,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// TestAccDebPackagesRecoverAfterKilledApply starts a real tofu apply with a
// sleeping package postinst script and sends SIGKILL to tofu during the install.
// It checks that the guest still has the policy file and the locked lock file,
// then runs a second apply that waits for the lock and succeeds.
func TestAccDebPackagesRecoverAfterKilledApply(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(false)
	if guest.exists(policyFilePath) {
		t.Fatalf("%s exists in the test guest; remove it before the test", policyFilePath)
	}
	guest.writeFile(postinstSleepPath, fmt.Sprintf("%d\n", killedApplySleepSeconds))
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}
	workspace.writeConfig(guest.debPackagesConfig("start_services = false"))
	workspace.mustRun("init", "-no-color")
	t.Cleanup(func() { workspace.run("destroy", "-no-color", "-auto-approve") })

	command := exec.CommandContext(t.Context(), workspace.binary, "apply", "-no-color", "-auto-approve")
	command.Dir = workspace.directory
	command.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", reattachVariableName+"=")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// The postinst script enables the unit before it sleeps. An enabled unit
	// shows that the install is running in that slow step.
	guest.waitFor("the slow installation to enable "+serviceUnit, func() bool {
		return guest.systemctlState("is-enabled", serviceUnit) == "enabled"
	})
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	if got := guest.readFile(policyFilePath); !strings.Contains(got, policyMarker) {
		t.Fatalf("%s contains %q after the kill, want the marker line %q", policyFilePath, got, policyMarker)
	}
	lockResult := guest.run(nil, "flock", "-n", installLockPath, "true")
	if lockResult.ExitCode != lockHeldExitCode {
		t.Fatalf("flock -n on %s exited with status %d after the kill, want %d (lock held)",
			installLockPath, lockResult.ExitCode, lockHeldExitCode)
	}

	guest.writeFile(postinstSleepPath, "0\n")
	workspace.mustRun("apply", "-no-color", "-auto-approve")

	if err := guest.expectPackageInstalled(servicePackage)(nil); err != nil {
		t.Fatal(err)
	}
	if err := guest.expectUnitState(serviceUnit, "enabled", "inactive")(nil); err != nil {
		t.Fatal(err)
	}
	if err := guest.expectNoPolicyFile()(nil); err != nil {
		t.Fatal(err)
	}
	output, exitCode := workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != 0 {
		t.Fatalf("plan after the recovery apply exited with status %d:\n%s", exitCode, output)
	}
}
