// Package dockersocket proxies Docker Engine API requests into a Proxmox guest.
package dockersocket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	socketFileMode    = 0o600
	shutdownTimeout   = 5 * time.Second
	readHeaderTimeout = 10 * time.Second
)

// Options selects the local and guest sockets, allowed methods, and request body limit.
type Options struct {
	SocketPath   string
	GuestSocket  string
	GetOnly      bool
	MaxBodyBytes int64
}

// Serve runs the proxy until ctx ends and removes its local socket before returning.
func Serve(ctx context.Context, client *transport.Client, guest transport.Guest, options Options) error {
	err := validateInputs(client, guest, options)
	if err != nil {
		slog.Error("docker socket configuration is invalid", "err", err)
		return err
	}
	proxy := newProxy(client, guest, options.GuestSocket, options.GetOnly, options.MaxBodyBytes)

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "unix", options.SocketPath)
	if err != nil {
		slog.Error("listen failed", "socket", options.SocketPath, "err", err)
		return fmt.Errorf("listen on %s: %w", options.SocketPath, err)
	}
	defer func() { _ = os.Remove(options.SocketPath) }()
	err = os.Chmod(options.SocketPath, socketFileMode)
	if err != nil {
		_ = listener.Close()
		slog.Error("restrict socket mode failed", "err", err)
		return fmt.Errorf("restrict socket mode: %w", err)
	}

	server := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	stop, cancelStop := context.WithCancel(ctx)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		defer func() {
			recovered := recover()
			if recovered != nil {
				slog.Error("shutdown panicked", "err", fmt.Errorf("%v", recovered))
			}
		}()
		shutdownOnStop(stop, server)
	}()

	slog.Info("listening", "socket", options.SocketPath, "guest", guest.String(), "get_only", options.GetOnly)
	err = server.Serve(listener)
	cancelStop()
	<-shutdownDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve failed", "err", err)
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func validateInputs(client *transport.Client, guest transport.Guest, options Options) error {
	if client == nil {
		return errors.New("transport client is required")
	}
	if guest.Node == "" || guest.VMID == 0 {
		return errors.New("guest node and VMID are required")
	}
	if guest.Kind != transport.KindLXC && guest.Kind != transport.KindQEMU {
		return errors.New("guest kind must be lxc or qemu")
	}
	if options.SocketPath == "" {
		return errors.New("local socket path is required")
	}
	if options.GuestSocket == "" {
		return errors.New("guest socket path is required")
	}
	if options.MaxBodyBytes <= 0 {
		return errors.New("max body bytes must be positive")
	}
	return nil
}

// shutdownOnStop waits for the stop context to end, then shuts down the server
// with shutdownTimeout.
func shutdownOnStop(stop context.Context, server *http.Server) {
	<-stop.Done()
	shutdownContext, shutdownCancel := context.WithTimeout(context.WithoutCancel(stop), shutdownTimeout)
	defer shutdownCancel()
	err := server.Shutdown(shutdownContext)
	if err != nil {
		slog.Error("shutdown failed", "err", err)
	}
}
