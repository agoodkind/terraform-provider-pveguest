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

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

// The exec timeout bounds the guest download. The transport limit is 3600
// seconds.
const downloadTimeoutSeconds = 600

var httpsURLPattern = regexp.MustCompile(`^https://[^\s\x00]+$`)

type downloadResource struct {
	data *providerData
}

type downloadModel struct {
	Node    types.String `tfsdk:"node"`
	VMID    types.Int64  `tfsdk:"vmid"`
	Kind    types.String `tfsdk:"kind"`
	URL     types.String `tfsdk:"url"`
	SHA256  types.String `tfsdk:"sha256"`
	Path    types.String `tfsdk:"path"`
	Mode    types.String `tfsdk:"mode"`
	Owner   types.String `tfsdk:"owner"`
	Group   types.String `tfsdk:"group"`
	WriteID types.String `tfsdk:"write_id"`
}

func newDownloadResource() resource.Resource {
	return &downloadResource{}
}

func (r *downloadResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_download"
}

func (r *downloadResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A file that the guest downloads over HTTPS and that must match a SHA-256 hash.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"url": schema.StringAttribute{
				Required:    true,
				Description: "HTTPS URL that the guest downloads with curl.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(httpsURLPattern, "must be an https:// URL"),
				},
			},
			"sha256": schema.StringAttribute{
				Required:    true,
				Description: "Expected SHA-256 hash of the file, as 64 lowercase hexadecimal characters.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(sha256Pattern, "must be 64 lowercase hexadecimal characters"),
				},
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
			"write_id": schema.StringAttribute{
				Computed: true,
				Description: "Random identifier that changes each time an apply writes the file, mode, owner, or group. " +
					"A rewrite after drift produces the same sha256 and a new write_id.",
			},
		}),
	}
}

func (r *downloadResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
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
	if !downloadWritePlanned(plan, state) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("write_id"), state.WriteID)...)
	}
}

// Read writes the hash of the guest file to sha256, and the plan compares that
// hash with the configuration. A changed url with an unchanged hash does not
// rewrite the file.
func downloadWritePlanned(plan downloadModel, state downloadModel) bool {
	sameContent := plan.SHA256.Equal(state.SHA256)
	sameAttributes := plan.Mode.Equal(state.Mode) && plan.Owner.Equal(state.Owner) && plan.Group.Equal(state.Group)
	return !sameContent || !sameAttributes
}

func (r *downloadResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan downloadModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	if err := r.download(ctx, guest, plan); err != nil {
		resp.Diagnostics.AddError("Download file", err.Error())
		return
	}
	writeID, err := newWriteID()
	if err != nil {
		resp.Diagnostics.AddError("Download file", err.Error())
		return
	}
	plan.WriteID = types.StringValue(writeID)
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
	state.SHA256 = types.StringValue(status.SHA256)
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
	case !plan.SHA256.Equal(state.SHA256):
		if err := r.download(ctx, guest, plan); err != nil {
			resp.Diagnostics.AddError("Download file", err.Error())
			return
		}
	case downloadWritePlanned(plan, state):
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

// download fetches the URL into a temporary file in the directory of the
// destination and renames the file after the hash matches. A rename inside
// one directory is atomic.
func (r *downloadResource) download(ctx context.Context, guest transport.Guest, plan downloadModel) error {
	destination := plan.Path.ValueString()
	temporaryPath, err := temporaryPathFor(destination)
	if err != nil {
		return err
	}

	// The umask gives every new directory in the path mode 0755.
	directoryCommand := guestCommand("sh", "-c", `umask 022 && mkdir -p -- "$1"`, "sh", path.Dir(destination))
	if err := runChecked(ctx, r.data.client, guest, directoryCommand); err != nil {
		return err
	}

	// The umask makes the temporary file unreadable for other users until
	// chmod sets the declared mode.
	downloadCommand := guestCommand(
		"sh", "-c",
		`umask 077 && cd -- "$1" && exec curl -fsSL --proto =https -o "$2" -- "$3"`,
		"sh", path.Dir(destination), temporaryPath, plan.URL.ValueString(),
	)
	downloadCommand.TimeoutSeconds = downloadTimeoutSeconds
	if err := runChecked(ctx, r.data.client, guest, downloadCommand); err != nil {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return err
	}

	receivedHash, found, err := readGuestSHA256(ctx, r.data.client, guest, temporaryPath)
	if err != nil {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return err
	}
	if !found {
		return fmt.Errorf("%s: the temporary file %s is missing after the download of %s",
			guest, temporaryPath, plan.URL.ValueString())
	}
	expectedHash := plan.SHA256.ValueString()
	if receivedHash != expectedHash {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return fmt.Errorf(
			"%s: the download of %s has sha256 %s, but the configuration expects %s",
			guest, plan.URL.ValueString(), receivedHash, expectedHash,
		)
	}

	err = setGuestAttributes(ctx, r.data.client, guest, plan.Mode, plan.Owner, plan.Group, temporaryPath)
	if err != nil {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return err
	}
	// -T makes mv fail on a directory at the destination instead of moving the
	// temporary file into it.
	moveCommand := guestCommand("mv", "-fT", "--", temporaryPath, destination)
	if err := runChecked(ctx, r.data.client, guest, moveCommand); err != nil {
		r.removeTemporaryFile(ctx, guest, temporaryPath)
		return err
	}
	return nil
}

// removeTemporaryFile runs after another step failed. The caller reports
// that failure and ignores a failed removal.
func (r *downloadResource) removeTemporaryFile(ctx context.Context, guest transport.Guest, temporaryPath string) {
	_, _ = r.data.client.Run(ctx, guest, guestCommand("rm", "-f", "--", temporaryPath))
}
