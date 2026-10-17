// Package provider implements the pveguest OpenTofu provider and its resources
// for files, links, apt packages, and systemd units inside Proxmox guests.
package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	aptTimeoutSeconds = 1800

	// dpkg-query expands this format itself, including the escapes.
	dpkgQueryFormat = `${Package}\t${db:Status-Abbrev}\n`

	// dpkg-query prints a status abbreviation for each package with the format
	// ${db:Status-Abbrev}. The first two characters of the status abbreviation are
	// the desired action and the current status. "ii" is an installed package.
	// "hi" is an installed package with a hold that apt-mark hold sets.
	dpkgInstalledPrefix     = "ii"
	dpkgHeldInstalledPrefix = "hi"

	dpkgNoMatchMessage = "no packages found matching"

	aptMarkHold   = "hold"
	aptMarkUnhold = "unhold"

	architectureSeparator = ":"
)

// The pattern is the Debian policy pattern for package names. The pattern
// also rejects a name that starts with a dash. apt-get reads such a name as
// an option.
var packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

// packageVersionPattern accepts an optional numeric epoch and colon before a
// required digit. It permits letters, digits, periods, plus signs, tildes, and
// hyphens after that digit.
var packageVersionPattern = regexp.MustCompile(`^([0-9]+:)?[0-9][A-Za-z0-9.+~-]*$`)

type aptPackagesResource struct {
	data *providerData
}

type aptPackagesModel struct {
	Node           types.String `tfsdk:"node"`
	VMID           types.Int64  `tfsdk:"vmid"`
	Kind           types.String `tfsdk:"kind"`
	Packages       types.Set    `tfsdk:"packages"`
	Versions       types.Map    `tfsdk:"versions"`
	Locked         types.Set    `tfsdk:"locked"`
	RemoveOnDelete types.Bool   `tfsdk:"remove_on_delete"`
}

// aptDesired stores package names, exact versions, lock selections, and the
// removal setting as Go values.
type aptDesired struct {
	Packages       []string
	Versions       map[string]string
	Locked         []string
	RemoveOnDelete bool
}

func newAptPackagesResource() resource.Resource {
	return &aptPackagesResource{}
}

func (r *aptPackagesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_apt_packages"
}

func (r *aptPackagesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A set of apt packages that are installed inside a guest. " +
			"The resource installs each missing package.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"packages": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Specify Debian package names without version or architecture suffixes.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(packageNamePattern, "must be a Debian package name"),
					),
				},
			},
			"versions": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "The versions map associates package names with exact Debian versions. Every key must also appear in packages.",
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringvalidator.RegexMatches(packageNamePattern, "must be a Debian package name")),
					mapvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(packageVersionPattern, "must start with a digit after an optional numeric epoch and colon, with only letters, digits, periods, plus signs, tildes, or hyphens after that digit"),
					),
				},
			},
			"locked": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "The provider runs apt-mark hold on the guest for each package in locked. Every package name must also appear in packages.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(packageNamePattern, "must be a Debian package name"),
					),
				},
			},
			"remove_on_delete": schema.BoolAttribute{
				Optional:    true,
				Description: "When remove_on_delete is true, the provider runs apt-get remove during apply for packages that the configuration removes from packages and removes all listed packages during destroy. When remove_on_delete is false or unset, the provider keeps the packages installed and changes only OpenTofu state.",
			},
		}),
	}
}

func (r *aptPackagesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *aptPackagesResource) ValidateConfig(
	ctx context.Context,
	req resource.ValidateConfigRequest,
	resp *resource.ValidateConfigResponse,
) {
	var config aptPackagesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if config.Packages.IsUnknown() {
		return
	}
	declared, hasUnknown := knownStrings(config.Packages.Elements())
	if hasUnknown {
		return
	}

	versionKeys := make([]string, 0, len(config.Versions.Elements()))
	for name := range config.Versions.Elements() {
		versionKeys = append(versionKeys, name)
	}
	for _, name := range subtractNames(versionKeys, declared) {
		resp.Diagnostics.AddAttributeError(
			path.Root("versions").AtMapKey(name), "versions key missing from packages", "versions key missing from packages: "+name,
		)
	}

	lockedNames, _ := knownStrings(config.Locked.Elements())
	for _, name := range subtractNames(lockedNames, declared) {
		resp.Diagnostics.AddAttributeError(
			path.Root("locked"), "locked package missing from packages", "locked package missing from packages: "+name,
		)
	}
}

// ModifyPlan warns which declared packages apply or destroy will remove when
// remove_on_delete is true.
func (r *aptPackagesResource) ModifyPlan(
	ctx context.Context,
	req resource.ModifyPlanRequest,
	resp *resource.ModifyPlanResponse,
) {
	if req.State.Raw.IsNull() {
		return
	}
	var state aptPackagesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior, diagnostics := desiredFrom(ctx, state)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	if req.Plan.Raw.IsNull() {
		if prior.RemoveOnDelete && len(prior.Packages) > 0 {
			resp.Diagnostics.AddWarning("Destroy will remove these packages", strings.Join(prior.Packages, ", "))
		}
		return
	}

	var plan aptPackagesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() || plan.Packages.IsUnknown() || !plan.RemoveOnDelete.ValueBool() {
		return
	}
	planned, hasUnknown := knownStrings(plan.Packages.Elements())
	if hasUnknown {
		return
	}
	dropped := subtractNames(prior.Packages, planned)
	if len(dropped) > 0 {
		resp.Diagnostics.AddWarning("Apply will remove these packages", strings.Join(dropped, ", "))
	}
}

func (r *aptPackagesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan aptPackagesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, plan, nil)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *aptPackagesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state aptPackagesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	desired, diagnostics := desiredFrom(ctx, state)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	installed, err := readInstalledState(ctx, r.data.client, guest, desired.Packages, len(desired.Versions) > 0)
	if err != nil {
		resp.Diagnostics.AddError("Read apt packages", err.Error())
		return
	}

	present := make([]string, 0, len(desired.Packages))
	for _, name := range desired.Packages {
		_, isInstalled := installed[name]
		if isInstalled {
			present = append(present, name)
		}
	}
	presentSet, diagnostics := types.SetValueFrom(ctx, types.StringType, present)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Packages = presentSet

	if !state.Versions.IsNull() {
		reported := make(map[string]string, len(desired.Versions))
		for name := range desired.Versions {
			installedVersion, isInstalled := installed[name]
			if isInstalled {
				reported[name] = installedVersion
			}
		}
		versions, versionDiagnostics := types.MapValueFrom(ctx, types.StringType, reported)
		resp.Diagnostics.Append(versionDiagnostics...)
		state.Versions = versions
	}

	if !state.Locked.IsNull() {
		held, holdErr := readHeldPackages(ctx, r.data.client, guest)
		if holdErr != nil {
			resp.Diagnostics.AddError("apt-mark showhold failed", holdErr.Error())
			return
		}
		stillLocked := make([]string, 0, len(desired.Locked))
		for _, name := range desired.Locked {
			if held[name] {
				stillLocked = append(stillLocked, name)
			}
		}
		locked, lockedDiagnostics := types.SetValueFrom(ctx, types.StringType, stillLocked)
		resp.Diagnostics.Append(lockedDiagnostics...)
		state.Locked = locked
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *aptPackagesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan aptPackagesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	var state aptPackagesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, plan, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// The framework removes the resource from state after Delete returns.
func (r *aptPackagesResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state aptPackagesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !state.RemoveOnDelete.ValueBool() {
		return
	}
	desired, diagnostics := desiredFrom(ctx, state)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	unlock := r.data.aptLocks.lock(guest)
	defer unlock()
	resp.Diagnostics.Append(r.removePackages(ctx, guest, desired.Packages)...)
}

// apply is the aptPackagesResource method that the provider runs under the
// guest apt lock. The provider runs apt-mark unhold for packages that the
// locked attribute no longer includes. The provider installs missing packages
// or corrects version differences. The provider removes dropped packages when
// remove_on_delete is true. The provider runs apt-mark hold for the packages in
// locked.
func (r *aptPackagesResource) apply(ctx context.Context, plan aptPackagesModel, prior *aptPackagesModel) diag.Diagnostics {
	desired, diagnostics := desiredFrom(ctx, plan)
	if diagnostics.HasError() {
		return diagnostics
	}
	var previous aptDesired
	if prior != nil {
		var priorDiagnostics diag.Diagnostics
		previous, priorDiagnostics = desiredFrom(ctx, *prior)
		diagnostics.Append(priorDiagnostics...)
		if diagnostics.HasError() {
			return diagnostics
		}
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	unlock := r.data.aptLocks.lock(guest)
	defer unlock()

	unlocked := subtractNames(previous.Locked, desired.Locked)
	if len(unlocked) > 0 {
		err := runChecked(ctx, r.data.client, guest, aptMarkCommand(aptMarkUnhold, unlocked))
		if err != nil {
			diagnostics.AddError("apt-mark unhold failed", err.Error())
			return diagnostics
		}
	}

	diagnostics.Append(r.install(ctx, guest, desired)...)
	if diagnostics.HasError() {
		return diagnostics
	}

	if desired.RemoveOnDelete {
		dropped := subtractNames(previous.Packages, desired.Packages)
		diagnostics.Append(r.removePackages(ctx, guest, dropped)...)
		if diagnostics.HasError() {
			return diagnostics
		}
	}

	if len(desired.Locked) > 0 {
		err := runChecked(ctx, r.data.client, guest, aptMarkCommand(aptMarkHold, desired.Locked))
		if err != nil {
			diagnostics.AddError("apt-mark hold failed", err.Error())
		}
	}
	return diagnostics
}

func (r *aptPackagesResource) install(ctx context.Context, guest transport.Guest, desired aptDesired) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	versioned := len(desired.Versions) > 0
	installed, err := readInstalledState(ctx, r.data.client, guest, desired.Packages, versioned)
	if err != nil {
		diagnostics.AddError("Read apt packages", err.Error())
		return diagnostics
	}
	pending := pendingPackages(desired.Packages, desired.Versions, installed)
	if len(pending) == 0 {
		return diagnostics
	}

	// Error-Mode=any makes apt-get update exit nonzero when an index
	// download fails. The default mode prints a warning and exits zero.
	updateCommand := aptCommand("update", "-o", "APT::Update::Error-Mode=any")
	if err := runChecked(ctx, r.data.client, guest, updateCommand); err != nil {
		diagnostics.AddError("apt-get update failed", err.Error())
		return diagnostics
	}

	changesVersions := versioned || len(desired.Locked) > 0
	installCommand := aptInstallCommand(installSpecs(pending, desired.Versions), changesVersions)
	if err := runChecked(ctx, r.data.client, guest, installCommand); err != nil {
		diagnostics.AddError("apt-get install failed", err.Error())
		return diagnostics
	}

	installed, err = readInstalledState(ctx, r.data.client, guest, desired.Packages, versioned)
	if err != nil {
		diagnostics.AddError("Read apt packages", err.Error())
		return diagnostics
	}
	var stillMissing []string
	var wrongVersion []string
	for _, name := range pendingPackages(desired.Packages, desired.Versions, installed) {
		installedVersion, isInstalled := installed[name]
		if isInstalled {
			entry := fmt.Sprintf("%s, %s, %s", name, installedVersion, desired.Versions[name])
			wrongVersion = append(wrongVersion, entry)
		} else {
			stillMissing = append(stillMissing, name)
		}
	}
	if len(stillMissing) > 0 {
		diagnostics.AddError(
			"Packages are not installed after apt-get install",
			fmt.Sprintf("%s: %s", guest, strings.Join(stillMissing, ", ")),
		)
	}
	if len(wrongVersion) > 0 {
		diagnostics.AddError(
			"Installed package versions differ from requested versions.",
			fmt.Sprintf(
				"After apt-get install, some packages on guest %s have installed versions that differ "+
					"from their requested versions: %s. "+
					"Each entry lists the package name, installed version, and requested version.",
				guest, strings.Join(wrongVersion, "; "),
			),
		)
	}
	return diagnostics
}

func (r *aptPackagesResource) removePackages(ctx context.Context, guest transport.Guest, names []string) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	if len(names) == 0 {
		return diagnostics
	}
	installed, err := readInstalledPackages(ctx, r.data.client, guest, names)
	if err != nil {
		diagnostics.AddError("Read apt packages", err.Error())
		return diagnostics
	}
	present := make([]string, 0, len(names))
	for _, name := range names {
		if installed[name] {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return diagnostics
	}

	if err := runChecked(ctx, r.data.client, guest, aptMarkCommand(aptMarkUnhold, present)); err != nil {
		diagnostics.AddError("apt-mark unhold failed", err.Error())
		return diagnostics
	}
	if err := runChecked(ctx, r.data.client, guest, aptRemoveCommand(present)); err != nil {
		diagnostics.AddError("apt-get remove failed", err.Error())
	}
	return diagnostics
}

func aptCommand(arguments ...string) transport.Command {
	argv := append([]string{"env", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "apt-get"}, arguments...)
	return transport.Command{Argv: argv, TimeoutSeconds: aptTimeoutSeconds}
}

// aptInstallCommand adds options that permit downgrades and changes to held
// packages when changesVersions is true.
func aptInstallCommand(specs []string, changesVersions bool) transport.Command {
	arguments := []string{"install", "-y", "--no-install-recommends"}
	if changesVersions {
		arguments = append(arguments, "--allow-downgrades", "--allow-change-held-packages")
	}
	arguments = append(arguments, "-o", "DPkg::Lock::Timeout=300", "--")
	return aptCommand(append(arguments, specs...)...)
}

func aptRemoveCommand(names []string) transport.Command {
	return aptCommand(append([]string{"remove", "-y", "--"}, names...)...)
}

func aptMarkCommand(verb string, names []string) transport.Command {
	return guestCommand(append([]string{"apt-mark", verb, "--"}, names...)...)
}

func installSpecs(names []string, versions map[string]string) []string {
	specs := make([]string, 0, len(names))
	for _, name := range names {
		version, isPinned := versions[name]
		if isPinned {
			specs = append(specs, name+"="+version)
		} else {
			specs = append(specs, name)
		}
	}
	return specs
}

func pendingPackages(declared []string, versions map[string]string, installed map[string]string) []string {
	pending := make([]string, 0, len(declared))
	for _, name := range declared {
		installedVersion, isInstalled := installed[name]
		wantedVersion, isPinned := versions[name]
		if !isInstalled || (isPinned && installedVersion != wantedVersion) {
			pending = append(pending, name)
		}
	}
	return pending
}

func desiredFrom(ctx context.Context, model aptPackagesModel) (aptDesired, diag.Diagnostics) {
	var diagnostics diag.Diagnostics
	desired := aptDesired{
		Versions:       map[string]string{},
		RemoveOnDelete: model.RemoveOnDelete.ValueBool(),
	}
	diagnostics.Append(model.Packages.ElementsAs(ctx, &desired.Packages, false)...)
	if !model.Versions.IsNull() {
		diagnostics.Append(model.Versions.ElementsAs(ctx, &desired.Versions, false)...)
	}
	if !model.Locked.IsNull() {
		diagnostics.Append(model.Locked.ElementsAs(ctx, &desired.Locked, false)...)
	}
	sort.Strings(desired.Packages)
	sort.Strings(desired.Locked)
	return desired, diagnostics
}

func knownStrings(elements []attr.Value) ([]string, bool) {
	names := make([]string, 0, len(elements))
	hasUnknown := false
	for _, element := range elements {
		text, isString := element.(types.String)
		if !isString || text.IsNull() {
			continue
		}
		if text.IsUnknown() {
			hasUnknown = true
			continue
		}
		names = append(names, text.ValueString())
	}
	return names, hasUnknown
}

func subtractNames(names []string, removed []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		if !slices.Contains(removed, name) {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

// readInstalledState maps installed package names to versions when
// withVersions is true and to empty strings otherwise.
func readInstalledState(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	names []string,
	withVersions bool,
) (map[string]string, error) {
	if withVersions {
		return readInstalledVersions(ctx, client, guest, names)
	}
	installed, err := readInstalledPackages(ctx, client, guest, names)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(installed))
	for name, isInstalled := range installed {
		if isInstalled {
			result[name] = ""
		}
	}
	return result, nil
}

func readHeldPackages(ctx context.Context, client *transport.Client, guest transport.Guest) (map[string]bool, error) {
	command := guestCommand("apt-mark", "showhold")
	result, err := runGuest(ctx, client, guest, command)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(guest, command, result)
	}
	return parseHeldPackages(string(result.Stdout)), nil
}

func parseHeldPackages(output string) map[string]bool {
	held := make(map[string]bool)
	for line := range strings.SplitSeq(output, "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), architectureSeparator)
		if name != "" {
			held[name] = true
		}
	}
	return held
}

func readInstalledPackages(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	names []string,
) (map[string]bool, error) {
	if len(names) == 0 {
		return map[string]bool{}, nil
	}
	arguments := append([]string{"dpkg-query", "--show", "--showformat", dpkgQueryFormat, "--"}, names...)
	command := guestCommand(arguments...)
	result, err := runGuest(ctx, client, guest, command)
	if err != nil {
		return nil, err
	}
	// dpkg-query returns status 1 when a requested package is not installed.
	// Its output still includes the installed packages.
	noMatch := result.ExitCode == 1 && strings.Contains(string(result.Stderr), dpkgNoMatchMessage)
	if result.ExitCode != 0 && !noMatch {
		return nil, commandFailure(guest, command, result)
	}
	return parseDpkgQueryOutput(string(result.Stdout)), nil
}

func dpkgStatusInstalled(status string) bool {
	return strings.HasPrefix(status, dpkgInstalledPrefix) || strings.HasPrefix(status, dpkgHeldInstalledPrefix)
}

func parseDpkgQueryOutput(output string) map[string]bool {
	installed := make(map[string]bool)
	for line := range strings.SplitSeq(output, "\n") {
		name, status, hasStatus := strings.Cut(line, "\t")
		if !hasStatus {
			continue
		}
		if dpkgStatusInstalled(status) {
			installed[name] = true
		}
	}
	return installed
}
