package acceptance_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	hostSecretAddress = "pveguest_host_secret.test"
	hostSecretField   = "PVEGUEST_ACC_SECRET"
	hostSecretFile    = "secret.env"

	hostSecretConsumer          = "hostartifact-acceptance.service"
	unrelatedHostSecretConsumer = "hostartifact-acceptance-b.service"

	hostSecretRecord       = "secrets.json"
	hostSecretMarkerPrefix = "pveguest-acc-"
	hostSecretMarkerBytes  = 16
	hostSecretDriftContent = hostSecretField + "=drifted\n"
	hostSecretDriftMode    = "0644"
	hostSecretDriftOwner   = "daemon:daemon"
	hostSecretAppliedKey   = "applied_sha256"

	hostSecretLowerVersionText = "lower than the stored version"
	hostSecretDestroyText      = "does not delete a secret field"
	hostSecretMissingValueText = "Missing value_wo"
	hostSecretValueArgument    = "value_wo         = var.secret"

	// hostSecretRestartFailureText matches the consumer restart failure message.
	hostSecretRestartFailureText = "systemctl restart " + hostSecretConsumer + " failed"
	// hostSecretConsumerFailFile is the path that hostartifact-acceptance.service
	// checks in ExecStart. The unit fails to start while the file exists.
	hostSecretConsumerFailFile = "/run/hostartifact-acceptance.fail"

	hostTaskLogDirectory = "/var/log/pve/tasks"
	hostSecretTaskType   = "hostsecret"
	hostTaskRecordsKey   = "task records"
	hostLockAcquiredLine = "acquired\n"
	stateBackupSuffix    = ".backup"
	secondVersion        = 2
	thirdVersion         = 3
)

// hostSecretTest adds the markers of one run to hostArtifactTest. The test
// passes each marker to OpenTofu only through TF_VAR_secret.
type hostSecretTest struct {
	*hostArtifactTest
	target          string
	unrelatedTarget string
	unrelated       map[string]string
	markers         []string
}

// stateDocument contains the private data of each instance in a state file.
type stateDocument struct {
	Resources []struct {
		Instances []struct {
			Private string `json:"private"`
		} `json:"instances"`
	} `json:"resources"`
}

func newHostSecretTest(t *testing.T) *hostSecretTest {
	t.Helper()
	return &hostSecretTest{
		hostArtifactTest: newHostArtifactTest(t),
		target:           transport.HostArtifactTarget(fixtureWorkspace),
		unrelatedTarget:  transport.HostArtifactTarget(unrelatedFixtureWorkspace),
	}
}

// TestAccHostSecret checks the fixture target and verifies after each step that
// the unrelated fixture target has not changed. The test uses random synthetic
// markers and does not print them in failure messages.
func TestAccHostSecret(t *testing.T) {
	test := newHostSecretTest(t)
	t.Cleanup(func() {
		test.removeTarget(test.target, fixtureDirectory)
		test.removeTarget(test.unrelatedTarget, unrelatedFixtureDirectory)
	})
	test.removeTarget(test.target, fixtureDirectory)
	test.removeTarget(test.unrelatedTarget, unrelatedFixtureDirectory)

	// Installing the fixtures creates the storage file directories.
	test.installDirectly(fixtureWorkspace, caseValidV1)
	test.installDirectly(unrelatedFixtureWorkspace, caseOtherTarget)
	test.setUnrelatedSecret()
	test.unrelated = test.unrelatedSnapshot()
	// The test acquires the unrelated target lock during setup and releases it
	// during cleanup. A write to the selected target fails if it also tries to
	// acquire the unrelated target lock.
	test.acquireTargetLock(test.unrelatedTarget)

	first := test.newMarker()
	second := test.newMarker()
	storagePath := fixtureDirectory + "/" + hostSecretFile

	test.useSecret(first, 1)
	test.mustTofu("init", "-no-color")
	consumer := test.invocationID(hostSecretConsumer)
	test.expectWritePlanned()
	test.applySecret(first, 1)
	consumer = test.expectRestart(consumer)
	test.expectUnrelated()

	test.useSecret(second, secondVersion)
	test.expectWritePlanned()
	test.applySecret(second, secondVersion)
	consumer = test.expectRestart(consumer)
	test.expectUnrelated()

	test.expectSameVersionChangeIgnored(second)

	test.hostWrite(storagePath, hostSecretDriftContent)
	test.expectWritePlanned()
	test.applySecret(second, secondVersion)
	consumer = test.expectRestart(consumer)
	test.expectUnrelated()

	test.expectMetadataRepair(second, storagePath, consumer, "chmod", hostSecretDriftMode, "--", storagePath)
	test.expectUnrelated()
	test.expectMetadataRepair(second, storagePath, consumer, "chown", hostSecretDriftOwner, "--", storagePath)
	test.expectUnrelated()

	test.expectRestartFailureRetried(second, storagePath)
	test.expectUnrelated()

	stable := test.expectLowerVersionRejected(first, second)
	test.expectDestroyRefused(stable)
	test.expectImportWithoutWrite(second, stable)
	test.expectPendingRestartImported()

	test.expectTaskLogsOmitMarkers()
}

// expectSameVersionChangeIgnored changes value_wo at the recorded version and
// checks for an empty plan while the test owns the selected target lock.
func (s *hostSecretTest) expectSameVersionChangeIgnored(marker string) {
	s.t.Helper()
	stable := s.fixtureSnapshot()
	s.useSecret(s.newMarker(), secondVersion)
	release := s.acquireTargetLock(s.target)
	s.expectEmptyPlan()
	release()
	s.expectFixture(stable)
	s.expectUnrelated()
	s.useSecret(marker, secondVersion)
}

// expectRestartFailureRetried makes the consumer restart fail after the node
// repairs the storage file. It checks that restart_pending plans a write at the
// recorded version when the storage file SHA-256 matches the applied SHA-256.
// The next apply retries the restart.
func (s *hostSecretTest) expectRestartFailureRetried(marker string, storagePath string) {
	s.t.Helper()
	s.t.Cleanup(s.restoreConsumer)
	s.hostWrite(hostSecretConsumerFailFile, "")
	s.hostWrite(storagePath, hostSecretDriftContent)
	s.expectWritePlanned()
	s.expectApplyFailure(hostSecretRestartFailureText)
	s.expectFileContent(marker)
	s.expectRestartPending()
	consumer := s.unitInvocation(hostSecretConsumer)
	s.restoreConsumer()
	s.expectWritePlanned()
	s.applySecret(marker, secondVersion)
	s.expectRestart(consumer)
}

// expectPendingRestartImported imports a field after its consumer restart fails.
// The test owns the selected target lock during import and the following plans.
// Import does not write. A plan without value_wo fails with Missing value_wo.
// A plan with value_wo proposes a write that retries the restart.
func (s *hostSecretTest) expectPendingRestartImported() {
	s.t.Helper()
	marker := s.newMarker()
	s.t.Cleanup(s.restoreConsumer)
	s.hostWrite(hostSecretConsumerFailFile, "")
	s.setSecretWithFailedRestart(marker, thirdVersion)
	consumer := s.unitInvocation(hostSecretConsumer)
	pending := s.fixtureSnapshot()

	s.workspace = s.newWorkspace()
	s.useSecretWithoutValue(thirdVersion)
	s.mustTofu("init", "-no-color")
	release := s.acquireTargetLock(s.target)
	s.mustTofu("import", "-no-color", hostSecretAddress, s.node+":"+fixtureWorkspace+":"+hostSecretField)
	s.expectFixture(pending)
	output, exitCode := s.tofu("plan", "-no-color")
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), hostSecretMissingValueText) {
		s.t.Fatalf("plan exited with status %d; it must fail with diagnostic %q:\n%s",
			exitCode, hostSecretMissingValueText, output)
	}
	s.useSecret(marker, thirdVersion)
	s.expectWritePlanned()
	release()
	s.expectFixture(pending)
	s.expectUnrelated()

	s.restoreConsumer()
	s.applySecret(marker, thirdVersion)
	s.expectRestart(consumer)
	s.expectUnrelated()
	s.mustTofu("state", "rm", hostSecretAddress)
}

// setSecretWithFailedRestart writes the field through the transport and checks
// that the task reports a consumer restart failure after writing the file.
func (s *hostSecretTest) setSecretWithFailedRestart(marker string, version int64) {
	s.t.Helper()
	upid, err := s.client.SetHostSecret(context.Background(), s.node, s.target, hostSecretField, marker, version)
	if err != nil {
		s.t.Fatalf("the transport could not set the fixture target's secret field: %v", err)
	}
	if status := s.waitTaskStopped(upid); !strings.Contains(status.ExitStatus, hostSecretRestartFailureText) {
		s.t.Fatalf("task %s did not report a consumer restart failure", upid)
	}
	s.expectFileContent(marker)
	s.expectRestartPending()
}

func (s *hostSecretTest) expectRestartPending() {
	s.t.Helper()
	if !s.secretEntry().RestartPending {
		s.t.Fatal("the GET secrets[] entry reports restart_pending as false after the consumer restart failed")
	}
}

func (s *hostSecretTest) restoreConsumer() {
	s.t.Helper()
	s.hostMustRun("rm", "-f", "--", hostSecretConsumerFailFile)
}

// expectLowerVersionRejected checks that the node rejects a lower version
// without changing the target files or consumer InvocationID. The node creates
// one task log. A plan with the recorded version is empty. The function returns
// the target snapshot after the task.
func (s *hostSecretTest) expectLowerVersionRejected(lower string, recorded string) map[string]string {
	s.t.Helper()
	before := s.fixtureSnapshot()
	s.useSecret(lower, 1)
	s.expectWritePlanned()
	s.expectApplyFailure(hostSecretLowerVersionText)
	after := s.fixtureSnapshot()
	for _, key := range slices.Sorted(maps.Keys(before)) {
		if key != hostTaskRecordsKey && after[key] != before[key] {
			s.t.Fatalf("%s changed after the node rejected the lower version", key)
		}
	}
	created := len(strings.Fields(after[hostTaskRecordsKey])) - len(strings.Fields(before[hostTaskRecordsKey]))
	if created != 1 {
		s.t.Fatalf("the rejected write created %d task logs instead of 1", created)
	}
	s.expectUnrelated()
	s.useSecret(recorded, secondVersion)
	s.expectEmptyPlan()
	return after
}

func (s *hostSecretTest) expectDestroyRefused(stable map[string]string) {
	s.t.Helper()
	output, exitCode := s.tofu("destroy", "-no-color", "-auto-approve")
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), hostSecretDestroyText) {
		s.t.Fatalf("destroy exited with status %d, want a failure that contains %q:\n%s",
			exitCode, hostSecretDestroyText, output)
	}
	s.expectFixture(stable)
	s.expectUnrelated()
	s.expectStateOmitsMarkers()
	s.mustTofu("state", "rm", hostSecretAddress)
}

// expectImportWithoutWrite imports the field into fresh state, plans, and applies
// the matching configuration while the test owns the selected target lock.
// PVE::Tools::lock_file acquires a flock on the same file with a 10-second
// timeout. A PUT would fail on that timeout and create one task log.
func (s *hostSecretTest) expectImportWithoutWrite(marker string, stable map[string]string) {
	s.t.Helper()
	s.workspace = s.newWorkspace()
	s.useSecret(marker, secondVersion)
	s.mustTofu("init", "-no-color")
	release := s.acquireTargetLock(s.target)
	s.mustTofu("import", "-no-color", hostSecretAddress, s.node+":"+fixtureWorkspace+":"+hostSecretField)
	s.expectFixture(stable)
	s.expectEmptyPlan()
	s.mustTofu("apply", "-no-color", "-auto-approve")
	release()
	s.expectFixture(stable)
	s.expectUnrelated()
	s.expectStateOmitsMarkers()
	s.mustTofu("state", "rm", hostSecretAddress)
}

// acquireTargetLock acquires the target lock with flock over ssh and returns a
// function that releases it. PVE::Tools::lock_file acquires a flock on the same
// file with a 10-second timeout. A PUT fails on that timeout while the test owns
// the lock. flock runs cat until the test closes its input.
func (s *hostSecretTest) acquireTargetLock(target string) func() {
	s.t.Helper()
	lockPath := hostLockPrefix + target + hostLockSuffix
	command := exec.CommandContext(
		context.Background(), "ssh", "-o", "BatchMode=yes", s.ssh, "--",
		"flock", "--nonblock", "--exclusive", transport.Quote(lockPath), "cat",
	)
	input, err := command.StdinPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		s.t.Fatalf("acquire %s: %v", lockPath, err)
	}
	if _, err := io.WriteString(input, hostLockAcquiredLine); err != nil {
		s.t.Fatalf("acquire %s: %v", lockPath, err)
	}
	// cat returns the line only after flock acquires the lock.
	if _, err := bufio.NewReader(output).ReadString('\n'); err != nil {
		s.t.Fatalf("acquire %s: %v: %s", lockPath, err, stderr.String())
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		closeErr := input.Close()
		if err := errors.Join(closeErr, command.Wait()); err != nil {
			s.t.Errorf("release %s: %v: %s", lockPath, err, stderr.String())
		}
	}
	s.t.Cleanup(release)
	return release
}

// newMarker returns a new random value that matches the pattern of the policy
// field. The test does not print the value.
func (s *hostSecretTest) newMarker() string {
	s.t.Helper()
	random := make([]byte, hostSecretMarkerBytes)
	if _, err := rand.Read(random); err != nil {
		s.t.Fatal(err)
	}
	marker := hostSecretMarkerPrefix + hex.EncodeToString(random)
	s.markers = append(s.markers, marker)
	return marker
}

// forbiddenTexts returns each marker and the SHA-256 of each marker.
func (s *hostSecretTest) forbiddenTexts() []string {
	texts := make([]string, 0, 2*len(s.markers))
	for _, marker := range s.markers {
		sum := sha256.Sum256([]byte(marker))
		texts = append(texts, marker, hex.EncodeToString(sum[:]))
	}
	return texts
}

func (s *hostSecretTest) containsForbidden(content []byte) bool {
	return slices.ContainsFunc(s.forbiddenTexts(), func(text string) bool {
		return bytes.Contains(content, []byte(text))
	})
}

// tofu runs OpenTofu and fails without printing the output when the output
// contains a marker.
func (s *hostSecretTest) tofu(arguments ...string) (string, int) {
	s.t.Helper()
	output, exitCode := s.workspace.run(arguments...)
	if s.containsForbidden([]byte(output)) {
		s.t.Fatalf("the output of tofu %s contains a secret marker", arguments[0])
	}
	return output, exitCode
}

func (s *hostSecretTest) mustTofu(arguments ...string) string {
	s.t.Helper()
	output, exitCode := s.tofu(arguments...)
	if exitCode != 0 {
		s.t.Fatalf("tofu %s exited with status %d:\n%s", strings.Join(arguments, " "), exitCode, output)
	}
	return output
}

// useSecret writes the configuration and sets the value that later OpenTofu
// commands read from TF_VAR_secret.
func (s *hostSecretTest) useSecret(marker string, version int) {
	s.t.Helper()
	s.workspace.variableValue = marker
	s.writeSecretConfig(hostSecretValueArgument, version)
}

// useSecretWithoutValue writes a configuration without value_wo.
func (s *hostSecretTest) useSecretWithoutValue(version int) {
	s.t.Helper()
	s.writeSecretConfig("", version)
}

func (s *hostSecretTest) writeSecretConfig(valueArgument string, version int) {
	s.t.Helper()
	s.workspace.writeConfig(fmt.Sprintf(`
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

variable "secret" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "pveguest_host_secret" "test" {
  node             = %q
  workspace        = %q
  field            = %q
  %s
  value_wo_version = %d
}
`, providerSource, candidateInstalledVersion, s.node, s.nodeConfig.Endpoint, s.nodeConfig.APIToken, s.nodeConfig.Insecure,
		s.node, fixtureWorkspace, hostSecretField, valueArgument, version))
}

// expectWritePlanned checks that the plan proposes a change and that the saved
// plan and its JSON form do not contain a marker.
func (s *hostSecretTest) expectWritePlanned() {
	s.t.Helper()
	output, exitCode := s.tofu("plan", "-no-color", "-detailed-exitcode", "-out", planFileName)
	if exitCode != planChangesExitCode {
		s.t.Fatalf("plan exited with status %d, want %d:\n%s", exitCode, planChangesExitCode, output)
	}
	for _, text := range s.forbiddenTexts() {
		s.workspace.requirePlanArchiveOmits(planFileName, text)
	}
	s.mustTofu("show", "-json", planFileName)
}

func (s *hostSecretTest) expectEmptyPlan() {
	s.t.Helper()
	output, exitCode := s.tofu("plan", "-no-color", "-detailed-exitcode")
	if exitCode != 0 {
		s.t.Fatalf("plan exited with status %d, want 0:\n%s", exitCode, output)
	}
}

// applySecret applies the saved plan that expectWritePlanned inspected.
// It checks the field's GET entry and storage file SHA-256. It also checks
// that the next plan is empty and state does not contain any marker.
func (s *hostSecretTest) applySecret(marker string, version int64) {
	s.t.Helper()
	s.mustTofu("apply", "-no-color", planFileName)
	entry := s.secretEntry()
	if !entry.Configured || entry.Version != version || entry.RestartPending {
		s.t.Fatalf("the GET secrets[] entry reports configured as %t, version as %d, and restart_pending as %t instead of configured true, version %d, and restart_pending false",
			entry.Configured, entry.Version, entry.RestartPending, version)
	}
	s.expectFileContent(marker)
	s.expectEmptyPlan()
	s.expectStateOmitsMarkers()
}

// expectFileContent compares the GET SHA-256 with the SHA-256 of the complete
// FIELD=value file, including the final newline. It does not print either value.
func (s *hostSecretTest) expectFileContent(marker string) {
	s.t.Helper()
	sum := sha256.Sum256([]byte(hostSecretField + "=" + marker + "\n"))
	if s.secretEntry().SHA256 != hex.EncodeToString(sum[:]) {
		s.t.Fatal("storage file SHA-256 differs from the expected SHA-256 for the complete FIELD=value file with a final newline")
	}
}

// expectApplyFailure applies the saved plan and checks that apply fails with
// the specified text.
func (s *hostSecretTest) expectApplyFailure(text string) {
	s.t.Helper()
	output, exitCode := s.tofu("apply", "-no-color", planFileName)
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), text) {
		s.t.Fatalf("apply exited with status %d, want a failure that contains %q:\n%s", exitCode, text, output)
	}
}

func (s *hostSecretTest) secretEntry() transport.HostSecretState {
	s.t.Helper()
	for _, entry := range s.get(s.target).Secrets {
		if entry.Field == hostSecretField {
			return entry
		}
	}
	s.t.Fatalf("the GET response of the fixture target does not list the field %s", hostSecretField)
	return transport.HostSecretState{}
}

// expectMetadataRepair runs the drift command, checks that the plan proposes a
// repair, and checks that the apply restores the storage file without a
// consumer restart.
func (s *hostSecretTest) expectMetadataRepair(marker string, storagePath string, consumer string, drift ...string) {
	s.t.Helper()
	before := s.hostPathState(storagePath)
	s.hostMustRun(drift...)
	s.expectWritePlanned()
	s.applySecret(marker, secondVersion)
	if s.hostPathState(storagePath) != before {
		s.t.Fatalf("storage file state for %s differs from its state before the drift", storagePath)
	}
	if after := s.invocationID(hostSecretConsumer); after != consumer {
		s.t.Fatalf("%s has InvocationID %s after the metadata repair, want %s", hostSecretConsumer, after, consumer)
	}
}

// setUnrelatedSecret stores a marker in the unrelated target through the
// transport.
func (s *hostSecretTest) setUnrelatedSecret() {
	s.t.Helper()
	ctx := context.Background()
	upid, err := s.client.SetHostSecret(ctx, s.node, s.unrelatedTarget, hostSecretField, s.newMarker(), 1)
	if err != nil {
		s.t.Fatalf("set the secret field of the unrelated target: %v", err)
	}
	if err := s.client.WaitTaskWithin(ctx, s.node, upid, hostTaskTimeout); err != nil {
		s.t.Fatalf("set the secret field of the unrelated target: %v", err)
	}
}

// unitInvocation returns the unit's InvocationID. A unit in the failed state can
// report an empty value.
func (s *hostSecretTest) unitInvocation(unit string) string {
	s.t.Helper()
	return strings.TrimSpace(s.hostMustRun("systemctl", "show", "--property=InvocationID", "--value", "--", unit))
}

func (s *hostSecretTest) invocationID(unit string) string {
	s.t.Helper()
	identifier := s.unitInvocation(unit)
	if identifier == "" {
		s.t.Fatalf("%s does not have an InvocationID; start the consumer unit before the test", unit)
	}
	return identifier
}

// expectRestart checks that the consumer has a new InvocationID and returns it.
func (s *hostSecretTest) expectRestart(before string) string {
	s.t.Helper()
	after := s.invocationID(hostSecretConsumer)
	if after == before {
		s.t.Fatalf("%s kept InvocationID %s after the write", hostSecretConsumer, before)
	}
	return after
}

// secretSnapshot adds the storage file, secret record, consumer InvocationID,
// and target task log file names to the target snapshot. An idempotent PUT
// does not change existing files and creates one task log.
func (s *hostSecretTest) secretSnapshot(target string, directory string, unit string) map[string]string {
	s.t.Helper()
	snapshot := s.snapshot(target, directory)
	for _, path := range []string{directory + "/" + hostSecretFile, hostStateDirectory + target + "/" + hostSecretRecord} {
		snapshot[path] = s.hostPathState(path)
	}
	snapshot["InvocationID "+unit] = s.unitInvocation(unit)
	snapshot[hostTaskRecordsKey] = s.taskRecords(target)
	return snapshot
}

// taskRecords returns sorted paths to the target's task logs. Each task log file
// name is its UPID. Each host artifact task uses the target as its ID.
func (s *hostSecretTest) taskRecords(target string) string {
	s.t.Helper()
	records := strings.Fields(s.hostMustRun("find", hostTaskLogDirectory, "-type", "f", "-name", "*:"+target+":*"))
	slices.Sort(records)
	return strings.Join(records, "\n")
}

func (s *hostSecretTest) fixtureSnapshot() map[string]string {
	s.t.Helper()
	return s.secretSnapshot(s.target, fixtureDirectory, hostSecretConsumer)
}

func (s *hostSecretTest) unrelatedSnapshot() map[string]string {
	s.t.Helper()
	return s.secretSnapshot(s.unrelatedTarget, unrelatedFixtureDirectory, unrelatedHostSecretConsumer)
}

// expectSecretSnapshot compares target snapshots and reports only the changed
// key. It does not print snapshot values, which can contain the storage file
// SHA-256.
func (s *hostSecretTest) expectSecretSnapshot(want map[string]string, target string, directory string, unit string) {
	s.t.Helper()
	got := s.secretSnapshot(target, directory, unit)
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if got[key] != want[key] {
			s.t.Fatalf("%s changed", key)
		}
	}
}

func (s *hostSecretTest) expectFixture(want map[string]string) {
	s.t.Helper()
	s.expectSecretSnapshot(want, s.target, fixtureDirectory, hostSecretConsumer)
}

func (s *hostSecretTest) expectUnrelated() {
	s.t.Helper()
	s.expectSecretSnapshot(s.unrelated, s.unrelatedTarget, unrelatedFixtureDirectory, unrelatedHostSecretConsumer)
}

// expectStateOmitsMarkers scans the raw bytes of each state file and the
// provider private data of each instance.
func (s *hostSecretTest) expectStateOmitsMarkers() {
	s.t.Helper()
	path := filepath.Join(s.workspace.directory, stateFileName)
	s.expectStateFileOmitsMarkers(path)
	backup := path + stateBackupSuffix
	if _, err := os.Stat(backup); !errors.Is(err, fs.ErrNotExist) {
		s.expectStateFileOmitsMarkers(backup)
	}
}

func (s *hostSecretTest) expectStateFileOmitsMarkers(path string) {
	s.t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatal(err)
	}
	if s.containsForbidden(content) {
		s.t.Fatalf("%s contains a secret marker", path)
	}
	var document stateDocument
	if err := json.Unmarshal(content, &document); err != nil {
		s.t.Fatalf("parse %s: %v", path, err)
	}
	for _, resourceState := range document.Resources {
		for _, instance := range resourceState.Instances {
			s.expectPrivateOmitsMarkers(path, instance.Private)
		}
	}
}

// expectPrivateOmitsMarkers decodes the private data, a base64 JSON object with
// base64 values, and requires the applied SHA-256 key.
func (s *hostSecretTest) expectPrivateOmitsMarkers(path string, private string) {
	s.t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(private)
	if err != nil {
		s.t.Fatalf("decode the private data in %s: %v", path, err)
	}
	if s.containsForbidden(decoded) {
		s.t.Fatalf("the private data in %s contains a secret marker", path)
	}
	var entries map[string][]byte
	if err := json.Unmarshal(decoded, &entries); err != nil {
		s.t.Fatalf("parse the private data in %s: %v", path, err)
	}
	if _, found := entries[hostSecretAppliedKey]; !found {
		s.t.Fatalf("the private data in %s does not contain %s", path, hostSecretAppliedKey)
	}
	for key, value := range entries {
		if s.containsForbidden(value) {
			s.t.Fatalf("the value for private key %s in %s contains a secret marker", key, path)
		}
	}
}

// expectTaskLogsOmitMarkers reads the hostsecret task logs of both targets
// through root ssh and checks each log in memory.
func (s *hostSecretTest) expectTaskLogsOmitMarkers() {
	s.t.Helper()
	for _, target := range []string{s.target, s.unrelatedTarget} {
		pattern := "*:" + hostSecretTaskType + ":" + target + ":*"
		listing := s.hostMustRun("find", hostTaskLogDirectory, "-type", "f", "-name", pattern)
		logs := strings.Fields(listing)
		if len(logs) == 0 {
			s.t.Fatalf("%s does not contain a hostsecret task log of target %s", hostTaskLogDirectory, target)
		}
		for _, log := range logs {
			if s.containsForbidden([]byte(s.hostMustRun("cat", "--", log))) {
				s.t.Fatalf("task log %s contains a secret marker", log)
			}
		}
	}
}
