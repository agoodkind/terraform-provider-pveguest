package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	pathpkg "path"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	hostArtifactManifestMember = "manifest.json"
	hostArtifactSchemaFiles    = 1
	hostArtifactSchemaPrograms = 2
	githubReleaseBase          = "https://github.com"
)

var hostArtifactModePattern = regexp.MustCompile(`^[0-7]{4}$`)

// hostArtifactPin identifies the pinned archive and attestation bundle of one
// target.
type hostArtifactPin struct {
	Workspace      string
	Repository     string
	ReleaseTag     string
	Asset          string
	SHA256         string
	BundleAsset    string
	BundleSHA256   string
	SourceRevision string
	// ProgramBundles maps program names to attestation bundle assets and hashes
	// in the release selected by Repository and ReleaseTag.
	ProgramBundles map[string]hostArtifactProgramBundle
}

// hostArtifactProgramBundle stores a program's attestation bundle asset and SHA-256.
type hostArtifactProgramBundle struct {
	Asset  string
	SHA256 string
}

// hostArtifactProgramBundleValue stores the Terraform values for one
// program_bundles entry.
type hostArtifactProgramBundleValue struct {
	Asset  types.String `tfsdk:"asset"`
	SHA256 types.String `tfsdk:"sha256"`
}

// hostArtifactManifest contains a host artifact archive manifest with schema
// version 1 or 2. Schema version 2 adds support for programs.
type hostArtifactManifest struct {
	SchemaVersion  int                             `json:"schema_version"`
	Repository     string                          `json:"repository"`
	SourceRevision string                          `json:"source_revision"`
	Workspace      string                          `json:"workspace"`
	Node           string                          `json:"node"`
	Environment    string                          `json:"environment"`
	Directories    []hostArtifactManifestDirectory `json:"directories"`
	Files          []hostArtifactManifestFile      `json:"files"`
	Programs       []hostArtifactManifestProgram   `json:"programs"`
}

// hostArtifactManifestProgram defines a program from a separate release archive.
// Repository and ReleaseTag select that release. Member selects the file that
// the host reads from the archive.
type hostArtifactManifestProgram struct {
	Name           string `json:"name"`
	Repository     string `json:"repository"`
	ReleaseTag     string `json:"release_tag"`
	Asset          string `json:"asset"`
	SHA256         string `json:"sha256"`
	SourceRevision string `json:"source_revision"`
	SignerRevision string `json:"signer_revision"`
	Member         string `json:"member"`
	MemberSHA256   string `json:"member_sha256"`
	Path           string `json:"path"`
	Mode           string `json:"mode"`
	Owner          string `json:"owner"`
	Group          string `json:"group"`
}

type hostArtifactManifestDirectory struct {
	Path  string `json:"path"`
	Mode  string `json:"mode"`
	Owner string `json:"owner"`
	Group string `json:"group"`
}

type hostArtifactManifestFile struct {
	Member string `json:"member"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
	Owner  string `json:"owner"`
	Group  string `json:"group"`
}

// hostArtifactFileValue is one element of the files attribute. A path that is
// not a regular file has an empty SHA256.
type hostArtifactFileValue struct {
	SHA256 string `tfsdk:"sha256"`
	Mode   string `tfsdk:"mode"`
	Owner  string `tfsdk:"owner"`
	Group  string `tfsdk:"group"`
}

// hostArtifactDirectoryValue is one element of the directories attribute.
type hostArtifactDirectoryValue struct {
	Mode  string `tfsdk:"mode"`
	Owner string `tfsdk:"owner"`
	Group string `tfsdk:"group"`
}

// releaseAssetURL returns the download URL of a GitHub release asset.
func releaseAssetURL(repository string, releaseTag string, asset string) string {
	return githubReleaseBase + "/" + repository + "/releases/download/" +
		url.PathEscape(releaseTag) + "/" + url.PathEscape(asset)
}

// loadPinnedManifest downloads the pinned archive into the controller cache
// and returns its manifest and the path of the cached archive. The manifest
// must belong to the pinned repository, source revision, and workspace.
func loadPinnedManifest(ctx context.Context, caFile string, pin hostArtifactPin) (hostArtifactManifest, string, error) {
	archiveURL := releaseAssetURL(pin.Repository, pin.ReleaseTag, pin.Asset)
	archivePath, err := cachedArchive(ctx, caFile, archiveURL, pin.SHA256, requestHeaders{})
	if err != nil {
		return hostArtifactManifest{}, "", err
	}
	content, err := extractArchiveMember(archivePath, hostArtifactManifestMember)
	if err != nil {
		return hostArtifactManifest{}, "", fmt.Errorf("read %s of %s: %w", hostArtifactManifestMember, archiveURL, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest hostArtifactManifest
	if err := decoder.Decode(&manifest); err != nil {
		slog.ErrorContext(ctx, "host artifact manifest is malformed", "url", archiveURL, "err", err)
		return hostArtifactManifest{}, "", fmt.Errorf("parse %s of %s: %w", hostArtifactManifestMember, archiveURL, err)
	}
	if err := manifest.check(pin); err != nil {
		return hostArtifactManifest{}, "", fmt.Errorf("%s of %s: %w", hostArtifactManifestMember, archiveURL, err)
	}
	return manifest, archivePath, nil
}

func (manifest hostArtifactManifest) check(pin hostArtifactPin) error {
	if manifest.SchemaVersion != hostArtifactSchemaFiles && manifest.SchemaVersion != hostArtifactSchemaPrograms {
		return fmt.Errorf("schema_version %d must equal %d or %d",
			manifest.SchemaVersion, hostArtifactSchemaFiles, hostArtifactSchemaPrograms)
	}
	if manifest.SchemaVersion == hostArtifactSchemaFiles && len(manifest.Programs) > 0 {
		return fmt.Errorf("schema_version %d requires an empty programs list", hostArtifactSchemaFiles)
	}
	identity := []struct {
		field string
		got   string
		want  string
	}{
		{field: "repository", got: manifest.Repository, want: pin.Repository},
		{field: "source_revision", got: manifest.SourceRevision, want: pin.SourceRevision},
		{field: "workspace", got: manifest.Workspace, want: pin.Workspace},
	}
	for _, entry := range identity {
		if entry.got != entry.want {
			return fmt.Errorf("%s is %q, and the configuration sets %q", entry.field, entry.got, entry.want)
		}
	}
	paths := map[string]bool{}
	for _, directory := range manifest.Directories {
		if paths[directory.Path] {
			return fmt.Errorf("the path %s appears more than once", directory.Path)
		}
		paths[directory.Path] = true
	}
	for _, file := range manifest.Files {
		if paths[file.Path] {
			return fmt.Errorf("the path %s appears more than once", file.Path)
		}
		paths[file.Path] = true
	}
	names := map[string]bool{}
	for _, program := range manifest.Programs {
		if err := program.check(); err != nil {
			return err
		}
		if names[program.Name] {
			return fmt.Errorf("program name %s appears more than once in the manifest", program.Name)
		}
		names[program.Name] = true
		if paths[program.Path] {
			return fmt.Errorf("path %s appears more than once among manifest directories, files, and programs", program.Path)
		}
		paths[program.Path] = true
	}
	return nil
}

func (program hostArtifactManifestProgram) check() error {
	if !hostArtifactNamePattern.MatchString(program.Name) {
		return fmt.Errorf("program name %q must begin with a letter or digit and use only letters, digits, '.', '_', and '-'", program.Name)
	}
	if !hostArtifactRepositoryPattern.MatchString(program.Repository) {
		return fmt.Errorf("repository %q for program %s must use the owner/repository format", program.Repository, program.Name)
	}
	_, repositoryName, _ := strings.Cut(program.Repository, "/")
	if repositoryName == "." || repositoryName == ".." {
		return fmt.Errorf("repository %q for program %s must not use '.' or '..' as the text after the slash", program.Repository, program.Name)
	}
	for _, field := range []struct{ name, value string }{
		{name: "release_tag", value: program.ReleaseTag},
		{name: "asset", value: program.Asset},
	} {
		if !hostArtifactNamePattern.MatchString(field.value) {
			return fmt.Errorf("field %s value %q for program %s must begin with a letter or digit and use only letters, digits, '.', '_', and '-'", field.name, field.value, program.Name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{name: "sha256", value: program.SHA256},
		{name: "member_sha256", value: program.MemberSHA256},
	} {
		if !sha256Pattern.MatchString(field.value) {
			return fmt.Errorf("field %s value %q for program %s must contain exactly 64 lowercase hexadecimal characters", field.name, field.value, program.Name)
		}
	}
	for _, field := range []struct{ name, value string }{
		{name: "source_revision", value: program.SourceRevision},
		{name: "signer_revision", value: program.SignerRevision},
	} {
		if !hostArtifactRevisionPattern.MatchString(field.value) {
			return fmt.Errorf("field %s with value %q for program %s must contain exactly 40 lowercase hexadecimal characters", field.name, field.value, program.Name)
		}
	}
	if err := validateArchiveMember(program.Member); err != nil {
		slog.Error("program archive member is invalid", "program", program.Name, "member", program.Member, "err", err)
		return fmt.Errorf("archive member %q for program %s fails validation: %w", program.Member, program.Name, err)
	}
	if !strings.HasPrefix(program.Path, "/") || pathpkg.Clean(program.Path) != program.Path {
		return fmt.Errorf("path %q for program %s must be a clean absolute path", program.Path, program.Name)
	}
	if !hostArtifactModePattern.MatchString(program.Mode) {
		return fmt.Errorf("mode %q for program %s must contain exactly four octal digits", program.Mode, program.Name)
	}
	for _, field := range []struct{ name, value string }{
		{name: "owner", value: program.Owner},
		{name: "group", value: program.Group},
	} {
		if field.value == "" {
			return fmt.Errorf("field %s for program %s must contain at least one character", field.name, program.Name)
		}
	}
	return nil
}

// checkProgramBundles checks that program_bundles has exactly one key for each
// manifest program name.
func (manifest hostArtifactManifest) checkProgramBundles(bundles map[string]hostArtifactProgramBundle) error {
	programNames := map[string]bool{}
	var missing []string
	for _, program := range manifest.Programs {
		programNames[program.Name] = true
		if _, found := bundles[program.Name]; !found {
			missing = append(missing, program.Name)
		}
	}
	var extra []string
	for _, name := range slices.Sorted(maps.Keys(bundles)) {
		if !programNames[name] {
			extra = append(extra, name)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf(
		"program_bundles omits manifest program names [%s] and includes names outside the manifest [%s]",
		strings.Join(missing, ", "), strings.Join(extra, ", "),
	)
}

func (manifest hostArtifactManifest) desiredFiles() map[string]hostArtifactFileValue {
	files := make(map[string]hostArtifactFileValue, len(manifest.Files))
	for _, file := range manifest.Files {
		files[file.Path] = hostArtifactFileValue{SHA256: file.SHA256, Mode: file.Mode, Owner: file.Owner, Group: file.Group}
	}
	for _, program := range manifest.Programs {
		files[program.Path] = hostArtifactFileValue{
			SHA256: program.MemberSHA256, Mode: program.Mode, Owner: program.Owner, Group: program.Group,
		}
	}
	return files
}

func (manifest hostArtifactManifest) desiredDirectories() map[string]hostArtifactDirectoryValue {
	directories := make(map[string]hostArtifactDirectoryValue, len(manifest.Directories))
	for _, directory := range manifest.Directories {
		directories[directory.Path] = hostArtifactDirectoryValue{Mode: directory.Mode, Owner: directory.Owner, Group: directory.Group}
	}
	return directories
}

// observedFiles returns the live value of each present file in the GET
// response.
func observedFiles(current transport.HostArtifactState) map[string]hostArtifactFileValue {
	files := make(map[string]hostArtifactFileValue, len(current.Files))
	for _, file := range current.Files {
		if file.Present {
			files[file.Path] = hostArtifactFileValue{SHA256: file.SHA256, Mode: file.Mode, Owner: file.Owner, Group: file.Group}
		}
	}
	return files
}

// observedDirectories returns the live value of each present owned directory
// in the GET response.
func observedDirectories(current transport.HostArtifactState) map[string]hostArtifactDirectoryValue {
	directories := make(map[string]hostArtifactDirectoryValue, len(current.Directories))
	for _, directory := range current.Directories {
		if directory.Present {
			directories[directory.Path] = hostArtifactDirectoryValue{Mode: directory.Mode, Owner: directory.Owner, Group: directory.Group}
		}
	}
	return directories
}

// observedWriteIDs returns the write_id of each file in the installed record.
func observedWriteIDs(current transport.HostArtifactState) map[string]string {
	writeIDs := make(map[string]string, len(current.Files))
	for _, file := range current.Files {
		if file.WriteID != "" {
			writeIDs[file.Path] = file.WriteID
		}
	}
	return writeIDs
}

// reportsMismatch returns true when GET reports matches false for a file or
// an owned directory.
func reportsMismatch(current transport.HostArtifactState) bool {
	for _, file := range current.Files {
		if file.HasMatches && !file.Matches {
			return true
		}
	}
	for _, directory := range current.Directories {
		if directory.HasMatches && !directory.Matches {
			return true
		}
	}
	return false
}

// liveDifferences lists each difference between the GET response and the
// pinned archive. An empty list means that the node has the pinned archive
// installed and every file and owned directory matches its manifest.
func liveDifferences(manifest hostArtifactManifest, pin hostArtifactPin, current transport.HostArtifactState) []string {
	var differences []string
	if current.State != transport.HostArtifactStateCommitted {
		differences = append(differences, "the target state is "+current.State)
	}
	switch {
	case current.Installed == nil:
		differences = append(differences, "the target does not have an installed record")
	case current.Installed.SHA256 != pin.SHA256 || current.Installed.SourceRevision != pin.SourceRevision:
		differences = append(differences, fmt.Sprintf(
			"the installed archive has sha256 %s from revision %s",
			current.Installed.SHA256, current.Installed.SourceRevision,
		))
	}
	if reportsMismatch(current) {
		differences = append(differences, "the node reports a file or directory that differs from the installed record")
	}
	differences = append(differences, fileDifferences(manifest.desiredFiles(), observedFiles(current))...)
	differences = append(differences, directoryDifferences(manifest.desiredDirectories(), observedDirectories(current))...)
	return differences
}

func fileDifferences(desired map[string]hostArtifactFileValue, observed map[string]hostArtifactFileValue) []string {
	return pathDifferences("file", desired, observed)
}

// directoryDifferences compares directories as file values without a SHA-256.
func directoryDifferences(desired map[string]hostArtifactDirectoryValue, observed map[string]hostArtifactDirectoryValue) []string {
	return pathDifferences("directory", directoryFileValues(desired), directoryFileValues(observed))
}

func directoryFileValues(directories map[string]hostArtifactDirectoryValue) map[string]hostArtifactFileValue {
	values := make(map[string]hostArtifactFileValue, len(directories))
	for path, directory := range directories {
		values[path] = hostArtifactFileValue{Mode: directory.Mode, Owner: directory.Owner, Group: directory.Group}
	}
	return values
}

func pathDifferences(kind string, desired map[string]hostArtifactFileValue, observed map[string]hostArtifactFileValue) []string {
	var differences []string
	for _, path := range slices.Sorted(maps.Keys(desired)) {
		got, found := observed[path]
		switch {
		case !found:
			differences = append(differences, fmt.Sprintf("the %s %s is missing", kind, path))
		case got != desired[path]:
			differences = append(differences, fmt.Sprintf("the %s %s differs from the manifest", kind, path))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(observed)) {
		if _, found := desired[path]; !found {
			differences = append(differences, fmt.Sprintf("the %s %s is not in the manifest", kind, path))
		}
	}
	return differences
}

func joinDifferences(differences []string) string {
	return strings.Join(differences, "; ")
}
