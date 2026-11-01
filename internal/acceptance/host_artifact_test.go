package acceptance_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	pathutil "path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	// hostSSHVariable selects an ssh destination with a root shell on the
	// acceptance node. The test edits, inspects, and deletes the files of
	// the two fixture targets through it.
	hostSSHVariable            = "PVEGUEST_ACC_HOST_SSH"
	hostSSHControlPathVariable = "PVEGUEST_ACC_HOST_SSH_CONTROL_PATH"
	// hostArtifactPinsVariable selects a JSON file that maps each fixture
	// case to the pin that the host-artifact job wrote for its release.
	hostArtifactPinsVariable = "PVEGUEST_ACC_HOST_ARTIFACT_PINS"

	hostArtifactRepository = "agoodkind/configs"
	hostArtifactAddress    = "pveguest_host_artifact.test"

	fixtureWorkspace          = "suburban/hostartifact-acceptance"
	fixtureDirectory          = "/srv/hostartifact-acceptance"
	unrelatedFixtureWorkspace = "suburban/hostartifact-acceptance-b"
	unrelatedFixtureDirectory = "/srv/hostartifact-acceptance-b"
	firstFixtureFile          = "a.conf"
	secondFixtureFile         = "b.conf"
	fixtureDirectoryMode      = "0755"
	driftedDirectoryMode      = "0700"
	editedFixtureContent      = "edited by hand\n"

	caseValidV1     = "valid-v1"
	caseValidV2     = "valid-v2"
	caseOtherTarget = "other-target"
	caseRecoveryV1  = "recovery-v1"
	caseRecoveryV2  = "recovery-v2"

	caseProgramValid        = "program-valid"
	caseProgramWrongSource  = "program-wrong-source"
	caseProgramWrongSigner  = "program-wrong-signer"
	caseProgramWrongArchive = "program-wrong-archive"
	caseProgramWrongMember  = "program-wrong-member"
	caseProgramCollision    = "program-collision"

	manifestMember = "manifest.json"

	fixtureAssetPrefix = "hostconfig_fixture-"
	fixtureAssetSuffix = ".tar.gz"
	releaseBaseURL     = "https://github.com/"

	hostStateDirectory = "/var/lib/pve-overlay/host-artifacts/"
	hostLockPrefix     = "/run/lock/pve-overlay-host-artifact-"
	hostLockSuffix     = ".lck"
	installedRecord    = "installed.json"
	journalRecord      = "journal.json"
	stageDirectory     = "stages"

	hostTaskTimeout     = 10 * time.Minute
	interruptDeadline   = 5 * time.Minute
	interruptPollPeriod = 20 * time.Millisecond
	taskStopDeadline    = time.Minute
	taskStopPollPeriod  = 200 * time.Millisecond
	taskInterruptedExit = "received interrupt"
	taskKilledExit      = "unexpected status"
	missingPathMessage  = "No such file or directory"
	absentSnapshotValue = "absent"
	destroyRefusalText  = "empty file list"
)

// hostArtifactPinEntry is the pin JSON that the host-artifact job writes to
// its summary.
type hostArtifactPinEntry struct {
	ReleaseTag     string `json:"release_tag"`
	Asset          string `json:"asset"`
	SHA256         string `json:"sha256"`
	BundleAsset    string `json:"bundle_asset"`
	BundleSHA256   string `json:"bundle_sha256"`
	SourceRevision string `json:"source_revision"`
	// ProgramBundles maps program names to bundles for this test case.
	ProgramBundles map[string]hostArtifactBundleEntry `json:"program_bundles"`
}

// hostArtifactBundleEntry stores a program bundle asset and SHA-256 from pin JSON.
type hostArtifactBundleEntry struct {
	Asset  string `json:"asset"`
	SHA256 string `json:"sha256"`
}

// fixtureProgram stores the manifest program fields that acceptance tests read.
type fixtureProgram struct {
	Name           string `json:"name"`
	Repository     string `json:"repository"`
	ReleaseTag     string `json:"release_tag"`
	Asset          string `json:"asset"`
	SHA256         string `json:"sha256"`
	SourceRevision string `json:"source_revision"`
	SignerRevision string `json:"signer_revision"`
	Path           string `json:"path"`
	Member         string `json:"member"`
	MemberSHA256   string `json:"member_sha256"`
}

type hostArtifactTest struct {
	t                     *testing.T
	client                *transport.Client
	audit                 *transport.Client
	auditPrincipal        string
	unrelatedInvocationID string
	node                  string
	nodeConfig            transport.NodeConfig
	ssh                   string
	sshControlPath        string
	pins                  map[string]hostArtifactPinEntry
	workspace             *tofuWorkspace
}

func newHostArtifactTest(t *testing.T) *hostArtifactTest {
	t.Helper()
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	ssh := requireVariable(t, hostSSHVariable)
	controlPath := hostSSHControlPath(t, ssh)
	pinFile := requireVariable(t, hostArtifactPinsVariable)
	node := requireVariable(t, nodeVariable)
	nodeConfig := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(map[string]transport.NodeConfig{node: nodeConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}
	audit, auditPrincipal := newHostAuditClient(t, node, nodeConfig)
	test := &hostArtifactTest{
		t:              t,
		client:         client,
		audit:          audit,
		auditPrincipal: auditPrincipal,
		node:           node,
		nodeConfig:     nodeConfig,
		ssh:            ssh,
		sshControlPath: controlPath,
		pins:           readFixturePins(t, pinFile),
	}
	test.workspace = test.newWorkspace()
	// The provider and the test download release assets into this cache.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return test
}

func hostSSHOptions(controlPath string) []string {
	options := []string{"-o", "BatchMode=yes"}
	if controlPath != "" {
		options = append(options, "-S", controlPath, "-o", "ControlMaster=no", "-o", "ProxyCommand=false")
	}
	return options
}

func hostSSHControlPath(t *testing.T, destination string) string {
	t.Helper()
	controlPath := os.Getenv(hostSSHControlPathVariable)
	if controlPath == "" {
		return controlPath
	}
	info, err := os.Stat(controlPath)
	if err != nil {
		t.Fatalf("%s: %v; start a private OpenSSH master at this socket before the test", hostSSHControlPathVariable, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s: %s is not a Unix socket; start a private OpenSSH master at this socket before the test", hostSSHControlPathVariable, controlPath)
	}
	arguments := append(hostSSHOptions(controlPath), "-O", "check", destination)
	output, err := exec.CommandContext(context.Background(), "ssh", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: ssh -O check failed: %v: %s; start a private OpenSSH master at this socket before the test", hostSSHControlPathVariable, err, strings.TrimSpace(string(output)))
	}
	return controlPath
}

// readFixturePins reads the pin file and checks that each case pins the
// fixture asset of its workspace.
func readFixturePins(t *testing.T, pinFile string) map[string]hostArtifactPinEntry {
	t.Helper()
	content, err := os.ReadFile(pinFile)
	if err != nil {
		t.Fatalf("%s: %v", hostArtifactPinsVariable, err)
	}
	var pins map[string]hostArtifactPinEntry
	if err := json.Unmarshal(content, &pins); err != nil {
		t.Fatalf("%s: parse %s: %v", hostArtifactPinsVariable, pinFile, err)
	}
	workspaces := map[string]string{
		caseValidV1:     fixtureWorkspace,
		caseValidV2:     fixtureWorkspace,
		caseRecoveryV1:  fixtureWorkspace,
		caseRecoveryV2:  fixtureWorkspace,
		caseOtherTarget: unrelatedFixtureWorkspace,
	}
	for _, rejection := range hostArtifactDefects {
		workspaces[rejection.fixtureCase] = fixtureWorkspace
	}
	for fixtureCase, workspace := range workspaces {
		checkFixturePin(t, pinFile, pins, fixtureCase, workspace)
	}
	return pins
}

func checkFixturePin(t *testing.T, pinFile string, pins map[string]hostArtifactPinEntry, fixtureCase string, workspace string) {
	t.Helper()
	pin, found := pins[fixtureCase]
	if !found {
		t.Fatalf("%s does not contain the case %s", pinFile, fixtureCase)
	}
	want := fixtureAssetPrefix + fixtureCase + "-" + transport.HostArtifactTarget(workspace) + fixtureAssetSuffix
	if pin.Asset != want {
		t.Fatalf("%s pins asset %s for the case %s, want %s", pinFile, pin.Asset, fixtureCase, want)
	}
	if pin.ReleaseTag == "" || pin.BundleAsset != pin.Asset+".sigstore.json" {
		t.Fatalf("%s: set the release tag and matching bundle asset for %s", pinFile, fixtureCase)
	}
	for label, value := range map[string]string{"sha256": pin.SHA256, "bundle_sha256": pin.BundleSHA256, "source_revision": pin.SourceRevision} {
		length := sha256.Size * 2
		if label == "source_revision" {
			length = 40
		}
		if len(value) != length || strings.Trim(value, "0123456789abcdef") != "" {
			t.Fatalf("%s: set a complete lowercase hexadecimal %s for %s", pinFile, label, fixtureCase)
		}
	}
}

func prepareHostArtifactFixtures(t *testing.T, requiredCases ...string) (*hostArtifactTest, map[string]string) {
	t.Helper()
	test := newHostArtifactTest(t)
	for _, fixtureCase := range requiredCases {
		checkFixturePin(t, os.Getenv(hostArtifactPinsVariable), test.pins, fixtureCase, fixtureWorkspace)
	}
	target := transport.HostArtifactTarget(fixtureWorkspace)
	unrelatedTarget := transport.HostArtifactTarget(unrelatedFixtureWorkspace)
	t.Cleanup(func() {
		test.removeTarget(target, fixtureDirectory)
		test.removeTarget(unrelatedTarget, unrelatedFixtureDirectory)
	})
	test.removeTarget(target, fixtureDirectory)

	test.installDirectly(unrelatedFixtureWorkspace, caseOtherTarget)
	unrelated := test.snapshot(unrelatedTarget, unrelatedFixtureDirectory)
	test.unrelatedInvocationID = (&hostSecretTest{hostArtifactTest: test}).invocationID(unrelatedHostSecretConsumer)
	return test, unrelated
}

func TestAccHostArtifactRejections(t *testing.T) {
	test, unrelated := prepareHostArtifactFixtures(t)
	test.installDirectly(fixtureWorkspace, caseValidV1)
	test.expectSnapshot(unrelated, transport.HostArtifactTarget(unrelatedFixtureWorkspace), unrelatedFixtureDirectory)
	test.expectUnrelatedInvocation()
	test.expectHostRejections(unrelated)
	test.expectAuditRejections(unrelated)
}

// TestAccHostArtifact runs every step against the fixture target and checks
// after each step that the files, metadata, installed record, journal, and
// lock of the unrelated fixture target are unchanged.
func TestAccHostArtifact(t *testing.T) {
	test, unrelated := prepareHostArtifactFixtures(t)
	target := transport.HostArtifactTarget(fixtureWorkspace)
	unrelatedTarget := transport.HostArtifactTarget(unrelatedFixtureWorkspace)

	test.workspace.writeConfig(test.config(caseValidV1))
	test.workspace.mustRun("init", "-no-color")

	// Import of the untracked target calls only GET.
	before := test.snapshot(target, fixtureDirectory)
	test.workspace.mustRun("import", "-no-color", hostArtifactAddress, test.node+":"+fixtureWorkspace)
	test.expectSnapshot(before, target, fixtureDirectory)
	if state := test.get(target).State; state != transport.HostArtifactStateNone {
		t.Fatalf("target state after import is %s, want %s", state, transport.HostArtifactStateNone)
	}
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)

	test.applyClean(caseValidV1)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)

	test.hostWrite(fixtureDirectory+"/"+firstFixtureFile, editedFixtureContent)
	test.expectRepair(caseValidV1)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)

	test.hostMustRun("rm", "-f", "--", fixtureDirectory+"/"+secondFixtureFile)
	test.expectRepair(caseValidV1)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)

	writeIDs := fileWriteIDs(test.get(target))
	test.hostMustRun("chmod", driftedDirectoryMode, "--", fixtureDirectory)
	repaired := test.expectRepair(caseValidV1)
	if got := fileWriteIDs(repaired); !maps.Equal(got, writeIDs) {
		t.Fatalf("write_ids after the directory repair are %v, want %v", got, writeIDs)
	}
	test.expectDirectoryMode(repaired, fixtureDirectory, fixtureDirectoryMode)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)

	test.workspace.writeConfig(test.config(caseValidV2))
	test.applyClean(caseValidV2)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)

	test.workspace.writeConfig(test.config(caseRecoveryV1))
	test.applyClean(caseRecoveryV1)
	test.stopInstall(target, caseRecoveryV1, caseRecoveryV2)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)
	test.workspace.writeConfig(test.config(caseRecoveryV2))
	test.expectRepair(caseRecoveryV2)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)

	// Destroy fails while the installed record lists files.
	installed := test.snapshot(target, fixtureDirectory)
	output, exitCode := test.workspace.run("destroy", "-no-color", "-auto-approve")
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), destroyRefusalText) {
		t.Fatalf("destroy exited with status %d, want a failure that contains %q:\n%s", exitCode, destroyRefusalText, output)
	}
	test.expectSnapshot(installed, target, fixtureDirectory)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)
	test.workspace.mustRun("state", "rm", hostArtifactAddress)

	// The test installs valid-v1 through the API, imports the target into fresh
	// state, and applies the matching pin. The target snapshot stays unchanged.
	test.installDirectly(fixtureWorkspace, caseValidV1)
	matching := test.snapshot(target, fixtureDirectory)
	test.workspace = test.newWorkspace()
	test.workspace.writeConfig(test.config(caseValidV1))
	test.workspace.mustRun("init", "-no-color")
	test.workspace.mustRun("import", "-no-color", hostArtifactAddress, test.node+":"+fixtureWorkspace)
	test.expectSnapshot(matching, target, fixtureDirectory)
	test.applyClean(caseValidV1)
	test.expectSnapshot(matching, target, fixtureDirectory)
	test.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)
	test.workspace.mustRun("state", "rm", hostArtifactAddress)
}

func (h *hostArtifactTest) newWorkspace() *tofuWorkspace {
	h.t.Helper()
	return &tofuWorkspace{t: h.t, directory: h.t.TempDir(), binary: requireVariable(h.t, tofuPathVariable)}
}

func (h *hostArtifactTest) config(fixtureCase string) string {
	pin := h.pins[fixtureCase]
	return fmt.Sprintf(`
terraform {
  required_providers {
    pveguest = {
      source  = %q
      version = %q
    }
  }
}

provider "pveguest" {
  nodes = {
    %q = {
      endpoint  = %q
      api_token = %q
      insecure  = %t
    }
  }
}

resource "pveguest_host_artifact" "test" {
  node            = %q
  workspace       = %q
  repository      = %q
  release_tag     = %q
  asset           = %q
  sha256          = %q
  bundle_asset    = %q
  bundle_sha256   = %q
  source_revision = %q
%s}
`, providerSource, candidateInstalledVersion, h.node, h.nodeConfig.Endpoint, h.nodeConfig.APIToken, h.nodeConfig.Insecure,
		h.node, fixtureWorkspace, hostArtifactRepository, pin.ReleaseTag, pin.Asset, pin.SHA256,
		pin.BundleAsset, pin.BundleSHA256, pin.SourceRevision, programBundlesBlock(pin))
}

// Omit the argument for archives without programs.
func programBundlesBlock(pin hostArtifactPinEntry) string {
	if len(pin.ProgramBundles) == 0 {
		return ""
	}
	var block strings.Builder
	block.WriteString("  program_bundles = {\n")
	for _, name := range slices.Sorted(maps.Keys(pin.ProgramBundles)) {
		bundle := pin.ProgramBundles[name]
		fmt.Fprintf(&block, "    %q = {\n      asset  = %q\n      sha256 = %q\n    }\n", name, bundle.Asset, bundle.SHA256)
	}
	block.WriteString("  }\n")
	return block.String()
}

// programPaths reads the manifest for fixtureCase and returns each program's
// installation path.
func (h *hostArtifactTest) programPaths(fixtureCase string) []string {
	h.t.Helper()
	programs := h.manifestPrograms(h.pins[fixtureCase])
	if len(programs) == 0 {
		h.t.Fatalf("manifest for test case %s does not list any programs", fixtureCase)
	}
	paths := make([]string, 0, len(programs))
	for _, program := range programs {
		paths = append(paths, program.Path)
	}
	return paths
}

// manifestPrograms downloads the pinned archive and reads its manifest's
// programs list.
func (h *hostArtifactTest) manifestPrograms(pin hostArtifactPinEntry) []fixtureProgram {
	h.t.Helper()
	archive := h.download(pin.ReleaseTag, pin.Asset, pin.SHA256)
	decompressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		h.t.Fatalf("gzip reader could not open archive %s: %v", pin.Asset, err)
	}
	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			h.t.Fatalf("archive %s does not contain member %s", pin.Asset, manifestMember)
		}
		if err != nil {
			h.t.Fatalf("tar reader could not read the next member of archive %s: %v", pin.Asset, err)
		}
		if pathutil.Clean(header.Name) != manifestMember {
			continue
		}
		var manifest struct {
			Programs []fixtureProgram `json:"programs"`
		}
		if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
			h.t.Fatalf("JSON decoder could not parse member %s in archive %s: %v", manifestMember, pin.Asset, err)
		}
		return manifest.Programs
	}
}

// applyClean applies the configuration, checks that the node reports the
// pinned archive with every file and owned directory matching, and checks
// that the next plan is empty. It returns the GET response after the apply.
func (h *hostArtifactTest) applyClean(fixtureCase string) transport.HostArtifactState {
	h.t.Helper()
	h.workspace.mustRun("apply", "-no-color", "-auto-approve")
	current := h.get(transport.HostArtifactTarget(fixtureWorkspace))
	h.expectInstalled(current, h.pins[fixtureCase])
	output, exitCode := h.workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != 0 {
		h.t.Fatalf("plan after the apply of %s exited with status %d, want 0:\n%s", fixtureCase, exitCode, output)
	}
	return current
}

// expectRepair checks that the plan proposes a change. It calls applyClean to
// repair the target and check that the repeat plan is empty. It returns the
// GET response after the apply.
func (h *hostArtifactTest) expectRepair(fixtureCase string) transport.HostArtifactState {
	h.t.Helper()
	output, exitCode := h.workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != planChangesExitCode {
		h.t.Fatalf("plan exited with status %d, want %d:\n%s", exitCode, planChangesExitCode, output)
	}
	return h.applyClean(fixtureCase)
}

func (h *hostArtifactTest) expectInstalled(current transport.HostArtifactState, pin hostArtifactPinEntry) {
	h.t.Helper()
	if current.State != transport.HostArtifactStateCommitted {
		h.t.Fatalf("target state is %s, want %s", current.State, transport.HostArtifactStateCommitted)
	}
	if current.Installed == nil || current.Installed.SHA256 != pin.SHA256 {
		h.t.Fatalf("installed record is %+v, want archive sha256 %s", current.Installed, pin.SHA256)
	}
	for _, file := range current.Files {
		if !file.Matches {
			h.t.Fatalf("file %s does not match the installed record: %+v", file.Path, file)
		}
	}
	for _, directory := range current.Directories {
		if !directory.Matches {
			h.t.Fatalf("directory %s does not match the installed record: %+v", directory.Path, directory)
		}
	}
}

func (h *hostArtifactTest) expectDirectoryMode(current transport.HostArtifactState, path string, mode string) {
	h.t.Helper()
	for _, directory := range current.Directories {
		if directory.Path == path && directory.Mode == mode {
			return
		}
	}
	h.t.Fatalf("GET does not report directory %s with mode %s: %+v", path, mode, current.Directories)
}

func fileWriteIDs(current transport.HostArtifactState) map[string]string {
	writeIDs := make(map[string]string, len(current.Files))
	for _, file := range current.Files {
		writeIDs[file.Path] = file.WriteID
	}
	return writeIDs
}

func (h *hostArtifactTest) get(target string) transport.HostArtifactState {
	h.t.Helper()
	current, err := h.client.GetHostArtifact(context.Background(), h.node, target)
	if err != nil {
		h.t.Fatalf("GET host artifact %s: %v", target, err)
	}
	return current
}

// installDirectly stages and installs a fixture through the transport
// without the provider and waits for the task.
func (h *hostArtifactTest) installDirectly(workspace string, fixtureCase string) {
	h.t.Helper()
	target := transport.HostArtifactTarget(workspace)
	upid := h.startInstall(target, fixtureCase)
	if err := h.client.WaitTaskWithin(context.Background(), h.node, upid, hostTaskTimeout); err != nil {
		h.t.Fatalf("install of %s: %v", fixtureCase, err)
	}
	h.expectInstalled(h.get(target), h.pins[fixtureCase])
}

func (h *hostArtifactTest) startInstall(target string, fixtureCase string) string {
	h.t.Helper()
	request, _ := h.stageInstall(target, fixtureCase)
	upid, err := h.client.InstallHostArtifact(context.Background(), h.node, target, request)
	if err != nil {
		h.t.Fatalf("install %s: %v", fixtureCase, err)
	}
	return upid
}

func (h *hostArtifactTest) stageInstall(target string, fixtureCase string) (transport.HostArtifactInstall, func()) {
	h.t.Helper()
	pin := h.pins[fixtureCase]
	archive := h.download(pin.ReleaseTag, pin.Asset, pin.SHA256)
	bundle := h.download(pin.ReleaseTag, pin.BundleAsset, pin.BundleSHA256)
	ctx := context.Background()
	archiveStage, err := h.client.StageHostArtifact(
		ctx, h.node, target, transport.HostArtifactKindArchive, bytes.NewReader(archive), int64(len(archive)),
	)
	if err != nil {
		h.t.Fatalf("stage the archive of %s: %v", fixtureCase, err)
	}
	removeArchive := h.stageCleanup(target, archiveStage)
	bundleStage, err := h.client.StageHostArtifact(
		ctx, h.node, target, transport.HostArtifactKindBundle, bytes.NewReader(bundle), int64(len(bundle)),
	)
	if err != nil {
		h.t.Fatalf("stage the bundle of %s: %v", fixtureCase, err)
	}
	removeBundle := h.stageCleanup(target, bundleStage)
	programStages, removePrograms := h.stagePrograms(target, pin)
	return transport.HostArtifactInstall{
		ReleaseTag:     pin.ReleaseTag,
		Asset:          pin.Asset,
		SHA256:         pin.SHA256,
		SourceRevision: pin.SourceRevision,
		ArchiveStage:   archiveStage,
		BundleStage:    bundleStage,
		ProgramStages:  programStages,
	}, func() {
		removeArchive()
		removeBundle()
		removePrograms()
	}
}

// stagePrograms uploads each pinned program archive and bundle to host stages.
// It returns the program-stages value, or an empty string when the pin does
// not include programs.
func (h *hostArtifactTest) stagePrograms(target string, pin hostArtifactPinEntry) (string, func()) {
	h.t.Helper()
	if len(pin.ProgramBundles) == 0 {
		return "", func() {}
	}
	ctx := context.Background()
	var stages []transport.HostProgramStage
	var cleanups []func()
	for _, program := range h.manifestPrograms(pin) {
		bundle, found := pin.ProgramBundles[program.Name]
		if !found {
			h.t.Fatalf("program_bundles does not include program %s", program.Name)
		}
		archive := h.downloadFrom(program.Repository, program.ReleaseTag, program.Asset, program.SHA256)
		bundleContent := h.download(pin.ReleaseTag, bundle.Asset, bundle.SHA256)
		archiveStage, err := h.client.StageHostArtifactProgram(
			ctx, h.node, target, transport.HostArtifactKindProgram, program.Name, bytes.NewReader(archive), int64(len(archive)),
		)
		if err != nil {
			h.t.Fatalf("archive staging failed for program %s: %v", program.Name, err)
		}
		cleanups = append(cleanups, h.stageCleanup(target, archiveStage))
		bundleStage, err := h.client.StageHostArtifactProgram(
			ctx, h.node, target, transport.HostArtifactKindProgramBundle, program.Name, bytes.NewReader(bundleContent), int64(len(bundleContent)),
		)
		if err != nil {
			h.t.Fatalf("bundle staging failed for program %s: %v", program.Name, err)
		}
		cleanups = append(cleanups, h.stageCleanup(target, bundleStage))
		stages = append(stages, transport.HostProgramStage{Name: program.Name, ArchiveStage: archiveStage, BundleStage: bundleStage})
	}
	programStages, err := transport.FormatProgramStages(stages)
	if err != nil {
		h.t.Fatal(err)
	}
	return programStages, func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}
}

// stopInstall starts an install of fixtureCase over the installed priorCase.
// It waits until GET reports the install journal, then calls
// DELETE /nodes/{node}/tasks/{upid}. It checks the task result and target state.
func (h *hostArtifactTest) stopInstall(target string, priorCase string, fixtureCase string) {
	h.t.Helper()
	pin := h.pins[fixtureCase]
	upid := h.startInstall(target, fixtureCase)
	deadline := time.Now().Add(interruptDeadline)
	for {
		current := h.get(target)
		if current.State == transport.HostArtifactStateInterrupted {
			break
		}
		if current.Installed != nil && current.Installed.SHA256 == pin.SHA256 {
			h.t.Fatalf("the install of %s committed before the test stopped it", fixtureCase)
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the GET response did not report the journal of %s within %s", fixtureCase, interruptDeadline)
		}
		time.Sleep(interruptPollPeriod)
	}
	if err := h.client.StopTask(context.Background(), h.node, upid); err != nil {
		h.t.Fatalf("stop task %s with DELETE: %v", upid, err)
	}
	h.expectStopResult(target, h.pins[priorCase], h.waitTaskStopped(upid))
}

// waitTaskStopped polls until the node reports TaskStopped. It fails the test
// if a status read fails or the task does not stop before the deadline.
func (h *hostArtifactTest) waitTaskStopped(upid string) transport.TaskStatus {
	h.t.Helper()
	return h.waitTaskStoppedWithin(upid, taskStopDeadline)
}

func (h *hostArtifactTest) waitTaskStoppedWithin(upid string, timeout time.Duration) transport.TaskStatus {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status, err := h.client.GetTaskStatus(context.Background(), h.node, upid)
		if err != nil {
			h.t.Fatalf("read the status of task %s: %v", upid, err)
		}
		if status.Status == transport.TaskStopped {
			return status
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task %s did not stop within %s", upid, timeout)
		}
		time.Sleep(taskStopPollPeriod)
	}
}

// expectStopResult checks the SIGTERM and SIGKILL results of a stopped install.
// On SIGTERM, the hostartifact task restores the files and removes the journal
// before it ends with exit status "received interrupt". GET reports committed
// with the prior installed record.
// After SIGKILL, the task log does not end with a result line, and the task
// exits with "unexpected status". The journal stays, and GET reports
// interrupted. The next install restores the files first.
func (h *hostArtifactTest) expectStopResult(target string, prior hostArtifactPinEntry, status transport.TaskStatus) {
	h.t.Helper()
	current := h.get(target)
	journal := h.hostPathState(hostStateDirectory + target + "/" + journalRecord)
	switch status.ExitStatus {
	case taskInterruptedExit:
		if journal != absentSnapshotValue {
			h.t.Fatalf("journal after the task restored the files is %q, want %s", journal, absentSnapshotValue)
		}
		h.expectInstalled(current, prior)
	case taskKilledExit:
		if journal == absentSnapshotValue || current.State != transport.HostArtifactStateInterrupted {
			h.t.Fatalf("target state after SIGKILL is %s with journal %q, want %s with a journal",
				current.State, journal, transport.HostArtifactStateInterrupted)
		}
		if current.Installed == nil || current.Installed.SHA256 != prior.SHA256 {
			h.t.Fatalf("installed record after SIGKILL is %+v, want archive sha256 %s", current.Installed, prior.SHA256)
		}
	default:
		h.t.Fatalf("stopped task has exit status %q, want %q or %q", status.ExitStatus, taskInterruptedExit, taskKilledExit)
	}
}

func (h *hostArtifactTest) download(releaseTag string, asset string, wantHash string) []byte {
	h.t.Helper()
	return h.downloadFrom(hostArtifactRepository, releaseTag, asset, wantHash)
}

func (h *hostArtifactTest) downloadFrom(repository string, releaseTag string, asset string, wantHash string) []byte {
	h.t.Helper()
	address := releaseBaseURL + repository + "/releases/download/" + releaseTag + "/" + asset
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, address, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("download %s: %v", address, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		h.t.Fatalf("download %s: HTTP status %d", address, response.StatusCode)
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatalf("download %s: %v", address, err)
	}
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); got != wantHash {
		h.t.Fatalf("download %s has sha256 %s, want %s", address, got, wantHash)
	}
	return content
}

// snapshot records the GET response of the target and the type, mode,
// owner, inode, size, modification time, and SHA-256 of its files, owned
// directory, installed record, journal, stage directory, and lock file.
func (h *hostArtifactTest) snapshot(target string, directory string) map[string]string {
	h.t.Helper()
	stateDirectory := hostStateDirectory + target
	paths := []string{
		directory,
		directory + "/" + firstFixtureFile,
		directory + "/" + secondFixtureFile,
		stateDirectory + "/" + installedRecord,
		stateDirectory + "/" + journalRecord,
		stateDirectory + "/" + stageDirectory,
		stateDirectory + "/" + hostSecretRecord,
		directory + "/" + hostSecretFile,
		hostLockPrefix + target + hostLockSuffix,
	}
	snapshot := make(map[string]string, len(paths)+1)
	for _, path := range paths {
		snapshot[path] = h.hostPathState(path)
	}
	for _, root := range []string{directory, stateDirectory + "/" + stageDirectory} {
		if snapshot[root] == absentSnapshotValue {
			continue
		}
		entries := strings.Fields(h.hostMustRun("find", root, "-mindepth", "1", "-print"))
		slices.Sort(entries)
		snapshot["contents "+root] = strings.Join(entries, "\n")
		for _, path := range entries {
			snapshot[path] = h.hostPathState(path)
		}
	}
	response, err := json.Marshal(h.get(target))
	if err != nil {
		h.t.Fatal(err)
	}
	snapshot["GET "+target] = string(response)
	return snapshot
}

func (h *hostArtifactTest) expectSnapshot(want map[string]string, target string, directory string) {
	h.t.Helper()
	got := h.snapshot(target, directory)
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if got[key] != want[key] {
			h.t.Fatalf("%s changed from %q to %q", key, want[key], got[key])
		}
	}
}

func (h *hostArtifactTest) hostPathState(path string) string {
	h.t.Helper()
	status, stderr, exitCode := h.hostRun(nil, "stat", "--format=%F %a %u %g %i %s %y", "--", path)
	if exitCode != 0 {
		if strings.Contains(stderr, missingPathMessage) {
			return absentSnapshotValue
		}
		h.t.Fatalf("stat %s exited with status %d: %s", path, exitCode, stderr)
	}
	if !strings.HasPrefix(status, "regular") {
		return strings.TrimSpace(status)
	}
	return strings.TrimSpace(status) + " " + strings.TrimSpace(h.hostMustRun("sha256sum", "--", path))
}

func (h *hostArtifactTest) removeTarget(target string, directory string) {
	h.t.Helper()
	h.hostMustRun("rm", "-rf", "--", hostStateDirectory+target, directory)
}

func (h *hostArtifactTest) hostWrite(path string, content string) {
	h.t.Helper()
	_, stderr, exitCode := h.hostRun([]byte(content), "tee", "--", path)
	if exitCode != 0 {
		h.t.Fatalf("write %s on the node exited with status %d: %s", path, exitCode, stderr)
	}
}

func (h *hostArtifactTest) hostMustRun(argv ...string) string {
	h.t.Helper()
	stdout, stderr, exitCode := h.hostRun(nil, argv...)
	if exitCode != 0 {
		h.t.Fatalf("%q on the node exited with status %d: %s", argv, exitCode, stderr)
	}
	return stdout
}

// hostRun quotes each argument and runs argv as root on the node through
// ssh. ssh joins its arguments into one remote shell command.
func (h *hostArtifactTest) hostRun(stdin []byte, argv ...string) (string, string, int) {
	h.t.Helper()
	quoted := make([]string, 0, len(argv))
	for _, argument := range argv {
		quoted = append(quoted, transport.Quote(argument))
	}
	arguments := append(hostSSHOptions(h.sshControlPath), h.ssh, "--")
	arguments = append(arguments, quoted...)
	command := exec.CommandContext(context.Background(), "ssh", arguments...)
	command.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		h.t.Fatalf("ssh %s %q: %v", h.ssh, argv, err)
	}
	return stdout.String(), stderr.String(), exitError.ExitCode()
}
