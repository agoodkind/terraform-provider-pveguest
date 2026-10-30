package acceptance_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	vmOptionsAddress = "pveguest_vm_options.test"
	vsockKey         = "vsock"
	vsockEnabled     = "1"
	vsockDisabled    = "0"

	// args is a QEMU config key that the vsock pattern rejects.
	unsupportedVMOptionKey = "args"

	// The comparison omits digest because Proxmox recomputes it on every write.
	configDigestKey = "digest"

	vmOptionsPendingCount = "pending.%"
)

var (
	everyConfigKey    = regexp.MustCompile(`.*`)
	vmOptionsKeyError = regexp.MustCompile(`must be vsock`)
)

func vmOptionsConfig(node string, nodeConfig transport.NodeConfig, vmid int64, key string, value string) string {
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

resource "pveguest_vm_options" "test" {
  node = %q
  vmid = %d
  options = {
    %s = %q
  }
}
`, providerSource, node, nodeConfig.Endpoint, nodeConfig.APIToken, nodeConfig.Insecure, node, vmid, key, value)
}

// vmConfigWithoutDigest returns every VM config key except digest.
func vmConfigWithoutDigest(client *transport.Client, guest transport.Guest) (map[string]string, error) {
	config, err := client.GetGuestOptions(context.Background(), guest, everyConfigKey)
	if err != nil {
		return nil, err
	}
	delete(config, configDigestKey)
	return config, nil
}

func TestAccVMOptions(t *testing.T) {
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	node := requireVariable(t, nodeVariable)
	vmid, err := strconv.ParseInt(requireVariable(t, vmVMIDVariable), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", vmVMIDVariable, err)
	}
	nodeConfig := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(map[string]transport.NodeConfig{node: nodeConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}
	guest := transport.Guest{Node: node, VMID: vmid, Kind: transport.KindQEMU}

	before, err := vmConfigWithoutDigest(client, guest)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := before[vsockKey]; found {
		t.Fatalf("vm %d already has %s; the test requires a VM without it", vmid, vsockKey)
	}

	config := vmOptionsConfig(node, nodeConfig, vmid, vsockKey, vsockEnabled)
	expectNodeValue := func(_ *terraform.State) error {
		current, err := client.GetGuestOptions(context.Background(), guest, everyConfigKey)
		if err != nil {
			return err
		}
		if current[vsockKey] != vsockEnabled {
			return fmt.Errorf("%s = %q on the node, want %q", vsockKey, current[vsockKey], vsockEnabled)
		}
		return nil
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			after, err := vmConfigWithoutDigest(client, guest)
			if err != nil {
				return err
			}
			if !maps.Equal(after, before) {
				return fmt.Errorf("VM %d config after destroy differs from before the test: after: %v; before: %v", vmid, after, before)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config:      vmOptionsConfig(node, nodeConfig, vmid, unsupportedVMOptionKey, vsockEnabled),
				PlanOnly:    true,
				ExpectError: vmOptionsKeyError,
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vmOptionsAddress, "options."+vsockKey, vsockEnabled),
					resource.TestCheckResourceAttr(vmOptionsAddress, vmOptionsPendingCount, "0"),
					expectNodeValue,
				),
			},
			{
				PreConfig: func() {
					changed := map[string]string{vsockKey: vsockDisabled}
					if err := client.SetGuestOptions(context.Background(), guest, changed, nil); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config,
				ConfigPlanChecks: expectAction(vmOptionsAddress, plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vmOptionsAddress, "options."+vsockKey, vsockEnabled),
					expectNodeValue,
				),
			},
			{
				Config:                               config,
				ResourceName:                         vmOptionsAddress,
				ImportState:                          true,
				ImportStateId:                        node + "/" + strconv.FormatInt(vmid, 10),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "vmid",
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}
