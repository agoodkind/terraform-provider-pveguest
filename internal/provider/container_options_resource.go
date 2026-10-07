package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

var containerOptionKeyPattern = regexp.MustCompile(transport.ContainerOptionKeyPattern)

type containerOptionsResource struct {
	data *providerData
}

type containerOptionsModel struct {
	Node    types.String `tfsdk:"node"`
	VMID    types.Int64  `tfsdk:"vmid"`
	Options types.Map    `tfsdk:"options"`
	Pending types.Map    `tfsdk:"pending"`
}

func newContainerOptionsResource() resource.Resource {
	return &containerOptionsResource{}
}

func (r *containerOptionsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container_options"
}

func (r *containerOptionsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The resource manages Proxmox container options bpfdelegate and hostnic0 through hostnic9. The resource does not start, stop, or restart the container.",
		Attributes: map[string]schema.Attribute{
			"node": schema.StringAttribute{
				Description:   "The node must be a key in the provider nodes map. Changing node replaces the resource.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"vmid": schema.Int64Attribute{
				Description:   "The vmid identifies the container. Changing vmid replaces the resource.",
				Required:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"options": schema.MapAttribute{
				Description: "The map sets bpfdelegate and hostnic0 through hostnic9. Values are Proxmox property strings, such as link=nic2,name=wan.",
				Required:    true,
				ElementType: types.StringType,
				Validators: []validator.Map{
					mapvalidator.KeysAre(
						stringvalidator.RegexMatches(containerOptionKeyPattern, "must be bpfdelegate or hostnic0 to hostnic9"),
					),
				},
			},
			"pending": schema.MapAttribute{
				Description: "The map contains supported options that Proxmox lists as pending for a running container. Values contain the pending value. Pending deletions have an empty value.",
				Computed:    true,
				ElementType: types.StringType,
			},
		},
	}
}

func (r *containerOptionsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

// Import IDs use the format <node>/<vmid>.
func (r *containerOptionsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	node, vmidText, found := strings.Cut(req.ID, "/")
	vmid, err := strconv.ParseInt(vmidText, 10, 64)
	if !found || node == "" || err != nil {
		resp.Diagnostics.AddError("Import container options", fmt.Sprintf("the import ID %q must have the form <node>/<vmid>", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("node"), node)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("vmid"), vmid)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("options"), types.MapNull(types.StringType))...)
}

func (r *containerOptionsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan containerOptionsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, nil)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerOptionsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan containerOptionsModel
	var state containerOptionsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior, diagnostics := optionsOf(ctx, state.Options)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Include every supported key in state to detect options added on the node.
func (r *containerOptionsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state containerOptionsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	node := state.Node.ValueString()
	vmid := state.VMID.ValueInt64()

	current, err := r.data.client.GetContainerOptions(ctx, node, vmid)
	if err != nil {
		resp.Diagnostics.AddError("Read container options", err.Error())
		return
	}
	pending, err := r.data.client.GetContainerPending(ctx, node, vmid)
	if err != nil {
		resp.Diagnostics.AddError("Read container pending options", err.Error())
		return
	}
	options, diagnostics := types.MapValueFrom(ctx, types.StringType, current)
	resp.Diagnostics.Append(diagnostics...)
	pendingMap, diagnostics := types.MapValueFrom(ctx, types.StringType, pending)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Options = options
	state.Pending = pendingMap
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *containerOptionsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state containerOptionsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior, diagnostics := optionsOf(ctx, state.Options)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.data.client.SetContainerOptions(ctx, state.Node.ValueString(), state.VMID.ValueInt64(), nil, sortedKeys(prior))
	if err != nil {
		resp.Diagnostics.AddError("Delete container options", err.Error())
	}
}

func (r *containerOptionsResource) apply(ctx context.Context, plan *containerOptionsModel, prior map[string]string) diag.Diagnostics {
	declared, diagnostics := optionsOf(ctx, plan.Options)
	if diagnostics.HasError() {
		return diagnostics
	}
	var removed []string
	for key := range prior {
		_, kept := declared[key]
		if !kept {
			removed = append(removed, key)
		}
	}
	slices.Sort(removed)

	node := plan.Node.ValueString()
	vmid := plan.VMID.ValueInt64()
	err := r.data.client.SetContainerOptions(ctx, node, vmid, declared, removed)
	if err != nil {
		diagnostics.AddError("Set container options", err.Error())
		return diagnostics
	}
	pending, err := r.data.client.GetContainerPending(ctx, node, vmid)
	if err != nil {
		diagnostics.AddError("Read container pending options", err.Error())
		return diagnostics
	}
	pendingMap, pendingDiagnostics := types.MapValueFrom(ctx, types.StringType, pending)
	diagnostics.Append(pendingDiagnostics...)
	plan.Pending = pendingMap
	return diagnostics
}

func optionsOf(ctx context.Context, value types.Map) (map[string]string, diag.Diagnostics) {
	options := make(map[string]string)
	if value.IsNull() || value.IsUnknown() {
		return options, nil
	}
	diagnostics := value.ElementsAs(ctx, &options, false)
	return options, diagnostics
}

func sortedKeys(options map[string]string) []string {
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
