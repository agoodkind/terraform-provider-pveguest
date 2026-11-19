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

var errEthernet = errors.New("invalid Ethernet interface")

// EthernetSettings declares the staged configuration of a physical port.
type EthernetSettings struct {
	CIDR      string `json:"cidr,omitempty"`
	CIDR6     string `json:"cidr6,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Gateway6  string `json:"gateway6,omitempty"`
	MTU       *int64 `json:"mtu,omitempty"`
	Comments  string `json:"comments,omitempty"`
	Autostart bool   `json:"autostart"`
}

// EthernetPort reports whether a physical port has an iface stanza.
type EthernetPort struct {
	Settings     EthernetSettings
	Configured   bool
	ipv4Options  bool
	ipv6Options  bool
	otherOptions bool
}

type ethernetUpdate struct {
	EthernetSettings

	Type   string `json:"type"`
	Delete string `json:"delete,omitempty"`
}

// ReadEthernet reads the physical port and its staged configuration.
//
// Proxmox returns an error for a missing interface. A configured stanza
// can be imported before its first managed update.
//
//	port, err := client.ReadEthernet(ctx, node, "eno1")
func (client *Client) ReadEthernet(ctx context.Context, node, name string) (EthernetPort, error) {
	data, err := client.hostNetworkCall(ctx, node, "network", name, http.MethodGet, nil)
	if err != nil {
		return EthernetPort{}, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return EthernetPort{}, fmt.Errorf("decode Ethernet properties: %w", err)
	}

	var port struct {
		Options    []string    `json:"options"`
		Options6   []string    `json:"options6"`
		Broadcast  string      `json:"broadcast"`
		Broadcast6 string      `json:"broadcast6"`
		CIDR       string      `json:"cidr"`
		CIDR6      string      `json:"cidr6"`
		Gateway    string      `json:"gateway"`
		Gateway6   string      `json:"gateway6"`
		MTU        json.Number `json:"mtu"`
		Comments   string      `json:"comments"`
		Autostart  flag        `json:"autostart"`
		Type       string      `json:"type"`
		Exists     flag        `json:"exists"`
		Priority   int         `json:"priority"`
	}
	if err := json.Unmarshal(data, &port); err != nil {
		slog.ErrorContext(ctx, "decode staged Ethernet", "interface", name, "err", err)

		return EthernetPort{}, fmt.Errorf("decode Ethernet interface %q: %w", name, err)
	}

	if port.Type != "eth" {
		return EthernetPort{}, fmt.Errorf(
			"%w: interface %q has type %q; expected eth",
			errEthernet,
			name,
			port.Type,
		)
	}

	if !port.Exists {
		return EthernetPort{}, fmt.Errorf(
			"%w: physical Ethernet interface %q does not exist on node %q",
			errEthernet,
			name,
			node,
		)
	}

	settings := EthernetSettings{
		CIDR:      port.CIDR,
		CIDR6:     port.CIDR6,
		Gateway:   port.Gateway,
		Gateway6:  port.Gateway6,
		Comments:  strings.TrimSuffix(port.Comments, "\n"),
		Autostart: bool(port.Autostart),
	}
	if port.MTU != "" {
		mtu, err := port.MTU.Int64()
		if err != nil {
			slog.ErrorContext(ctx, "decode Ethernet MTU", "interface", name, "err", err)

			return EthernetPort{}, fmt.Errorf("parse MTU of %q: %w", name, err)
		}

		settings.MTU = &mtu
	}

	// The point GET also returns kernel metadata and derived addresses.
	for key := range strings.FieldsSeq(
		"active altnames type link-type exists priority families method method6 cidr cidr6 address address6 " +
			"netmask netmask6 options options6 broadcast broadcast6 gateway gateway6 mtu comments comments6 autostart",
	) {
		delete(fields, key)
	}

	return EthernetPort{
		ipv4Options:  len(port.Options) > 0 || port.Broadcast != "",
		ipv6Options:  len(port.Options6) > 0 || port.Broadcast6 != "",
		otherOptions: len(fields) > 0,
		Settings:     settings,
		Configured:   port.Priority > 0 || settings.Autostart,
	}, nil
}

// StageEthernet writes the pending configuration for a physical port.
//
// The network applier activates these settings separately. Unset values
// clear the corresponding optional fields in the Proxmox API.
//
//	err := client.StageEthernet(ctx, node, "eno1", settings)
func (client *Client) StageEthernet(
	ctx context.Context,
	node string,
	name string,
	settings EthernetSettings,
) error {
	current, err := client.ReadEthernet(ctx, node, name)
	if err != nil {
		return err
	}

	return client.stagePhysicalPort(ctx, node, name, settings, current)
}

func (client *Client) stagePhysicalPort(
	ctx context.Context,
	node string,
	name string,
	settings EthernetSettings,
	current EthernetPort,
) error {
	// The update API omits all options in any address family it removes.
	if current.ipv6Options && settings.CIDR6 == "" ||
		current.ipv4Options && settings.CIDR == "" && settings.CIDR6 != "" {
		return fmt.Errorf(
			"%w: move unrelated options out of the address family being removed from %q",
			errEthernet,
			name,
		)
	}

	remove := []string{}
	if settings.CIDR == "" {
		remove = append(remove, "cidr")
	}

	if settings.CIDR6 == "" {
		remove = append(remove, "cidr6")
	}

	if settings.Gateway == "" {
		remove = append(remove, "gateway")
	}

	if settings.Gateway6 == "" {
		remove = append(remove, "gateway6")
	}

	if settings.MTU == nil {
		remove = append(remove, "mtu")
	}

	if settings.Comments == "" {
		remove = append(remove, "comments")
	}

	body, err := json.Marshal(ethernetUpdate{
		EthernetSettings: settings,
		Type:             "eth",
		Delete:           strings.Join(remove, ","),
	})
	if err != nil {
		slog.ErrorContext(ctx, "encode staged Ethernet", "interface", name, "err", err)

		return fmt.Errorf("encode Ethernet configuration: %w", err)
	}

	_, err = client.hostNetworkCall(ctx, node, "network", name, http.MethodPut, body)

	return err
}

// DeleteEthernet stages removal of the interface stanza.
//
// The physical port remains available for a new configuration after the
// node network applier reloads networking.
//
//	err := client.DeleteEthernet(ctx, node, "eno1")
func (client *Client) DeleteEthernet(ctx context.Context, node, name string) error {
	current, err := client.ReadEthernet(ctx, node, name)
	if err != nil {
		return err
	}

	if current.ipv4Options || current.ipv6Options || current.otherOptions {
		return client.stagePhysicalPort(ctx, node, name, EthernetSettings{}, current)
	}

	_, err = client.hostNetworkCall(ctx, node, "network", name, http.MethodDelete, nil)

	return err
}

func (client *Client) hostNetworkCall(
	ctx context.Context,
	node string,
	group string,
	name string,
	method string,
	body []byte,
) (json.RawMessage, error) {
	connection, found := client.nodes[node]
	if !found {
		slog.ErrorContext(
			ctx,
			"cannot use Ethernet interface",
			"node",
			node,
			"interface",
			name,
			"err",
			errEthernet,
		)

		return nil, fmt.Errorf("%w: node %q is not in the provider nodes map", errEthernet, node)
	}

	request := apiRequest{
		method: method,
		path: "/api2/json/nodes/" + url.PathEscape(
			connection.nodeName,
		) + "/" + group + "/" + url.PathEscape(name),
		body: body,
	}

	return connection.call(ctx, Guest{Node: node}, request)
}
