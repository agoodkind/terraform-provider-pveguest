package provider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
)

const (
	testKernelModulesNode  = "pve"
	testKernelModulesToken = "root@pam!test=secret"
	testKernelModulesJSON  = `{"data":{"modules":["loaded_one","unloaded_one"],"loaded":{"loaded_one":1,"unloaded_one":0}}}`
	kernelModulesType      = "pveguest_host_kernel_modules"
)

func findResourceType(t *testing.T, server tfprotov6.ProviderServer) tftypes.Type {
	t.Helper()
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[kernelModulesType]
	if !found {
		t.Fatalf("provider has no %s resource", kernelModulesType)
	}
	return resourceSchema.ValueType()
}

func TestHostKernelModulesReadDropsModuleThatIsNotLoaded(t *testing.T) {
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(testKernelModulesJSON))
	}))
	defer api.Close()

	server, err := providerserver.NewProtocol6WithError(provider.New("test")())()
	if err != nil {
		t.Fatal(err)
	}

	schemaResponse, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	providerType := schemaResponse.Provider.ValueType()
	nodeType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"endpoint":  tftypes.String,
		"api_token": tftypes.String,
		"insecure":  tftypes.Bool,
		"node_name": tftypes.String,
	}}
	node := tftypes.NewValue(nodeType, map[string]tftypes.Value{
		"endpoint":  tftypes.NewValue(tftypes.String, api.URL),
		"api_token": tftypes.NewValue(tftypes.String, testKernelModulesToken),
		"insecure":  tftypes.NewValue(tftypes.Bool, nil),
		"node_name": tftypes.NewValue(tftypes.String, nil),
	})
	providerConfig, err := tfprotov6.NewDynamicValue(providerType, tftypes.NewValue(providerType, map[string]tftypes.Value{
		"nodes": tftypes.NewValue(tftypes.Map{ElementType: nodeType}, map[string]tftypes.Value{
			testKernelModulesNode: node,
		}),
		"max_requests":       tftypes.NewValue(tftypes.Number, nil),
		"controller_ca_file": tftypes.NewValue(tftypes.String, nil),
	}))
	if err != nil {
		t.Fatal(err)
	}
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: &providerConfig})
	if err != nil {
		t.Fatal(err)
	}
	if len(configured.Diagnostics) > 0 {
		t.Fatalf("configure provider: %v", configured.Diagnostics[0].Detail)
	}

	resourceType := findResourceType(t, server)
	setType := tftypes.Set{ElementType: tftypes.String}
	mapType := tftypes.Map{ElementType: tftypes.Bool}
	prior, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node": tftypes.NewValue(tftypes.String, testKernelModulesNode),
		"modules": tftypes.NewValue(setType, []tftypes.Value{
			tftypes.NewValue(tftypes.String, "loaded_one"),
			tftypes.NewValue(tftypes.String, "unloaded_one"),
		}),
		"loaded": tftypes.NewValue(mapType, nil),
	}))
	if err != nil {
		t.Fatal(err)
	}

	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
		TypeName:     kernelModulesType,
		CurrentState: &prior,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Diagnostics) > 0 {
		t.Fatalf("read resource: %v", read.Diagnostics[0].Detail)
	}

	refreshed, err := read.NewState.Unmarshal(resourceType)
	if err != nil {
		t.Fatal(err)
	}
	attributes := make(map[string]tftypes.Value)
	if err := refreshed.As(&attributes); err != nil {
		t.Fatal(err)
	}
	var modules []tftypes.Value
	if err := attributes["modules"].As(&modules); err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || !modules[0].Equal(tftypes.NewValue(tftypes.String, "loaded_one")) {
		t.Fatalf("modules = %v, want only loaded_one", modules)
	}
	var loaded map[string]tftypes.Value
	if err := attributes["loaded"].As(&loaded); err != nil {
		t.Fatal(err)
	}
	var unloadedState bool
	if err := loaded["unloaded_one"].As(&unloadedState); err != nil {
		t.Fatal(err)
	}
	if unloadedState {
		t.Fatal("unloaded_one is reported as loaded")
	}
}
