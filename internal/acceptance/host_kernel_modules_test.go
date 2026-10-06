package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	testKernelModule = "dummy"

	kernelModulesAddress = "pveguest_host_kernel_modules.test"
)

func hostKernelModulesConfig(node string, nodeConfig transport.NodeConfig) string {
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

resource "pveguest_host_kernel_modules" "test" {
  node    = %q
  modules = [%q]
}
`, providerSource, node, nodeConfig.Endpoint, nodeConfig.APIToken, nodeConfig.Insecure, node, testKernelModule)
}

func TestAccHostKernelModules(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	node := requireVariable(t, nodeVariable)
	nodeConfig := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(map[string]transport.NodeConfig{node: nodeConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}

	original, err := client.GetKernelModules(context.Background(), node)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 501") {
			t.Skipf("node %s does not serve the kernel-modules endpoint: %v", node, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := client.SetKernelModules(context.Background(), node, original.Modules); err != nil {
			t.Errorf("restore kernel modules: %v", err)
		}
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: checkGuest(func() error {
			current, err := client.GetKernelModules(context.Background(), node)
			if err != nil {
				return err
			}
			if len(current.Modules) != 0 {
				return fmt.Errorf("modules %v remain after destroy", current.Modules)
			}
			return nil
		}),
		Steps: []resource.TestStep{
			{
				Config: hostKernelModulesConfig(node, nodeConfig),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(kernelModulesAddress, "modules.#", "1"),
					resource.TestCheckResourceAttr(kernelModulesAddress, "loaded."+testKernelModule, "true"),
				),
			},
			{
				Config:            hostKernelModulesConfig(node, nodeConfig),
				ResourceName:      kernelModulesAddress,
				ImportState:       true,
				ImportStateId:     node,
				ImportStateVerify: true,
			},
		},
	})
}
