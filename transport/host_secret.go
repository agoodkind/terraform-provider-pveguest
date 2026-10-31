package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

const (
	hostSecretsPath = "/secrets/"

	redactedSecretValue = "[redacted]"
)

// HostSecretState contains one secrets[] entry from
// GET /nodes/{node}/host-artifacts/{target}. The entry does not contain the
// secret value. Version is 0 when the node does not record the field.
// SHA256 is empty when the storage file is not a regular file.
type HostSecretState struct {
	Field      string
	Configured bool
	Version    int64
	SHA256     string
}

type hostSecretBody struct {
	Value   string `json:"value"`
	Version int64  `json:"version"`
}

type hostSecretData struct {
	Field      string      `json:"field"`
	Configured flag        `json:"configured"`
	Version    json.Number `json:"version"`
	SHA256     string      `json:"sha256"`
}

// SetHostSecret sends value and version in the request body to
// PUT /nodes/{node}/host-artifacts/{target}/secrets/{field}.
// It returns the UPID of the hostsecret task.
func (client *Client) SetHostSecret(
	ctx context.Context,
	node string,
	target string,
	field string,
	value string,
	version int64,
) (string, error) {
	body := hostSecretBody{Value: value, Version: version}
	document, err := encodeJSON(&body)
	if err != nil {
		return "", redactHostSecret(fmt.Errorf("node %q target %s field %s: %w", node, target, field, err), value)
	}
	suffix := hostSecretsPath + url.PathEscape(field)
	data, err := client.hostArtifactCall(ctx, node, http.MethodPut, target, suffix, document)
	if err != nil {
		return "", redactHostSecret(err, value)
	}
	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		slog.ErrorContext(ctx, "host secret response is malformed", "node", node, "target", target, "field", field, "err", err)
		return "", fmt.Errorf("node %q target %s field %s: parse task identifier: %w", node, target, field, err)
	}
	return upid, nil
}

// redactHostSecret replaces value with [redacted] in the text of err.
func redactHostSecret(err error, value string) error {
	if err == nil || value == "" || !strings.Contains(err.Error(), value) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), value, redactedSecretValue))
}

func (entry hostSecretData) convert() (HostSecretState, error) {
	numbers, err := optionalIntegers(entry.Version)
	if err != nil {
		return HostSecretState{}, err
	}
	return HostSecretState{
		Field:      entry.Field,
		Configured: bool(entry.Configured),
		Version:    numbers[0],
		SHA256:     entry.SHA256,
	}, nil
}
