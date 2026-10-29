package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

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

const systemctlTimeoutSeconds = 300

var (
	unitNamePattern = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+\.[a-z]+$`)

	missingUnitMessages = []string{missingFileMessage, "does not exist", "not found"}
)

type unitFileState string

const (
	unitFileEnabled        unitFileState = "enabled"
	unitFileEnabledRuntime unitFileState = "enabled-runtime"
	unitFileStatic         unitFileState = "static"
	unitFileAlias          unitFileState = "alias"
	unitFileIndirect       unitFileState = "indirect"
	unitFileGenerated      unitFileState = "generated"
	unitFileTransient      unitFileState = "transient"

	// systemd 253 and later print this state when the unit file does not
	// exist. Earlier versions print an error message.
	unitFileNotFound unitFileState = "not-found"
)

type unitActiveState string

const (
	unitActive     unitActiveState = "active"
	unitActivating unitActiveState = "activating"
	unitReloading  unitActiveState = "reloading"
	unitInactive   unitActiveState = "inactive"
	unitFailed     unitActiveState = "failed"
)

// enablement classifies the output of systemctl is-enabled.
type enablement int

const (
	enablementDisabled enablement = iota
	enablementEnabled
	// systemd reports a fixed unit as static, alias, indirect, generated, or
	// transient. A fixed unit satisfies both values of enabled.
	enablementFixed
)

type unitStatus struct {
	Found     bool
	FileState unitFileState
	Active    unitActiveState
}

type systemdUnitResource struct {
	data *providerData
}

type systemdUnitModel struct {
	Node           types.String `tfsdk:"node"`
	VMID           types.Int64  `tfsdk:"vmid"`
	Kind           types.String `tfsdk:"kind"`
	Name           types.String `tfsdk:"name"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	Active         types.Bool   `tfsdk:"active"`
	RestartOn      types.Map    `tfsdk:"restart_on"`
	RemoveOnDelete types.Bool   `tfsdk:"remove_on_delete"`
}

func newSystemdUnitResource() resource.Resource {
	return &systemdUnitResource{}
}

func (r *systemdUnitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_systemd_unit"
}

func (r *systemdUnitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The enabled state and the active state of a systemd unit inside a guest. " +
			"The unit file must exist.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Full unit name with suffix, for example ssh.service.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(unitNamePattern, "must be a unit name with a suffix"),
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
				Description: "Arbitrary values, usually the write_id of each file that the unit reads. " +
					"A changed map restarts an active unit during apply.",
			},
			"remove_on_delete": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "When true, Destroy stops the unit if active and disables it if enabled. Defaults to false, which removes only the resource from OpenTofu state.",
			},
		}),
	}
}

func (r *systemdUnitResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *systemdUnitResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan systemdUnitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A new restart_on map can reference files created before resource adoption.
	// The first restart applies those file dependencies to an active unit.
	restart := len(plan.RestartOn.Elements()) > 0
	if err := r.apply(ctx, plan, restart); err != nil {
		resp.Diagnostics.AddError("Create systemd unit state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *systemdUnitResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state systemdUnitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	status, err := readUnitStatus(ctx, r.data.client, guest, state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read systemd unit", err.Error())
		return
	}
	if !status.Found {
		resp.State.RemoveResource(ctx)
		return
	}

	switch classifyEnabledState(status.FileState) {
	case enablementEnabled:
		state.Enabled = types.BoolValue(true)
	case enablementDisabled:
		state.Enabled = types.BoolValue(false)
	case enablementFixed:
		// A fixed unit satisfies the declared value, so Read writes the
		// enabled value to state only for enabled and disabled units.
	}
	state.Active = types.BoolValue(isActiveState(status.Active))
	if state.RemoveOnDelete.IsNull() {
		state.RemoveOnDelete = types.BoolValue(false)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *systemdUnitResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan systemdUnitModel
	var state systemdUnitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	restart := !plan.RestartOn.Equal(state.RestartOn)
	if err := r.apply(ctx, plan, restart); err != nil {
		resp.Diagnostics.AddError("Update systemd unit state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *systemdUnitResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state systemdUnitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !state.RemoveOnDelete.ValueBool() {
		return
	}
	if err := r.remove(ctx, state); err != nil {
		resp.Diagnostics.AddError("Delete systemd unit", err.Error())
	}
}

func (r *systemdUnitResource) remove(ctx context.Context, state systemdUnitModel) error {
	guest := guestOf(state.Node, state.VMID, state.Kind)
	name := state.Name.ValueString()

	status, err := readUnitStatus(ctx, r.data.client, guest, name)
	if err != nil {
		return err
	}
	if !status.Found {
		return nil
	}

	if !isStoppedState(status.Active) {
		if err := runChecked(ctx, r.data.client, guest, systemctlCommand("stop", "--", name)); err != nil {
			return err
		}
	}
	if classifyEnabledState(status.FileState) == enablementEnabled {
		if err := runChecked(ctx, r.data.client, guest, systemctlCommand("disable", "--", name)); err != nil {
			return err
		}
	}

	after, err := readUnitStatus(ctx, r.data.client, guest, name)
	if err != nil {
		return err
	}
	if !after.Found {
		return nil
	}
	if !isStoppedState(after.Active) {
		return fmt.Errorf("on guest %s, unit %s has active state %s after stop; the active state must be inactive or failed", guest, name, after.Active)
	}
	if classifyEnabledState(after.FileState) == enablementEnabled {
		return fmt.Errorf("on guest %s, unit %s has unit file state %s after disable; the unit file state must not be enabled", guest, name, after.FileState)
	}
	return nil
}

func (r *systemdUnitResource) apply(ctx context.Context, plan systemdUnitModel, restart bool) error {
	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	name := plan.Name.ValueString()

	// A unit file written earlier in the same apply is unknown to the
	// manager until this reload.
	if err := runChecked(ctx, r.data.client, guest, systemctlCommand("daemon-reload")); err != nil {
		return err
	}
	status, err := readUnitStatus(ctx, r.data.client, guest, name)
	if err != nil {
		return err
	}
	if !status.Found {
		return fmt.Errorf("%s: the unit file of %s does not exist", guest, name)
	}

	current := classifyEnabledState(status.FileState)
	wantEnabled := plan.Enabled.ValueBool()
	if current == enablementDisabled && wantEnabled {
		if err := runChecked(ctx, r.data.client, guest, systemctlCommand("enable", "--", name)); err != nil {
			return err
		}
	}
	// Rebuild installation links after dependency changes.
	if current == enablementEnabled && wantEnabled && restart {
		if err := runChecked(ctx, r.data.client, guest, systemctlCommand("reenable", "--", name)); err != nil {
			return err
		}
	}
	if current == enablementEnabled && !wantEnabled {
		if err := runChecked(ctx, r.data.client, guest, systemctlCommand("disable", "--", name)); err != nil {
			return err
		}
	}

	isActive := isActiveState(status.Active)
	wantActive := plan.Active.ValueBool()
	var action string
	switch {
	case wantActive && !isActive:
		action = "start"
	case wantActive && restart:
		action = "restart"
	case !wantActive && isActive:
		action = "stop"
	default:
		return nil
	}
	return runChecked(ctx, r.data.client, guest, systemctlCommand(action, "--", name))
}

func systemctlCommand(arguments ...string) transport.Command {
	command := guestCommand(append([]string{"systemctl"}, arguments...)...)
	command.TimeoutSeconds = systemctlTimeoutSeconds
	return command
}

func readUnitStatus(ctx context.Context, client *transport.Client, guest transport.Guest, name string) (unitStatus, error) {
	enabledCommand := systemctlCommand("is-enabled", "--", name)
	enabledResult, err := runGuest(ctx, client, guest, enabledCommand)
	if err != nil {
		return unitStatus{}, err
	}
	fileState := unitFileState(firstLine(enabledResult.Stdout))
	if fileState == unitFileNotFound {
		return unitStatus{}, nil
	}
	if fileState == "" {
		if reportsMissingUnit(string(enabledResult.Stderr)) {
			return unitStatus{}, nil
		}
		return unitStatus{}, commandFailure(guest, enabledCommand, enabledResult)
	}

	// is-active exits nonzero for every state except active, and prints
	// the state in each case.
	activeCommand := systemctlCommand("is-active", "--", name)
	activeResult, err := runGuest(ctx, client, guest, activeCommand)
	if err != nil {
		return unitStatus{}, err
	}
	activeState := unitActiveState(firstLine(activeResult.Stdout))
	if activeState == "" {
		return unitStatus{}, commandFailure(guest, activeCommand, activeResult)
	}
	return unitStatus{Found: true, FileState: fileState, Active: activeState}, nil
}

func reportsMissingUnit(stderr string) bool {
	for _, message := range missingUnitMessages {
		if strings.Contains(stderr, message) {
			return true
		}
	}
	return false
}

func classifyEnabledState(state unitFileState) enablement {
	switch state {
	case unitFileEnabled, unitFileEnabledRuntime:
		return enablementEnabled
	case unitFileStatic, unitFileAlias, unitFileIndirect, unitFileGenerated, unitFileTransient:
		return enablementFixed
	case unitFileNotFound:
		return enablementDisabled
	default:
		return enablementDisabled
	}
}

func isStoppedState(state unitActiveState) bool {
	return state == unitInactive || state == unitFailed
}

func isActiveState(state unitActiveState) bool {
	switch state {
	case unitActive, unitActivating, unitReloading:
		return true
	case unitInactive, unitFailed:
		return false
	default:
		return false
	}
}
