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
	"sync"
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

// Serve runs the proxy until ctx ends and closes its server before returning.
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
	err = os.Chmod(options.SocketPath, socketFileMode)
	if err != nil {
		_ = listener.Close()
		slog.Error("restrict socket mode failed", "err", err)
		return fmt.Errorf("restrict socket mode: %w", err)
	}

	requestContext, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	var connections sync.WaitGroup
	server := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: readHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return requestContext },
		ConnState: func(_ net.Conn, state http.ConnState) {
			trackConnection(&connections, state)
		},
	}
	stop, cancelStop := context.WithCancel(ctx)
	shutdownResult := make(chan error, 1)
	go func() {
		defer func() {
			recovered := recover()
			if recovered != nil {
				failure := fmt.Errorf("%v", recovered)
				slog.Error("shutdown panicked", "err", failure)
				shutdownResult <- failure
			}
		}()
		shutdownResult <- shutdownOnStop(stop, server, cancelRequests)
	}()

	slog.Info("listening", "socket", options.SocketPath, "guest", guest.String(), "get_only", options.GetOnly)
	serveErr := server.Serve(listener)
	cancelStop()
	shutdownErr := <-shutdownResult
	connections.Wait()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		slog.Error("serve failed", "err", serveErr)
		return errors.Join(fmt.Errorf("serve: %w", serveErr), shutdownErr)
	}
	return shutdownErr
}

func trackConnection(connections *sync.WaitGroup, state http.ConnState) {
	switch state {
	case http.StateNew:
		connections.Add(1)
	case http.StateHijacked, http.StateClosed:
		connections.Done()
	case http.StateActive, http.StateIdle:
	}
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
func shutdownOnStop(stop context.Context, server *http.Server, cancelRequests context.CancelFunc) error {
	<-stop.Done()
	shutdownContext, shutdownCancel := context.WithTimeout(context.WithoutCancel(stop), shutdownTimeout)
	defer shutdownCancel()
	err := server.Shutdown(shutdownContext)
	if err == nil {
		return nil
	}
	slog.Error("shutdown failed", "err", err)
	shutdownErr := fmt.Errorf("shut down Docker socket proxy: %w", err)
	if !errors.Is(err, context.DeadlineExceeded) {
		return shutdownErr
	}

	// Close active connections when graceful shutdown exceeds its deadline.
	cancelRequests()
	err = server.Close()
	if err != nil {
		return errors.Join(shutdownErr, fmt.Errorf("close Docker socket proxy connections: %w", err))
	}
	return shutdownErr
}
