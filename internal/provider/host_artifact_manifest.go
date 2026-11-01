package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	hostArtifactManifestMember = "manifest.json"
	hostArtifactSchemaVersion  = 1
	githubReleaseBase          = "https://github.com"
)

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
}

// hostArtifactManifest contains a host artifact archive manifest with schema
// version 1.
type hostArtifactManifest struct {
	SchemaVersion  int                             `json:"schema_version"`
	Repository     string                          `json:"repository"`
	SourceRevision string                          `json:"source_revision"`
	Workspace      string                          `json:"workspace"`
	Node           string                          `json:"node"`
	Environment    string                          `json:"environment"`
	Directories    []hostArtifactManifestDirectory `json:"directories"`
	Files          []hostArtifactManifestFile      `json:"files"`
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
	if manifest.SchemaVersion != hostArtifactSchemaVersion {
		return fmt.Errorf("schema_version is %d, want %d", manifest.SchemaVersion, hostArtifactSchemaVersion)
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
	return nil
}

func (manifest hostArtifactManifest) desiredFiles() map[string]hostArtifactFileValue {
	files := make(map[string]hostArtifactFileValue, len(manifest.Files))
	for _, file := range manifest.Files {
		files[file.Path] = hostArtifactFileValue{SHA256: file.SHA256, Mode: file.Mode, Owner: file.Owner, Group: file.Group}
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
