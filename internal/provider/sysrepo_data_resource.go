package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/sysrepo"
)

const (
	datastoreStartup = "startup"
	datastoreRunning = "running"

	// Exclude implicit defaults from drift comparison.
	explicitDefaultsMode = "explicit"
)

type sysrepoDataResource struct {
	data *providerData
}

type sysrepoDataModel struct {
	Node      types.String `tfsdk:"node"`
	VMID      types.Int64  `tfsdk:"vmid"`
	Kind      types.String `tfsdk:"kind"`
	Datastore types.String `tfsdk:"datastore"`
	Module    types.String `tfsdk:"module"`
	Content   types.String `tfsdk:"content"`
}

func newSysrepoDataResource() resource.Resource {
	return &sysrepoDataResource{}
}

func (r *sysrepoDataResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sysrepo_data"
}

func (r *sysrepoDataResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "This resource manages the complete configuration of one YANG module in a guest datastore. " +
			"Apply replaces that configuration. Destroy removes it.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"datastore": schema.StringAttribute{
				Required:    true,
				Description: "Select startup or running. A change replaces the resource.",
				Validators: []validator.String{
					stringvalidator.OneOf(datastoreStartup, datastoreRunning),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"module": schema.StringAttribute{
				Required:    true,
				Description: "The installed YANG module must define the content. A changed module name replaces the resource.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(yangIdentifierPattern, "must be a YANG module name"),
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"content": schema.StringAttribute{
				Required: true,
				Description: "Supply the complete module configuration as XML compatible with sysrepocfg --export. " +
					"Comparison ignores comments, namespace prefixes, attribute order, and surrounding text whitespace. " +
					"Apply removes omitted nodes.",
			},
		}),
	}
}

func (r *sysrepoDataResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *sysrepoDataResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sysrepoDataModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.replace(ctx, plan); err != nil {
		resp.Diagnostics.AddError("Import sysrepo data", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Preserve the declared XML formatting when its canonical form matches the export.
func (r *sysrepoDataResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sysrepoDataModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	modules, err := listSysrepoModules(ctx, r.data.client, guest)
	if err != nil {
		resp.Diagnostics.AddError("Read sysrepo data", err.Error())
		return
	}
	if _, installed := sysrepo.FindImplemented(modules, state.Module.ValueString()); !installed {
		// sysrepocfg cannot export an uninstalled module.
		resp.State.RemoveResource(ctx)
		return
	}

	exportCommand := sysrepocfgCommand(
		"--export", "--datastore", state.Datastore.ValueString(),
		"--module", state.Module.ValueString(),
		"--format", "xml", "--defaults", explicitDefaultsMode,
	)
	result, err := runGuest(ctx, r.data.client, guest, exportCommand)
	if err != nil {
		resp.Diagnostics.AddError("Read sysrepo data", err.Error())
		return
	}
	if result.ExitCode != 0 {
		resp.Diagnostics.AddError("Read sysrepo data", commandFailure(guest, exportCommand, result).Error())
		return
	}

	exported := string(result.Stdout)
	exportedCanonical, err := sysrepo.CanonicalXML(exported)
	if err != nil {
		resp.Diagnostics.AddError("Read sysrepo data", "The export of sysrepocfg is not valid XML: "+err.Error())
		return
	}
	declaredCanonical, err := sysrepo.CanonicalXML(state.Content.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read sysrepo data", "The content in state is not valid XML: "+err.Error())
		return
	}
	if exportedCanonical != declaredCanonical {
		state.Content = types.StringValue(exported)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *sysrepoDataResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sysrepoDataModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.replace(ctx, plan); err != nil {
		resp.Diagnostics.AddError("Import sysrepo data", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete requests removal of module data with an empty import.
func (r *sysrepoDataResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sysrepoDataModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	unlock := r.data.sysrepoLocks.lock(guest)
	defer unlock()
	err := runSysrepocfgImport(
		ctx, r.data.client, guest, state.Datastore.ValueString(), state.Module.ValueString(), "",
	)
	if err != nil {
		resp.Diagnostics.AddError("Remove sysrepo data", err.Error())
	}
}

func (r *sysrepoDataResource) replace(ctx context.Context, plan sysrepoDataModel) error {
	content := plan.Content.ValueString()
	canonical, err := sysrepo.CanonicalXML(content)
	if err != nil {
		slog.ErrorContext(ctx, "sysrepo data content is not valid XML", "module", plan.Module.ValueString(), "err", err)
		return fmt.Errorf("the content is not valid XML: %w", err)
	}
	if canonical == "" {
		return errors.New("the content has no XML element")
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	unlock := r.data.sysrepoLocks.lock(guest)
	defer unlock()
	return runSysrepocfgImport(
		ctx, r.data.client, guest, plan.Datastore.ValueString(), plan.Module.ValueString(), content,
	)
}
