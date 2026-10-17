package transport_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

// resettingAPI closes the connection without a response for the first request
// that matches resetRequest and answers every other request normally.
type resettingAPI struct {
	mutex        sync.Mutex
	requests     []string
	resetRequest string
	resetDone    bool
	afterReset   func()
}

func (api *resettingAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	received := request.Method + " " + request.URL.Path
	api.mutex.Lock()
	api.requests = append(api.requests, received)
	shouldReset := received == api.resetRequest && !api.resetDone
	if shouldReset {
		api.resetDone = true
	}
	api.mutex.Unlock()

	if shouldReset {
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = connection.Close()
		if api.afterReset != nil {
			api.afterReset()
		}
		return
	}

	// A closed connection after the exec response makes the next request open a
	// new connection, so only the retry logic of the client can recover it.
	writer.Header().Set("Connection", "close")
	switch received {
	case qemuExecRequest:
		writeAPIDocument(writer, execStartDocument{PID: testExecID})
	case qemuStatusRequest:
		writeAPIDocument(writer, execStatusDocument{Exited: 1, ExitCode: 0})
	default:
		http.NotFound(writer, request)
	}
}

func (api *resettingAPI) receivedRequests() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return append([]string(nil), api.requests...)
}

func runAgainstResettingAPI(
	t *testing.T,
	ctx context.Context,
	api *resettingAPI,
) error {
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
	_, err = client.Run(ctx, guest, transport.Command{Argv: []string{"true"}})
	return err
}

func countRequests(requests []string, wanted string) int {
	count := 0
	for _, received := range requests {
		if received == wanted {
			count++
		}
	}
	return count
}

func TestRunRetriesExecStatusAfterConnectionReset(t *testing.T) {
	api := &resettingAPI{resetRequest: qemuStatusRequest}
	if err := runAgainstResettingAPI(t, context.Background(), api); err != nil {
		t.Fatalf("the Run call returned an error after the retry: %v", err)
	}
	if got := countRequests(api.receivedRequests(), qemuStatusRequest); got != 2 {
		t.Fatalf("the client sent %d exec-status requests, want 2", got)
	}
}

func TestRunDoesNotRetryExecStartAfterConnectionReset(t *testing.T) {
	api := &resettingAPI{resetRequest: qemuExecRequest}
	err := runAgainstResettingAPI(t, context.Background(), api)
	if err == nil {
		t.Fatal("the Run call succeeded after the exec request lost its connection")
	}
	if !strings.Contains(err.Error(), "guest command may have started") {
		t.Fatalf("the error does not state that the command may have started: %v", err)
	}
	requests := api.receivedRequests()
	if len(requests) != 1 {
		t.Fatalf("the client sent %d requests, want 1: %v", len(requests), requests)
	}
}

func TestRunStopsRetryingWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	api := &resettingAPI{resetRequest: qemuStatusRequest, afterReset: cancel}

	err := runAgainstResettingAPI(t, ctx, api)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the Run call did not return the context error: %v", err)
	}
	if got := countRequests(api.receivedRequests(), qemuStatusRequest); got != 1 {
		t.Fatalf("the client sent %d exec-status requests, want 1", got)
	}
}
