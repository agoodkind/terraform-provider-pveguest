package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	containerPowerAddress = "pveguest_container_power.test"
	stoppedStatus         = "stopped"
	runningStatus         = "running"
)

func containerPowerConfig(node string, nodeConfig transport.NodeConfig, vmid int64, running bool) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    pveguest = {
      source = %q
    }
  }
}

provider "pveguest" {
  nodes = {
    %q = {
      endpoint  = %q
      api_token = %q
      insecure  = %t
    }
  }
}

resource "pveguest_container_power" "test" {
  node    = %q
  vmid    = %d
  running = %t
}
`, providerSource, node, nodeConfig.Endpoint, nodeConfig.APIToken, nodeConfig.Insecure, node, vmid, running)
}

func TestAccContainerPower(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	node := requireVariable(t, nodeVariable)
	vmid, err := strconv.ParseInt(requireVariable(t, vmidVariable), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", vmidVariable, err)
	}
	nodeConfig := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(map[string]transport.NodeConfig{node: nodeConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}

	before, err := client.GetContainerStatus(context.Background(), node, vmid)
	if err != nil {
		t.Fatal(err)
	}
	if before != stoppedStatus {
		t.Fatalf("container %d has status %q; the test requires a stopped container", vmid, before)
	}

	statusOnNode := func(want string) resource.TestCheckFunc {
		return func(_ *terraform.State) error {
			got, err := client.GetContainerStatus(context.Background(), node, vmid)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("container %d has status %q on the node, want %q", vmid, got, want)
			}
			return nil
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: containerPowerConfig(node, nodeConfig, vmid, true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(containerPowerAddress, "status", runningStatus),
					statusOnNode(runningStatus),
				),
			},
			{
				Config: containerPowerConfig(node, nodeConfig, vmid, false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(containerPowerAddress, "status", stoppedStatus),
					statusOnNode(stoppedStatus),
				),
			},
			{
				Config:                               containerPowerConfig(node, nodeConfig, vmid, false),
				ResourceName:                         containerPowerAddress,
				ImportState:                          true,
				ImportStateId:                        node + "/" + strconv.FormatInt(vmid, 10),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "vmid",
			},
		},
	})
}
