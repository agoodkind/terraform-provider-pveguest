package netconfig_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/internal/netconfig"
	"golang.org/x/sys/unix"
)

// TestLinuxBond uses a disposable Linux namespace with two Ethernet peers.
func TestLinuxBond(t *testing.T) {
	if os.Getenv("PVEGUEST_ACC_DISPOSABLE_BOND") != "1" {
		t.Skip("requires a disposable Linux network namespace")
	}

	s := bondSettings()
	s.MAC = "02:00:00:00:00:ab"

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
	events := watchBondEvents(t, leaseFile, backend)
	waitReady := func(active, slaac, primaryMII string) {
		t.Helper()
		waitForBondEvent(
			t,
			events,
			func() string {
				status := command(
					t,
					jsonBytes(t, s),
					"perl",
					"-e",
					netconfig.RuntimeScript,
					"--",
					"status",
					string(backend),
				)
				if !bytes.Contains(status, []byte(`"ready":true`)) {
					return string(status)
				}

				state := command(t, nil, "ip", "-j", "-d", "link", "show", s.Name)
				if !bytes.Contains(state, []byte(`"active_slave":"`+active+`"`)) {
					return "unexpected active member: " + string(state)
				}

				carrier, err := os.ReadFile(
					"/sys/class/net/" + s.Bond.Primary + "/bonding_slave/mii_status",
				)
				if err != nil {
					t.Fatal(err)
				}

				if strings.TrimSpace(string(carrier)) != primaryMII {
					return "primary MII status: " + string(carrier)
				}

				addresses := command(t, nil, "ip", "-j", "addr", "show", s.Name)
				if !bytes.Contains(addresses, []byte(slaac)) {
					return "missing SLAAC address " + slaac + ": " + string(addresses)
				}

				return ""
			},
		)
	}
	waitReady("vf0", "fd99:1234::ff:fe00:ab", "up")
	expectedID := "01:" + s.MAC
	if backend == netconfig.Netplan {
		expectedID = "ff:00:00:00:ab:00:03:00:01:" + s.MAC
	}

	identity := leaseIdentity(t, leaseFile, s.Bond.Members[1].MAC)
	if identity != expectedID {
		t.Fatalf("primary DHCP client identifier = %q, want %q", identity, expectedID)
	}

	command(t, nil, "ip", "-n", ns, "link", "set", "peervf0", "down")
	waitReady("vf1", "fd99:1234::ff:fe00:ab", "down")
	command(t, nil, "ip", "-n", ns, "addr", "add", "fd99:5678::1/64", "dev", "accrouter", "nodad")
	waitReady("vf1", "fd99:5678::ff:fe00:ab", "down")
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

	waitForBondEvent(
		t,
		events,
		func() string {
			renewed := leaseIdentity(t, leaseFile, s.Bond.Members[0].MAC)
			if renewed != expectedID {
				return fmt.Sprintf(
					"secondary DHCP client identifier = %q, want %q",
					renewed,
					expectedID,
				)
			}

			return ""
		},
	)

	command(t, nil, "ip", "-n", ns, "link", "set", "peervf0", "up")
	waitReady("vf1", "fd99:5678::ff:fe00:ab", "up")
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

// Subscribe before checking readiness so events during an observation remain queued.
func watchBondEvents(t *testing.T, leaseFile string, backend netconfig.Backend) []unix.PollFd {
	t.Helper()
	netlink, err := unix.Socket(
		unix.AF_NETLINK,
		unix.SOCK_RAW|unix.SOCK_CLOEXEC,
		unix.NETLINK_ROUTE,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := unix.Close(netlink); err != nil {
			t.Error(err)
		}
	})
	groups := uint32(
		unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE,
	)
	if err := unix.Bind(
		netlink,
		&unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: groups},
	); err != nil {
		t.Fatal(err)
	}

	files, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := unix.Close(files); err != nil {
			t.Error(err)
		}
	})
	directories := []string{filepath.Dir(leaseFile)}
	if backend == netconfig.Netplan {
		directories = append(directories, "/run/systemd/netif/links")
	}

	for _, directory := range directories {
		if _, err := unix.InotifyAddWatch(
			files,
			directory,
			unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO,
		); err != nil {
			t.Fatal(err)
		}
	}

	return []unix.PollFd{
		{Fd: int32(netlink), Events: unix.POLLIN},
		{Fd: int32(files), Events: unix.POLLIN},
	}
}

func waitForBondEvent(
	t *testing.T,
	descriptors []unix.PollFd,
	pending func() string,
) {
	t.Helper()
	// The deadline bounds a stalled external manager. Only kernel or file events trigger another observation.
	deadline := time.Now().Add(45 * time.Second)
	buffer := make([]byte, 65536)

	for {
		message := pending()
		if message == "" {
			return
		}

		timeout := time.Until(deadline).Milliseconds()
		if timeout <= 0 {
			t.Fatal(message)
		}

		count, err := unix.Poll(descriptors, int(timeout))
		if errors.Is(err, unix.EINTR) {
			continue
		}

		if err != nil {
			t.Fatal(err)
		}

		if count == 0 {
			t.Fatal(message)
		}

		for _, descriptor := range descriptors {
			if descriptor.Revents == 0 {
				continue
			}

			if _, err := unix.Read(int(descriptor.Fd), buffer); err != nil {
				t.Fatal(err)
			}
		}
	}
}
