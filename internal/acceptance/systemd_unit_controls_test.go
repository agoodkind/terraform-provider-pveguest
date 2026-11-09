package acceptance_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const (
	controlsService     = "pveguest-acc-controls.service"
	controlsUnitAddress = "pveguest_systemd_unit.controls"
	controlsUnitContent = "[Unit]\nDescription=pveguest acceptance test controls service\n\n[Service]\nType=simple\nExecStart=/bin/sleep 3600\n\n[Install]\nWantedBy=multi-user.target\n"
	controlsFailingUnit = "[Unit]\nDescription=pveguest acceptance test failing service\n\n[Service]\nType=oneshot\nExecStart=/bin/false\n\n[Install]\nWantedBy=multi-user.target\n"

	controlsBaselineVersion = "0.1.2"

	// systemctl mask fails for regular unit files in /etc/systemd/system.
	vendorUnitDirectory = "/usr/lib/systemd/system"
)

func (g *testGuest) controlsConfig(extra string) string {
	return g.controlsConfigWith(false, false, extra)
}

func (g *testGuest) controlsConfigWith(enabled bool, active bool, extra string) string {
	return g.providerBlock() + fmt.Sprintf(
		`
resource "pveguest_systemd_unit" "controls" {
  node    = local.node
  vmid    = local.vmid
  kind    = local.kind
  name    = %q
  enabled = %t
  active  = %t
  %s
}
`,
		controlsService,
		enabled,
		active,
		extra,
	)
}

func (g *testGuest) useControlsUnit(directory string, content string) {
	g.t.Helper()
	g.useAbsentControlsUnit()
	g.writeFile(directory+"/"+controlsService, content)
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) useAbsentControlsUnit() {
	g.t.Helper()
	g.t.Cleanup(g.removeControlsUnit)
}

func (g *testGuest) removeControlsUnit() {
	g.t.Helper()
	g.run(nil, "systemctl", "unmask", "--", controlsService)
	g.run(nil, "systemctl", "--runtime", "unmask", "--", controlsService)
	g.run(nil, "systemctl", "disable", "--", controlsService)
	g.run(nil, "systemctl", "stop", "--", controlsService)
	g.run(nil, "systemctl", "reset-failed", "--", controlsService)
	g.mustRun(
		"rm",
		"-f",
		"--",
		unitDirectory+"/"+controlsService,
		vendorUnitDirectory+"/"+controlsService,
		unitDirectory+"/multi-user.target.wants/"+controlsService,
	)
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) failControlsUnit() {
	g.t.Helper()
	g.run(nil, "systemctl", "start", "--", controlsService)
	got := g.systemctlState("is-active", controlsService)
	if got != "failed" {
		g.t.Fatalf(
			"%s has active state %s after the failed start; it should be failed",
			controlsService,
			got,
		)
	}
}

func (g *testGuest) expectControlsUnit(enabled string, active ...string) resource.TestCheckFunc {
	return checkGuest(func() error {
		gotEnabled := g.systemctlState("is-enabled", controlsService)
		gotActive := g.systemctlState("is-active", controlsService)
		if gotEnabled != enabled || !slices.Contains(active, gotActive) {
			return fmt.Errorf(
				"%s has enabled state %s and active state %s; it should have enabled state %s and one of the active states %s",
				controlsService,
				gotEnabled,
				gotActive,
				enabled,
				strings.Join(active, ", "),
			)
		}

		return nil
	})
}

func (g *testGuest) expectControlsUnitAbsent() resource.TestCheckFunc {
	return checkGuest(func() error {
		for _, directory := range []string{unitDirectory, vendorUnitDirectory} {
			unitFile := directory + "/" + controlsService
			if g.exists(unitFile) {
				return fmt.Errorf("%s exists but should not exist", unitFile)
			}
		}

		return nil
	})
}

func controlsRequirementPattern(attribute string) *regexp.Regexp {
	return regexp.MustCompile(
		fmt.Sprintf(
			`(?s)%s\s+requires\s+enabled\s+=\s+false\s+and\s+active\s+=\s+false`,
			attribute,
		),
	)
}

func TestAccSystemdUnitControls(t *testing.T) {
	t.Run("allow_missing accepts an absent unit", testControlsAllowMissing)
	t.Run("invalid combinations fail validation", testControlsInvalidCombinations)
	t.Run("unit masking matches masked", testControlsMasked)
	t.Run("masked with an absent unit", testControlsMaskedAbsent)
	t.Run("reset_failed clears the failed state", testControlsResetFailed)
	t.Run("unset attributes preserve masked and failed states", testControlsUnset)
	t.Run("state from the installed provider produces a plan without changes", testControlsUpgrade)
}

func testControlsAllowMissing(t *testing.T) {
	guest := newTestGuest(t)
	guest.useAbsentControlsUnit()
	config := guest.controlsConfig("allow_missing = true")

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config: config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(
							controlsUnitAddress,
							"allow_missing",
							"true",
						),
						guest.expectControlsUnitAbsent(),
					),
				},
				{
					PreConfig: func() {
						guest.writeFile(unitDirectory+"/"+controlsService, controlsUnitContent)
						guest.mustRun("systemctl", "daemon-reload")
						guest.mustRun("systemctl", "enable", "--now", "--", controlsService)
					},
					Config: config,
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionUpdate,
					),
					Check: guest.expectControlsUnit("disabled", "inactive"),
				},
				{
					PreConfig: guest.removeControlsUnit,
					Config:    config,
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionNoop,
					),
					Check: guest.expectControlsUnitAbsent(),
				},
			},
		},
	)
}

func testControlsInvalidCombinations(t *testing.T) {
	guest := newTestGuest(t)

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config:      guest.controlsConfigWith(true, false, "allow_missing = true"),
					ExpectError: controlsRequirementPattern("allow_missing"),
				},
				{
					Config:      guest.controlsConfigWith(false, true, "allow_missing = true"),
					ExpectError: controlsRequirementPattern("allow_missing"),
				},
				{
					Config:      guest.controlsConfigWith(true, false, "masked = true"),
					ExpectError: controlsRequirementPattern("masked"),
				},
				{
					Config:      guest.controlsConfigWith(false, true, "masked = true"),
					ExpectError: controlsRequirementPattern("masked"),
				},
			},
		},
	)
}

func testControlsMasked(t *testing.T) {
	guest := newTestGuest(t)
	guest.useControlsUnit(vendorUnitDirectory, controlsUnitContent)
	declared := guest.controlsConfig("masked = true")

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					PreConfig: func() {
						guest.mustRun("systemctl", "start", "--", controlsService)
					},
					Config: declared,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(controlsUnitAddress, "masked", "true"),
						guest.expectControlsUnit("masked", "inactive"),
					),
				},
				{
					PreConfig: func() {
						guest.mustRun("systemctl", "unmask", "--", controlsService)
					},
					Config: declared,
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionUpdate,
					),
					Check: guest.expectControlsUnit("masked", "inactive"),
				},
				{
					Config: guest.controlsConfig("masked = false"),
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionUpdate,
					),
					Check: guest.expectControlsUnit("disabled", "inactive"),
				},
				{
					Config: declared,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(controlsUnitAddress, "masked", "true"),
						guest.expectControlsUnit("masked", "inactive"),
					),
				},
				{
					PreConfig: func() {
						guest.mustRun("systemctl", "unmask", "--", controlsService)
						guest.mustRun("systemctl", "--runtime", "mask", "--", controlsService)
						if got := guest.systemctlState(
							"is-enabled",
							controlsService,
						); got != "masked-runtime" {
							t.Fatalf(
								"%s has unit file state %s after runtime masking; it should be masked-runtime",
								controlsService,
								got,
							)
						}
					},
					Config: guest.controlsConfig("masked = false"),
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionUpdate,
					),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(controlsUnitAddress, "masked", "false"),
						guest.expectControlsUnit("disabled", "inactive"),
					),
				},
				{
					Config: guest.controlsConfig("masked = false"),
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionNoop,
					),
					Check: guest.expectControlsUnit("disabled", "inactive"),
				},
				{
					PreConfig: func() {
						guest.mustRun("systemctl", "mask", "--", controlsService)
					},
					Config: guest.controlsConfig(""),
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionNoop,
					),
					Check: guest.expectControlsUnit("masked", "inactive"),
				},
			},
		},
	)
}

func testControlsMaskedAbsent(t *testing.T) {
	guest := newTestGuest(t)
	guest.useAbsentControlsUnit()

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					Config: guest.controlsConfig("masked = true"),
					Check:  guest.expectControlsUnit("masked", "inactive"),
				},
				{
					Config: guest.controlsConfig(
						"masked = false\n  allow_missing = true",
					),
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionUpdate,
					),
					Check: guest.expectControlsUnitAbsent(),
				},
			},
		},
	)
}

func testControlsResetFailed(t *testing.T) {
	guest := newTestGuest(t)
	guest.useControlsUnit(unitDirectory, controlsFailingUnit)
	config := guest.controlsConfig("reset_failed = true")

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					PreConfig: guest.failControlsUnit,
					Config:    config,
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(controlsUnitAddress, "reset_failed", "true"),
						guest.expectControlsUnit("disabled", "inactive"),
					),
				},
				{
					PreConfig: guest.failControlsUnit,
					Config:    config,
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionUpdate,
					),
					Check: guest.expectControlsUnit("disabled", "inactive"),
				},
			},
		},
	)
}

func testControlsUnset(t *testing.T) {
	guest := newTestGuest(t)
	guest.useControlsUnit(vendorUnitDirectory, controlsFailingUnit)
	config := guest.controlsConfig("")

	resource.Test(
		t,
		resource.TestCase{
			ProtoV6ProviderFactories: protoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					PreConfig: guest.failControlsUnit,
					Config:    config,
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionCreate,
					),
					Check: guest.expectControlsUnit("disabled", "failed"),
				},
				{
					PreConfig: func() {
						guest.mustRun("systemctl", "mask", "--", controlsService)
					},
					Config: config,
					ConfigPlanChecks: expectAction(
						controlsUnitAddress,
						plancheck.ResourceActionNoop,
					),
					Check: guest.expectControlsUnit("masked", "inactive", "failed"),
				},
			},
		},
	)
}

func testControlsUpgrade(t *testing.T) {
	guest := newTestGuest(t)
	guest.useRemovalUnit()
	var invocationID string
	installed := map[string]resource.ExternalProvider{
		"pveguest": {
			Source:            providerSource,
			VersionConstraint: controlsBaselineVersion,
		},
	}

	resource.Test(
		t,
		resource.TestCase{
			CheckDestroy: guest.expectRemovalUnit("enabled", "active"),
			Steps: []resource.TestStep{
				{
					ExternalProviders: installed,
					Config:            guest.externalProviderConfig(),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckNoResourceAttr(removalUnitAddress, "allow_missing"),
						resource.TestCheckNoResourceAttr(removalUnitAddress, "masked"),
						resource.TestCheckNoResourceAttr(removalUnitAddress, "reset_failed"),
						guest.expectRemovalUnit("enabled", "active"),
						checkGuest(func() error {
							invocationID = strings.TrimSpace(
								guest.mustRun(
									"systemctl",
									"show",
									"--property=InvocationID",
									"--value",
									"--",
									testRemovalService,
								),
							)
							if invocationID == "" {
								return fmt.Errorf(
									"%s does not report an InvocationID after the baseline apply",
									testRemovalService,
								)
							}

							return nil
						}),
					),
				},
				{
					ProtoV6ProviderFactories: protoV6ProviderFactories,
					Config:                   guest.removalUnitConfig(""),
					ConfigPlanChecks: expectAction(
						removalUnitAddress,
						plancheck.ResourceActionNoop,
					),
					Check: resource.ComposeAggregateTestCheckFunc(
						resource.TestCheckResourceAttr(
							removalUnitAddress,
							"allow_missing",
							"false",
						),
						resource.TestCheckResourceAttr(removalUnitAddress, "masked", "false"),
						resource.TestCheckResourceAttr(removalUnitAddress, "reset_failed", "false"),
						guest.expectRemovalUnit("enabled", "active"),
						checkGuest(func() error {
							currentInvocationID := strings.TrimSpace(
								guest.mustRun(
									"systemctl",
									"show",
									"--property=InvocationID",
									"--value",
									"--",
									testRemovalService,
								),
							)
							if invocationID == "" || currentInvocationID != invocationID {
								return fmt.Errorf(
									"%s reports InvocationID %q after the upgrade; it should match the nonempty baseline InvocationID %q",
									testRemovalService,
									currentInvocationID,
									invocationID,
								)
							}

							return nil
						}),
					),
				},
			},
		},
	)
}
