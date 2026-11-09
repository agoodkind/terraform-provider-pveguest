package provider

import (
	"context"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	// The hostartifact task runs gh attestation verify with a 300-second
	// limit, waits up to 10 seconds for the target lock, and then writes the
	// files.
	hostArtifactTaskTimeout = 10 * time.Minute

	// The private state key stores the observed_sha256 that the node reported
	// after the last apply.
	hostArtifactAppliedObservedEntry = "applied_observed_sha256"
	// The private state key stores whether the last Read found a file or owned
	// directory that differs from the installed record.
	hostArtifactMismatchEntry = "observation_mismatch"

	hostArtifactImportFormat  = "<node>:<workspace>"
	hostArtifactImportSummary = "Cannot import host artifact"
	hostArtifactImportSep     = ":"
)

var (
	regularFileTypes = []string{"regular file", "regular empty file"}

	httpsURLPattern = regexp.MustCompile(`^https://[^\s\x00]+$`)

	hostArtifactNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
	hostArtifactRevisionPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hostArtifactRepositoryPattern = regexp.MustCompile(
		`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`,
	)
)

type hostArtifactResource struct {
	data *providerData
}

type hostArtifactModel struct {
	Node           types.String `tfsdk:"node"`
	Workspace      types.String `tfsdk:"workspace"`
	Repository     types.String `tfsdk:"repository"`
	ReleaseTag     types.String `tfsdk:"release_tag"`
	Asset          types.String `tfsdk:"asset"`
	SHA256         types.String `tfsdk:"sha256"`
	BundleAsset    types.String `tfsdk:"bundle_asset"`
	BundleSHA256   types.String `tfsdk:"bundle_sha256"`
	SourceRevision types.String `tfsdk:"source_revision"`
	ProgramBundles types.Map    `tfsdk:"program_bundles"`

	Target                  types.String `tfsdk:"target"`
	State                   types.String `tfsdk:"state"`
	InstalledSHA256         types.String `tfsdk:"installed_sha256"`
	InstalledSourceRevision types.String `tfsdk:"installed_source_revision"`
	ObservedSHA256          types.String `tfsdk:"observed_sha256"`
	Files                   types.Map    `tfsdk:"files"`
	Directories             types.Map    `tfsdk:"directories"`
	WriteIDs                types.Map    `tfsdk:"write_ids"`
}

// The framework accepts only JSON documents as private state values.
type hostArtifactMismatchDocument struct {
	Mismatch bool `json:"mismatch"`
}

type hostArtifactProgramFiles struct {
	Name        string
	ArchivePath string
	BundlePath  string
}

// pinOf returns the pin of the model and whether every pin attribute is known.
func pinOf(ctx context.Context, model hostArtifactModel) (hostArtifactPin, bool, diag.Diagnostics) {
	values := []types.String{
		model.Node,
		model.Workspace,
		model.Repository,
		model.ReleaseTag,
		model.Asset,
		model.SHA256,
		model.BundleAsset,
		model.BundleSHA256,
		model.SourceRevision,
	}
	for _, value := range values {
		if value.IsUnknown() || value.IsNull() {
			return hostArtifactPin{}, false, nil
		}
	}

	bundles, known, diagnostics := programBundlesOf(ctx, model)
	if diagnostics.HasError() || !known {
		return hostArtifactPin{}, false, diagnostics
	}

	return hostArtifactPin{
		Workspace:      model.Workspace.ValueString(),
		Repository:     model.Repository.ValueString(),
		ReleaseTag:     model.ReleaseTag.ValueString(),
		Asset:          model.Asset.ValueString(),
		SHA256:         model.SHA256.ValueString(),
		BundleAsset:    model.BundleAsset.ValueString(),
		BundleSHA256:   model.BundleSHA256.ValueString(),
		SourceRevision: model.SourceRevision.ValueString(),
		ProgramBundles: bundles,
	}, true, diagnostics
}

func programBundlesOf(
	ctx context.Context,
	model hostArtifactModel,
) (map[string]hostArtifactProgramBundle, bool, diag.Diagnostics) {
	bundles := map[string]hostArtifactProgramBundle{}
	if model.ProgramBundles.IsNull() {
		return bundles, true, nil
	}

	if model.ProgramBundles.IsUnknown() {
		return nil, false, nil
	}

	var values map[string]hostArtifactProgramBundleValue
	diagnostics := model.ProgramBundles.ElementsAs(ctx, &values, false)
	if diagnostics.HasError() {
		return nil, false, diagnostics
	}

	for name, value := range values {
		if value.Asset.IsUnknown() || value.Asset.IsNull() || value.SHA256.IsUnknown() ||
			value.SHA256.IsNull() {
			return nil, false, diagnostics
		}

		bundles[name] = hostArtifactProgramBundle{
			Asset:  value.Asset.ValueString(),
			SHA256: value.SHA256.ValueString(),
		}
	}

	return bundles, true, diagnostics
}

type downloadResource struct {
	data *providerData
}

type downloadModel struct {
	Node                    types.String `tfsdk:"node"`
	VMID                    types.Int64  `tfsdk:"vmid"`
	Kind                    types.String `tfsdk:"kind"`
	URL                     types.String `tfsdk:"url"`
	SHA256                  types.String `tfsdk:"sha256"`
	Fetch                   types.String `tfsdk:"fetch"`
	ArchiveMember           types.String `tfsdk:"archive_member"`
	RequestHeadersWO        types.Map    `tfsdk:"request_headers_wo"`
	RequestHeadersWOVersion types.Int64  `tfsdk:"request_headers_wo_version"`
	Path                    types.String `tfsdk:"path"`
	Mode                    types.String `tfsdk:"mode"`
	Owner                   types.String `tfsdk:"owner"`
	Group                   types.String `tfsdk:"group"`
	FileSHA256              types.String `tfsdk:"file_sha256"`
	WriteID                 types.String `tfsdk:"write_id"`
}
