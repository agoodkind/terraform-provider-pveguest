package acceptance_test

import (
	"strings"
	"testing"
)

// TestAccGuestIsClean runs after every other test of the package, because
// go test runs files in name order. It fails when a test left one of its
// items in the guest.
func TestAccGuestIsClean(t *testing.T) {
	guest := newTestGuest(t)

	leftoverPaths := []string{
		testDirectory,
		unitDirectory + "/" + testTimer,
		unitDirectory + "/" + testService,
		unitDirectory + "/timers.target.wants/" + testTimer,
		unitDirectory + "/" + testRemovalService,
		unitDirectory + "/multi-user.target.wants/" + testRemovalService,
		unitDirectory + "/" + testImportService,
		unitDirectory + "/multi-user.target.wants/" + testImportService,
		guestWorkDirectory,
		guestCAPath,
		guestCALink,
		guestCAHashLink,
		policyFilePath,
		installLockPath,
		serviceDebPath,
		"/etc/systemd/system/multi-user.target.wants/" + serviceUnit,
		"/etc/systemd/system/multi-user.target.wants/" + secondServiceUnit,
	}
	for _, path := range leftoverPaths {
		if guest.exists(path) {
			t.Errorf("%s exists after the tests", path)
		}
	}
	if guest.packageInstalled(testPackage) {
		t.Errorf("package %s is installed after the tests", testPackage)
	}
	if guest.packageInstalled(noStartPackage) {
		t.Errorf("%s is installed after the tests", noStartPackage)
	}
	if state := guest.systemctlState("is-active", noStartUnit); state != "inactive" {
		t.Errorf("%s has state %s after the tests; expected inactive", noStartUnit, state)
	}
	if guest.packageInstalled(servicePackage) {
		t.Errorf("%s is installed after the tests", servicePackage)
	}
	if state := guest.systemctlState("is-active", serviceUnit); state != "inactive" {
		t.Errorf("%s has state %s after the tests; expected inactive", serviceUnit, state)
	}
	if guest.packageInstalled(secondServicePackage) {
		t.Errorf("%s is installed after the tests", secondServicePackage)
	}
	if state := guest.systemctlState("is-active", secondServiceUnit); state != "inactive" {
		t.Errorf("%s has state %s after the tests; expected inactive", secondServiceUnit, state)
	}
	if guest.packageInstalled(thirdServicePackage) {
		t.Errorf("%s is installed after the tests", thirdServicePackage)
	}
	if state := guest.systemctlState("is-active", thirdServiceUnit); state != "inactive" {
		t.Errorf("%s has state %s after the tests; expected inactive", thirdServiceUnit, state)
	}
	if guest.packageInstalled(fourthServicePackage) {
		t.Errorf("%s is installed after the tests", fourthServicePackage)
	}
	if state := guest.systemctlState("is-active", fourthServiceUnit); state != "inactive" {
		t.Errorf("%s has state %s after the tests; expected inactive", fourthServiceUnit, state)
	}
	installed := guest.installedPackages()
	recordBaseline(installed)
	baselineMutex.Lock()
	baseline := baselinePackages
	baselineMutex.Unlock()
	if added := packagesAbsentFrom(baseline, installed); len(added) > 0 {
		t.Errorf("Packages added by the tests remain installed: %s.", strings.Join(added, ", "))
	}
	for _, pattern := range ownedFilePatterns {
		if remaining := guest.findFiles(pattern.directory, pattern.name); len(remaining) > 0 {
			t.Errorf("Files matching %s remain after the tests: %s", pattern.directory+"/"+pattern.name, strings.Join(remaining, ", "))
		}
	}
	hasSysrepo := guest.run(nil, "sh", "-c", "command -v sysrepoctl").ExitCode == 0
	if hasSysrepo && guest.sysrepoModuleRow() != "" {
		t.Errorf("sysrepo module %s is installed after the tests", testSysrepoModule)
	}
	if state := guest.systemctlState("is-active", testTimer); state != "inactive" {
		t.Errorf("%s is %s after the tests, want inactive", testTimer, state)
	}
	removalState := guest.systemctlState("is-active", testRemovalService)
	if removalState != "inactive" && removalState != "failed" {
		t.Errorf("%s is %s after the tests, want inactive or failed", testRemovalService, removalState)
	}
	importState := guest.systemctlState("is-active", testImportService)
	if importState != "inactive" && importState != "failed" {
		t.Errorf("%s is %s after the tests, want inactive or failed", testImportService, importState)
	}
	headerServerUnit := guestServerUnit + ".service"
	if state := guest.systemctlState("is-active", headerServerUnit); state != "inactive" {
		t.Errorf("%s is %s after the tests, want inactive", headerServerUnit, state)
	}
	t.Logf("%s: test directory, test units, and package %s are absent", guest.guest, testPackage)
}
