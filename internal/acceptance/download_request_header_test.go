package acceptance_test

import (
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

const (
	requestHeaderName     = "Authorization"
	requestHeaderPrefix   = "Bearer "
	requestHeaderToken    = "acc-request-header-token-4d81"
	requestHeaderRotated  = "acc-request-header-token-9c27"
	requestHeaderObject   = "/private.bin"
	requestHeaderBytes    = 4096
	requestHeaderCAMode   = 0o600
	requestHeaderCAName   = "controller-ca.pem"
	requestHeaderPEMBlock = "CERTIFICATE"
)

type requestHeaderServer struct {
	authorized atomic.Int64
	rejected   atomic.Int64
	url        string
}

func (g *testGuest) serveWithRequestHeader(t *testing.T, content []byte, token string) *requestHeaderServer {
	t.Helper()
	recorder := &requestHeaderServer{}
	mux := http.NewServeMux()
	mux.HandleFunc(requestHeaderObject, func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get(requestHeaderName) != requestHeaderPrefix+token {
			recorder.rejected.Add(1)
			http.Error(writer, "missing or wrong Authorization header", http.StatusUnauthorized)
			return
		}
		recorder.authorized.Add(1)
		_, _ = writer.Write(content)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	certificate := pem.EncodeToMemory(&pem.Block{Type: requestHeaderPEMBlock, Bytes: server.Certificate().Raw})
	caFile := filepath.Join(t.TempDir(), requestHeaderCAName)
	if err := os.WriteFile(caFile, certificate, requestHeaderCAMode); err != nil {
		t.Fatal(err)
	}
	g.controllerCAFile = caFile
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	recorder.url = server.URL
	return recorder
}

func requestHeaderArguments(token string, version int) string {
	return fmt.Sprintf(`request_headers_wo         = { %s = %q }
  request_headers_wo_version = %d`, requestHeaderName, requestHeaderPrefix+token, version)
}

func TestAccDownloadRequestHeader(t *testing.T) {
	guest := newTestGuest(t)
	guest.useTestDirectory()
	payload := make([]byte, requestHeaderBytes)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	hash := bytesHash(payload)
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
