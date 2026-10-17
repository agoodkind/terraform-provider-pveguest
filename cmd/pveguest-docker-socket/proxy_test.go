package main_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// startCommand builds the command and starts it with the given Proxmox API
// endpoint and a token file. It waits for the Unix socket and returns an HTTP
// client that connects to that socket.
func startCommand(t *testing.T, getOnly bool, endpoint string) *http.Client {
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
		"-node", testNode, "-endpoint", endpoint, "-token-file", tokenFile,
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
			client := startCommand(t, testCase.getOnly, testEndpoint)
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

// TestGuestResponseBody starts the command against a local TLS server that
// simulates the Proxmox exec API. POST exec returns a process ID. GET exec-status
// returns the status and curl output of an exited command. The curl output
// contains an HTTP response with Content-Length 10.
//
// For a complete 10-byte body, the test requires HTTP 200 and the body. For a
// 5-byte body, the test requires HTTP 502 and a message containing "incomplete".
// It also checks that the response does not include the partial body.
func TestGuestResponseBody(t *testing.T) {
	const (
		completeBody  = "0123456789"
		truncatedBody = "01234"
	)
	cases := []struct {
		name       string
		guestBody  string
		want       int
		wantInBody string
	}{
		{"complete body", completeBody, http.StatusOK, completeBody},
		{"truncated body", truncatedBody, http.StatusBadGateway, "incomplete"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			guestOutput := "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n" + testCase.guestBody
			encodedOutput := base64.StdEncoding.EncodeToString([]byte(guestOutput))
			proxmox := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasSuffix(request.URL.Path, "/exec") {
					_, _ = io.WriteString(writer, `{"data":{"pid":1}}`)
					return
				}
				_, _ = io.WriteString(writer, `{"data":{"exited":1,"exitcode":0,"out-data":"`+encodedOutput+`","err-data":""}}`)
			}))
			t.Cleanup(proxmox.Close)

			client := startCommand(t, true, proxmox.URL)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://docker/version", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("send request: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			text, _ := io.ReadAll(response.Body)
			if response.StatusCode != testCase.want {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, testCase.want, text)
			}
			if !strings.Contains(string(text), testCase.wantInBody) {
				t.Fatalf("body = %q, want it to contain %q", text, testCase.wantInBody)
			}
			if testCase.want != http.StatusOK && strings.Contains(string(text), testCase.guestBody) {
				t.Fatalf("body = %q, must not contain the partial guest body %q", text, testCase.guestBody)
			}
		})
	}
}
