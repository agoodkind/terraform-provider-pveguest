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
	data, err := client.containerCall(ctx, node, vmid, http.MethodGet, configOperation, nil)
	if err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		slog.ErrorContext(ctx, "container config response is malformed", "node", node, "vmid", vmid, "err", err)
		return nil, fmt.Errorf("node %q container %d: parse config response: %w", node, vmid, err)
	}
	options := make(map[string]string)
	for key, raw := range members {
		if containerOptionKey.MatchString(key) {
			options[key] = rawText(raw)
		}
	}
	return options, nil
}

// GetContainerPending returns the overlay options in the pending section of
// the container, with their pending values. A pending deletion has an empty
// value.
func (client *Client) GetContainerPending(
	ctx context.Context,
	node string,
	vmid int64,
) (map[string]string, error) {
	data, err := client.containerCall(ctx, node, vmid, http.MethodGet, pendingOperation, nil)
	if err != nil {
		return nil, err
	}
	var entries []pendingEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.ErrorContext(ctx, "container pending response is malformed", "node", node, "vmid", vmid, "err", err)
		return nil, fmt.Errorf("node %q container %d: parse pending response: %w", node, vmid, err)
	}
	pending := make(map[string]string)
	for _, entry := range entries {
		if !containerOptionKey.MatchString(entry.Key) {
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

// SetContainerOptions writes the options to the container config and deletes
// the keys in remove. The PUT request body is a JSON object that has one
// member per option and a comma separated delete member.
func (client *Client) SetContainerOptions(
	ctx context.Context,
	node string,
	vmid int64,
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
		slog.ErrorContext(ctx, "encode container config body failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("node %q container %d: %w", node, vmid, err)
	}
	_, err = client.containerCall(ctx, node, vmid, http.MethodPut, configOperation, document)
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
	connection, found := client.nodes[node]
	if !found {
		return nil, fmt.Errorf("node %q is not in the provider nodes map", node)
	}
	guest := Guest{Node: node, VMID: vmid, Kind: KindLXC}
	return connection.call(ctx, guest, apiRequest{
		method:    method,
		operation: operation,
		body:      body,
	})
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
