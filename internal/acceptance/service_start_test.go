package acceptance_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
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

	unrelatedStagedPath = "/usr/sbin/policy-rc.d.pveguest-staged.keep"
	unrelatedConfigPath = "/run/pveguest-apt-policy.4000001.conf"
	killedScriptPattern = "^bash -c .*pveguest-package-install[.]lock"

	servicePackage    = "pveguest-acc-svc"
	serviceUnit       = "pveguest-acc-svc.service"
	serviceDebPath    = testDirectory + "/pveguest-acc-svc.deb"
	postinstSleepPath = testDirectory + "/postinst-sleep"

	secondServicePackage = "pveguest-acc-svc-two"
	secondServiceUnit    = "pveguest-acc-svc-two.service"
	thirdServicePackage  = "pveguest-acc-svc-three"
	thirdServiceUnit     = "pveguest-acc-svc-three.service"
	fourthServicePackage = "pveguest-acc-svc-four"
	fourthServiceUnit    = "pveguest-acc-svc-four.service"

	killedApplySleepSeconds    = 30
	twoProcessSleepSeconds     = 20
	lockHoldSeconds            = 20
	ownedFilePairCount         = 2
	postinstTimestampLineCount = 2
	nanosecondsPerMillisecond  = 1_000_000
	postinstTimestampBase      = 10
	postinstTimestampBitSize   = 64

	guestWaitTimeout  = 3 * time.Minute
	guestPollInterval = 2 * time.Second

	lockHeldExitCode = 1
)

type serviceDebFile struct {
	source string
	target string
	mode   os.FileMode
}

func buildServiceDeb(t *testing.T, name string, failingPostinst bool) []byte {
	t.Helper()
	postinstSource := "postinst"
	if failingPostinst {
		postinstSource = "postinst-failing"
	}
	files := []serviceDebFile{
		{"control", "DEBIAN/control", 0o644},
		{postinstSource, "DEBIAN/postinst", 0o755},
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
		renamed := strings.ReplaceAll(string(content), servicePackage, name)
		target := filepath.Join(root, strings.ReplaceAll(file.target, servicePackage, name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(renamed), file.mode); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(t.TempDir(), name+".deb")
	command := exec.CommandContext(t.Context(), "dpkg-deb", "--root-owner-group", "--build", root, output)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dpkg-deb failed: %v; output: %s", err, combined)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// The first test that records a package set stores it as the baseline.
// TestAccGuestIsClean compares the guest's installed packages with the baseline.
var (
	baselineMutex    sync.Mutex
	baselinePackages map[string]bool
)

func recordBaseline(installed map[string]bool) {
	baselineMutex.Lock()
	defer baselineMutex.Unlock()
	if baselinePackages == nil {
		baselinePackages = installed
	}
}

// installedPackages returns a set of packages dpkg lists with the status installed.
func (g *testGuest) installedPackages() map[string]bool {
	g.t.Helper()
	installed := make(map[string]bool)
	output := g.mustRun("dpkg-query", "-W", "-f=${db:Status-Abbrev}\t${Package}\n")
	for line := range strings.SplitSeq(output, "\n") {
		status, name, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		if strings.HasPrefix(status, "ii") || strings.HasPrefix(status, "hi") {
			installed[name] = true
		}
	}
	return installed
}

// packagesAbsentFrom returns sorted package names in the current set but absent from the earlier set.
func packagesAbsentFrom(before map[string]bool, current map[string]bool) []string {
	var added []string
	for name := range current {
		if !before[name] {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	return added
}

// useAddedPackagePurge records the installed packages and registers cleanup
// that purges packages the test added. Cleanup does not run autoremove.
func (g *testGuest) useAddedPackagePurge() {
	g.t.Helper()
	before := g.installedPackages()
	recordBaseline(before)
	g.t.Cleanup(func() {
		added := packagesAbsentFrom(before, g.installedPackages())
		if len(added) == 0 {
			return
		}
		g.mustRun(append([]string{"apt-get", "purge", "-y", "--"}, added...)...)
	})
}

// useServicePackage builds a test .deb file with an enabled pveguest-acc-svc.service
// unit, writes the file to the guest, and purges the package after the test.
// The argument selects the variant with a postinst script that exits with status 1.
func (g *testGuest) useServicePackage(failingPostinst bool) {
	g.t.Helper()
	g.useNamedServicePackage(servicePackage, failingPostinst)
}

func (g *testGuest) useNamedServicePackage(name string, failingPostinst bool) {
	g.t.Helper()
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		g.t.Skip("The controller lacks dpkg-deb, which the test requires to build its .deb file.")
	}
	if g.packageInstalled(name) {
		g.t.Fatalf("%s is already installed in the test guest; purge it before the test", name)
	}
	g.useTestDirectory()
	g.useInstallLockCleanup()
	g.useAddedPackagePurge()
	g.t.Cleanup(func() { g.run(nil, "systemctl", "stop", "--", name+".service") })
	g.writeFile(serviceDebPathFor(name), string(buildServiceDeb(g.t, name, failingPostinst)))
}

// useInstallLockCleanup registers a cleanup that deletes the package install lock file.
func (g *testGuest) useInstallLockCleanup() {
	g.t.Helper()
	g.t.Cleanup(func() { g.mustRun("rm", "-f", "--", installLockPath) })
}

func serviceDebPathFor(name string) string {
	return testDirectory + "/" + name + ".deb"
}

func (g *testGuest) debPackagesBlockFor(name string, extra string) string {
	return fmt.Sprintf(`
resource "pveguest_deb_packages" "test" {
  node     = local.node
  vmid     = local.vmid
  kind     = local.kind
  packages = { %s = %q }
  %s
}
`, name, serviceDebPathFor(name), extra)
}

func (g *testGuest) debPackagesBlock(extra string) string {
	return g.debPackagesBlockFor(servicePackage, extra)
}

func (g *testGuest) debPackagesConfig(extra string) string {
	return g.providerBlock() + g.debPackagesBlock(extra)
}

func (g *testGuest) expectPackageInstalled(name string) resource.TestCheckFunc {
	return checkGuest(func() error {
		if !g.packageInstalled(name) {
			return fmt.Errorf("%s is not installed", name)
		}
		return nil
	})
}

func (g *testGuest) expectUnitState(unit string, enabled string, active string) resource.TestCheckFunc {
	return checkGuest(func() error {
		state := g.systemctlState("is-enabled", unit)
		if enabled == "" {
			g.t.Logf("%s is-enabled state: %s", unit, state)
		} else if state != enabled {
			return fmt.Errorf("%s is-enabled state is %s; expected %s", unit, state, enabled)
		}
		if state := g.systemctlState("is-active", unit); state != active {
			return fmt.Errorf("%s is-active state is %s; expected %s", unit, state, active)
		}
		return nil
	})
}

func (g *testGuest) expectNoPolicyFile() resource.TestCheckFunc {
	return checkGuest(func() error {
		if g.exists(policyFilePath) {
			return fmt.Errorf("the policy file %s exists", policyFilePath)
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
		t.Fatalf("%s is already installed in the test guest; purge it before the test", noStartPackage)
	}
	guest.useInstallLockCleanup()
	guest.useAddedPackagePurge()
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

// TestAccDebPackagesFailedInstallRemovesPolicyFile checks that the provider deletes
// the policy file after dpkg fails. The package's postinst script exits with status 1
// after the DPkg::Pre-Invoke hook.
func TestAccDebPackagesFailedInstallRemovesPolicyFile(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(true)
	config := guest.debPackagesConfig("start_services = false")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`returned\s+error\s+exit\s+status\s+1`),
			},
			{
				PreConfig: func() {
					if guest.exists(policyFilePath) {
						t.Fatalf("the policy file %s exists after the failed install", policyFilePath)
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
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
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
						t.Fatalf("%s contains %q after the refused apply; expected %q", policyFilePath, got, foreignPolicyContent)
					}
					if guest.packageInstalled(servicePackage) {
						t.Fatalf("%s is installed after the apply refused to run", servicePackage)
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
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
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
		t.Fatalf("%s is already installed in the test guest; purge it before the test", noStartPackage)
	}
	guest.useAddedPackagePurge()
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
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
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
	guest.waitFor(fmt.Sprintf("the slow installation to enable %s", serviceUnit), func() bool {
		return guest.systemctlState("is-enabled", serviceUnit) == "enabled"
	})
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	if got := guest.readFile(policyFilePath); !strings.Contains(got, policyMarker) {
		t.Fatalf("after the kill, the policy file %s contains %q; expected marker line %q", policyFilePath, got, policyMarker)
	}
	lockResult := guest.run(nil, "flock", "-n", installLockPath, "true")
	if lockResult.ExitCode != lockHeldExitCode {
		t.Fatalf("after the kill, flock -n on %s returned exit status %d; expected %d for a held lock",
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
		t.Fatalf("the plan after the recovery apply is not empty; exit status %d; output: %s", exitCode, output)
	}
}

type backgroundResult struct {
	result transport.Result
	err    error
}

// startBackground runs a guest command in a goroutine and sends the result on a channel.
func (g *testGuest) startBackground(argv ...string) <-chan backgroundResult {
	done := make(chan backgroundResult, 1)
	go func() {
		result, err := g.client.Run(context.Background(), g.guest, guestCommand(nil, argv))
		done <- backgroundResult{result: result, err: err}
	}()
	return done
}

func (g *testGuest) requireBackgroundSuccess(done <-chan backgroundResult, description string) {
	g.t.Helper()
	outcome := <-done
	if outcome.err != nil {
		g.t.Fatalf("background guest command %s failed: %v", description, outcome.err)
	}
	if outcome.result.ExitCode != 0 {
		g.t.Fatalf("background guest command %s exited with status %d; stderr: %s",
			description, outcome.result.ExitCode, outcome.result.Stderr)
	}
}

// TestAccDebPackagesOtherInstallKeepsServiceStart starts a pveguest apply while
// a plain apt-get install runs its slow postinst script. The test waits for the
// apply to create its staged file and apt configuration file, then checks that
// the plain postinst script still runs. The test fails if the installs do not overlap.
func TestAccDebPackagesOtherInstallKeepsServiceStart(t *testing.T) {
	guest := newTestGuest(t)
	guest.useNamedServicePackage(servicePackage, false)
	guest.useNamedServicePackage(secondServicePackage, false)
	if guest.exists(policyFilePath) {
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
	}
	guest.writeFile(postinstSleepPath, fmt.Sprintf("%d\n", twoProcessSleepSeconds))
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}
	workspace.writeConfig(guest.providerBlock() + guest.debPackagesBlockFor(secondServicePackage, "start_services = false"))
	workspace.mustRun("init", "-no-color")
	t.Cleanup(func() { workspace.run("destroy", "-no-color", "-auto-approve") })

	done := guest.startBackground(
		"apt-get", "install", "-y", "--no-install-recommends", "--", serviceDebPathFor(servicePackage),
	)
	guest.waitFor("start of the plain install's postinst script", func() bool {
		return guest.exists(testDirectory + "/" + servicePackage + ".times")
	})
	guest.writeFile(postinstSleepPath, "0\n")
	command, output := startApply(t, workspace)
	guest.waitFor("creation of the second install's files", func() bool { return guest.countOwnedFiles() == ownedFilePairCount })
	// The plain install's timestamp file contains one line while its postinst script
	// sleeps. This check verifies that the two installs overlap.
	if timestamps := strings.Fields(guest.readFile(testDirectory + "/" + servicePackage + ".times")); len(timestamps) != 1 {
		t.Fatalf("the plain install's postinst script ended before the second install created its files (%d timestamps); "+
			"the test did not overlap the two installs", len(timestamps))
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("the %s apply failed: %v; output: %s", "second", err, output.String())
	}
	guest.requireBackgroundSuccess(done, "the plain apt-get install")

	if err := guest.expectUnitState(serviceUnit, "enabled", "active")(nil); err != nil {
		t.Fatal(err)
	}
	if err := guest.expectUnitState(secondServiceUnit, "enabled", "inactive")(nil); err != nil {
		t.Fatal(err)
	}
	if err := guest.expectNoPolicyFile()(nil); err != nil {
		t.Fatal(err)
	}
}

// TestAccDebPackagesWaitsForInstallLock checks that the install script waits
// for the pveguest install lock. A guest command owns the lock for 20 seconds.
// The test waits for the install script process to exist, pauses for two seconds,
// and checks that the command still owns the lock and the script has not created any files.
func TestAccDebPackagesWaitsForInstallLock(t *testing.T) {
	guest := newTestGuest(t)
	guest.useServicePackage(false)
	if guest.exists(policyFilePath) {
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
	}
	workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}
	workspace.writeConfig(guest.debPackagesConfig("start_services = false"))
	workspace.mustRun("init", "-no-color")
	t.Cleanup(func() { workspace.run("destroy", "-no-color", "-auto-approve") })

	holder := guest.startBackground("flock", "-x", installLockPath, "sleep", strconv.Itoa(lockHoldSeconds))
	guest.waitFor("the lock holder command's ownership of the install lock", func() bool {
		return guest.run(nil, "flock", "-n", installLockPath, "true").ExitCode == lockHeldExitCode
	})
	command, output := startApply(t, workspace)
	guest.waitFor("the start of the install script and its wait for the install lock", func() bool {
		return guest.run(nil, "pgrep", "-f", "--", killedScriptPattern).ExitCode == 0
	})
	// After two seconds, the lock holder must still own the install lock, and the
	// install script must not have created any files.
	time.Sleep(guestPollInterval)
	if guest.run(nil, "flock", "-n", installLockPath, "true").ExitCode != lockHeldExitCode {
		t.Fatal("the lock holder ended before the test observed the waiting script")
	}
	if count := guest.countOwnedFiles(); count != 0 {
		t.Fatalf("the install script created %d files while the lock holder owned the install lock", count)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("the %s apply failed: %v; output: %s", "only", err, output.String())
	}
	guest.requireBackgroundSuccess(holder, "the lock holder command")

	if err := guest.expectPackageInstalled(servicePackage)(nil); err != nil {
		t.Fatal(err)
	}
	if err := guest.expectUnitState(serviceUnit, "enabled", "inactive")(nil); err != nil {
		t.Fatal(err)
	}
	if err := guest.expectNoPolicyFile()(nil); err != nil {
		t.Fatal(err)
	}
}

type ownedFilePattern struct {
	directory string
	name      string
}

var ownedFilePatterns = []ownedFilePattern{
	{directory: "/usr/sbin", name: "policy-rc.d.pveguest-staged.*"},
	{directory: "/run", name: "pveguest-apt-policy.*.conf"},
}

func (g *testGuest) findFiles(directory string, name string) []string {
	g.t.Helper()
	return strings.Fields(g.mustRun("find", directory, "-maxdepth", "1", "-name", name))
}

func (g *testGuest) ownedFilesIn(pattern ownedFilePattern) []string {
	g.t.Helper()
	var owned []string
	for _, path := range g.findFiles(pattern.directory, pattern.name) {
		if path != unrelatedStagedPath && path != unrelatedConfigPath {
			owned = append(owned, path)
		}
	}
	return owned
}

// TestAccDebPackagesRemovesFilesOfKilledScript kills a guest install script
// with SIGKILL and checks that the next install removes its marked files,
// preserves unrelated files, and installs another package.
func TestAccDebPackagesRemovesFilesOfKilledScript(t *testing.T) {
	guest := newTestGuest(t)
	guest.useNamedServicePackage(servicePackage, false)
	guest.useNamedServicePackage(secondServicePackage, false)
	if guest.exists(policyFilePath) {
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
	}
	guest.writeFile(unrelatedStagedPath, "#!/bin/sh\n"+policyMarker+"\nexit 101\n")
	guest.writeFile(unrelatedConfigPath, "DPkg::Pre-Invoke:: \"true\";\n")
	t.Cleanup(func() { guest.mustRun("rm", "-f", "--", unrelatedStagedPath, unrelatedConfigPath) })
	guest.writeFile(postinstSleepPath, fmt.Sprintf("%d\n", killedApplySleepSeconds))

	binary := requireVariable(t, tofuPathVariable)
	first := &tofuWorkspace{t: t, directory: t.TempDir(), binary: binary}
	second := &tofuWorkspace{t: t, directory: t.TempDir(), binary: binary}
	first.writeConfig(guest.providerBlock() + guest.debPackagesBlockFor(servicePackage, "start_services = false"))
	second.writeConfig(guest.providerBlock() + guest.debPackagesBlockFor(secondServicePackage, "start_services = false"))
	first.mustRun("init", "-no-color")
	second.mustRun("init", "-no-color")
	t.Cleanup(func() { first.run("destroy", "-no-color", "-auto-approve") })
	t.Cleanup(func() { second.run("destroy", "-no-color", "-auto-approve") })

	firstCommand, _ := startApply(t, first)
	guest.waitFor(fmt.Sprintf("the slow installation to enable %s", serviceUnit), func() bool {
		return guest.systemctlState("is-enabled", serviceUnit) == "enabled"
	})
	stagedFiles := guest.ownedFilesIn(ownedFilePatterns[0])
	configFiles := guest.ownedFilesIn(ownedFilePatterns[1])
	if len(stagedFiles) != 1 || len(configFiles) != 1 {
		t.Fatalf("The running install has %d staged files and %d apt configuration files; the test expects one of each.",
			len(stagedFiles), len(configFiles))
	}
	staleFiles := append(stagedFiles, configFiles...)
	// Match the guest script by the lock path in its command line and kill it.
	// The apt-get process that the script started continues to run.
	guest.mustRun("pkill", "-KILL", "-f", "--", killedScriptPattern)
	_ = firstCommand.Wait()
	for _, path := range staleFiles {
		if !guest.exists(path) {
			t.Fatalf("The killed script should leave %s in place, but the file is missing.", path)
		}
	}

	guest.writeFile(postinstSleepPath, "0\n")
	guest.waitFor("the end of the first install's surviving apt-get process", func() bool {
		return guest.packageInstalled(servicePackage) && guest.run(nil, "pgrep", "-x", "apt-get").ExitCode != 0
	})
	for _, path := range staleFiles {
		if !guest.exists(path) {
			t.Fatalf("file %s is missing before the later install can delete it", path)
		}
	}
	second.mustRun("apply", "-no-color", "-auto-approve")

	for _, path := range staleFiles {
		if guest.exists(path) {
			t.Fatalf("The next install should delete %s, but the file remains.", path)
		}
	}
	for _, path := range []string{unrelatedStagedPath, unrelatedConfigPath} {
		if !guest.exists(path) {
			t.Fatalf("The next install should leave %s in place, but the file is missing.", path)
		}
	}
	for _, name := range []string{servicePackage, secondServicePackage} {
		if err := guest.expectPackageInstalled(name)(nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, unit := range []string{serviceUnit, secondServiceUnit} {
		if err := guest.expectUnitState(unit, "enabled", "inactive")(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := guest.expectNoPolicyFile()(nil); err != nil {
		t.Fatal(err)
	}
}

func (g *testGuest) allOwnedFiles() []string {
	g.t.Helper()
	var owned []string
	for _, pattern := range ownedFilePatterns {
		owned = append(owned, g.ownedFilesIn(pattern)...)
	}
	return owned
}

func (g *testGuest) countOwnedFiles() int {
	g.t.Helper()
	return len(g.allOwnedFiles())
}

// TestAccDebPackagesKeepsFilesWhileAptWaitsForDpkgLock kills a guest
// script while its apt-get waits for the dpkg lock. It checks that a
// later install retains the files, that the surviving apt-get installs
// its package without starting services, and that another install
// deletes the stale files.
func TestAccDebPackagesKeepsFilesWhileAptWaitsForDpkgLock(t *testing.T) {
	guest := newTestGuest(t)
	for _, name := range []string{servicePackage, secondServicePackage, thirdServicePackage, fourthServicePackage} {
		guest.useNamedServicePackage(name, false)
	}
	if guest.exists(policyFilePath) {
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
	}
	guest.writeFile(postinstSleepPath, fmt.Sprintf("%d\n", killedApplySleepSeconds))

	binary := requireVariable(t, tofuPathVariable)
	workspaces := make(map[string]*tofuWorkspace)
	for _, name := range []string{secondServicePackage, thirdServicePackage, fourthServicePackage} {
		workspace := &tofuWorkspace{t: t, directory: t.TempDir(), binary: binary}
		workspace.writeConfig(guest.providerBlock() + guest.debPackagesBlockFor(name, "start_services = false"))
		workspace.mustRun("init", "-no-color")
		t.Cleanup(func() { workspace.run("destroy", "-no-color", "-auto-approve") })
		workspaces[name] = workspace
	}

	plainInstall := guest.startBackground(
		"apt-get", "install", "-y", "--no-install-recommends", "--", serviceDebPathFor(servicePackage),
	)
	guest.waitFor("start of the plain install's postinst script", func() bool {
		return guest.exists(testDirectory + "/" + servicePackage + ".times")
	})
	guest.writeFile(postinstSleepPath, "0\n")

	secondCommand, _ := startApply(t, workspaces[secondServicePackage])
	guest.waitFor("creation of the second install's files", func() bool { return guest.countOwnedFiles() == ownedFilePairCount })
	secondFiles := guest.allOwnedFiles()
	// Kill the guest script while its apt-get waits for the dpkg lock.
	// The apt-get process continues running.
	guest.mustRun("pkill", "-KILL", "-f", "--", killedScriptPattern)
	_ = secondCommand.Wait()

	thirdCommand, thirdOutput := startApply(t, workspaces[thirdServicePackage])
	guest.waitFor("creation of the third install's files", func() bool { return guest.countOwnedFiles() == 2*ownedFilePairCount })
	for _, path := range secondFiles {
		if !guest.exists(path) {
			t.Fatalf("file %s is missing while the surviving apt-get waits for the dpkg lock", path)
		}
	}

	guest.requireBackgroundSuccess(plainInstall, "the plain apt-get install")
	if err := thirdCommand.Wait(); err != nil {
		t.Fatalf("the %s apply failed: %v; output: %s", "third", err, thirdOutput.String())
	}
	guest.waitFor("installation of the second package and exit of the surviving apt-get", func() bool {
		return guest.packageInstalled(secondServicePackage) && guest.run(nil, "pgrep", "-x", "apt-get").ExitCode != 0
	})
	for _, path := range secondFiles {
		if !guest.exists(path) {
			t.Fatalf("file %s is missing before the later install can delete it", path)
		}
	}

	workspaces[fourthServicePackage].mustRun("apply", "-no-color", "-auto-approve")
	for _, path := range secondFiles {
		if guest.exists(path) {
			t.Fatalf("The next install should delete %s, but the file remains.", path)
		}
	}
	for _, name := range []string{servicePackage, secondServicePackage, thirdServicePackage, fourthServicePackage} {
		if err := guest.expectPackageInstalled(name)(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := guest.expectUnitState(serviceUnit, "enabled", "active")(nil); err != nil {
		t.Fatal(err)
	}
	for _, unit := range []string{secondServiceUnit, thirdServiceUnit, fourthServiceUnit} {
		if err := guest.expectUnitState(unit, "enabled", "inactive")(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := guest.expectNoPolicyFile()(nil); err != nil {
		t.Fatal(err)
	}
	if remaining := guest.allOwnedFiles(); len(remaining) > 0 {
		t.Fatalf("temporary files from the installs remain after both installs finished: %s", strings.Join(remaining, ", "))
	}
}

// startApply starts a tofu apply process and returns the process and its output buffer.
func startApply(t *testing.T, workspace *tofuWorkspace) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	var output bytes.Buffer
	command := exec.CommandContext(t.Context(), workspace.binary, "apply", "-no-color", "-auto-approve")
	command.Dir = workspace.directory
	command.Env = append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", reattachVariableName+"=")
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return command, &output
}

// postinstWindow returns the nanosecond timestamps that the postinst script
// writes before and after its sleep.
func (g *testGuest) postinstWindow(name string) (int64, int64) {
	g.t.Helper()
	lines := strings.Fields(g.readFile(testDirectory + "/" + name + ".times"))
	if len(lines) != postinstTimestampLineCount {
		g.t.Fatalf("%s has %d timestamps; expected %d", name, len(lines), postinstTimestampLineCount)
	}
	start, err := strconv.ParseInt(lines[0], postinstTimestampBase, postinstTimestampBitSize)
	if err != nil {
		g.t.Fatal(err)
	}
	end, err := strconv.ParseInt(lines[1], postinstTimestampBase, postinstTimestampBitSize)
	if err != nil {
		g.t.Fatal(err)
	}
	return start, end
}

// TestAccDebPackagesTwoProcessesShareLock runs two OpenTofu applies at the
// same time against one guest. It checks that both succeed, the second postinst
// starts after the first ends, both units are enabled and inactive, and the
// policy file does not exist.
func TestAccDebPackagesTwoProcessesShareLock(t *testing.T) {
	guest := newTestGuest(t)
	guest.useNamedServicePackage(servicePackage, false)
	guest.useNamedServicePackage(secondServicePackage, false)
	if guest.exists(policyFilePath) {
		t.Fatalf("the policy file %s exists in the test guest; remove it before the test", policyFilePath)
	}
	guest.writeFile(postinstSleepPath, fmt.Sprintf("%d\n", twoProcessSleepSeconds))

	binary := requireVariable(t, tofuPathVariable)
	first := &tofuWorkspace{t: t, directory: t.TempDir(), binary: binary}
	second := &tofuWorkspace{t: t, directory: t.TempDir(), binary: binary}
	first.writeConfig(guest.providerBlock() + guest.debPackagesBlockFor(servicePackage, "start_services = false"))
	second.writeConfig(guest.providerBlock() + guest.debPackagesBlockFor(secondServicePackage, "start_services = false"))
	first.mustRun("init", "-no-color")
	second.mustRun("init", "-no-color")
	t.Cleanup(func() { first.run("destroy", "-no-color", "-auto-approve") })
	t.Cleanup(func() { second.run("destroy", "-no-color", "-auto-approve") })

	firstCommand, firstOutput := startApply(t, first)
	// The first postinst script enables its unit before sleeping.
	// Start the second apply during that sleep.
	guest.waitFor(fmt.Sprintf("the first installation to enable %s", serviceUnit), func() bool {
		return guest.systemctlState("is-enabled", serviceUnit) == "enabled"
	})
	secondCommand, secondOutput := startApply(t, second)
	firstErr := firstCommand.Wait()
	secondErr := secondCommand.Wait()
	if firstErr != nil {
		t.Fatalf("the %s apply failed: %v; output: %s", "first", firstErr, firstOutput.String())
	}
	if secondErr != nil {
		t.Fatalf("the %s apply failed: %v; output: %s", "second", secondErr, secondOutput.String())
	}

	firstStart, firstEnd := guest.postinstWindow(servicePackage)
	secondStart, secondEnd := guest.postinstWindow(secondServicePackage)
	t.Logf("postinst times in nanoseconds since the epoch: first start %d, first end %d, second start %d, second end %d",
		firstStart, firstEnd, secondStart, secondEnd)
	t.Logf("the second postinst started %d milliseconds after the first postinst ended",
		(secondStart-firstEnd)/nanosecondsPerMillisecond)
	if secondStart < firstEnd {
		t.Fatalf("the second install started at %d before the first install ended at %d", secondStart, firstEnd)
	}

	for _, name := range []string{servicePackage, secondServicePackage} {
		if err := guest.expectPackageInstalled(name)(nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, unit := range []string{serviceUnit, secondServiceUnit} {
		if err := guest.expectUnitState(unit, "enabled", "inactive")(nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := guest.expectNoPolicyFile()(nil); err != nil {
		t.Fatal(err)
	}
}
