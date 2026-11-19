package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type (
	sriovDataSource struct{ data *providerData }
	sriovDataModel  struct {
		Node      types.String `tfsdk:"node"`
		Ifname    types.String `tfsdk:"ifname"`
		NumVFs    types.Int64  `tfsdk:"numvfs"`
		TotalVFs  types.Int64  `tfsdk:"totalvfs"`
		ActualVFs types.Map    `tfsdk:"actual_vfs"`
	}
)

func (d *sriovDataSource) Metadata(
	_ context.Context,
	req datasource.MetadataRequest,
	resp *datasource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_host_sriov"
}

func (d *sriovDataSource) Schema(
	_ context.Context,
	_ datasource.SchemaRequest,
	resp *datasource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "Reads the enabled SR-IOV devices by their actual VF indices.",
		Attributes: map[string]schema.Attribute{
			nodeAttributeName: schema.StringAttribute{
				Required: true,
			},
			networkIfnameField: schema.StringAttribute{
				Required:   true,
				Validators: interfaceNameAttribute().Validators,
			},
			"numvfs": schema.Int64Attribute{
				Computed: true,
			},
			"totalvfs":   schema.Int64Attribute{Computed: true},
			"actual_vfs": schema.MapAttribute{Computed: true, ElementType: vfRuntimeType},
		},
	}
}

func (d *sriovDataSource) Configure(
	_ context.Context,
	req datasource.ConfigureRequest,
	resp *datasource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}

	var ok bool
	d.data, ok = req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Got %T.", req.ProviderData),
		)
	}
}

func (d *sriovDataSource) Read(
	ctx context.Context,
	req datasource.ReadRequest,
	resp *datasource.ReadResponse,
) {
	var state sriovDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	current, err := d.data.client.ReadSRIOV(
		ctx,
		state.Node.ValueString(),
		state.Ifname.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError("Read SR-IOV adapter", err.Error())

		return
	}

	state.NumVFs = types.Int64Value(current.NumVFs)
	state.TotalVFs = types.Int64Value(current.TotalVFs)
	state.ActualVFs = actualVFMap(ctx, current, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
