package acceptance_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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
	vmOptionsPendingVsock = "pending.vsock"

	permissionsPath  = "/api2/json/access/permissions"
	apiTimeout       = 30 * time.Second
	apiResponseLimit = 1 << 20
	vmACLPathPrefix  = "/vms/"
	vmAPIPathPrefix  = "/api2/json/nodes/"
	vmAPIPathKind    = "/qemu/"
	tokenAuthScheme  = "PVEAPIToken="
	authHeader       = "Authorization"
	contentTypeJSON  = "application/json"

	vmConfigOperation  = "config"
	vmPendingOperation = "pending"
	vmStartOperation   = "status/start"
	vmStopOperation    = "status/stop"
)

// TestAccVMOptions requires these token privileges on the test VM.
var requiredVMPrivileges = []string{"VM.Audit", "VM.Config.Vsock", "VM.PowerMgmt"}

var (
	everyConfigKey    = regexp.MustCompile(`.*`)
	vsockConfigKey    = regexp.MustCompile(transport.VMOptionKeyPattern)
	vmOptionsKeyError = regexp.MustCompile(`must be vsock`)
)

type permissionsResponse struct {
	Data map[string]map[string]int `json:"data"`
}

type taskResponse struct {
	Data string `json:"data"`
}

type vmPendingResponse struct {
	Data []vmPendingEntry `json:"data"`
}

type vmPendingEntry struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Pending json.RawMessage `json:"pending"`
	Delete  json.RawMessage `json:"delete"`
}

type revertAndDeleteBody struct {
	Delete string `json:"delete"`
	Revert string `json:"revert"`
}

// vmOptionsTest stores the node, VM ID, settings, and transport client and
// guest for one TestAccVMOptions run.
type vmOptionsTest struct {
	node       string
	vmid       int64
	nodeConfig transport.NodeConfig
	client     *transport.Client
	guest      transport.Guest
}

func (v *vmOptionsTest) config(key string, value string) string {
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
`, providerSource, v.node, v.nodeConfig.Endpoint, v.nodeConfig.APIToken, v.nodeConfig.Insecure, v.node, v.vmid, key, value)
}

// vmConfigWithoutDigest returns every VM config key except digest.
func (v *vmOptionsTest) vmConfigWithoutDigest() (map[string]string, error) {
	config, err := v.client.GetGuestOptions(context.Background(), v.guest, everyConfigKey)
	if err != nil {
		return nil, err
	}
	delete(config, configDigestKey)
	return config, nil
}

func (v *vmOptionsTest) vmPath(operation string) string {
	return vmAPIPathPrefix + url.PathEscape(v.node) + vmAPIPathKind + strconv.FormatInt(v.vmid, 10) + "/" + operation
}

// proxmoxAPI sends a request with the test token and returns the HTTP status
// and response body.
func (v *vmOptionsTest) proxmoxAPI(t *testing.T, method string, apiPath string, query url.Values, body []byte) (int, []byte) {
	t.Helper()
	address := strings.TrimRight(v.nodeConfig.Endpoint, "/") + apiPath
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	tlsConfig.InsecureSkipVerify = v.nodeConfig.Insecure
	httpClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
		Timeout:   apiTimeout,
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, address, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(authHeader, tokenAuthScheme+v.nodeConfig.APIToken)
	if body != nil {
		request.Header.Set("Content-Type", contentTypeJSON)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s request failed: %v", method, apiPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(io.LimitReader(response.Body, apiResponseLimit))
	if err != nil {
		t.Fatalf("Cannot read the %s %s response: %v", method, apiPath, err)
	}
	return response.StatusCode, content
}

// tokenPrivileges returns sorted token privilege names for an ACL path.
func (v *vmOptionsTest) tokenPrivileges(t *testing.T) []string {
	t.Helper()
	aclPath := vmACLPathPrefix + strconv.FormatInt(v.vmid, 10)
	status, content := v.proxmoxAPI(t, http.MethodGet, permissionsPath, url.Values{"path": {aclPath}}, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s returned HTTP %d", permissionsPath, status)
	}
	var permissions permissionsResponse
	if err := json.Unmarshal(content, &permissions); err != nil {
		t.Fatalf("Cannot decode the GET %s response: %v", permissionsPath, err)
	}
	privileges := slices.Collect(maps.Keys(permissions.Data[aclPath]))
	slices.Sort(privileges)
	return privileges
}

// runVMTask starts a VM status task and uses WaitTask to wait for exit status OK.
func (v *vmOptionsTest) runVMTask(t *testing.T, operation string) {
	t.Helper()
	apiPath := v.vmPath(operation)
	status, content := v.proxmoxAPI(t, http.MethodPost, apiPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("POST %s returned HTTP %d: %s", apiPath, status, content)
	}
	var task taskResponse
	if err := json.Unmarshal(content, &task); err != nil {
		t.Fatalf("Cannot decode the POST %s response: %v", apiPath, err)
	}
	if err := v.client.WaitTask(context.Background(), v.node, task.Data); err != nil {
		t.Fatalf("Task from POST %s failed: %v", apiPath, err)
	}
}

// vsockPendingEntry returns the vsock entry from GET pending and whether it exists.
func (v *vmOptionsTest) vsockPendingEntry(t *testing.T) (vmPendingEntry, bool) {
	t.Helper()
	apiPath := v.vmPath(vmPendingOperation)
	status, content := v.proxmoxAPI(t, http.MethodGet, apiPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s returned HTTP %d", apiPath, status)
	}
	var pending vmPendingResponse
	if err := json.Unmarshal(content, &pending); err != nil {
		t.Fatalf("Cannot decode the GET %s response: %v", apiPath, err)
	}
	for _, entry := range pending.Data {
		if entry.Key == vsockKey {
			return entry, true
		}
	}
	return vmPendingEntry{}, false
}

// jsonText returns the text of a JSON string or the source text of another JSON
// value.
func jsonText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}

// probeRevertAndDelete sends revert and delete for vsock in one PUT request,
// logs the response, and returns an error if Proxmox accepts the request.
func (v *vmOptionsTest) probeRevertAndDelete(t *testing.T) error {
	t.Helper()
	apiPath := v.vmPath(vmConfigOperation)
	body, err := json.Marshal(revertAndDeleteBody{Delete: vsockKey, Revert: vsockKey})
	if err != nil {
		return err
	}
	status, content := v.proxmoxAPI(t, http.MethodPut, apiPath, nil, body)
	t.Logf("PUT %s with revert and delete for %s returned HTTP %d: %s", apiPath, vsockKey, status, content)
	if status == http.StatusOK {
		return fmt.Errorf("PUT %s accepted revert and delete for %s in one request", apiPath, vsockKey)
	}
	return nil
}

// expectNodeValue checks the vsock value in the config view that merges pending
// changes.
func (v *vmOptionsTest) expectNodeValue(want string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		current, err := v.vmConfigWithoutDigest()
		if err != nil {
			return err
		}
		if current[vsockKey] != want {
			return fmt.Errorf("%s = %q on the node, want %q", vsockKey, current[vsockKey], want)
		}
		return nil
	}
}

// restoreVMOptions restores the earlier vsock state and checks the VM config
// against the config from before the test.
func (v *vmOptionsTest) restoreVMOptions(t *testing.T, before map[string]string) {
	t.Helper()
	current, err := v.vmConfigWithoutDigest()
	if err != nil {
		t.Errorf("Cannot read VM %d config for restore: %v", v.vmid, err)
		return
	}
	pending, err := v.client.GetGuestPending(context.Background(), v.guest, vsockConfigKey)
	if err != nil {
		t.Errorf("Cannot read VM %d pending list for restore: %v", v.vmid, err)
		return
	}
	prior, hadVsock := before[vsockKey]
	_, hasVsock := current[vsockKey]
	_, vsockPending := pending[vsockKey]
	switch {
	case hadVsock && (current[vsockKey] != prior || vsockPending):
		err = v.client.SetGuestOptions(context.Background(), v.guest, map[string]string{vsockKey: prior}, nil)
	case !hadVsock && (hasVsock || vsockPending):
		err = v.client.SetGuestOptions(context.Background(), v.guest, nil, []string{vsockKey})
	}
	if err != nil {
		t.Errorf("Cannot restore %s on VM %d: %v", vsockKey, v.vmid, err)
		return
	}
	restored, err := v.vmConfigWithoutDigest()
	if err != nil {
		t.Errorf("Cannot read VM %d config after restore: %v", v.vmid, err)
		return
	}
	if !maps.Equal(restored, before) {
		t.Errorf("VM %d config after restore differs from the config before the test: after restore: %v; before test: %v", v.vmid, restored, before)
	}
	pending, err = v.client.GetGuestPending(context.Background(), v.guest, vsockConfigKey)
	if err != nil {
		t.Errorf("Cannot read VM %d pending list after restore: %v", v.vmid, err)
		return
	}
	if value, found := pending[vsockKey]; found {
		t.Errorf("VM %d pending list still has %s after restore: %q", v.vmid, vsockKey, value)
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
	v := &vmOptionsTest{
		node:       node,
		vmid:       vmid,
		nodeConfig: nodeConfig,
		client:     client,
		guest:      transport.Guest{Node: node, VMID: vmid, Kind: transport.KindQEMU},
	}

	before, err := v.vmConfigWithoutDigest()
	if err != nil {
		t.Fatal(err)
	}
	prior, hadVsock := before[vsockKey]
	privileges := v.tokenPrivileges(t)
	t.Logf("Test VM identity: node %s, VM ID %d, name %q", node, vmid, before[vmNameKey])
	t.Logf("Before the test, VM config key %s was present: %t, value: %q", vsockKey, hadVsock, prior)
	t.Logf("Token privileges on %s%d: %s", vmACLPathPrefix, vmid, strings.Join(privileges, ","))
	for _, required := range requiredVMPrivileges {
		if !slices.Contains(privileges, required) {
			t.Fatalf("The token does not have %s on %s%d", required, vmACLPathPrefix, vmid)
		}
	}
	t.Cleanup(func() { v.restoreVMOptions(t, before) })

	// Destroy deletes vsock and leaves the other VM config values unchanged.
	afterDestroy := maps.Clone(before)
	delete(afterDestroy, vsockKey)

	t.Run("stopped VM", func(t *testing.T) {
		testVMOptionsStopped(t, v, afterDestroy)
	})
	t.Run("running VM with applied and pending vsock", func(t *testing.T) {
		testVMOptionsAppliedAndPending(t, v)
	})
}

func testVMOptionsStopped(t *testing.T, v *vmOptionsTest, afterDestroy map[string]string) {
	config := v.config(vsockKey, vsockEnabled)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			after, err := v.vmConfigWithoutDigest()
			if err != nil {
				return err
			}
			if !maps.Equal(after, afterDestroy) {
				return fmt.Errorf("VM %d config after destroy: %v; expected config: %v", v.vmid, after, afterDestroy)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config:      v.config(unsupportedVMOptionKey, vsockEnabled),
				PlanOnly:    true,
				ExpectError: vmOptionsKeyError,
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vmOptionsAddress, "options."+vsockKey, vsockEnabled),
					resource.TestCheckResourceAttr(vmOptionsAddress, vmOptionsPendingCount, "0"),
					v.expectNodeValue(vsockEnabled),
				),
			},
			{
				PreConfig: func() {
					changed := map[string]string{vsockKey: vsockDisabled}
					if err := v.client.SetGuestOptions(context.Background(), v.guest, changed, nil); err != nil {
						t.Fatal(err)
					}
				},
				Config:           config,
				ConfigPlanChecks: expectAction(vmOptionsAddress, plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vmOptionsAddress, "options."+vsockKey, vsockEnabled),
					v.expectNodeValue(vsockEnabled),
				),
			},
			{
				Config:                               config,
				ResourceName:                         vmOptionsAddress,
				ImportState:                          true,
				ImportStateId:                        v.node + "/" + strconv.FormatInt(v.vmid, 10),
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

// testVMOptionsAppliedAndPending checks vsock with an applied value and a
// pending value on a running VM.
func testVMOptionsAppliedAndPending(t *testing.T, v *vmOptionsTest) {
	expectAppliedValue := func(_ *terraform.State) error {
		entry, found := v.vsockPendingEntry(t)
		if !found || jsonText(entry.Value) != vsockEnabled || jsonText(entry.Pending) != vsockDisabled {
			return fmt.Errorf("VM %d pending entry for %s: %+v, found: %t; want applied value %q and pending value %q",
				v.vmid, vsockKey, entry, found, vsockEnabled, vsockDisabled)
		}
		return nil
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			after, err := v.vmConfigWithoutDigest()
			if err != nil {
				return err
			}
			if _, found := after[vsockKey]; found {
				return fmt.Errorf("VM %d config still has %s after destroy", v.vmid, vsockKey)
			}
			// Proxmox applies deletion of the applied value at the next VM start.
			// Until then, the pending list contains only a pending deletion for vsock.
			entry, found := v.vsockPendingEntry(t)
			if !found || len(entry.Pending) > 0 || len(entry.Delete) == 0 {
				return fmt.Errorf("VM %d pending entry for %s after destroy: %+v, found: %t; want only a pending deletion",
					v.vmid, vsockKey, entry, found)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: v.config(vsockKey, vsockEnabled),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vmOptionsAddress, vmOptionsPendingCount, "0"),
					v.expectNodeValue(vsockEnabled),
				),
			},
			{
				PreConfig: func() {
					v.runVMTask(t, vmStartOperation)
					t.Cleanup(func() { v.runVMTask(t, vmStopOperation) })
				},
				Config: v.config(vsockKey, vsockDisabled),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(vmOptionsAddress, "options."+vsockKey, vsockDisabled),
					resource.TestCheckResourceAttr(vmOptionsAddress, vmOptionsPendingVsock, vsockDisabled),
					expectAppliedValue,
					func(_ *terraform.State) error { return v.probeRevertAndDelete(t) },
				),
			},
		},
	})
}
