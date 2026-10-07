package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	containerPowerDescription = "The resource sets the run state of one existing Proxmox LXC container through the Proxmox API. " +
		"Destroying the resource removes it from state without changing the container run state."
	containerPowerNodeDescription = "The node argument selects the Proxmox node by a key in the provider nodes map. " +
		"Changing node replaces the resource."
	containerPowerVMIDDescription = "The vmid argument specifies the numeric ID of the container. " +
		"Changing vmid replaces the resource."
	containerPowerRunningDescription = "A value of true starts a stopped container. " +
		"A value of false shuts down a running container."
	containerPowerRestartOnDescription = "The restart_on map accepts arbitrary string values, such as the write_id of a file resource. " +
		"The provider shuts down the container and starts it again when running is true and any map value changes."
	containerPowerStatusDescription = "The status attribute reports the Proxmox status/current string from the last apply or refresh. " +
		"Values include running and stopped."
)

type containerPowerResource struct {
	data *providerData
}

type containerPowerModel struct {
	Node      types.String `tfsdk:"node"`
	VMID      types.Int64  `tfsdk:"vmid"`
	Running   types.Bool   `tfsdk:"running"`
	RestartOn types.Map    `tfsdk:"restart_on"`
	Status    types.String `tfsdk:"status"`
}

func newContainerPowerResource() resource.Resource {
	return &containerPowerResource{}
}

func (r *containerPowerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container_power"
}

func (r *containerPowerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: containerPowerDescription,
		Attributes: map[string]schema.Attribute{
			"node": schema.StringAttribute{
				Description:   containerPowerNodeDescription,
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"vmid": schema.Int64Attribute{
				Description:   containerPowerVMIDDescription,
				Required:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"running": schema.BoolAttribute{
				Description: containerPowerRunningDescription,
				Required:    true,
			},
			"restart_on": schema.MapAttribute{
				Description: containerPowerRestartOnDescription,
				Optional:    true,
				ElementType: types.StringType,
			},
			"status": schema.StringAttribute{
				Description: containerPowerStatusDescription,
				Computed:    true,
			},
		},
	}
}

func (r *containerPowerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

// Import IDs use the format <node>/<vmid>.
func (r *containerPowerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	node, vmidText, found := strings.Cut(req.ID, "/")
	vmid, err := strconv.ParseInt(vmidText, 10, 64)
	if !found || node == "" || err != nil {
		resp.Diagnostics.AddError("Import container power", fmt.Sprintf("the import ID %q must have the form <node>/<vmid>", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("node"), node)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("vmid"), vmid)...)
}

func (r *containerPowerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan containerPowerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.apply(ctx, &plan, false); err != nil {
		resp.Diagnostics.AddError("Set container run state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerPowerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan containerPowerModel
	var state containerPowerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	restart := !plan.RestartOn.Equal(state.RestartOn)
	if err := r.apply(ctx, &plan, restart); err != nil {
		resp.Diagnostics.AddError("Set container run state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerPowerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state containerPowerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, err := r.data.client.GetContainerStatus(ctx, state.Node.ValueString(), state.VMID.ValueInt64())
	if err != nil {
		resp.Diagnostics.AddError("Read container run state", err.Error())
		return
	}
	state.Status = types.StringValue(status)
	state.Running = types.BoolValue(status == transport.ContainerRunning)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// The framework removes the resource from state after Delete returns.
func (r *containerPowerResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *containerPowerResource) apply(ctx context.Context, plan *containerPowerModel, restart bool) error {
	node := plan.Node.ValueString()
	vmid := plan.VMID.ValueInt64()
	client := r.data.client

	status, err := client.GetContainerStatus(ctx, node, vmid)
	if err != nil {
		slog.ErrorContext(ctx, "read container status failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("read status: %w", err)
	}
	running := status == transport.ContainerRunning
	switch {
	case plan.Running.ValueBool() && !running:
		err = client.StartContainer(ctx, node, vmid)
	case plan.Running.ValueBool() && restart:
		err = r.restart(ctx, node, vmid)
	case !plan.Running.ValueBool() && running:
		err = r.halt(ctx, node, vmid)
	}
	if err != nil {
		slog.ErrorContext(ctx, "change container run state failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("change run state: %w", err)
	}
	status, err = client.GetContainerStatus(ctx, node, vmid)
	if err != nil {
		slog.ErrorContext(ctx, "read container status failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("read status: %w", err)
	}
	plan.Status = types.StringValue(status)
	return nil
}

func (r *containerPowerResource) restart(ctx context.Context, node string, vmid int64) error {
	if err := r.halt(ctx, node, vmid); err != nil {
		slog.ErrorContext(ctx, "halt before restart failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("halt before restart: %w", err)
	}
	if err := r.data.client.StartContainer(ctx, node, vmid); err != nil {
		slog.ErrorContext(ctx, "start after shutdown failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("start after shutdown: %w", err)
	}
	return nil
}

// halt requests a shutdown and stops the container if it remains running.
func (r *containerPowerResource) halt(ctx context.Context, node string, vmid int64) error {
	client := r.data.client
	shutdownErr := client.ShutdownContainer(ctx, node, vmid, transport.ShutdownTimeoutSeconds)
	if shutdownErr != nil {
		slog.WarnContext(ctx, "container shutdown failed", "node", node, "vmid", vmid, "err", shutdownErr)
	}
	status, err := client.GetContainerStatus(ctx, node, vmid)
	if err != nil {
		slog.ErrorContext(ctx, "read container status after shutdown failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("read status after shutdown: %w", err)
	}
	if status != transport.ContainerRunning {
		return nil
	}
	if err := client.StopContainer(ctx, node, vmid); err != nil {
		slog.ErrorContext(ctx, "stop after shutdown failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("stop after shutdown: %w", err)
	}
	return nil
}
