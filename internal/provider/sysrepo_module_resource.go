package provider

import (
	"context"
	"fmt"
	"log/slog"
	"path"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/sysrepo"
	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

type sysrepoModuleResource struct {
	data *providerData
}

type sysrepoModuleModel struct {
	Node     types.String `tfsdk:"node"`
	VMID     types.Int64  `tfsdk:"vmid"`
	Kind     types.String `tfsdk:"kind"`
	Path     types.String `tfsdk:"path"`
	Features types.Set    `tfsdk:"features"`
	Update   types.Bool   `tfsdk:"update"`
	Module   types.String `tfsdk:"module"`
	Revision types.String `tfsdk:"revision"`
}

func newSysrepoModuleResource() resource.Resource {
	return &sysrepoModuleResource{}
}

func (r *sysrepoModuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sysrepo_module"
}

func (r *sysrepoModuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A YANG module that is installed in the sysrepo repository of a guest, with its enabled features. " +
			"Destroy uninstalls the module.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				Description: "Absolute path of the module file in the guest. The file name has the form " +
					"<module>@<revision>.yang. The directory of the file is the search directory for imports.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(absolutePathPattern, "must be an absolute path that ends in a file name"),
				},
			},
			"features": schema.SetAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default:     setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				Description: "Features that are enabled in the installed module. The default is none.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(yangIdentifierPattern, "must be a YANG feature name"),
					),
				},
			},
			"update": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "Whether an apply replaces an installed module that has another revision than the file. " +
					"The default is false, and an apply then fails for a module at another revision.",
			},
			"module": schema.StringAttribute{
				Computed:    true,
				Description: "Module name from the file name. A changed name replaces the resource.",
			},
			"revision": schema.StringAttribute{
				Computed:    true,
				Description: "Revision from the file name in the plan, and the installed revision after a refresh.",
			},
		}),
	}
}

func (r *sysrepoModuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *sysrepoModuleResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan sysrepoModuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Path.IsUnknown() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("module"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("revision"), types.StringUnknown())...)
		return
	}

	file, err := sysrepo.ParseModuleFileName(plan.Path.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(fwpath.Root("path"), "Invalid module file name", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("module"), types.StringValue(file.Module))...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, fwpath.Root("revision"), types.StringValue(file.Revision))...)

	if req.State.Raw.IsNull() {
		return
	}
	var state sysrepoModuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if state.Module.ValueString() != file.Module {
		resp.RequiresReplace.Append(fwpath.Root("path"))
	}
}

func (r *sysrepoModuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sysrepoModuleModel
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

// Read removes the resource from state when the module is not installed. A
// revision or feature set that differs from the declaration plans an update.
func (r *sysrepoModuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sysrepoModuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	modules, err := listSysrepoModules(ctx, r.data.client, guest)
	if err != nil {
		resp.Diagnostics.AddError("Read sysrepo module", err.Error())
		return
	}
	installed, found := sysrepo.FindImplemented(modules, state.Module.ValueString())
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if keepsInstalledModule(state.Update.ValueBool(), found) {
		return
	}

	features, diagnostics := stringSetOf(ctx, installed.Features)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Revision = types.StringValue(installed.Revision)
	state.Features = features
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *sysrepoModuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan sysrepoModuleModel
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

func (r *sysrepoModuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sysrepoModuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	guest := guestOf(state.Node, state.VMID, state.Kind)
	unlock := r.data.sysrepoLocks.lock(guest)
	defer unlock()

	command := sysrepoctlCommand("--uninstall", state.Module.ValueString())
	if err := runChecked(ctx, r.data.client, guest, command); err != nil {
		resp.Diagnostics.AddError("Uninstall sysrepo module", err.Error())
	}
}

func (r *sysrepoModuleResource) apply(ctx context.Context, plan *sysrepoModuleModel) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	features, featureDiagnostics := stringValues(ctx, plan.Features)
	diagnostics.Append(featureDiagnostics...)
	file, err := sysrepo.ParseModuleFileName(plan.Path.ValueString())
	if err != nil {
		diagnostics.AddAttributeError(fwpath.Root("path"), "Invalid module file name", err.Error())
	}
	if diagnostics.HasError() {
		return diagnostics
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	unlock := r.data.sysrepoLocks.lock(guest)
	defer unlock()

	if err := r.converge(ctx, guest, plan, file, features); err != nil {
		diagnostics.AddError("Install sysrepo module", err.Error())
		return diagnostics
	}
	plan.Module = types.StringValue(file.Module)
	plan.Revision = types.StringValue(file.Revision)
	return diagnostics
}

// converge installs the module when it is absent, updates it when it has
// another revision and update is set, and then enables and disables features
// until the installed features equal the declared features. It fails when the
// installed module does not match the declaration afterwards.
func (r *sysrepoModuleResource) converge(
	ctx context.Context,
	guest transport.Guest,
	plan *sysrepoModuleModel,
	file sysrepo.ModuleFile,
	features []string,
) error {
	client := r.data.client
	modulePath := plan.Path.ValueString()
	searchDirectory := path.Dir(modulePath)

	modules, err := listSysrepoModules(ctx, client, guest)
	if err != nil {
		return err
	}
	installed, isInstalled := sysrepo.FindImplemented(modules, file.Module)
	if keepsInstalledModule(plan.Update.ValueBool(), isInstalled) {
		return nil
	}
	switch {
	case !isInstalled:
		arguments := []string{"--install", modulePath, "--search-dirs", searchDirectory}
		for _, feature := range features {
			arguments = append(arguments, "--enable-feature", feature)
		}
		if err := runChecked(ctx, client, guest, sysrepoctlCommand(arguments...)); err != nil {
			return err
		}
	case installed.Revision != file.Revision:
		updateCommand := sysrepoctlCommand("--update", modulePath, "--search-dirs", searchDirectory)
		if err := runChecked(ctx, client, guest, updateCommand); err != nil {
			return err
		}
	}

	installed, err = r.requireInstalled(ctx, guest, file.Module)
	if err != nil {
		return err
	}
	enable := difference(features, installed.Features)
	disable := difference(installed.Features, features)
	if len(enable) > 0 || len(disable) > 0 {
		arguments := []string{"--change", file.Module}
		for _, feature := range enable {
			arguments = append(arguments, "--enable-feature", feature)
		}
		for _, feature := range disable {
			arguments = append(arguments, "--disable-feature", feature)
		}
		if err := runChecked(ctx, client, guest, sysrepoctlCommand(arguments...)); err != nil {
			return err
		}
		installed, err = r.requireInstalled(ctx, guest, file.Module)
		if err != nil {
			return err
		}
	}

	wrongFeatures := len(difference(features, installed.Features)) > 0 ||
		len(difference(installed.Features, features)) > 0
	if installed.Revision != file.Revision || wrongFeatures {
		mismatchErr := fmt.Errorf(
			"%s: module %s has revision %s and features %v after the install, want revision %s and features %v",
			guest, file.Module, installed.Revision, installed.Features, file.Revision, features,
		)
		slog.ErrorContext(ctx, "installed sysrepo module differs from the declaration", "err", mismatchErr)
		return mismatchErr
	}
	return nil
}

func keepsInstalledModule(update bool, isInstalled bool) bool {
	return isInstalled && !update
}

func (r *sysrepoModuleResource) requireInstalled(
	ctx context.Context,
	guest transport.Guest,
	module string,
) (sysrepo.InstalledModule, error) {
	modules, err := listSysrepoModules(ctx, r.data.client, guest)
	if err != nil {
		return sysrepo.InstalledModule{}, err
	}
	installed, found := sysrepo.FindImplemented(modules, module)
	if !found {
		missingErr := fmt.Errorf("%s: module %s is not installed after the sysrepoctl command", guest, module)
		slog.ErrorContext(ctx, "sysrepo module is missing after a sysrepoctl command", "err", missingErr)
		return sysrepo.InstalledModule{}, missingErr
	}
	return installed, nil
}
