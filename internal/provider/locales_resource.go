package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	localeGenTimeoutSeconds = 600

	localeEntrySeparator    = " "
	localeCodesetSeparator  = "."
	localeModifierSeparator = "@"

	// digitCodesetPrefix matches glibc's prefix for codesets without letters.
	digitCodesetPrefix = "iso"

	readLocalesSummary = "locale -a failed"
)

var localeEntryPattern = regexp.MustCompile(
	`^[A-Za-z0-9_]+(\.[A-Za-z0-9_-]+)?(@[A-Za-z0-9_]+)? [A-Za-z0-9_.:-]+$`,
)

type localesResource struct {
	data *providerData
}

type localesModel struct {
	Node       types.String `tfsdk:"node"`
	VMID       types.Int64  `tfsdk:"vmid"`
	Kind       types.String `tfsdk:"kind"`
	Locales    types.Set    `tfsdk:"locales"`
	GenerateOn types.Map    `tfsdk:"generate_on"`
}

func newLocalesResource() resource.Resource {
	return &localesResource{}
}

func (r *localesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_locales"
}

func (r *localesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The resource declares a set of generated locales inside a guest. " +
			"Apply runs locale-gen when a declared locale is absent. " +
			"Destroy removes the resource from state.",
		Attributes: withGuestAttributes(map[string]schema.Attribute{
			"locales": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Each entry uses the form <locale> <charmap>, for example en_US.UTF-8 UTF-8. " +
					"The guest's /etc/locale.gen must contain each declared entry.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(
						stringvalidator.RegexMatches(localeEntryPattern, "must be a locale with optional .codeset and @modifier, followed by one space and a charmap"),
					),
				},
			},
			"generate_on": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "The map accepts arbitrary string values, usually the write_id of the /etc/locale.gen file. " +
					"A changed map runs locale-gen during Update even when every declared locale exists. " +
					"Create does not use the map.",
			},
		}),
	}
}

func (r *localesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = providerDataFrom(req, resp)
}

func (r *localesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan localesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.generate(ctx, plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *localesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state localesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	declared, diagnostics := stringValues(ctx, state.Locales)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}

	guest := guestOf(state.Node, state.VMID, state.Kind)
	generated, err := readGeneratedLocales(ctx, r.data.client, guest)
	if err != nil {
		resp.Diagnostics.AddError(readLocalesSummary, err.Error())
		return
	}

	present := make([]string, 0, len(declared))
	for _, entry := range declared {
		if generated[archivedLocaleName(entry)] {
			present = append(present, entry)
		}
	}
	presentSet, diagnostics := stringSetOf(ctx, present)
	resp.Diagnostics.Append(diagnostics...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Locales = presentSet
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *localesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan localesModel
	var state localesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	regenerate := !plan.GenerateOn.Equal(state.GenerateOn)
	resp.Diagnostics.Append(r.generate(ctx, plan, regenerate)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete relies on the framework to remove the resource from state without deleting guest locales.
func (r *localesResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *localesResource) generate(ctx context.Context, plan localesModel, regenerate bool) diag.Diagnostics {
	var diagnostics diag.Diagnostics
	declared, valueDiagnostics := stringValues(ctx, plan.Locales)
	diagnostics.Append(valueDiagnostics...)
	if diagnostics.HasError() {
		return diagnostics
	}

	guest := guestOf(plan.Node, plan.VMID, plan.Kind)
	generated, err := readGeneratedLocales(ctx, r.data.client, guest)
	if err != nil {
		diagnostics.AddError(readLocalesSummary, err.Error())
		return diagnostics
	}
	if len(missingLocales(declared, generated)) == 0 && !regenerate {
		return diagnostics
	}

	// locale-gen rebuilds the archive from /etc/locale.gen but ignores failed localedef calls.
	// The subsequent locale -a check detects missing declared locales.
	command := guestCommand("locale-gen")
	command.TimeoutSeconds = localeGenTimeoutSeconds
	if err := runChecked(ctx, r.data.client, guest, command); err != nil {
		diagnostics.AddError("locale-gen failed", err.Error())
		return diagnostics
	}

	generated, err = readGeneratedLocales(ctx, r.data.client, guest)
	if err != nil {
		diagnostics.AddError(readLocalesSummary, err.Error())
		return diagnostics
	}
	stillMissing := missingLocales(declared, generated)
	if len(stillMissing) > 0 {
		diagnostics.AddError(
			"Declared locales are absent after locale-gen",
			fmt.Sprintf("%s: %s", guest, strings.Join(stillMissing, ", ")),
		)
	}
	return diagnostics
}

func missingLocales(declared []string, generated map[string]bool) []string {
	missing := make([]string, 0, len(declared))
	for _, entry := range declared {
		if !generated[archivedLocaleName(entry)] {
			missing = append(missing, entry)
		}
	}
	return missing
}

func readGeneratedLocales(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
) (map[string]bool, error) {
	command := guestCommand("locale", "-a")
	result, err := runGuest(ctx, client, guest, command)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(guest, command, result)
	}
	generated := make(map[string]bool)
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			generated[name] = true
		}
	}
	return generated, nil
}

// archivedLocaleName uses glibc archive naming to compare an entry with locale -a output.
func archivedLocaleName(entry string) string {
	name, _, _ := strings.Cut(entry, localeEntrySeparator)
	base, modifier, hasModifier := strings.Cut(name, localeModifierSeparator)
	stem, codeset, hasCodeset := strings.Cut(base, localeCodesetSeparator)
	if !hasCodeset {
		return name
	}
	archived := stem + localeCodesetSeparator + normalizedCodeset(codeset)
	if hasModifier {
		archived += localeModifierSeparator + modifier
	}
	return archived
}

// normalizedCodeset matches glibc _nl_normalize_codeset.
func normalizedCodeset(codeset string) string {
	var builder strings.Builder
	hasLetter := false
	for _, character := range strings.ToLower(codeset) {
		isLetter := character >= 'a' && character <= 'z'
		isDigit := character >= '0' && character <= '9'
		if isLetter {
			hasLetter = true
		}
		if isLetter || isDigit {
			builder.WriteRune(character)
		}
	}
	if hasLetter {
		return builder.String()
	}
	return digitCodesetPrefix + builder.String()
}
