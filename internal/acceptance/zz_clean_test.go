package acceptance

import "testing"

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
	}
	for _, path := range leftoverPaths {
		if guest.exists(path) {
			t.Errorf("%s exists after the tests", path)
		}
	}
	if guest.packageInstalled(testPackage) {
		t.Errorf("package %s is installed after the tests", testPackage)
	}
	if state := guest.systemctlState("is-active", testTimer); state != "inactive" {
		t.Errorf("%s is %s after the tests, want inactive", testTimer, state)
	}
	t.Logf("%s: test directory, test units, and package %s are absent", guest.guest, testPackage)
}
