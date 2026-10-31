package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	// The hostartifact task runs gh attestation verify with a 300-second
	// limit, waits up to 10 seconds for the target lock, and then writes the
	// files.
	hostArtifactTaskTimeout = 10 * time.Minute

	// The private state key stores the observed_sha256 that the node reported
	// after the last apply.
	hostArtifactAppliedObservedEntry = "applied_observed_sha256"
	// The private state key stores whether the last Read found a file or owned
	// directory that differs from the installed record.
	hostArtifactMismatchEntry = "observation_mismatch"

	hostArtifactImportFormat  = "<node>:<workspace>"
	hostArtifactImportSummary = "Cannot import host artifact"
	hostArtifactImportSep     = ":"
)

var (
	hostArtifactNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	hostArtifactRevisionPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hostArtifactRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

	hostArtifactFileType = types.ObjectType{AttrTypes: map[string]attr.Type{
		"sha256": types.StringType,
		"mode":   types.StringType,
		"owner":  types.StringType,
		"group":  types.StringType,
	}}
	hostArtifactDirectoryType = types.ObjectType{AttrTypes: map[string]attr.Type{
		"mode":  types.StringType,
		"owner": types.StringType,
		"group": types.StringType,
	}}
)

type hostArtifactResource struct {
	data *providerData
}

type hostArtifactModel struct {
	Node           types.String `tfsdk:"node"`
	Workspace      types.String `tfsdk:"workspace"`
	Repository     types.String `tfsdk:"repository"`
	ReleaseTag     types.String `tfsdk:"release_tag"`
	Asset          types.String `tfsdk:"asset"`
	SHA256         types.String `tfsdk:"sha256"`
	BundleAsset    types.String `tfsdk:"bundle_asset"`
	BundleSHA256   types.String `tfsdk:"bundle_sha256"`
	SourceRevision types.String `tfsdk:"source_revision"`

	Target                  types.String `tfsdk:"target"`
	State                   types.String `tfsdk:"state"`
	InstalledSHA256         types.String `tfsdk:"installed_sha256"`
	InstalledSourceRevision types.String `tfsdk:"installed_source_revision"`
	ObservedSHA256          types.String `tfsdk:"observed_sha256"`
	Files                   types.Map    `tfsdk:"files"`
	Directories             types.Map    `tfsdk:"directories"`
	WriteIDs                types.Map    `tfsdk:"write_ids"`
}

// The framework accepts only JSON documents as private state values.
type hostArtifactMismatchDocument struct {
	Mismatch bool `json:"mismatch"`
}

func newHostArtifactResource() resource.Resource {
	return &hostArtifactResource{}
}

func (r *hostArtifactResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_artifact"
}

func (r *hostArtifactResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attributes := hostArtifactPinAttributes()
	maps.Copy(attributes, hostArtifactObservedAttributes())
	resp.Schema = schema.Schema{
		Description: "The resource installs a signed host artifact archive for one workspace selector on a Proxmox node " +
			"through the overlay host artifact API. The node runs gh attestation verify to verify the archive. " +
			"The resource reads and writes only the target of its workspace.",
		Attributes: attributes,
	}
}

// hostArtifactPinAttributes returns the arguments that select the node, the
// target, and the pinned release assets.
func hostArtifactPinAttributes() map[string]schema.Attribute {
	nameValidators := []validator.String{
		stringvalidator.RegexMatches(hostArtifactNamePattern, "must start with a letter or digit and contain only letters, digits, '.', '_', and '-'"),
	}
	sha256Validators := []validator.String{
		stringvalidator.RegexMatches(sha256Pattern, "must be 64 lowercase hexadecimal characters"),
	}
	return map[string]schema.Attribute{
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
		"repository": schema.StringAttribute{
			Description: "The repository argument selects the GitHub repository that publishes the release, such as agoodkind/configs.",
			Required:    true,
			Validators: []validator.String{
				stringvalidator.RegexMatches(hostArtifactRepositoryPattern, "must have the form owner/repository"),
			},
		},
		"release_tag": schema.StringAttribute{
			Description: "The release_tag argument selects the release of the archive and the attestation bundle.",
			Required:    true,
			Validators:  nameValidators,
		},
		"asset": schema.StringAttribute{
			Description: "The asset argument selects the archive release asset.",
			Required:    true,
			Validators:  nameValidators,
		},
		"sha256": schema.StringAttribute{
			Description: "The sha256 argument specifies the archive SHA-256 as 64 lowercase hexadecimal characters.",
			Required:    true,
			Validators:  sha256Validators,
		},
		"bundle_asset": schema.StringAttribute{
			Description: "The bundle_asset argument selects the attestation bundle release asset.",
			Required:    true,
			Validators:  nameValidators,
		},
		"bundle_sha256": schema.StringAttribute{
			Description: "The bundle_sha256 argument specifies the attestation bundle SHA-256 as 64 lowercase hexadecimal characters.",
			Required:    true,
			Validators:  sha256Validators,
		},
		"source_revision": schema.StringAttribute{
			Description: "The source_revision argument specifies the 40-character commit that built the archive.",
			Required:    true,
			Validators: []validator.String{
				stringvalidator.RegexMatches(hostArtifactRevisionPattern, "must be 40 lowercase hexadecimal characters"),
			},
		},
	}
}

// hostArtifactObservedAttributes returns the attributes that the provider
// sets from the GET response.
func hostArtifactObservedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"target": schema.StringAttribute{
			Description: "The API uses target, the lowercase hexadecimal SHA-256 of workspace, as {target}.",
			Computed:    true,
		},
		"state": schema.StringAttribute{
			Description: "The node reports the target state as none, committed, or interrupted.",
			Computed:    true,
		},
		"installed_sha256": schema.StringAttribute{
			Description: "The installed_sha256 attribute contains the installed archive SHA-256. " +
				"It is an empty string when the target does not have an installed record.",
			Computed: true,
		},
		"installed_source_revision": schema.StringAttribute{
			Description: "The installed_source_revision attribute contains the installed archive source revision. " +
				"It is an empty string when the target does not have an installed record.",
			Computed: true,
		},
		"observed_sha256": schema.StringAttribute{
			Description: "The node reports observed_sha256 as the digest of the live files and owned directories.",
			Computed:    true,
		},
		"files": schema.MapNestedAttribute{
			Description: "The files map contains the live sha256, mode, owner, and group of each present file, keyed by path. " +
				"The plan proposes an install when these values differ from the pinned manifest.",
			Computed: true,
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"sha256": schema.StringAttribute{Computed: true, Description: "The sha256 attribute contains the SHA-256 of the file content."},
					"mode":   schema.StringAttribute{Computed: true, Description: "The mode attribute contains the permission bits as four octal digits."},
					"owner":  schema.StringAttribute{Computed: true, Description: "The owner attribute contains the owner name."},
					"group":  schema.StringAttribute{Computed: true, Description: "The group attribute contains the group name."},
				},
			},
		},
		"directories": schema.MapNestedAttribute{
			Description: "The directories map contains the live mode, owner, and group of each present owned directory, keyed by path. " +
				"The plan proposes an install when these values differ from the pinned manifest.",
			Computed: true,
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"mode":  schema.StringAttribute{Computed: true, Description: "The mode attribute contains the permission bits as four octal digits."},
					"owner": schema.StringAttribute{Computed: true, Description: "The owner attribute contains the owner name."},
					"group": schema.StringAttribute{Computed: true, Description: "The group attribute contains the group name."},
				},
			},
		},
		"write_ids": schema.MapAttribute{
			Description: "The write_ids map contains the UPID of the task that last wrote each file, keyed by path. " +
				"A file that an install does not change keeps its write_id.",
			Computed:    true,
			ElementType: types.StringType,
		},
	}
}

func (r *hostArtifactResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *hostArtifactResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	node, workspace, found := strings.Cut(req.ID, hostArtifactImportSep)
	if !found || node == "" || workspace == "" {
		resp.Diagnostics.AddError(hostArtifactImportSummary, importIDError(req.ID, hostArtifactImportFormat))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("node"), node)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("workspace"), workspace)...)
}

func (r *hostArtifactResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan hostArtifactModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.Workspace.IsUnknown() {
		target := types.StringValue(transport.HostArtifactTarget(plan.Workspace.ValueString()))
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("target"), target)...)
	}
	pin, known := pinOf(plan)
	if !known {
		return
	}
	manifest, _, err := loadPinnedManifest(ctx, r.data.controllerCAFile, pin)
	if err != nil {
		resp.Diagnostics.AddError("Read host artifact manifest", err.Error())
		return
	}
	desiredFiles := manifest.desiredFiles()
	desiredDirectories := manifest.desiredDirectories()

	var state hostArtifactModel
	hasState := !req.State.Raw.IsNull()
	if hasState {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		// A node or workspace change replaces the resource. The prior state
		// cannot predict values for the new target.
		hasState = plan.Node.Equal(state.Node) && plan.Workspace.Equal(state.Workspace)
	}
	repair := true
	if hasState {
		needed, diagnostics := repairPlanned(ctx, req, state, pin, desiredFiles, desiredDirectories)
		resp.Diagnostics.Append(diagnostics...)
		if resp.Diagnostics.HasError() {
			return
		}
		repair = needed
	}

	files, diagnostics := types.MapValueFrom(ctx, hostArtifactFileType, desiredFiles)
	resp.Diagnostics.Append(diagnostics...)
	directories, diagnostics := types.MapValueFrom(ctx, hostArtifactDirectoryType, desiredDirectories)
	resp.Diagnostics.Append(diagnostics...)
	writeIDs, diagnostics := plannedWriteIDs(ctx, state, hasState, desiredFiles)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	observed := types.StringUnknown()
	if !repair {
		observed = state.ObservedSHA256
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("state"), transport.HostArtifactStateCommitted)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("installed_sha256"), pin.SHA256)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("installed_source_revision"), pin.SourceRevision)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("observed_sha256"), observed)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("files"), files)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("directories"), directories)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_ids"), writeIDs)...)
}

// repairPlanned returns true when the refreshed state differs from the pinned
// manifest, when the last Read found a file or owned directory that differs
// from the installed record, or when observed_sha256 differs from the value
// after the last apply.
func repairPlanned(
	ctx context.Context,
	req resource.ModifyPlanRequest,
	state hostArtifactModel,
	pin hostArtifactPin,
	desiredFiles map[string]hostArtifactFileValue,
	desiredDirectories map[string]hostArtifactDirectoryValue,
) (bool, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	if state.State.ValueString() != transport.HostArtifactStateCommitted ||
		state.InstalledSHA256.ValueString() != pin.SHA256 ||
		state.InstalledSourceRevision.ValueString() != pin.SourceRevision {
		return true, diagnostics
	}
	stateFiles, fileDiagnostics := stateFileValues(ctx, state)
	diagnostics.Append(fileDiagnostics...)
	stateDirectories, directoryDiagnostics := stateDirectoryValues(ctx, state)
	diagnostics.Append(directoryDiagnostics...)
	if diagnostics.HasError() {
		return true, diagnostics
	}
	if len(fileDifferences(desiredFiles, stateFiles)) > 0 ||
		len(directoryDifferences(desiredDirectories, stateDirectories)) > 0 {
		return true, diagnostics
	}

	mismatch, getDiagnostics := req.Private.GetKey(ctx, hostArtifactMismatchEntry)
	diagnostics.Append(getDiagnostics...)
	applied, getDiagnostics := req.Private.GetKey(ctx, hostArtifactAppliedObservedEntry)
	diagnostics.Append(getDiagnostics...)
	if diagnostics.HasError() {
		return true, diagnostics
	}
	if decodeMismatch(mismatch) {
		return true, diagnostics
	}
	appliedObserved := decodeAppliedHash(applied)
	return appliedObserved != "" && appliedObserved != state.ObservedSHA256.ValueString(), diagnostics
}

// plannedWriteIDs keeps the write_id of each file that has the manifest
// value in a committed target. An install keeps the write_id of such a file.
// Every other write_id is unknown until apply.
func plannedWriteIDs(
	ctx context.Context,
	state hostArtifactModel,
	hasState bool,
	desiredFiles map[string]hostArtifactFileValue,
) (types.Map, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	stateFiles := map[string]hostArtifactFileValue{}
	stateWriteIDs := map[string]string{}
	committed := hasState && state.State.ValueString() == transport.HostArtifactStateCommitted
	if committed {
		values, fileDiagnostics := stateFileValues(ctx, state)
		diagnostics.Append(fileDiagnostics...)
		stateFiles = values
		if !state.WriteIDs.IsNull() && !state.WriteIDs.IsUnknown() {
			diagnostics.Append(state.WriteIDs.ElementsAs(ctx, &stateWriteIDs, false)...)
		}
		if diagnostics.HasError() {
			return types.MapUnknown(types.StringType), diagnostics
		}
	}
	elements := make(map[string]attr.Value, len(desiredFiles))
	for path, want := range desiredFiles {
		got, found := stateFiles[path]
		writeID := stateWriteIDs[path]
		if found && got == want && writeID != "" {
			elements[path] = types.StringValue(writeID)
			continue
		}
		elements[path] = types.StringUnknown()
	}
	writeIDs, mapDiagnostics := types.MapValue(types.StringType, elements)
	diagnostics.Append(mapDiagnostics...)
	return writeIDs, diagnostics
}

func stateFileValues(ctx context.Context, state hostArtifactModel) (map[string]hostArtifactFileValue, diag.Diagnostics) {
	values := map[string]hostArtifactFileValue{}
	if state.Files.IsNull() || state.Files.IsUnknown() {
		return values, nil
	}
	diagnostics := state.Files.ElementsAs(ctx, &values, false)
	return values, diagnostics
}

func stateDirectoryValues(ctx context.Context, state hostArtifactModel) (map[string]hostArtifactDirectoryValue, diag.Diagnostics) {
	values := map[string]hostArtifactDirectoryValue{}
	if state.Directories.IsNull() || state.Directories.IsUnknown() {
		return values, nil
	}
	diagnostics := state.Directories.ElementsAs(ctx, &values, false)
	return values, diagnostics
}

func (r *hostArtifactResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostArtifactModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	current, diagnostics := r.apply(ctx, plan)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(setObservedState(ctx, &plan, current)...)
	resp.Diagnostics.Append(storeAppliedObservation(ctx, resp.Private, current)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostArtifactResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hostArtifactModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	current, diagnostics := r.apply(ctx, plan)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(setObservedState(ctx, &plan, current)...)
	resp.Diagnostics.Append(storeAppliedObservation(ctx, resp.Private, current)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read calls only GET and stores the live values that the node reports.
func (r *hostArtifactResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostArtifactModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := transport.HostArtifactTarget(state.Workspace.ValueString())
	current, err := r.data.client.GetHostArtifact(ctx, state.Node.ValueString(), target)
	if err != nil {
		resp.Diagnostics.AddError("Read host artifact", err.Error())
		return
	}
	if current.Workspace != state.Workspace.ValueString() {
		resp.Diagnostics.AddError("Read host artifact", fmt.Sprintf(
			"the node reports workspace %q for target %s, and the resource sets workspace %q",
			current.Workspace, target, state.Workspace.ValueString(),
		))
		return
	}
	resp.Diagnostics.Append(setObservedState(ctx, &state, current)...)
	mismatch, err := json.Marshal(hostArtifactMismatchDocument{Mismatch: reportsMismatch(current)})
	if err != nil {
		slog.ErrorContext(ctx, "encode host artifact mismatch failed", "err", err)
		resp.Diagnostics.AddError("Store host artifact observation", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, hostArtifactMismatchEntry, mismatch)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete calls only GET and fails while the installed record lists files.
// Installing an archive with an empty file list deletes the target files.
func (r *hostArtifactResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostArtifactModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := transport.HostArtifactTarget(state.Workspace.ValueString())
	current, err := r.data.client.GetHostArtifact(ctx, state.Node.ValueString(), target)
	if err != nil {
		resp.Diagnostics.AddError("Destroy host artifact", err.Error())
		return
	}
	if current.Installed != nil && len(current.Files) > 0 {
		resp.Diagnostics.AddError("Destroy host artifact", fmt.Sprintf(
			"the installed record of workspace %q on node %q lists %d files. "+
				"Install an archive with an empty file list before destroying the resource.",
			state.Workspace.ValueString(), state.Node.ValueString(), len(current.Files),
		))
	}
}

// apply reads the target and installs the pinned archive if the installed
// archive or its files and owned directories differ. It returns the GET
// response after any install.
func (r *hostArtifactResource) apply(ctx context.Context, plan hostArtifactModel) (transport.HostArtifactState, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	pin, _ := pinOf(plan)
	node := plan.Node.ValueString()
	target := transport.HostArtifactTarget(pin.Workspace)
	manifest, archivePath, err := loadPinnedManifest(ctx, r.data.controllerCAFile, pin)
	if err != nil {
		diagnostics.AddError("Read host artifact manifest", err.Error())
		return transport.HostArtifactState{}, diagnostics
	}
	current, err := r.data.client.GetHostArtifact(ctx, node, target)
	if err != nil {
		diagnostics.AddError("Read host artifact", err.Error())
		return transport.HostArtifactState{}, diagnostics
	}
	if len(liveDifferences(manifest, pin, current)) == 0 {
		return current, diagnostics
	}

	diagnostics.Append(r.install(ctx, node, target, pin, archivePath)...)
	if diagnostics.HasError() {
		return transport.HostArtifactState{}, diagnostics
	}
	current, err = r.data.client.GetHostArtifact(ctx, node, target)
	if err != nil {
		diagnostics.AddError("Read host artifact", err.Error())
		return transport.HostArtifactState{}, diagnostics
	}
	if differences := liveDifferences(manifest, pin, current); len(differences) > 0 {
		diagnostics.AddError("Install host artifact", fmt.Sprintf(
			"after the install of %s on node %q, %s", pin.Asset, node, joinDifferences(differences),
		))
	}
	return current, diagnostics
}

// install stages the archive and the attestation bundle, starts the
// hostartifact task, and waits for it.
func (r *hostArtifactResource) install(
	ctx context.Context,
	node string,
	target string,
	pin hostArtifactPin,
	archivePath string,
) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	bundleURL := releaseAssetURL(pin.Repository, pin.ReleaseTag, pin.BundleAsset)
	bundlePath, err := cachedArchive(ctx, r.data.controllerCAFile, bundleURL, pin.BundleSHA256, requestHeaders{})
	if err != nil {
		diagnostics.AddError("Download host artifact bundle", err.Error())
		return diagnostics
	}
	archiveStage, stageDiagnostics := r.stageFile(ctx, node, target, transport.HostArtifactKindArchive, archivePath)
	diagnostics.Append(stageDiagnostics...)
	if diagnostics.HasError() {
		return diagnostics
	}
	bundleStage, stageDiagnostics := r.stageFile(ctx, node, target, transport.HostArtifactKindBundle, bundlePath)
	diagnostics.Append(stageDiagnostics...)
	if diagnostics.HasError() {
		return diagnostics
	}
	upid, err := r.data.client.InstallHostArtifact(ctx, node, target, transport.HostArtifactInstall{
		ReleaseTag:     pin.ReleaseTag,
		Asset:          pin.Asset,
		SHA256:         pin.SHA256,
		SourceRevision: pin.SourceRevision,
		ArchiveStage:   archiveStage,
		BundleStage:    bundleStage,
	})
	if err != nil {
		diagnostics.AddError("Install host artifact", err.Error())
		return diagnostics
	}
	if err := r.data.client.WaitTaskWithin(ctx, node, upid, hostArtifactTaskTimeout); err != nil {
		diagnostics.AddError("Install host artifact", err.Error())
	}
	return diagnostics
}

func (r *hostArtifactResource) stageFile(
	ctx context.Context,
	node string,
	target string,
	kind string,
	filePath string,
) (string, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	file, err := os.Open(filePath)
	if err != nil {
		diagnostics.AddError("Stage host artifact", fmt.Sprintf("open the cached %s: %s", kind, err))
		return "", diagnostics
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		diagnostics.AddError("Stage host artifact", fmt.Sprintf("read the size of the cached %s: %s", kind, err))
		return "", diagnostics
	}
	stage, err := r.data.client.StageHostArtifact(ctx, node, target, kind, file, info.Size())
	if err != nil {
		diagnostics.AddError("Stage host artifact", err.Error())
	}
	return stage, diagnostics
}

// pinOf returns the pin of the model and whether every pin attribute is known.
func pinOf(model hostArtifactModel) (hostArtifactPin, bool) {
	values := []types.String{
		model.Node, model.Workspace, model.Repository, model.ReleaseTag, model.Asset,
		model.SHA256, model.BundleAsset, model.BundleSHA256, model.SourceRevision,
	}
	for _, value := range values {
		if value.IsUnknown() || value.IsNull() {
			return hostArtifactPin{}, false
		}
	}
	return hostArtifactPin{
		Workspace:      model.Workspace.ValueString(),
		Repository:     model.Repository.ValueString(),
		ReleaseTag:     model.ReleaseTag.ValueString(),
		Asset:          model.Asset.ValueString(),
		SHA256:         model.SHA256.ValueString(),
		BundleAsset:    model.BundleAsset.ValueString(),
		BundleSHA256:   model.BundleSHA256.ValueString(),
		SourceRevision: model.SourceRevision.ValueString(),
	}, true
}

// setObservedState copies the GET response into the computed attributes.
func setObservedState(ctx context.Context, model *hostArtifactModel, current transport.HostArtifactState) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	files, fileDiagnostics := types.MapValueFrom(ctx, hostArtifactFileType, observedFiles(current))
	diagnostics.Append(fileDiagnostics...)
	directories, directoryDiagnostics := types.MapValueFrom(ctx, hostArtifactDirectoryType, observedDirectories(current))
	diagnostics.Append(directoryDiagnostics...)
	writeIDs, writeIDDiagnostics := types.MapValueFrom(ctx, types.StringType, observedWriteIDs(current))
	diagnostics.Append(writeIDDiagnostics...)
	if diagnostics.HasError() {
		return diagnostics
	}
	installedSHA256 := ""
	installedSourceRevision := ""
	if current.Installed != nil {
		installedSHA256 = current.Installed.SHA256
		installedSourceRevision = current.Installed.SourceRevision
	}
	model.Target = types.StringValue(transport.HostArtifactTarget(model.Workspace.ValueString()))
	model.State = types.StringValue(current.State)
	model.InstalledSHA256 = types.StringValue(installedSHA256)
	model.InstalledSourceRevision = types.StringValue(installedSourceRevision)
	model.ObservedSHA256 = types.StringValue(current.ObservedSHA256)
	model.Files = files
	model.Directories = directories
	model.WriteIDs = writeIDs
	return diagnostics
}

// privateSetter is the private state of a Create or Update response.
type privateSetter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// storeAppliedObservation stores observed_sha256 after an apply and clears the
// mismatch that the last Read stored.
func storeAppliedObservation(ctx context.Context, private privateSetter, current transport.HostArtifactState) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	applied, err := encodeAppliedHash(current.ObservedSHA256)
	if err != nil {
		diagnostics.AddError("Store host artifact observation", err.Error())
		return diagnostics
	}
	mismatch, err := json.Marshal(hostArtifactMismatchDocument{Mismatch: false})
	if err != nil {
		slog.ErrorContext(ctx, "encode host artifact mismatch failed", "err", err)
		diagnostics.AddError("Store host artifact observation", err.Error())
		return diagnostics
	}
	diagnostics.Append(private.SetKey(ctx, hostArtifactAppliedObservedEntry, applied)...)
	diagnostics.Append(private.SetKey(ctx, hostArtifactMismatchEntry, mismatch)...)
	return diagnostics
}

// decodeMismatch returns false for a missing or unreadable document.
func decodeMismatch(document []byte) bool {
	var decoded hostArtifactMismatchDocument
	if err := json.Unmarshal(document, &decoded); err != nil {
		return false
	}
	return decoded.Mismatch
}
