package transport_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	testNode     = "node"
	testAPIToken = "user@pam!token=secret"
	testVMID     = 105

	qemuExecPath       = "/api2/json/nodes/node/qemu/105/exec"
	qemuExecStatusPath = "/api2/json/nodes/node/qemu/105/exec-status"
	qemuExecRequest    = http.MethodPost + " " + qemuExecPath
	qemuStatusRequest  = http.MethodGet + " " + qemuExecStatusPath

	testExecID         = 4242
	testExitCode       = 3
	testTimeoutSeconds = 300
	timedOutExitCode   = 124
	nonTextByte        = 0xff
)

type execRequestDocument struct {
	Command   []string `json:"command"`
	InputData *string  `json:"input-data"`
	Timeout   *int     `json:"timeout"`
}

type execStartDocument struct {
	PID int64 `json:"pid"`
}

type execStatusDocument struct {
	Exited   int    `json:"exited"`
	ExitCode int    `json:"exitcode"`
	OutData  string `json:"out-data"`
	ErrData  string `json:"err-data"`
	TimedOut int    `json:"timed-out,omitempty"`
}

type apiDocument[T execStartDocument | execStatusDocument] struct {
	Data T `json:"data"`
}

type qemuOverlayAPI struct {
	mutex    sync.Mutex
	status   execStatusDocument
	requests []string
	exec     execRequestDocument
}

func (api *qemuOverlayAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()

	received := request.Method + " " + request.URL.Path
	api.requests = append(api.requests, received)
	switch received {
	case qemuExecRequest:
		if err := json.NewDecoder(request.Body).Decode(&api.exec); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		writeAPIDocument(writer, execStartDocument{PID: testExecID})
	case qemuStatusRequest:
		if request.URL.Query().Get("pid") != strconv.Itoa(testExecID) {
			http.NotFound(writer, request)
			return
		}
		writeAPIDocument(writer, api.status)
	default:
		http.NotFound(writer, request)
	}
}

func writeAPIDocument[T execStartDocument | execStatusDocument](writer http.ResponseWriter, data T) {
	if err := json.NewEncoder(writer).Encode(apiDocument[T]{Data: data}); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func runAgainstQEMUOverlayAPI(
	t *testing.T,
	api *qemuOverlayAPI,
	command transport.Command,
) (transport.Result, error) {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	nodes := map[string]transport.NodeConfig{
		testNode: {Endpoint: server.URL, APIToken: testAPIToken},
	}
	client, err := transport.NewClient(nodes, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatalf("the NewClient call returned an error: %v", err)
	}
	guest := transport.Guest{Node: testNode, VMID: testVMID, Kind: transport.KindQEMU}
	result, err := client.Run(context.Background(), guest, command)

	api.mutex.Lock()
	defer api.mutex.Unlock()
	if !slices.Equal(api.requests, []string{qemuExecRequest, qemuStatusRequest}) {
		t.Fatalf("the Run call did not send the QEMU exec request followed by the exec-status request")
	}
	return result, err
}

func TestRunCallsOverlayExecMethodsForQEMUGuest(t *testing.T) {
	argv := []string{"cat"}
	stdin := bytes.Repeat([]byte{nonTextByte}, transport.KindQEMU.StdinLimit())
	stdout := []byte{0, nonTextByte, '\n'}
	stderr := []byte{nonTextByte}
	api := &qemuOverlayAPI{status: execStatusDocument{
		Exited:   1,
		ExitCode: testExitCode,
		OutData:  base64.StdEncoding.EncodeToString(stdout),
		ErrData:  base64.StdEncoding.EncodeToString(stderr),
	}}

	command := transport.Command{Argv: argv, Stdin: stdin, TimeoutSeconds: testTimeoutSeconds}
	result, err := runAgainstQEMUOverlayAPI(t, api, command)
	if err != nil {
		t.Fatalf("the Run call returned an error for an exited QEMU command")
	}

	if !slices.Equal(api.exec.Command, argv) {
		t.Fatalf("the exec request command differs from the command arguments")
	}
	if api.exec.InputData == nil {
		t.Fatalf("the exec request has no input-data for a command with standard input")
	}
	decodedInput, err := base64.StdEncoding.DecodeString(*api.exec.InputData)
	if err != nil || !bytes.Equal(decodedInput, stdin) {
		t.Fatalf("the exec request input-data is not the base64 encoding of the standard input")
	}
	if api.exec.Timeout == nil || *api.exec.Timeout != testTimeoutSeconds {
		t.Fatalf("the exec request timeout is missing or differs from the command timeout")
	}
	if !bytes.Equal(result.Stdout, stdout) || !bytes.Equal(result.Stderr, stderr) {
		t.Fatalf("the Run result streams differ from the decoded exec-status output")
	}
	if result.ExitCode != testExitCode {
		t.Fatalf("the Run result exit code differs from the exec-status exitcode")
	}
}

func TestRunReturnsErrorForTimedOutQEMUCommand(t *testing.T) {
	api := &qemuOverlayAPI{status: execStatusDocument{
		Exited:   1,
		ExitCode: timedOutExitCode,
		TimedOut: 1,
	}}

	_, err := runAgainstQEMUOverlayAPI(t, api, transport.Command{Argv: []string{"true"}})
	if err == nil {
		t.Fatalf("the Run call returned no error for a timed-out QEMU command")
	}
}

func runAgainstFailingAPI(t *testing.T, responseText string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, responseText, http.StatusInternalServerError)
		},
	))
	t.Cleanup(server.Close)

	nodes := map[string]transport.NodeConfig{
		testNode: {Endpoint: server.URL, APIToken: testAPIToken},
	}
	client, err := transport.NewClient(nodes, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatalf("the NewClient call returned an error: %v", err)
	}
	guest := transport.Guest{Node: testNode, VMID: testVMID, Kind: transport.KindLXC}
	_, err = client.Run(context.Background(), guest, transport.Command{Argv: []string{"true"}})
	return err
}

func TestRunReturnsUnreachableErrorForStoppedContainer(t *testing.T) {
	err := runAgainstFailingAPI(t, "container '105' not running!")

	var unreachable *transport.UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("the Run error is not *UnreachableError: %v", err)
	}
}

func TestRunReturnsOtherErrorForOtherAPIFailure(t *testing.T) {
	err := runAgainstFailingAPI(t, "permission denied")

	var unreachable *transport.UnreachableError
	if err == nil || errors.As(err, &unreachable) {
		t.Fatalf("the Run permission error is *UnreachableError: %v", err)
	}
}
