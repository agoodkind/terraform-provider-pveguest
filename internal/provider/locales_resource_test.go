package provider_test

import (
	"context"
	"encoding/base64"
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
	localesType           = "pveguest_locales"
	localesKind           = "lxc"
	localesExecPath       = "/api2/json/nodes/pve/lxc/101/exec"
	localesExecStatusPath = "/api2/json/nodes/pve/lxc/101/exec-status"

	listLocalesCommand = "env LC_ALL=C locale -a"
	localeGenCommand   = "env LC_ALL=C locale-gen"

	unknownCommandExitCode = 127

	utf8Entry    = "en_US.UTF-8 UTF-8"
	utf8Archived = "en_US.utf8"

	euroEntry    = "de_DE.ISO-8859-15@euro ISO-8859-15"
	euroArchived = "de_DE.iso885915@euro"

	bareEntry    = "en_US ISO-8859-1"
	bareArchived = "en_US"

	digitEntry    = "fr_FR.8859-1 ISO-8859-1"
	digitArchived = "fr_FR.iso88591"

	latin9Entry = "en_US.ISO-8859-15 ISO-8859-15"
)

type localeExecRequest struct {
	Command []string `json:"command"`
}

type localeExecStart struct {
	PID int `json:"pid"`
}

type localeExecStartEnvelope struct {
	Data localeExecStart `json:"data"`
}

type localeExecStatus struct {
	Exited   int    `json:"exited"`
	ExitCode int    `json:"exitcode"`
	OutData  string `json:"out-data"`
	ErrData  string `json:"err-data"`
}

type localeExecStatusEnvelope struct {
	Data localeExecStatus `json:"data"`
}

type localeGuestAPI struct {
	mutex     sync.Mutex
	archive   []string
	localeGen []string
	commands  []string
	results   map[string]localeExecStatus
}

func (api *localeGuestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	switch {
	case request.Method == http.MethodPost && request.URL.Path == localesExecPath:
		var body localeExecRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		command := strings.Join(body.Command, " ")
		api.commands = append(api.commands, command)
		processID := len(api.commands)
		if api.results == nil {
			api.results = make(map[string]localeExecStatus)
		}
		api.results[strconv.Itoa(processID)] = api.run(command)
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

func (api *localeGuestAPI) run(command string) localeExecStatus {
	switch command {
	case listLocalesCommand:
		output := strings.Join(api.archive, "\n") + "\n"
		return localeExecStatus{Exited: 1, OutData: base64.StdEncoding.EncodeToString([]byte(output))}
	case localeGenCommand:
		for _, name := range api.localeGen {
			if !slices.Contains(api.archive, name) {
				api.archive = append(api.archive, name)
			}
		}
		return localeExecStatus{Exited: 1}
	default:
		return localeExecStatus{Exited: 1, ExitCode: unknownCommandExitCode}
	}
}

func (api *localeGuestAPI) recordedCommands() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return slices.Clone(api.commands)
}

func localesServer(t *testing.T, handler *localeGuestAPI) (tfprotov6.ProviderServer, tftypes.Type) {
	t.Helper()
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	server := configuredOptionsServer(t, api.URL)
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[localesType]
	if !found {
		t.Fatalf("Provider schema has no resource of this type: %s", localesType)
	}
	return server, resourceSchema.ValueType()
}

func localesValue(
	t *testing.T,
	resourceType tftypes.Type,
	entries []string,
	generateOn map[string]string,
) *tfprotov6.DynamicValue {
	t.Helper()
	members := make([]tftypes.Value, 0, len(entries))
	for _, entry := range entries {
		members = append(members, tftypes.NewValue(tftypes.String, entry))
	}
	trigger := tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)
	if generateOn != nil {
		trigger = stringMap(generateOn)
	}
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":        tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":        tftypes.NewValue(tftypes.Number, optionsVMID),
		"kind":        tftypes.NewValue(tftypes.String, localesKind),
		"locales":     tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, members),
		"generate_on": trigger,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func localeEntriesOf(t *testing.T, resourceType tftypes.Type, state *tfprotov6.DynamicValue) []string {
	t.Helper()
	value, err := state.Unmarshal(resourceType)
	if err != nil {
		t.Fatal(err)
	}
	attributes := make(map[string]tftypes.Value)
	if err := value.As(&attributes); err != nil {
		t.Fatal(err)
	}
	var members []tftypes.Value
	if err := attributes["locales"].As(&members); err != nil {
		t.Fatal(err)
	}
	entries := make([]string, 0, len(members))
	for _, member := range members {
		var entry string
		if err := member.As(&entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	slices.Sort(entries)
	return entries
}

func requireLocaleEntries(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("State locales differ from expected entries (received, expected): %v, %v", got, want)
	}
}

func requireLocaleCommands(t *testing.T, handler *localeGuestAPI, want ...string) {
	t.Helper()
	got := handler.recordedCommands()
	if !slices.Equal(got, want) {
		t.Fatalf("Guest commands differ from the expected sequence (received, expected): %v, %v", got, want)
	}
}

func requireNoLocaleDiagnostics(t *testing.T, diagnostics []*tfprotov6.Diagnostic) {
	t.Helper()
	if len(diagnostics) > 0 {
		t.Fatalf("Provider returned an unexpected diagnostic: %s: %s", diagnostics[0].Summary, diagnostics[0].Detail)
	}
}

func applyLocales(
	t *testing.T,
	server tfprotov6.ProviderServer,
	prior *tfprotov6.DynamicValue,
	planned *tfprotov6.DynamicValue,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	applied, err := server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     localesType,
		PriorState:   prior,
		PlannedState: planned,
		Config:       planned,
	})
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

func TestLocalesAllPresentSendsNoGenerationCommand(t *testing.T) {
	handler := &localeGuestAPI{archive: []string{"C", "C.utf8", "POSIX", euroArchived, utf8Archived}}
	server, resourceType := localesServer(t, handler)

	planned := localesValue(t, resourceType, []string{utf8Entry, euroEntry}, nil)
	applied := applyLocales(t, server, powerResourceNull(t, resourceType), planned)
	requireNoLocaleDiagnostics(t, applied.Diagnostics)

	requireLocaleCommands(t, handler, listLocalesCommand)
	requireLocaleEntries(t, localeEntriesOf(t, resourceType, applied.NewState), utf8Entry, euroEntry)
}

func TestLocalesAbsentLocalePlansUpdateAndGeneratesOnce(t *testing.T) {
	ctx := context.Background()
	handler := &localeGuestAPI{
		archive:   []string{"C", "C.utf8", "POSIX", utf8Archived},
		localeGen: []string{utf8Archived, euroArchived},
	}
	server, resourceType := localesServer(t, handler)

	declared := localesValue(t, resourceType, []string{utf8Entry, euroEntry}, nil)
	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: localesType, CurrentState: declared})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, read.Diagnostics)
	requireLocaleEntries(t, localeEntriesOf(t, resourceType, read.NewState), utf8Entry)

	plan, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         localesType,
		PriorState:       read.NewState,
		ProposedNewState: declared,
		Config:           declared,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, plan.Diagnostics)
	if len(plan.RequiresReplace) > 0 {
		t.Fatalf("Plan requires replacement instead of an update: %v", plan.RequiresReplace)
	}
	requireLocaleEntries(t, localeEntriesOf(t, resourceType, plan.PlannedState), utf8Entry, euroEntry)

	applied := applyLocales(t, server, read.NewState, plan.PlannedState)
	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	requireLocaleCommands(t, handler,
		listLocalesCommand, listLocalesCommand, localeGenCommand, listLocalesCommand)
	requireLocaleEntries(t, localeEntriesOf(t, resourceType, applied.NewState), utf8Entry, euroEntry)
}

func TestLocalesReadMatchesNormalizedArchiveNames(t *testing.T) {
	handler := &localeGuestAPI{
		archive: []string{"C", "C.utf8", "POSIX", bareArchived, digitArchived, euroArchived, utf8Archived},
	}
	server, resourceType := localesServer(t, handler)

	declared := localesValue(t, resourceType,
		[]string{utf8Entry, euroEntry, bareEntry, digitEntry, latin9Entry}, nil)
	read, err := server.ReadResource(context.Background(),
		&tfprotov6.ReadResourceRequest{TypeName: localesType, CurrentState: declared})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, read.Diagnostics)
	requireLocaleEntries(t, localeEntriesOf(t, resourceType, read.NewState),
		utf8Entry, euroEntry, bareEntry, digitEntry)
}

func TestLocalesChangedGenerateOnRunsGeneration(t *testing.T) {
	handler := &localeGuestAPI{archive: []string{"C", "C.utf8", "POSIX", utf8Archived}}
	server, resourceType := localesServer(t, handler)

	prior := localesValue(t, resourceType, []string{utf8Entry}, map[string]string{"locale_gen": "one"})
	planned := localesValue(t, resourceType, []string{utf8Entry}, map[string]string{"locale_gen": "two"})
	applied := applyLocales(t, server, prior, planned)
	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	requireLocaleCommands(t, handler, listLocalesCommand, localeGenCommand, listLocalesCommand)
}

func TestLocalesEntryAbsentAfterGenerationFailsApply(t *testing.T) {
	handler := &localeGuestAPI{archive: []string{"C", "C.utf8", "POSIX", utf8Archived}}
	server, resourceType := localesServer(t, handler)

	planned := localesValue(t, resourceType, []string{utf8Entry, euroEntry}, nil)
	applied := applyLocales(t, server, powerResourceNull(t, resourceType), planned)
	if len(applied.Diagnostics) == 0 {
		t.Fatal("Apply returned no diagnostic for a declared locale absent after locale-gen.")
	}
	requireLocaleCommands(t, handler, listLocalesCommand, localeGenCommand, listLocalesCommand)
}
