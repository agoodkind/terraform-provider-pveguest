package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	aptTimeoutSeconds = 1800

	// dpkg-query expands this format itself, including the escapes.
	dpkgQueryFormat = `${Package}\t${db:Status-Abbrev}\n`

	// The first two characters of the status abbreviation are the desired
	// action and the current status. "ii" is an installed package.
	dpkgInstalledPrefix = "ii"

	dpkgNoMatchMessage = "no packages found matching"
)

// The Debian policy pattern for package names. It also rejects a name that
// starts with a dash, which apt-get would read as an option.
var packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)

type aptPackagesResource struct {
	data *providerData
}

type aptPackagesModel struct {
	Node     types.String `tfsdk:"node"`
	VMID     types.Int64  `tfsdk:"vmid"`
	Kind     types.String `tfsdk:"kind"`
	Packages types.Set    `tfsdk:"packages"`
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
			"The resource installs missing packages and removes none.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"packages": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Package names without a version or an architecture.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(packageNamePattern, "must be a Debian package name"),
					),
				},
			},
		}),
	}
}

func (r *aptPackagesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *aptPackagesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan aptPackagesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.install(ctx, plan)...)
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
	var declared []string
	resp.Diagnostics.Append(state.Packages.ElementsAs(ctx, &declared, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	installed, err := readInstalledPackages(ctx, r.data.pool, guest, declared)
	if err != nil {
		resp.Diagnostics.AddError("Read apt packages", err.Error())
		return
	}

	present := make([]string, 0, len(declared))
	for _, name := range declared {
		if installed[name] {
			present = append(present, name)
		}
	}
	presentSet, diagnostics := types.SetValueFrom(ctx, types.StringType, present)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Packages = presentSet
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *aptPackagesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan aptPackagesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.install(ctx, plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete runs no command in the guest. The framework removes the resource
// from state, and the packages stay installed.
func (r *aptPackagesResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *aptPackagesResource) install(ctx context.Context, plan aptPackagesModel) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	var declared []string
	diagnostics.Append(plan.Packages.ElementsAs(ctx, &declared, false)...)
	if diagnostics.HasError() {
		return diagnostics
	}
	sort.Strings(declared)

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	unlock := r.data.aptLocks.lock(guest)
	defer unlock()

	installed, err := readInstalledPackages(ctx, r.data.pool, guest, declared)
	if err != nil {
		diagnostics.AddError("Read apt packages", err.Error())
		return diagnostics
	}
	missing := missingPackages(declared, installed)
	if len(missing) == 0 {
		return diagnostics
	}

	// Error-Mode=any makes apt-get update exit nonzero when an index
	// download fails. The default mode prints a warning and exits zero.
	updateCommand := aptCommand("update", "-o", "APT::Update::Error-Mode=any")
	if _, err := runChecked(ctx, r.data.pool, guest, updateCommand); err != nil {
		diagnostics.AddError("apt-get update failed", err.Error())
		return diagnostics
	}

	installArguments := append([]string{"install", "-y", "--no-install-recommends", "--"}, missing...)
	if _, err := runChecked(ctx, r.data.pool, guest, aptCommand(installArguments...)); err != nil {
		diagnostics.AddError("apt-get install failed", err.Error())
		return diagnostics
	}

	installed, err = readInstalledPackages(ctx, r.data.pool, guest, declared)
	if err != nil {
		diagnostics.AddError("Read apt packages", err.Error())
		return diagnostics
	}
	stillMissing := missingPackages(declared, installed)
	if len(stillMissing) > 0 {
		diagnostics.AddError(
			"Packages are not installed after apt-get install",
			fmt.Sprintf("%s: %s", guest, strings.Join(stillMissing, ", ")),
		)
	}
	return diagnostics
}

func aptCommand(arguments ...string) transport.Command {
	argv := append([]string{"env", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "apt-get"}, arguments...)
	return transport.Command{Argv: argv, TimeoutSeconds: aptTimeoutSeconds}
}

func missingPackages(declared []string, installed map[string]bool) []string {
	missing := make([]string, 0, len(declared))
	for _, name := range declared {
		if !installed[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func readInstalledPackages(
	ctx context.Context,
	pool *transport.Pool,
	guest transport.Guest,
	names []string,
) (map[string]bool, error) {
	if len(names) == 0 {
		return map[string]bool{}, nil
	}
	arguments := append([]string{"dpkg-query", "--show", "--showformat", dpkgQueryFormat, "--"}, names...)
	command := guestCommand(arguments...)
	result, err := pool.Run(ctx, guest, command)
	if err != nil {
		return nil, err
	}
	// dpkg-query exits 1 when at least one name matches no package and
	// still prints every package that it found.
	noMatch := result.ExitCode == 1 && strings.Contains(string(result.Stderr), dpkgNoMatchMessage)
	if result.ExitCode != 0 && !noMatch {
		return nil, commandFailure(guest, command, result)
	}
	return parseDpkgQueryOutput(string(result.Stdout)), nil
}

func parseDpkgQueryOutput(output string) map[string]bool {
	installed := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		name, status, hasStatus := strings.Cut(line, "\t")
		if !hasStatus {
			continue
		}
		if strings.HasPrefix(status, dpkgInstalledPrefix) {
			installed[name] = true
		}
	}
	return installed
}
