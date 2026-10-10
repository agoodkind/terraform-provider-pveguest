package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	hostNicLinkVariable = "PVEGUEST_ACC_HOSTNIC_LINK"

	containerOptionsAddress = "pveguest_container_options.test"
	hostNicKey              = "hostnic0"
	hostNicName             = "pveguestacc"
)

func containerOptionsConfig(node string, nodeConfig transport.NodeConfig, vmid int64, value string) string {
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

resource "pveguest_container_options" "test" {
  node = %q
  vmid = %d
  options = {
    %s = %q
  }
}
`, providerSource, node, nodeConfig.Endpoint, nodeConfig.APIToken, nodeConfig.Insecure, node, vmid, hostNicKey, value)
}

func TestAccContainerOptions(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	link := os.Getenv(hostNicLinkVariable)
	if link == "" {
		t.Skipf("%s is not set", hostNicLinkVariable)
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

	before, err := client.GetContainerOptions(context.Background(), node, vmid)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := before[hostNicKey]; found {
		t.Fatalf("container %d already has %s; the test requires a container without it", vmid, hostNicKey)
	}

	value := fmt.Sprintf("link=%s,name=%s", link, hostNicName)
	config := containerOptionsConfig(node, nodeConfig, vmid, value)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			after, err := client.GetContainerOptions(context.Background(), node, vmid)
			if err != nil {
				return err
			}
			if _, found := after[hostNicKey]; found {
				return fmt.Errorf("container %d still has %s after destroy", vmid, hostNicKey)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(containerOptionsAddress, "options."+hostNicKey, value),
					func(_ *terraform.State) error {
						current, err := client.GetContainerOptions(context.Background(), node, vmid)
						if err != nil {
							return err
						}
						if current[hostNicKey] != value {
							return fmt.Errorf("%s = %q on the node, want %q", hostNicKey, current[hostNicKey], value)
						}
						return nil
					},
				),
			},
			{
				Config:                               config,
				ResourceName:                         containerOptionsAddress,
				ImportState:                          true,
				ImportStateId:                        node + "/" + strconv.FormatInt(vmid, 10),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "vmid",
			},
		},
	})
}
