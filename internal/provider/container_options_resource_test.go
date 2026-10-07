package provider_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
)

const (
	optionsNode    = "pve"
	optionsToken   = "root@pam!test=secret"
	optionsVMID    = 101
	optionsType    = "pveguest_container_options"
	configPath     = "/api2/json/nodes/pve/lxc/101/config"
	pendingPath    = "/api2/json/nodes/pve/lxc/101/pending"
	declaredValue  = "link=nic2,name=wan"
	handChanged    = "link=nic9,name=wan"
	secondNicValue = "link=nic3,name=lan"
)

const (
	driftedConfigJSON = `{"data":{"memory":512,"hostnic0":"link=nic9,name=wan"}}`
	pendingJSON       = `{"data":[{"key":"memory","value":512},{"key":"hostnic0","value":"link=nic2,name=wan","pending":"link=nic9,name=wan"}]}`
)

type optionsAPI struct {
	mutex    sync.Mutex
	putBody  map[string]string
	putCount int
}

func (api *optionsAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	switch {
	case request.Method == http.MethodGet && request.URL.Path == configPath:
		_, _ = writer.Write([]byte(driftedConfigJSON))
	case request.Method == http.MethodGet && request.URL.Path == pendingPath:
		_, _ = writer.Write([]byte(pendingJSON))
	case request.Method == http.MethodPut && request.URL.Path == configPath:
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &api.putBody)
		api.putCount++
		_, _ = writer.Write([]byte(`{"data":null}`))
	default:
		http.NotFound(writer, request)
	}
}

func configuredOptionsServer(t *testing.T, endpoint string) tfprotov6.ProviderServer {
	t.Helper()
	ctx := context.Background()
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
		"endpoint":  tftypes.NewValue(tftypes.String, endpoint),
		"api_token": tftypes.NewValue(tftypes.String, optionsToken),
		"insecure":  tftypes.NewValue(tftypes.Bool, nil),
		"node_name": tftypes.NewValue(tftypes.String, nil),
	})
	providerConfig, err := tfprotov6.NewDynamicValue(providerType, tftypes.NewValue(providerType, map[string]tftypes.Value{
		"nodes": tftypes.NewValue(tftypes.Map{ElementType: nodeType}, map[string]tftypes.Value{
			optionsNode: node,
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
	return server
}

func containerOptionsType(t *testing.T, server tfprotov6.ProviderServer) tftypes.Type {
	t.Helper()
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[optionsType]
	if !found {
		t.Fatalf("provider has no %s resource", optionsType)
	}
	return resourceSchema.ValueType()
}

func stringMap(entries map[string]string) tftypes.Value {
	values := make(map[string]tftypes.Value, len(entries))
	for key, value := range entries {
		values[key] = tftypes.NewValue(tftypes.String, value)
	}
	return tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, values)
}

func optionsResource(
	t *testing.T,
	resourceType tftypes.Type,
	options map[string]string,
	pending tftypes.Value,
) *tfprotov6.DynamicValue {
	t.Helper()
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":    tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":    tftypes.NewValue(tftypes.Number, optionsVMID),
		"options": stringMap(options),
		"pending": pending,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func stringsOf(t *testing.T, value tftypes.Value) map[string]string {
	t.Helper()
	members := make(map[string]tftypes.Value)
	if err := value.As(&members); err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]string, len(members))
	for key, member := range members {
		var text string
		if err := member.As(&text); err != nil {
			t.Fatal(err)
		}
		entries[key] = text
	}
	return entries
}

func nullPending() tftypes.Value {
	return tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)
}

func TestContainerOptionsChangedValuePlansUpdateAndPendingIsRead(t *testing.T) {
	ctx := context.Background()
	api := httptest.NewServer(&optionsAPI{})
	defer api.Close()
	server := configuredOptionsServer(t, api.URL)
	resourceType := containerOptionsType(t, server)

	declared := map[string]string{"hostnic0": declaredValue}
	prior := optionsResource(t, resourceType, declared, nullPending())
	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: optionsType, CurrentState: prior})
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
	options := stringsOf(t, attributes["options"])
	if len(options) != 1 || options["hostnic0"] != handChanged {
		t.Fatalf("options = %v, want only hostnic0 = %q", options, handChanged)
	}
	pending := stringsOf(t, attributes["pending"])
	if len(pending) != 1 || pending["hostnic0"] != handChanged {
		t.Fatalf("pending = %v, want only hostnic0 = %q", pending, handChanged)
	}

	config := optionsResource(t, resourceType, declared, nullPending())
	plan, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         optionsType,
		PriorState:       read.NewState,
		ProposedNewState: optionsResource(t, resourceType, declared, attributes["pending"]),
		Config:           config,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Diagnostics) > 0 {
		t.Fatalf("plan resource change: %v", plan.Diagnostics[0].Detail)
	}
	planned, err := plan.PlannedState.Unmarshal(resourceType)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Equal(refreshed) {
		t.Fatal("the planned state equals the refreshed state; expected an update")
	}
	if len(plan.RequiresReplace) > 0 {
		t.Fatalf("the plan requires replacement at %v", plan.RequiresReplace)
	}
}

func TestContainerOptionsUpdateDeletesKeyThatLeftTheConfiguration(t *testing.T) {
	ctx := context.Background()
	handler := &optionsAPI{}
	api := httptest.NewServer(handler)
	defer api.Close()
	server := configuredOptionsServer(t, api.URL)
	resourceType := containerOptionsType(t, server)

	prior := optionsResource(t, resourceType,
		map[string]string{"hostnic0": declaredValue, "hostnic1": secondNicValue}, nullPending())
	planned := optionsResource(t, resourceType,
		map[string]string{"hostnic0": declaredValue},
		tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, tftypes.UnknownValue))
	applied, err := server.ApplyResourceChange(ctx, &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     optionsType,
		PriorState:   prior,
		PlannedState: planned,
		Config:       optionsResource(t, resourceType, map[string]string{"hostnic0": declaredValue}, nullPending()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Diagnostics) > 0 {
		t.Fatalf("apply resource change: %v", applied.Diagnostics[0].Detail)
	}

	handler.mutex.Lock()
	defer handler.mutex.Unlock()
	if handler.putCount != 1 {
		t.Fatalf("PUT count = %d, want 1", handler.putCount)
	}
	if handler.putBody["hostnic0"] != declaredValue || handler.putBody["delete"] != "hostnic1" {
		t.Fatalf("PUT body = %v, want hostnic0 and delete = hostnic1", handler.putBody)
	}
}
