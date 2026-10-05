package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/agoodkind/terraform-provider-pveguest/internal/provider"
	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	acceptanceVariable = "TF_ACC"
	nodeVariable       = "PVEGUEST_ACC_NODE"
	hostVariable       = "PVEGUEST_ACC_HOST"
	vmidVariable       = "PVEGUEST_ACC_VMID"
	kindVariable       = "PVEGUEST_ACC_KIND"

	providerSource = "tofu.home.arpa/agoodkind/pveguest"

	// The tests create and delete only these paths, this package, and
	// these units in the guest.
	testDirectory   = "/root/pveguest-acc"
	testPackage     = "hello"
	testTimer       = "pveguest-acc.timer"
	testService     = "pveguest-acc.service"
	unitDirectory   = "/etc/systemd/system"
	directTimeout   = 600
	testTimerUnit   = "[Unit]\nDescription=pveguest acceptance test timer\n\n[Timer]\nOnCalendar=*-01-01 00:00:00\n\n[Install]\nWantedBy=timers.target\n"
	testServiceUnit = "[Unit]\nDescription=pveguest acceptance test service\n\n[Service]\nType=oneshot\nExecStart=/bin/true\n"
)

var protoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"pveguest": providerserver.NewProtocol6WithError(provider.New("test")()),
}

// testGuest runs commands in the guest directly, outside the provider, for
// setup, hand edits, assertions, and cleanup.
type testGuest struct {
	t     *testing.T
	pool  *transport.Pool
	guest transport.Guest
	host  string
}

func newTestGuest(t *testing.T) *testGuest {
	t.Helper()
	if os.Getenv(acceptanceVariable) == "" {
		t.Skipf("%s is not set", acceptanceVariable)
	}
	node := requireVariable(t, nodeVariable)
	host := requireVariable(t, hostVariable)
	kind := requireVariable(t, kindVariable)
	vmid, err := strconv.ParseInt(requireVariable(t, vmidVariable), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", vmidVariable, err)
	}

	nodes := map[string]transport.NodeConfig{
		node: {Host: host, Port: transport.DefaultPort, User: transport.DefaultUser},
	}
	pool, err := transport.NewPool(nodes, transport.DefaultMaxSessions)
	if err != nil {
		t.Fatal(err)
	}
	handle := &testGuest{
		t:     t,
		pool:  pool,
		guest: transport.Guest{Node: node, VMID: vmid, Kind: transport.Kind(kind)},
		host:  host,
	}
	// Cleanups run in reverse order: the pool closes after every guest
	// cleanup that a test registers later.
	t.Cleanup(pool.Close)
	return handle
}

func requireVariable(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must be set for acceptance tests", name)
	}
	return value
}

func (g *testGuest) run(stdin []byte, argv ...string) transport.Result {
	g.t.Helper()
	command := transport.Command{
		Argv:           append([]string{"env", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}, argv...),
		Stdin:          stdin,
		TimeoutSeconds: directTimeout,
	}
	result, err := g.pool.Run(context.Background(), g.guest, command)
	if err != nil {
		g.t.Fatalf("run %q: %v", argv, err)
	}
	return result
}

func (g *testGuest) mustRun(argv ...string) string {
	g.t.Helper()
	result := g.run(nil, argv...)
	if result.ExitCode != 0 {
		g.t.Fatalf("%q exited with status %d: %s", argv, result.ExitCode, result.Stderr)
	}
	return string(result.Stdout)
}

func (g *testGuest) writeFile(path string, content string) {
	g.t.Helper()
	result := g.run([]byte(content), "sh", "-c", `cat > "$1"`, "sh", path)
	if result.ExitCode != 0 {
		g.t.Fatalf("write %s: status %d: %s", path, result.ExitCode, result.Stderr)
	}
}

func (g *testGuest) readFile(path string) string {
	g.t.Helper()
	return g.mustRun("cat", "--", path)
}

func (g *testGuest) exists(path string) bool {
	g.t.Helper()
	// test -e follows links; -L also accepts a dangling link.
	result := g.run(nil, "sh", "-c", `test -e "$1" || test -L "$1"`, "sh", path)
	return result.ExitCode == 0
}

func (g *testGuest) packageInstalled(name string) bool {
	g.t.Helper()
	result := g.run(nil, "dpkg-query", "--show", "--showformat", "${db:Status-Abbrev}", "--", name)
	return result.ExitCode == 0 && strings.HasPrefix(string(result.Stdout), "ii")
}

func (g *testGuest) systemctlState(verb string, unit string) string {
	g.t.Helper()
	result := g.run(nil, "systemctl", verb, "--", unit)
	return strings.TrimSpace(string(result.Stdout))
}

// useTestDirectory creates the test directory and deletes it after the test.
func (g *testGuest) useTestDirectory() {
	g.t.Helper()
	g.t.Cleanup(func() { g.mustRun("rm", "-rf", "--", testDirectory) })
	g.mustRun("mkdir", "-p", "--", testDirectory)
}

// useTestUnits writes the two test units and removes them after the test.
func (g *testGuest) useTestUnits() {
	g.t.Helper()
	g.t.Cleanup(g.removeTestUnits)
	g.writeFile(unitDirectory+"/"+testTimer, testTimerUnit)
	g.writeFile(unitDirectory+"/"+testService, testServiceUnit)
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) removeTestUnits() {
	g.t.Helper()
	// disable and stop exit nonzero for a unit file that a test already
	// deleted; the removal below still has to run.
	g.run(nil, "systemctl", "disable", "--", testTimer)
	g.run(nil, "systemctl", "stop", "--", testTimer)
	g.run(nil, "systemctl", "stop", "--", testService)
	g.mustRun("rm", "-f", "--", unitDirectory+"/"+testTimer, unitDirectory+"/"+testService)
	g.mustRun("systemctl", "daemon-reload")
}

func (g *testGuest) purgeTestPackage() {
	g.t.Helper()
	g.mustRun("apt-get", "purge", "-y", "--", testPackage)
}

// providerBlock returns the provider configuration for the test guest.
func (g *testGuest) providerBlock() string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    pveguest = {
      source = %q
    }
  }
}

provider "pveguest" {
  nodes = {
    %q = {
      host = %q
    }
  }
}

locals {
  node = %q
  vmid = %d
  kind = %q
}
`, providerSource, g.guest.Node, g.host, g.guest.Node, g.guest.VMID, string(g.guest.Kind))
}
