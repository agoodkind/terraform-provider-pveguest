package netconfig_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/internal/netconfig"
)

func bondSettings() netconfig.Settings {
	return netconfig.Settings{
		Name:      "eth0",
		MAC:       "02:00:00:00:00:01",
		DHCP4:     true,
		SLAAC:     true,
		DNS:       []string{},
		Search:    []string{},
		Addresses: []string{},
		Bond: &netconfig.Bond{
			Members: []netconfig.BondMember{
				{Name: "vf1", MAC: "02:00:00:00:00:02"},
				{Name: "vf0", MAC: "02:00:00:00:00:01"},
			},
			Primary:         "vf0",
			MIIMon:          100,
			FailOverMAC:     "active",
			PrimaryReselect: "failure",
		},
	}
}

func TestBondDeclaration(t *testing.T) {
	for name, change := range map[string]func(*netconfig.Bond){
		"one member":            func(b *netconfig.Bond) { b.Members = b.Members[:1] },
		"duplicate member name": func(b *netconfig.Bond) { b.Members[1].Name = b.Members[0].Name },
		"duplicate member MAC":  func(b *netconfig.Bond) { b.Members[1].MAC = b.Members[0].MAC },
		"bond name collision":   func(b *netconfig.Bond) { b.Members[1].Name = "eth0" },
		"multicast MAC":         func(b *netconfig.Bond) { b.Members[1].MAC = "01:00:00:00:00:01" },
		"unlisted primary":      func(b *netconfig.Bond) { b.Primary = "other" },
		"zero miimon":           func(b *netconfig.Bond) { b.MIIMon = 0 },
		"shared MAC":            func(b *netconfig.Bond) { b.FailOverMAC = "none" },
		"unknown policy":        func(b *netconfig.Bond) { b.PrimaryReselect = "default" },
	} {
		t.Run(
			name,
			func(t *testing.T) {
				s := bondSettings()
				change(s.Bond)
				if _, err := s.Plan(netconfig.Netplan, nil, false); err == nil {
					t.Fatal("invalid bond accepted")
				}
			},
		)
	}
}

func updateFiles(files map[string]string, edits map[string]netconfig.Edit) {
	for path, edit := range edits {
		if edit.After == nil {
			delete(files, path)

			continue
		}

		files[path] = *edit.After
	}
}

func TestBondMigration(t *testing.T) {
	for _, backend := range []netconfig.Backend{netconfig.Ifupdown, netconfig.Netplan} {
		t.Run(
			string(backend),
			func(t *testing.T) {
				s := bondSettings()
				source, initial := fixtureSource(backend, s.Name, s.MAC)
				unrelated := "auto other0\niface other0 inet dhcp\n"
				if backend == netconfig.Netplan {
					unrelated = "    other0: {dhcp4: true}\n"
				}

				files := map[string]string{source: initial + unrelated}
				if backend == netconfig.Ifupdown {
					files["/etc/network/interfaces"] = "source " + source + "\n"
				}

				apply := func(remove bool) map[string]netconfig.Edit {
					t.Helper()
					edits, err := s.Plan(backend, files, remove)
					if err != nil {
						t.Fatal(err)
					}

					updateFiles(files, edits)

					return edits
				}
				bond := s.Bond
				s.Bond = nil
				apply(false)
				s.Bond = bond
				apply(false)
				const clientConfig = "/etc/dhcp/pveguest-65746830.conf"
				if backend == netconfig.Ifupdown && files[clientConfig] == "" {
					t.Fatal("missing per-interface DHCP configuration under /etc/dhcp")
				}

				if !strings.Contains(files[source], "other0") {
					t.Fatal("unrelated interface removed")
				}

				if changed := apply(false); len(changed) > 0 {
					t.Fatalf("repeated staging: %v", changed)
				}

				s.Bond.Members[0].Name = "vf2"
				apply(false)

				for _, content := range files {
					if strings.Contains(content, "vf1") {
						t.Fatalf("obsolete member remains: %s", content)
					}
				}

				apply(true)
				if _, exists := files[clientConfig]; exists {
					t.Fatal("DHCP configuration remains after removal")
				}

				if changed := apply(true); len(changed) > 0 {
					t.Fatalf("repeated removal: %v", changed)
				}

				for filename := range files {
					if strings.Contains(filename, "pveguest-member") ||
						strings.Contains(filename, "pveguest-bonds") {
						t.Fatalf("stale %s", filename)
					}
				}
			},
		)
	}
}

func TestBondPreservesExistingRoute(t *testing.T) {
	s := bondSettings()
	source, initial := fixtureSource(netconfig.Netplan, s.Name, s.MAC)
	initial = strings.Replace(initial, "to: default", "to: 203.0.113.0/24", 1)
	changes, err := s.Plan(netconfig.Netplan, map[string]string{source: initial}, false)
	if err == nil || !strings.Contains(err.Error(), "migrate routes") || len(changes) != 0 {
		t.Fatalf("expected migration rejection before edits: %v %v", changes, err)
	}
}

func guestBondEdits(
	t *testing.T,
	s netconfig.Settings,
	backend netconfig.Backend,
	remove bool,
) map[string]netconfig.Edit {
	t.Helper()
	argv := []string{"-e", netconfig.FileScript, "--", string(backend), "read", s.Name, s.MAC}
	var files map[string]string
	if err := json.Unmarshal(command(t, nil, "perl", argv...), &files); err != nil {
		t.Fatal(err)
	}

	edits, err := s.Plan(backend, files, remove)
	if err != nil {
		t.Fatal(err)
	}

	if backend == netconfig.Netplan {
		argv[4] = "render"
		if remove {
			argv[4] = "unrender"
		}

		if s.Bond != nil {
			for _, member := range s.Bond.Members {
				argv = append(argv, member.Name, member.MAC)
			}
		}

		var overrides map[string]netconfig.Edit
		if err := json.Unmarshal(
			command(t, jsonBytes(t, edits), "perl", argv...),
			&overrides,
		); err != nil {
			t.Fatal(err)
		}

		for path, edit := range overrides {
			edits[path] = edit
		}
	}

	return edits
}

func stageBond(t *testing.T, s netconfig.Settings, backend netconfig.Backend, remove bool) {
	t.Helper()
	edits := guestBondEdits(t, s, backend, remove)
	command(
		t,
		jsonBytes(t, edits),
		"perl",
		"-e",
		netconfig.FileScript,
		"--",
		string(backend),
		"write",
		s.Name,
	)
	if changes := guestBondEdits(t, s, backend, remove); len(changes) > 0 {
		t.Fatalf("persistent drift: %v", changes)
	}
}

func TestLinuxBondProxyRemoval(t *testing.T) {
	if os.Getenv("PVEGUEST_ACC_DISPOSABLE_BOND") != "1" {
		t.Skip("requires a disposable Linux network namespace")
	}

	settings := bondSettings()
	const owned = "2001:db8::50"
	settings.ObsoleteProxyNeighbors = []string{owned}
	command(t, nil, "ip", "link", "add", settings.Name, "type", "bond", "mode", "active-backup")
	t.Cleanup(func() { command(t, nil, "ip", "link", "delete", settings.Name) })

	for _, address := range []string{owned, otherProxy} {
		command(t, nil, "ip", "-6", "neigh", "add", "proxy", address, "dev", settings.Name)
	}

	command(
		t,
		jsonBytes(t, settings),
		"perl",
		"-e",
		netconfig.RuntimeScript,
		"--",
		"remove-proxy-neighbors",
		"ifupdown",
	)
	remaining := command(t, nil, "ip", "-j", "-6", "neigh", "show", "proxy", "dev", settings.Name)
	if bytes.Contains(remaining, []byte(owned)) || !bytes.Contains(remaining, []byte(otherProxy)) {
		t.Fatalf("unexpected proxy neighbors on inactive bond: %s", remaining)
	}
}
