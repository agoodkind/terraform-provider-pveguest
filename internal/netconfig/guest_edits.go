package netconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
)

// GuestEdits compares the guest configuration with the requested settings.
//
// The read callback runs FileScript in the guest without changing its files.
//
// For example, a VM with two VFs can prefer the second port at startup.
// Its Netplan bond parameters change from primary: vf0 to this setting.
//
//	primary: vf1
//
// GuestEdits returns the YAML edit and the matching networkd edits.
// Comparing the files does not switch the active port.
func (s Settings) GuestEdits(
	ctx context.Context,
	backend Backend,
	removing bool,
	read func(args []string, input []byte) ([]byte, error),
) (map[string]Edit, error) {
	raw, err := read([]string{string(backend), "read", s.Name}, nil)
	if err != nil {
		return nil, err
	}

	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		slog.ErrorContext(ctx, "parse guest network configuration", "err", err)

		return nil, fmt.Errorf("read guest network files: %w", err)
	}

	changes, err := s.Plan(backend, files, removing)
	if err != nil {
		slog.ErrorContext(ctx, "plan guest network settings", "err", err)

		return nil, fmt.Errorf("plan %s: %w", backend, err)
	}

	if backend != Netplan {
		return changes, nil
	}

	body, err := json.Marshal(changes)
	if err != nil {
		slog.ErrorContext(ctx, "encode netplan sources", "err", err)

		return nil, fmt.Errorf("encode netplan sources: %w", err)
	}

	action := "render"
	if removing {
		action = "unrender"
	}

	args := []string{string(backend), action, s.Name, s.MAC}
	if s.Bond != nil {
		for _, member := range s.Bond.Members {
			args = append(args, member.Name, member.MAC)
		}
	}

	rendered, err := read(args, body)
	if err != nil {
		return nil, err
	}

	var overrides map[string]Edit
	if err := json.Unmarshal(rendered, &overrides); err != nil {
		slog.ErrorContext(ctx, "decode generated networkd units", "err", err)

		return nil, fmt.Errorf("decode generated networkd units: %w", err)
	}

	maps.Copy(changes, overrides)

	return changes, nil
}
