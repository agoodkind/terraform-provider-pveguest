package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	hostAuditTokenVariable = "PVEGUEST_ACC_HOST_AUDIT_TOKEN_FILE"
	hostPolicyDirectory    = "/etc/pve-overlay/host-artifacts/targets/"
	hostPolicyBackup       = "/acceptance-policy-original.json"
	hostTaskSuccess        = "OK"
)

type hostArtifactDefect struct {
	fixtureCase string
	failureText string
}

var hostArtifactDefects = []hostArtifactDefect{
	{fixtureCase: "digest", failureText: "has SHA-256"},
	{fixtureCase: "size", failureText: "bytes, and the manifest lists"},
	{fixtureCase: "symlink", failureText: "is not a regular file"},
	{fixtureCase: "hardlink", failureText: "is not a regular file"},
	{fixtureCase: "traversal", failureText: "has an empty, '.', or '..' component"},
	{fixtureCase: "absolute", failureText: "has an absolute name"},
	{fixtureCase: "extra", failureText: "is not in the manifest"},
	{fixtureCase: "missing", failureText: "does not contain the member"},
	{fixtureCase: "duplicate", failureText: "more than once"},
	{fixtureCase: "member-mode", failureText: "has mode bits outside 0777"},
}

func newHostAuditClient(t *testing.T, node string, config transport.NodeConfig) (*transport.Client, string) {
	t.Helper()
	tokenFile := requireVariable(t, hostAuditTokenVariable)
	content, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("%s: %v", hostAuditTokenVariable, err)
	}
	auditToken := strings.TrimSpace(string(content))
	if auditToken == config.APIToken {
		t.Fatalf("%s must contain a separate audit token", hostAuditTokenVariable)
	}
	config.APIToken = auditToken
	client, err := transport.NewClient(map[string]transport.NodeConfig{node: config}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatalf("%s: %v", hostAuditTokenVariable, err)
	}
	principal, _, found := strings.Cut(auditToken, "=")
	if !found {
		t.Fatalf("%s must contain the complete token identity and value", hostAuditTokenVariable)
	}
	return client, principal
}

// stageCleanup removes only the data and record of a stage that this request created.
// Failed installs retain stages until expiry; the acceptance test removes its own stages.
func (h *hostArtifactTest) stageCleanup(target string, stage string) func() {
	h.t.Helper()
	if len(stage) != 32 || strings.Trim(stage, "0123456789abcdef") != "" {
		h.t.Fatal("the stage response does not contain a 32-character lowercase hexadecimal stage ID")
	}
	path := hostStateDirectory + target + "/" + stageDirectory + "/" + stage
	removed := false
	remove := func() {
		if removed {
			return
		}
		h.hostMustRun("rm", "-f", "--", path, path+".json")
		if h.hostPathState(path) != absentSnapshotValue || h.hostPathState(path+".json") != absentSnapshotValue {
			h.t.Fatal("the request stage still exists after scoped cleanup")
		}
		removed = true
	}
	h.t.Cleanup(remove)
	return remove
}

func (h *hostArtifactTest) expectHostRejections(unrelated map[string]string) {
	h.t.Helper()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	for _, defect := range hostArtifactDefects {
		h.expectRejectedInstall(defect.fixtureCase, defect.failureText, unrelated, func() (transport.HostArtifactInstall, func()) {
			return h.stageInstall(target, defect.fixtureCase)
		})
	}
	h.expectRejectedInstall(caseOtherTarget, "manifest workspace is", unrelated, func() (transport.HostArtifactInstall, func()) {
		return h.stageInstall(target, caseOtherTarget)
	})
	for _, policyCase := range []string{"workflow_uri", "source_ref", "environment", "omitted-destination"} {
		failureText := "gh attestation verify exited with code"
		if policyCase == "environment" {
			failureText = "manifest environment is"
		}
		if policyCase == "omitted-destination" {
			failureText = "the policy does not list the file " + fixtureDirectory + "/" + secondFixtureFile
		}
		h.expectRejectedInstall(policyCase, failureText, unrelated, func() (transport.HostArtifactInstall, func()) {
			request, cleanup := h.stageInstall(target, caseValidV2)
			restore := h.changeFixturePolicy(policyCase)
			return request, func() {
				restore()
				cleanup()
			}
		})
	}
	h.expectRejectedInstall("source-revision", "gh attestation verify exited with code", unrelated, func() (transport.HostArtifactInstall, func()) {
		request, cleanup := h.stageInstall(target, caseValidV2)
		request.SourceRevision = h.pins[caseRecoveryV1].SourceRevision
		if request.SourceRevision == h.pins[caseValidV2].SourceRevision {
			h.t.Fatal("the source-revision control requires different retained valid-v2 and recovery-v1 source revisions")
		}
		return request, cleanup
	})
}

// rejectionSnapshot excludes request-owned stages and their directory metadata.
// It still compares installed files, records, GET, journal, and the target lock.
func (h *hostArtifactTest) rejectionSnapshot(target string, directory string) map[string]string {
	h.t.Helper()
	snapshot := h.snapshot(target, directory)
	stages := hostStateDirectory + target + "/" + stageDirectory
	for key := range snapshot {
		if key == stages || key == "contents "+stages || strings.HasPrefix(key, stages+"/") {
			delete(snapshot, key)
		}
	}
	return snapshot
}

func (h *hostArtifactTest) expectRejectedInstall(label string, failureText string, unrelated map[string]string, prepare func() (transport.HostArtifactInstall, func())) {
	h.t.Helper()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	unrelatedTarget := transport.HostArtifactTarget(unrelatedFixtureWorkspace)
	before := h.rejectionSnapshot(target, fixtureDirectory)
	priorTasks := strings.Fields(h.taskRecords(target))
	unrelatedTasks := h.taskRecords(unrelatedTarget)
	stages := h.stageSnapshot(target)
	request, cleanup := prepare()
	defer cleanup()
	upid, err := h.client.InstallHostArtifact(context.Background(), h.node, target, request)
	if err != nil {
		h.t.Fatalf("%s did not start an install task: %v", label, err)
	}
	defer h.stopUnfinishedTask(upid)
	status := h.waitTaskStoppedWithin(upid, hostTaskTimeout)
	if status.Status != transport.TaskStopped || status.ExitStatus == hostTaskSuccess || !strings.Contains(status.ExitStatus, failureText) {
		h.t.Fatalf("%s task %s reports status %q and exit %q; require a stopped task with %q", label, upid, status.Status, status.ExitStatus, failureText)
	}
	cleanup()
	h.expectSnapshotValues(before, h.rejectionSnapshot(target, fixtureDirectory))
	h.expectSnapshotValues(stages, h.stageSnapshot(target))
	created := []string{}
	afterTasks := strings.Fields(h.taskRecords(target))
	if len(afterTasks) != len(priorTasks)+1 {
		h.t.Fatalf("%s did not preserve earlier task records and add one failed task", label)
	}
	for _, record := range afterTasks {
		if !slices.Contains(priorTasks, record) {
			created = append(created, record)
		}
	}
	if len(created) != 1 || filepath.Base(created[0]) != upid {
		h.t.Fatalf("%s did not create exactly its returned task %s", label, upid)
	}
	log := h.hostMustRun("cat", "--", created[0])
	for _, line := range strings.Split(log, "\n") {
		for _, mutation := range []string{"wrote ", "deleted ", "prepared the directory ", "every file matches the archive", "the transaction failed:", "restoring the files ", "completing the committed transaction "} {
			if strings.HasPrefix(line, mutation) {
				h.t.Fatalf("%s task %s started an installed-state mutation", label, upid)
			}
		}
	}
	if !strings.HasPrefix(failureText, "gh attestation verify") && !strings.Contains(log, "gh attestation verify accepted "+request.Asset+" with SHA-256 "+request.SHA256) {
		h.t.Fatalf("%s task %s did not verify the signed fixture before rejecting its manifest or archive", label, upid)
	}
	h.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)
	h.expectUnrelatedInvocation()
	if h.taskRecords(unrelatedTarget) != unrelatedTasks {
		h.t.Fatalf("%s changed the unrelated target task history", label)
	}
	h.t.Logf("%s: task %s rejected the install with %q before installed state changed", label, upid, status.ExitStatus)
}

func (h *hostArtifactTest) stopUnfinishedTask(upid string) {
	h.t.Helper()
	status, err := h.client.GetTaskStatus(context.Background(), h.node, upid)
	if err != nil {
		h.t.Errorf("read task %s before cleanup: %v", upid, err)
		return
	}
	if status.Status == transport.TaskStopped {
		return
	}
	if err := h.client.StopTask(context.Background(), h.node, upid); err != nil {
		h.t.Errorf("stop task %s before cleanup: %v", upid, err)
		return
	}
	h.waitTaskStopped(upid)
}

func (h *hostArtifactTest) expectUnrelatedInvocation() {
	h.t.Helper()
	current := (&hostSecretTest{hostArtifactTest: h}).invocationID(unrelatedHostSecretConsumer)
	if h.unrelatedInvocationID == "" || current != h.unrelatedInvocationID {
		h.t.Fatalf("%s changed its InvocationID during a selected-target control", unrelatedHostSecretConsumer)
	}
}

func (h *hostArtifactTest) stageSnapshot(target string) map[string]string {
	h.t.Helper()
	entries := strings.Fields(h.hostMustRun("find", hostStateDirectory+target+"/"+stageDirectory, "-mindepth", "1", "-print"))
	slices.Sort(entries)
	snapshot := map[string]string{"entries": strings.Join(entries, "\n")}
	for _, path := range entries {
		snapshot[path] = h.hostPathState(path)
	}
	return snapshot
}

func (h *hostArtifactTest) expectSnapshotValues(want map[string]string, got map[string]string) {
	h.t.Helper()
	if len(want) != len(got) {
		h.t.Fatal("the target snapshot changed its path count")
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if want[key] != got[key] {
			h.t.Fatalf("%s changed", key)
		}
	}
}

// changeFixturePolicy edits only A's policy and restores its exact bytes, inode,
// mode, owner, group, and modification time on return or test cleanup.
func (h *hostArtifactTest) changeFixturePolicy(policyCase string) func() {
	h.t.Helper()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	path := hostPolicyDirectory + target + ".json"
	before := h.hostPathState(path)
	original := h.hostMustRun("cat", "--", path)
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(original), &document); err != nil {
		h.t.Fatalf("parse the fixture policy: %v", err)
	}
	switch policyCase {
	case "environment":
		document["environment"] = json.RawMessage(`"acceptance-denied"`)
	case "workflow_uri", "source_ref":
		var configuration map[string]json.RawMessage
		if err := json.Unmarshal(document["configuration"], &configuration); err != nil {
			h.t.Fatalf("parse the fixture configuration policy: %v", err)
		}
		var permitted string
		if err := json.Unmarshal(configuration[policyCase], &permitted); err != nil {
			h.t.Fatalf("parse the fixture %s policy: %v", policyCase, err)
		}
		denied := permitted + "-acceptance-denied"
		if policyCase == "workflow_uri" {
			workflow, reference, found := strings.Cut(permitted, "@")
			if !found {
				h.t.Fatal("the fixture workflow_uri must contain its workflow reference after @")
			}
			denied = workflow + ".acceptance-denied@" + reference
		}
		changed, err := json.Marshal(denied)
		if err != nil {
			h.t.Fatal(err)
		}
		configuration[policyCase] = changed
		encoded, err := json.Marshal(configuration)
		if err != nil {
			h.t.Fatal(err)
		}
		document["configuration"] = encoded
	case "omitted-destination":
		var files []json.RawMessage
		if err := json.Unmarshal(document["files"], &files); err != nil {
			h.t.Fatalf("parse the fixture file policy: %v", err)
		}
		retained := []json.RawMessage{}
		for _, file := range files {
			var entry struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(file, &entry); err != nil {
				h.t.Fatal(err)
			}
			if entry.Path != fixtureDirectory+"/"+secondFixtureFile {
				retained = append(retained, file)
			}
		}
		if len(retained) != len(files)-1 {
			h.t.Fatal("the fixture policy must list the omitted destination exactly once")
		}
		encoded, err := json.Marshal(retained)
		if err != nil {
			h.t.Fatal(err)
		}
		document["files"] = encoded
	default:
		h.t.Fatalf("unknown fixture policy control %q", policyCase)
	}
	changed, err := json.Marshal(document)
	if err != nil {
		h.t.Fatal(err)
	}
	backup := hostStateDirectory + target + hostPolicyBackup
	if h.hostPathState(backup) != absentSnapshotValue {
		h.t.Fatal("inspect and remove the previous acceptance policy backup before running the test")
	}
	h.hostMustRun("cp", "--preserve=all", "--", path, backup)
	restored := false
	restore := func() {
		if restored {
			return
		}
		h.hostWrite(path, original)
		h.hostMustRun("touch", "--reference="+backup, "--", path)
		if h.hostPathState(path) != before {
			h.t.Fatal("the fixture policy bytes or metadata changed after restoration")
		}
		h.hostMustRun("rm", "-f", "--", backup)
		restored = true
	}
	h.t.Cleanup(restore)
	h.hostWrite(path, string(changed))
	return restore
}

func (h *hostArtifactTest) expectAuditRejections(unrelated map[string]string) {
	h.t.Helper()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	unrelatedTarget := transport.HostArtifactTarget(unrelatedFixtureWorkspace)
	request, cleanup := h.stageInstall(target, caseValidV2)
	defer cleanup()
	h.assignAuditStages(request)
	secret := &hostSecretTest{hostArtifactTest: h}
	marker := secret.newMarker()
	before := secret.secretSnapshot(target, fixtureDirectory, hostSecretConsumer)
	unrelatedTasks := h.taskRecords(unrelatedTarget)
	current, err := h.audit.GetHostArtifact(context.Background(), h.node, target)
	if err != nil {
		h.t.Fatal("the audit token GET failed; grant Sys.HostArtifacts.Audit in both the parent user ACL and the audit token ACL")
	}
	h.expectInstalled(current, h.pins[caseValidV1])
	h.expectSnapshotValues(before, secret.secretSnapshot(target, fixtureDirectory, hostSecretConsumer))
	h.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)
	h.expectUnrelatedInvocation()
	if h.taskRecords(unrelatedTarget) != unrelatedTasks {
		h.t.Fatal("audit GET changed the unrelated target task history")
	}
	h.t.Log("audit GET succeeded without changing the target")
	archive := h.download(h.pins[caseValidV2].ReleaseTag, h.pins[caseValidV2].Asset, h.pins[caseValidV2].SHA256)
	checks := []struct {
		label  string
		method string
		suffix string
		call   func() (string, error)
	}{
		{label: "stage POST", method: http.MethodPost, suffix: "/stage", call: func() (string, error) {
			return h.audit.StageHostArtifact(context.Background(), h.node, target, transport.HostArtifactKindArchive, bytes.NewReader(archive), int64(len(archive)))
		}},
		{label: "install POST", method: http.MethodPost, call: func() (string, error) {
			return h.audit.InstallHostArtifact(context.Background(), h.node, target, request)
		}},
		{label: "secret PUT", method: http.MethodPut, suffix: "/secrets/" + hostSecretField, call: func() (string, error) {
			return h.audit.SetHostSecret(context.Background(), h.node, target, hostSecretField, marker, 1)
		}},
	}
	for _, check := range checks {
		identifier, callErr := check.call()
		if identifier != "" {
			if check.label == "stage POST" {
				h.stageCleanup(target, identifier)()
			} else {
				h.stopUnfinishedTask(identifier)
			}
		}
		prefix := fmt.Sprintf("%s: %s /api2/json/nodes/%s/host-artifacts/%s%s returned HTTP %d:",
			transport.Guest{Node: h.node}, check.method, url.PathEscape(h.node), target, check.suffix, http.StatusForbidden)
		if callErr == nil || identifier != "" || !strings.HasPrefix(callErr.Error(), prefix) {
			h.t.Fatalf("audit %s must return actual HTTP 403 without a task or stage identifier", check.label)
		}
		h.expectSnapshotValues(before, secret.secretSnapshot(target, fixtureDirectory, hostSecretConsumer))
		h.expectSnapshot(unrelated, unrelatedTarget, unrelatedFixtureDirectory)
		h.expectUnrelatedInvocation()
		if h.taskRecords(unrelatedTarget) != unrelatedTasks {
			h.t.Fatalf("audit %s changed the unrelated target task history", check.label)
		}
		h.t.Logf("audit %s returned HTTP 403 without creating a task, stage, or target mutation", check.label)
	}
}

// assignAuditStages models stages that remain after their caller loses Modify
// permission. Only this request's two A stage records change their user field.
func (h *hostArtifactTest) assignAuditStages(request transport.HostArtifactInstall) {
	h.t.Helper()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	deployPrincipal, _, found := strings.Cut(h.nodeConfig.APIToken, "=")
	if !found {
		h.t.Fatal("the deploy token must contain its complete token identity and value")
	}
	installed := h.get(target).Installed
	if installed == nil || installed.User != deployPrincipal {
		h.t.Fatal("the installed record must identify the deploy actor with its complete user@realm!tokenid principal")
	}
	for _, stage := range []struct {
		identifier string
		kind       string
	}{
		{identifier: request.ArchiveStage, kind: transport.HostArtifactKindArchive},
		{identifier: request.BundleStage, kind: transport.HostArtifactKindBundle},
	} {
		path := hostStateDirectory + target + "/" + stageDirectory + "/" + stage.identifier
		data := h.hostPathState(path)
		recordPath := path + ".json"
		metadata := strings.TrimSpace(h.hostMustRun("stat", "--format=%u %g %a %i", "--", recordPath))
		if !strings.HasPrefix(metadata, "0 0 600 ") {
			h.t.Fatal("the request stage record must have root owner, root group, and mode 0600")
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal([]byte(h.hostMustRun("cat", "--", recordPath)), &record); err != nil {
			h.t.Fatalf("parse the request stage record: %v", err)
		}
		var user string
		var kind string
		if err := json.Unmarshal(record["user"], &user); err != nil {
			h.t.Fatalf("parse the request stage user: %v", err)
		}
		if err := json.Unmarshal(record["kind"], &kind); err != nil {
			h.t.Fatalf("parse the request stage kind: %v", err)
		}
		if user != deployPrincipal || kind != stage.kind || record["created"] == nil {
			h.t.Fatal("the request stage record must identify its deploy actor, expected kind, and creation time")
		}
		encodedUser, err := json.Marshal(h.auditPrincipal)
		if err != nil {
			h.t.Fatal(err)
		}
		record["user"] = encodedUser
		encoded, err := json.Marshal(record)
		if err != nil {
			h.t.Fatal(err)
		}
		h.hostWrite(recordPath, string(encoded))
		var stored map[string]json.RawMessage
		if err := json.Unmarshal([]byte(h.hostMustRun("cat", "--", recordPath)), &stored); err != nil {
			h.t.Fatalf("parse the updated request stage record: %v", err)
		}
		if !maps.EqualFunc(record, stored, func(left json.RawMessage, right json.RawMessage) bool {
			return bytes.Equal(left, right)
		}) {
			h.t.Fatal("the request stage record changed fields other than its user")
		}
		if h.hostPathState(path) != data || strings.TrimSpace(h.hostMustRun("stat", "--format=%u %g %a %i", "--", recordPath)) != metadata {
			h.t.Fatal("preparing the audit actor changed stage data or stage record ownership, mode, or inode")
		}
	}
}
