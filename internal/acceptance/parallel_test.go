package acceptance

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agoodkind/terraform-provider-pveguest/internal/transport"
)

const (
	// A comma-separated list of running containers on the acceptance node.
	// The test runs only `cat /etc/hostname` in them.
	parallelVMIDsVariable = "PVEGUEST_ACC_PARALLEL_VMIDS"
	parallelRounds        = 20
)

// TestAccParallelGuestCommands runs one read-only command in several
// containers of one hypervisor at the same time. `pct exec` exited with
// status 129 and empty stderr under that load in an Ansible run on
// 2026-10-04.
func TestAccParallelGuestCommands(t *testing.T) {
	handle := newTestGuest(t)
	listed := os.Getenv(parallelVMIDsVariable)
	if listed == "" {
		t.Skipf("%s is not set", parallelVMIDsVariable)
	}
	var guests []transport.Guest
	for _, field := range strings.Split(listed, ",") {
		vmid, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			t.Fatalf("%s: %v", parallelVMIDsVariable, err)
		}
		guests = append(guests, transport.Guest{Node: handle.guest.Node, VMID: vmid, Kind: transport.KindLXC})
	}

	nodes := map[string]transport.NodeConfig{
		handle.guest.Node: {Host: handle.host, Port: transport.DefaultPort, User: transport.DefaultUser},
	}
	pool, err := transport.NewPool(nodes, len(guests))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	var mutex sync.Mutex
	failures := map[string]int{}
	for round := 0; round < parallelRounds; round++ {
		var group sync.WaitGroup
		for _, guest := range guests {
			group.Add(1)
			go func(guest transport.Guest) {
				defer group.Done()
				command := transport.Command{Argv: []string{"cat", "/etc/hostname"}, TimeoutSeconds: 60}
				result, runError := pool.Run(context.Background(), guest, command)
				outcome := ""
				if runError != nil {
					outcome = "error: " + runError.Error()
				} else if result.ExitCode != 0 {
					outcome = "exit " + strconv.Itoa(result.ExitCode) + " stderr=" + strconv.Quote(string(result.Stderr))
				} else if strings.TrimSpace(string(result.Stdout)) == "" {
					outcome = "exit 0 with empty stdout"
				}
				if outcome != "" {
					mutex.Lock()
					failures[outcome]++
					mutex.Unlock()
				}
			}(guest)
		}
		group.Wait()
	}

	total := parallelRounds * len(guests)
	for outcome, count := range failures {
		t.Errorf("%d of %d commands: %s", count, total, outcome)
	}
}
