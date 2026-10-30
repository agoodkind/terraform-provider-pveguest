package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"strings"
)

// ContainerOptionKeyPattern matches the container options that the overlay
// adds to Proxmox: bpfdelegate and hostnic0 to hostnic9.
const ContainerOptionKeyPattern = `^(bpfdelegate|hostnic[0-9])$`

// VMOptionKeyPattern matches the VM option that the overlay adds to
// Proxmox: vsock.
const VMOptionKeyPattern = `^vsock$`

const (
	configOperation  = "config"
	pendingOperation = "pending"
	deleteMember     = "delete"
	deleteListSep    = ","
)

var containerOptionKey = regexp.MustCompile(ContainerOptionKeyPattern)

type containerConfigBody map[string]string

type pendingEntry struct {
	Key     string          `json:"key"`
	Pending json.RawMessage `json:"pending"`
	Delete  json.RawMessage `json:"delete"`
}

// GetContainerOptions returns the overlay options in the container config.
// Proxmox merges pending changes into this view of a running container.
func (client *Client) GetContainerOptions(
	ctx context.Context,
	node string,
	vmid int64,
) (map[string]string, error) {
	return client.GetGuestOptions(ctx, containerGuest(node, vmid), containerOptionKey)
}

// GetContainerPending returns the overlay options in the pending section of
// the container, with their pending values. A pending deletion has an empty
// value.
func (client *Client) GetContainerPending(
	ctx context.Context,
	node string,
	vmid int64,
) (map[string]string, error) {
	return client.GetGuestPending(ctx, containerGuest(node, vmid), containerOptionKey)
}

// SetContainerOptions writes options to the container config and deletes the
// keys in remove.
func (client *Client) SetContainerOptions(
	ctx context.Context,
	node string,
	vmid int64,
	options map[string]string,
	remove []string,
) error {
	return client.SetGuestOptions(ctx, containerGuest(node, vmid), options, remove)
}

// GetGuestOptions reads the guest config and returns keys that match the
// pattern. Proxmox merges pending changes into the config of a running guest.
func (client *Client) GetGuestOptions(
	ctx context.Context,
	guest Guest,
	keys *regexp.Regexp,
) (map[string]string, error) {
	data, err := client.guestCall(ctx, guest, http.MethodGet, configOperation, nil)
	if err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		slog.ErrorContext(ctx, "config response contains invalid json",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err)
		return nil, fmt.Errorf("%s: parse config response: %w", guest, err)
	}
	options := make(map[string]string)
	for key, raw := range members {
		if keys.MatchString(key) {
			options[key] = rawText(raw)
		}
	}
	return options, nil
}

// GetGuestPending reads pending guest options and returns keys that match
// the pattern with their pending values. Pending deletions have an empty value.
func (client *Client) GetGuestPending(
	ctx context.Context,
	guest Guest,
	keys *regexp.Regexp,
) (map[string]string, error) {
	data, err := client.guestCall(ctx, guest, http.MethodGet, pendingOperation, nil)
	if err != nil {
		return nil, err
	}
	var entries []pendingEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.ErrorContext(ctx, "pending response contains invalid json",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err)
		return nil, fmt.Errorf("%s: parse pending response: %w", guest, err)
	}
	pending := make(map[string]string)
	for _, entry := range entries {
		if !keys.MatchString(entry.Key) {
			continue
		}
		if len(entry.Pending) > 0 {
			pending[entry.Key] = rawText(entry.Pending)
		} else if len(entry.Delete) > 0 {
			pending[entry.Key] = ""
		}
	}
	return pending, nil
}

// SetGuestOptions sends one synchronous request to write options and delete
// keys from the guest config. It sends a request only when options or keys
// to remove are present.
func (client *Client) SetGuestOptions(
	ctx context.Context,
	guest Guest,
	options map[string]string,
	remove []string,
) error {
	body := make(containerConfigBody, len(options)+1)
	maps.Copy(body, options)
	if len(remove) > 0 {
		body[deleteMember] = strings.Join(remove, deleteListSep)
	}
	if len(body) == 0 {
		return nil
	}
	document, err := encodeJSON(&body)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode config request",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err)
		return fmt.Errorf("%s: %w", guest, err)
	}
	_, err = client.guestCall(ctx, guest, http.MethodPut, configOperation, document)
	return err
}

func (client *Client) containerCall(
	ctx context.Context,
	node string,
	vmid int64,
	method string,
	operation string,
	body []byte,
) (json.RawMessage, error) {
	return client.guestCall(ctx, containerGuest(node, vmid), method, operation, body)
}

func (client *Client) guestCall(
	ctx context.Context,
	guest Guest,
	method string,
	operation string,
	body []byte,
) (json.RawMessage, error) {
	connection, found := client.nodes[guest.Node]
	if !found {
		return nil, fmt.Errorf("node %q is not in the provider nodes map", guest.Node)
	}
	return connection.call(ctx, guest, apiRequest{
		method:    method,
		operation: operation,
		body:      body,
	})
}

func containerGuest(node string, vmid int64) Guest {
	return Guest{Node: node, VMID: vmid, Kind: KindLXC}
}

// rawText returns the text of a JSON string and the source text of any other
// JSON value.
func rawText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}
