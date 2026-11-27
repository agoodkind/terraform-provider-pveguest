package netconfig

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"

	netplan "github.com/moznion/go-netplan-types/v2"
	value "github.com/moznion/go-yaml-nillable"
	"go.yaml.in/yaml/v3"
)

const (
	yamlMappingPair = 2
	netplanIndent   = 2
	netplanVersion  = 2
)

var ownedNetplanOptions = []string{
	"addresses",
	"gateway4",
	"gateway6",
	"dhcp4",
	"dhcp6",
	"accept-ra",
	"match",
	"set-name",
	"mtu",
	"nameservers",
	"renderer",
}

func yamlField(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}

	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}

	return nil
}

func removeYAMLField(mapping *yaml.Node, key string) bool {
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content = slices.Delete(mapping.Content, index, index+yamlMappingPair)

			return true
		}
	}

	return false
}

func yamlString(mapping *yaml.Node, key string) string {
	value := yamlField(mapping, key)
	if value == nil {
		return ""
	}

	return value.Value
}

// Netplan merges definitions with the same ID in filename order. With an
// explicit match, the ID is an opaque label rather than an interface name.
type netplanMatch struct {
	complex           bool
	explicit          bool
	mac, name, rename string
}

func (m *netplanMatch) merge(def *yaml.Node) {
	if match := yamlField(def, "match"); match != nil {
		m.explicit = true

		for index := 0; index < len(match.Content); index += yamlMappingPair {
			key := match.Content[index].Value
			if key != "macaddress" && key != "name" {
				m.complex = true
			}
		}

		if v := yamlField(match, "macaddress"); v != nil {
			m.mac = v.Value
		}

		if v := yamlField(match, "name"); v != nil {
			m.name = v.Value
		}
	}

	if rename := yamlField(def, "set-name"); rename != nil {
		m.rename = rename.Value
	}
}

func (m *netplanMatch) selects(definitionID string, settings Settings) bool {
	if m.mac != "" {
		return strings.EqualFold(m.mac, settings.MAC)
	}

	if m.rename != "" {
		return m.rename == settings.Name
	}

	if m.explicit {
		return m.name == settings.Name
	}

	return definitionID == settings.Name
}

func netplanDocuments(
	files map[string]string,
	managed string,
	name string,
) (map[string]*yaml.Node, map[string]netplanMatch, error) {
	docs := map[string]*yaml.Node{}
	selectors := map[string]netplanMatch{}
	// scan already resolves equal basenames according to netplan directory precedence.
	filenames := slices.Sorted(maps.Keys(files))
	slices.SortFunc(
		filenames,
		func(a, b string) int { return strings.Compare(path.Base(a), path.Base(b)) },
	)

	for _, filename := range filenames {
		content := files[filename]
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
			slog.Error("parse netplan YAML", "path", filename, "err", err)

			return nil, nil, fmt.Errorf("parse %s: %w", filename, err)
		}

		if len(doc.Content) == 0 {
			continue
		}

		docs[filename] = &doc
		network := yamlField(doc.Content[0], "network")
		if filename == managed && yamlField(yamlField(network, "bonds"), name) != nil {
			continue
		}

		entries := yamlField(network, "ethernets")
		if entries == nil {
			continue
		}

		if entries.Kind != yaml.MappingNode {
			slog.Error("unsupported netplan source", "path", filename, "err", errNetworkConfig)

			return nil, nil, fmt.Errorf(
				"%w: %s: ethernets must be a mapping",
				errNetworkConfig,
				filename,
			)
		}

		for index := 0; index < len(entries.Content); index += 2 {
			definitionID := entries.Content[index].Value
			m := selectors[definitionID]
			m.merge(entries.Content[index+1])
			selectors[definitionID] = m
		}
	}

	return docs, selectors, nil
}

func selectNetplanID(selectors map[string]netplanMatch, settings Settings) (string, error) {
	ids := []string{}

	for definitionID, selector := range selectors {
		if selector.mac != "" && !strings.EqualFold(selector.mac, settings.MAC) &&
			selector.rename == settings.Name {
			return "", fmt.Errorf(
				"%w: netplan assigns %s to another MAC in %s",
				errNetworkConfig,
				settings.Name,
				definitionID,
			)
		}

		if !selector.selects(definitionID, settings) {
			continue
		}

		if selector.complex || strings.ContainsAny(selector.name, "*?[") ||
			selector.name != "" && selector.name != settings.Name && selector.mac != "" {
			return "", fmt.Errorf(
				"%w: cannot verify all match conditions for netplan definition %s before adoption",
				errNetworkConfig,
				definitionID,
			)
		}

		ids = append(ids, definitionID)
	}

	if len(ids) > 1 {
		slog.Error("ambiguous netplan interface", "name", settings.Name, "err", errNetworkConfig)

		return "", fmt.Errorf(
			"%w: several netplan Ethernet definitions match %s or %s",
			errNetworkConfig,
			settings.Name,
			settings.MAC,
		)
	}

	if len(ids) == 1 {
		return ids[0], nil
	}

	if _, exists := selectors[settings.Name]; exists {
		return "", fmt.Errorf(
			"%w: netplan ID %s already selects another interface; choose a different ID for that definition",
			errNetworkConfig,
			settings.Name,
		)
	}

	return settings.Name, nil
}

func encodeNetplan(value *yaml.Node) (string, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(netplanIndent)
	if err := encoder.Encode(value); err != nil {
		slog.Error("encode netplan YAML", "err", err)

		return "", fmt.Errorf("encode netplan: %w", err)
	}

	return out.String(), encoder.Close()
}

func (s Settings) netplanEdits(
	files map[string]string,
	removing bool,
) (map[string]Edit, error) {
	managed := path.Join("/etc/netplan", "90-pveguest-"+s.Name+".yaml")
	updates := map[string]Edit{}
	if removing && s.Bond != nil {
		return updates, ownedNetworkFile(updates, files, managed, "", true)
	}

	docs, selectors, err := netplanDocuments(files, managed, s.Name)
	if err != nil {
		return nil, err
	}

	definitionID, err := selectNetplanID(selectors, s)
	if err != nil {
		return nil, err
	}

	ids := map[string]bool{definitionID: true}
	if s.Bond != nil {
		for _, member := range s.Bond.Members {
			id, err := selectNetplanID(selectors, Settings{Name: member.Name, MAC: member.MAC})
			if err != nil {
				return nil, err
			}

			ids[id] = true
		}
	}

	for filename, doc := range docs {
		if filename == managed || removing {
			continue
		}

		changed, err := s.adoptNetplan(yamlField(doc.Content[0], "network"), ids, filename)
		if err != nil {
			return nil, err
		}

		if !changed {
			continue
		}

		if !strings.HasPrefix(filename, "/etc/netplan/") {
			slog.Error("unsupported netplan source", "path", filename, "err", errNetworkConfig)

			return nil, fmt.Errorf(
				"%w: %s configures %s outside /etc/netplan; migrate that definition first",
				errNetworkConfig,
				filename,
				definitionID,
			)
		}

		content, err := encodeNetplan(doc)
		if err != nil {
			return nil, err
		}

		before := files[filename]
		updates[filename] = Edit{Before: &before, After: &content}
	}

	desired, err := s.renderNetplan(definitionID)
	if err != nil {
		return nil, err
	}

	owned := strings.HasPrefix(files[managed], networkOwnerHeader)
	if existing := docs[managed]; owned && existing != nil && !removing &&
		sameNetplan(desired, existing.Content[0]) {
		return updates, nil
	}

	body, err := encodeNetplan(desired)
	if err != nil {
		return nil, err
	}

	if err := ownedNetworkFile(updates, files, managed, body, removing); err != nil {
		return nil, err
	}

	return updates, nil
}

// Compare YAML tags, values and sequence order. A mapping's key order and
// whitespace do not change the guest configuration.
func sameNetplan(desired, existing *yaml.Node) bool {
	if existing == nil || desired.Kind != existing.Kind || desired.Value != existing.Value {
		return false
	}

	if desired.ShortTag() != existing.ShortTag() || len(desired.Content) != len(existing.Content) {
		return false
	}

	if desired.Kind == yaml.MappingNode {
		for i := 0; i < len(desired.Content); i += yamlMappingPair {
			if !sameNetplan(desired.Content[i+1], yamlField(existing, desired.Content[i].Value)) {
				return false
			}
		}

		return true
	}

	for i, child := range desired.Content {
		if !sameNetplan(child, existing.Content[i]) {
			return false
		}
	}

	return true
}

func (s Settings) renderNetplan(definitionID string) (*yaml.Node, error) {
	device := netplan.Device{
		Renderer: netplan.NetworkdRenderer(),
		DHCP4:    value.BoolOf(s.DHCP4),
		DHCP6:    value.BoolOf(false),
		AcceptRA: value.BoolOf(s.SLAAC),
	}
	for _, address := range s.Addresses {
		device.Addresses = append(device.Addresses, &netplan.Address{Address: address})
	}

	if s.MTU > 0 {
		device.MTU = value.Uint64Of(uint64(s.MTU))
		if s.DHCP4 {
			device.DHCP4Overrides = &netplan.DHCPOverride{UseMTU: value.BoolOf(false)}
		}
	}

	if len(s.DNS)+len(s.Search) > 0 {
		device.NameServers = &netplan.Nameservers{Addresses: s.DNS, Search: s.Search}
	}

	network := netplan.NetworkConfig{VersionMustBe2: netplanVersion, Ethernets: netplan.Ethernets{}}
	if s.Bond == nil {
		network.Ethernets[definitionID] = netplanEthernet(device, s.Name, s.MAC)
	} else {
		memberDevice := netplan.Device{
			Renderer: device.Renderer,
			MTU:      device.MTU,
			AcceptRA: value.BoolOf(false),
		}

		for _, member := range s.Bond.Members {
			network.Ethernets[member.Name] = netplanEthernet(memberDevice, member.Name, member.MAC)
		}

		if s.DHCP4 {
			device.DHCPIdentifier = value.StringOf("duid")
		}

		network.Bonds = netplan.Bonds{
			s.Name: {
				Device:     device,
				Interfaces: s.Bond.memberNames(),
				Parameters: &netplan.BondParameters{
					Mode:              netplan.ActiveBackupBondMode(),
					FailOverMACPolicy: netplan.ActiveFailOverMACPolicy(),
					Primary:           value.StringOf(s.Bond.Primary),
					PrimaryReselectPolicy: map[string]*netplan.PrimaryReselectPolicy{
						"always":  netplan.AlwaysPrimaryReselectPolicy(),
						"better":  netplan.BetterPrimaryReselectPolicy(),
						"failure": netplan.FailurePrimaryReselectPolicy(),
					}[s.Bond.PrimaryReselect],
				},
			},
		}
	}

	var doc yaml.Node
	if err := doc.Encode(map[string]netplan.NetworkConfig{"network": network}); err != nil {
		slog.Error("render netplan configuration", "err", err)

		return nil, fmt.Errorf("encode netplan configuration: %w", err)
	}

	if s.Bond != nil {
		if err := s.netplanBondFields(yamlField(&doc, "network")); err != nil {
			return nil, err
		}
	}

	return &doc, nil
}

func netplanEthernet(device netplan.Device, name, mac string) *netplan.Ethernet {
	ethernet := &netplan.Ethernet{Device: device}
	ethernet.Match = &netplan.Match{MacAddress: value.StringOf(mac)}
	ethernet.SetName = value.StringOf(name)

	return ethernet
}

// Netplan requires an explicit empty link-local list on each member. The Go
// types omit empty lists and do not support a token or a duration with units.
func (s Settings) netplanBondFields(network *yaml.Node) error {
	empty := yaml.Node{Kind: yaml.SequenceNode}
	ethernets := yamlField(network, "ethernets")

	for _, member := range s.Bond.Members {
		appendNetplanField(yamlField(ethernets, member.Name), "link-local", empty)
	}

	bond := yamlField(yamlField(network, "bonds"), s.Name)
	appendNetplanField(
		yamlField(bond, "parameters"),
		"mii-monitor-interval",
		yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: fmt.Sprintf("%dms", s.Bond.MIIMon),
		},
	)
	if dns := yamlField(bond, "nameservers"); dns != nil {
		for _, key := range []string{"addresses", "search"} {
			if yamlField(dns, key) == nil {
				appendNetplanField(dns, key, empty)
			}
		}
	}

	if s.SLAAC {
		token, err := slaacToken(s.MAC)
		if err != nil {
			return err
		}

		appendNetplanField(
			bond,
			"ipv6-address-token",
			yaml.Node{Kind: yaml.ScalarNode, Value: token},
		)
	}

	return nil
}

func appendNetplanField(mapping *yaml.Node, key string, field yaml.Node) {
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &field)
}

func removeMainDefaultRoutes(target *yaml.Node) bool {
	changed := false
	routes := yamlField(target, "routes")
	if routes != nil {
		kept := slices.DeleteFunc(
			routes.Content,
			func(route *yaml.Node) bool {
				dest := yamlString(route, "to")
				table := yamlString(route, "table")

				return (table == "" || table == "254") &&
					(dest == "default" || dest == "::/0" || dest == "0.0.0.0/0")
			},
		)
		if len(kept) != len(routes.Content) {
			routes.Content = kept
			changed = true
		}

		if len(kept) == 0 {
			removeYAMLField(target, "routes")
		}
	}

	return changed
}
