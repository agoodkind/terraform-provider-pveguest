package acceptance_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// RFC 1149 is an immutable document. The guest downloads it from the public
// internet. These tests require guest network access to www.rfc-editor.org
// over HTTPS.
const (
	downloadURL    = "https://www.rfc-editor.org/rfc/rfc1149.txt"
	downloadSHA256 = "a8660fa4f47bd5e3db1cd5d5baad983d8b6f3f1e8a1a04b8552f3c2ce8f33c18"
	wrongSHA256    = "0000000000000000000000000000000000000000000000000000000000000000"
)

func (g *testGuest) downloadConfig(path string, hash string, extra string) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_download" "test" {
  node   = local.node
  vmid   = local.vmid
  kind   = local.kind
  url    = %q
  sha256 = %q
  path   = %q
  %s
}
`, downloadURL, hash, path, extra)
}

func (g *testGuest) expectFileHash(path string, want string) func() error {
	return func() error {
		fields := strings.Fields(g.mustRun("sha256sum", "--", path))
		if len(fields) == 0 || fields[0] != want {
			return fmt.Errorf("sha256sum of %s reports %q, want %s", path, fields, want)
		}
		return nil
	}
}

func TestAccDownload(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	path := testDirectory + "/download/rfc1149.txt"
	config := guest.downloadConfig(path, downloadSHA256, "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: checkGuest(func() error {
			if guest.exists(path) {
				return fmt.Errorf("%s still exists after destroy", path)
			}
			return nil
		}),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "sha256", downloadSHA256),
					resource.TestCheckResourceAttr("pveguest_download.test", "mode", "0644"),
					checkGuest(guest.expectFileHash(path, downloadSHA256)),
					checkGuest(func() error {
						listing := strings.TrimSpace(guest.mustRun("ls", "-A", "--", testDirectory+"/download"))
						if listing != "rfc1149.txt" {
							return fmt.Errorf("directory contains %q, want only rfc1149.txt", listing)
						}
						return nil
					}),
				),
			},
			{
				// A hand edit produces a planned rewrite.
				PreConfig:        func() { guest.writeFile(path, "edited by hand\n") },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_download.test", plancheck.ResourceActionUpdate),
				Check:            checkGuest(guest.expectFileHash(path, downloadSHA256)),
			},
			{
				// A missing file is a normal Read result: the plan creates it.
				PreConfig:        func() { guest.mustRun("rm", "-f", "--", path) },
				Config:           config,
				ConfigPlanChecks: expectAction("pveguest_download.test", plancheck.ResourceActionCreate),
				Check:            checkGuest(guest.expectFileHash(path, downloadSHA256)),
			},
		},
	})
}

func TestAccDownloadHashMismatch(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	path := testDirectory + "/mismatch.txt"
	original := "original content\n"
	guest.writeFile(path, original)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// OpenTofu wraps the diagnostic text, and each gap in the
				// pattern matches a line break.
				Config: guest.downloadConfig(path, wrongSHA256, ""),
				ExpectError: regexp.MustCompile(
					`(?s)the\s+download\s+of\s+` + regexp.QuoteMeta(downloadURL) +
						`\s+has\s+sha256\s+` + downloadSHA256 +
						`,\s+but\s+the\s+configuration\s+expects\s+` + wrongSHA256,
				),
			},
			{
				PreConfig: func() {
					if err := guest.expectFileContent(path, original)(); err != nil {
						t.Fatal(err)
					}
					listing := strings.TrimSpace(guest.mustRun("ls", "-A", "--", testDirectory))
					if listing != "mismatch.txt" {
						t.Fatalf("directory contains %q after the failed apply", listing)
					}
				},
				Config:             guest.downloadConfig(path, wrongSHA256, ""),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
