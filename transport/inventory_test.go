package transport_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const sriovResponse = `{
	"numvfs":1,"totalvfs":64,
	"desired":{"numvfs":"1","vfs":[{"index":"0","mac":"02:01:02:03:04:05","driver":"iavf"}]},
	"vfs":[{"index":"0","mac":"02:01:02:03:04:05","driver":"iavf","pci":"0000:03:00.1","in_container":false}]
}`

func TestSetSRIOVDecodesProxmoxInventory(t *testing.T) {
	assignment := transport.VFDeclaration{Index: 0, MAC: "02:01:02:03:04:05", Driver: "iavf"}
	declaration := transport.SRIOVDeclaration{NumVFs: 1, VFs: []transport.VFDeclaration{assignment}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || request.URL.Path != "/api2/json/nodes/"+testNode+"/sriov/eno2" {
			http.NotFound(writer, request)
			return
		}

		var sent transport.SRIOVDeclaration
		if err := json.NewDecoder(request.Body).Decode(&sent); err != nil ||
			sent.NumVFs != declaration.NumVFs || len(sent.VFs) != 1 || sent.VFs[0] != assignment {
			t.Errorf("the PUT request did not contain the numeric declaration: %+v, %v", sent, err)
			http.Error(writer, "invalid SR-IOV declaration", http.StatusBadRequest)
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		if _, err := writer.Write([]byte(`{"data":` + sriovResponse + `}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := transport.NewClient(
		map[string]transport.NodeConfig{testNode: {Endpoint: server.URL, APIToken: testAPIToken}},
		transport.DefaultMaxRequests,
	)
	if err != nil {
		t.Fatal(err)
	}

	state, err := client.SetSRIOV(t.Context(), testNode, "eno2", declaration)
	if err != nil {
		t.Fatal(err)
	}

	if state.NumVFs != 1 || state.TotalVFs != 64 || state.Desired == nil || state.Desired.NumVFs != 1 ||
		len(state.Desired.VFs) != 1 || state.Desired.VFs[0] != assignment || len(state.VFs) != 1 ||
		state.VFs[0].VFDeclaration != assignment || state.VFs[0].PCI != "0000:03:00.1" || state.VFs[0].InContainer {
		t.Fatalf("the PUT response did not decode to the observed inventory: %+v", state)
	}
}

func TestSRIOVResponseRejectsInvalidIntegers(t *testing.T) {
	for _, count := range []string{`null`, `"invalid"`, `"1.0"`, `1e0`, `"9223372036854775808"`} {
		t.Run(count, func(t *testing.T) {
			document := strings.Replace(sriovResponse, `"numvfs":"1"`, `"numvfs":`+count, 1)
			var state transport.SRIOVState
			if err := json.Unmarshal([]byte(document), &state); err == nil {
				t.Fatal("the SR-IOV response accepted an invalid VF count")
			}
		})
	}
}

func TestSRIOVResponseRequiresNamespaceObservation(t *testing.T) {
	const prefix = `{"numvfs":1,"totalvfs":2,"vfs":[{"index":0,"mac":"02:01:02:03:04:05","driver":"iavf","pci":"0000:03:00.1","ifname":"eth0"`
	var state transport.SRIOVState
	if err := json.Unmarshal([]byte(prefix+"}]}"), &state); err == nil {
		t.Fatal("an API without in_container must fail")
	}

	if err := json.Unmarshal([]byte(prefix+`,"in_container":1}]}`), &state); err != nil {
		t.Fatal(err)
	}

	if !state.VFs[0].InContainer || state.VFs[0].Ifname != "eth0" {
		t.Fatalf("lost the namespace observation: %+v", state.VFs[0])
	}
}

func TestUnassignedVFInventory(t *testing.T) {
	const inventory = `{"numvfs":1,"totalvfs":16,"desired":{"numvfs":1,"vfs":[]},"vfs":[{"index":0,"mac":"00:00:00:00:00:00","pci":"0000:03:00.1","in_container":false}]}`
	var state transport.SRIOVState
	if err := json.Unmarshal([]byte(inventory), &state); err != nil {
		t.Fatal(err)
	}

	if len(state.VFs) != 1 || state.VFs[0].MAC != "00:00:00:00:00:00" ||
		len(state.Desired.VFs) != 0 {
		t.Fatalf("incorrect undeclared VF inventory: %+v", state)
	}
}
