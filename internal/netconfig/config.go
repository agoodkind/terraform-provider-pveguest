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
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	DHCP4     bool     `json:"dhcp4"`
	SLAAC     bool     `json:"slaac"`
	MTU       int64    `json:"mtu"`
	Addresses []string `json:"addresses"`
	DNS       []string `json:"dns"`
	Search    []string `json:"search"`
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
//	changes, err := settings.Plan(Netplan, files, false)
func (s Settings) Plan(
	backend Backend,
	files map[string]string,
	removing bool,
) (map[string]Edit, error) {
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
