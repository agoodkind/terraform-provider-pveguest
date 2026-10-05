package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// readlink -v prints this text for a path that exists and is not a link.
const notALinkMessage = "Invalid argument"

type linkResource struct {
	data *providerData
}

type linkModel struct {
	Node   types.String `tfsdk:"node"`
	VMID   types.Int64  `tfsdk:"vmid"`
	Kind   types.String `tfsdk:"kind"`
	Path   types.String `tfsdk:"path"`
	Target types.String `tfsdk:"target"`
}

func newLinkResource() resource.Resource {
	return &linkResource{}
}

func (r *linkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_link"
}

func (r *linkResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A symbolic link inside a guest.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required:    true,
				Description: "Absolute path of the link. The parent directory must exist.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(absolutePathPattern, "must be an absolute path that ends in a file name"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"target": schema.StringAttribute{
				Required:    true,
				Description: "Specify the symbolic link's target path. The target does not need to exist.",
				Validators:  []validator.String{stringvalidator.LengthAtLeast(1)},
			},
		}),
	}
}

func (r *linkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *linkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan linkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.writeLink(ctx, plan); err != nil {
		resp.Diagnostics.AddError("Create link", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *linkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state linkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	command := guestCommand("readlink", "-v", "--", state.Path.ValueString())
	result, err := runGuest(ctx, r.data.pool, guest, command)
	if err != nil {
		resp.Diagnostics.AddError("Read link", err.Error())
		return
	}
	if result.ExitCode != 0 {
		stderr := string(result.Stderr)
		if strings.Contains(stderr, missingFileMessage) || strings.Contains(stderr, notALinkMessage) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Read link", commandFailure(guest, command, result).Error())
		return
	}

	// readlink ends its output with one newline; a target can end with
	// whitespace of its own.
	state.Target = types.StringValue(strings.TrimSuffix(string(result.Stdout), "\n"))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *linkResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan linkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.writeLink(ctx, plan); err != nil {
		resp.Diagnostics.AddError("Update link", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *linkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state linkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	err := runChecked(ctx, r.data.pool, guest, guestCommand("rm", "-f", "--", state.Path.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Delete link", err.Error())
	}
}

func (r *linkResource) writeLink(ctx context.Context, plan linkModel) error {
	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	// -T makes ln replace a link to a directory instead of creating the new
	// link inside that directory.
	command := guestCommand("ln", "-sfT", "--", plan.Target.ValueString(), plan.Path.ValueString())
	return runChecked(ctx, r.data.pool, guest, command)
}
