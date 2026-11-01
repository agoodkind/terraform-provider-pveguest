package acceptance_test

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net/url"
	pathutil "path"
	"slices"
	"strings"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

var programFixtureCases = []string{
	caseProgramValid, caseProgramWrongSource, caseProgramWrongSigner,
	caseProgramWrongArchive, caseProgramWrongMember, caseProgramCollision,
}

// TestAccHostArtifactPrograms checks program installation, drift, rejection,
// and interrupted recovery through the existing fixture targets and OpenTofu.
func TestAccHostArtifactPrograms(t *testing.T) {
	test, unrelated := prepareHostArtifactFixtures(t, programFixtureCases...)
	test.requireProgramPins()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	unrelatedTarget := transport.HostArtifactTarget(unrelatedFixtureWorkspace)
	unrelatedTasks := test.taskRecords(unrelatedTarget)
	expectUnrelated := func() {
		test.expectSnapshotValues(unrelated, test.snapshot(unrelatedTarget, unrelatedFixtureDirectory))
		test.expectUnrelatedInvocation()
		if test.taskRecords(unrelatedTarget) != unrelatedTasks {
			t.Fatal("a program operation changed the unrelated target task history")
		}
	}

	test.installDirectly(fixtureWorkspace, caseValidV1)
	if len(test.get(target).Installed.Programs) != 0 {
		t.Fatal("the schema 1 fixture reports installed programs")
	}
	expectUnrelated()
	programs := test.manifestPrograms(test.pins[caseProgramValid])
	if len(programs) == 0 {
		t.Fatal("the qualified program-valid manifest must list at least one program")
	}
	test.workspace.writeConfig(test.config(caseProgramValid))
	test.workspace.mustRun("init", "-no-color")
	beforeImport := test.snapshot(target, fixtureDirectory)
	priorTasks := test.taskRecords(target)
	test.workspace.mustRun("import", "-no-color", hostArtifactAddress, test.node+":"+fixtureWorkspace)
	test.expectSnapshotValues(beforeImport, test.snapshot(target, fixtureDirectory))
	if test.taskRecords(target) != priorTasks {
		t.Fatal("program import changed the selected target task history")
	}
	expectUnrelated()
	test.expectInstalledPrograms(test.applyClean(caseProgramValid), programs)
	test.expectProgramRepeatPlan(programs)
	expectUnrelated()

	for _, programPath := range test.programPaths(caseProgramValid) {
		test.hostWrite(programPath, editedFixtureContent)
		test.expectInstalledPrograms(test.expectRepair(caseProgramValid), programs)
		expectUnrelated()

		test.hostMustRun("rm", "-f", "--", programPath)
		test.expectInstalledPrograms(test.expectRepair(caseProgramValid), programs)
		expectUnrelated()
	}

	collision := "path " + fixtureDirectory + "/" + firstFixtureFile +
		" appears more than once among manifest directories, files, and programs"
	test.expectProgramProviderRejection(caseProgramCollision, "Read host artifact manifest", collision, 0, true)
	test.expectProgramRepeatPlan(programs)
	expectUnrelated()

	wrongArchive := test.manifestPrograms(test.pins[caseProgramWrongArchive])
	if len(programs) != 1 || len(wrongArchive) != 1 {
		t.Fatal("the qualified archive rejection requires exactly one original program")
	}
	original := programs[0]
	rejected := wrongArchive[0]
	if rejected.Name != original.Name || rejected.Repository != original.Repository ||
		rejected.ReleaseTag != original.ReleaseTag || rejected.Asset != original.Asset || rejected.SHA256 == original.SHA256 {
		t.Fatal("program-wrong-archive must retain the original program release and change its archive checksum")
	}
	archiveURL := releaseBaseURL + rejected.Repository + "/releases/download/" +
		url.PathEscape(rejected.ReleaseTag) + "/" + url.PathEscape(rejected.Asset)
	checksumFailure := fmt.Sprintf("archive download failed for program %s: the download of %s has sha256 %s, but the configuration expects %s",
		rejected.Name, archiveURL, original.SHA256, rejected.SHA256)
	test.expectProgramProviderRejection(caseProgramWrongArchive, "Download host artifact program archive", checksumFailure, 0, false)
	test.expectProgramRepeatPlan(programs)
	expectUnrelated()

	for _, defect := range []hostArtifactDefect{
		{fixtureCase: caseProgramWrongSource, failureText: "gh attestation verify exited with code 1:"},
		{fixtureCase: caseProgramWrongSigner, failureText: "gh attestation verify exited with code 1:"},
		{fixtureCase: caseProgramWrongMember, failureText: "the member '" + original.Member + "' of the program " + original.Name + " has SHA-256 " + original.MemberSHA256 + ", and the manifest lists "},
	} {
		priorTasks := strings.Fields(test.taskRecords(target))
		test.expectRejectedInstall(defect.fixtureCase, defect.failureText, unrelated, func() (transport.HostArtifactInstall, func()) {
			return test.stageInstall(target, defect.fixtureCase)
		})
		for _, record := range strings.Fields(test.taskRecords(target)) {
			if slices.Contains(priorTasks, record) {
				continue
			}
			log := test.hostMustRun("cat", "--", record)
			pin := test.pins[defect.fixtureCase]
			if !strings.Contains(log, "gh attestation verify accepted "+pin.Asset+" with SHA-256 "+pin.SHA256) {
				t.Fatalf("%s did not verify its configuration archive before rejecting the program", defect.fixtureCase)
			}
			if defect.fixtureCase == caseProgramWrongMember &&
				!strings.Contains(log, "gh attestation verify accepted "+original.Asset+" with SHA-256 "+original.SHA256) {
				t.Fatal("the member rejection did not verify the complete original program archive")
			}
		}
		test.expectProgramProviderRejection(defect.fixtureCase, "Install host artifact", defect.failureText, 1, false)
		test.expectProgramRepeatPlan(programs)
		expectUnrelated()
	}

	test.workspace.writeConfig(test.config(caseValidV1))
	test.applyClean(caseValidV1)
	expectUnrelated()
	test.stopInstall(target, caseValidV1, caseProgramValid)
	expectUnrelated()
	test.workspace.writeConfig(test.config(caseProgramValid))
	test.expectInstalledPrograms(test.expectRepair(caseProgramValid), programs)
	test.expectProgramRepeatPlan(programs)
	expectUnrelated()
}

func (h *hostArtifactTest) requireProgramPins() {
	h.t.Helper()
	for _, fixtureCase := range programFixtureCases {
		pin := h.pins[fixtureCase]
		programs := h.manifestPrograms(pin)
		if len(programs) == 0 || len(pin.ProgramBundles) != len(programs) {
			h.t.Fatalf("%s must pin exactly one bundle for every manifest program", fixtureCase)
		}
		for _, program := range programs {
			bundle, found := pin.ProgramBundles[program.Name]
			if !found || bundle.Asset == "" || len(bundle.SHA256) != sha256.Size*2 ||
				strings.Trim(bundle.SHA256, "0123456789abcdef") != "" {
				h.t.Fatalf("%s must pin a bundle asset and complete lowercase hexadecimal SHA-256 for %s", fixtureCase, program.Name)
			}
		}
	}
}

func (h *hostArtifactTest) expectInstalledPrograms(current transport.HostArtifactState, programs []fixtureProgram) {
	h.t.Helper()
	if current.Installed == nil || len(current.Installed.Programs) != len(programs) {
		h.t.Fatal("the installed record must list exactly the qualified manifest programs")
	}
	want := make(map[string]transport.HostArtifactInstalledProgram, len(programs))
	for _, program := range programs {
		want[program.Name] = transport.HostArtifactInstalledProgram{
			Name:           program.Name,
			Repository:     program.Repository,
			ReleaseTag:     program.ReleaseTag,
			Asset:          program.Asset,
			SHA256:         program.SHA256,
			SourceRevision: program.SourceRevision,
			SignerRevision: program.SignerRevision,
		}
	}
	got := make(map[string]transport.HostArtifactInstalledProgram, len(current.Installed.Programs))
	for _, program := range current.Installed.Programs {
		if _, duplicate := got[program.Name]; duplicate {
			h.t.Fatalf("the installed record repeats program %s", program.Name)
		}
		got[program.Name] = program
	}
	if !maps.Equal(got, want) {
		h.t.Fatalf("installed program identities are %+v, want qualified manifest identities %+v", got, want)
	}
}

func (h *hostArtifactTest) expectProgramRepeatPlan(programs []fixtureProgram) {
	h.t.Helper()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	h.workspace.writeConfig(h.config(caseProgramValid))
	before := h.snapshot(target, fixtureDirectory)
	priorTasks := h.taskRecords(target)
	h.workspace.mustRun("plan", "-no-color", "-detailed-exitcode")
	h.expectSnapshotValues(before, h.snapshot(target, fixtureDirectory))
	h.expectInstalledPrograms(h.get(target), programs)
	if h.taskRecords(target) != priorTasks {
		h.t.Fatal("the clean program repeat plan changed the selected target task history")
	}
}

// expectProgramProviderRejection checks the provider diagnostic and installed
// observations. expectRejectedInstall checks the host task for each host defect.
func (h *hostArtifactTest) expectProgramProviderRejection(fixtureCase string, summary string, failureText string, newTasks int, rejectPlan bool) {
	h.t.Helper()
	target := transport.HostArtifactTarget(fixtureWorkspace)
	unrelatedTarget := transport.HostArtifactTarget(unrelatedFixtureWorkspace)
	selectedSnapshot := h.rejectionSnapshot
	if newTasks == 0 {
		selectedSnapshot = h.snapshot
	}
	before := selectedSnapshot(target, fixtureDirectory)
	unrelated := h.snapshot(unrelatedTarget, unrelatedFixtureDirectory)
	priorTasks := strings.Fields(h.taskRecords(target))
	unrelatedTasks := h.taskRecords(unrelatedTarget)
	stages := h.stageSnapshot(target)
	h.workspace.writeConfig(h.config(fixtureCase))
	if rejectPlan {
		output, exitCode := h.workspace.run("plan", "-no-color", "-detailed-exitcode")
		if exitCode == 0 || exitCode == planChangesExitCode ||
			!strings.Contains(collapseWhitespace(output), "Error: "+summary) || !strings.Contains(collapseWhitespace(output), failureText) {
			h.t.Fatalf("plan for %s exited with status %d; require %q and %q:\n%s", fixtureCase, exitCode, summary, failureText, output)
		}
		h.expectSnapshotValues(before, selectedSnapshot(target, fixtureDirectory))
		h.expectSnapshotValues(stages, h.stageSnapshot(target))
		if !slices.Equal(priorTasks, strings.Fields(h.taskRecords(target))) {
			h.t.Fatalf("the rejected plan for %s changed the selected target task history", fixtureCase)
		}
		h.expectSnapshotValues(unrelated, h.snapshot(unrelatedTarget, unrelatedFixtureDirectory))
		h.expectUnrelatedInvocation()
		if h.taskRecords(unrelatedTarget) != unrelatedTasks {
			h.t.Fatalf("the rejected plan for %s changed the unrelated target task history", fixtureCase)
		}
	}
	output, exitCode := h.workspace.run("apply", "-no-color", "-auto-approve")
	afterStages := h.stageSnapshot(target)
	var cleanups []func()
	for path := range afterStages {
		if _, existed := stages[path]; existed {
			continue
		}
		identifier := pathutil.Base(path)
		if len(identifier) == 32 && strings.Trim(identifier, "0123456789abcdef") == "" {
			cleanup := h.stageCleanup(target, identifier)
			defer cleanup()
			cleanups = append(cleanups, cleanup)
		}
	}
	if newTasks == 0 {
		h.expectSnapshotValues(stages, afterStages)
	}
	for _, cleanup := range cleanups {
		cleanup()
	}
	if exitCode == 0 || !strings.Contains(collapseWhitespace(output), "Error: "+summary) ||
		!strings.Contains(collapseWhitespace(output), failureText) {
		h.t.Fatalf("apply for %s exited with status %d; require %q and %q:\n%s", fixtureCase, exitCode, summary, failureText, output)
	}
	h.expectSnapshotValues(before, selectedSnapshot(target, fixtureDirectory))
	h.expectSnapshotValues(stages, h.stageSnapshot(target))
	afterTasks := strings.Fields(h.taskRecords(target))
	if len(afterTasks) != len(priorTasks)+newTasks {
		h.t.Fatalf("apply for %s changed the selected target task count by %d, want %d", fixtureCase, len(afterTasks)-len(priorTasks), newTasks)
	}
	for _, record := range priorTasks {
		if !slices.Contains(afterTasks, record) {
			h.t.Fatalf("apply for %s removed an earlier selected-target task record", fixtureCase)
		}
	}
	h.expectSnapshotValues(unrelated, h.snapshot(unrelatedTarget, unrelatedFixtureDirectory))
	h.expectUnrelatedInvocation()
	if h.taskRecords(unrelatedTarget) != unrelatedTasks {
		h.t.Fatalf("apply for %s changed the unrelated target task history", fixtureCase)
	}
}
