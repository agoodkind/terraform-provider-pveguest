package acceptance_test

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const (
	// The at package ships an enabled atd.service and does not affect other services.
	// The test purges at after the checks.
	noStartPackage = "at"
	noStartUnit    = "atd.service"
	policyFilePath = "/usr/sbin/policy-rc.d"
)

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
					checkGuest(func() error {
						if !guest.packageInstalled(noStartPackage) {
							return fmt.Errorf("package %s is not installed", noStartPackage)
						}
						// A default package install starts an enabled service.
						// An enabled but inactive atd.service shows that the install did not start it.
						if state := guest.systemctlState("is-enabled", noStartUnit); state != "enabled" {
							return fmt.Errorf("%s is %s after the apply, want enabled", noStartUnit, state)
						}
						if state := guest.systemctlState("is-active", noStartUnit); state != "inactive" {
							return fmt.Errorf("%s is %s after the apply, want inactive", noStartUnit, state)
						}
						if guest.exists(policyFilePath) {
							return fmt.Errorf("%s exists after the apply", policyFilePath)
						}
						return nil
					}),
				),
			},
		},
	})
}
