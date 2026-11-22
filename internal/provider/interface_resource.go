package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/agoodkind/terraform-provider-pveguest/internal/netconfig"
	"github.com/agoodkind/terraform-provider-pveguest/transport"
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
)

type guestNetworkModel struct {
	Node           types.String `tfsdk:"node"`
	VMID           types.Int64  `tfsdk:"vmid"`
	Kind           types.String `tfsdk:"kind"`
	Name           types.String `tfsdk:"name"`
	MAC            types.String `tfsdk:"mac"`
	Backend        types.String `tfsdk:"backend"`
	Phase          types.String `tfsdk:"phase"`
	DHCP4          types.Bool   `tfsdk:"dhcp4"`
	SLAAC          types.Bool   `tfsdk:"slaac"`
	MTU            types.Int64  `tfsdk:"mtu"`
	Addresses      types.Set    `tfsdk:"addresses"`
	ProxyNeighbors types.Set    `tfsdk:"ipv6_proxy_neighbors"`
	DNS            types.List   `tfsdk:"dns_servers"`
	Search         types.List   `tfsdk:"dns_search"`
	Reconciled     types.Bool   `tfsdk:"reconciled"`
}
type guestNetworkResource struct{ data *providerData }

func (r *guestNetworkResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_network_interface"
}

func (r *guestNetworkResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	r.data = providerDataFrom(req, resp)
}

func (r *guestNetworkResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "Configures a guest network manager for an interface selected by MAC.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			networkNameField: interfaceNameAttribute(),
			networkMACField:  schema.StringAttribute{Required: true},
			"backend": schema.StringAttribute{
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf("netplan", "networkd", "ifupdown"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"phase": schema.StringAttribute{
				Description: "Stage or activate this link. Restart networkd guests after staging changes and before activation.",
				Required:    true,
				Validators: []validator.String{
					stringvalidator.OneOf("staged", string(unitActive)),
				},
			},
			"dhcp4": schema.BoolAttribute{
				Required: true,
			},
			"slaac": schema.BoolAttribute{Required: true},
			"mtu": schema.Int64Attribute{
				Optional: true,
				Validators: []validator.Int64{
					int64validator.Between(minimumNetworkMTU, maximumGuestMTU),
				},
			},
			"addresses": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
			},
			"ipv6_proxy_neighbors": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Exact IPv6 addresses for which the ifupdown interface answers neighbor discovery.",
			},
			"dns_servers": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
			},
			"dns_search": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
			},
			"reconciled": schema.BoolAttribute{
				Computed:    true,
				Description: "Reports whether the persistent settings and requested live state match.",
			},
		}),
	}
}

func (r *guestNetworkResource) ValidateConfig(
	ctx context.Context,
	req resource.ValidateConfigRequest,
	resp *resource.ValidateConfigResponse,
) {
	var config guestNetworkModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if !resp.Diagnostics.HasError() {
		config.proxyNeighbors(&resp.Diagnostics)
	}
}

func (r *guestNetworkResource) ModifyPlan(
	ctx context.Context,
	req resource.ModifyPlanRequest,
	resp *resource.ModifyPlanResponse,
) {
	if !req.Plan.Raw.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("reconciled"), true)...)
	}
}

func (r *guestNetworkResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	r.apply(ctx, req.Plan, nil, resp.Private, &resp.State, &resp.Diagnostics)
}

func (r *guestNetworkResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	owned := readGuestProxyOwnership(ctx, req.Private, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		r.apply(ctx, req.Plan, owned, resp.Private, &resp.State, &resp.Diagnostics)
	}
}

func (r *guestNetworkResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var model guestNetworkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	owned := readGuestProxyOwnership(ctx, req.Private, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		r.execute(ctx, &model, owned, &resp.Diagnostics, "read")
	}

	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
	}
}

func (r *guestNetworkResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var model guestNetworkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	owned := readGuestProxyOwnership(ctx, req.Private, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		r.execute(ctx, &model, owned, &resp.Diagnostics, deleteNetworkAction)
	}
}

var searchDomain = regexp.MustCompile(
	`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.?$`,
)

func (m guestNetworkModel) settings(
	ctx context.Context,
	diags *diag.Diagnostics,
) netconfig.Settings {
	settings := netconfig.Settings{
		Name:               m.Name.ValueString(),
		MAC:                m.MAC.ValueString(),
		DHCP4:              m.DHCP4.ValueBool(),
		SLAAC:              m.SLAAC.ValueBool(),
		MTU:                m.MTU.ValueInt64(),
		DNS:                []string{},
		Search:             []string{},
		Addresses:          []string{},
		IPv6ProxyNeighbors: m.proxyNeighbors(diags),
	}
	if !m.Addresses.IsNull() {
		var d diag.Diagnostics
		settings.Addresses, d = stringValues(ctx, m.Addresses)
		diags.Append(d...)
	}

	if !m.DNS.IsNull() {
		diags.Append(m.DNS.ElementsAs(ctx, &settings.DNS, false)...)
	}

	if !m.Search.IsNull() {
		diags.Append(m.Search.ElementsAs(ctx, &settings.Search, false)...)
	}

	if !validAssignedMAC(settings.MAC) {
		diags.AddError("Invalid guest MAC", "Use a lowercase nonzero unicast Ethernet MAC.")
	}

	for index, value := range settings.Addresses {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			diags.AddError("Invalid guest CIDR", err.Error())

			continue
		}

		settings.Addresses[index] = prefix.String()
	}

	for _, value := range settings.DNS {
		addr, err := netip.ParseAddr(value)
		if err != nil || addr.Zone() != "" {
			diags.AddError(
				"Invalid guest DNS server",
				fmt.Sprintf("Expected an IPv4 or IPv6 address without a zone, got %q.", value),
			)
		}
	}

	for _, value := range settings.Search {
		if len(value) > 253 || !searchDomain.MatchString(value) {
			diags.AddError("Invalid DNS search domain", fmt.Sprintf("Invalid domain %q.", value))
		}
	}

	return settings
}

const guestProxyOwnershipKey = "guest_ipv6_proxy_neighbors"

type privateGetter interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

func readGuestProxyOwnership(
	ctx context.Context,
	private privateGetter,
	diags *diag.Diagnostics,
) []string {
	data, diagnostics := private.GetKey(ctx, guestProxyOwnershipKey)
	diags.Append(diagnostics...)
	if len(data) == 0 || diags.HasError() {
		return nil
	}

	var owned []string
	if err := json.Unmarshal(data, &owned); err != nil {
		diags.AddError("Read guest IPv6 proxy neighbor ownership", err.Error())
	}

	return owned
}

func (m guestNetworkModel) proxyNeighbors(diags *diag.Diagnostics) []string {
	attributePath := path.Root("ipv6_proxy_neighbors")
	if len(m.ProxyNeighbors.Elements()) == 0 {
		return nil
	}

	if !m.Backend.IsUnknown() && m.Backend.ValueString() != string(netconfig.Ifupdown) {
		diags.AddAttributeError(
			attributePath,
			"Unsupported guest network backend",
			"Use the ifupdown backend for IPv6 proxy neighbors.",
		)

		return nil
	}

	addresses := map[string]struct{}{}

	for _, element := range m.ProxyNeighbors.Elements() {
		value, _ := element.(types.String)
		if value.IsUnknown() {
			continue
		}

		address, err := netip.ParseAddr(value.ValueString())
		if err != nil || !address.Is6() || address.Is4In6() || address.Zone() != "" ||
			address.IsUnspecified() || address.IsMulticast() || address.IsLoopback() {
			diags.AddAttributeError(
				attributePath,
				"Invalid IPv6 proxy neighbor",
				fmt.Sprintf(
					"Use an exact IPv6 unicast address without a prefix or zone. Got %q.",
					value.ValueString(),
				),
			)

			continue
		}

		addresses[address.String()] = struct{}{}
	}

	return slices.Sorted(maps.Keys(addresses))
}

var errGuestNetwork = errors.New("guest interface operation failed")

type guestNetworkSession struct {
	client   *transport.Client
	guest    transport.Guest
	backend  string
	settings netconfig.Settings
}

func (r *guestNetworkResource) execute(
	ctx context.Context,
	model *guestNetworkModel,
	owned []string,
	diags *diag.Diagnostics,
	action string,
) []string {
	session := guestNetworkSession{
		r.data.client,
		guestOf(model.Node, model.VMID, model.Kind),
		model.Backend.ValueString(),
		model.settings(ctx, diags),
	}
	if diags.HasError() {
		return nil
	}

	wanted := session.settings.IPv6ProxyNeighbors
	session.settings.ObsoleteProxyNeighbors = difference(owned, wanted)
	if action == deleteNetworkAction {
		session.settings.ObsoleteProxyNeighbors = append(
			session.settings.ObsoleteProxyNeighbors,
			wanted...,
		)
	}

	ready, err := session.reconcile(ctx, action, model.Phase.ValueString() == string(unitActive))
	if err != nil {
		diags.AddError("Configure guest network interface", err.Error())

		return nil
	}

	model.Reconciled = types.BoolValue(ready)

	return wanted
}

func (s guestNetworkSession) helper(
	ctx context.Context,
	script string,
	argv []string,
	input []byte,
	readOnly bool,
) ([]byte, error) {
	command := guestCommand(append([]string{"perl", "-e", script, "--"}, argv...)...)
	command.Stdin = input
	command.TimeoutSeconds = 120
	run := runGuest
	if readOnly {
		run = runReadOnlyGuest
	}

	result, err := run(ctx, s.client, s.guest, command)
	if err != nil {
		return nil, err
	}

	if result.ExitCode != 0 {
		slog.ErrorContext(
			ctx,
			"guest network operation failed",
			"guest",
			s.guest,
			"backend",
			s.backend,
			"err",
			errGuestNetwork,
		)

		return nil, fmt.Errorf(
			"%w: %s: %s %s: %s",
			errGuestNetwork,
			s.guest,
			s.backend,
			strings.Join(argv, " "),
			strings.TrimSpace(string(result.Stderr)),
		)
	}

	return result.Stdout, nil
}

func (s guestNetworkSession) edits(
	ctx context.Context,
	removing bool,
) (map[string]netconfig.Edit, error) {
	raw, err := s.helper(
		ctx,
		netconfig.FileScript,
		[]string{s.backend, "read", s.settings.Name},
		nil,
		true,
	)
	if err != nil {
		return nil, err
	}

	var files map[string]string
	if err := json.Unmarshal(raw, &files); err != nil {
		slog.ErrorContext(ctx, "parse guest network configuration", "err", err)

		return nil, fmt.Errorf("read guest network files: %w", err)
	}

	changes, err := s.settings.Plan(netconfig.Backend(s.backend), files, removing)
	if err != nil {
		slog.ErrorContext(ctx, "plan guest network settings", "err", err)

		return nil, fmt.Errorf("plan %s: %w", s.backend, err)
	}

	if s.backend != string(netconfig.Netplan) {
		return changes, nil
	}

	body, err := json.Marshal(changes)
	if err != nil {
		slog.ErrorContext(ctx, "encode netplan sources", "err", err)

		return nil, fmt.Errorf("encode netplan sources: %w", err)
	}

	action := "render"
	if removing {
		action = "unrender"
	}

	rendered, err := s.helper(
		ctx,
		netconfig.FileScript,
		[]string{s.backend, action, s.settings.Name, s.settings.MAC},
		body,
		true,
	)
	if err != nil {
		return nil, err
	}

	var overrides map[string]netconfig.Edit
	if err := json.Unmarshal(rendered, &overrides); err != nil {
		slog.ErrorContext(ctx, "decode generated networkd units", "err", err)

		return nil, fmt.Errorf("decode generated networkd units: %w", err)
	}

	maps.Copy(changes, overrides)

	return changes, nil
}

func (s guestNetworkSession) reconcile(
	ctx context.Context,
	action string,
	active bool,
) (bool, error) {
	payload, err := json.Marshal(s.settings)
	if err != nil {
		slog.ErrorContext(ctx, "encode guest settings", "err", err)

		return false, fmt.Errorf("encode guest settings: %w", err)
	}

	if action != deleteNetworkAction {
		if _, err := s.helper(
			ctx,
			netconfig.RuntimeScript,
			[]string{"prerequisites", s.backend},
			payload,
			true,
		); err != nil {
			return false, err
		}
	}

	edits, err := s.edits(ctx, action == deleteNetworkAction)
	if err != nil {
		return false, err
	}

	if len(edits) > 0 && action == "read" {
		return false, nil
	}

	if action == deleteNetworkAction && len(s.settings.ObsoleteProxyNeighbors) > 0 {
		if _, err := s.helper(
			ctx,
			netconfig.RuntimeScript,
			[]string{"remove-proxy-neighbors", s.backend},
			payload,
			false,
		); err != nil {
			return false, err
		}
	}

	if len(edits) > 0 {
		if err := s.writeChanges(ctx, edits, action == deleteNetworkAction); err != nil {
			return false, err
		}
	}

	if action == deleteNetworkAction || !active {
		return true, nil
	}

	operation := networkStatusAction
	if action == "apply" {
		operation = "activate"
	}

	raw, err := s.helper(
		ctx,
		netconfig.RuntimeScript,
		[]string{operation, s.backend},
		payload,
		operation == networkStatusAction,
	)
	if err != nil {
		return false, err
	}

	var status struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		slog.ErrorContext(ctx, "decode guest network status", "err", err)

		return false, fmt.Errorf("decode runtime status: %w", err)
	}

	return status.Ready, nil
}

func (r *guestNetworkResource) apply(
	ctx context.Context,
	plan tfsdk.Plan,
	owned []string,
	private privateSetter,
	state *tfsdk.State,
	diags *diag.Diagnostics,
) {
	var model guestNetworkModel
	diags.Append(plan.Get(ctx, &model)...)
	if diags.HasError() {
		return
	}

	wanted := r.execute(ctx, &model, owned, diags, "apply")
	if diags.HasError() {
		return
	}

	if model.Phase.ValueString() != string(unitActive) {
		wanted = append(wanted, difference(owned, wanted)...)
	}

	if len(owned) > 0 || len(wanted) > 0 {
		var data []byte
		if len(wanted) > 0 {
			var err error
			data, err = json.Marshal(wanted)
			if err != nil {
				diags.AddError("Save guest IPv6 proxy neighbor ownership", err.Error())

				return
			}
		}

		diags.Append(private.SetKey(ctx, guestProxyOwnershipKey, data)...)
	}

	diags.Append(state.Set(ctx, &model)...)
}

func (s guestNetworkSession) writeChanges(
	ctx context.Context,
	edits map[string]netconfig.Edit,
	removing bool,
) error {
	data, err := json.Marshal(edits)
	if err != nil {
		slog.ErrorContext(ctx, "encode guest configuration edits", "err", err)

		return fmt.Errorf("encode guest configuration edits: %w", err)
	}

	if _, err := s.helper(
		ctx,
		netconfig.FileScript,
		[]string{s.backend, "write", s.settings.Name},
		data,
		false,
	); err != nil {
		return err
	}

	edits, err = s.edits(ctx, removing)
	if err != nil {
		return err
	}

	if len(edits) > 0 {
		slog.ErrorContext(
			ctx,
			"guest network operation failed",
			"guest",
			s.guest,
			"backend",
			s.backend,
			"err",
			errGuestNetwork,
		)

		return fmt.Errorf(
			"%w: %s persistent settings on %s differ after writing",
			errGuestNetwork,
			s.backend,
			s.settings.Name,
		)
	}

	return nil
}
