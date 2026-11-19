package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

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

	if !original.Configured || original.Method != "manual" || original.Method6 != "manual" ||
		original.Settings.CIDR != "" ||
		original.Settings.CIDR6 != "" ||
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

	if current.Method != "static" || current.Method6 != "static" ||
		current.Settings.CIDR != "192.0.2.30/27" ||
		current.Settings.CIDR6 != "fd00:1::30/64" {
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

	if current.Method != "manual" || current.Method6 != "manual" || current.Settings.CIDR != "" ||
		current.Settings.CIDR6 != "" ||
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

func TestEthernetMethodPlan(t *testing.T) {
	server, err := protoV6ProviderFactories["pveguest"]()
	if err != nil {
		t.Fatal(err)
	}

	const resource = "pveguest_host_network_interface"
	schema, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}

	typ := schema.ResourceSchemas[resource].ValueType()
	values := map[string]any{
		"node":      "test",
		"name":      "eno2",
		"autostart": true,
		"cidr":      nil,
		"cidr6":     nil,
		"gateway":   nil,
		"gateway6":  nil,
		"comments":  nil,
		"mtu":       nil,
		"change_id": nil,
		"method":    nil,
		"method6":   nil,
	}
	config := func() *tfprotov6.DynamicValue {
		b, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}

		return &tfprotov6.DynamicValue{JSON: b}
	}
	validate := func(wantError bool) {
		t.Helper()
		resp, err := server.ValidateResourceConfig(
			t.Context(),
			&tfprotov6.ValidateResourceConfigRequest{TypeName: resource, Config: config()},
		)
		if err != nil {
			t.Fatal(err)
		}

		if (len(resp.Diagnostics) > 0) != wantError {
			t.Fatalf("validation diagnostics: %v", resp.Diagnostics)
		}
	}
	validate(false)

	for _, field := range []string{"cidr", "cidr6", "gateway", "gateway6", "comments"} {
		values[field] = ""
		validate(true)
		values[field] = nil
	}

	declaration := config()
	values["change_id"] = "before"
	values["method6"] = "manual"

	for _, method := range []string{"dhcp", "manual"} {
		values["method"] = method
		observed := config()
		plan, err := server.PlanResourceChange(
			t.Context(),
			&tfprotov6.PlanResourceChangeRequest{
				TypeName:         resource,
				PriorState:       observed,
				ProposedNewState: observed,
				Config:           declaration,
			},
		)
		if err != nil || len(plan.Diagnostics) != 0 {
			t.Fatalf("plan: %v %v", err, plan.Diagnostics)
		}

		data, err := plan.PlannedState.Unmarshal(typ)
		if err != nil {
			t.Fatal(err)
		}

		var fields map[string]tftypes.Value
		if err := data.As(&fields); err != nil {
			t.Fatal(err)
		}

		if !fields["method"].Equal(tftypes.NewValue(tftypes.String, "manual")) ||
			fields["change_id"].IsKnown() != (method == "manual") {
			t.Fatalf("plan for %s: %s", method, data.String())
		}
	}
}
