package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	// dpkg-query expands this format itself, including the escapes.
	dpkgVersionQueryFormat = `${Package}\t${db:Status-Abbrev}\t${Version}\n`

	debVersionFieldCount = 3
)

type debPackagesResource struct {
	data *providerData
}

type debPackagesModel struct {
	Node        types.String `tfsdk:"node"`
	VMID        types.Int64  `tfsdk:"vmid"`
	Kind        types.String `tfsdk:"kind"`
	Packages    types.Map    `tfsdk:"packages"`
	DebVersions types.Map    `tfsdk:"deb_versions"`
}

func newDebPackagesResource() resource.Resource {
	return &debPackagesResource{}
}

func (r *debPackagesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_deb_packages"
}

func (r *debPackagesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Packages that are installed from .deb files inside a guest. " +
			"The resource installs every package when the installed version differs from the version of its file.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"packages": schema.MapAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Map from Debian package name to the absolute guest path of its .deb file, " +
					"for example a path that a pveguest_download writes.",
				Validators: []validator.Map{
					mapvalidator.SizeAtLeast(1),
					mapvalidator.KeysAre(stringvalidator.RegexMatches(packageNamePattern, "must be a Debian package name")),
					mapvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(absolutePathPattern, "must be an absolute path that ends in a file name"),
					),
				},
			},
			"deb_versions": schema.MapAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Installed version of each package in packages.",
			},
		}),
	}
}

func (r *debPackagesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *debPackagesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan debPackagesModel
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

// Read does not modify the guest. Read removes a package from state when the
// package is not installed or the installed version differs from the version of
// its .deb file, and the next plan installs it. A missing .deb file does not
// remove the package.
func (r *debPackagesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state debPackagesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var declared map[string]string
	resp.Diagnostics.Append(state.Packages.ElementsAs(ctx, &declared, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	fileVersions, err := readDebFileVersions(ctx, r.data.client, guest, declared)
	if err != nil {
		resp.Diagnostics.AddError("Read deb files", err.Error())
		return
	}
	installed, err := readInstalledVersions(ctx, r.data.client, guest, packageNames(declared))
	if err != nil {
		resp.Diagnostics.AddError("Read deb packages", err.Error())
		return
	}

	retained := make(map[string]string, len(declared))
	versions := make(map[string]string, len(declared))
	for name, debPath := range declared {
		installedVersion, isInstalled := installed[name]
		if !isInstalled {
			continue
		}
		fileVersion, hasFile := fileVersions[name]
		if hasFile && fileVersion != installedVersion {
			continue
		}
		retained[name] = debPath
		versions[name] = installedVersion
	}

	packages, diagnostics := types.MapValueFrom(ctx, types.StringType, retained)
	resp.Diagnostics.Append(diagnostics...)
	debVersions, diagnostics := types.MapValueFrom(ctx, types.StringType, versions)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Packages = packages
	state.DebVersions = debVersions
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *debPackagesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan debPackagesModel
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

// The framework removes the resource from state after Delete returns.
func (r *debPackagesResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

// apply installs every package that is missing or has another version than its
// .deb file in one apt-get call, then stores the versions in the plan.
func (r *debPackagesResource) apply(ctx context.Context, plan *debPackagesModel) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	var declared map[string]string
	diagnostics.Append(plan.Packages.ElementsAs(ctx, &declared, false)...)
	if diagnostics.HasError() {
		return diagnostics
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	unlock := r.data.aptLocks.lock(guest)
	defer unlock()

	versions, err := r.installPending(ctx, guest, declared)
	if err != nil {
		diagnostics.AddError("Install deb packages", err.Error())
		return diagnostics
	}
	debVersions, mapDiagnostics := types.MapValueFrom(ctx, types.StringType, versions)
	diagnostics.Append(mapDiagnostics...)
	plan.DebVersions = debVersions
	return diagnostics
}

func (r *debPackagesResource) installPending(
	ctx context.Context,
	guest transport.Guest,
	declared map[string]string,
) (map[string]string, error) {
	names := packageNames(declared)
	fileVersions, err := readDebFileVersions(ctx, r.data.client, guest, declared)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if _, hasFile := fileVersions[name]; !hasFile {
			return nil, fmt.Errorf("%s: the deb file %s of package %s does not exist", guest, declared[name], name)
		}
	}
	installed, err := readInstalledVersions(ctx, r.data.client, guest, names)
	if err != nil {
		return nil, err
	}

	pendingPaths := make([]string, 0, len(names))
	for _, name := range names {
		if installed[name] != fileVersions[name] {
			pendingPaths = append(pendingPaths, declared[name])
		}
	}
	if len(pendingPaths) == 0 {
		return fileVersions, nil
	}

	// --no-download makes a dependency that the guest lacks fail the install
	// with the apt error.
	arguments := append([]string{"install", "-y", "--no-install-recommends", "--no-download", "--"}, pendingPaths...)
	if err := runChecked(ctx, r.data.client, guest, aptCommand(arguments...)); err != nil {
		return nil, err
	}

	installed, err = readInstalledVersions(ctx, r.data.client, guest, names)
	if err != nil {
		return nil, err
	}
	var wrong []string
	for _, name := range names {
		if installed[name] != fileVersions[name] {
			wrong = append(wrong, name)
		}
	}
	if len(wrong) > 0 {
		return nil, fmt.Errorf(
			"%s: packages do not have the version of their deb file after apt-get install: %s",
			guest, strings.Join(wrong, ", "),
		)
	}
	return fileVersions, nil
}

func packageNames(declared map[string]string) []string {
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// readDebFileVersions returns the Version field of each .deb file. The result
// omits a package when its file does not exist.
func readDebFileVersions(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	declared map[string]string,
) (map[string]string, error) {
	versions := make(map[string]string, len(declared))
	for _, name := range packageNames(declared) {
		command := guestCommand("dpkg-deb", "-f", "--", declared[name], "Version")
		result, err := runGuest(ctx, client, guest, command)
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			if strings.Contains(string(result.Stderr), missingFileMessage) {
				continue
			}
			return nil, commandFailure(guest, command, result)
		}
		versions[name] = firstLine(result.Stdout)
	}
	return versions, nil
}

// readInstalledVersions returns the installed version of each package that
// dpkg reports as installed.
func readInstalledVersions(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	names []string,
) (map[string]string, error) {
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	arguments := append([]string{"dpkg-query", "--show", "--showformat", dpkgVersionQueryFormat, "--"}, names...)
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

	installed := make(map[string]string)
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != debVersionFieldCount {
			continue
		}
		if strings.HasPrefix(fields[1], dpkgInstalledPrefix) {
			installed[fields[0]] = strings.TrimSpace(fields[2])
		}
	}
	return installed, nil
}
