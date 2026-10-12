// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

type fakeAdopter struct {
	mu    sync.Mutex
	calls int
	// failures is how many calls fail before one succeeds; -1 never succeeds.
	failures int
	live     []telephony.LiveChannel
}

func (f *fakeAdopter) AdoptLiveChannels(context.Context) ([]telephony.LiveChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures < 0 || f.calls <= f.failures {
		return nil, errors.New("switch said no")
	}
	return f.live, nil
}

func (f *fakeAdopter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeRecovery struct {
	mu         sync.Mutex
	recovered  int
	marked     int
	sawBotFunc bool
	cold       []bool
	order      []string
}

func (f *fakeRecovery) Recover(_ context.Context, _ []telephony.LiveChannel,
	hasBot func(uuid.UUID) bool, isColdStart bool) outbound.RecoveryReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovered++
	f.sawBotFunc = hasBot != nil
	f.cold = append(f.cold, isColdStart)
	f.order = append(f.order, "recover")
	return outbound.RecoveryReport{}
}

func (f *fakeRecovery) MarkReconciled() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked++
	f.order = append(f.order, "open")
}

func (f *fakeRecovery) markCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marked
}

// errorCounter counts ERROR records.
type errorCounter struct{ n atomic.Int32 }

func (e *errorCounter) Enabled(context.Context, slog.Level) bool { return true }
func (e *errorCounter) WithAttrs([]slog.Attr) slog.Handler       { return e }
func (e *errorCounter) WithGroup(string) slog.Handler            { return e }
func (e *errorCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelError {
		e.n.Add(1)
	}
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func linkUp() bool { return true }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Recovery settles what was adopted, and only then is outbound opened.
func TestOutboundOpensOnlyAfterTheChannelsAreSettled(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{}, &fakeRecovery{}
	reconcileLive(adopter, rec, func(uuid.UUID) bool { return false }, linkUp, quiet(), time.Millisecond)(t.Context())

	if adopter.callCount() != 1 || rec.recovered != 1 || rec.marked != 1 {
		t.Fatalf("adopt=%d recover=%d open=%d", adopter.callCount(), rec.recovered, rec.marked)
	}
	if rec.order[0] != "recover" || rec.order[1] != "open" {
		t.Errorf("order = %v, want recover then open", rec.order)
	}
	if !rec.sawBotFunc {
		t.Error("recovery was not told how to ask whether a bot session exists")
	}
}

// A listing that cannot be had does not hold the link's hooks: the hook returns
// and a retry runs with backoff. The gate opens on the first success, and not
// before; every failure is an ERROR.
func TestAFailedListingIsRetriedUntilItWorksAndOutboundOpensOnlyThen(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{failures: 3}, &fakeRecovery{}
	errs := &errorCounter{}
	hook := reconcileLive(adopter, rec, nil, linkUp, slog.New(errs), time.Millisecond)

	start := time.Now()
	hook(t.Context())
	if time.Since(start) > time.Second {
		t.Fatal("the hook blocked on retries; the link would never start delivering events")
	}
	if rec.markCount() != 0 && adopter.callCount() <= 3 {
		t.Fatal("outbound opened before the listing could be read")
	}
	eventually(t, "the gate to open", func() bool { return rec.markCount() == 1 })
	if adopter.callCount() != 4 {
		t.Errorf("attempts = %d, want 3 failures then a success", adopter.callCount())
	}
	if got := errs.n.Load(); got != 3 {
		t.Errorf("%d ERROR records, want one per failure (3)", got)
	}
}

// No fail-open: for as long as the switch cannot be read, nothing is admitted.
func TestOutboundStaysClosedForAsLongAsTheSwitchCannotBeRead(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{failures: -1}, &fakeRecovery{}
	hook := reconcileLive(adopter, rec, nil, linkUp, quiet(), time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	hook(ctx)

	eventually(t, "many retries", func() bool { return adopter.callCount() >= 6 })
	cancel()
	if rec.markCount() != 0 {
		t.Errorf("outbound opened (%d) while the switch could not be read", rec.markCount())
	}
}

// The retry stops when the link does: a new connect starts its own.
func TestTheRetryStopsWhenTheLinkGoesDown(t *testing.T) {
	t.Parallel()
	var up atomic.Bool
	up.Store(true)
	adopter, rec := &fakeAdopter{failures: -1}, &fakeRecovery{}
	reconcileLive(adopter, rec, nil, up.Load, quiet(), time.Millisecond)(t.Context())
	eventually(t, "a retry", func() bool { return adopter.callCount() >= 3 })

	up.Store(false)
	time.Sleep(30 * time.Millisecond)
	settled := adopter.callCount()
	time.Sleep(50 * time.Millisecond)
	if got := adopter.callCount(); got != settled {
		t.Errorf("retries continued with the link down: %d -> %d", settled, got)
	}
}

// A newer connect supersedes an older connect's retries.
func TestANewerConnectSupersedesOlderRetries(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{failures: 2}, &fakeRecovery{}
	hook := reconcileLive(adopter, rec, nil, linkUp, quiet(), 50*time.Millisecond)
	hook(t.Context()) // fails, schedules a retry
	hook(t.Context()) // fails again, supersedes it
	eventually(t, "the gate", func() bool { return rec.markCount() >= 1 })
	time.Sleep(150 * time.Millisecond)
	if rec.markCount() != 1 {
		t.Errorf("opened %d times, want once: the older retry should have given way", rec.markCount())
	}
}

// Only the first success after the process started may end a call for want of
// a bot session, even when that success came after failures.
func TestOnlyTheFirstSuccessIsAColdStart(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{failures: 1}, &fakeRecovery{}
	hook := reconcileLive(adopter, rec, nil, linkUp, quiet(), time.Millisecond)
	hook(t.Context())
	eventually(t, "the first success", func() bool { return rec.markCount() == 1 })
	hook(t.Context())
	hook(t.Context())

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.cold) != 3 || !rec.cold[0] || rec.cold[1] || rec.cold[2] {
		t.Errorf("cold flags = %v, want [true false false]", rec.cold)
	}
}

// The reconcile runs on every connect, not only the first, and last.
func TestTheReconcileRunsOnEveryConnect(t *testing.T) {
	t.Parallel()
	f := newWiringFixture(t)
	adopter, rec := &fakeAdopter{}, &fakeRecovery{}
	f.comp.Reconcile = reconcileLive(adopter, rec, nil, linkUp, quiet(), time.Millisecond)
	f.comp.connect()

	f.link.connected(t.Context())
	f.link.lost()
	f.link.connected(t.Context())
	f.link.lost()
	f.link.connected(t.Context())

	if adopter.callCount() != 3 || rec.recovered != 3 {
		t.Errorf("three connects ran adopt=%d recover=%d, want 3 each", adopter.callCount(), rec.recovered)
	}
}

// A composition without one connects as before.
func TestNoReconcileHookMeansNoExtraConnectHook(t *testing.T) {
	t.Parallel()
	with, without := newWiringFixture(t), newWiringFixture(t)
	with.comp.Reconcile = func(context.Context) {}
	with.comp.connect()
	without.comp.connect()
	if len(with.link.onConnect) != len(without.link.onConnect)+1 {
		t.Errorf("hooks with=%d without=%d", len(with.link.onConnect), len(without.link.onConnect))
	}
}
