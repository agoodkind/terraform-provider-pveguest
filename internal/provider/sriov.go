package provider

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

var (
	driverName    = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]{0,63}$`)
	vfDesiredType = types.ObjectType{
		AttrTypes: map[string]attr.Type{
			networkMACField:    types.StringType,
			vfDriverField:      types.StringType,
			networkIfnameField: types.StringType,
		},
	}
	vfRuntimeType = types.ObjectType{
		AttrTypes: map[string]attr.Type{
			networkMACField:    types.StringType,
			vfDriverField:      types.StringType,
			networkIfnameField: types.StringType,
			"pci":              types.StringType,
			"in_container":     types.BoolType,
		},
	}
)

type vfAssignment struct {
	MAC    types.String `tfsdk:"mac"`
	Driver types.String `tfsdk:"driver"`
	Ifname types.String `tfsdk:"ifname"`
}
type vfObservation struct {
	MAC         types.String `tfsdk:"mac"`
	Driver      types.String `tfsdk:"driver"`
	Ifname      types.String `tfsdk:"ifname"`
	PCI         types.String `tfsdk:"pci"`
	InContainer types.Bool   `tfsdk:"in_container"`
}
type hostSRIOVModel struct {
	Node       types.String `tfsdk:"node"`
	Ifname     types.String `tfsdk:"ifname"`
	NumVFs     types.Int64  `tfsdk:"numvfs"`
	VFs        types.Map    `tfsdk:"vfs"`
	ActualVFs  types.Map    `tfsdk:"actual_vfs"`
	TotalVFs   types.Int64  `tfsdk:"totalvfs"`
	Reconciled types.Bool   `tfsdk:"reconciled"`
}
type hostSRIOVResource struct{ data *providerData }

func (r *hostSRIOVResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_host_sriov"
}

func (r *hostSRIOVResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	r.data = providerDataFrom(req, resp)
}

func (r *hostSRIOVResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "Manages SR-IOV assignments under the Proxmox guest ownership locks.",
		Attributes: map[string]schema.Attribute{
			nodeAttributeName:  guestAttributes()[nodeAttributeName],
			networkIfnameField: interfaceNameAttribute(),
			"numvfs": schema.Int64Attribute{
				Required:   true,
				Validators: []validator.Int64{int64validator.AtLeast(0)},
			},
			"vfs": schema.MapNestedAttribute{
				Required:    true,
				Description: "Assignments keyed by the decimal VF index.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						networkMACField: schema.StringAttribute{
							Required:    true,
							Description: "Lowercase nonzero unicast MAC.",
						},
						vfDriverField: schema.StringAttribute{
							Required: true,
							Validators: []validator.String{
								stringvalidator.RegexMatches(
									driverName,
									"must be a PCI driver name",
								),
							},
						},
						networkIfnameField: schema.StringAttribute{
							Optional: true,
							Validators: []validator.String{
								stringvalidator.RegexMatches(
									linuxInterfaceName,
									"must be a Linux interface name",
								),
							},
						},
					},
				},
			},
			"actual_vfs": schema.MapAttribute{
				Computed:    true,
				ElementType: vfRuntimeType,
			},
			"totalvfs": schema.Int64Attribute{Computed: true},
			"reconciled": schema.BoolAttribute{
				Computed:    true,
				Description: "The saved assignments and enabled VF count agree with runtime.",
			},
		},
	}
}

func (r *hostSRIOVResource) ModifyPlan(
	ctx context.Context,
	req resource.ModifyPlanRequest,
	resp *resource.ModifyPlanResponse,
) {
	if req.Plan.Raw.IsNull() {
		return
	}

	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("reconciled"), true)...)
	if req.State.Raw.IsNull() {
		return
	}

	var reconciled types.Bool
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("reconciled"), &reconciled)...)
	if !reconciled.ValueBool() {
		resp.Diagnostics.Append(
			resp.Plan.SetAttribute(
				ctx,
				path.Root("actual_vfs"),
				types.MapUnknown(vfRuntimeType),
			)...,
		)
	}
}

func (r *hostSRIOVResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	importNodeInterface(ctx, req, resp, networkIfnameField)
}

func (r *hostSRIOVResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	r.apply(ctx, req.Plan, &resp.State, &resp.Diagnostics, true)
}

func (r *hostSRIOVResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	r.apply(ctx, req.Plan, &resp.State, &resp.Diagnostics, false)
}

func validAssignedMAC(value string) bool {
	mac, err := net.ParseMAC(value)

	return err == nil && len(mac) == 6 && mac[0]&1 == 0 && mac.String() == value &&
		value != "00:00:00:00:00:00"
}

func (m *hostSRIOVModel) declaration(
	ctx context.Context,
	diags *diag.Diagnostics,
) transport.SRIOVDeclaration {
	desired := transport.SRIOVDeclaration{
		NumVFs: m.NumVFs.ValueInt64(),
		VFs:    []transport.VFDeclaration{},
	}
	var entries map[string]vfAssignment
	diags.Append(m.VFs.ElementsAs(ctx, &entries, false)...)
	macs, names := map[string]bool{}, map[string]bool{m.Ifname.ValueString(): true, "lo": true}

	for key, entry := range entries {
		index, err := strconv.ParseInt(key, 10, 64)
		if err != nil || index < 0 || index >= desired.NumVFs ||
			strconv.FormatInt(index, 10) != key {
			diags.AddError(
				"Invalid VF index",
				fmt.Sprintf(
					"Index %q must be a canonical decimal index below numvfs (%d).",
					key,
					desired.NumVFs,
				),
			)

			continue
		}

		mac, driver, name := entry.MAC.ValueString(), entry.Driver.ValueString(), entry.Ifname.ValueString()
		if !validAssignedMAC(mac) || macs[mac] {
			diags.AddError(
				"Invalid VF MAC",
				fmt.Sprintf("VF %s requires a unique lowercase nonzero unicast MAC.", key),
			)

			continue
		}

		if name != "" && (names[name] || driver == "vfio-pci") {
			diags.AddError(
				"Invalid VF interface name",
				fmt.Sprintf("VF %s requires a unique native-driver interface name.", key),
			)

			continue
		}

		macs[mac] = true
		names[name] = true
		desired.VFs = append(
			desired.VFs,
			transport.VFDeclaration{Index: index, MAC: mac, Driver: driver, Ifname: name},
		)
	}

	slices.SortFunc(
		desired.VFs,
		func(a, b transport.VFDeclaration) int {
			return cmp.Compare(a.Index, b.Index)
		},
	)

	return desired
}

func actualVFMap(
	ctx context.Context,
	current transport.SRIOVState,
	diags *diag.Diagnostics,
) types.Map {
	entries := make(map[string]vfObservation, len(current.VFs))
	for _, function := range current.VFs {
		entries[strconv.FormatInt(function.Index, 10)] = vfObservation{
			MAC:         types.StringValue(function.MAC),
			PCI:         types.StringValue(function.PCI),
			Driver:      optionalNetworkString(function.Driver),
			Ifname:      optionalNetworkString(function.Ifname),
			InContainer: types.BoolValue(function.InContainer),
		}
	}

	value, diagnostics := types.MapValueFrom(ctx, vfRuntimeType, entries)
	diags.Append(diagnostics...)

	return value
}

func (m *hostSRIOVModel) observe(
	ctx context.Context,
	current transport.SRIOVState,
	diags *diag.Diagnostics,
) {
	actual := make(map[int64]transport.VFState, len(current.VFs))
	for _, function := range current.VFs {
		actual[function.Index] = function
	}

	entries := make(map[string]vfAssignment, len(current.Desired.VFs))

	matches := current.NumVFs == current.Desired.NumVFs
	for _, desired := range current.Desired.VFs {
		function, ok := actual[desired.Index]
		matches = matches && ok && function.PCI != "" && function.MAC == desired.MAC &&
			function.Driver == desired.Driver &&
			(desired.Ifname == "" || function.InContainer || function.Ifname == desired.Ifname)
		entries[strconv.FormatInt(desired.Index, 10)] = vfAssignment{
			MAC:    types.StringValue(desired.MAC),
			Driver: types.StringValue(desired.Driver),
			Ifname: optionalNetworkString(desired.Ifname),
		}
	}

	vfs, diagnostics := types.MapValueFrom(ctx, vfDesiredType, entries)
	diags.Append(diagnostics...)
	m.VFs = vfs
	m.ActualVFs = actualVFMap(ctx, current, diags)
	m.NumVFs = types.Int64Value(current.Desired.NumVFs)
	m.TotalVFs = types.Int64Value(current.TotalVFs)
	m.Reconciled = types.BoolValue(matches)
}

func (r *hostSRIOVResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state hostSRIOVModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	current, err := r.data.client.ReadSRIOV(
		ctx,
		state.Node.ValueString(),
		state.Ifname.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError("Read SR-IOV adapter", err.Error())

		return
	}

	if current.Desired == nil {
		resp.State.RemoveResource(ctx)

		return
	}

	state.observe(ctx, current, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostSRIOVResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state hostSRIOVModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.data.client.DeleteSRIOV(
		ctx,
		state.Node.ValueString(),
		state.Ifname.ValueString(),
	); err != nil {
		resp.Diagnostics.AddError("Remove SR-IOV configuration", err.Error())
	}
}

func (r *hostSRIOVResource) apply(
	ctx context.Context,
	plan tfsdk.Plan,
	state *tfsdk.State,
	diags *diag.Diagnostics,
	creating bool,
) {
	var model hostSRIOVModel
	diags.Append(plan.Get(ctx, &model)...)
	if diags.HasError() {
		return
	}

	desired := model.declaration(ctx, diags)
	if diags.HasError() {
		return
	}

	node, name := model.Node.ValueString(), model.Ifname.ValueString()
	current, err := r.data.client.ReadSRIOV(ctx, node, name)
	if err != nil {
		diags.AddError("Read SR-IOV adapter", err.Error())

		return
	}

	if creating && current.Desired != nil {
		diags.AddError(
			"Import existing SR-IOV declaration",
			fmt.Sprintf("Import %s:%s before changing the saved declaration.", node, name),
		)

		return
	}

	if desired.NumVFs > current.TotalVFs {
		diags.AddError(
			"Invalid VF count",
			fmt.Sprintf("Adapter %s supports %d VFs.", name, current.TotalVFs),
		)

		return
	}

	current, err = r.data.client.SetSRIOV(ctx, node, name, desired)
	if err != nil {
		diags.AddError("Apply SR-IOV configuration", err.Error())

		return
	}

	if current.Desired == nil {
		diags.AddError(
			"Read SR-IOV declaration",
			"Proxmox did not return a saved declaration after PUT.",
		)

		return
	}

	model.observe(ctx, current, diags)
	if !model.Reconciled.ValueBool() {
		diags.AddError(
			"Verify SR-IOV adapter",
			"The runtime VF inventory differs from the saved declaration after PUT.",
		)

		return
	}

	diags.Append(state.Set(ctx, &model)...)
}
