package netconfig

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"regexp"
	"slices"
)

// Bond configures Linux active-backup bonding with independent member MACs.
type Bond struct {
	Members         []BondMember `json:"members"`
	Primary         string       `json:"primary"`
	MIIMon          int64        `json:"miimon"`
	FailOverMAC     string       `json:"fail_over_mac"`
	PrimaryReselect string       `json:"primary_reselect"`
}

// BondMember identifies one physical interface by its assigned MAC.
type BondMember struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

var memberName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,14}$`)

func ethernetMAC(value string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(value)
	if err != nil || len(mac) != 6 || mac.String() != value || mac[0]&1 != 0 ||
		value == "00:00:00:00:00:00" {
		slog.Error("invalid bond MAC", "address", value, "err", errNetworkConfig)

		return nil, fmt.Errorf(
			"%w: expected a nonzero lowercase Ethernet unicast MAC, got %q",
			errNetworkConfig,
			value,
		)
	}

	return mac, nil
}

// Validate checks the bond options and identifies ambiguous member assignments.
//
// A guest can stage the bond before either port is attached. For example,
// these two assignments would identify both ports by the same MAC.
//
//	vf0 = 02:00:00:00:00:01
//	vf1 = 02:00:00:00:00:01
//
// Validate rejects the duplicate before the guest changes its network.
func (b Bond) Validate(name string) error {
	if len(b.Members) < 2 || b.MIIMon <= 0 || b.MIIMon > math.MaxInt32 ||
		b.FailOverMAC != "active" ||
		!slices.Contains([]string{"always", "better", "failure"}, b.PrimaryReselect) {
		slog.Error("invalid bond parameters", "interface", name, "err", errNetworkConfig)

		return fmt.Errorf(
			"%w: invalid member count, miimon, fail_over_mac or primary_reselect",
			errNetworkConfig,
		)
	}

	names := map[string]bool{}
	macs := map[string]bool{}

	for _, member := range b.Members {
		if !memberName.MatchString(member.Name) || member.Name == name || names[member.Name] {
			return fmt.Errorf(
				"%w: invalid or conflicting bond member name %q",
				errNetworkConfig,
				member.Name,
			)
		}

		if _, err := ethernetMAC(member.MAC); err != nil {
			return err
		}

		if macs[member.MAC] {
			return fmt.Errorf("%w: duplicate bond member MAC %s", errNetworkConfig, member.MAC)
		}

		macs[member.MAC], names[member.Name] = true, true
	}

	if !names[b.Primary] {
		return fmt.Errorf(
			"%w: primary %q must match a bond member name",
			errNetworkConfig,
			b.Primary,
		)
	}

	return nil
}

func (b Bond) memberNames() []string {
	names := make([]string, 0, len(b.Members))
	for _, member := range b.Members {
		names = append(names, member.Name)
	}

	return names
}

// slaacToken uses the Ethernet modified EUI-64 identifier for the outer MAC.
func slaacToken(value string) (string, error) {
	mac, err := ethernetMAC(value)
	if err != nil {
		return "", err
	}

	var token [16]byte
	copy(token[8:], []byte{mac[0] ^ 2, mac[1], mac[2], 0xff, 0xfe, mac[3], mac[4], mac[5]})

	return netip.AddrFrom16(token).String(), nil
}
