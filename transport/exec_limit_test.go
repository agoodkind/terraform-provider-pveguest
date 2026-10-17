package transport_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	execLimitTestLimit   = 2
	execLimitTestCallers = 8
)

type execCountingAPI struct {
	mutex         sync.Mutex
	active        int
	maxActive     int
	polls         map[string]int
	nextProcessID atomic.Int64
}

func (api *execCountingAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	received := request.Method + " " + request.URL.Path
	api.mutex.Lock()
	defer api.mutex.Unlock()

	switch received {
	case qemuExecRequest:
		api.active++
		api.maxActive = max(api.maxActive, api.active)
		writeAPIDocument(writer, execStartDocument{PID: api.nextProcessID.Add(1)})
	case qemuStatusRequest:
		processID := request.URL.Query().Get("pid")
		api.polls[processID]++
		if api.polls[processID] < 2 {
			writeAPIDocument(writer, execStatusDocument{Exited: 0})
			return
		}
		api.active--
		writeAPIDocument(writer, execStatusDocument{Exited: 1})
	default:
		http.NotFound(writer, request)
	}
}

func TestRunLimitsConcurrentExecCommandsPerNode(t *testing.T) {
	api := &execCountingAPI{polls: map[string]int{}}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	nodes := map[string]transport.NodeConfig{
		testNode: {
			Endpoint:        server.URL,
			APIToken:        testAPIToken,
			MaxExecRequests: execLimitTestLimit,
		},
	}
	client, err := transport.NewClient(nodes, execLimitTestCallers)
	if err != nil {
		t.Fatalf("the NewClient call returned an error: %v", err)
	}

	guest := transport.Guest{Node: testNode, VMID: testVMID, Kind: transport.KindQEMU}
	failures := make(chan error, execLimitTestCallers)
	var group sync.WaitGroup
	for caller := range execLimitTestCallers {
		group.Add(1)
		go func() {
			defer group.Done()
			_, runErr := client.Run(
				context.Background(),
				guest,
				transport.Command{Argv: []string{"true", strconv.Itoa(caller)}},
			)
			failures <- runErr
		}()
	}
	group.Wait()
	close(failures)

	for runErr := range failures {
		if runErr != nil {
			t.Fatalf("a Run call returned an error: %v", runErr)
		}
	}
	if api.maxActive > execLimitTestLimit {
		t.Fatalf("the API saw %d commands at once, want at most %d", api.maxActive, execLimitTestLimit)
	}
	if api.maxActive < execLimitTestLimit {
		t.Fatalf("the API saw %d commands at once, want the limit of %d to be used", api.maxActive, execLimitTestLimit)
	}
}
