package provider

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	// The target lock has a 10-second limit, and each systemctl action has a
	// 300-second limit.
	hostUnitTaskTimeout = 10 * time.Minute

	hostUnitImportFormat  = "<node>:<workspace>:<unit>"
	hostUnitImportSummary = "Import host systemd unit"
	hostUnitCreateSummary = "Create host systemd unit state"
	hostUnitReadSummary   = "Read host systemd unit"
	hostUnitUpdateSummary = "Update host systemd unit state"
	hostUnitDeleteSummary = "Delete host systemd unit"
)

// The import format reserves the colon as a separator.
var hostUnitNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]{0,246}\.service$`)

type hostSystemdUnitResource struct {
	data *providerData
}

type hostSystemdUnitModel struct {
	Node           types.String `tfsdk:"node"`
	Workspace      types.String `tfsdk:"workspace"`
	Name           types.String `tfsdk:"name"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	Active         types.Bool   `tfsdk:"active"`
	RestartOn      types.Map    `tfsdk:"restart_on"`
	RemoveOnDelete types.Bool   `tfsdk:"remove_on_delete"`
}

func newHostSystemdUnitResource() resource.Resource {
	return &hostSystemdUnitResource{}
}

func (r *hostSystemdUnitResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_host_systemd_unit"
}

func (r *hostSystemdUnitResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "pveguest_host_systemd_unit sets the enabled state and the active state of a systemd unit on a Proxmox node " +
			"through the overlay host artifact API. The target policy of the workspace must list the unit file " +
			"/etc/systemd/system/<name>, and the committed installed artifact must contain a matching unit file.",
		Attributes: map[string]schema.Attribute{
			"node": schema.StringAttribute{
				Required:      true,
				Description:   "The node value must be a key in the provider's nodes map. Changing node replaces the resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"workspace": schema.StringAttribute{
				Required:      true,
				Description:   "The workspace argument selects the target workspace, such as suburban/mwan. Changing workspace replaces the resource.",
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Full service unit name, for example mwan-ifmgr.service. Changing name replaces the resource.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						hostUnitNamePattern,
						"must be a service unit name without a colon",
					),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"enabled": schema.BoolAttribute{
				Required:    true,
				Description: "Whether the unit is enabled. A static unit satisfies both values.",
			},
			"active": schema.BoolAttribute{
				Required:    true,
				Description: "Whether the unit is active.",
			},
			"restart_on": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Values to watch, such as the write_id of each file that the unit reads. " +
					"A changed map restarts an active unit during apply when active is true. A nonempty map also does this on creation.",
			},
			"remove_on_delete": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "When true, destroy stops the unit unless it is inactive or failed and disables it if enabled. By default, destroy removes only the resource from OpenTofu state.",
			},
		},
	}
}

func (r *hostSystemdUnitResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	r.data = providerDataFrom(req, resp)
}

func (r *hostSystemdUnitResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	node, workspace, name, isValid := splitHostImportID(req.ID)
	if !isValid || !hostUnitNamePattern.MatchString(name) {
		resp.Diagnostics.AddError(
			hostUnitImportSummary,
			importIDError(req.ID, hostUnitImportFormat),
		)

		return
	}

	imported := hostSystemdUnitModel{
		Node:           types.StringValue(node),
		Workspace:      types.StringValue(workspace),
		Name:           types.StringValue(name),
		Enabled:        types.BoolNull(),
		Active:         types.BoolNull(),
		RestartOn:      types.MapNull(types.StringType),
		RemoveOnDelete: types.BoolValue(false),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &imported)...)
}

func (r *hostSystemdUnitResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan hostSystemdUnitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// A nonempty restart_on map can reference files installed before the
	// resource existed. Restart an active unit if the plan keeps it active.
	restart := len(plan.RestartOn.Elements()) > 0
	if err := r.apply(ctx, plan, restart); err != nil {
		resp.Diagnostics.AddError(hostUnitCreateSummary, err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostSystemdUnitResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state hostSystemdUnitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	status, err := r.unitTarget(state).readUnit(ctx, state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(hostUnitReadSummary, err.Error())
		return
	}

	if !status.Found {
		resp.State.RemoveResource(ctx)
		return
	}

	state.Enabled, state.Active = observedUnitValues(status, state.Enabled)
	if state.RemoveOnDelete.IsNull() {
		state.RemoveOnDelete = types.BoolValue(false)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostSystemdUnitResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var plan hostSystemdUnitModel
	var state hostSystemdUnitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	restart := !plan.RestartOn.Equal(state.RestartOn)
	if err := r.apply(ctx, plan, restart); err != nil {
		resp.Diagnostics.AddError(hostUnitUpdateSummary, err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostSystemdUnitResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state hostSystemdUnitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !state.RemoveOnDelete.ValueBool() {
		return
	}

	target := r.unitTarget(state)
	defer r.data.hostTargetLocks.lock(target.node, target.target)()
	if err := removeUnit(ctx, target, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError(hostUnitDeleteSummary, err.Error())
	}
}

func (r *hostSystemdUnitResource) apply(
	ctx context.Context,
	plan hostSystemdUnitModel,
	restart bool,
) error {
	target := r.unitTarget(plan)
	name := plan.Name.ValueString()
	defer r.data.hostTargetLocks.lock(target.node, target.target)()

	// A unit file that the host artifact installed earlier in the same apply
	// is unknown to the manager until this reload.
	if err := target.runUnitAction(ctx, unitActionDaemonReload, name); err != nil {
		return err
	}

	status, err := target.readUnit(ctx, name)
	if err != nil {
		return err
	}

	if !status.Found {
		return fmt.Errorf("on %s, the unit file of %s does not exist", target.location(), name)
	}

	return reconcileUnit(
		ctx,
		target,
		name,
		status,
		plan.Enabled.ValueBool(),
		plan.Active.ValueBool(),
		restart,
	)
}

func (r *hostSystemdUnitResource) unitTarget(model hostSystemdUnitModel) hostUnitTarget {
	workspace := model.Workspace.ValueString()

	return hostUnitTarget{
		client:    r.data.client,
		node:      model.Node.ValueString(),
		workspace: workspace,
		target:    transport.HostArtifactTarget(workspace),
	}
}

// hostUnitTarget reads and controls units of one host artifact target
// through the typed unit methods of the host artifact API.
type hostUnitTarget struct {
	client    *transport.Client
	node      string
	workspace string
	target    string
}

func (target hostUnitTarget) location() string {
	return fmt.Sprintf("node %q workspace %q", target.node, target.workspace)
}

func (target hostUnitTarget) readUnit(ctx context.Context, name string) (unitStatus, error) {
	state, err := target.client.GetHostUnit(ctx, target.node, target.target, name)
	if err != nil {
		slog.ErrorContext(
			ctx,
			"read host unit failed",
			"node",
			target.node,
			"workspace",
			target.workspace,
			"unit",
			name,
			"err",
			err,
		)

		return unitStatus{}, fmt.Errorf("on %s, read unit %s: %w", target.location(), name, err)
	}

	if !state.Found {
		return unitStatus{}, nil
	}

	return unitStatus{
		Found:     true,
		FileState: unitFileState(state.UnitFileState),
		Active:    unitActiveState(state.ActiveState),
	}, nil
}

func (target hostUnitTarget) runUnitAction(
	ctx context.Context,
	action unitAction,
	name string,
) error {
	upid, err := target.client.StartHostUnitAction(
		ctx,
		target.node,
		target.target,
		name,
		string(action),
	)
	if err == nil {
		err = target.client.WaitTaskWithin(ctx, target.node, upid, hostUnitTaskTimeout)
	}

	if err != nil {
		slog.ErrorContext(
			ctx,
			"host unit action failed",
			"node",
			target.node,
			"workspace",
			target.workspace,
			"unit",
			name,
			"action",
			action,
			"err",
			err,
		)

		return fmt.Errorf(
			"on %s, systemctl %s for unit %s: %w",
			target.location(),
			action,
			name,
			err,
		)
	}

	return nil
}
