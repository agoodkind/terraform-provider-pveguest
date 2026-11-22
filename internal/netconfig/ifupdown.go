package netconfig

import (
	_ "embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
)

//go:embed scripts/dhclient_mtu.sh
var dhclientMTUHook string

const interfacesMain = "/etc/network/interfaces"

var ownedIfupdownOptions = []string{
	"address",
	"netmask",
	"gateway",
	"broadcast",
	"mtu",
	"dns-nameservers",
	"dns-search",
	"accept_ra",
	"autoconf",
}

// Proxmox retains source directives when it regenerates LXC net entries.
func (s Settings) ifupdownEdits(files map[string]string, removing bool) (map[string]Edit, error) {
	owner := path.Join("/etc/network/pveguest", s.Name)
	changes := map[string]Edit{}
	hook := "/etc/dhcp/dhclient-enter-hooks.d/pveguest-" + hex.EncodeToString([]byte(s.Name))
	if err := ownedNetworkFile(
		changes,
		files,
		hook,
		fmt.Sprintf(dhclientMTUHook, s.Name, s.MTU),
		removing || !s.DHCP4 || s.MTU == 0,
	); err != nil {
		return nil, err
	}

	if err := ownedNetworkFile(changes, files, owner, s.ifupdownStanza(), removing); err != nil {
		return nil, err
	}

	for filename, original := range files {
		if filename == owner || filename == hook {
			continue
		}

		editor := interfacesEditor{
			target:   s.Name,
			source:   "source " + owner,
			main:     filename == interfacesMain,
			removing: removing,
		}

		for line := range strings.SplitAfterSeq(original, "\n") {
			if err := editor.line(line); err != nil {
				slog.Error("parse ifupdown interface", "path", filename, "err", err)

				return nil, fmt.Errorf("%s: %w", filename, err)
			}
		}

		content := editor.result()
		if content != original {
			changes[filename] = Edit{Before: &original, After: &content}
		}
	}

	return changes, nil
}

type interfacesEditor struct {
	target, source                       string
	removing, main, inTarget, keptSource bool
	out                                  strings.Builder
}

func (editor *interfacesEditor) line(line string) error {
	clean, _, _ := strings.Cut(line, "#")
	fields := strings.Fields(clean)
	if len(fields) == 0 {
		editor.out.WriteString(line)

		return nil
	}

	directive := fields[0]
	if strings.TrimSpace(clean) == editor.source {
		if !editor.removing && editor.main && !editor.keptSource {
			editor.out.WriteString(line)
			editor.keptSource = true
		}

		editor.inTarget = false

		return nil
	}

	if editor.removing {
		editor.out.WriteString(line)

		return nil
	}

	boot := directive == "auto" || strings.HasPrefix(directive, "allow-")
	if boot ||
		slices.Contains([]string{"iface", "mapping", "source", "source-directory"}, directive) {
		editor.inTarget = false
	}

	if directive == "iface" && len(fields) > 1 && fields[1] == editor.target {
		editor.inTarget = true

		return nil
	}

	if editor.inTarget {
		if !slices.Contains(ownedIfupdownOptions, directive) {
			slog.Error(
				"unsupported ifupdown directive",
				"interface",
				editor.target,
				"directive",
				directive,
				"err",
				errNetworkConfig,
			)

			return fmt.Errorf(
				"%w: migrate unsupported %s directive on %s before adoption",
				errNetworkConfig,
				directive,
				editor.target,
			)
		}

		return nil
	}

	if boot && slices.Contains(fields[1:], editor.target) {
		remaining := slices.DeleteFunc(
			fields[1:],
			func(name string) bool { return name == editor.target },
		)
		if len(remaining) > 0 {
			fmt.Fprintf(&editor.out, "%s %s\n", directive, strings.Join(remaining, " "))
		}

		return nil
	}

	editor.out.WriteString(line)

	return nil
}

func (editor *interfacesEditor) result() string {
	content := editor.out.String()
	if editor.main && !editor.removing && !editor.keptSource {
		return strings.TrimRight(content, "\n") + "\n\n" + editor.source + "\n"
	}

	return content
}

func (s Settings) ifupdownStanza() string {
	var out strings.Builder
	fmt.Fprintf(&out, "auto %s\n", s.Name)
	method := "manual"
	if s.DHCP4 {
		method = "dhcp"
	}

	fmt.Fprintf(&out, "iface %s inet %s\n", s.Name, method)
	fmt.Fprintf(&out, "    pre-up grep -Fxi -- %s /sys/class/net/%s/address\n", s.MAC, s.Name)
	fmt.Fprintf(&out, "    pre-up ip link set dev %s up\n", s.Name)
	if s.MTU != 0 {
		fmt.Fprintf(
			&out,
			"    mtu %d\n    pre-up ip link set dev %s mtu %d\n",
			s.MTU,
			s.Name,
			s.MTU,
		)
	}

	if len(s.DNS) > 0 {
		fmt.Fprintf(&out, "    dns-nameservers %s\n", strings.Join(s.DNS, " "))
	}

	if len(s.Search) > 0 {
		fmt.Fprintf(&out, "    dns-search %s\n", strings.Join(s.Search, " "))
	}

	for _, address := range s.Addresses {
		fmt.Fprintf(&out, "    up ip address replace %s dev %s\n", address, s.Name)
	}

	fmt.Fprintf(&out, "iface %s inet6 manual\n", s.Name)
	fmt.Fprintf(&out, "    pre-up grep -Fxi -- %s /sys/class/net/%s/address\n", s.MAC, s.Name)
	acceptRA, autoconf := 0, 0
	if s.SLAAC {
		acceptRA, autoconf = 2, 1
	}

	fmt.Fprintf(
		&out,
		"    pre-up sysctl -qw net/ipv6/conf/%s/accept_ra=%d net/ipv6/conf/%s/autoconf=%d\n",
		s.Name,
		acceptRA,
		s.Name,
		autoconf,
	)

	if len(s.IPv6ProxyNeighbors) > 0 {
		fmt.Fprintf(&out, "    pre-up sysctl -qw net/ipv6/conf/%s/proxy_ndp=1\n", s.Name)
	}

	for _, address := range s.IPv6ProxyNeighbors {
		fmt.Fprintf(&out, "    up ip -6 neigh replace proxy %s dev %s\n", address, s.Name)
	}

	return out.String()
}
