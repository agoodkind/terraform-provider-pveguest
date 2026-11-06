package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
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
	systemctlTimeoutSeconds = 300

	systemdUnitImportFormat  = "<node>/<vmid>/<kind>/<name>"
	systemdUnitImportSummary = "Import systemd unit"
	guestImportFields        = 4

	// ImportState sets this private state key, and Update deletes it after the
	// first successful apply.
	importedEntry = "imported"
)

var (
	unitNamePattern = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]+\.[a-z]+$`)

	missingUnitMessages = []string{missingFileMessage, "does not exist", "not found"}

	importedMarker = []byte(`true`)
)

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
	AllowMissing   types.Bool   `tfsdk:"allow_missing"`
	Masked         types.Bool   `tfsdk:"masked"`
	ResetFailed    types.Bool   `tfsdk:"reset_failed"`
}

func newSystemdUnitResource() resource.Resource {
	return &systemdUnitResource{}
}

func (r *systemdUnitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_systemd_unit"
}

func (r *systemdUnitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "pveguest_systemd_unit sets the enabled state and the active state of a systemd unit inside a guest. " +
			"The unit file must exist unless allow_missing or masked is true.",
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
			"allow_missing": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "allow_missing defaults to false. When allow_missing is true, an absent unit satisfies the resource. " +
					"allow_missing requires enabled = false and active = false.",
			},
			"masked": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "masked defaults to false. When masked is true, an apply stops an active unit, " +
					"runs systemctl mask, and verifies that systemctl is-enabled prints masked. " +
					"An apply can mask an absent unit. Changing masked to false runs systemctl unmask. " +
					"masked requires enabled = false and active = false.",
			},
			"reset_failed": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "reset_failed defaults to false. When reset_failed is true and the unit reports failed, " +
					"an apply runs systemctl reset-failed and verifies that the unit no longer reports failed.",
			},
		}),
	}
}

// ValidateConfig requires enabled = false and active = false when
// allow_missing or masked is true.
func (r *systemdUnitResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config systemdUnitModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	controls := []struct {
		name  string
		value types.Bool
	}{
		{name: "allow_missing", value: config.AllowMissing},
		{name: "masked", value: config.Masked},
	}
	wantsRunningOrEnabled := isDeclaredTrue(config.Enabled) || isDeclaredTrue(config.Active)
	for _, control := range controls {
		if isDeclaredTrue(control.value) && wantsRunningOrEnabled {
			resp.Diagnostics.AddAttributeError(
				path.Root(control.name),
				"Invalid systemd unit attribute combination",
				control.name+" requires enabled = false and active = false.",
			)
		}
	}
}

func isDeclaredTrue(value types.Bool) bool {
	return !value.IsNull() && !value.IsUnknown() && value.ValueBool()
}

func (r *systemdUnitResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *systemdUnitResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parsed, isValid := splitGuestImportID(req.ID)
	if !isValid || strings.Contains(parsed.Rest, "/") {
		resp.Diagnostics.AddError(systemdUnitImportSummary, importIDError(req.ID, systemdUnitImportFormat))
		return
	}

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	attributes := schemaResp.Schema.Attributes
	resp.Diagnostics.Append(validateImportedString(ctx, systemdUnitImportSummary, attributes, "kind", parsed.Kind)...)
	resp.Diagnostics.Append(validateImportedString(ctx, systemdUnitImportSummary, attributes, "name", parsed.Rest)...)
	if resp.Diagnostics.HasError() {
		return
	}

	imported := systemdUnitModel{
		Node:           types.StringValue(parsed.Node),
		VMID:           types.Int64Value(parsed.VMID),
		Kind:           types.StringValue(parsed.Kind),
		Name:           types.StringValue(parsed.Rest),
		Enabled:        types.BoolNull(),
		Active:         types.BoolNull(),
		RestartOn:      types.MapNull(types.StringType),
		RemoveOnDelete: types.BoolValue(false),
		AllowMissing:   types.BoolValue(false),
		Masked:         types.BoolValue(false),
		ResetFailed:    types.BoolValue(false),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &imported)...)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, importedEntry, importedMarker)...)
}

func hasOnlyImportedWriteIDs(value types.Map) bool {
	for _, element := range value.Elements() {
		text, isString := element.(types.String)
		if !isString || text.IsNull() || text.IsUnknown() || !isImportedWriteID(text.ValueString()) {
			return false
		}
	}
	return true
}

// guestImportID stores the parts of an import ID in the format
// <node>/<vmid>/<kind>/<rest>.
type guestImportID struct {
	Node string
	VMID int64
	Kind string
	// Rest contains all text after the third slash, including later slashes.
	Rest string
}

func splitGuestImportID(id string) (guestImportID, bool) {
	fields := strings.SplitN(id, "/", guestImportFields)
	if len(fields) != guestImportFields || slices.Contains(fields, "") {
		return guestImportID{}, false
	}
	vmid, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || vmid < 1 {
		return guestImportID{}, false
	}
	return guestImportID{Node: fields[0], VMID: vmid, Kind: fields[2], Rest: fields[3]}, true
}

func importIDError(id string, format string) string {
	return fmt.Sprintf("import ID %q must use the format %s", id, format)
}

// validateImportedString applies the schema validators for a string attribute
// to an import ID value.
func validateImportedString(
	ctx context.Context,
	summary string,
	attributes map[string]schema.Attribute,
	name string,
	value string,
) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	attribute, isString := attributes[name].(schema.StringAttribute)
	if !isString {
		diagnostics.AddError(summary,
			fmt.Sprintf("The schema does not define the string attribute %s. The provider contains a defect.", name))
		return diagnostics
	}
	request := validator.StringRequest{
		Path:           path.Root(name),
		PathExpression: path.MatchRoot(name),
		ConfigValue:    types.StringValue(value),
	}
	for _, check := range attribute.Validators {
		var response validator.StringResponse
		check.ValidateString(ctx, request, &response)
		diagnostics.Append(response.Diagnostics...)
	}
	return diagnostics
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
	if err := r.apply(ctx, plan, restart, false); err != nil {
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
		if state.AllowMissing.ValueBool() {
			// When allow_missing is true and the unit is absent, Read keeps the
			// resource in state and clears masked.
			state.Enabled = types.BoolValue(false)
			state.Active = types.BoolValue(false)
			state.Masked = types.BoolValue(false)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
		resp.State.RemoveResource(ctx)
		return
	}

	state.Enabled, state.Active = observedUnitValues(status, state.Enabled)
	if state.RemoveOnDelete.IsNull() {
		state.RemoveOnDelete = types.BoolValue(false)
	}
	// Read compares masked and reset_failed with the guest only for
	// attributes that are true in state.
	isMasked := status.FileState == unitFileMasked || status.FileState == unitFileMaskedRuntime
	if state.Masked.ValueBool() && !isMasked {
		state.Masked = types.BoolValue(false)
	}
	if state.ResetFailed.ValueBool() && status.Active == unitFailed {
		state.ResetFailed = types.BoolValue(false)
	}
	if state.AllowMissing.IsNull() {
		state.AllowMissing = types.BoolValue(false)
	}
	if state.Masked.IsNull() {
		state.Masked = types.BoolValue(false)
	}
	if state.ResetFailed.IsNull() {
		state.ResetFailed = types.BoolValue(false)
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
	imported, diagnostics := req.Private.GetKey(ctx, importedEntry)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	restart := !plan.RestartOn.Equal(state.RestartOn)
	// If the import key exists, prior restart_on is null, and every planned value
	// matches import- followed by exactly 16 lowercase hexadecimal characters,
	// Update skips the restart and systemctl reenable.
	if imported != nil && state.RestartOn.IsNull() && hasOnlyImportedWriteIDs(plan.RestartOn) {
		restart = false
	}
	// Unmask the unit only when masked changes from true in the prior state
	// to false in the plan.
	unmask := state.Masked.ValueBool() && !plan.Masked.ValueBool()
	if err := r.apply(ctx, plan, restart, unmask); err != nil {
		resp.Diagnostics.AddError("Update systemd unit state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, importedEntry, nil)...)
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
	target := r.unitTarget(state)
	if err := removeUnit(ctx, target, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("Delete systemd unit", err.Error())
	}
}

func (r *systemdUnitResource) unitTarget(model systemdUnitModel) guestUnitTarget {
	return guestUnitTarget{client: r.data.client, guest: guestOf(model.Node, model.VMID, model.Kind)}
}

func (r *systemdUnitResource) apply(ctx context.Context, plan systemdUnitModel, restart bool, unmask bool) error {
	target := r.unitTarget(plan)
	guest := target.guest
	name := plan.Name.ValueString()

	// A unit file written earlier in the same apply is unknown to the
	// manager until this reload.
	if err := target.runUnitAction(ctx, unitActionDaemonReload, name); err != nil {
		return err
	}
	status, err := readUnitStatus(ctx, r.data.client, guest, name)
	if err != nil {
		return err
	}

	if unmask {
		status, err = r.unmaskUnit(ctx, guest, name, status)
		if err != nil {
			return err
		}
	}

	if plan.Masked.ValueBool() {
		status, err = r.mask(ctx, guest, name, status)
		if err != nil {
			return err
		}
		_, err = r.clearFailed(ctx, plan, guest, name, status)
		return err
	}

	if !status.Found {
		if plan.AllowMissing.ValueBool() {
			return nil
		}
		return fmt.Errorf("%s: the unit file of %s does not exist", guest, name)
	}

	status, err = r.clearFailed(ctx, plan, guest, name, status)
	if err != nil {
		return err
	}
	return reconcileUnit(ctx, target, name, status, plan.Enabled.ValueBool(), plan.Active.ValueBool(), restart)
}

// mask disables and stops the unit when necessary, masks it, verifies that
// is-enabled prints masked, and returns the unit status.
func (r *systemdUnitResource) mask(
	ctx context.Context,
	guest transport.Guest,
	name string,
	status unitStatus,
) (unitStatus, error) {
	if err := r.disableAndStop(ctx, guest, name, status); err != nil {
		return unitStatus{}, err
	}
	if status.FileState != unitFileMasked {
		if err := runChecked(ctx, r.data.client, guest, systemctlCommand("mask", "--", name)); err != nil {
			return unitStatus{}, err
		}
	}

	after, err := readUnitStatus(ctx, r.data.client, guest, name)
	if err != nil {
		return unitStatus{}, err
	}
	if !after.Found || after.FileState != unitFileMasked {
		return unitStatus{}, fmt.Errorf(
			"on guest %s, unit %s reports unit file state %q instead of masked after systemctl mask",
			guest, name, after.FileState,
		)
	}
	return after, nil
}

func (r *systemdUnitResource) disableAndStop(
	ctx context.Context,
	guest transport.Guest,
	name string,
	status unitStatus,
) error {
	if !status.Found {
		return nil
	}
	if classifyEnabledState(status.FileState) == enablementEnabled {
		if err := runChecked(ctx, r.data.client, guest, systemctlCommand("disable", "--", name)); err != nil {
			return err
		}
	}
	if !isStoppedState(status.Active) {
		return runChecked(ctx, r.data.client, guest, systemctlCommand("stop", "--", name))
	}
	return nil
}

func (r *systemdUnitResource) unmaskUnit(
	ctx context.Context,
	guest transport.Guest,
	name string,
	status unitStatus,
) (unitStatus, error) {
	if !status.Found || (status.FileState != unitFileMasked && status.FileState != unitFileMaskedRuntime) {
		return status, nil
	}
	if err := runChecked(ctx, r.data.client, guest, systemctlCommand("unmask", "--", name)); err != nil {
		return unitStatus{}, err
	}
	if err := runChecked(ctx, r.data.client, guest, systemctlCommand("--runtime", "unmask", "--", name)); err != nil {
		return unitStatus{}, err
	}

	after, err := readUnitStatus(ctx, r.data.client, guest, name)
	if err != nil {
		return unitStatus{}, err
	}
	if after.Found && (after.FileState == unitFileMasked || after.FileState == unitFileMaskedRuntime) {
		return unitStatus{}, fmt.Errorf(
			"on guest %s, unit %s still reports unit file state %s after persistent and runtime unmask commands",
			guest, name, after.FileState,
		)
	}
	return after, nil
}

// clearFailed runs systemctl reset-failed for a failed unit, verifies that it
// no longer reports failed, and returns the unit status.
func (r *systemdUnitResource) clearFailed(
	ctx context.Context,
	plan systemdUnitModel,
	guest transport.Guest,
	name string,
	status unitStatus,
) (unitStatus, error) {
	if !plan.ResetFailed.ValueBool() || status.Active != unitFailed {
		return status, nil
	}
	if err := runChecked(ctx, r.data.client, guest, systemctlCommand("reset-failed", "--", name)); err != nil {
		return unitStatus{}, err
	}

	after, err := readUnitStatus(ctx, r.data.client, guest, name)
	if err != nil {
		return unitStatus{}, err
	}
	if after.Found && after.Active == unitFailed {
		return unitStatus{}, fmt.Errorf(
			"on guest %s, unit %s still reports active state failed after systemctl reset-failed",
			guest, name,
		)
	}
	return after, nil
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

// guestUnitTarget runs systemctl inside a guest through the guest agent or
// the container exec API.
type guestUnitTarget struct {
	client *transport.Client
	guest  transport.Guest
}

func (target guestUnitTarget) location() string {
	return "guest " + target.guest.String()
}

func (target guestUnitTarget) readUnit(ctx context.Context, name string) (unitStatus, error) {
	return readUnitStatus(ctx, target.client, target.guest, name)
}

func (target guestUnitTarget) runUnitAction(ctx context.Context, action unitAction, name string) error {
	command := systemctlCommand(string(action), "--", name)
	if action == unitActionDaemonReload {
		command = systemctlCommand(string(action))
	}
	return runChecked(ctx, target.client, target.guest, command)
}
