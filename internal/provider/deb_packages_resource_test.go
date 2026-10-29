package provider_test

import (
	"context"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	debType = "pveguest_deb_packages"

	debCurlPath    = "/root/debs/curl.deb"
	debCurlVersion = "8.0-1"

	debFileVersionCommand = "env LC_ALL=C dpkg-deb -f -- " + debCurlPath + " Version"
	debInstallCommand     = aptEnvironment + " install -y --no-install-recommends --allow-downgrades" +
		" -o DPkg::Lock::Timeout=300 -o Dir::Etc::SourceList=/dev/null" +
		" -o Dir::Etc::SourceParts=/nonexistent -- " + debCurlPath
)

func debGuest() *aptGuestAPI {
	guest := newAptGuest(map[string]string{}, map[string]string{})
	guest.debs[debCurlPath] = aptDebFile{name: aptCurl, version: debCurlVersion}
	return guest
}

func debServer(t *testing.T, guest *aptGuestAPI) (tfprotov6.ProviderServer, tftypes.Type) {
	t.Helper()
	server, _ := aptServer(t, guest)
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[debType]
	if !found {
		t.Fatalf("Provider schema has no resource of this type: %s", debType)
	}
	return server, resourceSchema.ValueType()
}

func debValue(t *testing.T, resourceType tftypes.Type, startServices *bool) *tfprotov6.DynamicValue {
	t.Helper()
	startValue := tftypes.NewValue(tftypes.Bool, nil)
	if startServices != nil {
		startValue = tftypes.NewValue(tftypes.Bool, *startServices)
	}
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":     tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":     tftypes.NewValue(tftypes.Number, optionsVMID),
		"kind":     tftypes.NewValue(tftypes.String, localesKind),
		"packages": stringMap(map[string]string{aptCurl: debCurlPath}),
		"deb_versions": tftypes.NewValue(
			tftypes.Map{ElementType: tftypes.String}, tftypes.UnknownValue,
		),
		"start_services": startValue,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func debApply(
	t *testing.T,
	server tfprotov6.ProviderServer,
	resourceType tftypes.Type,
	planned *tfprotov6.DynamicValue,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	applied, err := server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     debType,
		PriorState:   powerResourceNull(t, resourceType),
		PlannedState: planned,
		Config:       planned,
	})
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

func TestDebPackagesStartServicesFalseBlocksStartsOnlyDuringInstall(t *testing.T) {
	disabled := false
	guest := debGuest()
	server, resourceType := debServer(t, guest)

	applied := debApply(t, server, resourceType, debValue(t, resourceType, &disabled))

	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	requireAptCommands(t, guest,
		debFileVersionCommand,
		versionQueryCommand+aptCurl,
		policyExistsCommand,
		policyCreateCommand,
		policyModeCommand,
		debInstallCommand,
		policyRemoveCommand,
		versionQueryCommand+aptCurl,
	)
	if got := guest.policyAtInstalls(); !slices.Equal(got, []string{policyAtInstallExpected}) {
		t.Fatalf("Policy file at each install command is %q, want %q", got, []string{policyAtInstallExpected})
	}
	if _, exists := guest.guestFile(policyPath); exists {
		t.Fatal("Guest still has the policy file after the apply.")
	}
}

func TestDebPackagesStartServicesUnsetRunsInstallWithoutPolicyFile(t *testing.T) {
	guest := debGuest()
	server, resourceType := debServer(t, guest)

	applied := debApply(t, server, resourceType, debValue(t, resourceType, nil))

	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	requireAptCommands(t, guest,
		debFileVersionCommand,
		versionQueryCommand+aptCurl,
		debInstallCommand,
		versionQueryCommand+aptCurl,
	)
}

func TestDebPackagesStartServicesFalseRefusesExistingPolicyFile(t *testing.T) {
	disabled := false
	guest := debGuest()
	guest.files[policyPath] = existingPolicyContent
	server, resourceType := debServer(t, guest)

	applied := debApply(t, server, resourceType, debValue(t, resourceType, &disabled))

	requireApplyError(t, applied.Diagnostics, policyPath+" already exists")
	requireAptCommands(t, guest,
		debFileVersionCommand,
		versionQueryCommand+aptCurl,
		policyExistsCommand,
	)
	content, exists := guest.guestFile(policyPath)
	if !exists || content != existingPolicyContent {
		t.Fatalf("Guest policy file is %q (exists %t), want the original content.", content, exists)
	}
}

func TestDebPackagesStartServicesFalseRemovesPolicyFileAfterFailedInstall(t *testing.T) {
	disabled := false
	guest := debGuest()
	guest.failInstall = true
	server, resourceType := debServer(t, guest)

	applied := debApply(t, server, resourceType, debValue(t, resourceType, &disabled))

	requireApplyError(t, applied.Diagnostics, "returned an error code")
	requireAptCommands(t, guest,
		debFileVersionCommand,
		versionQueryCommand+aptCurl,
		policyExistsCommand,
		policyCreateCommand,
		policyModeCommand,
		debInstallCommand,
		policyRemoveCommand,
	)
	if _, exists := guest.guestFile(policyPath); exists {
		t.Fatal("Guest still has the policy file after the failed install.")
	}
}
