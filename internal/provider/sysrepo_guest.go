package provider

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/agoodkind/terraform-provider-pveguest/internal/sysrepo"
	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	sysrepoTimeoutSeconds = 300

	// The temporary edit file of sysrepocfg lives outside the sysrepo
	// repository.
	sysrepoEditTemplatePath = "/tmp/sysrepo-edit.xml"
)

var yangIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

func sysrepoctlCommand(arguments ...string) transport.Command {
	command := guestCommand(append([]string{"sysrepoctl"}, arguments...)...)
	command.TimeoutSeconds = sysrepoTimeoutSeconds
	return command
}

func sysrepocfgCommand(arguments ...string) transport.Command {
	command := guestCommand(append([]string{"sysrepocfg"}, arguments...)...)
	command.TimeoutSeconds = sysrepoTimeoutSeconds
	return command
}

func listSysrepoModules(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
) ([]sysrepo.InstalledModule, error) {
	command := sysrepoctlCommand("--list")
	result, err := runGuest(ctx, client, guest, command)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(guest, command, result)
	}
	modules, err := sysrepo.ParseModuleList(string(result.Stdout))
	if err != nil {
		slog.ErrorContext(
			ctx, "sysrepoctl module list is malformed",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "err", err,
		)
		return nil, fmt.Errorf("%s: parse the sysrepoctl module list: %w", guest, err)
	}
	return modules, nil
}

// stringValues returns the sorted elements of a set of strings.
func stringValues(ctx context.Context, set types.Set) ([]string, diag.Diagnostics) {
	values := make([]string, 0, len(set.Elements()))
	diagnostics := set.ElementsAs(ctx, &values, false)
	sort.Strings(values)
	return values, diagnostics
}

func stringSetOf(ctx context.Context, values []string) (types.Set, diag.Diagnostics) {
	sorted := slices.Clone(values)
	if sorted == nil {
		sorted = []string{}
	}
	sort.Strings(sorted)
	return types.SetValueFrom(ctx, types.StringType, sorted)
}

// difference returns the elements of left that right lacks.
func difference(left []string, right []string) []string {
	var missing []string
	for _, value := range left {
		if !slices.Contains(right, value) {
			missing = append(missing, value)
		}
	}
	return missing
}

// runSysrepocfgEdit writes the edit document to a temporary file in the guest,
// merges it into the datastore of the module with sysrepocfg, and deletes the
// temporary file.
func runSysrepocfgEdit(
	ctx context.Context,
	client *transport.Client,
	guest transport.Guest,
	datastore string,
	module string,
	edit string,
) error {
	temporaryPath, err := temporaryPathFor(sysrepoEditTemplatePath)
	if err != nil {
		return err
	}
	removeCommand := guestCommand("rm", "-f", "--", temporaryPath)
	if err := writeGuestContent(ctx, client, guest, temporaryPath, []byte(edit)); err != nil {
		// The caller reports the write failure and ignores a failed removal.
		_, _ = client.Run(ctx, guest, removeCommand)
		return err
	}
	editCommand := sysrepocfgCommand(
		"--edit="+temporaryPath, "--datastore", datastore, "--module", module, "--format", "xml",
	)
	editErr := runChecked(ctx, client, guest, editCommand)
	_, _ = client.Run(ctx, guest, removeCommand)
	return editErr
}
