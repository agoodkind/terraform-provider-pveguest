package acceptance_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const resultTestOutput = "pveguest-result-receipt\n"

type resultEnvelope struct {
	Data json.RawMessage `json:"data"`
}

type resultStatus struct {
	Exited   json.RawMessage `json:"exited"`
	ExitCode int             `json:"exitcode"`
	Stdout   string          `json:"out-data"`
	Stderr   string          `json:"err-data"`
}

type resultStart struct {
	PID int64 `json:"pid"`
}

func (g *testGuest) execResultPath(operation string) string {
	return fmt.Sprintf("/api2/json/nodes/%s/%s/%d/%s",
		url.PathEscape(g.guest.Node), g.guest.Kind, g.guest.VMID, operation)
}

func resultHTTPClient(g *testGuest) *http.Client {
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12, InsecureSkipVerify: g.node.Insecure,
		}},
		Timeout: 45 * time.Second,
	}
}

func resultAPI(ctx context.Context, t *testing.T, g *testGuest, client *http.Client, method, operation string, pid int64) (json.RawMessage, int) {
	t.Helper()
	address := strings.TrimRight(g.node.Endpoint, "/") + g.execResultPath(operation)
	var body io.Reader
	if method == http.MethodPost {
		document, err := json.Marshal(resultStart{PID: pid})
		if err != nil {
			t.Fatal("cannot encode the acknowledgment")
		}
		body = bytes.NewReader(document)
	} else {
		address += "?pid=" + strconv.FormatInt(pid, 10)
	}
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		t.Fatal("cannot construct the result request")
	}
	request.Header.Set("Authorization", "PVEAPIToken="+g.node.APIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("the result request failed")
	}
	defer func() { _ = response.Body.Close() }()
	document, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("cannot read the complete result response")
	}
	if response.StatusCode != http.StatusOK {
		return append([]byte(response.Status+"\n"), document...), response.StatusCode
	}
	var envelope resultEnvelope
	if err := json.Unmarshal(document, &envelope); err != nil {
		t.Fatal("the result response is malformed")
	}
	return envelope.Data, response.StatusCode
}

// resultFaultProxy forwards every request to the real testbed API.
type resultFaultProxy struct {
	guest       *testGuest
	upstream    *http.Client
	mode        string
	mutex       sync.Mutex
	starts      int
	acks        int
	pid         int64
	statusPIDs  []string
	interrupted bool
}

func (proxy *resultFaultProxy) ServeHTTP(writer http.ResponseWriter, incoming *http.Request) {
	request := incoming.Clone(incoming.Context())
	target, err := url.Parse(proxy.guest.node.Endpoint)
	if err != nil {
		http.Error(writer, "invalid acceptance endpoint", http.StatusBadGateway)
		return
	}
	request.URL.Scheme, request.URL.Host = target.Scheme, target.Host
	request.Host = target.Host
	request.RequestURI = ""
	response, err := proxy.upstream.Do(request)
	if err != nil {
		http.Error(writer, "the testbed API request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	document, err := io.ReadAll(response.Body)
	if err != nil {
		http.Error(writer, "the testbed API reply failed", http.StatusBadGateway)
		return
	}

	proxy.mutex.Lock()
	operation := incoming.URL.Path[strings.LastIndex(incoming.URL.Path, "/")+1:]
	if operation == "exec" {
		proxy.starts++
		var envelope resultEnvelope
		var started resultStart
		if json.Unmarshal(document, &envelope) == nil && json.Unmarshal(envelope.Data, &started) == nil {
			proxy.pid = started.PID
		}
	}
	shouldInterrupt := false
	if operation == "exec-status" {
		proxy.statusPIDs = append(proxy.statusPIDs, incoming.URL.Query().Get("pid"))
		var envelope resultEnvelope
		var status resultStatus
		if json.Unmarshal(document, &envelope) == nil && json.Unmarshal(envelope.Data, &status) == nil {
			exited := string(status.Exited)
			shouldInterrupt = (exited == "1" || exited == "true") && proxy.mode != "lost-ack"
		}
	}
	if operation == "exec-ack" {
		proxy.acks++
		shouldInterrupt = proxy.mode == "lost-ack"
	}
	shouldInterrupt = shouldInterrupt && !proxy.interrupted
	if shouldInterrupt {
		proxy.interrupted = true
	}
	proxy.mutex.Unlock()

	for name, values := range response.Header {
		writer.Header()[name] = values
	}
	if shouldInterrupt && proxy.mode == "malformed-result" {
		document = append(document, '!')
		writer.Header().Set("Content-Length", strconv.Itoa(len(document)))
	}
	if shouldInterrupt && (proxy.mode == "invalid-stream" || proxy.mode == "missing-exit-code") {
		var envelope resultEnvelope
		var status map[string]json.RawMessage
		if json.Unmarshal(document, &envelope) != nil || json.Unmarshal(envelope.Data, &status) != nil {
			http.Error(writer, "cannot inspect the actual result response", http.StatusBadGateway)
			return
		}
		if proxy.mode == "invalid-stream" {
			status["out-data"] = json.RawMessage(`"!"`)
		} else {
			delete(status, "exitcode")
		}
		envelope.Data, err = json.Marshal(status)
		if err == nil {
			document, err = json.Marshal(envelope)
		}
		if err != nil {
			http.Error(writer, "cannot alter the actual result response", http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(document)))
	}
	writer.WriteHeader(response.StatusCode)
	if shouldInterrupt && (proxy.mode == "lost-result" || proxy.mode == "lost-ack") {
		_, _ = writer.Write(document[:len(document)/2])
		_ = http.NewResponseController(writer).Flush()
		connection, _, hijackError := http.NewResponseController(writer).Hijack()
		if hijackError == nil {
			_ = connection.Close()
		}
		return
	}
	_, _ = writer.Write(document)
}

func TestAccExecResultReceipt(t *testing.T) {
	g := newTestGuest(t)
	for _, mode := range []string{"lost-result", "lost-ack", "malformed-result", "invalid-stream", "missing-exit-code"} {
		t.Run(mode, func(t *testing.T) {
			upstream := resultHTTPClient(g)
			t.Cleanup(upstream.CloseIdleConnections)
			proxy := &resultFaultProxy{guest: g, upstream: upstream, mode: mode}
			server := httptest.NewServer(proxy)
			t.Cleanup(server.Close)
			client, err := transport.NewClient(map[string]transport.NodeConfig{
				g.guest.Node: {Endpoint: server.URL, APIToken: g.node.APIToken},
			}, transport.DefaultMaxRequests)
			if err != nil {
				t.Fatal("cannot create the production transport")
			}
			result, runError := client.Run(t.Context(), g.guest,
				transport.Command{Argv: []string{"printf", resultTestOutput}})
			proxy.mutex.Lock()
			starts, acks, pid := proxy.starts, proxy.acks, proxy.pid
			interrupted := proxy.interrupted
			statusPIDs := append([]string(nil), proxy.statusPIDs...)
			proxy.mutex.Unlock()
			if pid > 0 {
				t.Cleanup(func() {
					cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 45*time.Second)
					defer cancel()
					_, statusCode := resultAPI(cleanupContext, t, g, upstream, http.MethodPost, "exec-ack", pid)
					if statusCode != http.StatusOK {
						t.Error("cannot acknowledge the test command during cleanup")
					}
				})
			}
			if starts != 1 || pid <= 0 || !interrupted {
				t.Fatal("the test did not interrupt one real completed command response")
			}
			for _, statusPID := range statusPIDs {
				if statusPID != strconv.FormatInt(pid, 10) {
					t.Fatal("the transport polled another command identifier")
				}
			}
			if mode == "malformed-result" || mode == "invalid-stream" || mode == "missing-exit-code" {
				if runError == nil || acks != 0 {
					t.Fatal("the transport acknowledged or accepted malformed result data")
				}
				data, statusCode := resultAPI(t.Context(), t, g, upstream, http.MethodGet, "exec-status", pid)
				if statusCode != http.StatusOK {
					t.Fatal("the malformed response deleted the actual saved result")
				}
				var status resultStatus
				if json.Unmarshal(data, &status) != nil || status.Stdout != base64.StdEncoding.EncodeToString([]byte(resultTestOutput)) {
					t.Fatal("the saved result differs from the real command output")
				}
			} else {
				if runError != nil || result.ExitCode != 0 || string(result.Stdout) != resultTestOutput || len(result.Stderr) != 0 || acks != 1 {
					t.Fatal("the transport did not return and acknowledge the real decoded result")
				}
			}
			_, statusCode := resultAPI(t.Context(), t, g, upstream, http.MethodPost, "exec-ack", pid)
			if statusCode != http.StatusOK {
				t.Fatal("the first repeated acknowledgment failed")
			}
			_, statusCode = resultAPI(t.Context(), t, g, upstream, http.MethodPost, "exec-ack", pid)
			if statusCode != http.StatusOK {
				t.Fatal("the second repeated acknowledgment failed")
			}
			missingResult, statusCode := resultAPI(t.Context(), t, g, upstream, http.MethodGet, "exec-status", pid)
			expectedError := fmt.Sprintf("no stored result for command %d in ", pid)
			if statusCode != http.StatusInternalServerError || !bytes.Contains(missingResult, []byte(expectedError)) {
				t.Fatal("the API did not report the expected missing result after acknowledgment")
			}
		})
	}
}
