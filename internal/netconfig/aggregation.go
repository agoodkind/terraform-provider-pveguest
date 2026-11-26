package netconfig

import (
	"fmt"
	"log/slog"

	"go.yaml.in/yaml/v3"
)

func (s Settings) adoptNetplan(
	network *yaml.Node,
	ids map[string]bool,
	filename string,
) (bool, error) {
	entries := yamlField(network, "ethernets")
	if s.Bond != nil {
		if yamlField(yamlField(network, "bonds"), s.Name) != nil {
			slog.Error("existing netplan bond", "path", filename, "err", errNetworkConfig)

			return false, fmt.Errorf(
				"%w: an existing netplan bond in %s already uses %s",
				errNetworkConfig,
				filename,
				s.Name,
			)
		}

		for _, name := range append(s.Bond.memberNames(), s.Name) {
			if yamlField(entries, name) != nil && !ids[name] {
				return false, fmt.Errorf(
					"%w: netplan ID %s in %s selects another Ethernet interface",
					errNetworkConfig,
					name,
					filename,
				)
			}
		}
	}

	changed := false

	for definitionID := range ids {
		target := yamlField(entries, definitionID)
		if target == nil {
			continue
		}

		if removeMainDefaultRoutes(target) {
			changed = true
		}

		for _, key := range ownedNetplanOptions {
			if removeYAMLField(target, key) {
				changed = true
			}
		}

		// A single Ethernet retains its ID and unowned options. A bond replaces
		// the Ethernet definition and cannot preserve those options on a member.
		if len(target.Content) == 0 && (changed || s.Bond != nil) {
			removeYAMLField(entries, definitionID)
			changed = true
		} else if s.Bond != nil {
			return false, fmt.Errorf(
				"%w: migrate %s in netplan Ethernet %s in %s before creating the bond",
				errNetworkConfig,
				target.Content[0].Value,
				definitionID,
				filename,
			)
		}
	}

	return changed, nil
}
