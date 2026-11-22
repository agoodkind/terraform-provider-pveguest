package netconfig

import (
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
)

const networkOwnerHeader = "# Managed by pveguest_network_interface\n"

func (s Settings) networkdEdits(
	files map[string]string,
	removing bool,
) (map[string]Edit, error) {
	changes := map[string]Edit{}
	base := path.Join("/etc/systemd/network", "05-pveguest-"+s.Name)

	link := fmt.Sprintf("[Match]\nMACAddress=%s\n\n[Link]\nName=%s\n", s.MAC, s.Name)
	for filename, body := range map[string]string{
		base + ".network": s.nativeNetwork(),
		base + ".link":    link,
	} {
		if err := ownedNetworkFile(changes, files, filename, body, removing); err != nil {
			return nil, err
		}
	}

	old := path.Join("/etc/systemd/network", s.Name+".network")
	content, exists := files[old]
	if removing || !exists {
		return changes, nil
	}

	// Proxmox writes this file for netN devices. Reject other directives
	// before replacing it so custom routes and shared matches remain intact.
	section, matched := "", false

	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			section = line

			continue
		}

		key, value, parsed := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if parsed && section == "[Match]" && key == "Name" && value == s.Name {
			matched = true

			continue
		}

		if parsed && section == "[Network]" &&
			slices.Contains(
				[]string{
					"Description",
					"Address",
					"Gateway",
					"DHCP",
					"IPv6AcceptRA",
					"DNS",
					"Domains",
				},
				key,
			) {
			continue
		}

		slog.Error("cannot adopt native networkd unit", "path", old, "err", errNetworkConfig)

		return nil, fmt.Errorf(
			"%w: %s contains %s %s; migrate this directive before adoption",
			errNetworkConfig,
			old,
			section,
			key,
		)
	}

	if !matched {
		slog.Error("cannot adopt native networkd unit", "path", old, "err", errNetworkConfig)

		return nil, fmt.Errorf("%w: %s does not match only %s", errNetworkConfig, old, s.Name)
	}

	changes[old] = Edit{Before: &content}

	return changes, nil
}

func (s Settings) nativeNetwork() string {
	var network strings.Builder
	fmt.Fprintf(&network, "[Match]\nMACAddress=%s\n\n[Network]\n", s.MAC)
	dhcp := "no"
	if s.DHCP4 {
		dhcp = "ipv4"
	}

	fmt.Fprintf(&network, "DHCP=%s\nIPv6AcceptRA=%t\n", dhcp, s.SLAAC)

	for _, addr := range s.Addresses {
		fmt.Fprintf(&network, "Address=%s\n", addr)
	}

	for _, server := range s.DNS {
		fmt.Fprintf(&network, "DNS=%s\n", server)
	}

	if len(s.Search) > 0 {
		fmt.Fprintf(&network, "Domains=%s\n", strings.Join(s.Search, " "))
	}

	if s.MTU != 0 {
		fmt.Fprintf(&network, "\n[Link]\nMTUBytes=%d\n", s.MTU)
	}

	return network.String()
}
