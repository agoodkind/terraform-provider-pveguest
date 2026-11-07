// Command pveguest-docker-socket serves the Docker Engine API on a local Unix
// socket by running curl in a Proxmox container or VM.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/agoodkind/terraform-provider-pveguest/dockersocket"
	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	exitFailure = 1

	defaultKind        = string(transport.KindLXC)
	defaultGuestSocket = "/var/run/docker.sock"

	defaultMaxBodyBytes = 256 << 20
)

type config struct {
	node         string
	endpoint     string
	nodeName     string
	tokenFile    string
	vmid         int64
	kind         string
	insecure     bool
	socketPath   string
	getOnly      bool
	maxBodyBytes int64
}

func main() {
	err := run(os.Args[1:])
	if err != nil {
		slog.Error("pveguest-docker-socket exited", "err", err)
		os.Exit(exitFailure)
	}
}

func parseConfig(arguments []string) (config, error) {
	var settings config
	flags := flag.NewFlagSet("pveguest-docker-socket", flag.ContinueOnError)
	flags.StringVar(&settings.node, "node", "", "provider node key of the guest")
	flags.StringVar(&settings.endpoint, "endpoint", "", "Proxmox API base URL")
	flags.StringVar(&settings.nodeName, "node-name", "", "Proxmox node name in the API path")
	flags.StringVar(&settings.tokenFile, "token-file", "", "file with the token user@realm!tokenid=secret")
	flags.Int64Var(&settings.vmid, "vmid", 0, "guest ID")
	flags.StringVar(&settings.kind, "kind", defaultKind, "guest kind: lxc or qemu")
	flags.BoolVar(&settings.insecure, "insecure", false, "skip TLS certificate verification")
	flags.StringVar(&settings.socketPath, "socket", "", "local Unix socket path")
	flags.BoolVar(&settings.getOnly, "get-only", false, "allow only GET and HEAD requests")
	flags.Int64Var(&settings.maxBodyBytes, "max-body-bytes", defaultMaxBodyBytes, "largest request body in bytes")
	err := flags.Parse(arguments)
	if err != nil {
		slog.Error("parse flags failed", "err", err)
		return config{}, fmt.Errorf("parse flags: %w", err)
	}

	missing := missingFlags(settings)
	if len(missing) > 0 {
		failure := fmt.Errorf("missing required flags: %s", strings.Join(missing, ", "))
		slog.Error("configuration is incomplete", "err", failure)
		return config{}, failure
	}
	return settings, nil
}

func missingFlags(settings config) []string {
	var missing []string
	if settings.node == "" {
		missing = append(missing, "-node")
	}
	if settings.endpoint == "" {
		missing = append(missing, "-endpoint")
	}
	if settings.tokenFile == "" {
		missing = append(missing, "-token-file")
	}
	if settings.vmid == 0 {
		missing = append(missing, "-vmid")
	}
	if settings.socketPath == "" {
		missing = append(missing, "-socket")
	}
	return missing
}

func run(arguments []string) error {
	settings, err := parseConfig(arguments)
	if err != nil {
		return err
	}

	tokenBytes, err := os.ReadFile(settings.tokenFile)
	if err != nil {
		slog.Error("read token file failed", "err", err)
		return fmt.Errorf("read token file: %w", err)
	}
	client, err := transport.NewClient(map[string]transport.NodeConfig{
		settings.node: {
			Endpoint: settings.endpoint,
			APIToken: strings.TrimSpace(string(tokenBytes)),
			Insecure: settings.insecure,
			NodeName: settings.nodeName,
		},
	}, transport.DefaultMaxRequests)
	if err != nil {
		slog.Error("create transport client failed", "err", err)
		return fmt.Errorf("create transport client: %w", err)
	}

	guest := transport.Guest{Node: settings.node, VMID: settings.vmid, Kind: transport.Kind(settings.kind)}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err = dockersocket.Serve(stop, client, guest, dockersocket.Options{
		SocketPath:   settings.socketPath,
		GuestSocket:  defaultGuestSocket,
		GetOnly:      settings.getOnly,
		MaxBodyBytes: settings.maxBodyBytes,
	})
	if err != nil {
		return fmt.Errorf("run Docker socket proxy: %w", err)
	}
	return nil
}
