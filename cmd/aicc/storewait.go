// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// How long the server waits for its database at startup. Docker restarts
// containers after a host reboot without honouring depends_on, so the server
// can come up before PostgreSQL accepts connections; it waits rather than
// relying on the restart policy. The subcommands do not wait: an operator at a
// terminal wants the error now.
const (
	storeWaitBudget = 60 * time.Second
	storeRetryMin   = 500 * time.Millisecond
	storeRetryMax   = 5 * time.Second
)

// retryOpen calls open until it succeeds, the budget is spent or ctx is
// cancelled. Between attempts it sleeps with capped exponential backoff and
// logs one warning per failed attempt. When the budget runs out it returns the
// last attempt's error unchanged, so the caller fails exactly as a single
// attempt would. A malformed connection string is not a database that is still
// starting, so it is returned at once.
//
// now and sleep are injected for tests; sleep reports false when ctx ended
// before d elapsed.
func retryOpen[T any](
	ctx context.Context,
	budget time.Duration,
	open func(context.Context) (T, error),
	now func() time.Time,
	sleep func(context.Context, time.Duration) bool,
) (T, error) {
	deadline := now().Add(budget)
	backoff := storeRetryMin
	for {
		v, err := open(ctx)
		if err == nil {
			return v, nil
		}
		var parseErr *pgconn.ParseConfigError
		if errors.As(err, &parseErr) || ctx.Err() != nil {
			return v, err
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return v, err
		}
		wait := min(backoff, remaining)
		slog.WarnContext(ctx, "database not ready", "error", err, "retryIn", wait.Round(time.Millisecond))
		if !sleep(ctx, wait) {
			return v, fmt.Errorf("%w while waiting for the database: %w", ctx.Err(), err)
		}
		backoff = min(backoff*2, storeRetryMax)
	}
}

// sleepCtx sleeps for d, returning false early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
