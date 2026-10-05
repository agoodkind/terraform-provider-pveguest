package acceptance_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const (
	// 30 MiB, the size of the MWAN binary that the transfer test sends.
	transferPayloadBytes = 30 * 1024 * 1024

	debPackageName   = "pveguest-acc-test"
	debPackageSource = "Package: pveguest-acc-test\nVersion: 1.0\nSection: misc\nPriority: optional\n" +
		"Architecture: all\nMaintainer: pveguest <pveguest@example.invalid>\n" +
		"Description: pveguest acceptance test package\n"
)

func bytesHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// serveObjects starts an HTTPS server on the loopback interface with a
// self-signed certificate. The provider trusts that certificate through the
// controller_ca_file argument, which the test writes into the provider block.
func (g *testGuest) serveObjects(t *testing.T, objects map[string][]byte) string {
	t.Helper()
	mux := http.NewServeMux()
	for name, content := range objects {
		mux.HandleFunc(name, func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write(content)
		})
	}
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), "controller-ca.pem")
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	g.controllerCAFile = caFile
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return server.URL
}

func (g *testGuest) controllerDownloadBlock(name string, url string, hash string, path string, extra string) string {
	return fmt.Sprintf(`
resource "pveguest_download" %q {
  node   = local.node
  vmid   = local.vmid
  kind   = local.kind
  fetch  = "controller"
  url    = %q
  sha256 = %q
  path   = %q
  %s
}
`, name, url, hash, path, extra)
}

func buildArchive(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressor)
	for name, content := range members {
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestAccDownloadControllerTransfer records the time of a 30 MiB transfer
// through the exec API. The test logs the elapsed time of the apply step.
func TestAccDownloadControllerTransfer(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload := make([]byte, transferPayloadBytes)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	hash := bytesHash(payload)
	baseURL := guest.serveObjects(t, map[string][]byte{"/large.bin": payload})
	path := testDirectory + "/controller/large.bin"
	config := guest.providerBlock() + guest.controllerDownloadBlock("test", baseURL+"/large.bin", hash, path, "")

	var started time.Time
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() { started = time.Now() },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "file_sha256", hash),
					checkGuest(func() error {
						t.Logf("apply of a %d byte controller transfer took %s", len(payload), time.Since(started))
						return guest.expectFileHash(path, hash)()
					}),
				),
			},
		},
	})
}

func TestAccDownloadArchiveMember(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	member := []byte("member content\n")
	archive := buildArchive(t, map[string][]byte{
		"debs/manifest.txt": []byte("another member\n"),
		"mwan":              member,
	})
	baseURL := guest.serveObjects(t, map[string][]byte{"/stack.tar.gz": archive})
	path := testDirectory + "/archive/mwan"
	archiveURL := baseURL + "/stack.tar.gz"
	config := guest.providerBlock() +
		guest.controllerDownloadBlock("test", archiveURL, bytesHash(archive), path, `archive_member = "mwan"`)
	unsafeConfig := guest.providerBlock() +
		guest.controllerDownloadBlock("test", archiveURL, bytesHash(archive), path, `archive_member = "../mwan"`)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "sha256", bytesHash(archive)),
					resource.TestCheckResourceAttr("pveguest_download.test", "file_sha256", bytesHash(member)),
					checkGuest(guest.expectFileContent(path, string(member))),
				),
			},
			{
				// A hand edit produces a planned rewrite.
				PreConfig:        func() { guest.writeFile(path, "edited by hand\n") },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_download.test", plancheck.ResourceActionUpdate),
				Check:            checkGuest(guest.expectFileContent(path, string(member))),
			},
			{
				Config:      unsafeConfig,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)must\s+not\s+contain\s+a\s+\.\.\s+component`),
			},
		},
	})
}

func TestAccDebPackages(t *testing.T) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("dpkg-deb is missing on the controller, and the test builds its .deb file with dpkg-deb")
	}
	guest := newTestGuest(t)
	guest.useTestDirectory()
	t.Cleanup(func() { guest.run(nil, "apt-get", "purge", "-y", "--", debPackageName) })

	debContent := buildDeb(t)
	baseURL := guest.serveObjects(t, map[string][]byte{"/test.deb": debContent})
	debPath := testDirectory + "/debs/" + debPackageName + ".deb"
	config := guest.providerBlock() +
		guest.controllerDownloadBlock("deb", baseURL+"/test.deb", bytesHash(debContent), debPath, "") +
		fmt.Sprintf(`
resource "pveguest_deb_packages" "test" {
  node     = local.node
  vmid     = local.vmid
  kind     = local.kind
  packages = {
    %s = pveguest_download.deb.path
  }
}
`, debPackageName)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_deb_packages.test", "deb_versions."+debPackageName, "1.0"),
					checkGuest(func() error {
						if !guest.packageInstalled(debPackageName) {
							return fmt.Errorf("package %s is not installed", debPackageName)
						}
						return nil
					}),
				),
			},
			{
				PreConfig:        func() { guest.mustRun("apt-get", "remove", "-y", "--", debPackageName) },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_deb_packages.test", plancheck.ResourceActionUpdate),
				Check: checkGuest(func() error {
					if !guest.packageInstalled(debPackageName) {
						return fmt.Errorf("package %s is not installed after the update", debPackageName)
					}
					return nil
				}),
			},
		},
	})
}

// buildDeb runs dpkg-deb on the controller to build a minimal package.
func buildDeb(t *testing.T) []byte {
	t.Helper()
	root := filepath.Join(t.TempDir(), "package")
	if err := os.MkdirAll(filepath.Join(root, "DEBIAN"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "DEBIAN", "control"), []byte(debPackageSource), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), debPackageName+".deb")
	command := exec.CommandContext(t.Context(), "dpkg-deb", "--root-owner-group", "--build", root, output)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dpkg-deb: %v: %s", err, combined)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
