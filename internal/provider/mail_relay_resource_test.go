package provider_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	relayType = "pveguest_mail_relay"
	relayKind = "lxc"

	relayHeaderValue = "fixture-header-value"
	relayHeaderName  = "X-Smtp2go-Api-Key"
	relayAddPath     = "/users/smtp/add"
	relayEditPath    = "/users/smtp/edit"
	relayViewPath    = "/users/smtp/view"

	relayUsername       = "host-clyde"
	relayDescription    = "Host clyde"
	relayHost           = "mail.smtp2go.com"
	relayPort           = 2525
	relayFrom           = "clyde@example.net"
	relayPath           = "/etc/msmtprc"
	relayMode           = "0600"
	relayBlocked        = "blocked"
	relayDriftedFile    = "defaults\n"
	relayPasswordLength = 32

	relayExistingUserJSON = `{"data":{"error":"Already have SMTP user host-clyde"}}`
	relayUnknownUserJSON  = `{"data":{"error":"SMTP user host-clyde does not exist"}}`
	relayEmptyDataJSON    = `{"data":{}}`

	relayMissingUserSummary = "SMTP2GO user is absent"
	relayBlockedUserSummary = "SMTP2GO user status is not allowed"

	relayProgramIndex = 2
	relayScriptIndex  = 2
	relayModeIndex    = 1

	relayFileFormat = `defaults
auth           on
tls            on
tls_starttls   on
tls_trust_file /etc/ssl/certs/ca-certificates.crt
logfile        /var/log/msmtp.log

account smtp2go
host %s
port %d
user %s
auth           login
password %s
from %s

account default : smtp2go
`
)

type relayExecRequest struct {
	Command   []string `json:"command"`
	InputData string   `json:"input-data"`
}

type relayGuestAPI struct {
	mutex       sync.Mutex
	files       map[string][]byte
	modes       map[string]string
	results     map[string]localeExecStatus
	started     int
	renames     int
	failRenames int
}

func (api *relayGuestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	switch {
	case request.Method == http.MethodPost && request.URL.Path == localesExecPath:
		var body relayExecRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		stdin, err := base64.StdEncoding.DecodeString(body.InputData)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		api.started++
		api.results[strconv.Itoa(api.started)] = api.run(body.Command, stdin)
		_ = json.NewEncoder(writer).Encode(localeExecStartEnvelope{Data: localeExecStart{PID: api.started}})
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

func (api *relayGuestAPI) run(argv []string, stdin []byte) localeExecStatus {
	if len(argv) <= relayProgramIndex {
		return localeExecStatus{Exited: 1, ExitCode: unknownCommandExitCode}
	}
	arguments := argv[relayProgramIndex:]
	target := arguments[len(arguments)-1]
	switch arguments[0] {
	case "sh":
		if strings.Contains(arguments[relayScriptIndex], "cat >") {
			api.files[target] = stdin
		}
		return localeExecStatus{Exited: 1}
	case "chmod":
		api.modes[target] = arguments[relayModeIndex]
		return localeExecStatus{Exited: 1}
	case "chown":
		return localeExecStatus{Exited: 1}
	case "mv":
		return api.rename(arguments[len(arguments)-2], target)
	case "sha256sum":
		return api.hash(target)
	case "rm":
		delete(api.files, target)
		delete(api.modes, target)
		return localeExecStatus{Exited: 1}
	default:
		return localeExecStatus{Exited: 1, ExitCode: unknownCommandExitCode}
	}
}

func (api *relayGuestAPI) rename(source string, destination string) localeExecStatus {
	if api.failRenames > 0 {
		api.failRenames--
		message := "mv: cannot move '" + source + "': Read-only file system"
		return localeExecStatus{Exited: 1, ExitCode: 1, ErrData: base64.StdEncoding.EncodeToString([]byte(message))}
	}
	api.files[destination] = api.files[source]
	api.modes[destination] = api.modes[source]
	delete(api.files, source)
	delete(api.modes, source)
	api.renames++
	return localeExecStatus{Exited: 1}
}

func (api *relayGuestAPI) hash(filePath string) localeExecStatus {
	content, found := api.files[filePath]
	if !found {
		message := "sha256sum: " + filePath + ": No such file or directory"
		return localeExecStatus{Exited: 1, ExitCode: 1, ErrData: base64.StdEncoding.EncodeToString([]byte(message))}
	}
	output := relayHash(content) + "  " + filePath + "\n"
	return localeExecStatus{Exited: 1, OutData: base64.StdEncoding.EncodeToString([]byte(output))}
}

func (api *relayGuestAPI) file(filePath string) ([]byte, bool) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	content, found := api.files[filePath]
	return slices.Clone(content), found
}

func (api *relayGuestAPI) setFile(filePath string, content []byte) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.files[filePath] = content
}

func (api *relayGuestAPI) removeFile(filePath string) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	delete(api.files, filePath)
}

func (api *relayGuestAPI) mode(filePath string) string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return api.modes[filePath]
}

func (api *relayGuestAPI) renameCount() int {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return api.renames
}

func (api *relayGuestAPI) failNextRename() {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.failRenames = 1
}

type relayUserBody struct {
	Username    string `json:"username"`
	Generated   string `json:"email_password"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type relayViewData struct {
	Results []relayUserBody `json:"results"`
}

type relayViewEnvelope struct {
	Data relayViewData `json:"data"`
}

type relayMailAPI struct {
	mutex         sync.Mutex
	users         map[string]relayUserBody
	issued        []string
	adds          int
	edits         int
	writeRequests int
}

func (api *relayMailAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	if request.Method != http.MethodPost || request.Header.Get(relayHeaderName) != relayHeaderValue {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body relayUserBody
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	existing, exists := api.users[body.Username]
	switch request.URL.Path {
	case relayAddPath:
		api.writeRequests++
		if exists {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(relayExistingUserJSON))
			return
		}
		api.users[body.Username] = body
		api.issued = append(api.issued, body.Generated)
		api.adds++
		_, _ = writer.Write([]byte(relayEmptyDataJSON))
	case relayEditPath:
		api.writeRequests++
		if !exists {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(relayUnknownUserJSON))
			return
		}
		existing.Generated = body.Generated
		existing.Status = body.Status
		api.users[body.Username] = existing
		api.issued = append(api.issued, body.Generated)
		api.edits++
		_, _ = writer.Write([]byte(relayEmptyDataJSON))
	case relayViewPath:
		results := []relayUserBody{}
		if exists {
			results = append(results, existing)
		}
		_ = json.NewEncoder(writer).Encode(relayViewEnvelope{Data: relayViewData{Results: results}})
	default:
		http.NotFound(writer, request)
	}
}

func (api *relayMailAPI) user(username string) relayUserBody {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return api.users[username]
}

func (api *relayMailAPI) removeUser(username string) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	delete(api.users, username)
}

func (api *relayMailAPI) setStatus(username string, status string) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	user := api.users[username]
	user.Status = status
	api.users[username] = user
}

func (api *relayMailAPI) secrets() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return append([]string{relayHeaderValue}, api.issued...)
}

func (api *relayMailAPI) counts() (int, int, int) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return api.adds, api.edits, api.writeRequests
}

type relayComputed struct {
	path    tftypes.Value
	sha256  tftypes.Value
	writeID tftypes.Value
}

type relayHarness struct {
	server       tfprotov6.ProviderServer
	resourceType tftypes.Type
	guest        *relayGuestAPI
	mail         *relayMailAPI
}

func newRelayHarness(t *testing.T) *relayHarness {
	t.Helper()
	guest := &relayGuestAPI{
		files:   make(map[string][]byte),
		modes:   make(map[string]string),
		results: make(map[string]localeExecStatus),
	}
	mail := &relayMailAPI{users: make(map[string]relayUserBody)}
	guestServer := httptest.NewServer(guest)
	t.Cleanup(guestServer.Close)
	mailServer := httptest.NewServer(mail)
	t.Cleanup(mailServer.Close)

	server := configuredProviderServer(
		t, guestServer.URL,
		tftypes.NewValue(tftypes.String, relayHeaderValue),
		tftypes.NewValue(tftypes.String, mailServer.URL),
	)
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[relayType]
	requireRelay(t, "resource_schema", found)
	return &relayHarness{server: server, resourceType: resourceSchema.ValueType(), guest: guest, mail: mail}
}

func (harness *relayHarness) value(t *testing.T, version int64, computed relayComputed) *tfprotov6.DynamicValue {
	t.Helper()
	resourceType := harness.resourceType
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":             tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":             tftypes.NewValue(tftypes.Number, optionsVMID),
		"kind":             tftypes.NewValue(tftypes.String, relayKind),
		"username":         tftypes.NewValue(tftypes.String, relayUsername),
		"description":      tftypes.NewValue(tftypes.String, relayDescription),
		"host":             tftypes.NewValue(tftypes.String, relayHost),
		"port":             tftypes.NewValue(tftypes.Number, relayPort),
		"from":             tftypes.NewValue(tftypes.String, relayFrom),
		"path":             computed.path,
		"password_version": tftypes.NewValue(tftypes.Number, version),
		"sha256":           computed.sha256,
		"write_id":         computed.writeID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func (harness *relayHarness) config(t *testing.T, version int64) *tfprotov6.DynamicValue {
	t.Helper()
	return harness.value(t, version, relayComputed{
		path:    tftypes.NewValue(tftypes.String, nil),
		sha256:  tftypes.NewValue(tftypes.String, nil),
		writeID: tftypes.NewValue(tftypes.String, nil),
	})
}

func (harness *relayHarness) attributes(t *testing.T, state *tfprotov6.DynamicValue) map[string]tftypes.Value {
	t.Helper()
	value, err := state.Unmarshal(harness.resourceType)
	if err != nil {
		t.Fatal(err)
	}
	if value.IsNull() {
		return nil
	}
	attributes := make(map[string]tftypes.Value)
	if err := value.As(&attributes); err != nil {
		t.Fatal(err)
	}
	return attributes
}

func (harness *relayHarness) text(t *testing.T, state *tfprotov6.DynamicValue, name string) string {
	t.Helper()
	var text string
	if err := harness.attributes(t, state)[name].As(&text); err != nil {
		t.Fatal(err)
	}
	return text
}

func (harness *relayHarness) equal(t *testing.T, left *tfprotov6.DynamicValue, right *tfprotov6.DynamicValue) bool {
	t.Helper()
	leftValue, err := left.Unmarshal(harness.resourceType)
	if err != nil {
		t.Fatal(err)
	}
	rightValue, err := right.Unmarshal(harness.resourceType)
	if err != nil {
		t.Fatal(err)
	}
	return leftValue.Equal(rightValue)
}

func (harness *relayHarness) plan(
	t *testing.T,
	prior *tfprotov6.DynamicValue,
	priorPrivate []byte,
	version int64,
) *tfprotov6.PlanResourceChangeResponse {
	t.Helper()
	proposed := relayComputed{
		path:    tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		sha256:  tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		writeID: tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	}
	priorAttributes := harness.attributes(t, prior)
	if priorAttributes != nil {
		proposed = relayComputed{
			path:    priorAttributes["path"],
			sha256:  priorAttributes["sha256"],
			writeID: priorAttributes["write_id"],
		}
	}
	plan, err := harness.server.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName:         relayType,
		PriorState:       prior,
		ProposedNewState: harness.value(t, version, proposed),
		Config:           harness.config(t, version),
		PriorPrivate:     priorPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.requireSecretFree(
		t, relayDynamicBytes(plan.PlannedState), plan.PlannedPrivate, relayDiagnosticBytes(plan.Diagnostics),
	)
	return plan
}

func (harness *relayHarness) apply(
	t *testing.T,
	prior *tfprotov6.DynamicValue,
	plan *tfprotov6.PlanResourceChangeResponse,
	version int64,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	applied, err := harness.server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:       relayType,
		PriorState:     prior,
		PlannedState:   plan.PlannedState,
		Config:         harness.config(t, version),
		PlannedPrivate: plan.PlannedPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.requireSecretFree(
		t, relayDynamicBytes(applied.NewState), applied.Private, relayDiagnosticBytes(applied.Diagnostics),
	)
	return applied
}

func (harness *relayHarness) planAndApply(
	t *testing.T,
	prior *tfprotov6.DynamicValue,
	priorPrivate []byte,
	version int64,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	plan := harness.plan(t, prior, priorPrivate, version)
	requireNoLocaleDiagnostics(t, plan.Diagnostics)
	requireRelay(t, "plan_without_replacement", len(plan.RequiresReplace) == 0)
	requireRelay(t, "plan_differs_from_prior", !harness.equal(t, plan.PlannedState, prior))
	applied := harness.apply(t, prior, plan, version)
	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	return applied
}

func (harness *relayHarness) create(t *testing.T) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	return harness.planAndApply(t, powerResourceNull(t, harness.resourceType), nil, 1)
}

func (harness *relayHarness) read(
	t *testing.T,
	state *tfprotov6.DynamicValue,
	private []byte,
) *tfprotov6.ReadResourceResponse {
	t.Helper()
	read, err := harness.server.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{
		TypeName:     relayType,
		CurrentState: state,
		Private:      private,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness.requireSecretFree(
		t, relayDynamicBytes(read.NewState), read.Private, relayDiagnosticBytes(read.Diagnostics),
	)
	return read
}

func (harness *relayHarness) requireSecretFree(t *testing.T, documents ...[]byte) {
	t.Helper()
	for index, secret := range harness.mail.secrets() {
		for _, document := range documents {
			if bytes.Contains(document, []byte(secret)) {
				t.Fatalf("A document contains the secret at index %d", index)
			}
		}
	}
}

func (harness *relayHarness) requireGuestFile(t *testing.T) {
	t.Helper()
	password := harness.mail.user(relayUsername).Generated
	want := fmt.Sprintf(relayFileFormat, relayHost, relayPort, relayUsername, password, relayFrom)
	content, found := harness.guest.file(relayPath)
	requireRelay(t, "guest_file_exists", found)
	requireRelay(t, "guest_file_content", string(content) == want)
	requireRelay(t, "guest_file_mode", harness.guest.mode(relayPath) == relayMode)
}

func (harness *relayHarness) requireCounts(t *testing.T, adds int, edits int, renames int) {
	t.Helper()
	gotAdds, gotEdits, writeRequests := harness.mail.counts()
	requireRelayCount(t, "adds", gotAdds, adds)
	requireRelayCount(t, "edits", gotEdits, edits)
	requireRelayCount(t, "write_requests", writeRequests, adds+2*edits)
	requireRelayCount(t, "renames", harness.guest.renameCount(), renames)
}

func requireRelay(t *testing.T, name string, condition bool) {
	t.Helper()
	if !condition {
		t.Fatalf("Condition %s is false", name)
	}
}

func requireRelayCount(t *testing.T, name string, got int, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("Counter %s is %d instead of %d", name, got, want)
	}
}

func requireRelayWarning(t *testing.T, diagnostics []*tfprotov6.Diagnostic, summary string) {
	t.Helper()
	requireRelayCount(t, "diagnostics", len(diagnostics), 1)
	requireRelay(t, "warning_severity", diagnostics[0].Severity == tfprotov6.DiagnosticSeverityWarning)
	requireRelay(t, "warning_summary", diagnostics[0].Summary == summary)
}

func relayDynamicBytes(value *tfprotov6.DynamicValue) []byte {
	if value == nil {
		return nil
	}
	return slices.Concat(value.MsgPack, value.JSON)
}

func relayDiagnosticBytes(diagnostics []*tfprotov6.Diagnostic) []byte {
	var text bytes.Buffer
	for _, diagnostic := range diagnostics {
		text.WriteString(diagnostic.Summary)
		text.WriteString(diagnostic.Detail)
	}
	return text.Bytes()
}

func relayHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestMailRelayFirstApplyCreatesUserAndWritesFile(t *testing.T) {
	harness := newRelayHarness(t)

	applied := harness.create(t)

	harness.requireCounts(t, 1, 0, 1)
	harness.requireGuestFile(t)
	user := harness.mail.user(relayUsername)
	requireRelay(t, "user_description", user.Description == relayDescription)
	requireRelayCount(t, "password_length", len(user.Generated), relayPasswordLength)
	content, _ := harness.guest.file(relayPath)
	requireRelay(t, "state_sha256", harness.text(t, applied.NewState, "sha256") == relayHash(content))
	requireRelay(t, "state_path", harness.text(t, applied.NewState, "path") == relayPath)
}

func TestMailRelayUnchangedPlanSendsNoWrite(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)

	read := harness.read(t, applied.NewState, applied.Private)
	requireNoLocaleDiagnostics(t, read.Diagnostics)
	requireRelay(t, "read_keeps_state", harness.equal(t, read.NewState, applied.NewState))
	plan := harness.plan(t, read.NewState, read.Private, 1)
	requireNoLocaleDiagnostics(t, plan.Diagnostics)

	requireRelay(t, "plan_equals_prior", harness.equal(t, plan.PlannedState, read.NewState))
	harness.requireCounts(t, 1, 0, 1)
}

func TestMailRelayHigherVersionRotatesPasswordOnce(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)
	firstPassword := harness.mail.user(relayUsername).Generated
	read := harness.read(t, applied.NewState, applied.Private)

	rotated := harness.planAndApply(t, read.NewState, read.Private, 2)

	harness.requireCounts(t, 1, 1, 2)
	harness.requireGuestFile(t)
	requireRelay(t, "password_changed", harness.mail.user(relayUsername).Generated != firstPassword)
	firstWriteID := harness.text(t, applied.NewState, "write_id")
	requireRelay(t, "write_id_changed", harness.text(t, rotated.NewState, "write_id") != firstWriteID)

	second := harness.read(t, rotated.NewState, rotated.Private)
	plan := harness.plan(t, second.NewState, second.Private, 2)
	requireRelay(t, "plan_equals_prior", harness.equal(t, plan.PlannedState, second.NewState))
}

func TestMailRelayLowerVersionFailsPlan(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)

	plan := harness.plan(t, applied.NewState, applied.Private, 0)

	requireRelayCount(t, "diagnostics", len(plan.Diagnostics), 1)
	requireRelay(t, "error_severity", plan.Diagnostics[0].Severity == tfprotov6.DiagnosticSeverityError)
	harness.requireCounts(t, 1, 0, 1)
}

func TestMailRelayDriftedFileRunsCreatePath(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)
	firstPassword := harness.mail.user(relayUsername).Generated
	harness.guest.setFile(relayPath, []byte(relayDriftedFile))

	read := harness.read(t, applied.NewState, applied.Private)
	requireNoLocaleDiagnostics(t, read.Diagnostics)
	driftedHash := relayHash([]byte(relayDriftedFile))
	requireRelay(t, "read_stores_guest_hash", harness.text(t, read.NewState, "sha256") == driftedHash)
	harness.planAndApply(t, read.NewState, read.Private, 1)

	harness.requireCounts(t, 1, 1, 2)
	harness.requireGuestFile(t)
	requireRelay(t, "password_changed", harness.mail.user(relayUsername).Generated != firstPassword)
}

func TestMailRelayMissingFilePlansCreate(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)
	harness.guest.removeFile(relayPath)

	read := harness.read(t, applied.NewState, applied.Private)

	requireNoLocaleDiagnostics(t, read.Diagnostics)
	requireRelay(t, "state_removed", harness.attributes(t, read.NewState) == nil)
}

func TestMailRelayMissingUserWarnsAndPlansCreate(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)
	harness.mail.removeUser(relayUsername)

	read := harness.read(t, applied.NewState, applied.Private)

	requireRelayWarning(t, read.Diagnostics, relayMissingUserSummary)
	requireRelay(t, "state_removed", harness.attributes(t, read.NewState) == nil)
	harness.planAndApply(t, read.NewState, nil, 1)
	harness.requireCounts(t, 2, 0, 2)
	harness.requireGuestFile(t)
}

func TestMailRelayBlockedUserWarnsWithoutPlannedChange(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)
	harness.mail.setStatus(relayUsername, relayBlocked)

	read := harness.read(t, applied.NewState, applied.Private)

	requireRelayWarning(t, read.Diagnostics, relayBlockedUserSummary)
	plan := harness.plan(t, read.NewState, read.Private, 1)
	requireRelay(t, "plan_equals_prior", harness.equal(t, plan.PlannedState, read.NewState))
	harness.requireCounts(t, 1, 0, 1)
}

func TestMailRelayFailedFileWriteConvergesOnRetry(t *testing.T) {
	harness := newRelayHarness(t)
	harness.guest.failNextRename()
	absent := powerResourceNull(t, harness.resourceType)

	plan := harness.plan(t, absent, nil, 1)
	failed := harness.apply(t, absent, plan, 1)

	requireRelayCount(t, "diagnostics", len(failed.Diagnostics), 1)
	requireRelay(t, "state_absent", harness.attributes(t, failed.NewState) == nil)
	_, found := harness.guest.file(relayPath)
	requireRelay(t, "guest_file_absent", !found)

	harness.planAndApply(t, failed.NewState, failed.Private, 1)
	harness.requireCounts(t, 1, 1, 1)
	harness.requireGuestFile(t)
}

func TestMailRelayFailedRotationPlansCreatePathAfterRevert(t *testing.T) {
	harness := newRelayHarness(t)
	applied := harness.create(t)
	read := harness.read(t, applied.NewState, applied.Private)
	harness.guest.failNextRename()

	plan := harness.plan(t, read.NewState, read.Private, 2)
	failed := harness.apply(t, read.NewState, plan, 2)

	requireRelayCount(t, "diagnostics", len(failed.Diagnostics), 1)
	requireRelay(t, "state_keeps_prior", harness.equal(t, failed.NewState, read.NewState))
	harness.requireCounts(t, 1, 1, 1)

	second := harness.read(t, failed.NewState, failed.Private)
	harness.planAndApply(t, second.NewState, second.Private, 1)
	harness.requireCounts(t, 1, 2, 2)
	harness.requireGuestFile(t)
}
