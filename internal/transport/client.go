package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxRequests applies when the provider configuration omits
// max_requests.
const DefaultMaxRequests = 4

const (
	// The Proxmox proxy closes a request after 30 seconds.
	requestTimeout        = 45 * time.Second
	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	idleConnectionTimeout = 90 * time.Second

	// An exec-status response contains two base64 streams of up to 1 MiB each.
	responseBodyLimit = 8 << 20

	authorizationScheme = "PVEAPIToken="
	jsonContentType     = "application/json"
	statusClassSuccess  = 2
	statusClassDivisor  = 100
	authorizationHeader = "Authorization"
)

// The token has the form user@realm!tokenid=secret.
var tokenPattern = regexp.MustCompile(`^[^@!=\s]+@[^@!=\s]+![^!=\s]+=\S+$`)

// NodeConfig is the API endpoint of one hypervisor. APIToken is the full
// token string user@realm!tokenid=secret. Insecure skips TLS certificate
// verification.
type NodeConfig struct {
	Endpoint string
	APIToken string
	Insecure bool
}

// Result is the outcome of a command that ran to its end. ExitCode is the
// exit status of the command, and Stdout and Stderr are the bytes that it
// wrote.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Client calls the Proxmox VE API of each configured node and limits the
// concurrent requests per node.
type Client struct {
	nodes map[string]*nodeConnection
}

type nodeConnection struct {
	endpoint      string
	authorization string
	httpClient    *http.Client
	slots         chan struct{}
}

type apiRequest struct {
	method    string
	operation string
	query     url.Values
	body      *execStartBody
}

type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// NewClient returns a client for the named hypervisors. The key of each node
// is its Proxmox node name. maxRequests limits the concurrent API requests
// per node and must be at least 1.
func NewClient(nodes map[string]NodeConfig, maxRequests int) (*Client, error) {
	if maxRequests < 1 {
		return nil, fmt.Errorf("max_requests must be at least 1, got %d", maxRequests)
	}
	client := &Client{nodes: make(map[string]*nodeConnection, len(nodes))}
	for name, config := range nodes {
		connection, err := newNodeConnection(name, config, maxRequests)
		if err != nil {
			return nil, err
		}
		client.nodes[name] = connection
	}
	return client, nil
}

func newNodeConnection(name string, config NodeConfig, maxRequests int) (*nodeConnection, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil {
		slog.Error("parse node endpoint failed", "node", name, "err", err)
		return nil, fmt.Errorf("node %q: parse endpoint: %w", name, err)
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return nil, fmt.Errorf("node %q: endpoint must start with https:// or http://", name)
	}
	if endpoint.Host == "" {
		return nil, fmt.Errorf("node %q: endpoint has no host", name)
	}
	if !tokenPattern.MatchString(config.APIToken) {
		return nil, fmt.Errorf("node %q: api_token must have the form user@realm!tokenid=secret", name)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	tlsConfig.InsecureSkipVerify = config.Insecure
	dialer := &net.Dialer{Timeout: dialTimeout}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: tlsHandshakeTimeout,
		IdleConnTimeout:     idleConnectionTimeout,
		MaxIdleConnsPerHost: maxRequests,
	}
	return &nodeConnection{
		endpoint:      strings.TrimRight(config.Endpoint, "/"),
		authorization: authorizationScheme + config.APIToken,
		httpClient:    &http.Client{Transport: transport, Timeout: requestTimeout},
		slots:         make(chan struct{}, maxRequests),
	}, nil
}

// call sends one API request for the guest and returns the data member of the
// response. An unreachable guest returns an *unreachableError. Every other
// failure returns an error that states the guest, the API path, the HTTP
// status, and the response text.
func (connection *nodeConnection) call(
	ctx context.Context,
	guest Guest,
	request apiRequest,
) (json.RawMessage, error) {
	apiPath := guest.apiPath(request.operation)

	select {
	case connection.slots <- struct{}{}:
	case <-ctx.Done():
		slog.ErrorContext(ctx, "wait for a request slot ended", "node", guest.Node, "err", ctx.Err())
		return nil, fmt.Errorf("%s: wait for a request slot for %s %s: %w", guest, request.method, apiPath, ctx.Err())
	}
	defer func() { <-connection.slots }()

	httpRequest, err := connection.newHTTPRequest(ctx, guest, request, apiPath)
	if err != nil {
		return nil, err
	}
	response, err := connection.httpClient.Do(httpRequest)
	if err != nil {
		slog.ErrorContext(ctx, "api request failed", "guest", guest.String(), "path", apiPath, "err", err)
		return nil, fmt.Errorf("%s: %s %s: %w", guest, request.method, apiPath, err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit))
	if err != nil {
		slog.ErrorContext(ctx, "read api response failed", "guest", guest.String(), "path", apiPath, "err", err)
		return nil, fmt.Errorf("%s: %s %s: read response: %w", guest, request.method, apiPath, err)
	}
	if response.StatusCode/statusClassDivisor != statusClassSuccess {
		return nil, apiFailure(guest, request.method, apiPath, response, body)
	}

	var envelope apiEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		slog.ErrorContext(ctx, "api response is not a JSON document", "guest", guest.String(), "path", apiPath, "err", err)
		return nil, fmt.Errorf(
			"%s: %s %s returned HTTP %d with a response that is not a JSON document: %w",
			guest, request.method, apiPath, response.StatusCode, err,
		)
	}
	return envelope.Data, nil
}

func (connection *nodeConnection) newHTTPRequest(
	ctx context.Context,
	guest Guest,
	request apiRequest,
	apiPath string,
) (*http.Request, error) {
	address := connection.endpoint + apiPath
	if len(request.query) > 0 {
		address += "?" + request.query.Encode()
	}
	var bodyReader io.Reader
	if request.body != nil {
		document, err := encodeJSON(request.body)
		if err != nil {
			return nil, fmt.Errorf("%s: %s %s: %w", guest, request.method, apiPath, err)
		}
		bodyReader = bytes.NewReader(document)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.method, address, bodyReader)
	if err != nil {
		slog.ErrorContext(ctx, "build api request failed", "guest", guest.String(), "path", apiPath, "err", err)
		return nil, fmt.Errorf("%s: %s %s: build request: %w", guest, request.method, apiPath, err)
	}
	httpRequest.Header.Set(authorizationHeader, connection.authorization)
	httpRequest.Header.Set("Accept", jsonContentType)
	if bodyReader != nil {
		httpRequest.Header.Set("Content-Type", jsonContentType)
	}
	return httpRequest, nil
}

// encodeJSON leaves the characters <, >, and & unescaped, because the request
// body limit of the API counts the encoded size.
func encodeJSON(value *execStartBody) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		slog.Error("encode api request body failed", "err", err)
		return nil, fmt.Errorf("encode request body: %w", err)
	}
	return buffer.Bytes(), nil
}

// The Proxmox API writes the error message in the status line reason phrase
// and can repeat it in the body. The error text includes both.
func apiFailure(
	guest Guest,
	method string,
	apiPath string,
	response *http.Response,
	body []byte,
) error {
	reason := strings.TrimSpace(strings.TrimPrefix(response.Status, strconv.Itoa(response.StatusCode)))
	text := reason
	bodyText := strings.TrimSpace(string(body))
	if bodyText != "" {
		if text != "" {
			text += "; "
		}
		text += bodyText
	}
	detail := fmt.Sprintf("%s %s returned HTTP %d: %s", method, apiPath, response.StatusCode, text)
	for _, pattern := range unreachablePatterns {
		if pattern.MatchString(text) {
			return &unreachableError{guest: guest, detail: detail}
		}
	}
	return fmt.Errorf("%s: %s", guest, detail)
}
