package provider

import (
	"context"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var kernelModuleNamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

type hostKernelModulesResource struct {
	data *providerData
}

type hostKernelModulesModel struct {
	Node    types.String `tfsdk:"node"`
	Modules types.Set    `tfsdk:"modules"`
	Loaded  types.Map    `tfsdk:"loaded"`
}

func newHostKernelModulesResource() resource.Resource {
	return &hostKernelModulesResource{}
}

func (r *hostKernelModulesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_kernel_modules"
}

func (r *hostKernelModulesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The resource manages a node's kernel module list. The resource never unloads modules. The destroy operation empties the node's module list.",
		Attributes: map[string]schema.Attribute{
			"node": schema.StringAttribute{
				Description:   "The node value must be a key in the provider's nodes map. Changing node replaces the resource.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"modules": schema.SetAttribute{
				Description: "The set specifies kernel modules to load at boot and during apply. Each name must match ^[a-z0-9_]+$. The node accepts only allowlisted modules and rejects built-in modules.",
				Required:    true,
				ElementType: types.StringType,
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(kernelModuleNamePattern, "must be a kernel module name"),
					),
				},
			},
			"loaded": schema.MapAttribute{
				Description: "The map reports whether each module in the node list is loaded.",
				Computed:    true,
				ElementType: types.BoolType,
			},
		},
	}
}

func (r *hostKernelModulesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *hostKernelModulesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("node"), req, resp)
}

func (r *hostKernelModulesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostKernelModulesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostKernelModulesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hostKernelModulesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read omits unloaded entries from modules to make the next plan propose an update.
func (r *hostKernelModulesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostKernelModulesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	current, err := r.data.client.GetKernelModules(ctx, state.Node.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read host kernel modules", err.Error())
		return
	}

	present := make([]string, 0, len(current.Modules))
	for _, name := range current.Modules {
		if current.Loaded[name] {
			present = append(present, name)
		}
	}
	modules, diagnostics := types.SetValueFrom(ctx, types.StringType, present)
	resp.Diagnostics.Append(diagnostics...)
	loaded, diagnostics := types.MapValueFrom(ctx, types.BoolType, current.Loaded)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Modules = modules
	state.Loaded = loaded
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostKernelModulesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostKernelModulesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.data.client.SetKernelModules(ctx, state.Node.ValueString(), []string{})
	if err != nil {
		resp.Diagnostics.AddError("Clear host kernel modules", err.Error())
	}
}

func (r *hostKernelModulesResource) apply(ctx context.Context, plan *hostKernelModulesModel) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	var declared []string
	diagnostics.Append(plan.Modules.ElementsAs(ctx, &declared, false)...)
	if diagnostics.HasError() {
		return diagnostics
	}

	current, err := r.data.client.SetKernelModules(ctx, plan.Node.ValueString(), declared)
	if err != nil {
		diagnostics.AddError("Set host kernel modules", err.Error())
		return diagnostics
	}
	loaded, loadedDiagnostics := types.MapValueFrom(ctx, types.BoolType, current.Loaded)
	diagnostics.Append(loadedDiagnostics...)
	plan.Loaded = loaded
	return diagnostics
}
