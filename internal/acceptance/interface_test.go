package acceptance_test

import (
	"fmt"
	"os"
	"testing"
)

// TestAccGuestNetwork adopts a dedicated interface through the guest command
// API. The selected port must be available for static address assignment.
func TestAccGuestNetwork(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" || os.Getenv("PVEGUEST_ACC_GUEST_IFNAME") == "" {
		t.Skip("Set TF_ACC and PVEGUEST_ACC_GUEST_IFNAME for a dedicated guest interface.")
	}

	guest := newTestGuest(t)
	name := requireVariable(t, "PVEGUEST_ACC_GUEST_IFNAME")
	mac := requireVariable(t, "PVEGUEST_ACC_GUEST_MAC")
	backend := requireVariable(t, "PVEGUEST_ACC_GUEST_BACKEND")
	workspace := tofuWorkspace{
		t:         t,
		directory: t.TempDir(),
		binary:    requireVariable(t, tofuPathVariable),
	}
	config := guest.providerBlock() + fmt.Sprintf(
		`
resource "pveguest_network_interface" "port" {
 node = local.node
 vmid = local.vmid
 kind = local.kind
 name = %q
 mac = %q
 backend = %q
 phase = %%q
 dhcp4 = false
 slaac = false
 addresses = ["192.0.2.10/24"]
 mtu = 1470
}
`,
		name,
		mac,
		backend,
	)
	workspace.writeConfig(fmt.Sprintf(config, "staged"))
	workspace.mustRun("init", "-no-color")
	workspace.mustRun("apply", "-auto-approve", "-no-color")
	t.Cleanup(func() { workspace.mustRun("destroy", "-auto-approve", "-no-color") })
	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
	workspace.writeConfig(fmt.Sprintf(config, "active"))
	workspace.mustRun("apply", "-auto-approve", "-no-color")
	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
	guest.mustRun("ip", "addr", "del", "192.0.2.10/24", "dev", name)
	if _, code := workspace.run(
		"plan",
		"-detailed-exitcode",
		"-no-color",
	); code != planChangesExitCode {
		t.Fatalf("plan after runtime drift: exit %d", code)
	}

	workspace.mustRun("apply", "-auto-approve", "-no-color")
	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
}
