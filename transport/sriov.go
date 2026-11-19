package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// VFDeclaration assigns a MAC, driver, and optional name to a VF index.
type VFDeclaration struct {
	Index  int64  `json:"index"`
	MAC    string `json:"mac"`
	Driver string `json:"driver"`
	Ifname string `json:"ifname,omitempty"`
}

// SRIOVDeclaration specifies the enabled VF count and explicitly assigned VFs.
type SRIOVDeclaration struct {
	NumVFs int64           `json:"numvfs"`
	VFs    []VFDeclaration `json:"vfs"`
}

// VFState contains actual properties of an enabled PCI virtual function.
type VFState struct {
	VFDeclaration

	PCI         string `json:"pci"`
	InContainer bool   `json:"in_container"`
}

// SRIOVState reports the saved declaration and current hardware state.
type SRIOVState struct {
	NumVFs   int64             `json:"numvfs"`
	TotalVFs int64             `json:"totalvfs"`
	Desired  *SRIOVDeclaration `json:"desired"`
	VFs      []VFState         `json:"vfs"`
}

var errSRIOVInventory = errors.New("incompatible SR-IOV API response")

type runtimeVF struct {
	Index       *int64 `json:"index"`
	MAC         string `json:"mac"`
	Driver      string `json:"driver"`
	Ifname      string `json:"ifname"`
	PCI         string `json:"pci"`
	InContainer *flag  `json:"in_container"`
}

// UnmarshalJSON decodes the saved declaration and observed hardware.
//
// The API must identify each VF and distinguish container and host names.
// A missing namespace observation makes the response incompatible.
//
//	err := json.Unmarshal(document, &state)
func (state *SRIOVState) UnmarshalJSON(data []byte) error {
	var wire struct {
		NumVFs   *int64 `json:"numvfs"`
		TotalVFs *int64 `json:"totalvfs"`
		Desired  *struct {
			NumVFs *int64      `json:"numvfs"`
			VFs    []runtimeVF `json:"vfs"`
		} `json:"desired"`
		VFs []runtimeVF `json:"vfs"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("%w: %w", errSRIOVInventory, err)
	}

	if wire.NumVFs == nil || wire.TotalVFs == nil || wire.VFs == nil ||
		*wire.NumVFs > *wire.TotalVFs ||
		int64(len(wire.VFs)) != *wire.NumVFs {
		return fmt.Errorf("%w: invalid or missing numvfs, totalvfs, or vfs", errSRIOVInventory)
	}

	decoded := SRIOVState{
		NumVFs:   *wire.NumVFs,
		TotalVFs: *wire.TotalVFs,
		VFs:      make([]VFState, len(wire.VFs)),
	}
	seen := map[int64]bool{}

	for position, function := range wire.VFs {
		if function.Index == nil || *function.Index < 0 || *function.Index >= decoded.NumVFs ||
			seen[*function.Index] ||
			function.MAC == "" ||
			function.PCI == "" ||
			function.InContainer == nil {
			return fmt.Errorf(
				"%w: invalid index, MAC, PCI address, or in_container in runtime entry %d",
				errSRIOVInventory,
				position,
			)
		}

		seen[*function.Index] = true
		decoded.VFs[position] = VFState{
			Index:       *function.Index,
			MAC:         function.MAC,
			Driver:      function.Driver,
			Ifname:      function.Ifname,
			PCI:         function.PCI,
			InContainer: bool(*function.InContainer),
		}
	}

	if saved := wire.Desired; saved != nil {
		if saved.NumVFs == nil || saved.VFs == nil {
			return fmt.Errorf("%w: incomplete desired configuration", errSRIOVInventory)
		}

		decoded.Desired = &SRIOVDeclaration{
			NumVFs: *saved.NumVFs,
			VFs:    make([]VFDeclaration, len(saved.VFs)),
		}
		clear(seen)

		for position, function := range saved.VFs {
			if function.Index == nil || *function.Index < 0 || *function.Index >= *saved.NumVFs ||
				seen[*function.Index] ||
				function.MAC == "" ||
				function.Driver == "" {
				return fmt.Errorf("%w: incomplete desired VF %d", errSRIOVInventory, position)
			}

			seen[*function.Index] = true
			decoded.Desired.VFs[position] = VFDeclaration{
				Index:  *function.Index,
				MAC:    function.MAC,
				Driver: function.Driver,
				Ifname: function.Ifname,
			}
		}
	}

	*state = decoded

	return nil
}

// ReadSRIOV reads an adapter's enabled functions and saved declaration.
//
// The node reports PCI addresses and namespace ownership for each VF.
// Consumers can inspect the hardware without adopting it.
//
//	state, err := client.ReadSRIOV(ctx, node, "eno2")
func (client *Client) ReadSRIOV(ctx context.Context, node, name string) (SRIOVState, error) {
	return client.sriovState(ctx, node, name, http.MethodGet, nil)
}

// SetSRIOV applies a saved declaration to a physical function.
//
// The API reconciles assignments under guest ownership locks and returns
// the observed hardware after it saves the declaration.
//
//	observed, err := client.SetSRIOV(ctx, node, "eno2", declaration)
func (client *Client) SetSRIOV(
	ctx context.Context,
	node string,
	name string,
	desired SRIOVDeclaration,
) (SRIOVState, error) {
	body, err := json.Marshal(desired)
	if err != nil {
		slog.ErrorContext(ctx, "encode SR-IOV declaration", "err", err)

		return SRIOVState{}, fmt.Errorf("encode SR-IOV declaration: %w", err)
	}

	return client.sriovState(ctx, node, name, http.MethodPut, body)
}

func (client *Client) sriovState(
	ctx context.Context,
	node string,
	name string,
	method string,
	body []byte,
) (SRIOVState, error) {
	data, err := client.hostNetworkCall(ctx, node, "sriov", name, method, body)
	if err != nil {
		return SRIOVState{}, err
	}

	var state SRIOVState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.ErrorContext(ctx, "decode SR-IOV interface", "interface", name, "err", err)

		return SRIOVState{}, fmt.Errorf("decode SR-IOV interface %q: %w", name, err)
	}

	return state, nil
}

// DeleteSRIOV removes a managed declaration and its enabled VFs.
//
// The node checks guest ownership before disabling the physical
// function's VFs.
//
//	err := client.DeleteSRIOV(ctx, node, "eno2")
func (client *Client) DeleteSRIOV(ctx context.Context, node, name string) error {
	_, err := client.hostNetworkCall(ctx, node, "sriov", name, http.MethodDelete, nil)

	return err
}
