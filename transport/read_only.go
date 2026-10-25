package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const maximumReadCommandAttempts = 3

// closedConnectionError identifies a connection failure from the HTTP transport.
type closedConnectionError struct {
	cause error
}

func (failure *closedConnectionError) Error() string {
	return failure.cause.Error()
}

func (failure *closedConnectionError) Unwrap() error {
	return failure.cause
}

// RunReadOnly retries a read-only command after a closed API connection.
// The caller must guarantee that repeating the command cannot change the guest.
func (client *Client) RunReadOnly(ctx context.Context, guest Guest, command Command) (Result, error) {
	delay := retryInitialDelay
	for attempt := 1; ; attempt++ {
		result, err := client.run(ctx, guest, command)
		if err == nil {
			return result, nil
		}
		var resultRead *resultReadError
		if errors.As(err, &resultRead) {
			return Result{}, err
		}
		var closed *closedConnectionError
		if !errors.As(err, &closed) {
			return Result{}, err
		}
		if attempt >= maximumReadCommandAttempts {
			return Result{}, fmt.Errorf("read-only guest command failed after %d attempts: %w", attempt, err)
		}
		slog.WarnContext(ctx, "retry read-only guest command after closed API connection",
			"node", guest.Node, "vmid", guest.VMID, "kind", guest.Kind, "attempt", attempt)
		if err := waitReadCommandRetry(ctx, delay); err != nil {
			return Result{}, fmt.Errorf("%s: stop retrying read-only command: %w", guest, err)
		}
		delay *= retryDelayFactor
	}
}

func waitReadCommandRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		slog.WarnContext(ctx, "read-only command retry canceled", "err", ctx.Err())
		return fmt.Errorf("wait for read-only command retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
