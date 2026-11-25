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
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
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
// edit, when given, changes the dialer's collaborators before it is built.
func newTermRig(t *testing.T, sessionLimit, maxConcurrent int, u voice.Usage, edit ...func(*termRig, *Deps)) *termRig {
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
	deps := Deps{
		Store: r.st, Placer: r.placer, Slots: r.slots, ProviderRate: r.rate, Clock: clock,
		IsLive: r.placer.isLive, Log: slog.New(slog.DiscardHandler),
	}
	for _, e := range edit {
		e(r, &deps)
	}
	r.d = New(Config{
		MaxRinging: 1000, OutboundHours: outer, Frequency: 24 * time.Hour,
		TrunkBreakerCount: 1 << 30, ProviderBreakerCount: 1 << 30, noOverdial: true,
		ProviderSessionLimit: sessionLimit, ProviderSessionPeriod: time.Minute,
	}, deps)
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

// stopAdmitting takes the campaign out of the running ones, so that no call is
// placed when a leg ends and the counts are the legs already in flight.
func (r *termRig) stopAdmitting() {
	r.st.mu.Lock()
	r.st.camps = nil
	r.st.mu.Unlock()
}

// total is how many calls the dialer has placed so far.
func (r *termRig) total() int { return r.placer.total() }

// answer tells the dialer the callees of calls picked up.
func (r *termRig) answer(ids ...uuid.UUID) {
	r.t.Helper()
	for _, id := range ids {
		r.d.Answered(id)
	}
	r.settle()
}

// callIDs is the call ids of calls, in their order.
func callIDs(calls []placed) []uuid.UUID {
	ids := make([]uuid.UUID, len(calls))
	for i, c := range calls {
		ids[i] = c.Req.CallID
	}
	return ids
}

// counted tells the dialer the session starts of calls were counted, as the
// orchestrator does once the meter took each.
func (r *termRig) counted(ids ...uuid.UUID) {
	r.t.Helper()
	for _, id := range ids {
		r.d.SessionCounted(id)
	}
	r.settle()
}

// end is a call's attempt finishing with outcome, as its CDR would finish it.
func (r *termRig) end(id uuid.UUID, outcome campaign.Outcome) {
	r.t.Helper()
	r.placer.drop(id)
	if _, err := r.st.Finish(context.Background(), id, campaign.Ending{Outcome: outcome, EndedAt: r.clock.Now()}); err != nil {
		r.t.Fatal(err)
	}
	r.settle()
}

// legs is the dialer's ringing and answered legs in flight, read on the actor.
func (r *termRig) legs() (ringing, talking, flights int) {
	r.t.Helper()
	if err := r.d.run(context.Background(), func(context.Context) {
		ringing, talking, flights = r.d.ringing, r.d.talking, len(r.d.flights)
	}); err != nil {
		r.t.Fatal(err)
	}
	return ringing, talking, flights
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
	r.answer(callIDs(calls)...)
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

// adoptStore is the in-memory store with attempts left DIALING by a process
// that died: what a restarted dialer reconciles (design 10 section 8.2).
type adoptStore struct {
	*simStore
	dialing []campaign.DialingAttempt
}

func (s *adoptStore) Dialing(context.Context) ([]campaign.DialingAttempt, error) {
	return s.dialing, nil
}

// A restart with legs in the window (design 10 section 8.2). The new dialer
// adopts every live attempt as ringing, so none is in the term then. The
// outbound service re-arms the bridge to the bot of a leg that still rings,
// and that leg's answer, reported to the new dialer, takes the term like any
// other; its counted start or its end releases it. An answer that arrives
// before the attempts are adopted finds no flight: that leg stays a ringing
// one until it ends, and a counted start for it is ignored. Nothing leaks
// either way, the ringing and talking counts included.
func TestALegAdoptedAfterARestartTakesTheTermOnlyWhenItsAnswerReachesTheDialer(t *testing.T) {
	gate := make(chan struct{})
	var stillRinging, hangsUp, answeredEarly uuid.UUID
	r := newTermRig(t, 0, 3, voice.Usage{Cap: 100}, func(r *termRig, d *Deps) {
		cam := r.st.camps[0]
		var dialing []campaign.DialingAttempt
		for _, id := range []*uuid.UUID{&stillRinging, &hangsUp, &answeredEarly} {
			a := campaign.Attempt{
				ID: uuid.Must(uuid.NewV7()), CampaignID: cam.ID, ContactID: uuid.Must(uuid.NewV7()),
				CallID: uuid.Must(uuid.NewV7()), LegID: uuid.Must(uuid.NewV7()),
				State: campaign.AttemptDialing, StartedAt: r.clock.Now().Add(-10 * time.Second),
			}
			*id = a.CallID
			r.st.attempts[a.CallID] = a
			r.placer.live[a.CallID] = placed{Req: outboundReqOf(a), At: a.StartedAt}
			dialing = append(dialing, campaign.DialingAttempt{Attempt: a, RingTimeoutSec: cam.RingTimeoutSec})
		}
		d.Store = &adoptStore{simStore: r.st, dialing: dialing}
		d.Reconciled = gate
	})

	// The switch reports one answer while the new process is still reading
	// it, before the dialer has adopted anything.
	r.answer(answeredEarly)
	close(gate)
	r.settle()
	r.wantTerm(0, "adopted as ringing")
	if ringing, talking, flights := r.legs(); ringing != 3 || talking != 0 || flights != 3 {
		t.Fatalf("after adoption %d ringing, %d talking, %d flights, want 3, 0, 3", ringing, talking, flights)
	}
	if got := r.total(); got != 0 {
		t.Fatalf("%d calls placed while the adopted legs fill the campaign", got)
	}
	// No new calls from here on: the counts below are the adopted legs' alone.
	r.stopAdmitting()

	r.answer(stillRinging, hangsUp)
	r.wantTerm(2, "two re-armed legs answered after the restart")
	r.counted(stillRinging, answeredEarly)
	r.wantTerm(1, "one counted start, and one for a leg the dialer saw ring")
	r.end(hangsUp, campaign.OutcomeAnswered)
	r.wantTerm(0, "the callee hung up inside the window")

	r.end(stillRinging, campaign.OutcomeAnswered)
	r.end(answeredEarly, campaign.OutcomeAnswered)
	r.wantTerm(0, "every adopted leg ended")
	if ringing, talking, flights := r.legs(); ringing != 0 || talking != 0 || flights != 0 {
		t.Errorf("after every leg ended %d ringing, %d talking, %d flights, want none", ringing, talking, flights)
	}
}

// outboundReqOf is the placement of an attempt as the fake switch keeps it.
func outboundReqOf(a campaign.Attempt) outbound.AIDialRequest {
	return outbound.AIDialRequest{CallID: a.CallID, LegID: a.LegID, To: "+86139" + a.CallID.String()[:8],
		CampaignID: a.CampaignID, AttemptID: a.ID}
}

// The bot rule with the term (#640), on 5 slots with nothing kept for inbound
// and every ringing leg planned as an answer. An answered leg holds its place
// from the answer, before its slot is reserved (the reservation waits for the
// originate's +OK, design 10 section 6.4); reserved, it counts once, not as a
// reservation and an unstarted leg; with its slot taken and its start not yet
// counted it counts twice, the safe direction; counted, once again. A
// reservation that is no flight's, an AI call placed through the API, still
// counts.
func TestTheBotRuleHoldsAPlaceForAnAnsweredLegFromItsAnswer(t *testing.T) {
	r := newTermRig(t, 0, 100, voice.Usage{Cap: 5})
	calls := r.placer.placedCalls()
	if len(calls) != 5 {
		t.Fatalf("%d calls placed on 5 slots, want 5", len(calls))
	}
	ids := callIDs(calls)
	wantTotal := func(n int, what string) {
		t.Helper()
		if got := r.total(); got != n {
			t.Errorf("%s: %d calls placed, want %d", what, got, n)
		}
	}
	look := func(u voice.Usage) {
		r.slots.set(u)
		r.d.Wake()
		r.settle()
	}

	r.answer(ids[:3]...)
	wantTotal(5, "3 answered legs not yet reserved and 2 ringing on 5 slots")

	look(voice.Usage{Reserved: 3, Cap: 5})
	r.end(ids[3], campaign.OutcomeNoAnswer)
	wantTotal(6, "the 3 reserved, 1 ringing: one more fits")

	look(voice.Usage{InUse: 3, Cap: 5})
	r.end(ids[4], campaign.OutcomeNoAnswer)
	wantTotal(6, "slots taken, starts not counted: each counts twice")

	r.counted(ids[:3]...)
	wantTotal(7, "starts counted: 3 in use and 1 ringing, one more fits")

	look(voice.Usage{InUse: 3, Reserved: 1, Cap: 5})
	r.end(r.placer.placedCalls()[5].Req.CallID, campaign.OutcomeNoAnswer)
	wantTotal(7, "a reservation of a call placed through the API")
	r.wantTerm(0, "every answered leg counted")
	r.noViolations()
}

// Every ending releases the term, and no order of its messages leaves it
// anything but the answered flights whose start is not counted (#640): an
// answer or a count reported twice, a count before its answer (the leg is then
// held until it ends, the safe direction) or after its leg ended, a count for
// an inbound call, which is no flight's, or for a leg still ringing; a callee
// who hangs up inside the window, a leg whose overflow hold expires, a leg
// whose originate never let go of it (design 10 section 5.6: no slot, no
// session, ended by the switch's park timeout), and one whose CDR was lost,
// settled by its backstop. The ringing and talking counts return to zero too.
func TestTheUnstartedTermLeaksNothingOnAnyEndingOrMessageOrder(t *testing.T) {
	r := newTermRig(t, 0, 7, voice.Usage{Cap: 100})
	calls := r.placer.placedCalls()
	if len(calls) != 7 {
		t.Fatalf("%d calls placed, want the campaign's 7", len(calls))
	}
	r.stopAdmitting()
	ids := callIDs(calls)
	talked, early, window, parked, noOK, lostCDR, rang := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6]

	r.answer(talked, talked)
	r.wantTerm(1, "an answer reported twice")
	r.counted(talked, talked)
	r.wantTerm(0, "a start counted twice")

	r.counted(early)
	r.wantTerm(0, "a count before its answer")
	r.answer(early)
	r.wantTerm(1, "the answer after its count")

	r.answer(window, parked, noOK, lostCDR)
	r.wantTerm(5, "four more answers")
	r.counted(uuid.Must(uuid.NewV7()), rang)
	r.wantTerm(5, "a count for an inbound call and one for a leg still ringing")

	r.end(window, campaign.OutcomeAnswered)
	r.wantTerm(4, "a callee who hung up inside the window")
	r.end(parked, campaign.OutcomeOverflow)
	r.wantTerm(3, "a leg whose overflow hold expired")
	r.end(noOK, campaign.OutcomeBotFailed)
	r.wantTerm(2, "a leg the originate never let go of, ended by the park timeout")
	r.end(early, campaign.OutcomeAnswered)
	r.wantTerm(1, "the leg whose count came before its answer ended")
	r.end(talked, campaign.OutcomeAnswered)
	r.wantTerm(1, "a counted leg ended")
	r.counted(talked)
	r.wantTerm(1, "a count after its leg ended")

	// No CDR ever comes for this one and its call is gone: past its ring
	// timeout and the margin the backstop settles it as UNKNOWN.
	r.placer.drop(lostCDR)
	r.clock.Advance(time.Minute + time.Second)
	r.d.Wake()
	r.settle()
	r.wantTerm(0, "a leg whose CDR was lost, settled by its backstop")

	r.end(rang, campaign.OutcomeNoAnswer)
	r.wantTerm(0, "every leg ended")
	if ringing, talking, flights := r.legs(); ringing != 0 || talking != 0 || flights != 0 {
		t.Errorf("after every leg ended %d ringing, %d talking, %d flights, want none", ringing, talking, flights)
	}
	r.noViolations()
}
