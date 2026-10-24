package provider_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
)

func nullObjectAttributes(valueType tftypes.Type) map[string]tftypes.Value {
	objectType := valueType.(tftypes.Object)
	attributes := make(map[string]tftypes.Value, len(objectType.AttributeTypes))
	for name, attributeType := range objectType.AttributeTypes {
		attributes[name] = tftypes.NewValue(attributeType, nil)
	}
	return attributes
}

func kernelModulesDataSourceServer(t *testing.T) (tfprotov6.ProviderServer, tftypes.Type) {
	t.Helper()
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(provider.New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	providerType := schemas.Provider.ValueType()
	attributes := nullObjectAttributes(providerType)
	nodesType := attributes["nodes"].Type().(tftypes.Map)
	nodeAttributes := nullObjectAttributes(nodesType.ElementType)
	nodeAttributes["endpoint"] = tftypes.NewValue(tftypes.String, "http://127.0.0.1:1")
	nodeAttributes["api_token"] = tftypes.NewValue(tftypes.String, testKernelModulesToken)
	attributes["nodes"] = tftypes.NewValue(nodesType, map[string]tftypes.Value{
		testKernelModulesNode: tftypes.NewValue(nodesType.ElementType, nodeAttributes),
	})
	config, err := tfprotov6.NewDynamicValue(providerType, tftypes.NewValue(providerType, attributes))
	if err != nil {
		t.Fatal(err)
	}
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: &config})
	if err != nil {
		t.Fatal(err)
	}
	if len(configured.Diagnostics) > 0 {
		t.Fatalf("configure provider: %v", configured.Diagnostics)
	}
	return server, schemas.DataSourceSchemas[kernelModulesType].ValueType()
}

func TestHostKernelModulesDataSourceRejectsUnknownNode(t *testing.T) {
	server, dataType := kernelModulesDataSourceServer(t)
	attributes := nullObjectAttributes(dataType)
	attributes["node"] = tftypes.NewValue(tftypes.String, "missing")
	config, err := tfprotov6.NewDynamicValue(dataType, tftypes.NewValue(dataType, attributes))
	if err != nil {
		t.Fatal(err)
	}
	read, err := server.ReadDataSource(context.Background(), &tfprotov6.ReadDataSourceRequest{
		TypeName: kernelModulesType,
		Config:   &config,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range read.Diagnostics {
		if diagnostic.Severity == tfprotov6.DiagnosticSeverityError &&
			strings.Contains(diagnostic.Detail, `node "missing" is not in the provider nodes map`) {
			return
		}
	}
	t.Fatalf("read diagnostics = %v, want an unknown-node error", read.Diagnostics)
}
