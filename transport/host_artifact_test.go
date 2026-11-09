package transport_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

func TestDecodeHostArtifactState(t *testing.T) {
	data := []byte(
		`{
		"workspace":"current/workspace","node":"current-node","environment":"current",
		"state":"interrupted","observed_sha256":"observed",
		"installed":{"workspace":"saved/workspace","node":"saved-node","environment":"saved","sha256":"archive"},
		"files":[{"path":"/file","present":1,"size":"12","uid":0,"gid":"1","matches":false,"write_id":"task"}],
		"directories":[{"path":"/directory","present":true,"uid":"2","gid":3,"matches":1}],
		"secrets":[{"field":"TOKEN","configured":1,"version":"4","restart_pending":false}]
	}`,
	)
	state, err := transport.DecodeHostArtifactState(data)
	if err != nil {
		t.Fatal(err)
	}

	want := transport.HostArtifactState{
		Workspace:      "current/workspace",
		Node:           "current-node",
		Environment:    "current",
		State:          transport.HostArtifactStateInterrupted,
		ObservedSHA256: "observed",
		Installed: &transport.HostArtifactInstalled{
			Workspace:   "saved/workspace",
			Node:        "saved-node",
			Environment: "saved",
			SHA256:      "archive",
		},
		Files: []transport.HostArtifactFile{
			{
				Path:       "/file",
				Present:    true,
				Size:       12,
				GID:        1,
				HasMatches: true,
				WriteID:    "task",
			},
		},
		Directories: []transport.HostArtifactDirectory{
			{
				Path:       "/directory",
				Present:    true,
				UID:        2,
				GID:        3,
				HasMatches: true,
				Matches:    true,
			},
		},
		Secrets: []transport.HostSecretState{{Field: "TOKEN", Configured: true, Version: 4}},
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("decoded state = %+v, want %+v", state, want)
	}
}

func TestDecodeHostArtifactStateAcceptsOlderInstalledRecord(t *testing.T) {
	state, err := transport.DecodeHostArtifactState(
		[]byte(`{"state":"committed","installed":{"sha256":"archive"}}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	want := &transport.HostArtifactInstalled{SHA256: "archive"}
	if !reflect.DeepEqual(state.Installed, want) {
		t.Fatalf("older installed record = %+v, want %+v", state.Installed, want)
	}
}

func TestDecodeHostArtifactStateRejectsMalformedEntries(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		prefix string
	}{
		{name: "JSON", data: `{`, prefix: "parse host artifact response:"},
		{name: "file", data: `{"files":[{"path":"/file","size":1.5}]}`, prefix: "file /file:"},
		{
			name:   "directory",
			data:   `{"directories":[{"path":"/directory","uid":1.5}]}`,
			prefix: "directory /directory:",
		},
		{name: "secret", data: `{"secrets":[{"field":"TOKEN"}]}`, prefix: "secret field TOKEN:"},
	}
	for _, test := range tests {
		t.Run(
			test.name,
			func(t *testing.T) {
				state, err := transport.DecodeHostArtifactState([]byte(test.data))
				if err == nil || !strings.HasPrefix(err.Error(), test.prefix) {
					t.Fatalf("decode error = %v, want prefix %q", err, test.prefix)
				}

				if !reflect.DeepEqual(state, transport.HostArtifactState{}) {
					t.Fatalf("malformed response returned partial state: %+v", state)
				}
			},
		)
	}
}
