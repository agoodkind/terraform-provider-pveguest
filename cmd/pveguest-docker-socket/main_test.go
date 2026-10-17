package main_test

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"testing"
)

const interruptExitCode = 130

type commandRegistry struct {
	mutex    sync.Mutex
	commands map[*exec.Cmd]struct{}
}

var startedCommands = &commandRegistry{commands: map[*exec.Cmd]struct{}{}}

func (registry *commandRegistry) add(command *exec.Cmd) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	registry.commands[command] = struct{}{}
}

func (registry *commandRegistry) remove(command *exec.Cmd) {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	delete(registry.commands, command)
}

func (registry *commandRegistry) killAll() {
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	for command := range registry.commands {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}
}

func TestMain(m *testing.M) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		startedCommands.killAll()
		os.Exit(interruptExitCode)
	}()
	code := m.Run()
	startedCommands.killAll()
	os.Exit(code)
}
