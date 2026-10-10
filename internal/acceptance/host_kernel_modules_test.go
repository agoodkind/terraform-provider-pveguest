package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	kernelModuleVariable    = "PVEGUEST_ACC_KERNEL_MODULE"
	auditTokenFileVariable  = "PVEGUEST_ACC_AUDIT_TOKEN_FILE"
	forbiddenStatusFragment = "HTTP 403"

	kernelModulesAddress = "pveguest_host_kernel_modules.test"
)

func hostKernelModulesConfig(node string, nodeConfig transport.NodeConfig, module string) string {
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
`, providerSource, node, nodeConfig.Endpoint, nodeConfig.APIToken, nodeConfig.Insecure, node, module)
}

func TestAccHostKernelModules(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	module := os.Getenv(kernelModuleVariable)
	if module == "" {
		t.Skipf("%s is not set", kernelModuleVariable)
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
			return
		}
		restored, err := client.GetKernelModules(context.Background(), node)
		if err != nil {
			t.Errorf("read restored kernel modules: %v", err)
			return
		}
		if !slices.Equal(restored.Modules, original.Modules) {
			t.Errorf("restored modules %v differ from original %v", restored.Modules, original.Modules)
		}
	})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: hostKernelModulesConfig(node, nodeConfig, module),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(kernelModulesAddress, "modules.#", "1"),
					resource.TestCheckResourceAttr(kernelModulesAddress, "loaded."+module, "true"),
				),
			},
			{
				Config:            hostKernelModulesConfig(node, nodeConfig, module),
				ResourceName:      kernelModulesAddress,
				ImportState:       true,
				ImportStateId:     node,
				ImportStateVerify: true,

				ImportStateVerifyIdentifierAttribute: "node",
			},
		},
	})
}

func TestAccHostKernelModulesDenied(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	auditTokenFile := os.Getenv(auditTokenFileVariable)
	if auditTokenFile == "" {
		t.Skipf("%s is not set", auditTokenFileVariable)
	}
	node := requireVariable(t, nodeVariable)
	adminConfig := nodeConfigFromEnvironment(t)
	tokenBytes, err := os.ReadFile(auditTokenFile)
	if err != nil {
		t.Fatalf("%s: %v", auditTokenFileVariable, err)
	}
	auditConfig := adminConfig
	auditConfig.APIToken = strings.TrimSpace(string(tokenBytes))
	auditClient, err := transport.NewClient(map[string]transport.NodeConfig{node: auditConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}

	before, err := auditClient.GetKernelModules(context.Background(), node)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 501") {
			t.Skipf("node %s does not serve the kernel-modules endpoint: %v", node, err)
		}
		t.Fatalf("audit token read: %v", err)
	}
	_, err = auditClient.SetKernelModules(context.Background(), node, before.Modules)
	if err == nil {
		t.Fatal("audit token write succeeded")
	}
	if !strings.Contains(err.Error(), forbiddenStatusFragment) {
		t.Fatalf("audit token write error %q does not contain %q", err, forbiddenStatusFragment)
	}

	adminClient, err := transport.NewClient(map[string]transport.NodeConfig{node: adminConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}
	after, err := adminClient.GetKernelModules(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.Modules, before.Modules) {
		t.Fatalf("modules changed from %v to %v", before.Modules, after.Modules)
	}
}

const (
	unlistedModuleVariable = "PVEGUEST_ACC_UNLISTED_KERNEL_MODULE"
	notAllowedFragment     = "is not in /etc/pve-overlay/kernel-modules.allow"
	notAllowedPattern      = `is\s+not\s+in\s+/etc/pve-overlay/kernel-modules\.allow`
)

var httpFailureStatus = regexp.MustCompile(`HTTP [45][0-9][0-9]`)

func TestAccHostKernelModulesRejected(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	unlisted := os.Getenv(unlistedModuleVariable)
	if unlisted == "" {
		t.Skipf("%s is not set", unlistedModuleVariable)
	}
	node := requireVariable(t, nodeVariable)
	nodeConfig := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(map[string]transport.NodeConfig{node: nodeConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}

	before, err := client.GetKernelModules(context.Background(), node)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "HTTP 501") {
			t.Skipf("node %s does not serve the kernel-modules endpoint: %v", node, err)
		}
		t.Fatal(err)
	}

	requested := append(slices.Clone(before.Modules), unlisted)
	_, err = client.SetKernelModules(context.Background(), node, requested)
	if err == nil {
		t.Fatalf("write of unlisted module %q succeeded", unlisted)
	}
	if !httpFailureStatus.MatchString(err.Error()) {
		t.Fatalf("write error %q does not contain an HTTP 4xx or 5xx status", err)
	}
	if !strings.Contains(err.Error(), notAllowedFragment) {
		t.Fatalf("write error %q does not contain %q", err, notAllowedFragment)
	}

	after, err := client.GetKernelModules(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.Modules, before.Modules) {
		t.Fatalf("modules changed from %v to %v", before.Modules, after.Modules)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      hostKernelModulesConfig(node, nodeConfig, unlisted),
				ExpectError: regexp.MustCompile(notAllowedPattern),
			},
		},
	})

	final, err := client.GetKernelModules(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(final.Modules, before.Modules) {
		t.Fatalf("modules changed from %v to %v", before.Modules, final.Modules)
	}
}
