package acceptance_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	importDownloadName    = "conf"
	importDownloadAddress = "pveguest_download." + importDownloadName
	importObjectPath      = "/import.conf"
	importArchivePath     = "/stack.tar.gz"
	importArchiveMember   = "import.conf"
	importOtherMember     = "debs/manifest.txt"
	importUnusedURL       = "https://127.0.0.1/unused"
	importMissingPath     = testDirectory + "/missing-import.conf"

	importWriteIDHexLength = 16
	importedWriteIDLiteral = "import-"

	downloadImportIDFormat    = `<node>/<vmid>/<kind>/<absolute\s+path>`
	downloadImportPathMessage = `must\s+be\s+an\s+absolute\s+path\s+that\s+ends\s+in\s+a\s+file\s+name`
	relativeImportID          = "node/101/lxc/relative.conf"
)

// downloadImportCase describes one import and the apply that follows it.
type downloadImportCase struct {
	config       string
	wantFileHash string
	wantWriteID  *regexp.Regexp
	wantRewrite  bool
}

// importFileIdentity returns the inode and modification time of the import test file.
func (g *testGuest) importFileIdentity() string {
	g.t.Helper()
	result := g.runReadOnly("stat", "--format=%i %y", "--", testImportConfPath)
	identity := strings.TrimSpace(string(result.Stdout))
	if result.ExitCode != 0 || identity == "" {
		g.t.Fatalf("reading the configuration file identity returned %q with exit status %d and stderr %s", identity, result.ExitCode, result.Stderr)
	}
	return identity
}

// importUnitInvocationID returns the running test unit's invocation ID, which systemd
// changes at each start.
func (g *testGuest) importUnitInvocationID() string {
	g.t.Helper()
	result := g.runReadOnly("systemctl", "show", "--property=InvocationID", "--value", "--", testImportService)
	invocationID := strings.TrimSpace(string(result.Stdout))
	if result.ExitCode != 0 || invocationID == "" {
		g.t.Fatalf("reading the invocation ID for %s returned %q with exit status %d and stderr %s", testImportService, invocationID, result.ExitCode, result.Stderr)
	}
	return invocationID
}

func (g *testGuest) expectImportUnitInvocation(want *string, same bool) resource.TestCheckFunc {
	return checkGuest(func() error {
		got := g.importUnitInvocationID()
		if same && got != *want {
			return fmt.Errorf("unit %s has invocation ID %s after apply instead of the previous ID %s", testImportService, got, *want)
		}
		if !same && got == *want {
			return fmt.Errorf("unit %s has invocation ID %s after apply but should have a different ID after restart", testImportService, got)
		}
		*want = got
		return nil
	})
}

func (g *testGuest) expectImportFileIdentity(want string, same bool) resource.TestCheckFunc {
	return checkGuest(func() error {
		got := g.importFileIdentity()
		if same && got != want {
			return fmt.Errorf("file %s has identity %s after apply instead of the previous identity %s", testImportConfPath, got, want)
		}
		if !same && got == want {
			return fmt.Errorf("file %s has identity %s after apply but should have a different identity after the rewrite", testImportConfPath, got)
		}
		return nil
	})
}

// expectImportedDownload checks the state that import writes: the write_id in
// the import form, and no url and no sha256.
func expectImportedDownload(wantWriteID string) resource.ImportStateCheckFunc {
	return func(states []*terraform.InstanceState) error {
		var downloadStates []*terraform.InstanceState
		for _, state := range states {
			if state.Attributes["path"] == testImportConfPath {
				downloadStates = append(downloadStates, state)
			}
		}
		if len(downloadStates) != 1 {
			return fmt.Errorf("import returned %d states for path %s instead of 1", len(downloadStates), testImportConfPath)
		}
		attributes := downloadStates[0].Attributes
		if attributes["write_id"] != wantWriteID {
			return fmt.Errorf("imported %s is %q instead of %q", "write_id", attributes["write_id"], wantWriteID)
		}
		for _, name := range []string{"url", "sha256"} {
			if attributes[name] != "" {
				return fmt.Errorf("imported %s has value %q but should not exist in state", name, attributes[name])
			}
		}
		return nil
	}
}

// importDownloadConfig returns a download of the import test file and a unit
// that restarts on the write_id of that download.
func (g *testGuest) importDownloadConfig(url string, hash string, extra string) string {
	return g.providerBlock() +
		g.controllerDownloadBlock(importDownloadName, url, hash, testImportConfPath, extra) +
		g.importUnitResource(importDownloadAddress+".write_id")
}

// TestAccDownloadImport checks that importing a matching file does not rewrite it or
// restart the dependent unit.
func TestAccDownloadImport(t *testing.T) {
	t.Run("matching download avoids file rewrite and unit restart", testDownloadImportMatching)
	t.Run("differing download rewrites the file and restarts the unit", testDownloadImportDiffering)
	t.Run("matching archive member avoids file rewrite and unit restart", testDownloadImportArchiveMember)
	t.Run("missing file error includes the path", testDownloadImportMissingFile)
	t.Run("malformed import error includes the required format", testDownloadImportMalformedID)
}

func testDownloadImportMatching(t *testing.T) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	content := []byte(adoptedConfContent)
	baseURL := guest.serveObjects(t, map[string][]byte{importObjectPath: content})
	runDownloadImport(t, guest, downloadImportCase{
		config:       guest.importDownloadConfig(baseURL+importObjectPath, bytesHash(content), ""),
		wantFileHash: bytesHash(content),
		wantWriteID:  regexp.MustCompile("^" + importedWriteID(content) + "$"),
		wantRewrite:  false,
	})
}

func testDownloadImportDiffering(t *testing.T) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	content := []byte(changedConfContent)
	baseURL := guest.serveObjects(t, map[string][]byte{importObjectPath: content})
	runDownloadImport(t, guest, downloadImportCase{
		config:       guest.importDownloadConfig(baseURL+importObjectPath, bytesHash(content), ""),
		wantFileHash: bytesHash(content),
		wantWriteID:  regexp.MustCompile(fmt.Sprintf("^[0-9a-f]{%d}$", importWriteIDHexLength)),
		wantRewrite:  true,
	})
}

func testDownloadImportArchiveMember(t *testing.T) {
	guest := newTestGuest(t)
	guest.useImportUnit()
	member := []byte(adoptedConfContent)
	archive := buildArchive(t, map[string][]byte{
		importOtherMember:   []byte(changedConfContent),
		importArchiveMember: member,
	})
	baseURL := guest.serveObjects(t, map[string][]byte{importArchivePath: archive})
	extra := fmt.Sprintf("archive_member = %q", importArchiveMember)
	runDownloadImport(t, guest, downloadImportCase{
		config:       guest.importDownloadConfig(baseURL+importArchivePath, bytesHash(archive), extra),
		wantFileHash: bytesHash(member),
		wantWriteID:  regexp.MustCompile("^" + importedWriteID(member) + "$"),
		wantRewrite:  false,
	})
}

// importedWriteID returns the write_id that import sets for a guest file with
// the given content.
func importedWriteID(content []byte) string {
	return importedWriteIDLiteral + bytesHash(content)[:importWriteIDHexLength]
}

// runDownloadImport imports the download and unit, applies configuration, and checks
// file identity, unit invocation ID, and a clean repeat plan.
func runDownloadImport(t *testing.T, guest *testGuest, testCase downloadImportCase) {
	t.Helper()
	invocationID := guest.importUnitInvocationID()
	identity := guest.importFileIdentity()
	wantImportedWriteID := importedWriteID([]byte(adoptedConfContent))

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy:             guest.expectImportUnit(),
		Steps: []resource.TestStep{
			{
				Config:             testCase.config,
				ResourceName:       importDownloadAddress,
				ImportState:        true,
				ImportStateId:      guest.importFileID(),
				ImportStatePersist: true,
				ImportStateCheck:   expectImportedDownload(wantImportedWriteID),
			},
			{
				Config:             testCase.config,
				ResourceName:       importUnitAddress,
				ImportState:        true,
				ImportStateId:      guest.importUnitID(),
				ImportStatePersist: true,
				ImportStateCheck:   expectImportedAttributes,
			},
			{
				Config:           testCase.config,
				ConfigPlanChecks: expectActions(importDownloadAddress, importUnitAddress),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(importDownloadAddress, "file_sha256", testCase.wantFileHash),
					resource.TestMatchResourceAttr(importDownloadAddress, "write_id", testCase.wantWriteID),
					checkGuest(guest.expectFileHash(testImportConfPath, testCase.wantFileHash)),
					guest.expectImportFileIdentity(identity, !testCase.wantRewrite),
					guest.expectImportUnitInvocation(&invocationID, !testCase.wantRewrite),
				),
			},
			{
				Config:   testCase.config,
				PlanOnly: true,
			},
		},
	})
}

func testDownloadImportMissingFile(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	config := guest.providerBlock() +
		guest.controllerDownloadBlock(importDownloadName, importUnusedURL, bytesHash([]byte("unused")), importMissingPath, "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:        config,
				ResourceName:  importDownloadAddress,
				ImportState:   true,
				ImportStateId: fmt.Sprintf("%s/%d/%s/%s", guest.guest.Node, guest.guest.VMID, guest.guest.Kind, importMissingPath),
				ExpectError:   regexp.MustCompile(regexp.QuoteMeta(importMissingPath)),
			},
		},
	})
}

func testDownloadImportMalformedID(t *testing.T) {
	guest := newTestGuest(t)
	config := guest.providerBlock() +
		guest.controllerDownloadBlock(importDownloadName, importUnusedURL, bytesHash([]byte("unused")), importMissingPath, "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:        config,
				ResourceName:  importDownloadAddress,
				ImportState:   true,
				ImportStateId: malformedImportID,
				ExpectError:   regexp.MustCompile(downloadImportIDFormat),
			},
			{
				Config:        config,
				ResourceName:  importDownloadAddress,
				ImportState:   true,
				ImportStateId: relativeImportID,
				ExpectError:   regexp.MustCompile(downloadImportPathMessage),
			},
		},
	})
}
