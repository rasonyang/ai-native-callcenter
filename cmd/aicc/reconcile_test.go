// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

type fakeAdopter struct {
	calls int
	// failures is how many calls fail before one succeeds; -1 never succeeds.
	failures int
	live     []telephony.LiveChannel
}

func (f *fakeAdopter) AdoptLiveChannels(context.Context) ([]telephony.LiveChannel, error) {
	f.calls++
	if f.failures < 0 || f.calls <= f.failures {
		return nil, errors.New("switch said no")
	}
	return f.live, nil
}

type fakeRecovery struct {
	recovered  int
	marked     int
	sawBotFunc bool
	order      []string
}

func (f *fakeRecovery) Recover(_ context.Context, _ []telephony.LiveChannel,
	hasBot func(uuid.UUID) bool) outbound.RecoveryReport {
	f.recovered++
	f.sawBotFunc = hasBot != nil
	f.order = append(f.order, "recover")
	return outbound.RecoveryReport{}
}

func (f *fakeRecovery) MarkReconciled() {
	f.marked++
	f.order = append(f.order, "open")
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Recovery settles what was adopted, and only then is outbound opened.
func TestOutboundOpensOnlyAfterTheChannelsAreSettled(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{}, &fakeRecovery{}
	reconcileLive(adopter, rec, func(uuid.UUID) bool { return false }, quiet(), time.Millisecond)(t.Context())

	if adopter.calls != 1 || rec.recovered != 1 || rec.marked != 1 {
		t.Fatalf("adopt=%d recover=%d open=%d", adopter.calls, rec.recovered, rec.marked)
	}
	if rec.order[0] != "recover" || rec.order[1] != "open" {
		t.Errorf("order = %v, want recover then open", rec.order)
	}
	if !rec.sawBotFunc {
		t.Error("recovery was not told how to ask whether a bot session exists")
	}
}

// A listing that cannot be had is retried; when it comes, the gate opens.
func TestAFailedListingIsRetriedBeforeOutboundOpens(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{failures: 2}, &fakeRecovery{}
	reconcileLive(adopter, rec, nil, quiet(), time.Millisecond)(t.Context())

	if adopter.calls != 3 || rec.marked != 1 {
		t.Errorf("adopt=%d open=%d, want 3 attempts and one opening", adopter.calls, rec.marked)
	}
}

// Admitting a call while ignorant of those in flight is the failure the gate
// exists to prevent, so a switch that never answers leaves it shut.
func TestOutboundStaysClosedWhenTheSwitchCannotBeRead(t *testing.T) {
	t.Parallel()
	adopter, rec := &fakeAdopter{failures: -1}, &fakeRecovery{}
	reconcileLive(adopter, rec, nil, quiet(), time.Millisecond)(t.Context())

	if adopter.calls != reconcileAttempts {
		t.Errorf("attempts = %d, want %d", adopter.calls, reconcileAttempts)
	}
	if rec.recovered != 0 || rec.marked != 0 {
		t.Errorf("recover=%d open=%d after a switch that never answered", rec.recovered, rec.marked)
	}
}

func TestAReconcileInterruptedByShutdownStopsRetrying(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	adopter, rec := &fakeAdopter{failures: -1}, &fakeRecovery{}
	done := make(chan struct{})
	go func() {
		reconcileLive(adopter, rec, nil, quiet(), time.Hour)(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reconcile kept waiting after the context ended")
	}
}

// The reconcile runs on every connect, not only the first, and last.
func TestTheReconcileRunsOnEveryConnect(t *testing.T) {
	t.Parallel()
	f := newWiringFixture(t)
	adopter, rec := &fakeAdopter{}, &fakeRecovery{}
	f.comp.Reconcile = reconcileLive(adopter, rec, nil, quiet(), time.Millisecond)
	f.comp.connect()

	f.link.connected(t.Context())
	f.link.lost()
	f.link.connected(t.Context())
	f.link.lost()
	f.link.connected(t.Context())

	if adopter.calls != 3 || rec.recovered != 3 {
		t.Errorf("three connects ran adopt=%d recover=%d, want 3 each", adopter.calls, rec.recovered)
	}
	if rec.marked != 3 {
		// Opening is idempotent in the service; the hook may say it each time.
		t.Errorf("open=%d", rec.marked)
	}
	if last := f.link.onConnect[len(f.link.onConnect)-1]; last == nil {
		t.Error("no hook registered")
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
