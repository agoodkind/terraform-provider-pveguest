package transport_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

type readExecution struct {
	pid      int64
	stdout   []byte
	stderr   []byte
	exitCode int
	finished time.Time
	started  time.Time
}

type realReadExecService struct {
	mutex        sync.Mutex
	results      map[int64]readExecution
	statusReads  map[int64]int
	executions   []readExecution
	lostStarts   int
	lostStatuses int
	firstLost    chan struct{}
	lostSignaled bool
}

func newRealReadClient(t *testing.T, lostStarts int) (*transport.Client, *realReadExecService, transport.Guest) {
	t.Helper()
	service := &realReadExecService{
		mutex: sync.Mutex{}, results: make(map[int64]readExecution), statusReads: make(map[int64]int), executions: nil,
		lostStarts: lostStarts, lostStatuses: 0, firstLost: make(chan struct{}), lostSignaled: false,
	}
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)
	client, err := transport.NewClient(map[string]transport.NodeConfig{
		testNode: {Endpoint: server.URL, APIToken: testAPIToken},
	}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatal(err)
	}
	return client, service, transport.Guest{Node: testNode, VMID: testVMID, Kind: transport.KindQEMU}
}

func (service *realReadExecService) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.Method + " " + request.URL.Path {
	case qemuExecRequest:
		service.start(writer, request)
	case qemuStatusRequest:
		service.status(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (service *realReadExecService) start(writer http.ResponseWriter, request *http.Request) {
	var document execRequestDocument
	if err := json.NewDecoder(request.Body).Decode(&document); err != nil || len(document.Command) == 0 {
		http.Error(writer, "the local command request is invalid", http.StatusBadRequest)
		return
	}
	stdin := []byte(nil)
	if document.InputData != nil {
		var err error
		stdin, err = base64.StdEncoding.DecodeString(*document.InputData)
		if err != nil {
			http.Error(writer, "the local command input is invalid", http.StatusBadRequest)
			return
		}
	}
	ctx := request.Context()
	if document.Timeout != nil {
		bounded, cancel := context.WithTimeout(ctx, time.Duration(*document.Timeout)*time.Second)
		defer cancel()
		ctx = bounded
	}
	command := exec.CommandContext(ctx, document.Command[0], document.Command[1:]...)
	command.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now()
	if err := command.Start(); err != nil {
		http.Error(writer, "the local program could not start", http.StatusInternalServerError)
		return
	}
	exitCode := 0
	if err := command.Wait(); err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			http.Error(writer, "the local program could not finish", http.StatusInternalServerError)
			return
		}
		exitCode = exited.ExitCode()
	}
	result := readExecution{
		pid: int64(command.Process.Pid), stdout: bytes.Clone(stdout.Bytes()), stderr: bytes.Clone(stderr.Bytes()),
		exitCode: exitCode, started: started, finished: time.Now(),
	}
	service.mutex.Lock()
	service.results[result.pid] = result
	service.executions = append(service.executions, result)
	loseResponse := service.lostStarts != 0
	if service.lostStarts > 0 {
		service.lostStarts--
	}
	service.mutex.Unlock()
	if loseResponse {
		service.disconnect(writer)
		return
	}
	writeAPIDocument(writer, execStartDocument{PID: result.pid})
}

func (service *realReadExecService) status(writer http.ResponseWriter, request *http.Request) {
	pid, err := strconv.ParseInt(request.URL.Query().Get("pid"), 10, 64)
	if err != nil {
		http.Error(writer, "the local process ID is invalid", http.StatusBadRequest)
		return
	}
	service.mutex.Lock()
	service.statusReads[pid]++
	result, found := service.results[pid]
	if found {
		delete(service.results, pid)
	}
	loseResponse := found && service.lostStatuses > 0
	if loseResponse {
		service.lostStatuses--
	}
	service.mutex.Unlock()
	if !found {
		http.NotFound(writer, request)
		return
	}
	if loseResponse {
		service.disconnect(writer)
		return
	}
	writeAPIDocument(writer, execStatusDocument{
		Exited: 1, ExitCode: result.exitCode,
		OutData: base64.StdEncoding.EncodeToString(result.stdout), ErrData: base64.StdEncoding.EncodeToString(result.stderr),
	})
}

func (service *realReadExecService) disconnect(writer http.ResponseWriter) {
	connection, _, err := http.NewResponseController(writer).Hijack()
	if err != nil {
		http.Error(writer, "the local connection could not close", http.StatusInternalServerError)
		return
	}
	_ = connection.Close()
	service.mutex.Lock()
	if !service.lostSignaled {
		service.lostSignaled = true
		close(service.firstLost)
	}
	service.mutex.Unlock()
}

func readOnlyInput(t *testing.T) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "read-input")
	contents := []byte("synthetic read-only command bytes\x00\xff\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, contents
}

func (service *realReadExecService) requireExecutions(t *testing.T, expected int, output []byte) []readExecution {
	t.Helper()
	service.mutex.Lock()
	defer service.mutex.Unlock()
	if len(service.executions) != expected {
		t.Fatalf("the local service completed %d real commands, want %d", len(service.executions), expected)
	}
	for _, execution := range service.executions {
		if execution.pid <= 0 || execution.exitCode != 0 || !bytes.Equal(execution.stdout, output) || len(execution.stderr) != 0 {
			t.Fatal("the local service did not execute the real file command successfully")
		}
	}
	return append([]readExecution(nil), service.executions...)
}

func requireReadOnlyFile(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatal("the read command changed the source file or its bytes could not be read")
	}
}

func TestRunReadOnlyRecoversActualBytesAfterLostStartResponse(t *testing.T) {
	path, contents := readOnlyInput(t)
	client, service, guest := newRealReadClient(t, 1)
	result, err := client.RunReadOnly(t.Context(), guest, transport.Command{Argv: []string{"cat", path}})
	if err != nil {
		t.Fatalf("the read-only command did not recover its lost start response: %v", err)
	}
	if result.ExitCode != 0 || !bytes.Equal(result.Stdout, contents) || len(result.Stderr) != 0 {
		t.Fatal("recovery did not return the actual source file bytes")
	}
	service.requireExecutions(t, 2, contents)
	requireReadOnlyFile(t, path, contents)
}

func TestRunReadOnlyStopsAfterThreeLostStartResponses(t *testing.T) {
	path, contents := readOnlyInput(t)
	client, service, guest := newRealReadClient(t, -1)
	result, err := client.RunReadOnly(t.Context(), guest, transport.Command{Argv: []string{"cat", path}})
	if err == nil || len(result.Stdout) != 0 {
		t.Fatal("persistent lost responses did not fail without returning an unconfirmed result")
	}
	executions := service.requireExecutions(t, 3, contents)
	if executions[1].started.Sub(executions[0].finished) < 250*time.Millisecond || executions[2].started.Sub(executions[1].finished) < time.Second {
		t.Fatal("read-only recovery omitted the bounded retry backoff")
	}
	requireReadOnlyFile(t, path, contents)
}

func TestRunReadOnlyCancellationStopsFurtherCommands(t *testing.T) {
	path, contents := readOnlyInput(t)
	client, service, guest := newRealReadClient(t, -1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-service.firstLost:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, err := client.RunReadOnly(ctx, guest, transport.Command{Argv: []string{"cat", path}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("read-only recovery did not preserve cancellation after the real command")
	}
	service.requireExecutions(t, 1, contents)
	requireReadOnlyFile(t, path, contents)
}

func TestRunDoesNotRepeatActualAppendAfterLostStartResponse(t *testing.T) {
	path, original := readOnlyInput(t)
	addition := []byte("synthetic ordinary command append\n")
	client, service, guest := newRealReadClient(t, 1)
	_, err := client.Run(t.Context(), guest, transport.Command{Argv: []string{"tee", "-a", path}, Stdin: addition})
	requireReadOnlyFile(t, path, bytes.Join([][]byte{original, addition}, nil))
	service.requireExecutions(t, 1, addition)
	if err == nil {
		t.Fatal("the ordinary append reported success after losing its start response")
	}
}
