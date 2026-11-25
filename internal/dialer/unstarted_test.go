// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// termRig is the dialer on the simulation's in-memory store, no database, with
// the bot-slot counter and the provider meter's reading set by the test: the
// unstarted term (#640) one message at a time. Every ringing leg is planned
// as an answer (noOverdial), so the expected-answer sum is the legs ringing.
type termRig struct {
	t      *testing.T
	clock  *fakeClock
	st     *simStore
	placer *fakePlacer
	slots  *fakeSlots
	rate   *fakeRate
	d      *Dialer
}

// fakeRate is the provider meter as the dialer reads it.
type fakeRate struct {
	mu sync.Mutex
	r  ratemeter.Reading
}

func (f *fakeRate) Read() ratemeter.Reading {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.r
}

func (f *fakeRate) set(r ratemeter.Reading) {
	f.mu.Lock()
	f.r = r
	f.mu.Unlock()
}

// newTermRig runs a dialer with one campaign placing at most maxConcurrent
// calls, and the provider rule at sessionLimit starts a minute (0: none).
func newTermRig(t *testing.T, sessionLimit, maxConcurrent int, u voice.Usage) *termRig {
	t.Helper()
	clock := &fakeClock{now: ny(t, "2026-06-01 09:30")}
	r := &termRig{t: t, clock: clock, st: newSimStore(), placer: newPlacer(clock),
		slots: &fakeSlots{u: u}, rate: &fakeRate{r: ratemeter.Reading{At: clock.Now()}}}
	r.st.camps = []campaign.Campaign{{
		ID: uuid.Must(uuid.NewV7()), Name: "c0", DIDNumber: "95099", Status: campaign.StatusRunning,
		TimeZone: "America/New_York", MaxConcurrent: maxConcurrent, RingTimeoutSec: 30,
		CallingWindow: campaign.CallingWindow{Days: everyDay, Ranges: []campaign.TimeRange{{Start: "09:00", End: "18:00"}}},
		RetryPolicy:   campaign.DefaultRetryPolicy(),
	}}
	outer, err := dayrange.Parse("08:00-21:00")
	if err != nil {
		t.Fatal(err)
	}
	r.d = New(Config{
		MaxRinging: 1000, OutboundHours: outer, Frequency: 24 * time.Hour,
		TrunkBreakerCount: 1 << 30, ProviderBreakerCount: 1 << 30, noOverdial: true,
		ProviderSessionLimit: sessionLimit, ProviderSessionPeriod: time.Minute,
	}, Deps{
		Store: r.st, Placer: r.placer, Slots: r.slots, ProviderRate: r.rate, Clock: clock,
		IsLive: r.placer.isLive, Log: slog.New(slog.DiscardHandler),
	})
	r.st.onFinish = r.d.AttemptFinished
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.d.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	r.settle()
	return r
}

func (r *termRig) settle() {
	r.t.Helper()
	for i := 0; i < 4; i++ {
		if err := r.d.Sync(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
}

// setMax changes the campaign's max_concurrent and lets the dialer look again.
func (r *termRig) setMax(n int) {
	r.t.Helper()
	r.st.mu.Lock()
	r.st.camps[0].MaxConcurrent = n
	r.st.mu.Unlock()
	r.d.Wake()
	r.settle()
}

// total is how many calls the dialer has placed so far.
func (r *termRig) total() int { return r.placer.total() }

// answer tells the dialer the callees of calls picked up.
func (r *termRig) answer(calls ...placed) {
	r.t.Helper()
	for _, c := range calls {
		r.d.Answered(c.Req.CallID)
	}
	r.settle()
}

// term is the unstarted term and, read with it on the actor, the answered
// flights whose start is not counted: what the term must equal.
func (r *termRig) term() (n, want int) {
	r.t.Helper()
	if err := r.d.run(context.Background(), func(context.Context) {
		n = r.d.unstarted
		for _, f := range r.d.flights {
			if f.isAnswered && !f.sessionCounted {
				want++
			}
		}
	}); err != nil {
		r.t.Fatal(err)
	}
	return n, want
}

// wantTerm fails unless the term is n and equals the flights it stands for.
func (r *termRig) wantTerm(n int, what string) {
	r.t.Helper()
	got, flights := r.term()
	if got != n || flights != n {
		r.t.Errorf("%s: the unstarted term is %d over %d answered, uncounted flights, want %d", what, got, flights, n)
	}
}

// A campaign leg whose session start waits in the meter's start gate (#644) is
// an answered flight whose start is not counted yet, and a start waiting in the
// gate: both terms the provider rule reads hold it. It is one start to come,
// not two (#640). At 10 starts a minute with 2 made and 4 legs in the gate,
// four more ringing legs fit: 2 + 4 + 4 = 10.
func TestALegWaitingInTheStartGateCountsOnceInTheProviderRule(t *testing.T) {
	r := newTermRig(t, 10, 4, voice.Usage{Cap: 100})
	calls := r.placer.placedCalls()
	if len(calls) != 4 {
		t.Fatalf("%d calls placed, want the campaign's 4", len(calls))
	}
	// Each callee answers, the leg takes its bot slot, and its session start
	// waits in the gate for room in the provider's window.
	r.slots.set(voice.Usage{InUse: 4, Cap: 100})
	r.rate.set(ratemeter.Reading{At: r.clock.Now(), Starts: 2, StartsWaiting: 4})
	r.answer(calls...)
	r.wantTerm(4, "four legs in the gate")

	r.setMax(100)
	if got := r.total() - 4; got != 4 {
		t.Errorf("%d more calls placed with 2 starts made and 4 waiting in the gate of 10 a minute, want 4", got)
	}
	r.noViolations()
}

func (r *termRig) noViolations() {
	r.t.Helper()
	r.placer.mu.Lock()
	defer r.placer.mu.Unlock()
	if len(r.placer.violations) > 0 {
		r.t.Fatalf("placer violations: %v", r.placer.violations)
	}
}

// The term of #640: taken as the dialer hears of the answer, released when the
// leg's provider session start is counted (SessionCounted) or when the leg
// ends — and every ending is a drop, so nothing else can leak it.
func TestTheUnstartedTermIsTakenAtTheAnswerAndReleasedOnEveryEnd(t *testing.T) {
	r := newRig(t, opts{})
	c := r.newCampaign("Win", func(s *camSpec) { s.max = 50 })
	r.importContacts(c, numbers("9", 3))
	r.start(c)
	live := r.placer.liveCalls()
	if len(live) < 3 {
		t.Fatalf("%d calls live, want the 3 placed", len(live))
	}

	read := func() int {
		t.Helper()
		var n int
		if err := r.dialer().run(context.Background(), func(context.Context) { n = r.dialer().unstarted }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := read(); n != 0 {
		t.Fatalf("unstarted = %d, want 0: ringing legs are not counted", n)
	}

	// Each answer holds its own count from the message that reports it.
	r.dialer().Answered(live[0].Req.CallID)
	r.settle()
	if n := read(); n != 1 {
		t.Fatalf("after the first answer unstarted = %d, want 1", n)
	}
	r.dialer().Answered(live[1].Req.CallID)
	r.settle()
	if n := read(); n != 2 {
		t.Fatalf("after the second answer unstarted = %d, want 2", n)
	}

	// The counted start releases it.
	r.dialer().SessionCounted(live[0].Req.CallID)
	r.settle()
	if n := read(); n != 1 {
		t.Fatalf("after the counted start unstarted = %d, want 1", n)
	}
	// ...and ending a counted leg takes nothing a second time.
	r.end(live[0], endAnswered)
	r.settle()
	if n := read(); n != 1 {
		t.Fatalf("after the counted leg ended unstarted = %d, want 1", n)
	}

	// A callee hanging up inside the window releases it by ending; an
	// answered leg never in the term (the one still ringing) is untouched.
	r.end(live[1], endHangup1s)
	r.settle()
	if n := read(); n != 0 {
		t.Fatalf("after the window hangup unstarted = %d, want 0", n)
	}
	// A count that arrives for a call that was never answered is ignored.
	r.dialer().SessionCounted(live[2].Req.CallID)
	r.settle()
	if n := read(); n != 0 {
		t.Fatalf("after a stranger's count unstarted = %d, want 0", n)
	}
	r.noViolations()
}
