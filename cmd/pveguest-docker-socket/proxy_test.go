package main_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

	testMaxBodyBytes = 1 << 20
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
		"-max-body-bytes", strconv.Itoa(testMaxBodyBytes),
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
	oversize := strings.Repeat("a", testMaxBodyBytes+1)
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

// guestCommand records the arguments and standard input of one command that
// the fake Proxmox API received.
type guestCommand struct {
	argv  []string
	stdin []byte
}

type execRequestBody struct {
	Command   []string `json:"command"`
	InputData string   `json:"input-data"`
}

// newFakeProxmox starts a local TLS server that simulates the Proxmox exec API
// and records each command. The server assigns exit code 1 to every command
// that runs failProgram. An empty failProgram does not match any program.
// The server supplies an HTTP 200 response as curl output.
func newFakeProxmox(t *testing.T, failProgram string) (*httptest.Server, func() []guestCommand) {
	t.Helper()
	var lock sync.Mutex
	var commands []guestCommand
	exitCodes := map[int]int{}
	nextPID := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		lock.Lock()
		defer lock.Unlock()
		if strings.HasSuffix(request.URL.Path, "/exec") {
			var body execRequestBody
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			stdin, err := base64.StdEncoding.DecodeString(body.InputData)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			commands = append(commands, guestCommand{argv: body.Command, stdin: stdin})
			nextPID++
			exitCodes[nextPID] = 0
			if body.Command[0] == failProgram {
				exitCodes[nextPID] = 1
			}
			_, _ = fmt.Fprintf(writer, `{"data":{"pid":%d}}`, nextPID)
			return
		}
		pid, _ := strconv.Atoi(request.URL.Query().Get("pid"))
		output := ""
		if strings.Contains(strings.Join(commands[pid-1].argv, " "), "curl") {
			output = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"
		}
		_, _ = fmt.Fprintf(
			writer, `{"data":{"exited":1,"exitcode":%d,"out-data":"%s","err-data":""}}`,
			exitCodes[pid], base64.StdEncoding.EncodeToString([]byte(output)),
		)
	}))
	t.Cleanup(server.Close)
	return server, func() []guestCommand {
		lock.Lock()
		defer lock.Unlock()
		return append([]guestCommand(nil), commands...)
	}
}

// TestStagedRequestBody sends a body of two stdin pieces plus 1000 bytes
// through the public handler. It checks that the dd inputs reconstruct the
// complete body in order and that curl uses @<directory>/body. It also checks
// that the last command requests directory removal after success or a dd
// failure, and that the dd failure returns HTTP 502.
func TestStagedRequestBody(t *testing.T) {
	pieceSize := transport.KindLXC.StdinLimit()
	body := make([]byte, 2*pieceSize+1000)
	for i := range body {
		body[i] = byte(i % 251)
	}
	cases := []struct {
		name        string
		failProgram string
		want        int
	}{
		{"complete body", "", http.StatusOK},
		{"append failure", "dd", http.StatusBadGateway},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			proxmox, recorded := newFakeProxmox(t, testCase.failProgram)
			client := startCommand(t, false, proxmox.URL)
			request, err := http.NewRequestWithContext(
				t.Context(), http.MethodPost, "http://docker/images/load", bytes.NewReader(body),
			)
			if err != nil {
				t.Fatalf("build request: %v", err)
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

			commands := recorded()
			directory := commands[0].argv[len(commands[0].argv)-1]
			if commands[0].argv[0] != "mkdir" || !strings.HasPrefix(directory, "/tmp/") {
				t.Fatalf("first command = %v, want mkdir of a directory under /tmp", commands[0].argv)
			}
			last := commands[len(commands)-1]
			wantRemove := []string{"rm", "-r", "-f", "--", directory}
			if strings.Join(last.argv, " ") != strings.Join(wantRemove, " ") {
				t.Fatalf("last command = %v, want %v", last.argv, wantRemove)
			}
			if testCase.want != http.StatusOK {
				return
			}

			var staged []byte
			var curlArgv []string
			for _, command := range commands {
				switch command.argv[0] {
				case "dd":
					staged = append(staged, command.stdin...)
				case "curl":
					curlArgv = command.argv
				}
			}
			if !bytes.Equal(staged, body) {
				t.Fatalf("staged %d bytes, want the %d bytes of the request body", len(staged), len(body))
			}
			wantSource := "@" + directory + "/body"
			if !slices.Contains(curlArgv, wantSource) {
				t.Fatalf("curl arguments = %v, want %s", curlArgv, wantSource)
			}
		})
	}
}
