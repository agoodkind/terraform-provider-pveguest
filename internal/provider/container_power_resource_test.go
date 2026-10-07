package provider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	powerType        = "pveguest_container_power"
	statusPath       = "/api2/json/nodes/pve/lxc/101/status/current"
	startPath        = "/api2/json/nodes/pve/lxc/101/status/start"
	shutdownPath     = "/api2/json/nodes/pve/lxc/101/status/shutdown"
	stopPath         = "/api2/json/nodes/pve/lxc/101/status/stop"
	taskStatusPath   = "/api2/json/nodes/pve/tasks/UPID:pve:1/status"
	taskLogPath      = "/api2/json/nodes/pve/tasks/UPID:pve:1/log"
	taskIdentifier   = `{"data":"UPID:pve:1"}`
	taskOKJSON       = `{"data":{"status":"stopped","exitstatus":"OK"}}`
	taskFailedJSON   = `{"data":{"status":"stopped","exitstatus":"command failed"}}`
	taskLogJSON      = `{"data":[{"n":1,"t":"starting"},{"n":2,"t":"hostnic0 link nic9 is missing"}]}`
	stoppedStatus    = "stopped"
	runningStatus    = "running"
	failedTaskDetail = "hostnic0 link nic9 is missing"
)

type powerAPI struct {
	mutex    sync.Mutex
	status   string
	failTask bool
	posts    []string
}

func (api *powerAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	path := request.URL.Path
	switch {
	case request.Method == http.MethodGet && path == statusPath:
		_, _ = writer.Write([]byte(`{"data":{"status":"` + api.status + `"}}`))
	case request.Method == http.MethodPost && (path == startPath || path == shutdownPath || path == stopPath):
		api.posts = append(api.posts, path[strings.LastIndex(path, "/")+1:])
		api.finishOperation(path)
		_, _ = writer.Write([]byte(taskIdentifier))
	case request.Method == http.MethodGet && path == taskStatusPath:
		if api.failTask {
			_, _ = writer.Write([]byte(taskFailedJSON))
			return
		}
		_, _ = writer.Write([]byte(taskOKJSON))
	case request.Method == http.MethodGet && path == taskLogPath:
		_, _ = writer.Write([]byte(taskLogJSON))
	default:
		http.NotFound(writer, request)
	}
}

func (api *powerAPI) finishOperation(path string) {
	if api.failTask {
		return
	}
	if path == startPath {
		api.status = runningStatus
		return
	}
	api.status = stoppedStatus
}

func (api *powerAPI) recordedPosts() []string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return append([]string(nil), api.posts...)
}

func (api *powerAPI) currentStatus() string {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	return api.status
}

func powerResourceType(t *testing.T, server tfprotov6.ProviderServer) tftypes.Type {
	t.Helper()
	response, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resourceSchema, found := response.ResourceSchemas[powerType]
	if !found {
		t.Fatalf("provider has no %s resource", powerType)
	}
	return resourceSchema.ValueType()
}

func powerResource(
	t *testing.T,
	resourceType tftypes.Type,
	running bool,
	restartOn map[string]string,
	status tftypes.Value,
) *tfprotov6.DynamicValue {
	t.Helper()
	restart := tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil)
	if restartOn != nil {
		restart = stringMap(restartOn)
	}
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, map[string]tftypes.Value{
		"node":       tftypes.NewValue(tftypes.String, optionsNode),
		"vmid":       tftypes.NewValue(tftypes.Number, optionsVMID),
		"running":    tftypes.NewValue(tftypes.Bool, running),
		"restart_on": restart,
		"status":     status,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}

func statusValue(text string) tftypes.Value {
	return tftypes.NewValue(tftypes.String, text)
}

func applyPower(
	t *testing.T,
	server tfprotov6.ProviderServer,
	prior *tfprotov6.DynamicValue,
	planned *tfprotov6.DynamicValue,
	config *tfprotov6.DynamicValue,
) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	applied, err := server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     powerType,
		PriorState:   prior,
		PlannedState: planned,
		Config:       config,
	})
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

func TestContainerPowerStoppedContainerPlansUpdateAndStartsOnce(t *testing.T) {
	ctx := context.Background()
	handler := &powerAPI{status: stoppedStatus}
	api := httptest.NewServer(handler)
	defer api.Close()
	server := configuredOptionsServer(t, api.URL)
	resourceType := powerResourceType(t, server)

	prior := powerResource(t, resourceType, true, nil, statusValue(runningStatus))
	read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{TypeName: powerType, CurrentState: prior})
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
	var running bool
	if err := attributes["running"].As(&running); err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("running = true after reading a stopped container, want false")
	}

	config := powerResource(t, resourceType, true, nil, tftypes.NewValue(tftypes.String, nil))
	plan, err := server.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         powerType,
		PriorState:       read.NewState,
		ProposedNewState: powerResource(t, resourceType, true, nil, attributes["status"]),
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

	applied := applyPower(t, server, read.NewState, plan.PlannedState, config)
	if len(applied.Diagnostics) > 0 {
		t.Fatalf("apply resource change: %v", applied.Diagnostics[0].Detail)
	}
	posts := handler.recordedPosts()
	if len(posts) != 1 || posts[0] != "start" {
		t.Fatalf("POST requests = %v, want one start", posts)
	}
	if status := handler.currentStatus(); status != runningStatus {
		t.Fatalf("container status = %q after apply, want %q", status, runningStatus)
	}
	requireAppliedStatus(t, resourceType, applied, runningStatus)
}

func TestContainerPowerChangedRestartOnShutsDownThenStarts(t *testing.T) {
	handler := &powerAPI{status: runningStatus}
	api := httptest.NewServer(handler)
	defer api.Close()
	server := configuredOptionsServer(t, api.URL)
	resourceType := powerResourceType(t, server)

	prior := powerResource(t, resourceType, true, map[string]string{"write_id": "one"}, statusValue(runningStatus))
	planned := powerResource(t, resourceType, true, map[string]string{"write_id": "two"},
		tftypes.NewValue(tftypes.String, tftypes.UnknownValue))
	config := powerResource(t, resourceType, true, map[string]string{"write_id": "two"},
		tftypes.NewValue(tftypes.String, nil))
	applied := applyPower(t, server, prior, planned, config)
	if len(applied.Diagnostics) > 0 {
		t.Fatalf("apply resource change: %v", applied.Diagnostics[0].Detail)
	}
	posts := handler.recordedPosts()
	if len(posts) != 2 || posts[0] != "shutdown" || posts[1] != "start" {
		t.Fatalf("POST requests = %v, want shutdown then start", posts)
	}
	if status := handler.currentStatus(); status != runningStatus {
		t.Fatalf("container status = %q after restart, want %q", status, runningStatus)
	}
	requireAppliedStatus(t, resourceType, applied, runningStatus)
}

func requireAppliedStatus(
	t *testing.T,
	resourceType tftypes.Type,
	applied *tfprotov6.ApplyResourceChangeResponse,
	want string,
) {
	t.Helper()
	state, err := applied.NewState.Unmarshal(resourceType)
	if err != nil {
		t.Fatal(err)
	}
	attributes := make(map[string]tftypes.Value)
	if err := state.As(&attributes); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := attributes["status"].As(&status); err != nil {
		t.Fatal(err)
	}
	if status != want {
		t.Fatalf("status attribute = %q after apply, want %q", status, want)
	}
}

func TestContainerPowerFailedStartTaskReturnsLogTail(t *testing.T) {
	handler := &powerAPI{status: stoppedStatus, failTask: true}
	api := httptest.NewServer(handler)
	defer api.Close()
	server := configuredOptionsServer(t, api.URL)
	resourceType := powerResourceType(t, server)

	planned := powerResource(t, resourceType, true, nil,
		tftypes.NewValue(tftypes.String, tftypes.UnknownValue))
	config := powerResource(t, resourceType, true, nil, tftypes.NewValue(tftypes.String, nil))
	null := powerResourceNull(t, resourceType)
	applied := applyPower(t, server, null, planned, config)
	if len(applied.Diagnostics) == 0 {
		t.Fatal("apply resource change returned no diagnostics for a failed start task")
	}
	if !strings.Contains(applied.Diagnostics[0].Detail, failedTaskDetail) {
		t.Fatalf("diagnostic = %q, want the log tail %q", applied.Diagnostics[0].Detail, failedTaskDetail)
	}
}

func powerResourceNull(t *testing.T, resourceType tftypes.Type) *tfprotov6.DynamicValue {
	t.Helper()
	value, err := tfprotov6.NewDynamicValue(resourceType, tftypes.NewValue(resourceType, nil))
	if err != nil {
		t.Fatal(err)
	}
	return &value
}
