package dockersocket_test

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/dockersocket"
	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	testToken        = "tester@pve!probe=secret-value"
	testEndpoint     = "https://127.0.0.1:1/"
	testNode         = "suburban"
	testVMID         = 217
	testGuestSocket  = "/var/run/docker.sock"
	testMaxBodyBytes = 1 << 20
	serveWait        = 10 * time.Second
	socketPoll       = 50 * time.Millisecond
)

func TestServeRemovesSocketAfterCancel(t *testing.T) {
	client, err := transport.NewClient(map[string]transport.NodeConfig{
		testNode: {Endpoint: testEndpoint, APIToken: testToken, Insecure: true},
	}, transport.DefaultMaxRequests)
	if err != nil {
		t.Fatalf("create transport client: %v", err)
	}
	directory, err := os.MkdirTemp("", "ds")
	if err != nil {
		t.Fatalf("create directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "d.sock")

	serveContext, cancel := context.WithCancel(t.Context())
	defer cancel()
	guest := transport.Guest{Node: testNode, VMID: testVMID, Kind: transport.KindLXC}
	options := dockersocket.Options{
		SocketPath:   socketPath,
		GuestSocket:  testGuestSocket,
		MaxBodyBytes: testMaxBodyBytes,
	}
	served := make(chan error, 1)
	go func() {
		served <- dockersocket.Serve(serveContext, client, guest, options)
	}()

	var dialer net.Dialer
	deadline := time.Now().Add(serveWait)
	for {
		connection, dialErr := dialer.DialContext(t.Context(), "unix", socketPath)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s did not accept a connection: %v", socketPath, dialErr)
		}
		time.Sleep(socketPoll)
	}

	cancel()
	select {
	case serveErr := <-served:
		if serveErr != nil {
			t.Fatalf("serve: %v", serveErr)
		}
	case <-time.After(serveWait):
		t.Fatalf("serve did not return within %s after cancellation", serveWait)
	}
	_, err = os.Stat(socketPath)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat socket after serve returned: err = %v, want a missing file", err)
	}
}
