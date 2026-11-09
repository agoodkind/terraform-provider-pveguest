package acceptance_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	fileAbsentAddress = "pveguest_file_absent.test"
	fileAbsentPath    = testDirectory + "/absent"
	fileAbsentContent = "pveguest-acceptance-absent\n"
)

func (g *testGuest) fileAbsentConfig(filePath string) string {
	return g.providerBlock() + fmt.Sprintf(
		`
resource "pveguest_file_absent" "test" {
  node = local.node
  vmid = local.vmid
  kind = local.kind
  path = %q
}
`,
		filePath,
	)
}

func (g *testGuest) expectPathAbsent(filePath string) resource.TestCheckFunc {
	return checkGuest(func() error {
		if g.exists(filePath) {
			return fmt.Errorf("%s exists but should not exist", filePath)
		}

		return nil
	})
}

func (g *testGuest) expectFileAbsentImport(filePath string) resource.ImportStateCheckFunc {
	return func(states []*terraform.InstanceState) error {
		if len(states) != 1 {
			return fmt.Errorf("import returned %d states instead of 1", len(states))
		}

		want := map[string]string{
			"node": g.guest.Node,
			"vmid": fmt.Sprint(g.guest.VMID),
			"kind": string(g.guest.Kind),
			"path": filePath,
		}
		for name, value := range want {
			if states[0].Attributes[name] != value {
				return fmt.Errorf(
					"imported %s is %q instead of %q",
					name,
					states[0].Attributes[name],
					value,
				)
			}
		}

		return nil
	}
}

func TestAccFileAbsent(t *testing.T) {
	t.Run("removes a regular file", testFileAbsentRegularFile)
	t.Run("removes a dangling symbolic link", testFileAbsentDanglingLink)
	t.Run("plans another removal when the file reappears", testFileAbsentReappears)
	t.Run("rejects a directory", testFileAbsentDirectory)
	t.Run("destroy preserves the file", testFileAbsentDestroy)
	t.Run("imports an absent path", testFileAbsentImport)
	t.Run("rejects an unclean path", testFileAbsentUncleanPath)
}

func testFileAbsentRegularFile(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			CheckDestroy:             guest.expectPathAbsent(fileAbsentPath),
			Steps: []resource.TestStep{
				{
					PreConfig: func() { guest.writeFile(fileAbsentPath, fileAbsentContent) },
					Config:    guest.fileAbsentConfig(fileAbsentPath),
					Check:     guest.expectPathAbsent(fileAbsentPath),
				},
			},
		},
	)
}

func testFileAbsentDanglingLink(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			CheckDestroy:             guest.expectPathAbsent(fileAbsentPath),
			Steps: []resource.TestStep{
				{
					PreConfig: func() {
						guest.mustRun("ln", "-sfT", "--", "/nonexistent", fileAbsentPath)
					},
					Config: guest.fileAbsentConfig(fileAbsentPath),
					Check:  guest.expectPathAbsent(fileAbsentPath),
				},
			},
		},
	)
}

func testFileAbsentReappears(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	config := guest.fileAbsentConfig(fileAbsentPath)

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			CheckDestroy:             guest.expectPathAbsent(fileAbsentPath),
			Steps: []resource.TestStep{
				{
					PreConfig: func() { guest.writeFile(fileAbsentPath, fileAbsentContent) },
					Config:    config,
					Check:     guest.expectPathAbsent(fileAbsentPath),
				},
				{
					PreConfig: func() { guest.writeFile(fileAbsentPath, fileAbsentContent) },
					Config:    config,
					ConfigPlanChecks: expectAction(
						fileAbsentAddress,
						plancheck.ResourceActionCreate,
					),
					Check: guest.expectPathAbsent(fileAbsentPath),
				},
			},
		},
	)
}

func testFileAbsentDirectory(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	directory := fileAbsentPath + "-directory"
	config := guest.fileAbsentConfig(directory)

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					PreConfig:   func() { guest.mustRun("mkdir", "--", directory) },
					Config:      config,
					ExpectError: regexp.MustCompile(`(?s)is\s+a\s+directory`),
				},
				{
					PreConfig: func() {
						if !guest.exists(directory) {
							t.Fatalf(
								"%s does not exist after the failed apply but should remain a directory",
								directory,
							)
						}

						guest.mustRun("rmdir", "--", directory)
					},
					Config: config,
					Check:  guest.expectPathAbsent(directory),
				},
			},
		},
	)
}

func testFileAbsentDestroy(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	config := guest.fileAbsentConfig(fileAbsentPath)

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			AdditionalCLIOptions: &resource.AdditionalCLIOptions{
				Plan: resource.PlanOptions{NoRefresh: true},
			},
			Steps: []resource.TestStep{
				{
					Config: config,
					Check:  guest.expectPathAbsent(fileAbsentPath),
				},
				{
					// PreConfig writes the file before Destroy; Destroy must preserve the file.
					PreConfig: func() { guest.writeFile(fileAbsentPath, fileAbsentContent) },
					Config:    config,
					Destroy:   true,
				},
				{
					PreConfig: func() {
						if !guest.exists(fileAbsentPath) {
							t.Fatalf(
								"%s does not exist after destroy but should remain a file",
								fileAbsentPath,
							)
						}
					},
					Config: config,
					Check:  guest.expectPathAbsent(fileAbsentPath),
				},
			},
		},
	)
}

func testFileAbsentImport(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	config := guest.fileAbsentConfig(fileAbsentPath)
	importID := fmt.Sprintf(
		"%s/%d/%s/%s",
		guest.guest.Node,
		guest.guest.VMID,
		guest.guest.Kind,
		fileAbsentPath,
	)

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config:           config,
					ResourceName:     fileAbsentAddress,
					ImportState:      true,
					ImportStateId:    importID,
					ImportStateCheck: guest.expectFileAbsentImport(fileAbsentPath),
				},
				{
					Config:        config,
					ResourceName:  fileAbsentAddress,
					ImportState:   true,
					ImportStateId: "node/101/lxc",
					ExpectError:   regexp.MustCompile(`(?s)must\s+use\s+the\s+format`),
				},
			},
		},
	)
}

func testFileAbsentUncleanPath(t *testing.T) {
	guest := newTestGuest(t)

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config:      guest.fileAbsentConfig(testDirectory + "/../absent"),
					ExpectError: regexp.MustCompile(`(?s)path\s+must\s+start\s+with`),
				},
				{
					Config:      guest.fileAbsentConfig("relative/absent"),
					ExpectError: regexp.MustCompile(`(?s)path\s+must\s+start\s+with`),
				},
				{
					Config:      guest.fileAbsentConfig(testDirectory + "/"),
					ExpectError: regexp.MustCompile(`(?s)path\s+must\s+start\s+with`),
				},
			},
		},
	)
}
