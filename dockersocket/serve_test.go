package dockersocket_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
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

	incompletePost = "POST /containers/create HTTP/1.1\r\nHost: docker\r\n" +
		"Content-Length: 10\r\nExpect: 100-continue\r\n\r\n"
	continueStatus = "HTTP/1.1 100 Continue"
)

func TestServeClosesStalledConnectionAndKeepsReplacementSocket(t *testing.T) {
	client, err := transport.NewClient(
		map[string]transport.NodeConfig{
			testNode: {Endpoint: testEndpoint, APIToken: testToken, Insecure: true},
		},
		transport.DefaultMaxRequests,
	)
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

	connection := dialWhenReady(t, socketPath)
	err = connection.SetDeadline(time.Now().Add(serveWait))
	if err != nil {
		t.Fatalf("set connection deadline: %v", err)
	}

	_, err = io.WriteString(connection, incompletePost)
	if err != nil {
		t.Fatalf("write request headers: %v", err)
	}

	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(statusLine, continueStatus) {
		t.Fatalf("status line = %q, err = %v, want %q", statusLine, err, continueStatus)
	}

	cancel()
	waitForRemoval(t, socketPath)
	var listenConfig net.ListenConfig
	replacement, err := listenConfig.Listen(t.Context(), "unix", socketPath)
	if err != nil {
		t.Fatalf("listen on replacement socket: %v", err)
	}

	t.Cleanup(func() { _ = replacement.Close() })

	select {
	case serveErr := <-served:
		if !errors.Is(serveErr, context.DeadlineExceeded) {
			t.Fatalf("serve error = %v, want %v", serveErr, context.DeadlineExceeded)
		}
	case <-time.After(serveWait):
		t.Fatalf("serve did not return within %s after cancellation", serveWait)
	}

	_, err = io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read original connection after serve returned: %v, want end of file", err)
	}

	replacementConnection := dialWhenReady(t, socketPath)
	_ = replacementConnection.Close()
}

func dialWhenReady(t *testing.T, socketPath string) net.Conn {
	t.Helper()
	var dialer net.Dialer
	deadline := time.Now().Add(serveWait)

	for {
		connection, err := dialer.DialContext(t.Context(), "unix", socketPath)
		if err == nil {
			t.Cleanup(func() { _ = connection.Close() })
			return connection
		}

		if time.Now().After(deadline) {
			t.Fatalf("socket %s did not accept a connection: %v", socketPath, err)
		}

		time.Sleep(socketPoll)
	}
}

func waitForRemoval(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(serveWait)

	for {
		_, err := os.Stat(socketPath)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("socket %s still exists after cancellation: err = %v", socketPath, err)
		}

		time.Sleep(socketPoll)
	}
}
