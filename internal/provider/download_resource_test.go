package provider_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
)

const (
	headerValidationType   = "pveguest_download"
	headerValidationName   = "X-Pveguest-Token"
	headerValidationValue  = "fixture-token-value-7f3a"
	headerValidationURL    = "https://objects.example.net/object"
	headerValidationSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	headerValidationPath   = "/opt/tool/tool.bin"
	headerValidationVMID   = 101
)

type headerValidationSettings struct {
	headers map[string]string
	version int64
}

func headerValidationServer(t *testing.T) (tfprotov6.ProviderServer, tftypes.Type) {
	t.Helper()
	server, err := providerserver.NewProtocol6WithError(provider.New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[headerValidationType]
	if !found {
		t.Fatalf("provider has no %s resource", headerValidationType)
	}
	return server, resourceSchema.ValueType()
}

func headerValidationConfig(
	t *testing.T,
	resourceType tftypes.Type,
	settings headerValidationSettings,
) *tfprotov6.DynamicValue {
	t.Helper()
	unsetString := tftypes.NewValue(tftypes.String, nil)
	headers := tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)
	if settings.headers != nil {
		values := make(map[string]tftypes.Value, len(settings.headers))
		for name, value := range settings.headers {
			values[name] = tftypes.NewValue(tftypes.String, value)
		}
		headers = tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, values)
	}
	version := tftypes.NewValue(tftypes.Number, nil)
	if settings.version != 0 {
		version = tftypes.NewValue(tftypes.Number, settings.version)
	}
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":                       tftypes.NewValue(tftypes.String, "pve"),
		"vmid":                       tftypes.NewValue(tftypes.Number, headerValidationVMID),
		"kind":                       tftypes.NewValue(tftypes.String, "lxc"),
		"url":                        tftypes.NewValue(tftypes.String, headerValidationURL),
		"sha256":                     tftypes.NewValue(tftypes.String, headerValidationSHA256),
		"fetch":                      tftypes.NewValue(tftypes.String, "controller"),
		"archive_member":             unsetString,
		"request_headers_wo":         headers,
		"request_headers_wo_version": version,
		"path":                       tftypes.NewValue(tftypes.String, headerValidationPath),
		"mode":                       unsetString,
		"owner":                      unsetString,
		"group":                      unsetString,
		"file_sha256":                unsetString,
		"write_id":                   unsetString,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func headerValidationDiagnostics(
	t *testing.T,
	settings headerValidationSettings,
) []*tfprotov6.Diagnostic {
	t.Helper()
	server, resourceType := headerValidationServer(t)
	response, err := server.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
		TypeName:           headerValidationType,
		Config:             headerValidationConfig(t, resourceType, settings),
		ClientCapabilities: &tfprotov6.ValidateResourceConfigClientCapabilities{WriteOnlyAttributesAllowed: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return response.Diagnostics
}

func headerValidationText(diagnostics []*tfprotov6.Diagnostic) []byte {
	var text bytes.Buffer
	for _, diagnostic := range diagnostics {
		text.WriteString(diagnostic.Summary)
		text.WriteString(diagnostic.Detail)
	}
	return text.Bytes()
}

func TestDownloadValidatesRequestHeaders(t *testing.T) {
	valid := headerValidationSettings{headers: map[string]string{headerValidationName: headerValidationValue}, version: 1}
	if diagnostics := headerValidationDiagnostics(t, valid); len(diagnostics) != 0 {
		t.Fatalf("valid headers returned %d diagnostics: %s", len(diagnostics), diagnostics[0].Detail)
	}

	invalid := map[string]headerValidationSettings{
		"carriage return and line feed in a value": {
			headers: map[string]string{headerValidationName: headerValidationValue + "\r\nX-Injected: yes"}, version: 1,
		},
		"space in a name":         {headers: map[string]string{"Bad Name": headerValidationValue}, version: 1},
		"missing version":         {headers: valid.headers},
		"version without headers": {version: 1},
	}
	for name, settings := range invalid {
		diagnostics := headerValidationDiagnostics(t, settings)
		if len(diagnostics) == 0 {
			t.Fatalf("%s: validation returned no diagnostics", name)
		}
		if bytes.Contains(headerValidationText(diagnostics), []byte(headerValidationValue)) {
			t.Fatalf("%s: a diagnostic contains the header value", name)
		}
	}

	valueDiagnostics := headerValidationDiagnostics(t, invalid["carriage return and line feed in a value"])
	if len(valueDiagnostics) != 1 || !strings.Contains(valueDiagnostics[0].Detail, headerValidationName) {
		t.Fatal("the value diagnostic does not name the header")
	}
}
