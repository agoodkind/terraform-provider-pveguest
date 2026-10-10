package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// KernelModules contains a node's persistent kernel module list and the load
// state of each listed module.
type KernelModules struct {
	Modules []string
	Loaded  map[string]bool
}

type kernelModulesBody struct {
	Modules []string `json:"modules"`
}

type kernelModulesData struct {
	Modules []string        `json:"modules"`
	Loaded  map[string]flag `json:"loaded"`
}

// GetKernelModules returns the node's kernel module list and the load state
// of each listed module.
func (client *Client) GetKernelModules(ctx context.Context, node string) (KernelModules, error) {
	return client.kernelModulesCall(ctx, node, http.MethodGet, nil)
}

// SetKernelModules replaces the node's kernel module list.
// The method returns the module list and load states from the node response.
func (client *Client) SetKernelModules(
	ctx context.Context,
	node string,
	modules []string,
) (KernelModules, error) {
	body := kernelModulesBody{Modules: append([]string{}, modules...)}
	return client.kernelModulesCall(ctx, node, http.MethodPut, &body)
}

func (client *Client) kernelModulesCall(
	ctx context.Context,
	node string,
	method string,
	body *kernelModulesBody,
) (KernelModules, error) {
	host := Guest{Node: node}
	connection, found := client.nodes[node]
	if !found {
		return KernelModules{}, fmt.Errorf("node %q is not in the provider nodes map", node)
	}
	request := apiRequest{
		method: method,
		path:   "/api2/json/nodes/" + url.PathEscape(connection.nodeName) + "/kernel-modules",
	}
	if body != nil {
		document, err := encodeJSON(body)
		if err != nil {
			return KernelModules{}, fmt.Errorf("node %q: %w", node, err)
		}
		request.body = document
	}
	data, err := connection.call(ctx, host, request)
	if err != nil {
		return KernelModules{}, err
	}
	var parsed kernelModulesData
	if err := json.Unmarshal(data, &parsed); err != nil {
		slog.ErrorContext(ctx, "kernel-modules response is malformed", "node", node, "err", err)
		return KernelModules{}, fmt.Errorf("node %q: parse kernel-modules response: %w", node, err)
	}
	loaded := make(map[string]bool, len(parsed.Loaded))
	for name, state := range parsed.Loaded {
		loaded[name] = bool(state)
	}
	return KernelModules{Modules: parsed.Modules, Loaded: loaded}, nil
}
