package provider_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
)

const (
	headerDownloadType     = "pveguest_download"
	headerDownloadKind     = "lxc"
	headerDownloadPath     = "/opt/tool/tool.bin"
	headerDownloadName     = "X-Pveguest-Token"
	headerDownloadValue    = "fixture-token-value-7f3a"
	headerDownloadOtherKey = "Accept"
	headerDownloadOtherVal = "application/octet-stream"
	headerDownloadObject   = "/object"
	headerDownloadRedirect = "/redirect"
	headerDownloadPayload  = "downloaded payload\n"
	headerDownloadCurl     = "curl"
	headerDownloadFailure  = "curl: (22) The requested URL returned error: 401 for " + headerDownloadValue
	headerDownloadGuestURL = "https://objects.example.net" + headerDownloadObject
	headerDownloadPEMType  = "CERTIFICATE"

	headerDownloadStatusRedirect = http.StatusFound
	headerDownloadCurlExit       = 22
	headerDownloadFirstVersion   = 1
	headerDownloadSecondVersion  = 2
	headerDownloadFileMode       = 0o600
	headerDownloadProgramIndex   = 2
	headerDownloadScriptIndex    = 2
)

type headerDownloadExecRequest struct {
	Command   []string `json:"command"`
	InputData string   `json:"input-data"`
}

func requireHeaderDownload(t *testing.T, name string, condition bool) {
	t.Helper()
	if !condition {
		t.Fatalf("Condition %s is false", name)
	}
}

func requireHeaderDownloadCount(t *testing.T, name string, got int, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("Counter %s is %d instead of %d", name, got, want)
	}
}

func headerDownloadDynamicBytes(value *tfprotov6.DynamicValue) []byte {
	if value == nil {
		return nil
	}
	return slices.Concat(value.MsgPack, value.JSON)
}

func headerDownloadDiagnosticBytes(diagnostics []*tfprotov6.Diagnostic) []byte {
	var text bytes.Buffer
	for _, diagnostic := range diagnostics {
		text.WriteString(diagnostic.Summary)
		text.WriteString(diagnostic.Detail)
	}
	return text.Bytes()
}

type headerDownloadSettings struct {
	url     string
	fetch   string
	headers map[string]string
	version int64
}

type headerDownloadComputed struct {
	mode       tftypes.Value
	owner      tftypes.Value
	group      tftypes.Value
	fileSHA256 tftypes.Value
	writeID    tftypes.Value
}

type headerDownloadCommand struct {
	argv  []string
	stdin []byte
}

type headerDownloadGuestAPI struct {
	mutex       sync.Mutex
	files       map[string][]byte
	objects     map[string][]byte
	commands    []headerDownloadCommand
	results     map[string]localeExecStatus
	curlFailure string
}

func (api *headerDownloadGuestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	switch {
	case request.Method == http.MethodPost && request.URL.Path == localesExecPath:
		var body headerDownloadExecRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		stdin, err := base64.StdEncoding.DecodeString(body.InputData)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		api.commands = append(api.commands, headerDownloadCommand{argv: body.Command, stdin: stdin})
		processID := len(api.commands)
		api.results[strconv.Itoa(processID)] = api.run(body.Command, stdin)
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

func headerDownloadOutput(exitCode int, stdout string, stderr string) localeExecStatus {
	return localeExecStatus{
		Exited:   1,
		ExitCode: exitCode,
		OutData:  base64.StdEncoding.EncodeToString([]byte(stdout)),
		ErrData:  base64.StdEncoding.EncodeToString([]byte(stderr)),
	}
}

func (api *headerDownloadGuestAPI) run(argv []string, stdin []byte) localeExecStatus {
	arguments := argv[headerDownloadProgramIndex:]
	target := arguments[len(arguments)-1]
	switch arguments[0] {
	case "sh":
		return api.shell(arguments[headerDownloadScriptIndex], arguments[headerDownloadScriptIndex+2:], stdin)
	case "chmod", "chown":
		return headerDownloadOutput(0, "", "")
	case "mv":
		api.files[target] = api.files[arguments[len(arguments)-2]]
		delete(api.files, arguments[len(arguments)-2])
		return headerDownloadOutput(0, "", "")
	case "sha256sum":
		content, found := api.files[target]
		if !found {
			return headerDownloadOutput(1, "", "sha256sum: "+target+": No such file or directory")
		}
		return headerDownloadOutput(0, headerDownloadHash(content)+"  "+target+"\n", "")
	case "rm":
		delete(api.files, target)
		return headerDownloadOutput(0, "", "")
	default:
		return headerDownloadOutput(unknownCommandExitCode, "", "unknown command")
	}
}

func (api *headerDownloadGuestAPI) shell(script string, parameters []string, stdin []byte) localeExecStatus {
	switch {
	case strings.Contains(script, headerDownloadCurl):
		if api.curlFailure != "" {
			return headerDownloadOutput(headerDownloadCurlExit, "", api.curlFailure)
		}
		content, found := api.objects[parameters[2]]
		if !found {
			return headerDownloadOutput(headerDownloadCurlExit, "", "curl: (22) not found")
		}
		api.files[parameters[1]] = content
		return headerDownloadOutput(0, "", "")
	case strings.Contains(script, "gzip -dc"):
		reader, err := gzip.NewReader(bytes.NewReader(api.files[parameters[0]]))
		if err != nil {
			return headerDownloadOutput(1, "", err.Error())
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			return headerDownloadOutput(1, "", err.Error())
		}
		api.files[parameters[1]] = content
		return headerDownloadOutput(0, "", "")
	case strings.Contains(script, "cat >"):
		api.files[parameters[0]] = stdin
		return headerDownloadOutput(0, "", "")
	case strings.Contains(script, "mkdir -p"):
		return headerDownloadOutput(0, "", "")
	default:
		return headerDownloadOutput(unknownCommandExitCode, "", "unknown script")
	}
}

func (api *headerDownloadGuestAPI) file(filePath string) ([]byte, bool) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	content, found := api.files[filePath]
	return slices.Clone(content), found
}

func (api *headerDownloadGuestAPI) curlCommands() []headerDownloadCommand {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	var curls []headerDownloadCommand
	for _, command := range api.commands {
		if strings.Contains(strings.Join(command.argv, " "), headerDownloadCurl) {
			curls = append(curls, command)
		}
	}
	return curls
}

func (api *headerDownloadGuestAPI) allArguments() []byte {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	var text bytes.Buffer
	for _, command := range api.commands {
		text.WriteString(strings.Join(command.argv, "\x00"))
	}
	return text.Bytes()
}

type headerDownloadHarness struct {
	server       tfprotov6.ProviderServer
	resourceType tftypes.Type
	guest        *headerDownloadGuestAPI
}

func headerDownloadHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func headerDownloadTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	pemBytes := []byte("-----BEGIN " + headerDownloadPEMType + "-----\n" +
		base64.StdEncoding.EncodeToString(server.Certificate().Raw) +
		"\n-----END " + headerDownloadPEMType + "-----\n")
	caFile := filepath.Join(t.TempDir(), "controller-ca.pem")
	if err := os.WriteFile(caFile, pemBytes, headerDownloadFileMode); err != nil {
		t.Fatal(err)
	}
	return server, caFile
}

func headerDownloadProvider(t *testing.T, endpoint string, caFile string) tfprotov6.ProviderServer {
	t.Helper()
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(provider.New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	schemaResponse, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	providerType := schemaResponse.Provider.ValueType()
	nodeType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"endpoint":          tftypes.String,
		"api_token":         tftypes.String,
		"insecure":          tftypes.Bool,
		"node_name":         tftypes.String,
		"max_exec_requests": tftypes.Number,
	}}
	node := tftypes.NewValue(nodeType, map[string]tftypes.Value{
		"endpoint":          tftypes.NewValue(tftypes.String, endpoint),
		"api_token":         tftypes.NewValue(tftypes.String, optionsToken),
		"insecure":          tftypes.NewValue(tftypes.Bool, nil),
		"node_name":         tftypes.NewValue(tftypes.String, nil),
		"max_exec_requests": tftypes.NewValue(tftypes.Number, nil),
	})
	providerConfig, err := tfprotov6.NewDynamicValue(providerType, tftypes.NewValue(providerType, map[string]tftypes.Value{
		"nodes": tftypes.NewValue(tftypes.Map{ElementType: nodeType}, map[string]tftypes.Value{
			optionsNode: node,
		}),
		"max_requests":       tftypes.NewValue(tftypes.Number, nil),
		"controller_ca_file": tftypes.NewValue(tftypes.String, caFile),
		"smtp2go_api_key":    tftypes.NewValue(tftypes.String, nil),
		"smtp2go_api_url":    tftypes.NewValue(tftypes.String, nil),
	}))
	if err != nil {
		t.Fatal(err)
	}
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: &providerConfig})
	if err != nil {
		t.Fatal(err)
	}
	if len(configured.Diagnostics) > 0 {
		t.Fatalf("configure provider: %v", configured.Diagnostics[0].Detail)
	}
	return server
}

func newHeaderDownloadHarness(t *testing.T, caFile string) *headerDownloadHarness {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	guest := &headerDownloadGuestAPI{
		files:   make(map[string][]byte),
		objects: make(map[string][]byte),
		results: make(map[string]localeExecStatus),
	}
	guestServer := httptest.NewServer(guest)
	t.Cleanup(guestServer.Close)
	server := headerDownloadProvider(t, guestServer.URL, caFile)
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[headerDownloadType]
	requireHeaderDownload(t, "resource_schema", found)
	return &headerDownloadHarness{server: server, resourceType: resourceSchema.ValueType(), guest: guest}
}

func headerDownloadMap(headers map[string]string) tftypes.Value {
	mapType := tftypes.Map{ElementType: tftypes.String}
	if headers == nil {
		return tftypes.NewValue(mapType, nil)
	}
	values := make(map[string]tftypes.Value, len(headers))
	for name, value := range headers {
		values[name] = tftypes.NewValue(tftypes.String, value)
	}
	return tftypes.NewValue(mapType, values)
}

func headerDownloadVersion(version int64) tftypes.Value {
	if version == 0 {
		return tftypes.NewValue(tftypes.Number, nil)
	}
	return tftypes.NewValue(tftypes.Number, version)
}

func (harness *headerDownloadHarness) value(
	t *testing.T,
	settings headerDownloadSettings,
	computed headerDownloadComputed,
) *tfprotov6.DynamicValue {
	t.Helper()
	resourceType := harness.resourceType
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":                       tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":                       tftypes.NewValue(tftypes.Number, optionsVMID),
		"kind":                       tftypes.NewValue(tftypes.String, headerDownloadKind),
		"url":                        tftypes.NewValue(tftypes.String, settings.url),
		"sha256":                     tftypes.NewValue(tftypes.String, headerDownloadHash([]byte(headerDownloadPayload))),
		"fetch":                      tftypes.NewValue(tftypes.String, settings.fetch),
		"archive_member":             tftypes.NewValue(tftypes.String, nil),
		"request_headers_wo":         headerDownloadMap(settings.headers),
		"request_headers_wo_version": headerDownloadVersion(settings.version),
		"path":                       tftypes.NewValue(tftypes.String, headerDownloadPath),
		"mode":                       computed.mode,
		"owner":                      computed.owner,
		"group":                      computed.group,
		"file_sha256":                computed.fileSHA256,
		"write_id":                   computed.writeID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func (harness *headerDownloadHarness) config(t *testing.T, settings headerDownloadSettings) *tfprotov6.DynamicValue {
	t.Helper()
	unset := tftypes.NewValue(tftypes.String, nil)
	return harness.value(t, settings, headerDownloadComputed{
		mode: unset, owner: unset, group: unset, fileSHA256: unset, writeID: unset,
	})
}

func (harness *headerDownloadHarness) attributes(t *testing.T, state *tfprotov6.DynamicValue) map[string]tftypes.Value {
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

func (harness *headerDownloadHarness) text(t *testing.T, state *tfprotov6.DynamicValue, name string) string {
	t.Helper()
	var text string
	if err := harness.attributes(t, state)[name].As(&text); err != nil {
		t.Fatal(err)
	}
	return text
}

func (harness *headerDownloadHarness) equal(t *testing.T, left *tfprotov6.DynamicValue, right *tfprotov6.DynamicValue) bool {
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

func (harness *headerDownloadHarness) validate(
	t *testing.T,
	settings headerDownloadSettings,
) []*tfprotov6.Diagnostic {
	t.Helper()
	response, err := harness.server.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
		TypeName:           headerDownloadType,
		Config:             harness.config(t, settings),
		ClientCapabilities: &tfprotov6.ValidateResourceConfigClientCapabilities{WriteOnlyAttributesAllowed: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response.Diagnostics
}

func (harness *headerDownloadHarness) plan(
	t *testing.T,
	prior *tfprotov6.DynamicValue,
	priorPrivate []byte,
	settings headerDownloadSettings,
) *tfprotov6.PlanResourceChangeResponse {
	t.Helper()
	proposed := headerDownloadComputed{
		mode:       tftypes.NewValue(tftypes.String, nil),
		owner:      tftypes.NewValue(tftypes.String, nil),
		group:      tftypes.NewValue(tftypes.String, nil),
		fileSHA256: tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		writeID:    tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	}
	priorAttributes := harness.attributes(t, prior)
	if priorAttributes != nil {
		proposed = headerDownloadComputed{
			mode:       priorAttributes["mode"],
			owner:      priorAttributes["owner"],
			group:      priorAttributes["group"],
			fileSHA256: priorAttributes["file_sha256"],
			writeID:    priorAttributes["write_id"],
		}
	}
	plan, err := harness.server.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName:         headerDownloadType,
		PriorState:       prior,
		ProposedNewState: harness.value(t, settings, proposed),
		Config:           harness.config(t, settings),
		PriorPrivate:     priorPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireHeaderDownloadClean(
		t, headerDownloadDynamicBytes(plan.PlannedState), plan.PlannedPrivate,
		headerDownloadDiagnosticBytes(plan.Diagnostics),
	)
	return plan
}

func (harness *headerDownloadHarness) apply(
	t *testing.T,
	prior *tfprotov6.DynamicValue,
	plan *tfprotov6.PlanResourceChangeResponse,
	settings headerDownloadSettings,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	applied, err := harness.server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:       headerDownloadType,
		PriorState:     prior,
		PlannedState:   plan.PlannedState,
		Config:         harness.config(t, settings),
		PlannedPrivate: plan.PlannedPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireHeaderDownloadClean(
		t, headerDownloadDynamicBytes(applied.NewState), applied.Private,
		headerDownloadDiagnosticBytes(applied.Diagnostics),
	)
	return applied
}

func (harness *headerDownloadHarness) create(
	t *testing.T,
	settings headerDownloadSettings,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	prior := powerResourceNull(t, harness.resourceType)
	plan := harness.plan(t, prior, nil, settings)
	requireNoLocaleDiagnostics(t, plan.Diagnostics)
	applied := harness.apply(t, prior, plan, settings)
	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	return applied
}

func (harness *headerDownloadHarness) requireInstalled(t *testing.T) {
	t.Helper()
	content, found := harness.guest.file(headerDownloadPath)
	requireHeaderDownload(t, "installed_file_exists", found)
	requireHeaderDownload(t, "installed_file_content", string(content) == headerDownloadPayload)
}

func requireHeaderDownloadClean(t *testing.T, documents ...[]byte) {
	t.Helper()
	for index, document := range documents {
		if bytes.Contains(document, []byte(headerDownloadValue)) {
			t.Fatalf("Document %d contains the header value", index)
		}
	}
}

func headerDownloadHeaders() map[string]string {
	return map[string]string{headerDownloadName: headerDownloadValue, headerDownloadOtherKey: headerDownloadOtherVal}
}

func headerDownloadControllerSettings(url string, version int64) headerDownloadSettings {
	return headerDownloadSettings{
		url: url, fetch: "controller", headers: headerDownloadHeaders(), version: version,
	}
}

func headerDownloadGuestSettings(headers map[string]string) headerDownloadSettings {
	return headerDownloadSettings{
		url: headerDownloadGuestURL, fetch: "guest", headers: headers, version: headerDownloadFirstVersion,
	}
}

type headerDownloadObjectServer struct {
	mutex    sync.Mutex
	received []string
	target   string
}

func (api *headerDownloadObjectServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	token := request.Header.Get(headerDownloadName)
	api.mutex.Lock()
	api.received = append(api.received, token)
	api.mutex.Unlock()
	switch request.URL.Path {
	case headerDownloadRedirect:
		http.Redirect(writer, request, api.target, headerDownloadStatusRedirect)
	case headerDownloadObject:
		if token != headerDownloadValue {
			http.Error(writer, "missing token", http.StatusUnauthorized)
			return
		}
		_, _ = writer.Write([]byte(headerDownloadPayload))
	default:
		http.NotFound(writer, request)
	}
}

func (api *headerDownloadObjectServer) tokens() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return slices.Clone(api.received)
}

type headerDownloadOpenServer struct {
	mutex    sync.Mutex
	received []string
}

func (api *headerDownloadOpenServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	api.received = append(api.received, request.Header.Get(headerDownloadName))
	api.mutex.Unlock()
	_, _ = writer.Write([]byte(headerDownloadPayload))
}

func (api *headerDownloadOpenServer) tokens() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return slices.Clone(api.received)
}

type lockedLogBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (log *lockedLogBuffer) Write(content []byte) (int, error) {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return log.buffer.Write(content)
}

func (log *lockedLogBuffer) contents() []byte {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return slices.Clone(log.buffer.Bytes())
}

func TestDownloadControllerSendsRequestHeaders(t *testing.T) {
	objects := &headerDownloadObjectServer{}
	server, caFile := headerDownloadTLSServer(t, objects)
	harness := newHeaderDownloadHarness(t, caFile)
	settings := headerDownloadControllerSettings(server.URL+headerDownloadObject, headerDownloadFirstVersion)

	applied := harness.create(t, settings)

	harness.requireInstalled(t)
	attributes := harness.attributes(t, applied.NewState)
	requireHeaderDownload(t, "server_received_header", slices.Contains(objects.tokens(), headerDownloadValue))
	requireHeaderDownload(t, "state_file_hash", harness.text(t, applied.NewState, "file_sha256") == headerDownloadHash([]byte(headerDownloadPayload)))
	requireHeaderDownload(t, "state_headers_null", attributes["request_headers_wo"].IsNull())
	requireHeaderDownload(t, "state_keeps_version", !attributes["request_headers_wo_version"].IsNull())
	requireHeaderDownload(t, "guest_arguments_without_header", !bytes.Contains(harness.guest.allArguments(), []byte(headerDownloadValue)))
}

func TestDownloadVersionChangePlansRewrite(t *testing.T) {
	objects := &headerDownloadObjectServer{}
	server, caFile := headerDownloadTLSServer(t, objects)
	harness := newHeaderDownloadHarness(t, caFile)
	settings := headerDownloadControllerSettings(server.URL+headerDownloadObject, headerDownloadFirstVersion)
	applied := harness.create(t, settings)

	unchanged := harness.plan(t, applied.NewState, applied.Private, settings)
	requireNoLocaleDiagnostics(t, unchanged.Diagnostics)
	requireHeaderDownload(t, "same_version_plans_no_change", harness.equal(t, unchanged.PlannedState, applied.NewState))

	rotated := settings
	rotated.headers = map[string]string{headerDownloadName: headerDownloadValue + "-rotated"}
	valueOnly := harness.plan(t, applied.NewState, applied.Private, rotated)
	requireNoLocaleDiagnostics(t, valueOnly.Diagnostics)
	requireHeaderDownload(t, "header_value_alone_plans_no_change", harness.equal(t, valueOnly.PlannedState, applied.NewState))

	rotated.version = headerDownloadSecondVersion
	versioned := harness.plan(t, applied.NewState, applied.Private, rotated)
	requireNoLocaleDiagnostics(t, versioned.Diagnostics)
	requireHeaderDownload(t, "version_change_plans_rewrite", !harness.equal(t, versioned.PlannedState, applied.NewState))
	requireHeaderDownload(t, "version_change_without_replacement", len(versioned.RequiresReplace) == 0)
	requireHeaderDownload(t, "version_change_leaves_hash_unknown", !harness.attributes(t, versioned.PlannedState)["file_sha256"].IsKnown())
}

func TestDownloadControllerRedirectToOtherHostDropsHeaders(t *testing.T) {
	target := &headerDownloadOpenServer{}
	targetServer, caFile := headerDownloadTLSServer(t, target)
	origin := &headerDownloadObjectServer{target: targetServer.URL + headerDownloadObject}
	originServer, _ := headerDownloadTLSServer(t, origin)
	harness := newHeaderDownloadHarness(t, caFile)
	settings := headerDownloadControllerSettings(originServer.URL+headerDownloadRedirect, headerDownloadFirstVersion)

	harness.create(t, settings)

	harness.requireInstalled(t)
	requireHeaderDownload(t, "origin_received_header", slices.Contains(origin.tokens(), headerDownloadValue))
	requireHeaderDownload(t, "target_received_no_header", slices.Equal(target.tokens(), []string{""}))
}

func TestDownloadControllerRedirectToSameOriginKeepsHeaders(t *testing.T) {
	origin := &headerDownloadObjectServer{}
	originServer, caFile := headerDownloadTLSServer(t, origin)
	origin.target = originServer.URL + headerDownloadObject
	harness := newHeaderDownloadHarness(t, caFile)
	settings := headerDownloadControllerSettings(originServer.URL+headerDownloadRedirect, headerDownloadFirstVersion)

	harness.create(t, settings)

	harness.requireInstalled(t)
	requireHeaderDownload(t, "both_requests_send_header", slices.Equal(origin.tokens(), []string{headerDownloadValue, headerDownloadValue}))
}

func TestDownloadGuestSendsHeadersThroughStandardInput(t *testing.T) {
	harness := newHeaderDownloadHarness(t, "")
	harness.guest.objects[headerDownloadGuestURL] = []byte(headerDownloadPayload)

	harness.create(t, headerDownloadGuestSettings(headerDownloadHeaders()))

	harness.requireInstalled(t)
	curls := harness.guest.curlCommands()
	requireHeaderDownloadCount(t, "curl_commands", len(curls), 1)
	arguments := harness.guest.allArguments()
	requireHeaderDownload(t, "arguments_without_header_value", !bytes.Contains(arguments, []byte(headerDownloadValue)))
	requireHeaderDownload(t, "arguments_without_header_name", !bytes.Contains(arguments, []byte(headerDownloadName)))
	requireHeaderDownload(t, "curl_reads_standard_input", strings.Contains(strings.Join(curls[0].argv, " "), "--config -"))
	wantConfig := "header = \"" + headerDownloadOtherKey + ": " + headerDownloadOtherVal + "\"\n" +
		"header = \"" + headerDownloadName + ": " + headerDownloadValue + "\"\n"
	requireHeaderDownload(t, "standard_input_has_headers", string(curls[0].stdin) == wantConfig)
}

func TestDownloadGuestEscapesHeaderValueInCurlConfiguration(t *testing.T) {
	harness := newHeaderDownloadHarness(t, "")
	harness.guest.objects[headerDownloadGuestURL] = []byte(headerDownloadPayload)

	harness.create(t, headerDownloadGuestSettings(map[string]string{headerDownloadName: `a"b\c`}))

	curls := harness.guest.curlCommands()
	requireHeaderDownloadCount(t, "curl_commands", len(curls), 1)
	wantConfig := "header = \"" + headerDownloadName + ": a\\\"b\\\\c\"\n"
	requireHeaderDownload(t, "escaped_configuration", string(curls[0].stdin) == wantConfig)
}

func TestDownloadGuestFailureRedactsHeaderValue(t *testing.T) {
	logs := &lockedLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	harness := newHeaderDownloadHarness(t, "")
	harness.guest.curlFailure = headerDownloadFailure
	settings := headerDownloadGuestSettings(headerDownloadHeaders())
	prior := powerResourceNull(t, harness.resourceType)
	plan := harness.plan(t, prior, nil, settings)

	applied := harness.apply(t, prior, plan, settings)

	requireHeaderDownloadCount(t, "diagnostics", len(applied.Diagnostics), 1)
	requireHeaderDownload(t, "diagnostic_redacts_value", strings.Contains(applied.Diagnostics[0].Detail, "[redacted]"))
	requireHeaderDownload(t, "diagnostic_keeps_status", strings.Contains(applied.Diagnostics[0].Detail, "returned error: 401"))
	requireHeaderDownloadClean(t, logs.contents())
	_, installed := harness.guest.file(headerDownloadPath)
	requireHeaderDownload(t, "no_file_installed", !installed)
}

func TestDownloadRejectsInvalidRequestHeaders(t *testing.T) {
	harness := newHeaderDownloadHarness(t, "")
	valid := headerDownloadControllerSettings(headerDownloadGuestURL, headerDownloadFirstVersion)
	requireNoLocaleDiagnostics(t, harness.validate(t, valid))

	badValue := valid
	badValue.headers = map[string]string{headerDownloadName: headerDownloadValue + "\r\nX-Injected: yes"}
	badValueDiagnostics := harness.validate(t, badValue)
	requireHeaderDownloadCount(t, "bad_value_diagnostics", len(badValueDiagnostics), 1)
	requireHeaderDownload(t, "bad_value_names_header", strings.Contains(badValueDiagnostics[0].Detail, headerDownloadName))
	requireHeaderDownloadClean(t, headerDownloadDiagnosticBytes(badValueDiagnostics))

	badName := valid
	badName.headers = map[string]string{"Bad Name": headerDownloadValue}
	badNameDiagnostics := harness.validate(t, badName)
	requireHeaderDownloadCount(t, "bad_name_diagnostics", len(badNameDiagnostics), 1)
	requireHeaderDownloadClean(t, headerDownloadDiagnosticBytes(badNameDiagnostics))

	withoutVersion := valid
	withoutVersion.version = 0
	withoutVersionDiagnostics := harness.validate(t, withoutVersion)
	requireHeaderDownload(t, "missing_version_rejected", len(withoutVersionDiagnostics) > 0)
	requireHeaderDownloadClean(t, headerDownloadDiagnosticBytes(withoutVersionDiagnostics))

	withoutHeaders := valid
	withoutHeaders.headers = nil
	requireHeaderDownload(t, "version_without_headers_rejected", len(harness.validate(t, withoutHeaders)) > 0)
}
