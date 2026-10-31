package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	// The target lock has a 10-second limit, and each consumer unit command has
	// a 300-second limit.
	hostSecretTaskTimeout = 10 * time.Minute

	hostSecretValueAttribute = "value_wo"

	// The private state key stores the storage file SHA-256 that the last apply
	// or import read from the node.
	hostSecretAppliedEntry = "applied_sha256"
	// The private state key stores whether the storage file SHA-256 in the last
	// Read differs from the applied SHA-256.
	hostSecretDriftEntry = "content_drift"

	hostSecretMinimumVersion = 1
	hostFieldImportFormat    = "<node>:<workspace>:<field>"
	hostFieldImportSummary   = "Cannot import host secret"
	hostSecretImportSep      = ":"

	hostSecretReadSummary    = "Read host secret"
	hostSecretWriteSummary   = "Set host secret"
	hostSecretDestroySummary = "Destroy host secret"
)

var hostSecretFieldPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

type hostSecretResource struct {
	data *providerData
}

type hostSecretModel struct {
	Node           types.String `tfsdk:"node"`
	Workspace      types.String `tfsdk:"workspace"`
	Field          types.String `tfsdk:"field"`
	ValueWO        types.String `tfsdk:"value_wo"`
	ValueWOVersion types.Int64  `tfsdk:"value_wo_version"`
	Configured     types.Bool   `tfsdk:"configured"`
	Version        types.Int64  `tfsdk:"version"`
}

func newHostSecretResource() resource.Resource {
	return &hostSecretResource{}
}

func (r *hostSecretResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_secret"
}

func (r *hostSecretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The resource sets one secret field of one workspace selector on a Proxmox node " +
			"through the overlay host artifact API. The node validates the value against the target policy " +
			"and restarts the consumer units when the storage file content changes. OpenTofu does not store the value in plans or state.",
		Attributes: map[string]schema.Attribute{
			"node": schema.StringAttribute{
				Description:   "The node value must be a key in the provider's nodes map. Changing node replaces the resource.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"workspace": schema.StringAttribute{
				Description:   "The workspace argument selects the exact target workspace, such as suburban/mwan. Changing workspace replaces the resource.",
				Required:      true,
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"field": schema.StringAttribute{
				Description: "The field argument selects a secret field in the target policy. Changing field replaces the resource.",
				Required:    true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(hostSecretFieldPattern, "must contain only uppercase letters, digits, and underscores"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			hostSecretValueAttribute: schema.StringAttribute{
				Description: "The value_wo argument specifies the secret. OpenTofu does not store it in plans or state. " +
					"A plan that needs a write fails when value_wo is absent.",
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
			},
			"value_wo_version": schema.Int64Attribute{
				Description: "The value_wo_version argument sets the secret version. Changing value_wo_version sends value_wo to the node. " +
					"The node rejects a lower version and rejects another value at the same version.",
				Required:   true,
				Validators: []validator.Int64{int64validator.AtLeast(hostSecretMinimumVersion)},
			},
			"configured": schema.BoolAttribute{
				Description: "The node reports configured as true when the storage file is a regular file and its mode, owner, and group match the policy.",
				Computed:    true,
			},
			"version": schema.Int64Attribute{
				Description: "The version attribute contains the version that the node recorded for the field.",
				Computed:    true,
			},
		},
	}
}

func (r *hostSecretResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

// ImportState calls only GET. It sets value_wo_version to the recorded version
// and stores the storage file SHA-256 as the applied SHA-256.
func (r *hostSecretResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	node, workspace, field, isValid := splitHostSecretImportID(req.ID)
	if !isValid {
		resp.Diagnostics.AddError(hostFieldImportSummary, importIDError(req.ID, hostFieldImportFormat))
		return
	}
	state := hostSecretModel{
		Node:           types.StringValue(node),
		Workspace:      types.StringValue(workspace),
		Field:          types.StringValue(field),
		ValueWO:        types.StringNull(),
		ValueWOVersion: types.Int64Null(),
	}
	entry, diagnostics := r.readEntry(ctx, state, hostFieldImportSummary)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	if entry.Version >= hostSecretMinimumVersion {
		state.ValueWOVersion = types.Int64Value(entry.Version)
	}
	setHostSecretObservation(&state, entry)
	resp.Diagnostics.Append(storeHostSecretApplied(ctx, resp.Private, entry)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// splitHostSecretImportID splits at the first and the last separator. A workspace
// selector can contain the separator, and a node or a field cannot.
func splitHostSecretImportID(id string) (string, string, string, bool) {
	node, rest, found := strings.Cut(id, hostSecretImportSep)
	if !found {
		return "", "", "", false
	}
	separator := strings.LastIndex(rest, hostSecretImportSep)
	if separator < 0 {
		return "", "", "", false
	}
	workspace := rest[:separator]
	field := rest[separator+len(hostSecretImportSep):]
	if node == "" || workspace == "" || !hostSecretFieldPattern.MatchString(field) {
		return "", "", "", false
	}
	return node, workspace, field, true
}

func (r *hostSecretResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan hostSecretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state hostSecretModel
	write := true
	if !req.State.Raw.IsNull() {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		drift, diagnostics := req.Private.GetKey(ctx, hostSecretDriftEntry)
		resp.Diagnostics.Append(diagnostics...)
		if resp.Diagnostics.HasError() {
			return
		}
		write = hostSecretWritePlanned(plan, state, decodeMismatch(drift))
	}
	if !write {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("configured"), state.Configured)...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("version"), state.Version)...)
		return
	}

	var value types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, fwpath.Root(hostSecretValueAttribute), &value)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if value.IsNull() {
		resp.Diagnostics.AddAttributeError(
			fwpath.Root(hostSecretValueAttribute),
			"Missing value_wo",
			"The plan must write the secret field to the node. Set value_wo in the configuration.",
		)
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("configured"), types.BoolUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("version"), types.Int64Unknown())...)
}

// hostSecretWritePlanned returns true when the resource needs replacement,
// value_wo_version changes, configured is false, or the storage file SHA-256
// differs from the applied SHA-256.
func hostSecretWritePlanned(plan hostSecretModel, state hostSecretModel, drift bool) bool {
	replaced := !plan.Node.Equal(state.Node) || !plan.Workspace.Equal(state.Workspace) || !plan.Field.Equal(state.Field)
	return replaced || !plan.ValueWOVersion.Equal(state.ValueWOVersion) || !state.Configured.ValueBool() || drift
}

func (r *hostSecretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostSecretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entry, diagnostics := r.write(ctx, plan, req.Config)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	setHostSecretObservation(&plan, entry)
	resp.Diagnostics.Append(storeHostSecretApplied(ctx, resp.Private, entry)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostSecretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hostSecretModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entry, diagnostics := r.write(ctx, plan, req.Config)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	setHostSecretObservation(&plan, entry)
	resp.Diagnostics.Append(storeHostSecretApplied(ctx, resp.Private, entry)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read calls only GET. It stores a boolean in private state that records whether
// the storage file SHA-256 differs from the applied SHA-256.
// It does not store either SHA-256 in an attribute.
func (r *hostSecretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostSecretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	applied, diagnostics := req.Private.GetKey(ctx, hostSecretAppliedEntry)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	entry, diagnostics := r.readEntry(ctx, state, hostSecretReadSummary)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	setHostSecretObservation(&state, entry)
	drift, err := encodeHostSecretDrift(ctx, entry.SHA256 != decodeAppliedHash(applied))
	if err != nil {
		resp.Diagnostics.AddError(hostSecretReadSummary, err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, hostSecretDriftEntry, drift)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostSecretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostSecretModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.destroyField(ctx, state)...)
}

// destroyField uses only GET and never changes the node. It returns an error
// while the node stores the field. OpenTofu keeps the resource in state on
// error and removes it on success.
//
// Root review may replace this interim destroy behavior.
func (r *hostSecretResource) destroyField(ctx context.Context, state hostSecretModel) diag.Diagnostics {
	entry, diagnostics := r.readEntry(ctx, state, hostSecretDestroySummary)
	if diagnostics.HasError() {
		return diagnostics
	}
	if entry.Version >= hostSecretMinimumVersion || entry.SHA256 != "" {
		diagnostics.AddError(hostSecretDestroySummary, fmt.Sprintf(
			"Node %q stores secret field %s of workspace %q. The API does not delete a secret field. "+
				"Run tofu state rm to stop managing the field.",
			state.Node.ValueString(), state.Field.ValueString(), state.Workspace.ValueString(),
		))
	}
	return diagnostics
}

// readEntry calls GET and returns the entry for the selected field.
func (r *hostSecretResource) readEntry(
	ctx context.Context,
	model hostSecretModel,
	summary string,
) (transport.HostSecretState, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	workspace := model.Workspace.ValueString()
	field := model.Field.ValueString()
	current, err := r.data.client.GetHostArtifact(ctx, model.Node.ValueString(), transport.HostArtifactTarget(workspace))
	if err != nil {
		diagnostics.AddError(summary, err.Error())
		return transport.HostSecretState{}, diagnostics
	}
	if current.Workspace != workspace {
		diagnostics.AddError(summary, fmt.Sprintf(
			"The node reports workspace %q for the target of workspace %q.", current.Workspace, workspace,
		))
		return transport.HostSecretState{}, diagnostics
	}
	for _, entry := range current.Secrets {
		if entry.Field == field {
			return entry, diagnostics
		}
	}
	diagnostics.AddError(summary, fmt.Sprintf("The policy for workspace %q does not list secret field %s.", workspace, field))
	return transport.HostSecretState{}, diagnostics
}

// write sends the configured value and version, waits for the hostsecret task,
// and returns the field entry from GET after the task finishes.
func (r *hostSecretResource) write(
	ctx context.Context,
	plan hostSecretModel,
	config tfsdk.Config,
) (transport.HostSecretState, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	var value types.String
	diagnostics.Append(config.GetAttribute(ctx, fwpath.Root(hostSecretValueAttribute), &value)...)
	if diagnostics.HasError() {
		return transport.HostSecretState{}, diagnostics
	}
	if value.IsNull() || value.IsUnknown() {
		diagnostics.AddAttributeError(
			fwpath.Root(hostSecretValueAttribute),
			"Missing value_wo",
			"The apply must write the secret field to the node. Set value_wo to a value that is known during apply.",
		)
		return transport.HostSecretState{}, diagnostics
	}

	node := plan.Node.ValueString()
	field := plan.Field.ValueString()
	version := plan.ValueWOVersion.ValueInt64()
	target := transport.HostArtifactTarget(plan.Workspace.ValueString())
	upid, err := r.data.client.SetHostSecret(ctx, node, target, field, value.ValueString(), version)
	if err != nil {
		diagnostics.AddError(hostSecretWriteSummary, err.Error())
		return transport.HostSecretState{}, diagnostics
	}
	if err := r.data.client.WaitTaskWithin(ctx, node, upid, hostSecretTaskTimeout); err != nil {
		diagnostics.AddError(hostSecretWriteSummary, redactSecret(err.Error(), value.ValueString()))
		return transport.HostSecretState{}, diagnostics
	}

	entry, readDiagnostics := r.readEntry(ctx, plan, hostSecretWriteSummary)
	diagnostics.Append(readDiagnostics...)
	if diagnostics.HasError() {
		return transport.HostSecretState{}, diagnostics
	}
	if !entry.Configured || entry.Version != version {
		diagnostics.AddError(hostSecretWriteSummary, fmt.Sprintf(
			"After the hostsecret task, node %q reports configured %t and version %d for secret field %s. The apply requires configured true and version %d.",
			node, entry.Configured, entry.Version, field, version,
		))
	}
	return entry, diagnostics
}

// setHostSecretObservation sets configured and version from the GET entry.
func setHostSecretObservation(model *hostSecretModel, entry transport.HostSecretState) {
	model.Configured = types.BoolValue(entry.Configured)
	model.Version = types.Int64Null()
	if entry.Version >= hostSecretMinimumVersion {
		model.Version = types.Int64Value(entry.Version)
	}
}

// storeHostSecretApplied stores the storage file SHA-256 from GET in private
// state and sets content_drift to false.
func storeHostSecretApplied(ctx context.Context, private privateSetter, entry transport.HostSecretState) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	applied, err := encodeAppliedHash(entry.SHA256)
	if err != nil {
		diagnostics.AddError(hostSecretWriteSummary, err.Error())
		return diagnostics
	}
	drift, err := encodeHostSecretDrift(ctx, false)
	if err != nil {
		diagnostics.AddError(hostSecretWriteSummary, err.Error())
		return diagnostics
	}
	diagnostics.Append(private.SetKey(ctx, hostSecretAppliedEntry, applied)...)
	diagnostics.Append(private.SetKey(ctx, hostSecretDriftEntry, drift)...)
	return diagnostics
}

func encodeHostSecretDrift(ctx context.Context, drift bool) ([]byte, error) {
	document, err := json.Marshal(hostArtifactMismatchDocument{Mismatch: drift})
	if err != nil {
		slog.ErrorContext(ctx, "encode host secret drift failed", "err", err)
		return nil, fmt.Errorf("encode host secret drift: %w", err)
	}
	return document, nil
}
