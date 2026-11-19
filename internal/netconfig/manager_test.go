package netconfig_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/internal/netconfig"
)

// TestNetplanAdoption checks that editing the matched Ethernet definition
// preserves another port and a nondefault route on the adopted port.
func TestNetplanAdoption(t *testing.T) {
	const source = "/etc/netplan/50-cloud-init.yaml"
	const original = `network:
  version: 2
  ethernets:
    unrelated:
      dhcp4: true
    original:
      match: {macaddress: "02:01:02:03:04:05"}
      set-name: eth0
      addresses: [192.0.2.10/24]
      routes:
        - {to: default, via: 192.0.2.1}
        - {to: 198.51.100.0/24, via: 192.0.2.99}
`
	settings := netconfig.Settings{
		Name:      "eth0",
		MAC:       "02:01:02:03:04:05",
		DHCP4:     true,
		SLAAC:     true,
		Addresses: []string{"192.0.2.10/24", "fd00::10/64"},
	}
	files := map[string]string{source: original}
	edits, err := settings.Plan(netconfig.Netplan, files, false)
	if err != nil {
		t.Fatal(err)
	}

	for file, edit := range edits {
		files[file] = *edit.After
	}

	updated := files[source]
	if !strings.Contains(updated, "unrelated") || !strings.Contains(updated, "198.51.100.0/24") ||
		strings.Contains(updated, "to: default") {
		t.Fatalf("unexpected migrated source:\n%s", updated)
	}

	again, err := settings.Plan(netconfig.Netplan, files, false)
	if err != nil || len(again) != 0 {
		t.Fatalf("repeated plan contains %d edits: %v", len(again), err)
	}

	netplan, err := exec.LookPath("netplan")
	if err != nil {
		t.Log("netplan is unavailable; skipped native generation")

		return
	}

	root := t.TempDir()

	for filename, content := range files {
		dest := filepath.Join(root, filename)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := exec.CommandContext(
		t.Context(),
		netplan,
		"generate",
		"--root-dir",
		root,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("generate: %s: %v", result, err)
	}

	generated, err := os.ReadFile(
		filepath.Join(root, "run/systemd/network/10-netplan-original.network"),
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"PermanentMACAddress=" + settings.MAC,
		"IPv6AcceptRA=yes",
		"DHCP=ipv4",
		"Destination=198.51.100.0/24",
	} {
		if !bytes.Contains(generated, []byte(want)) {
			t.Fatalf("generated unit lacks %q:\n%s", want, generated)
		}
	}

	if bytes.Contains(generated, []byte("Gateway=192.0.2.1\n")) {
		t.Fatalf("generated unit retained the old gateway:\n%s", generated)
	}
}

func TestNetplanLogicalID(t *testing.T) {
	const owner = "/etc/netplan/50-port.yaml"
	const unrelated = "network:\n  version: 2\n  ethernets:\n    eth0:\n      match: {macaddress: '02:00:00:00:00:02'}\n      set-name: lan0\n      addresses: [192.0.2.9/24]\n"
	settings := netconfig.Settings{Name: "eth0", MAC: "02:00:00:00:00:01"}
	files := map[string]string{owner: unrelated}
	if edits, err := settings.Plan(netconfig.Netplan, files, false); err == nil || len(edits) > 0 {
		t.Fatalf(
			"expected an occupied netplan ID error before changing the other port: %v %v",
			edits,
			err,
		)
	}

	files["/etc/netplan/60-guest.yaml"] = "network:\n  version: 2\n  ethernets:\n    guest:\n      match: {macaddress: '02:00:00:00:00:01'}\n      set-name: eth0\n      addresses: [192.0.2.10/24]\n"
	edits, err := settings.Plan(netconfig.Netplan, files, false)
	if err != nil {
		t.Fatal(err)
	}

	if _, changed := edits[owner]; changed {
		t.Fatalf("adoption edited a different MAC: %v", edits)
	}
}

// TestLinuxManagers runs only in a disposable Linux guest with both managers
// installed. It adopts a dedicated interface, verifies activation, and checks
// that a stale cloud-init source cannot replace the selected networkd unit.
func TestLinuxManagers(t *testing.T) {
	if os.Getenv("PVEGUEST_ACC_DISPOSABLE_LINUX") != "1" {
		t.Skip("Set PVEGUEST_ACC_DISPOSABLE_LINUX=1 inside the disposable backend test guest.")
	}

	for _, backend := range []netconfig.Backend{
		netconfig.Ifupdown,
		netconfig.Networkd,
		netconfig.Netplan,
	} {
		t.Run(string(backend), func(t *testing.T) { exerciseManager(t, backend) })
	}
}

func command(t *testing.T, stdin []byte, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v: %s", name, err, stderr.Bytes())
	}

	return data
}

func exerciseManager(t *testing.T, backend netconfig.Backend) {
	t.Helper()
	port := "acc" + string(backend)
	mac := map[netconfig.Backend]string{
		netconfig.Ifupdown: "02:01:02:03:04:05",
		netconfig.Networkd: "02:01:02:03:04:06",
		netconfig.Netplan:  "02:01:02:03:04:07",
	}[backend]
	settings := netconfig.Settings{
		Name:      port,
		MAC:       mac,
		Addresses: []string{"192.0.2.10/24"},
		DNS:       []string{},
		Search:    []string{},
		MTU:       1470,
		DHCP4:     true,
		SLAAC:     true,
	}
	command(t, nil, "netplan", "generate")
	command(t, nil, "networkctl", "reload")
	routerPeer(t, port)
	command(t, nil, "ip", "link", "set", port, "address", mac)
	command(t, nil, "sysctl", "-qw", "net.ipv6.conf.all.forwarding=1")
	source, initial := fixtureSource(backend, port, mac)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(source, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.Remove(source); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	fileCall := func(action string, stdin []byte) []byte {
		return command(
			t,
			stdin,
			"perl",
			"-e",
			netconfig.FileScript,
			"--",
			string(backend),
			action,
			port,
			mac,
		)
	}
	inspect := func(deleting bool) map[string]netconfig.Edit {
		var files map[string]string
		if err := json.Unmarshal(fileCall("read", nil), &files); err != nil {
			t.Fatal(err)
		}

		edits, err := settings.Plan(backend, files, deleting)
		if err != nil {
			t.Fatal(err)
		}

		if backend == netconfig.Netplan {
			action := "render"
			if deleting {
				action = "unrender"
			}

			var units map[string]netconfig.Edit
			if err := json.Unmarshal(fileCall(action, jsonBytes(t, edits)), &units); err != nil {
				t.Fatal(err)
			}

			for path, value := range units {
				edits[path] = value
			}
		}

		return edits
	}
	fileCall("write", jsonBytes(t, inspect(false)))
	t.Cleanup(func() { fileCall("write", jsonBytes(t, inspect(true))) })
	if len(inspect(false)) != 0 {
		t.Fatal("persistent configuration differs immediately after staging")
	}

	if bytes.Contains(
		command(t, nil, "ip", "-j", "address", "show", "dev", port),
		[]byte("192.0.2.10"),
	) {
		t.Fatal("staging changed the live address")
	}

	if backend == netconfig.Netplan {
		if err := os.WriteFile(source, []byte(initial), 0o600); err != nil {
			t.Fatal(err)
		}

		command(t, nil, "netplan", "generate")
		if len(inspect(false)) == 0 {
			t.Fatal("restoring the old cloud-init definition did not produce drift")
		}
	}

	runtime := func(action string) []byte {
		return command(
			t,
			jsonBytes(t, settings),
			"perl",
			"-e",
			netconfig.RuntimeScript,
			"--",
			action,
			string(backend),
		)
	}
	if backend != netconfig.Ifupdown {
		expectRestartRequired(t, backend, settings)
		command(t, nil, "systemctl", "restart", "systemd-networkd.service")
		stageSecondPort(t)
	}

	command(t, nil, "ip", "link", "set", port, "up", "mtu", "1470")
	command(t, nil, "ip", "address", "replace", settings.Addresses[0], "dev", port)
	command(
		t,
		nil,
		"ip",
		"address",
		"add",
		"fd88:1234::1/64",
		"dev",
		port,
		"nodad",
		"valid_lft",
		"600",
		"preferred_lft",
		"300",
	)
	settings.DHCP4 = false
	if output := runtime("status"); !bytes.Contains(
		output,
		[]byte("Waiting for a SLAAC IPv6 address"),
	) {
		t.Fatalf("finite-lifetime static IPv6 without router advertisements: %s", output)
	}

	settings.DHCP4 = true
	startRouter(t)
	if !bytes.Contains(runtime("activate"), []byte(`"ready":true`)) {
		t.Fatal("activation did not verify the live interface")
	}

	if !bytes.Contains(runtime("status"), []byte(`"ready":true`)) {
		t.Fatal("runtime status differs from the applied configuration")
	}

	if backend != netconfig.Ifupdown {
		if bytes.Contains(
			command(t, nil, "ip", "-j", "addr", "show", "dev", "accpending"),
			[]byte("192.0.2.77"),
		) {
			t.Fatal("activating one interface configured a second staged interface")
		}

		settings.DNS = []string{"192.0.2.53"}
		fileCall("write", jsonBytes(t, inspect(false)))
		expectRestartRequired(t, backend, settings)
		settings.DNS = []string{}
		fileCall("write", jsonBytes(t, inspect(false)))
	}

	if output := command(t, nil, "sysctl", "-n", "net.ipv6.conf.all.forwarding"); strings.TrimSpace(
		string(output),
	) != "1" {
		t.Fatal("guest forwarding changed")
	}

	command(
		t,
		nil,
		"ip",
		"route",
		"add",
		"default",
		"via",
		"192.0.2.1",
		"dev",
		port,
		"metric",
		"987",
	)
	if !bytes.Contains(runtime("status"), []byte(`"ready":false`)) {
		t.Fatal("an unexpected default gateway did not produce runtime drift")
	}

	command(
		t,
		nil,
		"ip",
		"route",
		"delete",
		"default",
		"via",
		"192.0.2.1",
		"dev",
		port,
		"metric",
		"987",
	)
	command(t, nil, "ip", "address", "delete", settings.Addresses[0], "dev", port)
	if !bytes.Contains(runtime("status"), []byte(`"ready":false`)) {
		t.Fatal("removing a configured live address did not produce runtime drift")
	}
}

func expectRestartRequired(t *testing.T, backend netconfig.Backend, settings netconfig.Settings) {
	t.Helper()
	cmd := exec.CommandContext(
		t.Context(),
		"perl",
		"-e",
		netconfig.RuntimeScript,
		"--",
		"activate",
		string(backend),
	)
	cmd.Stdin = bytes.NewReader(jsonBytes(t, settings))
	output, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("Restart the guest")) {
		t.Fatalf("activation before loading staged units: %v %s", err, output)
	}
}

func stageSecondPort(t *testing.T) {
	t.Helper()
	const port = "accpending"
	settings := netconfig.Settings{
		Name:      port,
		MAC:       "02:00:00:00:00:77",
		Addresses: []string{"192.0.2.77/32"},
	}
	command(t, nil, "ip", "link", "add", port, "type", "dummy")
	t.Cleanup(func() { command(t, nil, "ip", "link", "delete", port) })
	command(t, nil, "ip", "link", "set", port, "address", settings.MAC, "up")
	write := func(deleting bool) {
		argv := []string{"-e", netconfig.FileScript, "--", "networkd", "read", port}
		var current map[string]string
		if err := json.Unmarshal(command(t, nil, "perl", argv...), &current); err != nil {
			t.Fatal(err)
		}

		edits, err := settings.Plan(netconfig.Networkd, current, deleting)
		if err != nil {
			t.Fatal(err)
		}

		argv[4] = "write"
		command(t, jsonBytes(t, edits), "perl", argv...)
	}
	write(false)
	t.Cleanup(func() { write(true) })
}

func fixtureSource(backend netconfig.Backend, name, mac string) (string, string) {
	switch backend {
	case netconfig.Netplan:
		return "/etc/netplan/50-pveguest-acceptance.yaml", fmt.Sprintf(
			"network:\n  version: 2\n  ethernets:\n    %s:\n      match: {macaddress: %q}\n      set-name: %s\n      addresses: [192.0.2.11/24]\n      routes: [{to: default, via: 192.0.2.1}]\n",
			name,
			mac,
			name,
		)
	case netconfig.Networkd:
		return "/etc/systemd/network/" + name + ".network", "[Match]\nName=" + name + "\n[Network]\nAddress=192.0.2.11/24\nGateway=192.0.2.1\n"
	default:
		return "/etc/network/interfaces.d/pveguest-acceptance", "auto " + name + "\niface " + name + " inet static\n  address 192.0.2.11/24\n  gateway 192.0.2.1\n"
	}
}

func jsonBytes[T any](t *testing.T, v T) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func routerPeer(t *testing.T, port string) {
	t.Helper()
	const namespace = "pveguest-router"
	const peer = "accrouter"
	command(t, nil, "ip", "netns", "add", namespace)
	t.Cleanup(func() { command(t, nil, "ip", "netns", "delete", namespace) })
	command(t, nil, "ip", "link", "add", port, "type", "veth", "peer", "name", peer)
	t.Cleanup(func() { command(t, nil, "ip", "link", "delete", port) })
	command(t, nil, "ip", "link", "set", peer, "netns", namespace)
	command(t, nil, "ip", "-n", namespace, "link", "set", peer, "up")
	command(t, nil, "ip", "-n", namespace, "link", "set", "lo", "up")
	command(t, nil, "ip", "-n", namespace, "address", "add", "198.51.100.1/24", "dev", peer)
	command(
		t,
		nil,
		"ip",
		"-n",
		namespace,
		"address",
		"add",
		"fd99:1234::1/64",
		"dev",
		peer,
		"nodad",
	)
}

func startRouter(t *testing.T) {
	t.Helper()
	const namespace = "pveguest-router"
	const peer = "accrouter"
	pidFile := filepath.Join(t.TempDir(), "dnsmasq.pid")
	command(
		t,
		nil,
		"ip",
		"netns",
		"exec",
		namespace,
		"dnsmasq",
		"--conf-file=/dev/null",
		"--port=0",
		"--user=root",
		"--bind-interfaces",
		"--interface="+peer,
		"--enable-ra",
		"--dhcp-option=option:mtu,1400",
		"--dhcp-range=198.51.100.100,198.51.100.150,255.255.255.0,10m",
		"--dhcp-range=::,constructor:"+peer+",ra-only,64,10m",
		"--ra-param="+peer+",5,90",
		"--dhcp-leasefile="+filepath.Join(t.TempDir(), "leases"),
		"--pid-file="+pidFile,
	)
	pidText, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			t.Error(err)
		}
	})
}
