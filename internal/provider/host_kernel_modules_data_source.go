package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type hostKernelModulesDataSource struct {
	data *providerData
}

func newHostKernelModulesDataSource() datasource.DataSource {
	return &hostKernelModulesDataSource{}
}

func (d *hostKernelModulesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host_kernel_modules"
}

func (d *hostKernelModulesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads the node's persistent kernel module list and each listed module's load state without changing the node.",
		Attributes: map[string]schema.Attribute{
			"node": schema.StringAttribute{
				Description: "Select a key in the provider's nodes map.",
				Required:    true,
				Validators:  []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"modules": schema.SetAttribute{
				Description: "The set contains every module in the node's persistent module list, including modules that are not loaded.",
				Computed:    true,
				ElementType: types.StringType,
			},
			"loaded": schema.MapAttribute{
				Description: "The map reports whether each listed module is loaded.",
				Computed:    true,
				ElementType: types.BoolType,
			},
		},
	}
}

func (d *hostKernelModulesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, valid := req.ProviderData.(*providerData)
	if !valid {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Got %T. This is a defect in the provider.", req.ProviderData),
		)
		return
	}
	d.data = data
}

func (d *hostKernelModulesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var state hostKernelModulesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if d.data == nil {
		resp.Diagnostics.AddError("Unconfigured provider", "Configure the provider before reading host kernel modules.")
		return
	}

	current, err := d.data.client.GetKernelModules(ctx, state.Node.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read host kernel modules", err.Error())
		return
	}
	modules, diagnostics := types.SetValueFrom(ctx, types.StringType, current.Modules)
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
