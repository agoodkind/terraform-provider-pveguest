package acceptance_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
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

	hostTaskLogDirectory = "/var/log/pve/tasks"
	hostSecretTaskType   = "hostsecret"
	stateBackupSuffix    = ".backup"
	secondVersion        = 2
)

// hostSecretTest adds the markers of one run to hostArtifactTest. The test
// passes each marker to OpenTofu only through TF_VAR_secret.
type hostSecretTest struct {
	*hostArtifactTest
	target          string
	unrelatedTarget string
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
	unrelated := test.unrelatedSnapshot()

	first := test.newMarker()
	second := test.newMarker()
	storagePath := fixtureDirectory + "/" + hostSecretFile

	test.useSecret(first, 1)
	test.mustTofu("init", "-no-color")
	consumer := test.invocationID(hostSecretConsumer)
	test.expectWritePlanned()
	test.applySecret(1)
	consumer = test.expectRestart(consumer)
	test.expectUnrelated(unrelated)

	test.useSecret(second, secondVersion)
	test.expectWritePlanned()
	test.applySecret(secondVersion)
	consumer = test.expectRestart(consumer)
	test.expectUnrelated(unrelated)

	// A changed value at the same version does not plan a change.
	stable := test.fixtureSnapshot()
	test.useSecret(test.newMarker(), secondVersion)
	test.expectEmptyPlan()
	test.expectSecretSnapshot(stable, test.target, fixtureDirectory, hostSecretConsumer)
	test.useSecret(second, secondVersion)

	test.hostWrite(storagePath, hostSecretDriftContent)
	test.expectWritePlanned()
	test.applySecret(secondVersion)
	consumer = test.expectRestart(consumer)
	test.expectUnrelated(unrelated)

	test.expectMetadataRepair(storagePath, consumer, "chmod", hostSecretDriftMode, "--", storagePath)
	test.expectUnrelated(unrelated)
	test.expectMetadataRepair(storagePath, consumer, "chown", hostSecretDriftOwner, "--", storagePath)
	test.expectUnrelated(unrelated)

	// The node rejects a lower version without changing the target.
	stable = test.fixtureSnapshot()
	test.useSecret(first, 1)
	test.expectWritePlanned()
	output, exitCode := test.tofu("apply", "-no-color", "-auto-approve")
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), hostSecretLowerVersionText) {
		t.Fatalf("apply of a lower version exited with status %d, want a failure that contains %q:\n%s",
			exitCode, hostSecretLowerVersionText, output)
	}
	test.expectSecretSnapshot(stable, test.target, fixtureDirectory, hostSecretConsumer)
	test.expectUnrelated(unrelated)
	test.useSecret(second, secondVersion)
	test.expectEmptyPlan()

	// Destroy fails while the node stores the field.
	output, exitCode = test.tofu("destroy", "-no-color", "-auto-approve")
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), hostSecretDestroyText) {
		t.Fatalf("destroy exited with status %d, want a failure that contains %q:\n%s",
			exitCode, hostSecretDestroyText, output)
	}
	test.expectSecretSnapshot(stable, test.target, fixtureDirectory, hostSecretConsumer)
	test.expectUnrelated(unrelated)
	test.expectStateOmitsMarkers()
	test.mustTofu("state", "rm", hostSecretAddress)

	// Import into fresh state sets value_wo_version to the recorded version.
	// The matching configuration does not plan a write.
	test.workspace = test.newWorkspace()
	test.useSecret(second, secondVersion)
	test.mustTofu("init", "-no-color")
	test.mustTofu("import", "-no-color", hostSecretAddress, test.node+":"+fixtureWorkspace+":"+hostSecretField)
	test.expectSecretSnapshot(stable, test.target, fixtureDirectory, hostSecretConsumer)
	test.expectEmptyPlan()
	test.mustTofu("apply", "-no-color", "-auto-approve")
	test.expectSecretSnapshot(stable, test.target, fixtureDirectory, hostSecretConsumer)
	test.expectUnrelated(unrelated)
	test.expectStateOmitsMarkers()
	test.mustTofu("state", "rm", hostSecretAddress)

	test.expectTaskLogsOmitMarkers()
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
  value_wo         = var.secret
  value_wo_version = %d
}
`, providerSource, candidateInstalledVersion, s.node, s.nodeConfig.Endpoint, s.nodeConfig.APIToken, s.nodeConfig.Insecure,
		s.node, fixtureWorkspace, hostSecretField, version))
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

// applySecret applies the configuration, checks the GET entry of the field, and
// checks that the next plan is empty and that the state omits every marker.
func (s *hostSecretTest) applySecret(version int64) {
	s.t.Helper()
	s.mustTofu("apply", "-no-color", "-auto-approve")
	entry := s.secretEntry()
	if !entry.Configured || entry.Version != version {
		s.t.Fatalf("the secret field has configured %t and version %d, want configured true and version %d",
			entry.Configured, entry.Version, version)
	}
	s.expectEmptyPlan()
	s.expectStateOmitsMarkers()
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
func (s *hostSecretTest) expectMetadataRepair(storagePath string, consumer string, drift ...string) {
	s.t.Helper()
	before := s.hostPathState(storagePath)
	s.hostMustRun(drift...)
	s.expectWritePlanned()
	s.applySecret(secondVersion)
	if after := s.hostPathState(storagePath); after != before {
		s.t.Fatalf("storage file state after the repair is %q, want %q", after, before)
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

func (s *hostSecretTest) invocationID(unit string) string {
	s.t.Helper()
	identifier := strings.TrimSpace(s.hostMustRun("systemctl", "show", "--property=InvocationID", "--value", "--", unit))
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

// secretSnapshot adds the storage file, the secret record, and the consumer
// InvocationID to the snapshot of the target.
func (s *hostSecretTest) secretSnapshot(target string, directory string, unit string) map[string]string {
	s.t.Helper()
	snapshot := s.snapshot(target, directory)
	for _, path := range []string{directory + "/" + hostSecretFile, hostStateDirectory + target + "/" + hostSecretRecord} {
		snapshot[path] = s.hostPathState(path)
	}
	snapshot["InvocationID "+unit] = s.invocationID(unit)
	return snapshot
}

func (s *hostSecretTest) fixtureSnapshot() map[string]string {
	s.t.Helper()
	return s.secretSnapshot(s.target, fixtureDirectory, hostSecretConsumer)
}

func (s *hostSecretTest) unrelatedSnapshot() map[string]string {
	s.t.Helper()
	return s.secretSnapshot(s.unrelatedTarget, unrelatedFixtureDirectory, unrelatedHostSecretConsumer)
}

func (s *hostSecretTest) expectSecretSnapshot(want map[string]string, target string, directory string, unit string) {
	s.t.Helper()
	got := s.secretSnapshot(target, directory, unit)
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if got[key] != want[key] {
			s.t.Fatalf("%s changed from %q to %q", key, want[key], got[key])
		}
	}
}

func (s *hostSecretTest) expectUnrelated(want map[string]string) {
	s.t.Helper()
	s.expectSecretSnapshot(want, s.unrelatedTarget, unrelatedFixtureDirectory, unrelatedHostSecretConsumer)
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
