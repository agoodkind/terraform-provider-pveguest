package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

const hostUnitsPath = "/units/"

// HostUnitState is the response of GET /nodes/{node}/host-artifacts/{target}/units/{name}.
// UnitFileState and ActiveState are empty when Found is false.
type HostUnitState struct {
	Name          string
	Found         bool
	UnitFileState string
	ActiveState   string
}

type hostUnitData struct {
	Name          string `json:"name"`
	Found         flag   `json:"found"`
	UnitFileState string `json:"unit_file_state"`
	ActiveState   string `json:"active_state"`
}

type hostUnitActionBody struct {
	Action string `json:"action"`
}

// GetHostUnit returns the unit file state and the active state of a unit that
// the target policy lists. The request does not change the node.
func (client *Client) GetHostUnit(
	ctx context.Context,
	node string,
	target string,
	name string,
) (HostUnitState, error) {
	data, err := client.hostArtifactCall(
		ctx,
		node,
		http.MethodGet,
		target,
		hostUnitsPath+url.PathEscape(name),
		nil,
	)
	if err != nil {
		return HostUnitState{}, err
	}

	var parsed hostUnitData
	if err := json.Unmarshal(data, &parsed); err != nil {
		slog.ErrorContext(
			ctx,
			"host unit response is malformed",
			"node",
			node,
			"target",
			target,
			"unit",
			name,
			"err",
			err,
		)
		return HostUnitState{}, fmt.Errorf(
			"node %q target %s unit %s: parse unit response: %w",
			node,
			target,
			name,
			err,
		)
	}

	return HostUnitState{
		Name:          parsed.Name,
		Found:         bool(parsed.Found),
		UnitFileState: parsed.UnitFileState,
		ActiveState:   parsed.ActiveState,
	}, nil
}

// StartHostUnitAction starts a hostunit task that runs one action for a unit
// that the target policy lists and returns the UPID of the task.
func (client *Client) StartHostUnitAction(
	ctx context.Context,
	node string,
	target string,
	name string,
	action string,
) (string, error) {
	body := hostUnitActionBody{Action: action}
	document, err := encodeJSON(&body)
	if err != nil {
		return "", fmt.Errorf("node %q target %s unit %s: %w", node, target, name, err)
	}

	data, err := client.hostArtifactCall(
		ctx,
		node,
		http.MethodPost,
		target,
		hostUnitsPath+url.PathEscape(name),
		document,
	)
	if err != nil {
		return "", err
	}

	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		slog.ErrorContext(
			ctx,
			"host unit action response is malformed",
			"node",
			node,
			"target",
			target,
			"unit",
			name,
			"err",
			err,
		)
		return "", fmt.Errorf(
			"node %q target %s unit %s: parse task identifier: %w",
			node,
			target,
			name,
			err,
		)
	}

	return upid, nil
}
