package transport_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	testNode     = "node"
	testAPIToken = "user@pam!token=secret"
	testVMID     = 105
)

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
