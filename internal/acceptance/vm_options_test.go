package acceptance_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	vmNameKey       = "name"

	vmOptionsPendingCount = "pending.%"

	permissionsPath    = "/api2/json/access/permissions"
	permissionsTimeout = 30 * time.Second
	vmACLPathPrefix    = "/vms/"
	tokenAuthScheme    = "PVEAPIToken="
	authHeader         = "Authorization"
)

// TestAccVMOptions requires these token privileges on the test VM.
var requiredVMPrivileges = []string{"VM.Audit", "VM.Config.Vsock"}

var (
	everyConfigKey    = regexp.MustCompile(`.*`)
	vmOptionsKeyError = regexp.MustCompile(`must be vsock`)
)

type permissionsResponse struct {
	Data map[string]map[string]int `json:"data"`
}

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

// tokenPrivileges returns sorted token privilege names for an ACL path.
func tokenPrivileges(t *testing.T, nodeConfig transport.NodeConfig, vmid int64) []string {
	t.Helper()
	aclPath := vmACLPathPrefix + strconv.FormatInt(vmid, 10)
	query := url.Values{"path": {aclPath}}
	address := strings.TrimRight(nodeConfig.Endpoint, "/") + permissionsPath + "?" + query.Encode()

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	tlsConfig.InsecureSkipVerify = nodeConfig.Insecure
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
		Timeout:   permissionsTimeout,
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(authHeader, tokenAuthScheme+nodeConfig.APIToken)
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", permissionsPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned HTTP %d", permissionsPath, response.StatusCode)
	}
	var permissions permissionsResponse
	if err := json.NewDecoder(response.Body).Decode(&permissions); err != nil {
		t.Fatalf("Cannot decode the GET %s response: %v", permissionsPath, err)
	}
	privileges := slices.Collect(maps.Keys(permissions.Data[aclPath]))
	slices.Sort(privileges)
	return privileges
}

// restoreVMOptions restores the earlier vsock state and checks the VM config
// against the config from before the test.
func restoreVMOptions(t *testing.T, client *transport.Client, guest transport.Guest, before map[string]string) {
	t.Helper()
	current, err := vmConfigWithoutDigest(client, guest)
	if err != nil {
		t.Errorf("Cannot read VM %d config for restore: %v", guest.VMID, err)
		return
	}
	prior, hadVsock := before[vsockKey]
	_, hasVsock := current[vsockKey]
	switch {
	case hadVsock && current[vsockKey] != prior:
		err = client.SetGuestOptions(context.Background(), guest, map[string]string{vsockKey: prior}, nil)
	case !hadVsock && hasVsock:
		err = client.SetGuestOptions(context.Background(), guest, nil, []string{vsockKey})
	}
	if err != nil {
		t.Errorf("Cannot restore %s on VM %d: %v", vsockKey, guest.VMID, err)
		return
	}
	restored, err := vmConfigWithoutDigest(client, guest)
	if err != nil {
		t.Errorf("Cannot read VM %d config after restore: %v", guest.VMID, err)
		return
	}
	if !maps.Equal(restored, before) {
		t.Errorf("VM %d config after restore differs from the config before the test: after restore: %v; before test: %v", guest.VMID, restored, before)
	}
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
	prior, hadVsock := before[vsockKey]
	privileges := tokenPrivileges(t, nodeConfig, vmid)
	t.Logf("Test VM identity: node %s, VM ID %d, name %q", node, vmid, before[vmNameKey])
	t.Logf("Before the test, VM config key %s was present: %t, value: %q", vsockKey, hadVsock, prior)
	t.Logf("Token privileges on %s%d: %s", vmACLPathPrefix, vmid, strings.Join(privileges, ","))
	for _, required := range requiredVMPrivileges {
		if !slices.Contains(privileges, required) {
			t.Fatalf("The token does not have %s on %s%d", required, vmACLPathPrefix, vmid)
		}
	}
	t.Cleanup(func() { restoreVMOptions(t, client, guest, before) })

	// Destroy deletes vsock and leaves the other VM config values unchanged.
	afterDestroy := maps.Clone(before)
	delete(afterDestroy, vsockKey)

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
			if !maps.Equal(after, afterDestroy) {
				return fmt.Errorf("VM %d config after destroy: %v; expected config: %v", vmid, after, afterDestroy)
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
