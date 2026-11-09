package acceptance_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	// The test requires fixture archives with different content for the same
	// unit file and a policy that lists the file.
	caseUnitV1 = "unit-v1"
	caseUnitV2 = "unit-v2"

	hostUnitFixture     = "hostartifact-acceptance-unit.service"
	hostUnitFixturePath = "/etc/systemd/system/" + hostUnitFixture
	hostUnitAddress     = "pveguest_host_systemd_unit.test"
	hostUnitUnlisted    = "hostartifact-acceptance-unlisted.service"
	hostUnitUnlistedErr = "does not list the unit file"
	hostUnitRestartOn   = "\n  restart_on = { unit = pveguest_host_artifact.test.write_ids[\"" + hostUnitFixturePath + "\"] }"

	// PVE::Tools::lock_file writes this text to the task log when its first
	// attempt to acquire the target lock fails.
	hostLockWaitText = "trying to acquire lock"
	hostUnitTaskType = "hostunit"
)

type hostUnitTest struct {
	*hostSecretTest

	unrelated map[string]string
}

func TestAccHostSystemdUnit(t *testing.T) {
	artifact, unrelated := prepareSelectedFixtures(t, caseUnitV1, caseUnitV2)
	secret := &hostSecretTest{
		hostArtifactTest: artifact,
		target:           transport.HostArtifactTarget(fixtureWorkspace),
	}
	test := &hostUnitTest{hostSecretTest: secret, unrelated: unrelated}
	t.Cleanup(test.removeFixtureUnit)
	test.removeFixtureUnit()

	test.workspace.writeConfig(test.unitConfig(caseUnitV1, hostUnitFixture, hostUnitRestartOn))
	test.workspace.mustRun("init", "-no-color")
	test.applyUnit()
	test.expectUnitState("enabled", "active")
	first := test.invocationID(hostUnitFixture)
	test.expectUnchangedRepeat(first)

	test.hostMustRun("systemctl", "stop", "--", hostUnitFixture)
	test.expectPlannedChange()
	test.applyUnit()
	test.expectUnitState("enabled", "active")
	started := test.expectNewInvocation(first)

	test.hostMustRun("systemctl", "disable", "--", hostUnitFixture)
	test.expectPlannedChange()
	test.applyUnit()
	test.expectUnitState("enabled", "active")
	test.expectInvocation(started)

	test.workspace.writeConfig(test.unitConfig(caseUnitV2, hostUnitFixture, hostUnitRestartOn))
	test.applyUnit()
	restarted := test.expectNewInvocation(started)
	test.expectUnchangedRepeat(restarted)

	test.workspace.mustRun("state", "rm", hostUnitAddress)
	test.workspace.mustRun(
		"import",
		"-no-color",
		hostUnitAddress,
		test.node+":"+fixtureWorkspace+":"+hostUnitFixture,
	)
	test.expectInvocation(restarted)
	test.expectUnrelated()
	test.workspace.writeConfig(test.unitConfig(caseUnitV2, hostUnitFixture, ""))
	test.expectUnchangedRepeat(restarted)

	test.workspace.writeConfig(test.unitConfig(caseUnitV2, hostUnitUnlisted, ""))
	output, exitCode := test.workspace.run("apply", "-no-color", "-auto-approve")
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), hostUnitUnlistedErr) {
		t.Fatalf(
			"apply of an unlisted unit exited with status %d, want a failure that contains %q:\n%s",
			exitCode,
			hostUnitUnlistedErr,
			output,
		)
	}

	test.expectInvocation(restarted)
	test.expectUnrelated()

	test.workspace.writeConfig(test.config(caseUnitV2))
	test.applyUnit()
	test.expectUnitState("enabled", "active")
	test.expectInvocation(restarted)

	test.expectSerializedMutations(restarted)
	test.workspace.mustRun("state", "rm", hostArtifactAddress)
}

func (u *hostUnitTest) expectSerializedMutations(invocation string) {
	u.t.Helper()
	marker := u.newMarker()
	u.workspace.variableValue = marker
	u.workspace.writeConfig(
		u.unitConfig(caseUnitV1, hostUnitFixture, hostUnitRestartOn) + fmt.Sprintf(
			`
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
  value_wo_version = 1

  depends_on = [pveguest_host_artifact.test]
}
`,
			u.node,
			fixtureWorkspace,
			hostSecretField,
			hostSecretValueArgument,
		),
	)
	before := strings.Fields(u.taskRecords(u.target))
	u.mustTofu("apply", "-no-color", "-auto-approve")
	u.expectUnrelated()
	u.expectUnitState("enabled", "active")
	u.expectNewInvocation(invocation)
	entry := u.secretEntry()
	if !entry.Configured || entry.Version != 1 || entry.RestartPending {
		u.t.Fatalf(
			"the GET secrets[] entry reports configured as %t, version as %d, and restart_pending as %t instead of configured true, version 1, and restart_pending false",
			entry.Configured,
			entry.Version,
			entry.RestartPending,
		)
	}

	u.expectFileContent(marker)

	taskTypes := map[string]bool{}

	for _, record := range strings.Fields(u.taskRecords(u.target)) {
		if slices.Contains(before, record) {
			continue
		}

		for _, taskType := range []string{hostSecretTaskType, hostUnitTaskType} {
			if strings.Contains(record, ":"+taskType+":") {
				taskTypes[taskType] = true
			}
		}

		if strings.Contains(u.hostMustRun("cat", "--", record), hostLockWaitText) {
			u.t.Fatalf("task log %s shows that the task waited for the target lock", record)
		}
	}

	if !taskTypes[hostSecretTaskType] || !taskTypes[hostUnitTaskType] {
		u.t.Fatalf(
			"the apply created task types %v on the fixture target, want %s and %s",
			taskTypes,
			hostSecretTaskType,
			hostUnitTaskType,
		)
	}

	u.mustTofu("state", "rm", hostSecretAddress)
}

func (u *hostUnitTest) unitConfig(fixtureCase string, unit string, extra string) string {
	return u.config(fixtureCase) + fmt.Sprintf(
		`
resource "pveguest_host_systemd_unit" "test" {
  node      = %q
  workspace = %q
  name      = %q
  enabled   = true
  active    = true%s
}
`,
		u.node,
		fixtureWorkspace,
		unit,
		extra,
	)
}

func (u *hostUnitTest) applyUnit() {
	u.t.Helper()
	u.workspace.mustRun("apply", "-no-color", "-auto-approve")
	u.expectUnrelated()
}

func (u *hostUnitTest) expectUnchangedRepeat(invocation string) {
	u.t.Helper()
	output, exitCode := u.workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != 0 {
		u.t.Fatalf("plan exited with status %d, want 0:\n%s", exitCode, output)
	}

	u.applyUnit()
	u.expectInvocation(invocation)
}

func (u *hostUnitTest) expectPlannedChange() {
	u.t.Helper()
	output, exitCode := u.workspace.run("plan", "-no-color", "-detailed-exitcode")
	if exitCode != planChangesExitCode {
		u.t.Fatalf(
			"plan exited with status %d, want %d:\n%s",
			exitCode,
			planChangesExitCode,
			output,
		)
	}
}

func (u *hostUnitTest) expectUnitState(fileState string, activeState string) {
	u.t.Helper()
	enabled := strings.TrimSpace(u.hostStdout("systemctl", "is-enabled", "--", hostUnitFixture))
	active := strings.TrimSpace(u.hostStdout("systemctl", "is-active", "--", hostUnitFixture))
	if enabled != fileState || active != activeState {
		u.t.Fatalf(
			"%s is %s and %s, want %s and %s",
			hostUnitFixture,
			enabled,
			active,
			fileState,
			activeState,
		)
	}
}

// hostStdout returns the standard output of a command that can exit with a
// nonzero status, such as systemctl is-enabled.
func (u *hostUnitTest) hostStdout(argv ...string) string {
	u.t.Helper()
	stdout, _, _ := u.hostRun(nil, argv...)

	return stdout
}

func (u *hostUnitTest) expectInvocation(want string) {
	u.t.Helper()
	if got := u.invocationID(hostUnitFixture); got != want {
		u.t.Fatalf("%s has InvocationID %s, want %s", hostUnitFixture, got, want)
	}
}

func (u *hostUnitTest) expectNewInvocation(before string) string {
	u.t.Helper()
	after := u.invocationID(hostUnitFixture)
	if after == before {
		u.t.Fatalf("%s kept InvocationID %s", hostUnitFixture, before)
	}

	return after
}

func (u *hostUnitTest) expectUnrelated() {
	u.t.Helper()
	u.expectSnapshot(
		u.unrelated,
		transport.HostArtifactTarget(unrelatedFixtureWorkspace),
		unrelatedFixtureDirectory,
	)
	u.expectUnrelatedInvocation()
}

// removeFixtureUnit stops and disables the fixture unit and deletes its unit
// file. systemctl exits with a nonzero status when the unit does not exist.
func (u *hostUnitTest) removeFixtureUnit() {
	u.t.Helper()
	u.hostRun(nil, "systemctl", "disable", "--now", "--", hostUnitFixture)
	u.hostMustRun("rm", "-f", "--", hostUnitFixturePath)
	u.hostMustRun("systemctl", "daemon-reload")
}
