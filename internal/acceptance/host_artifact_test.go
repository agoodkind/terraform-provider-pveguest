package acceptance_test

import (
	"bytes"
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
	hostSSHVariable = "PVEGUEST_ACC_HOST_SSH"
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
}

type hostArtifactTest struct {
	t          *testing.T
	client     *transport.Client
	node       string
	nodeConfig transport.NodeConfig
	ssh        string
	pins       map[string]hostArtifactPinEntry
	workspace  *tofuWorkspace
}

func newHostArtifactTest(t *testing.T) *hostArtifactTest {
	t.Helper()
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	ssh := os.Getenv(hostSSHVariable)
	if ssh == "" {
		t.Skipf("%s is not set", hostSSHVariable)
	}
	pinFile := os.Getenv(hostArtifactPinsVariable)
	if pinFile == "" {
		t.Skipf("%s is not set", hostArtifactPinsVariable)
	}
	node := requireVariable(t, nodeVariable)
	nodeConfig := nodeConfigFromEnvironment(t)
	client, err := transport.NewClient(map[string]transport.NodeConfig{node: nodeConfig}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}
	test := &hostArtifactTest{
		t:          t,
		client:     client,
		node:       node,
		nodeConfig: nodeConfig,
		ssh:        ssh,
		pins:       readFixturePins(t, pinFile),
	}
	test.workspace = test.newWorkspace()
	// The provider and the test download release assets into this cache.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return test
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
	for fixtureCase, workspace := range workspaces {
		pin, found := pins[fixtureCase]
		if !found {
			t.Fatalf("%s does not contain the case %s", pinFile, fixtureCase)
		}
		want := fixtureAssetPrefix + fixtureCase + "-" + transport.HostArtifactTarget(workspace) + fixtureAssetSuffix
		if pin.Asset != want {
			t.Fatalf("%s pins asset %s for the case %s, want %s", pinFile, pin.Asset, fixtureCase, want)
		}
	}
	return pins
}

// TestAccHostArtifact runs every step against the fixture target and checks
// after each step that the files, metadata, installed record, journal, and
// lock of the unrelated fixture target are unchanged.
func TestAccHostArtifact(t *testing.T) {
	test := newHostArtifactTest(t)
	target := transport.HostArtifactTarget(fixtureWorkspace)
	unrelatedTarget := transport.HostArtifactTarget(unrelatedFixtureWorkspace)
	t.Cleanup(func() {
		test.removeTarget(target, fixtureDirectory)
		test.removeTarget(unrelatedTarget, unrelatedFixtureDirectory)
	})
	test.removeTarget(target, fixtureDirectory)

	test.installDirectly(unrelatedFixtureWorkspace, caseOtherTarget)
	unrelated := test.snapshot(unrelatedTarget, unrelatedFixtureDirectory)

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
}
`, providerSource, candidateInstalledVersion, h.node, h.nodeConfig.Endpoint, h.nodeConfig.APIToken, h.nodeConfig.Insecure,
		h.node, fixtureWorkspace, hostArtifactRepository, pin.ReleaseTag, pin.Asset, pin.SHA256,
		pin.BundleAsset, pin.BundleSHA256, pin.SourceRevision)
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
	bundleStage, err := h.client.StageHostArtifact(
		ctx, h.node, target, transport.HostArtifactKindBundle, bytes.NewReader(bundle), int64(len(bundle)),
	)
	if err != nil {
		h.t.Fatalf("stage the bundle of %s: %v", fixtureCase, err)
	}
	upid, err := h.client.InstallHostArtifact(ctx, h.node, target, transport.HostArtifactInstall{
		ReleaseTag:     pin.ReleaseTag,
		Asset:          pin.Asset,
		SHA256:         pin.SHA256,
		SourceRevision: pin.SourceRevision,
		ArchiveStage:   archiveStage,
		BundleStage:    bundleStage,
	})
	if err != nil {
		h.t.Fatalf("install %s: %v", fixtureCase, err)
	}
	return upid
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
	deadline := time.Now().Add(taskStopDeadline)
	for {
		status, err := h.client.GetTaskStatus(context.Background(), h.node, upid)
		if err != nil {
			h.t.Fatalf("read the status of task %s: %v", upid, err)
		}
		if status.Status == transport.TaskStopped {
			return status
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task %s did not stop within %s", upid, taskStopDeadline)
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
	address := releaseBaseURL + hostArtifactRepository + "/releases/download/" + releaseTag + "/" + asset
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
		hostLockPrefix + target + hostLockSuffix,
	}
	snapshot := make(map[string]string, len(paths)+1)
	for _, path := range paths {
		snapshot[path] = h.hostPathState(path)
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
	status, stderr, exitCode := h.hostRun(nil, "stat", "--format=%F %a %u %g %i %s %Y", "--", path)
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
	arguments := append([]string{"-o", "BatchMode=yes", h.ssh, "--"}, quoted...)
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
