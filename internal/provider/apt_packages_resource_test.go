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
	aptType = "pveguest_apt_packages"

	aptEnvironment = "env LC_ALL=C DEBIAN_FRONTEND=noninteractive apt-get"

	plainQueryCommand   = `env LC_ALL=C dpkg-query --show --showformat ${Package}\t${db:Status-Abbrev}\n -- `
	versionQueryCommand = `env LC_ALL=C dpkg-query --show --showformat ` +
		`${Package}\t${db:Status-Abbrev}\t${Version}\n -- `

	updateCommand = aptEnvironment + " update -o APT::Update::Error-Mode=any"

	aptCurl       = "curl"
	aptWget       = "wget"
	aptOldVersion = "7.0-1"
	aptNewVersion = "8.0-1"

	dpkgNoMatchExitCode = 1

	policyPath          = "/usr/sbin/policy-rc.d"
	policyContent       = "#!/bin/sh\nexit 101\n"
	policyMode          = "0755"
	policyExistsCommand = `env LC_ALL=C sh -c test -e "$1" || test -L "$1" sh ` + policyPath
	policyCreateCommand = `env LC_ALL=C sh -c set -C && cat > "$1" sh ` + policyPath
	policyModeCommand   = "env LC_ALL=C chmod " + policyMode + " -- " + policyPath
	policyRemoveCommand = "env LC_ALL=C rm -f -- " + policyPath

	aptInstallFailureExitCode = 100
	existingPolicyContent     = "#!/bin/sh\nexit 0\n"
	aptNoPolicy               = "absent"
	policyAtInstallExpected   = policyContent + "mode=" + policyMode
	aptInstallPrefix          = aptEnvironment + " install -y --no-install-recommends -o DPkg::Lock::Timeout=300 -- "
)

type aptExecRequest struct {
	Command   []string `json:"command"`
	InputData string   `json:"input-data"`
}

// aptDebFile stores the package name and version of one .deb file in the fake guest.
type aptDebFile struct {
	name    string
	version string
}

type aptGuestAPI struct {
	mutex       sync.Mutex
	installed   map[string]string
	held        map[string]bool
	available   map[string]string
	debs        map[string]aptDebFile
	files       map[string]string
	modes       map[string]string
	failInstall bool
	commands    []string
	results     map[string]localeExecStatus

	// policyAtInstall records the policy-rc.d content and mode at each apt-get install.
	// It records aptNoPolicy when the file does not exist.
	policyAtInstall []string
}

func newAptGuest(installed map[string]string, available map[string]string) *aptGuestAPI {
	return &aptGuestAPI{
		installed: installed,
		held:      map[string]bool{},
		available: available,
		debs:      map[string]aptDebFile{},
		files:     map[string]string{},
		modes:     map[string]string{},
	}
}

func (api *aptGuestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	switch {
	case request.Method == http.MethodPost && request.URL.Path == localesExecPath:
		var body aptExecRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		stdin, err := base64.StdEncoding.DecodeString(body.InputData)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		api.commands = append(api.commands, strings.Join(body.Command, " "))
		processID := len(api.commands)
		if api.results == nil {
			api.results = make(map[string]localeExecStatus)
		}
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

func aptOutput(exitCode int, stdout string, stderr string) localeExecStatus {
	return localeExecStatus{
		Exited:   1,
		ExitCode: exitCode,
		OutData:  base64.StdEncoding.EncodeToString([]byte(stdout)),
		ErrData:  base64.StdEncoding.EncodeToString([]byte(stderr)),
	}
}

// run simulates guest commands using the package and file maps.
func (api *aptGuestAPI) run(argv []string, stdin []byte) localeExecStatus {
	arguments := argv
	for len(arguments) > 0 && (arguments[0] == "env" || strings.Contains(arguments[0], "=")) {
		arguments = arguments[1:]
	}
	if len(arguments) == 0 {
		return aptOutput(unknownCommandExitCode, "", "empty command")
	}
	_, operands, _ := strings.Cut(strings.Join(arguments[1:], " "), " -- ")
	names := strings.Fields(operands)
	switch arguments[0] {
	case "dpkg-query":
		return api.query(arguments, names)
	case "apt-get":
		return api.aptGet(arguments[1], names)
	case "apt-mark":
		return api.aptMark(arguments[1], names)
	case "dpkg-deb":
		return api.debVersion(names[0])
	case "sh":
		return api.shell(arguments[2], arguments[4], stdin)
	case "chmod":
		return api.chmod(names[0], arguments[1])
	case "rm":
		delete(api.files, names[0])
		delete(api.modes, names[0])
		return aptOutput(0, "", "")
	default:
		return aptOutput(unknownCommandExitCode, "", "unknown command")
	}
}

func (api *aptGuestAPI) debVersion(debPath string) localeExecStatus {
	deb, found := api.debs[debPath]
	if !found {
		return aptOutput(1, "", "dpkg-deb: error: cannot open "+debPath+": No such file or directory")
	}
	return aptOutput(0, deb.version+"\n", "")
}

// shell simulates the policy-rc.d existence check and creation script.
// The creation script fails if the file exists, as a shell with noclobber set would.
func (api *aptGuestAPI) shell(script string, target string, stdin []byte) localeExecStatus {
	_, exists := api.files[target]
	switch {
	case strings.Contains(script, "test -e"):
		if exists {
			return aptOutput(0, "", "")
		}
		return aptOutput(1, "", "")
	case strings.Contains(script, "set -C"):
		if exists {
			return aptOutput(1, "", "sh: cannot create "+target+": File exists")
		}
		api.files[target] = string(stdin)
		return aptOutput(0, "", "")
	default:
		return aptOutput(unknownCommandExitCode, "", "unknown script")
	}
}

func (api *aptGuestAPI) chmod(target string, mode string) localeExecStatus {
	if _, exists := api.files[target]; !exists {
		return aptOutput(1, "", "chmod: cannot access "+target+": No such file or directory")
	}
	api.modes[target] = mode
	return aptOutput(0, "", "")
}

func (api *aptGuestAPI) query(arguments []string, names []string) localeExecStatus {
	format := arguments[slices.Index(arguments, "--showformat")+1]
	withVersion := strings.Contains(format, "${Version}")
	var stdout strings.Builder
	exitCode := 0
	stderr := ""
	for _, name := range names {
		version, isInstalled := api.installed[name]
		if !isInstalled {
			exitCode = dpkgNoMatchExitCode
			stderr = "dpkg-query: no packages found matching " + name
			continue
		}
		status := "ii "
		if api.held[name] {
			status = "hi "
		}
		line := name + "\t" + status
		if withVersion {
			line += "\t" + version
		}
		stdout.WriteString(line + "\n")
	}
	return aptOutput(exitCode, stdout.String(), stderr)
}

func (api *aptGuestAPI) aptGet(verb string, specs []string) localeExecStatus {
	switch verb {
	case "install":
		policy, hasPolicy := api.files[policyPath]
		if hasPolicy {
			api.policyAtInstall = append(api.policyAtInstall, policy+"mode="+api.modes[policyPath])
		} else {
			api.policyAtInstall = append(api.policyAtInstall, aptNoPolicy)
		}
		if api.failInstall {
			return aptOutput(aptInstallFailureExitCode, "", "E: Sub-process /usr/bin/dpkg returned an error code (1)")
		}
		for _, spec := range specs {
			deb, isDeb := api.debs[spec]
			if isDeb {
				api.installed[deb.name] = deb.version
				continue
			}
			name, version, isPinned := strings.Cut(spec, "=")
			if !isPinned {
				version = api.available[name]
			}
			api.installed[name] = version
		}
	case "remove":
		for _, name := range specs {
			delete(api.installed, name)
		}
	}
	return aptOutput(0, "", "")
}

func (api *aptGuestAPI) aptMark(verb string, names []string) localeExecStatus {
	switch verb {
	case "hold":
		for _, name := range names {
			api.held[name] = true
		}
	case "unhold":
		for _, name := range names {
			delete(api.held, name)
		}
	case "showhold":
		held := make([]string, 0, len(api.held))
		for name := range api.held {
			held = append(held, name)
		}
		slices.Sort(held)
		return aptOutput(0, strings.Join(held, "\n")+"\n", "")
	}
	return aptOutput(0, "", "")
}

func (api *aptGuestAPI) recordedCommands() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return slices.Clone(api.commands)
}

func (api *aptGuestAPI) resetCommands() {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.commands = nil
}

func (api *aptGuestAPI) setInstalled(name string, version string) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.installed[name] = version
}

func (api *aptGuestAPI) clearHolds() {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	api.held = map[string]bool{}
}

func (api *aptGuestAPI) installedVersion(name string) (string, bool) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	version, isInstalled := api.installed[name]
	return version, isInstalled
}

func (api *aptGuestAPI) guestFile(path string) (string, bool) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	content, exists := api.files[path]
	return content, exists
}

func (api *aptGuestAPI) policyAtInstalls() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return slices.Clone(api.policyAtInstall)
}

func (api *aptGuestAPI) isHeld(name string) bool {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return api.held[name]
}

func aptServer(t *testing.T, guest *aptGuestAPI) (tfprotov6.ProviderServer, tftypes.Type) {
	t.Helper()
	api := httptest.NewServer(guest)
	t.Cleanup(api.Close)
	server := configuredOptionsServer(t, api.URL)
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[aptType]
	if !found {
		t.Fatalf("Provider schema has no resource of this type: %s", aptType)
	}
	return server, resourceSchema.ValueType()
}

// aptSpec stores test inputs and distinguishes null attributes from empty
// collections and false.
type aptSpec struct {
	packages       []string
	versions       map[string]string
	locked         []string
	removeOnDelete *bool
	startServices  *bool
}

func aptStringSet(names []string) tftypes.Value {
	setType := tftypes.Set{ElementType: tftypes.String}
	if names == nil {
		return tftypes.NewValue(setType, nil)
	}
	members := make([]tftypes.Value, 0, len(names))
	for _, name := range names {
		members = append(members, tftypes.NewValue(tftypes.String, name))
	}
	return tftypes.NewValue(setType, members)
}

func aptValue(t *testing.T, resourceType tftypes.Type, spec aptSpec) *tfprotov6.DynamicValue {
	t.Helper()
	versions := tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)
	if spec.versions != nil {
		versions = stringMap(spec.versions)
	}
	removeOnDelete := tftypes.NewValue(tftypes.Bool, nil)
	if spec.removeOnDelete != nil {
		removeOnDelete = tftypes.NewValue(tftypes.Bool, *spec.removeOnDelete)
	}
	startServices := tftypes.NewValue(tftypes.Bool, nil)
	if spec.startServices != nil {
		startServices = tftypes.NewValue(tftypes.Bool, *spec.startServices)
	}
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":             tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":             tftypes.NewValue(tftypes.Number, optionsVMID),
		"kind":             tftypes.NewValue(tftypes.String, localesKind),
		"packages":         aptStringSet(spec.packages),
		"versions":         versions,
		"locked":           aptStringSet(spec.locked),
		"remove_on_delete": removeOnDelete,
		"start_services":   startServices,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func aptAttributes(t *testing.T, resourceType tftypes.Type, state *tfprotov6.DynamicValue) map[string]tftypes.Value {
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

func aptSetMembers(t *testing.T, value tftypes.Value) []string {
	t.Helper()
	var members []tftypes.Value
	if err := value.As(&members); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(members))
	for _, member := range members {
		var name string
		if err := member.As(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func aptMapMembers(t *testing.T, value tftypes.Value) map[string]string {
	t.Helper()
	var members map[string]tftypes.Value
	if err := value.As(&members); err != nil {
		t.Fatal(err)
	}
	result := make(map[string]string, len(members))
	for key, member := range members {
		var text string
		if err := member.As(&text); err != nil {
			t.Fatal(err)
		}
		result[key] = text
	}
	return result
}

func aptApply(
	t *testing.T,
	server tfprotov6.ProviderServer,
	prior *tfprotov6.DynamicValue,
	planned *tfprotov6.DynamicValue,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	applied, err := server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     aptType,
		PriorState:   prior,
		PlannedState: planned,
		Config:       planned,
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, applied.Diagnostics)
	return applied
}

func aptRead(t *testing.T, server tfprotov6.ProviderServer, state *tfprotov6.DynamicValue) *tfprotov6.DynamicValue {
	t.Helper()
	read, err := server.ReadResource(context.Background(),
		&tfprotov6.ReadResourceRequest{TypeName: aptType, CurrentState: state})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, read.Diagnostics)
	return read.NewState
}

func requireAptCommands(t *testing.T, guest *aptGuestAPI, want ...string) {
	t.Helper()
	got := guest.recordedCommands()
	if !slices.Equal(got, want) {
		t.Fatalf("Guest commands differ from the expected sequence (received, expected):\n%q\n%q", got, want)
	}
}

func requireApplyError(t *testing.T, diagnostics []*tfprotov6.Diagnostic, text string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		isError := diagnostic.Severity == tfprotov6.DiagnosticSeverityError
		if isError && strings.Contains(diagnostic.Detail, text) {
			return
		}
	}
	t.Fatalf("Apply diagnostics %v have no error that contains %q", diagnostics, text)
}

func aptApplyWithError(
	t *testing.T,
	server tfprotov6.ProviderServer,
	planned *tfprotov6.DynamicValue,
	resourceType tftypes.Type,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	applied, err := server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     aptType,
		PriorState:   powerResourceNull(t, resourceType),
		PlannedState: planned,
		Config:       planned,
	})
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

func TestAptPackagesStartServicesFalseBlocksStartsOnlyDuringInstall(t *testing.T) {
	disabled := false
	guest := newAptGuest(map[string]string{}, map[string]string{aptCurl: aptOldVersion})
	server, resourceType := aptServer(t, guest)

	planned := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl}, startServices: &disabled})
	applied := aptApply(t, server, powerResourceNull(t, resourceType), planned)

	requireAptCommands(t, guest,
		plainQueryCommand+aptCurl,
		updateCommand,
		policyExistsCommand,
		policyCreateCommand,
		policyModeCommand,
		aptInstallPrefix+aptCurl,
		policyRemoveCommand,
		plainQueryCommand+aptCurl,
	)
	if got := guest.policyAtInstalls(); !slices.Equal(got, []string{policyAtInstallExpected}) {
		t.Fatalf("Policy file at each install command is %q, want %q", got, []string{policyAtInstallExpected})
	}
	if _, exists := guest.guestFile(policyPath); exists {
		t.Fatal("Guest still has the policy file after the apply.")
	}
	attributes := aptAttributes(t, resourceType, applied.NewState)
	if !attributes["start_services"].Equal(tftypes.NewValue(tftypes.Bool, false)) {
		t.Fatalf("State start_services is %s, want false.", attributes["start_services"])
	}
}

func TestAptPackagesStartServicesFalseRefusesExistingPolicyFile(t *testing.T) {
	disabled := false
	guest := newAptGuest(map[string]string{}, map[string]string{aptCurl: aptOldVersion})
	guest.files[policyPath] = existingPolicyContent
	server, resourceType := aptServer(t, guest)

	planned := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl}, startServices: &disabled})
	applied := aptApplyWithError(t, server, planned, resourceType)

	requireApplyError(t, applied.Diagnostics, policyPath+" already exists")
	requireAptCommands(t, guest,
		plainQueryCommand+aptCurl,
		updateCommand,
		policyExistsCommand,
	)
	content, exists := guest.guestFile(policyPath)
	if !exists || content != existingPolicyContent {
		t.Fatalf("Guest policy file is %q (exists %t), want the original content.", content, exists)
	}
	if _, isInstalled := guest.installedVersion(aptCurl); isInstalled {
		t.Fatal("Guest installed curl although the apply refused to run.")
	}
}

func TestAptPackagesStartServicesFalseRemovesPolicyFileAfterFailedInstall(t *testing.T) {
	disabled := false
	guest := newAptGuest(map[string]string{}, map[string]string{aptCurl: aptOldVersion})
	guest.failInstall = true
	server, resourceType := aptServer(t, guest)

	planned := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl}, startServices: &disabled})
	applied := aptApplyWithError(t, server, planned, resourceType)

	requireApplyError(t, applied.Diagnostics, "returned an error code")
	requireAptCommands(t, guest,
		plainQueryCommand+aptCurl,
		updateCommand,
		policyExistsCommand,
		policyCreateCommand,
		policyModeCommand,
		aptInstallPrefix+aptCurl,
		policyRemoveCommand,
	)
	if _, exists := guest.guestFile(policyPath); exists {
		t.Fatal("Guest still has the policy file after the failed install.")
	}
}

func TestAptPackagesWithoutNewAttributesRunsOriginalCommands(t *testing.T) {
	guest := newAptGuest(map[string]string{}, map[string]string{aptCurl: aptOldVersion})
	server, resourceType := aptServer(t, guest)

	planned := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl}})
	applied := aptApply(t, server, powerResourceNull(t, resourceType), planned)

	requireAptCommands(t, guest,
		plainQueryCommand+aptCurl,
		updateCommand,
		aptEnvironment+" install -y --no-install-recommends -o DPkg::Lock::Timeout=300 -- "+aptCurl,
		plainQueryCommand+aptCurl,
	)
	attributes := aptAttributes(t, resourceType, applied.NewState)
	if !attributes["versions"].IsNull() || !attributes["locked"].IsNull() || !attributes["remove_on_delete"].IsNull() {
		t.Fatal("The new attributes are not null in state although the configuration omits them.")
	}

	guest.resetCommands()
	aptApply(t, server, applied.NewState, powerResourceNull(t, resourceType))
	requireAptCommands(t, guest)
}

func TestAptPackagesInstallsExactVersionAndReportsDrift(t *testing.T) {
	guest := newAptGuest(map[string]string{aptCurl: aptOldVersion}, map[string]string{aptCurl: aptOldVersion})
	server, resourceType := aptServer(t, guest)

	spec := aptSpec{packages: []string{aptCurl}, versions: map[string]string{aptCurl: aptNewVersion}}
	applied := aptApply(t, server, powerResourceNull(t, resourceType), aptValue(t, resourceType, spec))

	requireAptCommands(t, guest,
		versionQueryCommand+aptCurl,
		updateCommand,
		aptEnvironment+" install -y --no-install-recommends --allow-downgrades --allow-change-held-packages"+
			" -o DPkg::Lock::Timeout=300 -- "+aptCurl+"="+aptNewVersion,
		versionQueryCommand+aptCurl,
	)
	version, _ := guest.installedVersion(aptCurl)
	if version != aptNewVersion {
		t.Fatalf("Guest has curl %s, want %s", version, aptNewVersion)
	}

	guest.setInstalled(aptCurl, aptOldVersion)
	read := aptRead(t, server, applied.NewState)
	reported := aptMapMembers(t, aptAttributes(t, resourceType, read)["versions"])
	if reported[aptCurl] != aptOldVersion {
		t.Fatalf("State reports curl %s after a manual downgrade, want %s", reported[aptCurl], aptOldVersion)
	}
}

func TestAptPackagesLocksAndReportsManualUnhold(t *testing.T) {
	guest := newAptGuest(map[string]string{aptCurl: aptOldVersion}, map[string]string{})
	server, resourceType := aptServer(t, guest)

	spec := aptSpec{packages: []string{aptCurl}, locked: []string{aptCurl}}
	applied := aptApply(t, server, powerResourceNull(t, resourceType), aptValue(t, resourceType, spec))
	if !guest.isHeld(aptCurl) {
		t.Fatal("Guest did not hold curl after apply.")
	}
	requireAptCommands(t, guest,
		plainQueryCommand+aptCurl,
		"env LC_ALL=C apt-mark hold -- "+aptCurl,
	)

	guest.clearHolds()
	read := aptRead(t, server, applied.NewState)
	if locked := aptSetMembers(t, aptAttributes(t, resourceType, read)["locked"]); len(locked) != 0 {
		t.Fatalf("State reports locked %v after a manual unhold, want none", locked)
	}

	aptApply(t, server, applied.NewState, aptValue(t, resourceType, spec))
	if !guest.isHeld(aptCurl) {
		t.Fatal("Guest did not hold curl after the repair apply.")
	}

	empty := aptSpec{packages: []string{aptCurl}, locked: []string{}}
	aptApply(t, server, applied.NewState, aptValue(t, resourceType, empty))
	if guest.isHeld(aptCurl) {
		t.Fatal("Guest still marks curl as held after the package left locked.")
	}
}

func TestAptPackagesRemovesDroppedPackageWhenEnabled(t *testing.T) {
	enabled := true
	guest := newAptGuest(
		map[string]string{aptCurl: aptOldVersion, aptWget: aptOldVersion},
		map[string]string{},
	)
	guest.held[aptWget] = true
	server, resourceType := aptServer(t, guest)

	prior := aptValue(t, resourceType, aptSpec{
		packages: []string{aptCurl, aptWget}, locked: []string{aptWget}, removeOnDelete: &enabled,
	})
	planned := aptValue(t, resourceType, aptSpec{
		packages: []string{aptCurl}, locked: []string{}, removeOnDelete: &enabled,
	})
	applied := aptApply(t, server, prior, planned)

	if _, isInstalled := guest.installedVersion(aptWget); isInstalled {
		t.Fatal("Guest still has wget after it left packages.")
	}
	if _, isInstalled := guest.installedVersion(aptCurl); !isInstalled {
		t.Fatal("Guest lost curl, which stays in packages.")
	}
	requireAptCommands(t, guest,
		"env LC_ALL=C apt-mark unhold -- "+aptWget,
		plainQueryCommand+aptCurl,
		plainQueryCommand+aptWget,
		"env LC_ALL=C apt-mark unhold -- "+aptWget,
		aptEnvironment+" remove -y -- "+aptWget,
	)

	aptApply(t, server, applied.NewState, powerResourceNull(t, resourceType))
	if _, isInstalled := guest.installedVersion(aptCurl); isInstalled {
		t.Fatal("Guest still has curl after destroy with remove_on_delete.")
	}
}

func TestAptPackagesKeepsDroppedPackageByDefault(t *testing.T) {
	guest := newAptGuest(
		map[string]string{aptCurl: aptOldVersion, aptWget: aptOldVersion},
		map[string]string{},
	)
	server, resourceType := aptServer(t, guest)

	prior := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl, aptWget}})
	planned := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl}})
	applied := aptApply(t, server, prior, planned)
	aptApply(t, server, applied.NewState, powerResourceNull(t, resourceType))

	for _, name := range []string{aptCurl, aptWget} {
		if _, isInstalled := guest.installedVersion(name); !isInstalled {
			t.Fatalf("Guest lost %s although remove_on_delete is unset.", name)
		}
	}
}

func TestAptPackagesPlanWarnsWhichPackagesAreRemoved(t *testing.T) {
	enabled := true
	guest := newAptGuest(map[string]string{aptCurl: aptOldVersion, aptWget: aptOldVersion}, map[string]string{})
	server, resourceType := aptServer(t, guest)

	prior := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl, aptWget}, removeOnDelete: &enabled})
	proposed := aptValue(t, resourceType, aptSpec{packages: []string{aptCurl}, removeOnDelete: &enabled})
	plan, err := server.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName:         aptType,
		PriorState:       prior,
		ProposedNewState: proposed,
		Config:           proposed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Diagnostics) != 1 || plan.Diagnostics[0].Severity != tfprotov6.DiagnosticSeverityWarning ||
		plan.Diagnostics[0].Detail != aptWget {
		t.Fatalf("Plan diagnostics %v, want one warning that lists wget", plan.Diagnostics)
	}
}

func TestAptPackagesValidationRejectsInvalidInput(t *testing.T) {
	guest := newAptGuest(map[string]string{}, map[string]string{})
	server, resourceType := aptServer(t, guest)

	rejected := map[string]aptSpec{
		"version outside packages": {
			packages: []string{aptCurl}, versions: map[string]string{aptWget: aptOldVersion},
		},
		"lock outside packages": {packages: []string{aptCurl}, locked: []string{aptWget}},
		"version with a dash":   {packages: []string{aptCurl}, versions: map[string]string{aptCurl: "-1.0"}},
		"version with options":  {packages: []string{aptCurl}, versions: map[string]string{aptCurl: "1.0 --purge"}},
		"lock with a dash":      {packages: []string{aptCurl}, locked: []string{"-o"}},
	}
	for name, spec := range rejected {
		response, err := server.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
			TypeName: aptType,
			Config:   aptValue(t, resourceType, spec),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Diagnostics) == 0 {
			t.Errorf("Validation accepted the case: %s", name)
		}
	}

	accepted := aptSpec{
		packages: []string{aptCurl, aptWget},
		versions: map[string]string{aptCurl: "2:8.0+dfsg-1~bpo12+1"},
		locked:   []string{aptWget},
	}
	response, err := server.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
		TypeName: aptType,
		Config:   aptValue(t, resourceType, accepted),
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNoLocaleDiagnostics(t, response.Diagnostics)
}
