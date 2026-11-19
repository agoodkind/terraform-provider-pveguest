package acceptance_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

// TestAccVFInventory provisions two functions on an unused, unmanaged
// adapter. It checks that a separate read-only lookup returns the actual PCI
// address for the explicitly assigned index.
func TestAccVFInventory(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" || os.Getenv("PVEGUEST_ACC_SRIOV_PF") == "" {
		t.Skip("Set TF_ACC and PVEGUEST_ACC_SRIOV_PF to an unused PF.")
	}

	ssh := requireVariable(t, hostSSHVariable)
	host := hostArtifactTest{t: t, ssh: ssh, sshControlPath: hostSSHControlPath(t, ssh)}
	node := requireVariable(t, nodeVariable)
	pf := requireVariable(t, "PVEGUEST_ACC_SRIOV_PF")
	config := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(
		map[string]transport.NodeConfig{node: config},
		transport.DefaultMaxRequests,
	)
	if err != nil {
		t.Fatal(err)
	}

	state, err := client.ReadSRIOV(t.Context(), node, pf)
	if err != nil {
		t.Fatal(err)
	}

	if state.NumVFs != 0 || state.Desired != nil || state.TotalVFs < 2 {
		t.Fatal(
			"This test requires an unmanaged adapter with zero enabled VFs and capacity for two.",
		)
	}

	fixture := testGuest{t: t, guest: transport.Guest{Node: node}, node: config}
	workspace := tofuWorkspace{
		t:         t,
		directory: t.TempDir(),
		binary:    requireVariable(t, tofuPathVariable),
	}
	workspace.writeConfig(
		fixture.providerBlock() + fmt.Sprintf(
			`
resource "pveguest_host_sriov" "port" {
 node = local.node
 ifname = %q
 numvfs = 2
 vfs = {"1" = {mac = "02:01:02:03:04:05", driver = "vfio-pci"}}
}
data "pveguest_host_sriov" "inventory" {
 node = local.node
 ifname = pveguest_host_sriov.port.ifname
 depends_on = [pveguest_host_sriov.port]
}
output "pci" { value = data.pveguest_host_sriov.inventory.actual_vfs["1"].pci }
`,
			pf,
		),
	)
	workspace.mustRun("init", "-no-color")
	workspace.mustRun("apply", "-auto-approve", "-no-color")
	t.Cleanup(func() { workspace.mustRun("destroy", "-auto-approve", "-no-color") })
	state, err = client.ReadSRIOV(t.Context(), node, pf)
	if err != nil {
		t.Fatal(err)
	}

	address := workspace.mustRun("output", "-raw", "pci")
	if len(state.VFs) != 2 || state.VFs[1].Index != 1 || state.VFs[1].PCI != address ||
		state.VFs[1].Driver != "vfio-pci" {
		t.Fatalf("unexpected PCI assignment: %+v", state.VFs)
	}

	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
	host.hostMustRun("ip", "link", "set", "dev", pf, "vf", "1", "mac", "02:01:02:03:04:06")
	if _, code := workspace.run(
		"plan",
		"-detailed-exitcode",
		"-no-color",
	); code != planChangesExitCode {
		t.Fatalf("plan after runtime VF MAC drift: exit %d", code)
	}

	workspace.mustRun("apply", "-auto-approve", "-no-color")
	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
	workspace.mustRun("state", "rm", "pveguest_host_sriov.port")
	workspace.mustRun("import", "pveguest_host_sriov.port", node+":"+pf)
	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
}
