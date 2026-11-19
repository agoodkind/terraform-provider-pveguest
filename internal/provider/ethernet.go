package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
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
	networkNameField    = "name"
	networkMACField     = "mac"
	vfDriverField       = "driver"
	networkIfnameField  = "ifname"
	deleteNetworkAction = "delete"
	networkStatusAction = "status"
	minimumNetworkMTU   = 1280
	maximumHostMTU      = 65520
	maximumGuestMTU     = 65535
)

var (
	errNetworkSettings = errors.New("invalid network settings")
	linuxInterfaceName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,14}$`)
)

func interfaceNameAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Required: true,
		Validators: []validator.String{
			stringvalidator.RegexMatches(linuxInterfaceName, "must be a Linux interface name"),
		},
		PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
	}
}

func importNodeInterface(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
	field string,
) {
	node, name, found := strings.Cut(req.ID, ":")
	if !found || node == "" || !linuxInterfaceName.MatchString(name) {
		resp.Diagnostics.AddError(
			"Import node interface",
			importIDError(req.ID, "<node>:<interface>"),
		)

		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(nodeAttributeName), node)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(field), name)...)
}

type (
	hostEthernetResource struct{ data *providerData }
	hostEthernetModel    struct {
		Node      types.String `tfsdk:"node"`
		Name      types.String `tfsdk:"name"`
		CIDR      types.String `tfsdk:"cidr"`
		CIDR6     types.String `tfsdk:"cidr6"`
		Gateway   types.String `tfsdk:"gateway"`
		Gateway6  types.String `tfsdk:"gateway6"`
		MTU       types.Int64  `tfsdk:"mtu"`
		Autostart types.Bool   `tfsdk:"autostart"`
		Comments  types.String `tfsdk:"comments"`
		Method    types.String `tfsdk:"method"`
		Method6   types.String `tfsdk:"method6"`
		ChangeID  types.String `tfsdk:"change_id"`
	}
)

func (r *hostEthernetResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_host_network_interface"
}

func (r *hostEthernetResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	r.data = providerDataFrom(req, resp)
}

func (r *hostEthernetResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "Stages Ethernet settings for the node network applier. Import existing stanzas before changing them.",
		Attributes: map[string]schema.Attribute{
			"method": schema.StringAttribute{
				Computed:    true,
				Description: "Staged IPv4 configuration method derived by Proxmox.",
			},
			"method6": schema.StringAttribute{
				Computed:    true,
				Description: "Staged IPv6 configuration method derived by Proxmox.",
			},
			nodeAttributeName: guestAttributes()[nodeAttributeName],
			networkNameField:  interfaceNameAttribute(),
			"cidr":            optionalEthernetText("Static IPv4 address with prefix length."),
			"cidr6":           optionalEthernetText("Static IPv6 address with prefix length."),
			"gateway":         optionalEthernetText("Static IPv4 default gateway."),
			"gateway6":        optionalEthernetText("Static IPv6 default gateway."),
			"mtu": schema.Int64Attribute{
				Optional: true,
				Validators: []validator.Int64{
					int64validator.Between(minimumNetworkMTU, maximumHostMTU),
				},
			},
			"autostart": schema.BoolAttribute{
				Required: true,
			},
			"comments": optionalEthernetText("Interface comments."),
			"change_id": schema.StringAttribute{
				Computed:    true,
				Description: "Changes after each staged write. Use this value in proxmox_network_applier triggers.",
			},
		},
	}
}

func optionalEthernetText(description string) schema.StringAttribute {
	return schema.StringAttribute{
		Optional:    true,
		Description: description,
		Validators:  []validator.String{stringvalidator.LengthAtLeast(1)},
	}
}

func desiredEthernetMethod(cidr types.String) types.String {
	if cidr.IsUnknown() {
		return types.StringUnknown()
	}

	if cidr.IsNull() {
		return types.StringValue("manual")
	}

	return types.StringValue("static")
}

func (r *hostEthernetResource) ModifyPlan(
	ctx context.Context,
	req resource.ModifyPlanRequest,
	resp *resource.ModifyPlanResponse,
) {
	if req.Plan.Raw.IsNull() {
		return
	}

	var model hostEthernetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	v4, v6 := desiredEthernetMethod(model.CIDR), desiredEthernetMethod(model.CIDR6)
	if !model.Method.Equal(v4) || !model.Method6.Equal(v6) {
		model.ChangeID = types.StringUnknown()
	}

	model.Method, model.Method6 = v4, v6
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &model)...)
}

func (r *hostEthernetResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	importNodeInterface(ctx, req, resp, networkNameField)
}

func (r *hostEthernetResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	r.stage(ctx, req.Plan, &resp.State, &resp.Diagnostics, true)
}

func (r *hostEthernetResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	r.stage(ctx, req.Plan, &resp.State, &resp.Diagnostics, false)
}

func checkEthernetSettings(settings transport.EthernetSettings) error {
	for _, family := range []struct {
		cidr, gateway string
		v4            bool
	}{
		{settings.CIDR, settings.Gateway, true},
		{settings.CIDR6, settings.Gateway6, false},
	} {
		if family.cidr != "" {
			prefix, err := netip.ParsePrefix(family.cidr)
			if err != nil || prefix.Addr().Is4() != family.v4 {
				slog.Error(
					"invalid Ethernet gateway or address",
					"cidr",
					family.cidr,
					"gateway",
					family.gateway,
					"err",
					errNetworkSettings,
				)

				return fmt.Errorf(
					"%w: invalid CIDR %q for this address family",
					errNetworkSettings,
					family.cidr,
				)
			}
		}

		if family.gateway != "" {
			addr, err := netip.ParseAddr(family.gateway)
			if err != nil || addr.Is4() != family.v4 || family.cidr == "" {
				slog.Error(
					"invalid Ethernet gateway or address",
					"cidr",
					family.cidr,
					"gateway",
					family.gateway,
					"err",
					errNetworkSettings,
				)

				return fmt.Errorf(
					"%w: gateway %q requires a CIDR in the same address family",
					errNetworkSettings,
					family.gateway,
				)
			}
		}
	}

	return nil
}

func optionalNetworkString(value string) types.String {
	if value == "" {
		return types.StringNull()
	}

	return types.StringValue(value)
}

func (m *hostEthernetModel) observe(port transport.EthernetPort) {
	settings := port.Settings
	m.Method = types.StringValue(port.Method)
	m.Method6 = types.StringValue(port.Method6)
	m.CIDR = optionalNetworkString(settings.CIDR)
	m.CIDR6 = optionalNetworkString(settings.CIDR6)
	m.Gateway = optionalNetworkString(settings.Gateway)
	m.Gateway6 = optionalNetworkString(settings.Gateway6)
	m.MTU = types.Int64PointerValue(settings.MTU)
	m.Autostart = types.BoolValue(settings.Autostart)
	m.Comments = optionalNetworkString(settings.Comments)
}

func (r *hostEthernetResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state hostEthernetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	current, err := r.data.client.ReadEthernet(
		ctx,
		state.Node.ValueString(),
		state.Name.ValueString(),
	)
	if err != nil {
		resp.Diagnostics.AddError("Read Ethernet interface", err.Error())

		return
	}

	if !current.Configured {
		resp.State.RemoveResource(ctx)

		return
	}

	state.observe(current)
	if state.ChangeID.IsNull() {
		state.ChangeID = types.StringValue("imported")
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostEthernetResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state hostEthernetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	node, name := state.Node.ValueString(), state.Name.ValueString()
	current, err := r.data.client.ReadEthernet(ctx, node, name)
	if err != nil {
		resp.Diagnostics.AddError("Read Ethernet interface", err.Error())

		return
	}

	if current.Configured {
		if err := r.data.client.DeleteEthernet(ctx, node, name); err != nil {
			resp.Diagnostics.AddError("Stage Ethernet removal", err.Error())
		}
	}
}

func (r *hostEthernetResource) stage(
	ctx context.Context,
	plan tfsdk.Plan,
	state *tfsdk.State,
	diags *diag.Diagnostics,
	creating bool,
) {
	var model hostEthernetModel
	diags.Append(plan.Get(ctx, &model)...)
	if diags.HasError() {
		return
	}

	node, name := model.Node.ValueString(), model.Name.ValueString()
	current, err := r.data.client.ReadEthernet(ctx, node, name)
	if err != nil {
		diags.AddError("Read physical Ethernet interface", err.Error())

		return
	}

	if creating && current.Configured {
		diags.AddError(
			"Import existing Ethernet configuration",
			fmt.Sprintf("Import %s:%s before changing it.", node, name),
		)

		return
	}

	desired := transport.EthernetSettings{
		CIDR:      model.CIDR.ValueString(),
		CIDR6:     model.CIDR6.ValueString(),
		Gateway:   model.Gateway.ValueString(),
		Gateway6:  model.Gateway6.ValueString(),
		MTU:       model.MTU.ValueInt64Pointer(),
		Autostart: model.Autostart.ValueBool(),
		Comments:  model.Comments.ValueString(),
	}
	if err := checkEthernetSettings(desired); err != nil {
		diags.AddError("Invalid Ethernet settings", err.Error())

		return
	}

	if err := r.data.client.StageEthernet(ctx, node, name, desired); err != nil {
		diags.AddError("Stage Ethernet interface", err.Error())

		return
	}

	current, err = r.data.client.ReadEthernet(ctx, node, name)
	if err != nil {
		diags.AddError("Read staged Ethernet interface", err.Error())

		return
	}

	model.observe(current)
	model.ChangeID = types.StringValue(rand.Text())
	diags.Append(state.Set(ctx, &model)...)
}
