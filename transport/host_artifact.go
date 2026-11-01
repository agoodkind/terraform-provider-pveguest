package transport

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
)

// GET /nodes/{node}/host-artifacts/{target} reports these host artifact states.
const (
	HostArtifactStateNone        = "none"
	HostArtifactStateCommitted   = "committed"
	HostArtifactStateInterrupted = "interrupted"
)

// POST /nodes/{node}/host-artifacts/{target}/stage accepts these stage kinds.
const (
	HostArtifactKindArchive = "archive"
	HostArtifactKindBundle  = "bundle"
)

const (
	hostArtifactsPath    = "/host-artifacts/"
	hostArtifactStageSub = "/stage"

	// The stage method accepts at most 128 KiB of base64 in data. 96 KiB of
	// bytes encode to exactly 128 KiB of base64.
	hostArtifactChunkBytes = 96 * 1024
)

// HostArtifactInstall contains the parameters of
// POST /nodes/{node}/host-artifacts/{target}.
type HostArtifactInstall struct {
	ReleaseTag     string `json:"release-tag"`
	Asset          string `json:"asset"`
	SHA256         string `json:"sha256"`
	SourceRevision string `json:"source-revision"`
	ArchiveStage   string `json:"archive-stage"`
	BundleStage    string `json:"bundle-stage"`
}

// HostArtifactInstalled is the installed record of a target.
type HostArtifactInstalled struct {
	ReleaseTag     string `json:"release_tag"`
	Asset          string `json:"asset"`
	SHA256         string `json:"sha256"`
	SourceRevision string `json:"source_revision"`
	ManifestSHA256 string `json:"manifest_sha256"`
	UPID           string `json:"upid"`
	User           string `json:"user"`
	InstalledAt    string `json:"installed_at"`
}

// HostArtifactFile contains the live state of one target file. HasMatches is
// true, and WriteID has a value, only when the target has an installed record.
type HostArtifactFile struct {
	Path       string
	Present    bool
	SHA256     string
	Size       int64
	Mode       string
	Owner      string
	Group      string
	UID        int64
	GID        int64
	WriteID    string
	HasMatches bool
	Matches    bool
}

// HostArtifactDirectory is the live state of one directory that a target
// owns. HasMatches is true only when the target has an installed record.
type HostArtifactDirectory struct {
	Path       string
	Present    bool
	Mode       string
	Owner      string
	Group      string
	UID        int64
	GID        int64
	HasMatches bool
	Matches    bool
}

// HostArtifactState is the response of GET /nodes/{node}/host-artifacts/{target}.
type HostArtifactState struct {
	Workspace      string
	Node           string
	Environment    string
	State          string
	Installed      *HostArtifactInstalled
	Files          []HostArtifactFile
	Directories    []HostArtifactDirectory
	ObservedSHA256 string
	Secrets        []HostSecretState
}

type hostArtifactStageBody struct {
	Kind   string `json:"kind"`
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
	Stage  string `json:"stage,omitempty"`
}

type hostArtifactStageData struct {
	Stage  string      `json:"stage"`
	Length json.Number `json:"length"`
}

type hostArtifactFileData struct {
	Path    string      `json:"path"`
	Present flag        `json:"present"`
	SHA256  string      `json:"sha256"`
	Size    json.Number `json:"size"`
	Mode    string      `json:"mode"`
	Owner   string      `json:"owner"`
	Group   string      `json:"group"`
	UID     json.Number `json:"uid"`
	GID     json.Number `json:"gid"`
	WriteID string      `json:"write_id"`
	Matches *flag       `json:"matches"`
}

type hostArtifactDirectoryData struct {
	Path    string      `json:"path"`
	Present flag        `json:"present"`
	Mode    string      `json:"mode"`
	Owner   string      `json:"owner"`
	Group   string      `json:"group"`
	UID     json.Number `json:"uid"`
	GID     json.Number `json:"gid"`
	Matches *flag       `json:"matches"`
}

type hostArtifactStateData struct {
	Workspace      string                      `json:"workspace"`
	Node           string                      `json:"node"`
	Environment    string                      `json:"environment"`
	State          string                      `json:"state"`
	Installed      *HostArtifactInstalled      `json:"installed"`
	Files          []hostArtifactFileData      `json:"files"`
	Directories    []hostArtifactDirectoryData `json:"directories"`
	ObservedSHA256 string                      `json:"observed_sha256"`
	Secrets        []hostSecretData            `json:"secrets"`
}

// HostArtifactTarget returns the {target} path value of a workspace selector:
// the lowercase hexadecimal SHA-256 of the selector bytes.
func HostArtifactTarget(workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	return hex.EncodeToString(sum[:])
}

// StageHostArtifact sends content to a new stage of the target in chunks and
// returns the stage that the node chose. size is the number of bytes in
// content.
func (client *Client) StageHostArtifact(
	ctx context.Context,
	node string,
	target string,
	kind string,
	content io.Reader,
	size int64,
) (string, error) {
	buffer := make([]byte, hostArtifactChunkBytes)
	stage := ""
	var offset int64
	for {
		count, readErr := io.ReadFull(content, buffer)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			slog.ErrorContext(ctx, "read host artifact content failed", "node", node, "target", target, "kind", kind, "err", readErr)
			return "", fmt.Errorf("node %q target %s: read the %s: %w", node, target, kind, readErr)
		}
		// The chunk at offset 0 creates the stage. Empty content sends one
		// empty chunk at offset 0.
		if count == 0 && offset > 0 {
			break
		}
		length, nextStage, err := client.stageChunk(ctx, node, target, kind, offset, buffer[:count], stage)
		if err != nil {
			return "", err
		}
		offset += int64(count)
		if length != offset {
			return "", fmt.Errorf("node %q target %s: stage %s has %d bytes after a chunk, want %d", node, target, nextStage, length, offset)
		}
		stage = nextStage
		if count < hostArtifactChunkBytes || offset >= size {
			break
		}
	}
	if offset != size {
		return "", fmt.Errorf("node %q target %s: the %s has %d bytes, want %d", node, target, kind, offset, size)
	}
	return stage, nil
}

// stageChunk sends one chunk and returns the stage length and the stage.
func (client *Client) stageChunk(
	ctx context.Context,
	node string,
	target string,
	kind string,
	offset int64,
	chunk []byte,
	stage string,
) (int64, string, error) {
	body := hostArtifactStageBody{
		Kind:   kind,
		Offset: offset,
		Data:   base64.StdEncoding.EncodeToString(chunk),
		Stage:  stage,
	}
	document, err := encodeJSON(&body)
	if err != nil {
		return 0, "", fmt.Errorf("node %q target %s: %w", node, target, err)
	}
	data, err := client.hostArtifactCall(ctx, node, http.MethodPost, target, hostArtifactStageSub, document)
	if err != nil {
		return 0, "", err
	}
	var parsed hostArtifactStageData
	if err := json.Unmarshal(data, &parsed); err != nil {
		slog.ErrorContext(ctx, "host artifact stage response is malformed", "node", node, "target", target, "err", err)
		return 0, "", fmt.Errorf("node %q target %s: parse stage response: %w", node, target, err)
	}
	length, err := parsed.Length.Int64()
	if err != nil {
		slog.ErrorContext(ctx, "host artifact stage length is malformed", "node", node, "target", target, "err", err)
		return 0, "", fmt.Errorf("node %q target %s: parse stage length: %w", node, target, err)
	}
	return length, parsed.Stage, nil
}

// InstallHostArtifact starts a hostartifact task that installs the staged
// archive and returns its UPID.
func (client *Client) InstallHostArtifact(
	ctx context.Context,
	node string,
	target string,
	request HostArtifactInstall,
) (string, error) {
	document, err := encodeJSON(&request)
	if err != nil {
		return "", fmt.Errorf("node %q target %s: %w", node, target, err)
	}
	data, err := client.hostArtifactCall(ctx, node, http.MethodPost, target, "", document)
	if err != nil {
		return "", err
	}
	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		slog.ErrorContext(ctx, "host artifact install response is malformed", "node", node, "target", target, "err", err)
		return "", fmt.Errorf("node %q target %s: parse task identifier: %w", node, target, err)
	}
	return upid, nil
}

// GetHostArtifact returns the installed record of the target and the live
// state of its files and owned directories. The request does not change the
// node.
func (client *Client) GetHostArtifact(ctx context.Context, node string, target string) (HostArtifactState, error) {
	data, err := client.hostArtifactCall(ctx, node, http.MethodGet, target, "", nil)
	if err != nil {
		return HostArtifactState{}, err
	}
	var parsed hostArtifactStateData
	if err := json.Unmarshal(data, &parsed); err != nil {
		slog.ErrorContext(ctx, "host artifact response is malformed", "node", node, "target", target, "err", err)
		return HostArtifactState{}, fmt.Errorf("node %q target %s: parse host artifact response: %w", node, target, err)
	}
	state := HostArtifactState{
		Workspace:      parsed.Workspace,
		Node:           parsed.Node,
		Environment:    parsed.Environment,
		State:          parsed.State,
		Installed:      parsed.Installed,
		Files:          make([]HostArtifactFile, 0, len(parsed.Files)),
		Directories:    make([]HostArtifactDirectory, 0, len(parsed.Directories)),
		ObservedSHA256: parsed.ObservedSHA256,
		Secrets:        make([]HostSecretState, 0, len(parsed.Secrets)),
	}
	for _, entry := range parsed.Files {
		file, err := entry.convert()
		if err != nil {
			slog.ErrorContext(ctx, "host artifact file entry is malformed", "node", node, "target", target, "path", entry.Path, "err", err)
			return HostArtifactState{}, fmt.Errorf("node %q target %s: file %s: %w", node, target, entry.Path, err)
		}
		state.Files = append(state.Files, file)
	}
	for _, entry := range parsed.Directories {
		directory, err := entry.convert()
		if err != nil {
			slog.ErrorContext(ctx, "host artifact directory entry is malformed", "node", node, "target", target, "path", entry.Path, "err", err)
			return HostArtifactState{}, fmt.Errorf("node %q target %s: directory %s: %w", node, target, entry.Path, err)
		}
		state.Directories = append(state.Directories, directory)
	}
	for _, entry := range parsed.Secrets {
		secret, err := entry.convert()
		if err != nil {
			slog.ErrorContext(ctx, "host secret entry is malformed", "node", node, "target", target, "field", entry.Field, "err", err)
			return HostArtifactState{}, fmt.Errorf("node %q target %s: secret field %s: %w", node, target, entry.Field, err)
		}
		state.Secrets = append(state.Secrets, secret)
	}
	return state, nil
}

func (entry hostArtifactFileData) convert() (HostArtifactFile, error) {
	numbers, err := optionalIntegers(entry.Size, entry.UID, entry.GID)
	if err != nil {
		return HostArtifactFile{}, err
	}
	file := HostArtifactFile{
		Path:    entry.Path,
		Present: bool(entry.Present),
		SHA256:  entry.SHA256,
		Size:    numbers[0],
		Mode:    entry.Mode,
		Owner:   entry.Owner,
		Group:   entry.Group,
		UID:     numbers[1],
		GID:     numbers[2],
		WriteID: entry.WriteID,
	}
	if entry.Matches != nil {
		file.HasMatches = true
		file.Matches = bool(*entry.Matches)
	}
	return file, nil
}

func (entry hostArtifactDirectoryData) convert() (HostArtifactDirectory, error) {
	numbers, err := optionalIntegers(entry.UID, entry.GID)
	if err != nil {
		return HostArtifactDirectory{}, err
	}
	directory := HostArtifactDirectory{
		Path:    entry.Path,
		Present: bool(entry.Present),
		Mode:    entry.Mode,
		Owner:   entry.Owner,
		Group:   entry.Group,
		UID:     numbers[0],
		GID:     numbers[1],
	}
	if entry.Matches != nil {
		directory.HasMatches = true
		directory.Matches = bool(*entry.Matches)
	}
	return directory, nil
}

// optionalIntegers converts each value to an integer. An absent value is 0.
func optionalIntegers(values ...json.Number) ([]int64, error) {
	numbers := make([]int64, 0, len(values))
	for _, value := range values {
		if value == "" {
			numbers = append(numbers, 0)
			continue
		}
		number, err := value.Int64()
		if err != nil {
			slog.Error("host artifact integer is malformed", "value", value.String(), "err", err)
			return nil, fmt.Errorf("parse integer %q: %w", value, err)
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

func (client *Client) hostArtifactCall(
	ctx context.Context,
	node string,
	method string,
	target string,
	suffix string,
	body []byte,
) (json.RawMessage, error) {
	connection, found := client.nodes[node]
	if !found {
		return nil, fmt.Errorf("node %q is not in the provider nodes map", node)
	}
	request := apiRequest{
		method: method,
		path:   "/api2/json/nodes/" + url.PathEscape(connection.nodeName) + hostArtifactsPath + url.PathEscape(target) + suffix,
		body:   body,
	}
	return connection.call(ctx, Guest{Node: node}, request)
}
