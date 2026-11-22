package netconfig_test

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/internal/netconfig"
)

const (
	proxyAddress = "::c000:22a"
	otherProxy   = "2001:db8:10::93"
)

func TestIfupdownProxyNeighborPlan(t *testing.T) {
	const main = "/etc/network/interfaces"
	const managed = "/etc/network/pveguest/eth0"
	settings := netconfig.Settings{
		Name:               "eth0",
		MAC:                "02:01:02:03:04:05",
		IPv6ProxyNeighbors: []string{proxyAddress},
	}
	files := map[string]string{main: "auto lo\niface lo inet loopback\n"}
	edits, err := settings.Plan(netconfig.Ifupdown, files, false)
	if err != nil {
		t.Fatal(err)
	}

	for path, edit := range edits {
		files[path] = *edit.After
	}

	want := "    pre-up sysctl -qw net/ipv6/conf/eth0/proxy_ndp=1\n" +
		"    up ip -6 neigh replace proxy " + proxyAddress + " dev eth0\n"
	if !strings.Contains(files[managed], want) {
		t.Fatalf("managed interface lacks its boot settings:\n%s", files[managed])
	}

	if changes, err := settings.Plan(netconfig.Ifupdown, files, false); err != nil ||
		len(changes) != 0 {
		t.Fatalf("repeated plan contains changes: %v %v", changes, err)
	}

	for _, backend := range []netconfig.Backend{netconfig.Netplan, netconfig.Networkd} {
		if _, err := settings.Plan(backend, files, false); err == nil {
			t.Fatalf("%s accepted IPv6 proxy neighbors", backend)
		}
	}

	settings.IPv6ProxyNeighbors = nil
	edits, err = settings.Plan(netconfig.Ifupdown, files, false)
	if err != nil {
		t.Fatal(err)
	}

	if content := edits[managed].After; content == nil || strings.Contains(*content, "proxy") {
		t.Fatalf("managed interface retained proxy instructions: %#v", content)
	}
}

func TestLinuxIfupdownProxyNeighbors(t *testing.T) {
	if os.Getenv("PVEGUEST_ACC_DISPOSABLE_LINUX") != "1" {
		t.Skip("Set PVEGUEST_ACC_DISPOSABLE_LINUX=1 inside the disposable backend test guest.")
	}

	const backend = "ifupdown"
	const port = "accproxy"
	const mac = "02:01:02:03:04:09"
	settings := netconfig.Settings{
		Name:               port,
		MAC:                mac,
		Addresses:          []string{},
		DNS:                []string{},
		Search:             []string{},
		IPv6ProxyNeighbors: []string{proxyAddress},
	}
	command(t, nil, "ip", "link", "add", port, "address", mac, "type", "dummy")
	t.Cleanup(func() { command(t, nil, "ip", "link", "delete", port) })

	configure := func(removing bool) {
		t.Helper()
		var files map[string]string
		contents := command(t, nil, "perl", "-e", netconfig.FileScript, "--", backend, "read", port)
		if err := json.Unmarshal(contents, &files); err != nil {
			t.Fatal(err)
		}

		edits, err := settings.Plan(netconfig.Ifupdown, files, removing)
		if err != nil {
			t.Fatal(err)
		}

		command(
			t,
			jsonBytes(t, edits),
			"perl",
			"-e",
			netconfig.FileScript,
			"--",
			backend,
			"write",
			port,
		)
	}
	runtime := func(action string) []byte {
		t.Helper()

		return command(
			t,
			jsonBytes(t, settings),
			"perl",
			"-e",
			netconfig.RuntimeScript,
			"--",
			action,
			backend,
		)
	}
	neighbor := func(action, address string) {
		t.Helper()
		command(t, nil, "ip", "-6", "neigh", action, "proxy", address, "dev", port)
	}
	expectNeighbors := func(wantManaged, wantUnrelated bool) {
		t.Helper()
		observed := command(t, nil, "ip", "-j", "-6", "neigh", "show", "proxy", "dev", port)

		type entry struct {
			Address netip.Addr `json:"dst"`
		}

		var entries []entry
		if err := json.Unmarshal(observed, &entries); err != nil {
			t.Fatal(err)
		}

		if slices.Contains(
			entries,
			entry{Address: netip.MustParseAddr(proxyAddress)},
		) != wantManaged ||
			slices.Contains(
				entries,
				entry{Address: netip.MustParseAddr(otherProxy)},
			) != wantUnrelated {
			t.Fatalf(
				"expected managed=%t and unrelated=%t, got %s",
				wantManaged,
				wantUnrelated,
				observed,
			)
		}
	}

	configure(false)
	t.Cleanup(func() { configure(true) })
	expectNeighbors(false, false)
	command(t, nil, "ifup", port)
	t.Cleanup(func() { command(t, nil, "ifdown", port) })
	expectNeighbors(true, false)
	neighbor("add", otherProxy)
	neighbor("delete", proxyAddress)
	if output := runtime("status"); !bytes.Contains(
		output,
		[]byte("Missing IPv6 proxy neighbors"),
	) {
		t.Fatalf("missing live proxy entry did not produce drift: %s", output)
	}

	if output := runtime("activate"); !bytes.Contains(output, []byte(`"ready":true`)) {
		t.Fatalf("activating did not restore the configured proxy entry: %s", output)
	}

	expectNeighbors(true, true)

	expectIfupFailure(t, settings)
	expectNeighbors(false, true)
	runtime("activate")
	expectNeighbors(true, true)

	settings.IPv6ProxyNeighbors = nil
	settings.ObsoleteProxyNeighbors = []string{proxyAddress}
	configure(false)
	if output := runtime("status"); !bytes.Contains(
		output,
		[]byte("Obsolete IPv6 proxy neighbors"),
	) {
		t.Fatalf("the obsolete live proxy entry did not produce drift: %s", output)
	}

	if output := runtime("activate"); !bytes.Contains(output, []byte(`"ready":true`)) {
		t.Fatalf("activating did not remove the obsolete proxy entry: %s", output)
	}

	expectNeighbors(false, true)

	neighbor("add", proxyAddress)
	runtime("remove-proxy-neighbors")
	expectNeighbors(false, true)
	runtime("remove-proxy-neighbors")
	expectNeighbors(false, true)
}

func expectIfupFailure(t *testing.T, settings netconfig.Settings) {
	t.Helper()
	filename := "/etc/network/pveguest/" + settings.Name
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	restore := func() {
		t.Helper()
		if err := os.WriteFile(filename, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer restore()

	broken := string(contents) + "    pre-up /bin/false\n"
	if err := os.WriteFile(filename, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	process := exec.CommandContext(
		t.Context(),
		"perl",
		"-e",
		netconfig.RuntimeScript,
		"--",
		"activate",
		"ifupdown",
	)
	process.Stdin = bytes.NewReader(jsonBytes(t, settings))
	output, err := process.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("ifup "+settings.Name+" exited with status")) {
		t.Fatalf("activation did not preserve the ifup failure: %v\n%s", err, output)
	}
}
