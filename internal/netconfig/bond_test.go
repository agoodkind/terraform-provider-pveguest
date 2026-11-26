package netconfig_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

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

// TestLinuxBond uses a disposable Linux namespace with two Ethernet peers.
func TestLinuxBond(t *testing.T) {
	if os.Getenv("PVEGUEST_ACC_DISPOSABLE_BOND") != "1" {
		t.Skip("requires a disposable Linux network namespace")
	}

	s := bondSettings()

	for _, backend := range []netconfig.Backend{netconfig.Ifupdown, netconfig.Netplan} {
		t.Run(
			string(backend),
			func(t *testing.T) {
				single := s
				single.Bond = nil
				stageBond(t, single, backend, false)
				stageBond(t, s, backend, false)
				replaced := s
				bond := *s.Bond
				bond.Members = []netconfig.BondMember{
					{Name: "vf2", MAC: "02:00:00:00:00:03"},
					s.Bond.Members[1],
				}
				replaced.Bond = &bond
				stageBond(t, replaced, backend, false)
				stageBond(t, s, backend, false)
				exerciseBondFailover(t, s, backend)
				stageBond(t, s, backend, true)
			},
		)
	}
}

func exerciseBondFailover(t *testing.T, s netconfig.Settings, backend netconfig.Backend) {
	t.Helper()
	const ns = "pveguest-router"
	command(t, nil, "ip", "netns", "add", ns)
	t.Cleanup(func() { command(t, nil, "ip", "netns", "del", ns) })
	command(t, nil, "ip", "-n", ns, "link", "add", "accrouter", "type", "bridge")
	command(t, nil, "ip", "-n", ns, "link", "set", "accrouter", "up")
	command(t, nil, "ip", "-n", ns, "link", "set", "lo", "up")
	command(t, nil, "ip", "-n", ns, "addr", "add", "198.51.100.1/24", "dev", "accrouter")
	command(t, nil, "ip", "-n", ns, "addr", "add", "fd99:1234::1/64", "dev", "accrouter", "nodad")

	for _, member := range s.Bond.Members {
		peer := "peer" + member.Name
		command(
			t,
			nil,
			"ip",
			"link",
			"add",
			member.Name,
			"type",
			"veth",
			"peer",
			"name",
			peer,
			"netns",
			ns,
		)
		command(t, nil, "ip", "link", "set", member.Name, "address", member.MAC)
		command(t, nil, "ip", "-n", ns, "link", "set", peer, "master", "accrouter", "up")
	}

	leaseFile := startRouter(t)
	if backend == netconfig.Netplan {
		command(t, nil, "systemctl", "enable", "systemd-networkd")
		command(t, nil, "systemctl", "restart", "systemd-networkd")
	} else {
		command(t, nil, "ifup", s.Name)
	}

	t.Cleanup(func() { command(t, nil, "ip", "link", "del", s.Name) })
	waitReady := func(active string) {
		t.Helper()
		var status []byte

		for range 40 {
			status = command(
				t,
				jsonBytes(t, s),
				"perl",
				"-e",
				netconfig.RuntimeScript,
				"--",
				"status",
				string(backend),
			)
			if bytes.Contains(status, []byte(`"ready":true`)) {
				break
			}

			time.Sleep(time.Second)
		}

		if !bytes.Contains(status, []byte(`"ready":true`)) {
			t.Fatalf("bond status: %s", status)
		}

		state := command(t, nil, "ip", "-j", "-d", "link", "show", s.Name)
		if !bytes.Contains(state, []byte(`"active_slave":"`+active+`"`)) {
			t.Fatalf("unexpected active member: %s", state)
		}

		addrs := command(t, nil, "ip", "-j", "addr", "show", s.Name)
		if !bytes.Contains(addrs, []byte("fd99:1234::ff:fe00:1")) {
			t.Fatalf("missing stable SLAAC address: %s", addrs)
		}
	}
	waitReady("vf0")
	identity := leaseIdentity(t, leaseFile, s.Bond.Members[1].MAC)
	command(t, nil, "ip", "-n", ns, "link", "set", "peervf0", "down")
	time.Sleep(time.Second)
	waitReady("vf1")
	if backend == netconfig.Netplan {
		command(t, nil, "networkctl", "renew", s.Name)
	} else {
		command(
			t,
			jsonBytes(t, s),
			"perl",
			"-e",
			netconfig.RuntimeScript,
			"--",
			"activate",
			string(backend),
		)
	}

	var renewed string

	for range 15 {
		renewed = leaseIdentity(t, leaseFile, s.Bond.Members[0].MAC)
		if renewed != "" {
			break
		}

		time.Sleep(time.Second)
	}

	if identity == "" || renewed != identity {
		t.Fatalf("DHCP client identity before and after failover: %q %q", identity, renewed)
	}

	command(t, nil, "ip", "-n", ns, "link", "set", "peervf0", "up")
	time.Sleep(time.Second)
	waitReady("vf1")
	if backend == netconfig.Ifupdown {
		command(t, nil, "ifdown", s.Name)
	}
}

func leaseIdentity(t *testing.T, leaseFile, mac string) string {
	t.Helper()
	leases, err := os.ReadFile(leaseFile)
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.SplitSeq(string(leases), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 5 && fields[1] == mac {
			return fields[4]
		}
	}

	return ""
}
