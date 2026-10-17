package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	commandTimeoutSeconds = 60

	// requestDeadlineMargin extends each exec deadline 30 seconds beyond the
	// command timeout for command execution and status polling.
	requestDeadlineMargin = 30 * time.Second

	// stdinBodySource selects standard input as the request body source for curl.
	stdinBodySource = "@-"

	queryFollow = "follow"
	queryStream = "stream"

	pathEvents = "events"
	pathAttach = "attach"
	pathStats  = "stats"

	upgradeToken = "upgrade"
)

// droppedRequestHeaders contains the request headers that curlArguments omits.
var droppedRequestHeaders = map[string]bool{
	"Host":              true,
	"Content-Length":    true,
	"Connection":        true,
	"Transfer-Encoding": true,
	"Expect":            true,
	"Accept-Encoding":   true,
	"Upgrade":           true,
}

// droppedResponseHeaders contains the response headers that relay does not copy.
// relay sets Content-Length from the buffered body, except that HEAD requests
// use the daemon value.
var droppedResponseHeaders = map[string]bool{
	"Connection":        true,
	"Transfer-Encoding": true,
	"Content-Length":    true,
}

type proxy struct {
	client       *transport.Client
	guest        transport.Guest
	guestSocket  string
	getOnly      bool
	maxBodyBytes int64
}

func newProxy(
	client *transport.Client,
	guest transport.Guest,
	guestSocket string,
	getOnly bool,
	maxBodyBytes int64,
) *proxy {
	return &proxy{
		client:       client,
		guest:        guest,
		guestSocket:  guestSocket,
		getOnly:      getOnly,
		maxBodyBytes: maxBodyBytes,
	}
}

func (handler *proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	status := handler.handle(writer, request)
	slog.Info(
		"docker request",
		"method", request.Method,
		"path", request.URL.RequestURI(),
		"status", status,
		"seconds", time.Since(started).Seconds(),
	)
}

func (handler *proxy) handle(writer http.ResponseWriter, request *http.Request) int {
	if handler.getOnly && request.Method != http.MethodGet && request.Method != http.MethodHead {
		return reject(writer, request, http.StatusNotImplemented, "get-only mode allows only GET and HEAD requests")
	}
	reason := unsupportedReason(request)
	if reason != "" {
		return reject(writer, request, http.StatusNotImplemented, reason)
	}

	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, handler.maxBodyBytes))
	if err != nil {
		return reject(
			writer, request, http.StatusNotImplemented,
			fmt.Sprintf("request body exceeds the limit of %d bytes: %v", handler.maxBodyBytes, err),
		)
	}
	if len(body) > handler.guest.Kind.StdinLimit() {
		return handler.handleStaged(writer, request, body)
	}

	bodySource := ""
	if len(body) > 0 {
		bodySource = stdinBodySource
	}
	result, err := handler.runGuest(
		request.Context(), curlArguments(handler.guestSocket, request, bodySource), body, commandTimeoutSeconds,
	)
	if err != nil {
		return reject(writer, request, http.StatusBadGateway, err.Error())
	}
	return relay(writer, request, result.Stdout)
}

// runGuest runs one command in the guest and fails on a transport error or a
// nonzero exit code.
func (handler *proxy) runGuest(
	ctx context.Context,
	argv []string,
	stdin []byte,
	timeoutSeconds int,
) (transport.Result, error) {
	deadline := time.Duration(timeoutSeconds)*time.Second + requestDeadlineMargin
	commandContext, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	result, err := handler.client.Run(commandContext, handler.guest, transport.Command{
		Argv:           argv,
		Stdin:          stdin,
		TimeoutSeconds: timeoutSeconds,
	})
	if err != nil {
		slog.Error("guest command failed", "program", argv[0], "err", err)
		return transport.Result{}, fmt.Errorf("guest command %s failed: %w", argv[0], err)
	}
	if result.ExitCode != 0 {
		failure := fmt.Errorf(
			"%s exited with code %d: %s", argv[0], result.ExitCode, strings.TrimSpace(string(result.Stderr)),
		)
		slog.Error("guest command exited with an error", "err", failure)
		return transport.Result{}, failure
	}
	return result, nil
}

func reject(writer http.ResponseWriter, request *http.Request, status int, reason string) int {
	slog.Warn("docker request rejected", "method", request.Method, "path", request.URL.RequestURI(), "status", status, "reason", reason)
	http.Error(writer, reason, status)
	return status
}

// unsupportedReason returns a reason for requests that need a streaming or
// hijacked connection. It returns an empty string for other requests.
func unsupportedReason(request *http.Request) string {
	if request.Header.Get("Upgrade") != "" || headerHasToken(request.Header, "Connection", upgradeToken) {
		return "connection upgrade and hijack requests are not supported"
	}
	query := request.URL.Query()
	follow := query.Get(queryFollow)
	if follow == "1" || strings.EqualFold(follow, "true") {
		return "streaming requests are not supported: follow is set"
	}
	segments := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	last := segments[len(segments)-1]
	if last == pathEvents {
		return "streaming requests are not supported: /events"
	}
	if slices.Contains(segments, pathAttach) {
		return "streaming requests are not supported: /attach"
	}
	if last == pathStats {
		stream := query.Get(queryStream)
		if stream != "0" && !strings.EqualFold(stream, "false") {
			return "streaming requests are not supported: stats without stream=false"
		}
	}
	return ""
}

func headerHasToken(header http.Header, name string, token string) bool {
	for _, value := range header.Values(name) {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// curlArguments builds the curl command line for the guest socket and request.
// It adds --data-binary @- when curl must read a body from standard input.
func curlArguments(guestSocket string, request *http.Request, bodySource string) []string {
	arguments := []string{"curl", "-sS", "--raw", "-i", "--unix-socket", guestSocket}
	if request.Method == http.MethodHead {
		arguments = append(arguments, "--head")
	} else {
		arguments = append(arguments, "-X", request.Method)
	}
	arguments = append(arguments, "-H", "Expect:")
	for name, values := range request.Header {
		if droppedRequestHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, value := range values {
			arguments = append(arguments, "-H", name+": "+value)
		}
	}
	if bodySource != "" {
		arguments = append(arguments, "--data-binary", bodySource)
	}
	target := url.URL{Scheme: "http", Host: "localhost", Path: "", RawPath: "", RawQuery: ""}
	return append(arguments, target.String()+request.URL.RequestURI())
}

// relay parses curl output, including headers, as an HTTP response and writes
// the response to the client.
func relay(writer http.ResponseWriter, request *http.Request, output []byte) int {
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(output)), request)
	if err != nil {
		slog.Error("guest response is not an HTTP response", "err", err)
		return reject(writer, request, http.StatusBadGateway, "guest response is not an HTTP response: "+err.Error())
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		slog.Error("read guest response body failed", "err", err)
		return reject(writer, request, http.StatusBadGateway, "guest response body is incomplete: "+err.Error())
	}

	for name, values := range response.Header {
		if droppedResponseHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	if request.Method == http.MethodHead {
		writer.Header().Set("Content-Length", response.Header.Get("Content-Length"))
	} else {
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	writer.WriteHeader(response.StatusCode)
	_, err = writer.Write(body)
	if err != nil {
		slog.Error("write response to client failed", "err", err)
	}
	return response.StatusCode
}
