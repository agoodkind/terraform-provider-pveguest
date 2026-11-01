package provider

import (
	"context"
	"fmt"
	"path"
	"strings"

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
	notADirectoryMessage    = "Not a directory"
	fileAbsentImportSummary = "Cannot import pveguest_file_absent"

	invalidPathSummary    = "Path must be a clean absolute path"
	invalidPathDescriptor = "must start with / and must not end with / or contain //, . or .. path segments"

	// stat prints these file types for regular files and symbolic links.
	fileTypeRegular      = "regular file"
	fileTypeRegularEmpty = "regular empty file"
	fileTypeSymbolicLink = "symbolic link"
	fileTypeDirectory    = "directory"
)

type fileAbsentResource struct {
	data *providerData
}

type fileAbsentModel struct {
	Node types.String `tfsdk:"node"`
	VMID types.Int64  `tfsdk:"vmid"`
	Kind types.String `tfsdk:"kind"`
	Path types.String `tfsdk:"path"`
}

func newFileAbsentResource() resource.Resource {
	return &fileAbsentResource{}
}

// pathValidator requires an absolute path without a trailing slash, repeated
// slashes, or . or .. segments.
type pathValidator struct{}

func (pathValidator) Description(_ context.Context) string {
	return invalidPathDescriptor
}

func (v pathValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (pathValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	if absolutePathPattern.MatchString(value) && path.Clean(value) == value {
		return
	}
	resp.Diagnostics.AddAttributeError(
		req.Path,
		invalidPathSummary,
		fmt.Sprintf("path %s; the rejected value is %q", invalidPathDescriptor, value),
	)
}

func (r *fileAbsentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file_absent"
}

func (r *fileAbsentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "pveguest_file_absent requires a path inside a guest not to exist. " +
			"Create and Update delete a regular file or symbolic link at the path. " +
			"When the path identifies a directory, the apply fails.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				Description: "The path must not exist. It must start with / and must not end with / " +
					"or contain //, . or .. path segments. Changing the path replaces the resource.",
				Validators:    []validator.String{pathValidator{}},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		}),
	}
}

func (r *fileAbsentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *fileAbsentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parsed, isValid := splitGuestImportID(req.ID)
	if !isValid {
		resp.Diagnostics.AddError(fileAbsentImportSummary, importIDError(req.ID, fileImportFormat))
		return
	}

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	attributes := schemaResp.Schema.Attributes
	resp.Diagnostics.Append(validateImportedString(ctx, fileAbsentImportSummary, attributes, "kind", parsed.Kind)...)
	resp.Diagnostics.Append(validateImportedString(ctx, fileAbsentImportSummary, attributes, "path", parsed.Rest)...)
	if resp.Diagnostics.HasError() {
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("node"), parsed.Node)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("vmid"), parsed.VMID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("kind"), parsed.Kind)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("path"), parsed.Rest)...)
}

func (r *fileAbsentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileAbsentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	if err := r.removePath(ctx, guest, plan.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Cannot remove file", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// When the path exists, Read removes the resource from state. The next plan
// proposes recreating the resource; applying it tries to delete the path.
func (r *fileAbsentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileAbsentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	_, exists, err := lstatType(ctx, r.data.client, guest, state.Path.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot read pveguest_file_absent", err.Error())
		return
	}
	if exists {
		resp.State.RemoveResource(ctx)
	}
}

func (r *fileAbsentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan fileAbsentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	if err := r.removePath(ctx, guest, plan.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Cannot remove file", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes only the state entry and does not change the guest.
func (r *fileAbsentResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

// removePath deletes a regular file or symbolic link and verifies that the
// path does not exist.
func (r *fileAbsentResource) removePath(ctx context.Context, guest transport.Guest, filePath string) error {
	fileType, exists, err := lstatType(ctx, r.data.client, guest, filePath)
	if err != nil {
		return err
	}
	if exists {
		switch fileType {
		case fileTypeRegular, fileTypeRegularEmpty, fileTypeSymbolicLink:
			removeCommand := guestCommand("rm", "-f", "--", filePath)
			if err := runChecked(ctx, r.data.client, guest, removeCommand); err != nil {
				return err
			}
		case fileTypeDirectory:
			return fmt.Errorf("on guest %s, %s is a directory; pveguest_file_absent removes only a regular file or a symbolic link", guest, filePath)
		default:
			return fmt.Errorf("on guest %s, %s has file type %s; pveguest_file_absent removes only a regular file or a symbolic link", guest, filePath, fileType)
		}
	}

	_, stillExists, err := lstatType(ctx, r.data.client, guest, filePath)
	if err != nil {
		return err
	}
	if stillExists {
		return fmt.Errorf("on guest %s, %s still exists after removal", guest, filePath)
	}
	return nil
}

// lstatType returns the file type from stat without following symbolic links.
// It returns false when the path does not exist.
func lstatType(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	filePath string,
) (string, bool, error) {
	command := guestCommand("stat", "-c", "%F", "--", filePath)
	result, err := runReadOnlyGuest(ctx, client, guest, command)
	if err != nil {
		return "", false, err
	}
	if result.ExitCode != 0 {
		stderr := string(result.Stderr)
		if strings.Contains(stderr, missingFileMessage) || strings.Contains(stderr, notADirectoryMessage) {
			return "", false, nil
		}
		return "", false, commandFailure(guest, command, result)
	}
	return firstLine(result.Stdout), true, nil
}
