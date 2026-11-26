package netconfig

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"

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
	settings Settings,
) (map[string]*yaml.Node, string, error) {
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

			return nil, "", fmt.Errorf("parse %s: %w", filename, err)
		}

		if len(doc.Content) == 0 {
			continue
		}

		docs[filename] = &doc
		entries := yamlField(yamlField(doc.Content[0], "network"), "ethernets")
		if entries == nil {
			continue
		}

		if entries.Kind != yaml.MappingNode {
			slog.Error("unsupported netplan source", "path", filename, "err", errNetworkConfig)

			return nil, "", fmt.Errorf(
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

	id, err := selectNetplanID(selectors, settings)

	return docs, id, err
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
	if s.Bond != nil {
		return s.netplanBondEdits(files, removing)
	}

	managed := path.Join("/etc/netplan", "90-pveguest-"+s.Name+".yaml")
	sources, err := netplanEthernetSources(files, managed, s.Name)
	if err != nil {
		return nil, err
	}

	docs, definitionID, err := netplanDocuments(sources, s)
	if err != nil {
		return nil, err
	}

	updates := map[string]Edit{}
	// Keep unrelated route and link options in their original definition. Both
	// source files use the same ID, which netplan merges before generating units.
	for filename, doc := range docs {
		if filename == managed || removing {
			continue
		}

		entries := yamlField(yamlField(doc.Content[0], "network"), "ethernets")
		target := yamlField(entries, definitionID)
		if target == nil {
			continue
		}

		changed := false

		for _, key := range ownedNetplanOptions {
			if removeYAMLField(target, key) {
				changed = true
			}
		}

		if removeMainDefaultRoutes(target) {
			changed = true
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

		if len(target.Content) == 0 {
			removeYAMLField(entries, definitionID)
		}

		content, err := encodeNetplan(doc)
		if err != nil {
			return nil, err
		}

		before := files[filename]
		updates[filename] = Edit{Before: &before, After: &content}
	}

	body, err := s.renderNetplan(definitionID)
	if err != nil {
		return nil, err
	}

	if err := ownedNetworkFile(updates, files, managed, body, removing); err != nil {
		return nil, err
	}

	return updates, nil
}

func (s Settings) renderNetplan(definitionID string) (string, error) {
	type match struct {
		MAC string `yaml:"macaddress"`
	}
	type dns struct {
		Addresses []string `yaml:"addresses,omitempty"`
		Search    []string `yaml:"search,omitempty"`
	}
	type ethernet struct {
		DHCP4Overrides map[string]bool `yaml:"dhcp4-overrides,omitempty"`
		Renderer       string          `yaml:"renderer"`
		Match          match           `yaml:"match"`
		Name           string          `yaml:"set-name"`
		DHCP4          bool            `yaml:"dhcp4"`
		DHCP6          bool            `yaml:"dhcp6"`
		RA             bool            `yaml:"accept-ra"`
		Addresses      []string        `yaml:"addresses,omitempty"`
		MTU            int64           `yaml:"mtu,omitempty"`
		DNS            dns             `yaml:"nameservers,omitempty"`
	}
	configuration := ethernet{
		Renderer:  "networkd",
		Match:     match{MAC: s.MAC},
		Name:      s.Name,
		DHCP4:     s.DHCP4,
		RA:        s.SLAAC,
		Addresses: s.Addresses,
		MTU:       s.MTU,
		DNS:       dns{Addresses: s.DNS, Search: s.Search},
	}
	if s.MTU != 0 && s.DHCP4 {
		configuration.DHCP4Overrides = map[string]bool{"use-mtu": false}
	}

	type layer struct {
		Version   int                 `yaml:"version"`
		Ethernets map[string]ethernet `yaml:"ethernets"`
	}
	content, err := yaml.Marshal(map[string]layer{
		"network": {
			Version:   netplanVersion,
			Ethernets: map[string]ethernet{definitionID: configuration},
		},
	})
	if err != nil {
		slog.Error("render netplan", "err", err)

		return "", fmt.Errorf("render netplan: %w", err)
	}

	return string(content), nil
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
