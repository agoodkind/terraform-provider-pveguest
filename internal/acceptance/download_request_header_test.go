package acceptance_test

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	requestHeaderName     = "Authorization"
	requestHeaderPrefix   = "Bearer "
	requestHeaderToken    = "acc-request-header-token-4d81"
	requestHeaderRotated  = "acc-request-header-token-9c27"
	requestHeaderWrong    = "acc-request-header-token-0000"
	requestHeaderObject   = "/private.bin"
	requestHeaderBytes    = 4096
	requestHeaderCAMode   = 0o600
	requestHeaderCAName   = "controller-ca.pem"
	requestHeaderPEMBlock = "CERTIFICATE"

	cacheDirectoryName = "pveguest"

	guestWorkDirectory    = "/root/pveguest-acc"
	guestServerUnit       = "pveguest-acc-https"
	guestServerPort       = "18443"
	guestServerBase       = "https://127.0.0.1:" + guestServerPort
	guestCAPath           = "/usr/local/share/ca-certificates/pveguest-acc-header.crt"
	guestCALink           = "/etc/ssl/certs/pveguest-acc-header.pem"
	guestServerSource     = "./testdata/headerserver"
	guestServerCommonName = "pveguest-acc-header"
	guestCertificateSpan  = time.Hour
	guestReadyAttempts    = 40
	guestReadyDelay       = 500 * time.Millisecond
	guestSecretMode       = "0600"
	guestPublicMode       = "0644"
	guestProgramMode      = "0755"
)

var (
	controllerRejection = regexp.MustCompile(`HTTP\s+status\s+401`)
	guestRejection      = regexp.MustCompile(`returned\s+error:\s+401`)
)

type requestHeaderServer struct {
	accepted   atomic.Value
	authorized atomic.Int64
	rejected   atomic.Int64
	url        string
	cacheHome  string
}

func (s *requestHeaderServer) accept(token string) {
	s.accepted.Store(requestHeaderPrefix + token)
}

func (s *requestHeaderServer) clearCache(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(s.cacheHome, cacheDirectoryName)); err != nil {
		t.Fatal(err)
	}
}

func (s *requestHeaderServer) handler(content []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(requestHeaderObject, func(writer http.ResponseWriter, request *http.Request) {
		want, _ := s.accepted.Load().(string)
		if request.Header.Get(requestHeaderName) != want {
			s.rejected.Add(1)
			http.Error(writer, "missing or wrong Authorization header", http.StatusUnauthorized)
			return
		}
		s.authorized.Add(1)
		_, _ = writer.Write(content)
	})
	return mux
}

func (g *testGuest) serveWithRequestHeader(t *testing.T, content []byte, token string) *requestHeaderServer {
	t.Helper()
	recorder := &requestHeaderServer{cacheHome: t.TempDir()}
	recorder.accept(token)
	server := httptest.NewTLSServer(recorder.handler(content))
	t.Cleanup(server.Close)

	certificate := pem.EncodeToMemory(&pem.Block{Type: requestHeaderPEMBlock, Bytes: server.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), requestHeaderCAName)
	if err := os.WriteFile(caFile, certificate, requestHeaderCAMode); err != nil {
		t.Fatal(err)
	}
	g.controllerCAFile = caFile
	t.Setenv("XDG_CACHE_HOME", recorder.cacheHome)
	recorder.url = server.URL
	return recorder
}

type guestHeaderServerState struct {
	Authorized int `json:"authorized"`
	Rejected   int `json:"rejected"`
	Scanned    int `json:"scanned"`
	Leaks      int `json:"leaks"`
}

type guestHeaderServer struct {
	guest     *testGuest
	url       string
	statePath string
}

func (s *guestHeaderServer) state() guestHeaderServerState {
	s.guest.t.Helper()
	var state guestHeaderServerState
	if err := json.Unmarshal([]byte(s.guest.readFile(s.statePath)), &state); err != nil {
		s.guest.t.Fatal(err)
	}
	return state
}

func (s *guestHeaderServer) checkProcessScan() error {
	state := s.state()
	if state.Authorized == 0 || state.Scanned == 0 {
		return fmt.Errorf("the guest server scanned %d process command lines for %d authorized requests",
			state.Scanned, state.Authorized)
	}
	if state.Leaks != 0 {
		return fmt.Errorf("%d guest process command lines contained the header value", state.Leaks)
	}
	return nil
}

func goArchitecture(t *testing.T, unameMachine string) string {
	t.Helper()
	switch unameMachine {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		t.Fatalf("the guest architecture %q has no Go build mapping", unameMachine)
		return ""
	}
}

func buildHeaderServer(t *testing.T, architecture string) []byte {
	t.Helper()
	output := filepath.Join(t.TempDir(), "headerserver")
	command := exec.CommandContext(
		t.Context(), "go", "build", "-trimpath", "-ldflags=-s -w", "-o", output, guestServerSource,
	)
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+architecture, "CGO_ENABLED=0")
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v: %s", guestServerSource, err, combined)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func gzipBytes(t *testing.T, content []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func guestServerCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: guestServerCommonName},
		NotBefore:             time.Now().Add(-guestCertificateSpan),
		NotAfter:              time.Now().Add(guestCertificateSpan),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: requestHeaderPEMBlock, Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certificatePEM, keyPEM
}

func (g *testGuest) putFile(path string, content []byte, mode string) {
	g.t.Helper()
	g.mustRun("install", "-m", mode, "/dev/null", path)
	for _, piece := range transport.SplitStdin(g.guest.Kind, content) {
		result := g.run(piece, "dd", "of="+path, "oflag=append", "conv=notrunc", "status=none")
		if result.ExitCode != 0 {
			g.t.Fatalf("append to %s: status %d: %s", path, result.ExitCode, result.Stderr)
		}
	}
}

func (g *testGuest) stopHeaderServer() {
	unit := guestServerUnit + ".service"
	g.run(nil, "systemctl", "stop", "--", unit)
	g.run(nil, "systemctl", "reset-failed", "--", unit)
	g.mustRun("rm", "-f", "--", guestCAPath)
	g.mustRun("update-ca-certificates", "--fresh")
	g.mustRun("rm", "-rf", "--", guestWorkDirectory)
}

func (g *testGuest) startHeaderServer(t *testing.T, content []byte, token string) *guestHeaderServer {
	t.Helper()
	t.Cleanup(g.stopHeaderServer)
	architecture := goArchitecture(t, strings.TrimSpace(g.mustRun("uname", "-m")))
	program := gzipBytes(t, buildHeaderServer(t, architecture))
	certificatePEM, keyPEM := guestServerCertificate(t)

	g.mustRun("mkdir", "-m", "0700", "--", guestWorkDirectory)
	binary := guestWorkDirectory + "/headerserver"
	g.putFile(binary+".gz", program, guestSecretMode)
	g.mustRun("gzip", "-df", "--", binary+".gz")
	g.mustRun("chmod", guestProgramMode, "--", binary)
	certificatePath := guestWorkDirectory + "/cert.pem"
	keyPath := guestWorkDirectory + "/key.pem"
	tokenPath := guestWorkDirectory + "/token"
	contentPath := guestWorkDirectory + "/content.bin"
	statePath := guestWorkDirectory + "/state.json"
	g.putFile(certificatePath, certificatePEM, guestSecretMode)
	g.putFile(keyPath, keyPEM, guestSecretMode)
	g.putFile(tokenPath, []byte(token), guestSecretMode)
	g.putFile(contentPath, content, guestSecretMode)

	g.putFile(guestCAPath, certificatePEM, guestPublicMode)
	g.mustRun("update-ca-certificates")

	g.mustRun(
		"systemd-run", "--unit="+guestServerUnit, "--collect", binary,
		"-addr", "127.0.0.1:"+guestServerPort, "-cert", certificatePath, "-key", keyPath,
		"-token-file", tokenPath, "-content-file", contentPath, "-state-file", statePath,
	)
	for attempt := 0; attempt < guestReadyAttempts; attempt++ {
		result := g.run(nil, "curl", "-fsS", "-o", "/dev/null", guestServerBase+"/ready")
		if result.ExitCode == 0 {
			return &guestHeaderServer{guest: g, url: guestServerBase + requestHeaderObject, statePath: statePath}
		}
		time.Sleep(guestReadyDelay)
	}
	t.Fatalf("the guest server did not answer %s/ready", guestServerBase)
	return nil
}

func requestHeaderArguments(token string, version int) string {
	return fmt.Sprintf(`request_headers_wo         = { %s = %q }
  request_headers_wo_version = %d`, requestHeaderName, requestHeaderPrefix+token, version)
}

func (g *testGuest) guestDownloadBlock(name string, url string, hash string, path string, extra string) string {
	return fmt.Sprintf(`
resource "pveguest_download" %q {
  node   = local.node
  vmid   = local.vmid
  kind   = local.kind
  fetch  = "guest"
  url    = %q
  sha256 = %q
  path   = %q
  %s
}
`, name, url, hash, path, extra)
}

func (g *testGuest) workspaceConfig(fetch string, url string, hash string, path string, version int, headers bool) string {
	extra := ""
	if headers {
		extra = fmt.Sprintf(`request_headers_wo         = { %s = "%s${var.secret}" }
  request_headers_wo_version = %d`, requestHeaderName, requestHeaderPrefix, version)
	}
	return g.providerBlock() + fmt.Sprintf(`
variable "secret" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "pveguest_download" "test" {
  node   = local.node
  vmid   = local.vmid
  kind   = local.kind
  fetch  = %q
  url    = %q
  sha256 = %q
  path   = %q
  %s
}
`, fetch, url, hash, path, extra)
}

func newHeaderWorkspace(t *testing.T) *tofuWorkspace {
	t.Helper()
	return &tofuWorkspace{t: t, directory: t.TempDir(), binary: requireVariable(t, tofuPathVariable)}
}

func (w *tofuWorkspace) applyConfig(config string, token string) (string, int) {
	w.t.Helper()
	w.variableValue = token
	w.writeConfig(config)
	return w.run("apply", "-no-color", "-auto-approve")
}

func (w *tofuWorkspace) requireRejected(config string, token string, pattern *regexp.Regexp) {
	w.t.Helper()
	output, exitCode := w.applyConfig(config, token)
	if exitCode == 0 {
		w.t.Fatalf("the apply succeeded, want a rejected download:\n%s", output)
	}
	if !pattern.MatchString(output) {
		w.t.Fatalf("the apply output does not match %s:\n%s", pattern, output)
	}
	if token != "" && strings.Contains(output, token) {
		w.t.Fatal("the apply output contains the header value")
	}
	w.requireStateOmitsIfPresent(token)
}

func (w *tofuWorkspace) requireApplied(config string, token string) {
	w.t.Helper()
	output, exitCode := w.applyConfig(config, token)
	if exitCode != 0 {
		w.t.Fatalf("the apply exited with status %d:\n%s", exitCode, output)
	}
	if strings.Contains(output, token) {
		w.t.Fatal("the apply output contains the header value")
	}
	w.requireStateOmitsIfPresent(token)
}

func (w *tofuWorkspace) requireStateOmitsIfPresent(token string) {
	w.t.Helper()
	if token == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(w.directory, stateFileName)); err != nil {
		return
	}
	w.requireOmits(stateFileName, token)
}

func requestHeaderPayload(t *testing.T) ([]byte, string) {
	t.Helper()
	payload := make([]byte, requestHeaderBytes)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return payload, bytesHash(payload)
}

func TestAccDownloadRequestHeader(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload, hash := requestHeaderPayload(t)
	recorder := guest.serveWithRequestHeader(t, payload, requestHeaderToken)
	path := testDirectory + "/request-header/private.bin"
	url := recorder.url + requestHeaderObject
	download := func(token string, version int) string {
		return guest.providerBlock() +
			guest.controllerDownloadBlock("test", url, hash, path, requestHeaderArguments(token, version))
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: download(requestHeaderToken, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "file_sha256", hash),
					resource.TestCheckResourceAttr("pveguest_download.test", "request_headers_wo_version", "1"),
					resource.TestCheckNoResourceAttr("pveguest_download.test", "request_headers_wo.%"),
					checkStateOmits(requestHeaderToken),
					checkGuest(func() error {
						if recorder.authorized.Load() != 1 {
							return fmt.Errorf("the server answered %d requests with the header, want 1", recorder.authorized.Load())
						}
						return guest.expectFileHash(path, hash)()
					}),
				),
			},
			{
				Config: download(requestHeaderRotated, 1),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				Config:           download(requestHeaderRotated, 2),
				ConfigPlanChecks: expectAction("pveguest_download.test", plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "request_headers_wo_version", "2"),
					checkStateOmits(requestHeaderRotated),
					checkGuest(guest.expectFileHash(path, hash)),
				),
			},
		},
	})
}

func TestAccDownloadRequestHeaderCacheMissRejected(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload, hash := requestHeaderPayload(t)
	recorder := guest.serveWithRequestHeader(t, payload, requestHeaderToken)
	path := testDirectory + "/request-header/rejected.bin"
	config := guest.workspaceConfig("controller", recorder.url+requestHeaderObject, hash, path, 1, true)
	workspace := newHeaderWorkspace(t)
	workspace.writeConfig(config)
	workspace.mustRun("init", "-no-color")
	recorder.clearCache(t)

	workspace.requireRejected(config, requestHeaderWrong, controllerRejection)

	if recorder.rejected.Load() == 0 || recorder.authorized.Load() != 0 {
		t.Fatalf("server counts: %d rejected, %d authorized", recorder.rejected.Load(), recorder.authorized.Load())
	}
	if guest.exists(path) {
		t.Fatalf("%s exists after the rejected download", path)
	}
}

func TestAccDownloadRequestHeaderRotation(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload, hash := requestHeaderPayload(t)
	recorder := guest.serveWithRequestHeader(t, payload, requestHeaderToken)
	path := testDirectory + "/request-header/rotation.bin"
	url := recorder.url + requestHeaderObject
	firstConfig := guest.workspaceConfig("controller", url, hash, path, 1, true)
	rotatedConfig := guest.workspaceConfig("controller", url, hash, path, 2, true)
	workspace := newHeaderWorkspace(t)
	workspace.writeConfig(firstConfig)
	workspace.mustRun("init", "-no-color")

	workspace.requireApplied(firstConfig, requestHeaderToken)
	if err := guest.expectFileHash(path, hash)(); err != nil {
		t.Fatal(err)
	}

	recorder.accept(requestHeaderRotated)
	recorder.clearCache(t)
	workspace.requireRejected(rotatedConfig, requestHeaderToken, controllerRejection)

	recorder.clearCache(t)
	authorizedBefore := recorder.authorized.Load()
	workspace.requireApplied(rotatedConfig, requestHeaderRotated)
	if got := recorder.authorized.Load() - authorizedBefore; got != 1 {
		t.Fatalf("the rotated token made %d authorized requests, want 1", got)
	}
	if err := guest.expectFileHash(path, hash)(); err != nil {
		t.Fatal(err)
	}
	workspace.requireOmits(stateFileName, requestHeaderToken)

	workspace.mustRun("destroy", "-no-color", "-auto-approve")
}

func TestAccDownloadGuestRequestHeader(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload, hash := requestHeaderPayload(t)
	server := guest.startHeaderServer(t, payload, requestHeaderToken)
	path := testDirectory + "/request-header/guest.bin"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: guest.providerBlock() +
					guest.guestDownloadBlock("test", server.url, hash, path, requestHeaderArguments(requestHeaderToken, 1)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "file_sha256", hash),
					resource.TestCheckNoResourceAttr("pveguest_download.test", "request_headers_wo.%"),
					checkStateOmits(requestHeaderToken),
					checkGuest(server.checkProcessScan),
					checkGuest(guest.expectFileHash(path, hash)),
				),
			},
		},
	})
}

func TestAccDownloadGuestRequestHeaderRejected(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload, hash := requestHeaderPayload(t)
	server := guest.startHeaderServer(t, payload, requestHeaderToken)
	path := testDirectory + "/request-header/guest-rejected.bin"
	withHeaders := guest.workspaceConfig("guest", server.url, hash, path, 1, true)
	withoutHeaders := guest.workspaceConfig("guest", server.url, hash, path, 1, false)
	workspace := newHeaderWorkspace(t)
	workspace.writeConfig(withHeaders)
	workspace.mustRun("init", "-no-color")

	workspace.requireRejected(withHeaders, requestHeaderWrong, guestRejection)
	workspace.requireRejected(withoutHeaders, "", guestRejection)

	state := server.state()
	if state.Authorized != 0 || state.Rejected < 2 {
		t.Fatalf("guest server counts: %d authorized, %d rejected", state.Authorized, state.Rejected)
	}
	if guest.exists(path) {
		t.Fatalf("%s exists after the rejected downloads", path)
	}

	workspace.requireApplied(withHeaders, requestHeaderToken)
	if err := guest.expectFileHash(path, hash)(); err != nil {
		t.Fatal(err)
	}
	if err := server.checkProcessScan(); err != nil {
		t.Fatal(err)
	}
	workspace.mustRun("destroy", "-no-color", "-auto-approve")
}
