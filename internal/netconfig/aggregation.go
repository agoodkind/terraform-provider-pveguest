package netconfig

import (
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

func (s Settings) netplanBondEdits(
	files map[string]string,
	removing bool,
) (map[string]Edit, error) {
	managed := path.Join("/etc/netplan", "90-pveguest-"+s.Name+".yaml")
	changes := map[string]Edit{}
	if removing {
		return changes, ownedNetworkFile(changes, files, managed, "", true)
	}

	sources, err := netplanEthernetSources(files, managed, s.Name)
	if err != nil {
		return nil, err
	}

	docs, outerID, err := netplanDocuments(sources, s)
	if err != nil {
		return nil, err
	}

	ids := map[string]bool{outerID: true}

	for _, member := range s.Bond.Members {
		candidate := Settings{Name: member.Name, MAC: member.MAC}
		_, id, err := netplanDocuments(sources, candidate)
		if err != nil {
			return nil, err
		}

		ids[id] = true
	}

	for filename, doc := range docs {
		if filename == managed {
			continue
		}

		network := yamlField(doc.Content[0], "network")
		if yamlField(yamlField(network, "bonds"), s.Name) != nil {
			slog.Error("existing netplan bond", "path", filename, "err", errNetworkConfig)

			return nil, fmt.Errorf(
				"%w: an existing netplan bond in %s already uses %s",
				errNetworkConfig,
				filename,
				s.Name,
			)
		}

		entries := yamlField(network, "ethernets")

		for _, name := range append(s.Bond.memberNames(), s.Name) {
			if yamlField(entries, name) != nil && !ids[name] {
				return nil, fmt.Errorf(
					"%w: netplan ID %s in %s selects another Ethernet interface",
					errNetworkConfig,
					name,
					filename,
				)
			}
		}

		changed, err := removeBondEthernetDefinitions(entries, ids, filename)
		if err != nil {
			return nil, err
		}

		if changed {
			if !strings.HasPrefix(filename, "/etc/netplan/") {
				slog.Error("unsupported netplan source", "path", filename, "err", errNetworkConfig)

				return nil, fmt.Errorf(
					"%w: migrate the Ethernet definition in %s before staging the bond",
					errNetworkConfig,
					filename,
				)
			}

			content, err := encodeNetplan(doc)
			if err != nil {
				return nil, err
			}

			before := files[filename]
			changes[filename] = Edit{Before: &before, After: &content}
		}
	}

	body, err := s.renderNetplanBond()
	if err != nil {
		return nil, err
	}

	return changes, ownedNetworkFile(changes, files, managed, body, false)
}

func removeBondEthernetDefinitions(
	entries *yaml.Node,
	ids map[string]bool,
	filename string,
) (bool, error) {
	changed := false

	for definitionID := range ids {
		target := yamlField(entries, definitionID)
		if target == nil {
			continue
		}

		if err := removeBondEthernetOptions(target, filename, definitionID); err != nil {
			return false, err
		}

		removeYAMLField(entries, definitionID)
		changed = true
	}

	return changed, nil
}

func removeBondEthernetOptions(target *yaml.Node, filename, definitionID string) error {
	removeMainDefaultRoutes(target)

	for i := 0; i < len(target.Content); i += 2 {
		key := target.Content[i].Value
		if !slices.Contains(ownedNetplanOptions, key) {
			slog.Error(
				"unsupported Ethernet option",
				"key",
				key,
				"path",
				filename,
				"err",
				errNetworkConfig,
			)

			return fmt.Errorf(
				"%w: migrate %s in netplan Ethernet %s in %s before creating the bond",
				errNetworkConfig,
				key,
				definitionID,
				filename,
			)
		}
	}

	return nil
}

func netplanEthernetSources(
	files map[string]string,
	managed string,
	name string,
) (map[string]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(files[managed]), &doc); err != nil {
		slog.Error("parse managed netplan configuration", "path", managed, "err", err)

		return nil, fmt.Errorf("parse %s: %w", managed, err)
	}

	if len(doc.Content) == 0 ||
		yamlField(yamlField(yamlField(doc.Content[0], "network"), "bonds"), name) == nil {
		return files, nil
	}

	sources := maps.Clone(files)
	delete(sources, managed)

	return sources, nil
}

type netplanMember struct {
	Renderer  string            `yaml:"renderer"`
	Match     map[string]string `yaml:"match"`
	Name      string            `yaml:"set-name"`
	LinkLocal []string          `yaml:"link-local"`
	AcceptRA  bool              `yaml:"accept-ra"`
	MTU       int64             `yaml:"mtu,omitempty"`
}

type netplanBond struct {
	Renderer       string              `yaml:"renderer"`
	Members        []string            `yaml:"interfaces"`
	Parameters     map[string]string   `yaml:"parameters"`
	DHCP4          bool                `yaml:"dhcp4"`
	DHCP6          bool                `yaml:"dhcp6"`
	RA             bool                `yaml:"accept-ra"`
	Token          string              `yaml:"ipv6-address-token,omitempty"`
	Identifier     string              `yaml:"dhcp-identifier,omitempty"`
	DHCP4Overrides map[string]bool     `yaml:"dhcp4-overrides,omitempty"`
	MTU            int64               `yaml:"mtu,omitempty"`
	Addresses      []string            `yaml:"addresses,omitempty"`
	DNS            map[string][]string `yaml:"nameservers,omitempty"`
}

func (s Settings) renderNetplanBond() (string, error) {
	type layer struct {
		Version   int                      `yaml:"version"`
		Ethernets map[string]netplanMember `yaml:"ethernets"`
		Bonds     map[string]netplanBond   `yaml:"bonds"`
	}
	ethernets := map[string]netplanMember{}

	for _, member := range s.Bond.Members {
		ethernets[member.Name] = netplanMember{
			Renderer: string(Networkd),
			Name:     member.Name,
			Match:    map[string]string{"macaddress": member.MAC},
			MTU:      s.MTU,
		}
	}

	bond := netplanBond{
		Renderer:  string(Networkd),
		Members:   s.Bond.memberNames(),
		DHCP4:     s.DHCP4,
		RA:        s.SLAAC,
		MTU:       s.MTU,
		Addresses: s.Addresses,
		Parameters: map[string]string{
			"mode":                    "active-backup",
			"mii-monitor-interval":    fmt.Sprintf("%dms", s.Bond.MIIMon),
			"fail-over-mac-policy":    s.Bond.FailOverMAC,
			"primary":                 s.Bond.Primary,
			"primary-reselect-policy": s.Bond.PrimaryReselect,
		},
	}
	if s.SLAAC {
		token, err := slaacToken(s.MAC)
		if err != nil {
			return "", err
		}

		bond.Token = token
	}

	if s.DHCP4 {
		bond.Identifier = "duid"
		if s.MTU != 0 {
			bond.DHCP4Overrides = map[string]bool{"use-mtu": false}
		}
	}

	if len(s.DNS)+len(s.Search) > 0 {
		bond.DNS = map[string][]string{"addresses": s.DNS, "search": s.Search}
	}

	body, err := yaml.Marshal(map[string]layer{
		"network": {
			Version:   netplanVersion,
			Ethernets: ethernets,
			Bonds:     map[string]netplanBond{s.Name: bond},
		},
	})
	if err != nil {
		slog.Error("render netplan bond", "err", err)

		return "", fmt.Errorf("render netplan bond: %w", err)
	}

	return string(body), nil
}
