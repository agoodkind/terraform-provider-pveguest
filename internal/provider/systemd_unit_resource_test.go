package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	unitType     = "pveguest_systemd_unit"
	unitName     = "pveguest-test.service"
	unitImportID = optionsNode + "/101/" + localesKind + "/" + unitName
	unitFormat   = "<node>/<vmid>/<kind>/<name>"

	// ImportState sets this private state key.
	unitImportedKey = "imported"

	systemctlPrefix = "env LC_ALL=C systemctl "

	unitEnabled  = "enabled"
	unitDisabled = "disabled"
	unitActive   = "active"
	unitInactive = "inactive"

	isEnabledFailureExitCode = 1
	isActiveFailureExitCode  = 3
)

type systemdGuestAPI struct {
	mutex     sync.Mutex
	fileState string
	active    string
	commands  []string
	results   map[string]localeExecStatus
}

func (api *systemdGuestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	switch {
	case request.Method == http.MethodPost && request.URL.Path == localesExecPath:
		var body localeExecRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		api.commands = append(api.commands, strings.Join(body.Command, " "))
		processID := len(api.commands)
		if api.results == nil {
			api.results = make(map[string]localeExecStatus)
		}
		api.results[strconv.Itoa(processID)] = api.run(body.Command)
		_ = json.NewEncoder(writer).Encode(localeExecStartEnvelope{Data: localeExecStart{PID: processID}})
	case request.Method == http.MethodGet && request.URL.Path == localesExecStatusPath:
		status, found := api.results[request.URL.Query().Get("pid")]
		if !found {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(localeExecStatusEnvelope{Data: status})
	default:
		http.NotFound(writer, request)
	}
}

// run simulates systemctl commands for one unit.
func (api *systemdGuestAPI) run(argv []string) localeExecStatus {
	arguments := argv
	for len(arguments) > 0 && (arguments[0] == "env" || strings.Contains(arguments[0], "=")) {
		arguments = arguments[1:]
	}
	if len(arguments) < 2 || arguments[0] != "systemctl" {
		return aptOutput(unknownCommandExitCode, "", "unknown command")
	}
	switch arguments[1] {
	case "is-enabled":
		exitCode := 0
		if api.fileState != unitEnabled {
			exitCode = isEnabledFailureExitCode
		}
		return aptOutput(exitCode, api.fileState+"\n", "")
	case "is-active":
		exitCode := 0
		if api.active != unitActive {
			exitCode = isActiveFailureExitCode
		}
		return aptOutput(exitCode, api.active+"\n", "")
	case "enable", "reenable":
		api.fileState = unitEnabled
	case "disable":
		api.fileState = unitDisabled
	case "start", "restart":
		api.active = unitActive
	case "stop":
		api.active = unitInactive
	}
	return aptOutput(0, "", "")
}

func (api *systemdGuestAPI) recordedCommands() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return slices.Clone(api.commands)
}

func (api *systemdGuestAPI) resetCommands() {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.commands = nil
}

func systemdServer(t *testing.T, guest *systemdGuestAPI) (tfprotov6.ProviderServer, tftypes.Type) {
	t.Helper()
	api := httptest.NewServer(guest)
	t.Cleanup(api.Close)
	server := configuredOptionsServer(t, api.URL)
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[unitType]
	if !found {
		t.Fatalf("Provider schema has no resource of this type: %s", unitType)
	}
	return server, resourceSchema.ValueType()
}

// unitValue builds a configuration value. A nil restartOn sets restart_on to
// null; removeOnDelete sets remove_on_delete.
func unitValue(
	t *testing.T,
	resourceType tftypes.Type,
	restartOn map[string]string,
	removeOnDelete tftypes.Value,
) *tfprotov6.DynamicValue {
	t.Helper()
	restart := tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)
	if restartOn != nil {
		restart = stringMap(restartOn)
	}
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":             tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":             tftypes.NewValue(tftypes.Number, optionsVMID),
		"kind":             tftypes.NewValue(tftypes.String, localesKind),
		"name":             tftypes.NewValue(tftypes.String, unitName),
		"enabled":          tftypes.NewValue(tftypes.Bool, true),
		"active":           tftypes.NewValue(tftypes.Bool, true),
		"restart_on":       restart,
		"remove_on_delete": removeOnDelete,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func unitAttributes(t *testing.T, resourceType tftypes.Type, state *tfprotov6.DynamicValue) map[string]tftypes.Value {
	t.Helper()
	value, err := state.Unmarshal(resourceType)
	if err != nil {
		t.Fatal(err)
	}
	attributes := make(map[string]tftypes.Value)
	if err := value.As(&attributes); err != nil {
		t.Fatal(err)
	}
	return attributes
}

func requireUnitBool(t *testing.T, attributes map[string]tftypes.Value, name string, want bool) {
	t.Helper()
	var got bool
	if err := attributes[name].As(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("state %s = %t, want %t", name, got, want)
	}
}

func hasImportedKey(t *testing.T, private []byte) bool {
	t.Helper()
	if len(private) == 0 {
		return false
	}
	entries := make(map[string][]byte)
	if err := json.Unmarshal(private, &entries); err != nil {
		t.Fatal(err)
	}
	_, found := entries[unitImportedKey]
	return found
}

func requireSystemctlCommands(t *testing.T, guest *systemdGuestAPI, want ...string) {
	t.Helper()
	expected := make([]string, 0, len(want))
	for _, arguments := range want {
		expected = append(expected, systemctlPrefix+arguments)
	}
	got := guest.recordedCommands()
	if !slices.Equal(got, expected) {
		t.Fatalf("Guest commands differ from the expected sequence (received, expected):\n%q\n%q", got, expected)
	}
}

// planAndApplyUnit plans against the prior state and applies the plan with its
// private state.
func planAndApplyUnit(
	t *testing.T,
	server tfprotov6.ProviderServer,
	resourceType tftypes.Type,
	prior *tfprotov6.DynamicValue,
	priorPrivate []byte,
	restartOn map[string]string,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	ctx := context.Background()
	unset := tftypes.NewValue(tftypes.Bool, nil)
	config := unitValue(t, resourceType, restartOn, unset)
	proposed := unitValue(t, resourceType, restartOn, unitAttributes(t, resourceType, prior)["remove_on_delete"])
	plan, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         unitType,
		PriorState:       prior,
		ProposedNewState: proposed,
		Config:           config,
		PriorPrivate:     priorPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, plan.Diagnostics)
	if len(plan.RequiresReplace) > 0 {
		t.Fatalf("plan requires replacement at %v, want an update", plan.RequiresReplace)
	}
	applied, err := server.ApplyResourceChange(ctx, &tfprotov6.ApplyResourceChangeRequest{
		TypeName:       unitType,
		PriorState:     prior,
		PlannedState:   plan.PlannedState,
		Config:         config,
		PlannedPrivate: plan.PlannedPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	return applied
}

func TestSystemdUnitImportRejectsMalformedID(t *testing.T) {
	guest := &systemdGuestAPI{fileState: unitEnabled, active: unitActive}
	server, _ := systemdServer(t, guest)

	malformed := map[string]string{
		"missing name":    optionsNode + "/101/" + localesKind,
		"extra field":     unitImportID + "/extra",
		"empty vmid":      optionsNode + "//" + localesKind + "/" + unitName,
		"nonnumeric vmid": optionsNode + "/one/" + localesKind + "/" + unitName,
	}
	for name, id := range malformed {
		response, err := server.ImportResourceState(context.Background(),
			&tfprotov6.ImportResourceStateRequest{TypeName: unitType, ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Diagnostics) == 0 || !strings.Contains(response.Diagnostics[0].Detail, unitFormat) {
			t.Errorf("import case %q did not report the ID format: %v", name, response.Diagnostics)
		}
	}

	invalid := map[string]string{
		"unknown kind":        optionsNode + "/101/docker/" + unitName,
		"name without suffix": optionsNode + "/101/" + localesKind + "/ssh",
	}
	for name, id := range invalid {
		response, err := server.ImportResourceState(context.Background(),
			&tfprotov6.ImportResourceStateRequest{TypeName: unitType, ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Diagnostics) == 0 || response.Diagnostics[0].Severity != tfprotov6.DiagnosticSeverityError {
			t.Errorf("import accepted invalid case %s, want an error", name)
		}
	}
	requireSystemctlCommands(t, guest)
}

func TestSystemdUnitImportSkipsRestartOnFirstUpdate(t *testing.T) {
	ctx := context.Background()
	guest := &systemdGuestAPI{fileState: unitDisabled, active: unitActive}
	server, resourceType := systemdServer(t, guest)

	imported, err := server.ImportResourceState(ctx,
		&tfprotov6.ImportResourceStateRequest{TypeName: unitType, ID: unitImportID})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, imported.Diagnostics)
	if len(imported.ImportedResources) != 1 {
		t.Fatalf("import returned %d resources, want 1", len(imported.ImportedResources))
	}
	stub := imported.ImportedResources[0]
	if !hasImportedKey(t, stub.Private) {
		t.Fatal("import did not record the imported key in private state")
	}
	requireUnitBool(t, unitAttributes(t, resourceType, stub.State), "remove_on_delete", false)

	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
		TypeName:     unitType,
		CurrentState: stub.State,
		Private:      stub.Private,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, read.Diagnostics)
	readAttributes := unitAttributes(t, resourceType, read.NewState)
	requireUnitBool(t, readAttributes, "enabled", false)
	requireUnitBool(t, readAttributes, "active", true)
	if !hasImportedKey(t, read.Private) {
		t.Fatal("Read deleted the imported key from private state")
	}

	guest.resetCommands()
	first := planAndApplyUnit(t, server, resourceType, read.NewState, read.Private,
		map[string]string{"write_id": "one"})
	requireSystemctlCommands(t, guest, "daemon-reload", "is-enabled -- "+unitName,
		"is-active -- "+unitName, "enable -- "+unitName)
	if hasImportedKey(t, first.Private) {
		t.Fatal("first Update after import retained the imported key in private state")
	}

	guest.resetCommands()
	planAndApplyUnit(t, server, resourceType, first.NewState, first.Private,
		map[string]string{"write_id": "two"})
	requireSystemctlCommands(t, guest, "daemon-reload", "is-enabled -- "+unitName,
		"is-active -- "+unitName, "reenable -- "+unitName, "restart -- "+unitName)
}
