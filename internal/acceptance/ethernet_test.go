package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const ethernetAddress = "pveguest_host_network_interface.port"

// TestAccEthernetAdoption changes only staged configuration on a dedicated
// physical port. The environment must identify an existing manual stanza.
func TestAccEthernetAdoption(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" || os.Getenv("PVEGUEST_ACC_ETHERNET") == "" {
		t.Skip("Set TF_ACC and PVEGUEST_ACC_ETHERNET to a dedicated physical port")
	}

	port := requireVariable(t, "PVEGUEST_ACC_ETHERNET")
	node := requireVariable(t, nodeVariable)
	config := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(
		map[string]transport.NodeConfig{node: config},
		transport.DefaultMaxRequests,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	original, err := client.ReadEthernet(ctx, node, port)
	if err != nil {
		t.Fatal(err)
	}

	if !original.Configured || original.Settings.CIDR != "" || original.Settings.CIDR6 != "" ||
		original.Settings.Gateway != "" ||
		original.Settings.Gateway6 != "" {
		t.Fatal("The dedicated port must have an existing addressless Ethernet stanza.")
	}

	t.Cleanup(func() {
		if err := client.StageEthernet(
			context.Background(),
			node,
			port,
			original.Settings,
		); err != nil {
			t.Errorf("restore staged interface: %v", err)
		}
	})
	g := testGuest{t: t, guest: transport.Guest{Node: node}, node: config}
	workspace := tofuWorkspace{
		t:         t,
		directory: t.TempDir(),
		binary:    requireVariable(t, tofuPathVariable),
	}
	settings := fmt.Sprintf(
		`
resource "pveguest_host_network_interface" "port" {
 node = local.node
 name = %q
 autostart = true
 %%s
}
`,
		port,
	)
	workspace.writeConfig(
		g.providerBlock() + fmt.Sprintf(
			settings,
			"cidr = \"192.0.2.30/27\"\n cidr6 = \"fd00:1::30/64\"",
		),
	)
	workspace.mustRun("init", "-no-color")
	if output, code := workspace.run("apply", "-no-color", "-auto-approve"); code == 0 ||
		!regexp.MustCompile("Import existing Ethernet configuration").MatchString(output) {
		t.Fatal("Apply must require import before changing the dedicated port.")
	}

	workspace.mustRun("import", "-no-color", ethernetAddress, node+":"+port)
	workspace.mustRun("apply", "-no-color", "-auto-approve")
	current, err := client.ReadEthernet(ctx, node, port)
	if err != nil {
		t.Fatal(err)
	}

	if current.Settings.CIDR != "192.0.2.30/27" || current.Settings.CIDR6 != "fd00:1::30/64" {
		t.Fatalf("unexpected staged settings: %+v", current.Settings)
	}

	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
	current.Settings.Comments = "external change on dedicated port"
	if err := client.StageEthernet(ctx, node, port, current.Settings); err != nil {
		t.Fatal(err)
	}

	if _, code := workspace.run(
		"plan",
		"-detailed-exitcode",
		"-no-color",
	); code != planChangesExitCode {
		t.Fatalf("plan after drift: exit %d", code)
	}

	workspace.writeConfig(g.providerBlock() + fmt.Sprintf(settings, ""))
	workspace.mustRun("apply", "-auto-approve", "-no-color")
	current, err = client.ReadEthernet(ctx, node, port)
	if err != nil {
		t.Fatal(err)
	}

	if current.Settings.CIDR != "" || current.Settings.CIDR6 != "" ||
		current.Settings.Comments != "" {
		t.Fatalf("optional fields remain after update: %+v", current.Settings)
	}

	workspace.mustRun("plan", "-detailed-exitcode", "-no-color")
	workspace.mustRun("destroy", "-auto-approve", "-no-color")
}

func TestAccNetworkErrors(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" || os.Getenv("PVEGUEST_ACC_NETWORK_ERRORS") == "" {
		t.Skip("Set TF_ACC and PVEGUEST_ACC_NETWORK_ERRORS to test unavailable adapters")
	}

	node := requireVariable(t, nodeVariable)
	config := nodeConfigFromEnvironment(t)
	guest := testGuest{t: t, guest: transport.Guest{Node: node}, node: config}
	const unsupported = `
data "pveguest_host_sriov" "absent" {
 node = local.node
 ifname = "pveabsent01"
}
`
	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config:      guest.providerBlock() + unsupported,
					ExpectError: regexp.MustCompile("(?s)Read SR-IOV adapter.*pveabsent01"),
				},
			},
		},
	)
}
