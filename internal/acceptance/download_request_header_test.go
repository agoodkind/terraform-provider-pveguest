package acceptance_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

	controllerAddressVariable = "PVEGUEST_ACC_CONTROLLER_ADDRESS"
	guestCertificatePath      = "/usr/local/share/ca-certificates/pveguest-acc-header.crt"
	guestCertificateLifetime  = time.Hour
	cacheDirectoryName        = "pveguest"
	controllerRejection       = `HTTP status 401`
	guestRejection            = `returned error: 401`
)

type requestHeaderServer struct {
	accepted          atomic.Value
	authorized        atomic.Int64
	rejected          atomic.Int64
	processListChecks atomic.Int64
	processListLeaks  atomic.Int64
	processListErrors atomic.Int64
	inspect           func()
	url               string
	cacheHome         string
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
		if s.inspect != nil {
			s.inspect()
		}
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

func guestServerCertificate(t *testing.T, address net.IP) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pveguest-acc-header"},
		NotBefore:             time.Now().Add(-guestCertificateLifetime),
		NotAfter:              time.Now().Add(guestCertificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{address},
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
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, certificatePEM
}

func (g *testGuest) serveToGuest(t *testing.T, content []byte, token string) *requestHeaderServer {
	t.Helper()
	address := net.ParseIP(requireVariable(t, controllerAddressVariable))
	if address == nil {
		t.Fatalf("%s must be the controller IP address that the guest connects to", controllerAddressVariable)
	}
	recorder := &requestHeaderServer{cacheHome: t.TempDir()}
	recorder.accept(token)
	recorder.inspect = func() { g.inspectProcessList(recorder, token) }

	pair, certificatePEM := guestServerCertificate(t, address)
	listener, err := net.Listen("tcp", net.JoinHostPort(address.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(recorder.handler(content))
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)

	t.Cleanup(func() {
		g.mustRun("rm", "-f", "--", guestCertificatePath)
		g.mustRun("update-ca-certificates", "--fresh")
	})
	g.mustRun("mkdir", "-p", "--", filepath.Dir(guestCertificatePath))
	g.writeFile(guestCertificatePath, string(certificatePEM))
	g.mustRun("update-ca-certificates")

	t.Setenv("XDG_CACHE_HOME", recorder.cacheHome)
	recorder.url = server.URL
	return recorder
}

func (g *testGuest) inspectProcessList(recorder *requestHeaderServer, token string) {
	command := transport.Command{
		Argv:           []string{"env", "LC_ALL=C", "ps", "-eo", "args"},
		TimeoutSeconds: directTimeout,
	}
	result, err := g.client.Run(context.Background(), g.guest, command)
	if err != nil || result.ExitCode != 0 {
		recorder.processListErrors.Add(1)
		return
	}
	recorder.processListChecks.Add(1)
	if bytes.Contains(result.Stdout, []byte(token)) {
		recorder.processListLeaks.Add(1)
	}
}

func (s *requestHeaderServer) checkProcessList() error {
	if s.processListErrors.Load() != 0 || s.processListChecks.Load() == 0 {
		return fmt.Errorf("the guest process list was not read during an authorized request (errors %d, checks %d)",
			s.processListErrors.Load(), s.processListChecks.Load())
	}
	if s.processListLeaks.Load() != 0 {
		return errors.New("a guest process argument list contained the header value")
	}
	return nil
}

type rejection struct {
	expecting atomic.Bool
	pattern   *regexp.Regexp
	secrets   []string
}

func (r *rejection) expect() { r.expecting.Store(true) }

func (r *rejection) clear() { r.expecting.Store(false) }

func (r *rejection) errorCheck(err error) error {
	if !r.expecting.Load() {
		return err
	}
	for _, secret := range r.secrets {
		if strings.Contains(err.Error(), secret) {
			return errors.New("the error text contains a header value")
		}
	}
	if !r.pattern.MatchString(err.Error()) {
		return err
	}
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
	config := guest.providerBlock() + guest.controllerDownloadBlock(
		"test", recorder.url+requestHeaderObject, hash, path, requestHeaderArguments(requestHeaderWrong, 1),
	)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig:   func() { recorder.clearCache(t) },
				Config:      config,
				ExpectError: regexp.MustCompile(controllerRejection),
			},
			{
				PreConfig: func() {
					if recorder.rejected.Load() == 0 || recorder.authorized.Load() != 0 {
						t.Fatalf("server counts: %d rejected, %d authorized", recorder.rejected.Load(), recorder.authorized.Load())
					}
					if guest.exists(path) {
						t.Fatalf("%s exists after the rejected download", path)
					}
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccDownloadRequestHeaderRotation(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload, hash := requestHeaderPayload(t)
	recorder := guest.serveWithRequestHeader(t, payload, requestHeaderToken)
	path := testDirectory + "/request-header/rotation.bin"
	url := recorder.url + requestHeaderObject
	download := func(token string, version int) string {
		return guest.providerBlock() +
			guest.controllerDownloadBlock("test", url, hash, path, requestHeaderArguments(token, version))
	}
	var authorizedBefore int64

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: download(requestHeaderToken, 1),
				Check:  checkGuest(guest.expectFileHash(path, hash)),
			},
			{
				PreConfig: func() {
					recorder.accept(requestHeaderRotated)
					recorder.clearCache(t)
				},
				Config:      download(requestHeaderToken, 2),
				ExpectError: regexp.MustCompile(controllerRejection),
			},
			{
				PreConfig: func() {
					recorder.clearCache(t)
					authorizedBefore = recorder.authorized.Load()
				},
				Config: download(requestHeaderRotated, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "request_headers_wo_version", "2"),
					checkStateOmits(requestHeaderRotated),
					checkGuest(func() error {
						if recorder.authorized.Load() != authorizedBefore+1 {
							return fmt.Errorf("the rotated token made %d authorized requests, want 1",
								recorder.authorized.Load()-authorizedBefore)
						}
						return guest.expectFileHash(path, hash)()
					}),
				),
			},
		},
	})
}

func TestAccDownloadGuestRequestHeader(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload, hash := requestHeaderPayload(t)
	recorder := guest.serveToGuest(t, payload, requestHeaderToken)
	path := testDirectory + "/request-header/guest.bin"
	url := recorder.url + requestHeaderObject

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: guest.providerBlock() +
					guest.guestDownloadBlock("test", url, hash, path, requestHeaderArguments(requestHeaderToken, 1)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("pveguest_download.test", "file_sha256", hash),
					resource.TestCheckNoResourceAttr("pveguest_download.test", "request_headers_wo.%"),
					checkStateOmits(requestHeaderToken),
					checkGuest(recorder.checkProcessList),
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
	recorder := guest.serveToGuest(t, payload, requestHeaderToken)
	url := recorder.url + requestHeaderObject
	wrongPath := testDirectory + "/request-header/wrong.bin"
	missingPath := testDirectory + "/request-header/missing.bin"
	expectation := &rejection{pattern: regexp.MustCompile(guestRejection), secrets: []string{requestHeaderWrong}}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		ErrorCheck:               expectation.errorCheck,
		Steps: []resource.TestStep{
			{
				PreConfig: expectation.expect,
				Config: guest.providerBlock() +
					guest.guestDownloadBlock("test", url, hash, wrongPath, requestHeaderArguments(requestHeaderWrong, 1)),
			},
			{
				PreConfig: expectation.expect,
				Config:    guest.providerBlock() + guest.guestDownloadBlock("test", url, hash, missingPath, ""),
			},
			{
				PreConfig: func() {
					expectation.clear()
					if recorder.authorized.Load() != 0 {
						t.Fatalf("the server authorized %d requests, want 0", recorder.authorized.Load())
					}
					if guest.exists(wrongPath) || guest.exists(missingPath) {
						t.Fatal("a rejected download left a file in the guest")
					}
				},
				Config: guest.providerBlock() +
					guest.guestDownloadBlock("test", url, hash, wrongPath, requestHeaderArguments(requestHeaderToken, 1)),
				Check: checkGuest(guest.expectFileHash(wrongPath, hash)),
			},
		},
	})
}
