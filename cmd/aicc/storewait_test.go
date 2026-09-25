// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeClock advances only when the retry loop sleeps, so a test covers a
// minute of waiting without spending one.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) bool {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return ctx.Err() == nil
}

var errRefused = errors.New("dial tcp 10.130.0.3:5432: connect: connection refused")

// A host reboot brings the server up before PostgreSQL: it waits, then runs.
func TestTheServerWaitsForADatabaseThatIsStillStarting(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Unix(0, 0)}
	attempts := 0
	got, err := retryOpen(context.Background(), storeWaitBudget, func(context.Context) (string, error) {
		attempts++
		if attempts <= 3 {
			return "", errRefused
		}
		return "store", nil
	}, clock.now, clock.sleep)
	if err != nil || got != "store" {
		t.Fatalf("got %q, %v; want the store", got, err)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	if len(clock.sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", clock.sleeps, want)
	}
	for i := range want {
		if clock.sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v", clock.sleeps, want)
		}
	}
}

// A database that never comes fails the way a single attempt does, with the
// last error, once the budget is spent and not a moment past it.
func TestTheServerGivesUpOnADatabaseThatNeverComes(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Unix(0, 0)}
	start := clock.t
	attempts := 0
	_, err := retryOpen(context.Background(), storeWaitBudget, func(context.Context) (string, error) {
		attempts++
		return "", errRefused
	}, clock.now, clock.sleep)
	if err != errRefused {
		t.Fatalf("err = %v, want the last attempt's error unchanged", err)
	}
	if waited := clock.t.Sub(start); waited != storeWaitBudget {
		t.Fatalf("waited %v, want exactly the budget %v", waited, storeWaitBudget)
	}
	for _, d := range clock.sleeps {
		if d > storeRetryMax {
			t.Fatalf("a sleep of %v exceeds the cap %v", d, storeRetryMax)
		}
	}
	if attempts != len(clock.sleeps)+1 {
		t.Fatalf("attempts = %d, sleeps = %d: the last attempt must follow the last sleep",
			attempts, len(clock.sleeps))
	}
}

// SIGTERM during the wait ends the wait.
func TestStoppingTheServerEndsTheWaitForTheDatabase(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := retryOpen(ctx, storeWaitBudget, func(context.Context) (string, error) {
		attempts++
		return "", errRefused
	}, time.Now, func(ctx context.Context, d time.Duration) bool {
		cancel()
		return sleepCtx(ctx, time.Hour)
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errRefused) {
		t.Fatalf("err = %v, want cancellation joined with the last error", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

// A malformed connection string will not fix itself in a minute.
func TestAMalformedDatabaseURLFailsAtOnce(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Unix(0, 0)}
	parseErr := &pgconn.ParseConfigError{}
	_, err := retryOpen(context.Background(), storeWaitBudget, func(context.Context) (string, error) {
		return "", parseErr
	}, clock.now, clock.sleep)
	if err != parseErr || len(clock.sleeps) != 0 {
		t.Fatalf("err = %v after %d sleeps, want the parse error at once", err, len(clock.sleeps))
	}
}
