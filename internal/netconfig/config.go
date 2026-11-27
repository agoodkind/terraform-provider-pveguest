// Package netconfig configures Ethernet in Linux guests.
package netconfig

import (
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// FileScript reads or atomically replaces network configuration in a guest.
//
//go:embed scripts/guest_network_files.pl
var FileScript string

// RuntimeScript activates an interface and checks its addresses and manager.
//
//go:embed scripts/guest_network_runtime.pl
var RuntimeScript string

// Settings declares addresses and per-interface DNS for a stable guest name.
type Settings struct {
	Name                   string   `json:"name"`
	MAC                    string   `json:"mac"`
	Bond                   *Bond    `json:"bond,omitempty"`
	DHCP4                  bool     `json:"dhcp4"`
	SLAAC                  bool     `json:"slaac"`
	MTU                    int64    `json:"mtu"`
	Addresses              []string `json:"addresses"`
	DNS                    []string `json:"dns"`
	Search                 []string `json:"search"`
	IPv6ProxyNeighbors     []string `json:"ipv6_proxy_neighbors,omitempty"`
	ObsoleteProxyNeighbors []string `json:"obsolete_proxy_neighbors,omitempty"`
}

// Edit compares the original contents before replacing or removing a file.
type Edit struct {
	Before *string `json:"before"`
	After  *string `json:"after"`
}

// Backend selects the network manager that owns the adopted interface.
type Backend string

const (
	// Netplan generates units for systemd-networkd.
	Netplan Backend = "netplan"
	// Networkd uses native systemd-networkd files.
	Networkd Backend = "networkd"
	// Ifupdown reads interface stanzas from Debian networking files.
	Ifupdown Backend = "ifupdown"
)

var errNetworkConfig = errors.New("invalid guest network configuration")

// Plan compares the guest's files with the requested settings.
//
// Only files that require a change appear in the result. An absent After
// value requests removal. The writer compares Before before changing a file.
//
// For example, a Debian container may lack the include for eth0. Plan
// then returns an edit to /etc/network/interfaces that adds this line.
//
//	source /etc/network/pveguest/eth0
func (s Settings) Plan(
	backend Backend,
	files map[string]string,
	removing bool,
) (map[string]Edit, error) {
	if s.Bond != nil {
		if err := s.Bond.Validate(s.Name); err != nil {
			return nil, err
		}

		if _, err := ethernetMAC(s.MAC); err != nil {
			return nil, err
		}

		if backend == Networkd {
			return nil, fmt.Errorf("%w: use netplan or ifupdown for a bond", errNetworkConfig)
		}
	}

	if len(s.IPv6ProxyNeighbors) > 0 && backend != Ifupdown {
		return nil, fmt.Errorf(
			"%w: ipv6_proxy_neighbors requires the ifupdown backend",
			errNetworkConfig,
		)
	}

	switch backend {
	case Netplan:
		return s.netplanEdits(files, removing)
	case Networkd:
		return s.networkdEdits(files, removing)
	case Ifupdown:
		return s.ifupdownEdits(files, removing)
	default:
		slog.Error("unsupported guest network backend", "backend", backend, "err", errNetworkConfig)

		return nil, fmt.Errorf("%w: unsupported backend %q", errNetworkConfig, backend)
	}
}

func ownedNetworkFile(
	changes map[string]Edit,
	files map[string]string,
	filename string,
	body string,
	removing bool,
) error {
	original, exists := files[filename]
	if exists && !strings.HasPrefix(original, networkOwnerHeader) {
		slog.Error(
			"guest network file ownership conflict",
			"path",
			filename,
			"err",
			errNetworkConfig,
		)

		return fmt.Errorf(
			"%w: %s does not have the provider ownership header",
			errNetworkConfig,
			filename,
		)
	}

	if removing {
		if exists {
			changes[filename] = Edit{Before: &original}
		}

		return nil
	}

	content := networkOwnerHeader + body
	if content != original {
		entry := Edit{After: &content}
		if exists {
			entry.Before = &original
		}

		changes[filename] = entry
	}

	return nil
}
