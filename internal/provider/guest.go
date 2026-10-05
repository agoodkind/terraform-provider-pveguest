package provider

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const maximumErrorOutputBytes = 4000

// A changed node, vmid, or kind selects another guest. Each of the three
// attributes forces replacement of the resource.
func guestAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"node": schema.StringAttribute{
			Required:      true,
			Description:   "Key of the hypervisor in the provider nodes map.",
			PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
		},
		"vmid": schema.Int64Attribute{
			Required:      true,
			Description:   "Proxmox ID of the guest.",
			Validators:    []validator.Int64{int64validator.AtLeast(1)},
			PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
		},
		"kind": schema.StringAttribute{
			Required:    true,
			Description: "Guest type: lxc for a container, qemu for a VM.",
			Validators: []validator.String{
				stringvalidator.OneOf(string(transport.KindLXC), string(transport.KindQEMU)),
			},
			PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
		},
	}
}

func withGuestAttributes(attributes map[string]schema.Attribute) map[string]schema.Attribute {
	maps.Copy(attributes, guestAttributes())
	return attributes
}

func guestOf(node types.String, vmid types.Int64, kind types.String) transport.Guest {
	return transport.Guest{
		Node: node.ValueString(),
		VMID: vmid.ValueInt64(),
		Kind: transport.Kind(kind.ValueString()),
	}
}

func providerDataFrom(req resource.ConfigureRequest, resp *resource.ConfigureResponse) *providerData {
	if req.ProviderData == nil {
		return nil
	}
	data, isProviderData := req.ProviderData.(*providerData)
	if !isProviderData {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Got %T. This is a defect in the provider.", req.ProviderData),
		)
		return nil
	}
	return data
}

// The provider sets LC_ALL=C for command-output and error parsing.
func guestCommand(argv ...string) transport.Command {
	return transport.Command{Argv: append([]string{"env", "LC_ALL=C"}, argv...)}
}

// runGuest runs the command in the guest. A nonzero exit status of the command
// is part of the result, and the caller decides whether it is a failure.
func runGuest(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	command transport.Command,
) (transport.Result, error) {
	result, err := client.Run(ctx, guest, command)
	if err != nil {
		slog.ErrorContext(
			ctx, "guest command failed",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err,
		)
		return transport.Result{}, fmt.Errorf("run guest command: %w", err)
	}
	return result, nil
}

func runChecked(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	command transport.Command,
) error {
	result, err := runGuest(ctx, client, guest, command)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandFailure(guest, command, result)
	}
	return nil
}

func commandFailure(guest transport.Guest, command transport.Command, result transport.Result) error {
	return fmt.Errorf(
		"%s: command %s exited with status %d: %s",
		guest,
		transport.QuoteCommand(command.Argv),
		result.ExitCode,
		combinedOutput(result),
	)
}

func combinedOutput(result transport.Result) string {
	text := strings.TrimSpace(string(result.Stderr))
	stdout := strings.TrimSpace(string(result.Stdout))
	if stdout != "" {
		if text != "" {
			text += "\n"
		}
		text += stdout
	}
	if len(text) > maximumErrorOutputBytes {
		text = text[len(text)-maximumErrorOutputBytes:]
	}
	return text
}

func firstLine(output []byte) string {
	line, _, _ := strings.Cut(string(output), "\n")
	return strings.TrimSpace(line)
}
