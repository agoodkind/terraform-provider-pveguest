package main_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	testToken      = "tester@pve!probe=secret-value"
	testEndpoint   = "https://127.0.0.1:1/"
	testNode       = "suburban"
	testVMID       = "217"
	socketWait     = 10 * time.Second
	socketPoll     = 50 * time.Millisecond
	tokenFileMode  = 0o600
	buildTimeout   = 120 * time.Second
	requestTimeout = 10 * time.Second
)

// startCommand builds and starts the command against an unreachable endpoint.

// It returns an HTTP client that connects to the command's Unix socket.
func startCommand(t *testing.T, getOnly bool) *http.Client {
	t.Helper()
	directory, err := os.MkdirTemp("", "pg")
	if err != nil {
		t.Fatalf("create directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	binary := filepath.Join(directory, "pveguest-docker-socket")
	buildContext, buildCancel := context.WithTimeout(context.Background(), buildTimeout)
	defer buildCancel()
	build := exec.CommandContext(buildContext, "go", "build", "-o", binary, ".")
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build command: %v\n%s", err, output)
	}

	tokenFile := filepath.Join(directory, "token")
	err = os.WriteFile(tokenFile, []byte(testToken), tokenFileMode)
	if err != nil {
		t.Fatalf("write token file: %v", err)
	}
	socketPath := filepath.Join(directory, "d.sock")
	arguments := []string{
		"-node", testNode, "-endpoint", testEndpoint, "-token-file", tokenFile,
		"-vmid", testVMID, "-socket", socketPath, "-insecure",
	}
	if getOnly {
		arguments = append(arguments, "-get-only")
	}
	runContext, runCancel := context.WithCancel(context.Background())
	command := exec.CommandContext(runContext, binary, arguments...)
	err = command.Start()
	if err != nil {
		runCancel()
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() {
		runCancel()
		_ = command.Wait()
	})

	deadline := time.Now().Add(socketWait)
	for {
		_, statErr := os.Stat(socketPath)
		if statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket %s did not appear", socketPath)
		}
		time.Sleep(socketPoll)
	}

	dialer := &net.Dialer{Timeout: requestTimeout}
	transportConfig := &http.Transport{
		DialContext: func(ctx context.Context, _ string, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: transportConfig, Timeout: requestTimeout}
}

// TestRejectedRequests checks that each rejected request class returns HTTP 501

// before the command attempts a guest exec.
func TestRejectedRequests(t *testing.T) {
	oversize := strings.Repeat("a", transport.KindLXC.StdinLimit()+1)
	cases := []struct {
		name    string
		getOnly bool
		method  string
		path    string
		headers map[string]string
		body    string
		want    int
	}{
		{"write with get only", true, http.MethodPost, "/containers/create", nil, "{}", http.StatusNotImplemented},
		{"upgrade header", false, http.MethodPost, "/containers/x/exec", map[string]string{"Upgrade": "tcp", "Connection": "Upgrade"}, "", http.StatusNotImplemented},
		{"follow query", false, http.MethodGet, "/containers/x/logs?follow=1", nil, "", http.StatusNotImplemented},
		{"events", false, http.MethodGet, "/v1.56/events", nil, "", http.StatusNotImplemented},
		{"attach", false, http.MethodPost, "/containers/x/attach?stream=1", nil, "", http.StatusNotImplemented},
		{"stats stream", false, http.MethodGet, "/containers/x/stats", nil, "", http.StatusNotImplemented},
		{"oversize body", false, http.MethodPost, "/build", nil, oversize, http.StatusNotImplemented},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := startCommand(t, testCase.getOnly)
			request, err := http.NewRequestWithContext(
				t.Context(), testCase.method, "http://docker"+testCase.path, bytes.NewBufferString(testCase.body),
			)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			for name, value := range testCase.headers {
				request.Header.Set(name, value)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("send request: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != testCase.want {
				text, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, testCase.want, text)
			}
		})
	}
}
