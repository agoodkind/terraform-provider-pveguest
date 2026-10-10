package provider

import (
	"context"
	"fmt"
	"path"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

// The exec timeout bounds the guest download and the guest extraction. The
// transport limit is 3600 seconds.
const downloadTimeoutSeconds = 600

var httpsURLPattern = regexp.MustCompile(`^https://[^\s\x00]+$`)

type downloadResource struct {
	data *providerData
}

type downloadModel struct {
	Node          types.String `tfsdk:"node"`
	VMID          types.Int64  `tfsdk:"vmid"`
	Kind          types.String `tfsdk:"kind"`
	URL           types.String `tfsdk:"url"`
	SHA256        types.String `tfsdk:"sha256"`
	Fetch         types.String `tfsdk:"fetch"`
	ArchiveMember types.String `tfsdk:"archive_member"`
	Path          types.String `tfsdk:"path"`
	Mode          types.String `tfsdk:"mode"`
	Owner         types.String `tfsdk:"owner"`
	Group         types.String `tfsdk:"group"`
	FileSHA256    types.String `tfsdk:"file_sha256"`
	WriteID       types.String `tfsdk:"write_id"`
}

func newDownloadResource() resource.Resource {
	return &downloadResource{}
}

func (r *downloadResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_download"
}

func (r *downloadResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A file that is downloaded over HTTPS, optionally extracted from a .tar.gz archive, " +
			"and that must match a SHA-256 hash.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"url": schema.StringAttribute{
				Required:    true,
				Description: "HTTPS URL of the object. The guest downloads it with curl, or the controller downloads it when fetch is controller.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(httpsURLPattern, "must be an https:// URL"),
				},
			},
			"sha256": schema.StringAttribute{
				Required: true,
				Description: "Expected SHA-256 hash of the object at url, as 64 lowercase hexadecimal characters. " +
					"With archive_member, the hash is the hash of the archive.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(sha256Pattern, "must be 64 lowercase hexadecimal characters"),
				},
			},
			"fetch": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(fetchGuest),
				Description: "Where the download runs. guest, the default, runs curl in the guest. controller " +
					"downloads on the machine that runs OpenTofu, caches the object, and sends the file to the " +
					"guest through the exec API. controller requires kind lxc.",
				Validators: []validator.String{
					stringvalidator.OneOf(fetchGuest, fetchController),
				},
			},
			"archive_member": schema.StringAttribute{
				Optional: true,
				Description: "Relative path of one file inside a .tar.gz archive at url. " +
					"The installed file is that member.",
			},
			"path": schema.StringAttribute{
				Required:    true,
				Description: "Absolute path of the file. An apply creates missing parent directories with mode 0755 and owner root:root.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(absolutePathPattern, "must be an absolute path that ends in a file name"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"mode": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFileMode),
				Description: "Permission bits as four octal digits. The default is 0644.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(fileModePattern, "must be four octal digits, for example 0644"),
				},
			},
			"owner": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFileOwner),
				Description: "Owner user name. The default is root.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(accountNamePattern, "must be a user name"),
				},
			},
			"group": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(defaultFileGroup),
				Description: "Owner group name. The default is root.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(accountNamePattern, "must be a group name"),
				},
			},
			"file_sha256": schema.StringAttribute{
				Computed: true,
				Description: "SHA-256 hash of the installed file in the guest, in hexadecimal. " +
					"A difference between the guest file and this value plans a rewrite.",
			},
			"write_id": schema.StringAttribute{
				Computed: true,
				Description: "Random identifier that changes each time an apply writes the file, mode, owner, or group. " +
					"A rewrite after drift produces the same file_sha256 and a new write_id.",
			},
		}),
	}
}

func (r *downloadResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *downloadResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config downloadModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !config.ArchiveMember.IsNull() && !config.ArchiveMember.IsUnknown() {
		if err := validateArchiveMember(config.ArchiveMember.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(fwpath.Root("archive_member"), "Invalid archive_member", err.Error())
		}
	}
	controllerFetch := config.Fetch.ValueString() == fetchController
	if controllerFetch && config.Kind.ValueString() == string(transport.KindQEMU) {
		resp.Diagnostics.AddAttributeError(
			fwpath.Root("fetch"),
			"Unsupported fetch for a VM",
			"fetch = \"controller\" sends binary data through the exec API, which the QEMU guest agent API does not accept. Use kind = \"lxc\" or fetch = \"guest\".",
		)
	}
}

func (r *downloadResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	var plan downloadModel
	var state downloadModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	appliedHash, diagnostics := req.Private.GetKey(ctx, appliedHashEntry)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Read stores the hash of the guest file in file_sha256, and the private
	// state stores the hash that the last apply wrote.
	sourceChanged := !plan.URL.Equal(state.URL) || !plan.SHA256.Equal(state.SHA256) ||
		!plan.ArchiveMember.Equal(state.ArchiveMember) || !plan.Fetch.Equal(state.Fetch)
	drifted := decodeAppliedHash(appliedHash) != state.FileSHA256.ValueString()
	if sourceChanged || drifted {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("file_sha256"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_id"), types.StringUnknown())...)
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("file_sha256"), state.FileSHA256)...)
	if downloadAttributesChanged(plan, state) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_id"), types.StringUnknown())...)
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_id"), state.WriteID)...)
}

func downloadAttributesChanged(plan downloadModel, state downloadModel) bool {
	return !plan.Mode.Equal(state.Mode) || !plan.Owner.Equal(state.Owner) || !plan.Group.Equal(state.Group)
}

func (r *downloadResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan downloadModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	fileHash, err := r.install(ctx, guest, plan)
	if err != nil {
		resp.Diagnostics.AddError("Download file", err.Error())
		return
	}
	plan.FileSHA256 = types.StringValue(fileHash)
	writeID, err := newWriteID()
	if err != nil {
		resp.Diagnostics.AddError("Download file", err.Error())
		return
	}
	plan.WriteID = types.StringValue(writeID)
	appliedHash, err := encodeAppliedHash(fileHash)
	if err != nil {
		resp.Diagnostics.AddError("Store applied hash", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedHashEntry, appliedHash)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *downloadResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state downloadModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	status, found, err := readFileStatus(ctx, r.data.client, guest, state.Path.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read downloaded file", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	state.Mode = types.StringValue(status.Mode)
	state.Owner = types.StringValue(status.Owner)
	state.Group = types.StringValue(status.Group)
	state.FileSHA256 = types.StringValue(status.SHA256)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *downloadResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan downloadModel
	var state downloadModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	switch {
	case plan.FileSHA256.IsUnknown():
		fileHash, err := r.install(ctx, guest, plan)
		if err != nil {
			resp.Diagnostics.AddError("Download file", err.Error())
			return
		}
		plan.FileSHA256 = types.StringValue(fileHash)
	case downloadAttributesChanged(plan, state):
		// Only mode, owner, or group differ. The file in the guest already has
		// the expected hash.
		err := setGuestAttributes(
			ctx, r.data.client, guest, plan.Mode, plan.Owner, plan.Group, plan.Path.ValueString(),
		)
		if err != nil {
			resp.Diagnostics.AddError("Update file attributes", err.Error())
			return
		}
	}

	if plan.WriteID.IsUnknown() {
		writeID, err := newWriteID()
		if err != nil {
			resp.Diagnostics.AddError("Download file", err.Error())
			return
		}
		plan.WriteID = types.StringValue(writeID)
	}
	appliedHash, err := encodeAppliedHash(plan.FileSHA256.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Store applied hash", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, appliedHashEntry, appliedHash)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *downloadResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state downloadModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	err := runChecked(ctx, r.data.client, guest, guestCommand("rm", "-f", "--", state.Path.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Delete downloaded file", err.Error())
	}
}

// install writes the file into a temporary file in the directory of the
// destination, sets mode and owner, and renames the temporary file after the
// hash matches. A rename inside one directory is atomic. The result is the
// SHA-256 hash of the installed file.
func (r *downloadResource) install(ctx context.Context, guest transport.Guest, plan downloadModel) (string, error) {
	destination := plan.Path.ValueString()
	finalTemporaryPath, err := temporaryPathFor(destination)
	if err != nil {
		return "", err
	}
	intermediatePath, err := temporaryPathFor(destination)
	if err != nil {
		return "", err
	}

	// The umask gives every new directory in the path mode 0755.
	directoryCommand := guestCommand("sh", "-c", `umask 022 && mkdir -p -- "$1"`, "sh", path.Dir(destination))
	if err := runChecked(ctx, r.data.client, guest, directoryCommand); err != nil {
		return "", err
	}

	// Both temporary files are removed after a failure and the intermediate
	// file is removed after a success.
	defer r.removeTemporaryFile(ctx, guest, intermediatePath)

	var fileHash string
	if plan.Fetch.ValueString() == fetchController {
		fileHash, err = r.stageFromController(ctx, guest, plan, intermediatePath, finalTemporaryPath)
	} else {
		fileHash, err = r.stageFromGuest(ctx, guest, plan, intermediatePath, finalTemporaryPath)
	}
	if err != nil {
		r.removeTemporaryFile(ctx, guest, finalTemporaryPath)
		return "", err
	}

	err = setGuestAttributes(ctx, r.data.client, guest, plan.Mode, plan.Owner, plan.Group, finalTemporaryPath)
	if err != nil {
		r.removeTemporaryFile(ctx, guest, finalTemporaryPath)
		return "", err
	}
	// -T makes mv fail on a directory at the destination instead of moving the
	// temporary file into it.
	moveCommand := guestCommand("mv", "-fT", "--", finalTemporaryPath, destination)
	if err := runChecked(ctx, r.data.client, guest, moveCommand); err != nil {
		r.removeTemporaryFile(ctx, guest, finalTemporaryPath)
		return "", err
	}
	return fileHash, nil
}

// stageFromGuest runs curl in the guest. With archive_member, curl writes the
// archive to the intermediate file and tar extracts the member into the final
// temporary file.
func (r *downloadResource) stageFromGuest(
	ctx context.Context,
	guest transport.Guest,
	plan downloadModel,
	intermediatePath string,
	finalTemporaryPath string,
) (string, error) {
	hasMember := !plan.ArchiveMember.IsNull()
	downloadPath := finalTemporaryPath
	if hasMember {
		downloadPath = intermediatePath
	}

	// The umask makes the temporary file unreadable for other users until
	// chmod sets the declared mode.
	downloadCommand := guestCommand(
		"sh", "-c",
		`umask 077 && cd -- "$1" && exec curl -fsSL --proto =https -o "$2" -- "$3"`,
		"sh", path.Dir(plan.Path.ValueString()), downloadPath, plan.URL.ValueString(),
	)
	downloadCommand.TimeoutSeconds = downloadTimeoutSeconds
	if err := runChecked(ctx, r.data.client, guest, downloadCommand); err != nil {
		return "", err
	}

	receivedHash, found, err := readGuestSHA256(ctx, r.data.client, guest, downloadPath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s: the temporary file %s is missing after the download of %s",
			guest, downloadPath, plan.URL.ValueString())
	}
	expectedHash := plan.SHA256.ValueString()
	if receivedHash != expectedHash {
		return "", fmt.Errorf(
			"%s: the download of %s has sha256 %s, but the configuration expects %s",
			guest, plan.URL.ValueString(), receivedHash, expectedHash,
		)
	}
	if !hasMember {
		return receivedHash, nil
	}

	extractCommand := guestCommand(
		"sh", "-c", `umask 077 && tar -xzf "$1" -O -- "$3" > "$2"`,
		"sh", intermediatePath, finalTemporaryPath, plan.ArchiveMember.ValueString(),
	)
	extractCommand.TimeoutSeconds = downloadTimeoutSeconds
	if err := runChecked(ctx, r.data.client, guest, extractCommand); err != nil {
		return "", err
	}
	fileHash, found, err := readGuestSHA256(ctx, r.data.client, guest, finalTemporaryPath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s: the temporary file %s is missing after the extraction of %s",
			guest, finalTemporaryPath, plan.ArchiveMember.ValueString())
	}
	return fileHash, nil
}

// stageFromController downloads and extracts on the controller, compresses the
// payload, writes the compressed stream to the intermediate file, and runs
// gzip -dc in the guest into the final temporary file. The hash of the guest
// file must equal the hash that the controller computed.
func (r *downloadResource) stageFromController(
	ctx context.Context,
	guest transport.Guest,
	plan downloadModel,
	intermediatePath string,
	finalTemporaryPath string,
) (string, error) {
	member := ""
	if !plan.ArchiveMember.IsNull() {
		member = plan.ArchiveMember.ValueString()
	}
	payload, err := fetchControllerPayload(
		ctx, r.data.controllerCAFile, plan.URL.ValueString(), plan.SHA256.ValueString(), member,
	)
	if err != nil {
		return "", err
	}
	compressed, err := gzipContent(payload.Content)
	if err != nil {
		return "", err
	}
	if err := writeGuestContent(ctx, r.data.client, guest, intermediatePath, compressed); err != nil {
		return "", err
	}

	decompressCommand := guestCommand(
		"sh", "-c", `umask 077 && gzip -dc -- "$1" > "$2"`, "sh", intermediatePath, finalTemporaryPath,
	)
	decompressCommand.TimeoutSeconds = downloadTimeoutSeconds
	if err := runChecked(ctx, r.data.client, guest, decompressCommand); err != nil {
		return "", err
	}
	guestHash, found, err := readGuestSHA256(ctx, r.data.client, guest, finalTemporaryPath)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s: the temporary file %s is missing after the decompression",
			guest, finalTemporaryPath)
	}
	if guestHash != payload.SHA256 {
		return "", fmt.Errorf(
			"%s: the transferred file has sha256 %s, but the controller sent sha256 %s",
			guest, guestHash, payload.SHA256,
		)
	}
	return guestHash, nil
}

// removeTemporaryFile runs after another step failed. The caller reports
// that failure and ignores a failed removal.
func (r *downloadResource) removeTemporaryFile(ctx context.Context, guest transport.Guest, temporaryPath string) {
	_, _ = r.data.client.Run(ctx, guest, guestCommand("rm", "-f", "--", temporaryPath))
}
