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

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const guestOptionsNodeDescription = "The node must be a key in the provider nodes map. Changing node replaces the resource."

// guestOptionsDefinition specifies the guest kind, allowed keys, type name
// suffix, schema, and diagnostic text for an options resource.
type guestOptionsDefinition struct {
	typeSuffix         string
	kind               transport.Kind
	keys               *regexp.Regexp
	keysMessage        string
	noun               string
	description        string
	vmidDescription    string
	optionsDescription string
	pendingDescription string
}

var containerOptionsDefinition = guestOptionsDefinition{
	typeSuffix:         "_container_options",
	kind:               transport.KindLXC,
	keys:               regexp.MustCompile(transport.ContainerOptionKeyPattern),
	keysMessage:        "must be bpfdelegate or hostnic0 to hostnic9",
	noun:               "container",
	description:        "The resource manages Proxmox container options bpfdelegate and hostnic0 through hostnic9. The resource does not start, stop, or restart the container.",
	vmidDescription:    "The vmid identifies the container. Changing vmid replaces the resource.",
	optionsDescription: "The map sets bpfdelegate and hostnic0 through hostnic9. Values are Proxmox property strings, such as link=nic2,name=wan.",
	pendingDescription: "The map contains supported options that Proxmox lists as pending for a running container. Values contain the pending value. Pending deletions have an empty value.",
}

var vmOptionsDefinition = guestOptionsDefinition{
	typeSuffix:         "_vm_options",
	kind:               transport.KindQEMU,
	keys:               regexp.MustCompile(transport.VMOptionKeyPattern),
	keysMessage:        "must be vsock",
	noun:               "VM",
	description:        "The resource manages the Proxmox VM option vsock. The resource does not start, stop, or restart the VM.",
	vmidDescription:    "The vmid identifies the VM. Changing vmid replaces the resource.",
	optionsDescription: "The map sets vsock. A value of 1 adds a virtio vsock device with the VM ID as its guest CID, and a value of 0 disables it.",
	pendingDescription: "The map contains supported options that Proxmox lists as pending for a running VM. Each entry contains the pending value. Pending deletions have an empty value.",
}

type guestOptionsResource struct {
	definition guestOptionsDefinition
	data       *providerData
}

type guestOptionsModel struct {
	Node    types.String `tfsdk:"node"`
	VMID    types.Int64  `tfsdk:"vmid"`
	Options types.Map    `tfsdk:"options"`
	Pending types.Map    `tfsdk:"pending"`
}

func newContainerOptionsResource() resource.Resource {
	return &guestOptionsResource{definition: containerOptionsDefinition}
}

func newVMOptionsResource() resource.Resource {
	return &guestOptionsResource{definition: vmOptionsDefinition}
}

func (r *guestOptionsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + r.definition.typeSuffix
}

func (r *guestOptionsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: r.definition.description,
		Attributes: map[string]schema.Attribute{
			"node": schema.StringAttribute{
				Description:   guestOptionsNodeDescription,
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"vmid": schema.Int64Attribute{
				Description:   r.definition.vmidDescription,
				Required:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"options": schema.MapAttribute{
				Description: r.definition.optionsDescription,
				Required:    true,
				ElementType: types.StringType,
				Validators: []validator.Map{
					mapvalidator.KeysAre(
						stringvalidator.RegexMatches(r.definition.keys, r.definition.keysMessage),
					),
				},
			},
			"pending": schema.MapAttribute{
				Description: r.definition.pendingDescription,
				Computed:    true,
				ElementType: types.StringType,
			},
		},
	}
}

func (r *guestOptionsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

// Import IDs use the format <node>/<vmid>.
func (r *guestOptionsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	node, vmidText, found := strings.Cut(req.ID, "/")
	vmid, err := strconv.ParseInt(vmidText, 10, 64)
	if !found || node == "" || err != nil {
		resp.Diagnostics.AddError(r.summary("Import %s options"), fmt.Sprintf("the import ID %q must have the form <node>/<vmid>", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("node"), node)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("vmid"), vmid)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("options"), types.MapNull(types.StringType))...)
}

func (r *guestOptionsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan guestOptionsModel
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

func (r *guestOptionsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan guestOptionsModel
	var state guestOptionsModel
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
func (r *guestOptionsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state guestOptionsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := r.guest(state.Node.ValueString(), state.VMID.ValueInt64())

	current, err := r.data.client.GetGuestOptions(ctx, guest, r.definition.keys)
	if err != nil {
		resp.Diagnostics.AddError(r.summary("Read %s options"), err.Error())
		return
	}
	pending, err := r.data.client.GetGuestPending(ctx, guest, r.definition.keys)
	if err != nil {
		resp.Diagnostics.AddError(r.summary("Read %s pending options"), err.Error())
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

func (r *guestOptionsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state guestOptionsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior, diagnostics := optionsOf(ctx, state.Options)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := r.guest(state.Node.ValueString(), state.VMID.ValueInt64())
	err := r.data.client.SetGuestOptions(ctx, guest, nil, sortedKeys(prior))
	if err != nil {
		resp.Diagnostics.AddError(r.summary("Delete %s options"), err.Error())
	}
}

func (r *guestOptionsResource) apply(ctx context.Context, plan *guestOptionsModel, prior map[string]string) diag.Diagnostics {
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

	guest := r.guest(plan.Node.ValueString(), plan.VMID.ValueInt64())
	err := r.data.client.SetGuestOptions(ctx, guest, declared, removed)
	if err != nil {
		diagnostics.AddError(r.summary("Set %s options"), err.Error())
		return diagnostics
	}
	pending, err := r.data.client.GetGuestPending(ctx, guest, r.definition.keys)
	if err != nil {
		diagnostics.AddError(r.summary("Read %s pending options"), err.Error())
		return diagnostics
	}
	pendingMap, pendingDiagnostics := types.MapValueFrom(ctx, types.StringType, pending)
	diagnostics.Append(pendingDiagnostics...)
	plan.Pending = pendingMap
	return diagnostics
}

func (r *guestOptionsResource) guest(node string, vmid int64) transport.Guest {
	return transport.Guest{Node: node, VMID: vmid, Kind: r.definition.kind}
}

// summary formats a diagnostic summary with the guest noun.
func (r *guestOptionsResource) summary(format string) string {
	return fmt.Sprintf(format, r.definition.noun)
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
