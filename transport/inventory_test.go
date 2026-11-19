package transport_test

import (
	"encoding/json"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

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
