// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
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

// The capacity simulation: the real dialer actor on a fake clock, an in-memory
// store with an endless list per campaign, a placer whose calls ring, answer
// and end after realistic times drawn from a seeded generator, and a bot-slot
// counter that behaves like voice.BotSlots with the overflow hold. No database,
// so thousands of attempts run in seconds.

// simStore is the dialer's Store in memory. Every campaign has an endless list
// of fresh numbers; nothing is retried.
type simStore struct {
	mu       sync.Mutex
	camps    []campaign.Campaign
	next     map[uuid.UUID]int
	attempts map[uuid.UUID]campaign.Attempt
	history  map[uuid.UUID][]campaign.Outcome // newest first, for the warm start
	tally    map[uuid.UUID]map[campaign.Outcome]int
	onFinish func(campaign.Finished)
	voided   int
	owner    map[uuid.UUID]uuid.UUID // contact -> campaign
}

func newSimStore() *simStore {
	return &simStore{
		next: map[uuid.UUID]int{}, owner: map[uuid.UUID]uuid.UUID{}, attempts: map[uuid.UUID]campaign.Attempt{},
		history: map[uuid.UUID][]campaign.Outcome{}, tally: map[uuid.UUID]map[campaign.Outcome]int{},
	}
}

var errSimUnused = errors.New("simStore: not used by the simulation")

func (s *simStore) RunningCampaigns(context.Context) ([]campaign.Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]campaign.Campaign(nil), s.camps...), nil
}
func (s *simStore) Start(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, errSimUnused
}
func (s *simStore) Pause(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, errSimUnused
}
func (s *simStore) Resume(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, errSimUnused
}
func (s *simStore) Cancel(context.Context, uuid.UUID) (campaign.Campaign, error) {
	return campaign.Campaign{}, errSimUnused
}

func (s *simStore) Due(_ context.Context, id uuid.UUID, _ time.Time, _ time.Duration, limit int) ([]campaign.Contact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit = min(limit, 8) // a short batch is as good as a long one and costs less
	out := make([]campaign.Contact, 0, limit)
	for i := 0; i < limit; i++ {
		n := s.next[id] + i
		cid := uuid.NewSHA1(id, []byte(fmt.Sprint(n)))
		s.owner[cid] = id
		out = append(out, campaign.Contact{
			ID: cid, CampaignID: id,
			Number: fmt.Sprintf("+86139%08d", n), Status: campaign.ContactActive,
		})
	}
	return out, nil
}

func (s *simStore) HeldByFrequency(context.Context, uuid.UUID, time.Time, time.Duration, int) ([]campaign.Held, error) {
	return nil, nil
}
func (s *simStore) Defer(context.Context, []uuid.UUID, []time.Time, campaign.HoldKind) error {
	return nil
}
func (s *simStore) Waiting(context.Context, uuid.UUID, time.Time, uuid.UUID, int) ([]campaign.Waiting, error) {
	return nil, nil
}
func (s *simStore) Rearm(context.Context, []campaign.Waiting, []time.Time) (int, error) {
	return 0, nil
}
func (s *simStore) Pending(context.Context, uuid.UUID, time.Time) (campaign.Pending, error) {
	return campaign.Pending{ActiveTotal: 1, AllTotal: 1, DueNow: 1}, nil
}
func (s *simStore) EarliestPending(context.Context, uuid.UUID) (*campaign.Contact, error) {
	return nil, nil
}
func (s *simStore) FrequencyHold(context.Context, uuid.UUID, time.Time, time.Duration) (*time.Time, error) {
	return nil, nil
}
func (s *simStore) RearmHeld(context.Context, time.Time) error { return nil }
func (s *simStore) DialsToday(_ context.Context, numbers []string, _ []time.Time, _ time.Time) ([]int, error) {
	return make([]int, len(numbers)), nil
}
func (s *simStore) Complete(context.Context, uuid.UUID, time.Time) (bool, error) { return false, nil }
func (s *simStore) PauseFor(context.Context, uuid.UUID, campaign.PauseReason) (bool, error) {
	return false, nil
}
func (s *simStore) DNCContacts(context.Context, int) ([]uuid.UUID, error) { return nil, nil }
func (s *simStore) MarkDialingDNC(context.Context) error                  { return nil }
func (s *simStore) Dialing(context.Context) ([]campaign.DialingAttempt, error) {
	return nil, nil
}

func (s *simStore) RecentOutcomesSince(_ context.Context, id uuid.UUID, _ time.Time, limit int) ([]campaign.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.history[id]
	if len(h) > limit {
		h = h[:limit]
	}
	return append([]campaign.Outcome(nil), h...), nil
}

func (s *simStore) Begin(_ context.Context, contactID uuid.UUID, now time.Time) (campaign.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cid := s.owner[contactID]
	s.next[cid]++ // contacts are taken in order
	a := campaign.Attempt{
		ID: uuid.Must(uuid.NewV7()), CampaignID: cid, ContactID: contactID, CallID: uuid.Must(uuid.NewV7()), LegID: uuid.Must(uuid.NewV7()),
		State: campaign.AttemptDialing, StartedAt: now,
	}
	s.attempts[a.CallID] = a
	return a, nil
}

func (s *simStore) SetJob(context.Context, uuid.UUID, string) error { return nil }

func (s *simStore) Finish(_ context.Context, callID uuid.UUID, e campaign.Ending) (*campaign.Finished, error) {
	s.mu.Lock()
	a, ok := s.attempts[callID]
	if !ok {
		s.mu.Unlock()
		return nil, campaign.ErrNoAttempt
	}
	delete(s.attempts, callID)
	s.history[a.CampaignID] = append([]campaign.Outcome{e.Outcome}, s.history[a.CampaignID]...)
	if s.tally[a.CampaignID] == nil {
		s.tally[a.CampaignID] = map[campaign.Outcome]int{}
	}
	s.tally[a.CampaignID][e.Outcome]++
	f := campaign.Finished{CampaignID: a.CampaignID, ContactID: a.ContactID, AttemptID: a.ID, CallID: callID, Outcome: e.Outcome}
	cb := s.onFinish
	s.mu.Unlock()
	if cb != nil {
		cb(f)
	}
	return &f, nil
}

func (s *simStore) Void(_ context.Context, callID uuid.UUID, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.attempts[callID]; !ok {
		return campaign.ErrNoAttempt
	}
	delete(s.attempts, callID)
	s.voided++
	return nil
}

func (s *simStore) FinishFromLedger(context.Context, uuid.UUID) (bool, error) { return false, nil }

func (s *simStore) count(id uuid.UUID, o campaign.Outcome) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tally[id][o]
}

// simEvent kinds.
const (
	evAnswer = iota
	evRingOut
	evTalkEnd
	evHoldExpire
	// evSessionStart is the window's end (startWindow): the leg's slot is
	// taken, its provider session is counted, and the dialer is told.
	evSessionStart
	// evWindowHangup is a callee hanging up inside the window (#640): no
	// slot, no session, the unstarted term releases as the leg ends.
	evWindowHangup
	// evTurn is a provider session reporting what one turn cost (#604).
	evTurn
	// evTick moves the clock on by a second when nothing else would: a hold
	// by the provider's quota can end by time alone, after the last call has
	// ended, and the pass after a tick is what dials again then, as the
	// dialer's own timer does outside the simulation.
	evTick
	// evTimer is the rate meter's start gate looking at its queue again
	// (#644): its timers run on the simulation's clock, as events.
	evTimer
)

type simEvent struct {
	at    time.Time
	seq   int
	kind  int
	call  uuid.UUID
	timer *simTimer
}

// simTimer is one of the rate meter's timers (ratemeter.Config.AfterFunc).
type simTimer struct {
	f         func()
	isStopped bool
}

type eventHeap []simEvent

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(simEvent)) }
func (h *eventHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// simCall is one placed call and what is going to happen to it.
type simCall struct {
	camp     uuid.UUID
	did      string
	talk     time.Duration
	isParked bool
	hasSlot  bool
	// isRefused: the provider refused its session; the platform hangs up a
	// second later and the outcome is BOT_FAILED (#604).
	isRefused bool
}

// sim wires the pieces; it implements dialer.Placer, Slots and Lines.
type sim struct {
	t     *testing.T
	clock *fakeClock
	store *simStore
	d     *Dialer
	rng   *rand.Rand

	mu     sync.Mutex
	events eventHeap
	seq    int
	calls  map[uuid.UUID]*simCall
	parked []uuid.UUID
	// answerRate is each campaign's probability that a call is answered, at a
	// given time since the start.
	answerRate map[uuid.UUID]func(since time.Duration) float64
	start      time.Time

	capacity, outboundReserve int
	inUse                     int
	// inbound is the bot sessions inbound calls hold, set by the test.
	inbound int
	hold    time.Duration
	// startWindow and hangupInWindow model the production window between an
	// answer and the leg's counted session start (#640); isWindowJittered draws
	// each call's span instead of fixing it.
	startWindow      time.Duration
	hangupInWindow   float64
	isWindowJittered bool

	// accounting
	lastAt        time.Time
	slotSeconds   float64 // integral of bot sessions in use (all kinds)
	measuredSince time.Time
	peakInUse     int
	placements    int
	failPlace     func(req outbound.AIDialRequest) error
	lines         *simLines

	// prov is the speech provider and its quota (#604), and meter the
	// process's account of it that admission reads; nil: every session
	// starts and nothing is metered. isStartGated makes an answered call's
	// session start wait in the meter's start gate (#644), and startWaits
	// records how each of those starts went. counted is the calls whose
	// session start was counted in the event being applied, told to the
	// dialer when it is whole.
	prov         *simProvider
	meter        *ratemeter.Meter
	isStartGated bool
	startWaits   []ratemeter.Start
	counted      []uuid.UUID
}

// simLines is the DID line account as the dialer sees it.
type simLines struct {
	mu     sync.Mutex
	limits map[string]int
	inUse  map[string]int
	peak   map[string]int
}

func (l *simLines) DIDLimits(context.Context) (map[string]int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for k, v := range l.limits {
		out[k] = v
	}
	return out, nil
}
func (l *simLines) LinesInUse(did string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse[did]
}
func (l *simLines) take(did string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.limits[did]; ok && l.inUse[did] >= lim {
		return false
	}
	l.inUse[did]++
	if l.inUse[did] > l.peak[did] {
		l.peak[did] = l.inUse[did]
	}
	return true
}
func (l *simLines) give(did string) {
	l.mu.Lock()
	l.inUse[did]--
	l.mu.Unlock()
}

type simSpec struct {
	capacity        int
	inboundReserve  int // Config.InboundReserve (campaign admission)
	campaigns       int
	rates           []func(since time.Duration) float64
	seed            int64
	maxConcurrent   int
	noOverdial      bool
	tighten, stop   float64
	didLimit        map[string]int // by DID number
	dids            []string       // DID of each campaign; default "95099"
	primeHistory    func(id uuid.UUID) []campaign.Outcome
	maxRinging      int
	outboundReserve int
	onChange        func(uuid.UUID)
	// provider is the speech provider every answered call opens a session
	// with; nil: sessions always start. isGated wires the rate meter into
	// admission with sessionLimit per sessionPeriod and tokenLimit per
	// tokenPeriod as the limits it paces by (a zero period is a minute); the
	// meter counts over the same periods and is fed either way. isStartGated
	// makes each session start wait in the meter's start gate, up to
	// ratemeter.StartWaitBound, as a campaign call's does (#644).
	provider                   *simProvider
	isGated                    bool
	sessionLimit, tokenLimit   int
	sessionPeriod, tokenPeriod time.Duration
	isStartGated               bool
	// startWindow is how long an answered leg stands between the answer and
	// its provider session's counted start — in production the reservation,
	// the INVITE, +OK and the session take a little while; 0 is no window
	// (#640). hangupInWindow is the share of answered calls whose callee
	// hangs up inside it.
	startWindow    time.Duration
	hangupInWindow float64
	// isWindowJittered draws each answered call's window uniformly in
	// [startWindow/2, 3·startWindow/2) instead of the fixed span.
	isWindowJittered bool
}

func newSim(t *testing.T, sp simSpec) *sim {
	t.Helper()
	start := ny(t, "2026-06-01 09:30")
	s := &sim{
		t: t, clock: &fakeClock{now: start}, store: newSimStore(), rng: rand.New(rand.NewSource(sp.seed)),
		calls: map[uuid.UUID]*simCall{}, answerRate: map[uuid.UUID]func(time.Duration) float64{},
		start: start, capacity: sp.capacity, outboundReserve: sp.outboundReserve, hold: 1500 * time.Millisecond,
		lines: &simLines{limits: sp.didLimit, inUse: map[string]int{}, peak: map[string]int{}},
	}
	if s.lines.limits == nil {
		s.lines.limits = map[string]int{}
	}
	zone := "America/New_York"
	for i := 0; i < sp.campaigns; i++ {
		did := "95099"
		if i < len(sp.dids) {
			did = sp.dids[i]
		}
		c := campaign.Campaign{
			ID: uuid.Must(uuid.NewV7()), Name: fmt.Sprintf("c%d", i), DIDNumber: did, Status: campaign.StatusRunning,
			TimeZone: zone, MaxConcurrent: max(sp.maxConcurrent, 1), RingTimeoutSec: 30,
			CallingWindow: campaign.CallingWindow{Days: everyDay, Ranges: []campaign.TimeRange{{Start: "09:00", End: "18:00"}}},
			RetryPolicy:   campaign.DefaultRetryPolicy(),
		}
		s.store.camps = append(s.store.camps, c)
		rate := func(time.Duration) float64 { return 0.2 }
		if i < len(sp.rates) {
			rate = sp.rates[i]
		}
		s.answerRate[c.ID] = rate
		if sp.primeHistory != nil {
			s.store.history[c.ID] = sp.primeHistory(c.ID)
		}
	}
	outer, err := dayrange.Parse("08:00-21:00")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		MaxRinging: max(sp.maxRinging, 1), OutboundHours: outer, Frequency: 24 * time.Hour,
		TrunkBreakerCount: 1 << 30, ProviderBreakerCount: 1 << 30,
		InboundReserve: sp.inboundReserve, OverflowTightenRate: sp.tighten, OverflowStopRate: sp.stop,
		noOverdial: sp.noOverdial,
	}
	if cfg.MaxRinging == 1 && sp.maxRinging == 0 {
		cfg.MaxRinging = 100000
	}
	deps := Deps{
		Store: s.store, Placer: s, Slots: s, Lines: s.lines, Clock: s.clock,
		IsLive: s.isLive, Log: slog.New(slog.DiscardHandler), OnChange: sp.onChange,
	}
	s.prov = sp.provider
	s.startWindow = sp.startWindow
	s.hangupInWindow = sp.hangupInWindow
	s.isWindowJittered = sp.isWindowJittered
	meterCfg := ratemeter.Config{SessionPeriod: sp.sessionPeriod, TokenPeriod: sp.tokenPeriod, AfterFunc: s.afterFunc}
	if sp.isStartGated {
		meterCfg.SessionLimit = sp.sessionLimit
		s.isStartGated = true
	}
	s.meter = ratemeter.New(s.clock.Now, meterCfg)
	if s.prov != nil {
		s.push(start.Add(time.Second), evTick, uuid.Nil)
	}
	if sp.isGated {
		deps.ProviderRate = s.meter
		cfg.ProviderSessionLimit, cfg.ProviderSessionPeriod = sp.sessionLimit, sp.sessionPeriod
		cfg.ProviderTokenLimit, cfg.ProviderTokenPeriod = sp.tokenLimit, sp.tokenPeriod
	}
	s.d = New(cfg, deps)
	s.store.onFinish = s.d.AttemptFinished
	s.lastAt = start
	s.measuredSince = start
	return s
}

func (s *sim) isLive(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.calls[id]
	return ok
}

func (s *sim) push(at time.Time, kind int, call uuid.UUID) {
	s.seq++
	heap.Push(&s.events, simEvent{at: at, seq: s.seq, kind: kind, call: call})
}

// afterFunc is the rate meter's timer, an event on the simulation's clock.
// The meter sets one only from Pace and from its own timer, which both run
// on the simulation's goroutine with s.mu held (beginTalk, step).
func (s *sim) afterFunc(d time.Duration, f func()) func() bool {
	t := &simTimer{f: f}
	s.seq++
	heap.Push(&s.events, simEvent{at: s.clock.Now().Add(d), seq: s.seq, kind: evTimer, timer: t})
	return func() bool {
		was := !t.isStopped
		t.isStopped = true
		return was
	}
}

// Usage is the bot-capacity counter as the dialer reads it.
func (s *sim) Usage() voice.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return voice.Usage{InUse: s.inUse + s.inbound, Waiting: len(s.parked), Cap: s.capacity, InboundReserve: s.outboundReserve}
}

// PlaceAI is the originate: it takes a line, draws what becomes of the call and
// schedules it.
func (s *sim) PlaceAI(_ context.Context, req outbound.AIDialRequest) (outbound.Placement, error) {
	if s.failPlace != nil {
		if err := s.failPlace(req); err != nil {
			return outbound.Placement{}, err
		}
	}
	if !s.lines.take(req.DIDNumber) {
		return outbound.Placement{}, outbound.ErrDIDChannelsExhausted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.placements++
	now := s.clock.Now()
	call := &simCall{camp: req.CampaignID, did: req.DIDNumber}
	s.calls[req.CallID] = call
	if s.rng.Float64() < s.answerRate[req.CampaignID](now.Sub(s.start)) {
		ring := time.Duration(4+s.rng.Intn(9)) * time.Second // 4-12 s
		call.talk = time.Duration(60+s.rng.Intn(71)) * time.Second
		s.push(now.Add(ring), evAnswer, req.CallID)
	} else {
		ring := 30 * time.Second
		if s.rng.Intn(10) == 0 {
			ring = 3 * time.Second // busy
		}
		s.push(now.Add(ring), evRingOut, req.CallID)
	}
	return outbound.Placement{CallID: req.CallID, JobID: "job"}, nil
}

// step applies the next event, or reports that there is none before until.
//
// The dialer is its own goroutine and must not look while an event is half
// applied: whether a pass ran before or after part of an event would be the
// scheduler's choice, and one seed would give one run on a laptop and another
// on CI (#604: 3 refusals and 9). So nothing wakes it until the event is
// whole: its timers are held for the run (run), and every change the event
// makes to the calls, the bot slots and the meter is in place before the
// dialer is told of any of them. Then the pass the event causes runs.
func (s *sim) step(until time.Time) bool {
	s.mu.Lock()
	if len(s.events) == 0 || s.events[0].at.After(until) {
		s.mu.Unlock()
		return false
	}
	ev := heap.Pop(&s.events).(simEvent)
	s.account(ev.at)
	s.mu.Unlock()
	s.clock.Set(ev.at)

	switch ev.kind {
	case evAnswer:
		if s.startWindow > 0 {
			// The production order (outbound's onAnswer wakes the dialer,
			// then the reservation, the INVITE and the session take the
			// window, #640): the leg stands in the unstarted term until its
			// counted start or its end, whichever comes first. The window's
			// events are pushed before the dialer is told, because posting
			// wakes it and a pass must never run while an event is half
			// applied.
			s.mu.Lock()
			w := s.startWindow
			if s.isWindowJittered {
				w = w/2 + time.Duration(s.rng.Int63n(int64(w)))
			}
			if s.hangupInWindow > 0 && s.rng.Float64() < s.hangupInWindow {
				s.push(ev.at.Add(w/2), evWindowHangup, ev.call)
			} else {
				s.push(ev.at.Add(w), evSessionStart, ev.call)
			}
			s.mu.Unlock()
			s.d.Answered(ev.call)
		} else {
			// The dialer hears of the answer once the call has its slot and
			// its session has started, which here take no time, so no pass
			// finds a leg that has stopped ringing and is in neither Usage
			// nor the meter.
			s.bridge(ev.at, ev.call)
			s.d.Answered(ev.call)
		}
	case evSessionStart:
		s.mu.Lock()
		_, alive := s.calls[ev.call]
		s.mu.Unlock()
		if alive {
			s.bridge(ev.at, ev.call)
		}
	case evWindowHangup:
		s.end(ev.call, campaign.OutcomeAnswered)
	case evRingOut:
		s.end(ev.call, campaign.OutcomeNoAnswer)
	case evTalkEnd:
		s.mu.Lock()
		s.inUse--
		outcome := campaign.OutcomeAnswered
		if s.calls[ev.call].isRefused {
			outcome = campaign.OutcomeBotFailed
		}
		s.serveParked(ev.at)
		s.mu.Unlock()
		s.end(ev.call, outcome)
	case evHoldExpire:
		s.mu.Lock()
		isParked := false
		for i, id := range s.parked {
			if id == ev.call {
				s.parked = append(s.parked[:i], s.parked[i+1:]...)
				isParked = true
				break
			}
		}
		s.mu.Unlock()
		if isParked {
			s.end(ev.call, campaign.OutcomeOverflow)
		}
	case evTick:
		s.mu.Lock()
		s.push(ev.at.Add(time.Second), evTick, uuid.Nil)
		s.mu.Unlock()
	case evTimer:
		s.mu.Lock()
		if !ev.timer.isStopped {
			ev.timer.isStopped = true
			ev.timer.f()
		}
		s.mu.Unlock()
	case evTurn:
		s.mu.Lock()
		if call, ok := s.calls[ev.call]; ok && !call.isRefused {
			s.meter.TokensUsed(s.prov.tokensPerTurn)
			s.prov.spend(ev.at, s.prov.tokensPerTurn)
			s.push(ev.at.Add(s.prov.turnEvery), evTurn, ev.call)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	counted := s.counted
	s.counted = nil
	s.mu.Unlock()
	for _, id := range counted {
		s.d.SessionCounted(id)
	}
	s.d.Wake()
	if err := s.d.Sync(context.Background()); err != nil {
		s.t.Fatal(err)
	}
	return true
}

// bridge gives an answered call its bot slot, or parks it in the overflow
// hold when there is none.
func (s *sim) bridge(at time.Time, id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := s.calls[id]
	if s.inUse+s.inbound < s.capacity {
		s.inUse++
		call.hasSlot = true
		s.beginTalk(at, id)
		return
	}
	call.isParked = true
	s.parked = append(s.parked, id)
	s.push(at.Add(s.hold), evHoldExpire, id)
}

// end finishes a call: its line goes back and its attempt gets its outcome.
func (s *sim) end(id uuid.UUID, o campaign.Outcome) {
	s.mu.Lock()
	call := s.calls[id]
	delete(s.calls, id)
	s.mu.Unlock()
	s.lines.give(call.did)
	_, _ = s.store.Finish(context.Background(), id, campaign.Ending{Outcome: o, EndedAt: s.clock.Now()})
}

func (s *sim) serveParked(at time.Time) {
	for len(s.parked) > 0 && s.inUse+s.inbound < s.capacity {
		id := s.parked[0]
		s.parked = s.parked[1:]
		call := s.calls[id]
		call.isParked, call.hasSlot = false, true
		s.inUse++
		s.beginTalk(at, id)
	}
}

// beginTalk is an answered call that has its bot slot: the bot opens its
// provider session, counted on the meter before the dial as the orchestrator
// counts it, and the conversation runs. With the start gate the start first
// waits in the meter for room in the session window, holding its slot, as a
// campaign call's does (#644). Called with s.mu held.
func (s *sim) beginTalk(at time.Time, id uuid.UUID) {
	if s.prov == nil {
		s.push(at.Add(s.calls[id].talk), evTalkEnd, id)
		// No meter, so the leg's start is counted nowhere: the unstarted
		// term releases here.
		s.counted = append(s.counted, id)
		return
	}
	if !s.isStartGated {
		s.meter.SessionStarted()
		s.openSession(id)
		return
	}
	// The start is made here when the window has room, else from a later
	// evTimer; either way on this goroutine with s.mu held.
	s.meter.Pace(ratemeter.StartWaitBound, func(st ratemeter.Start) {
		s.startWaits = append(s.startWaits, st)
		s.openSession(id)
	})
}

// openSession dials the provider now. A session the provider refuses is hung
// up a second later. Called with s.mu held.
func (s *sim) openSession(id uuid.UUID) {
	// The start the gate just granted is counted on the meter, so the leg
	// leaves the unstarted term — even when the provider then refuses it,
	// as in production the refusal follows the counted start. The message
	// goes out at the step's end (counted): a post wakes the dialer, and a
	// pass must never run while an event is half applied.
	s.counted = append(s.counted, id)
	at := s.clock.Now()
	call := s.calls[id]
	if !s.prov.start(at) {
		s.meter.QuotaRefused(ratemeter.LimitUnknown)
		call.isRefused = true
		s.push(at.Add(time.Second), evTalkEnd, id)
		return
	}
	if s.prov.tokensPerTurn > 0 {
		s.push(at.Add(s.prov.turnEvery), evTurn, id)
	}
	s.push(at.Add(call.talk), evTalkEnd, id)
}

// account integrates the bot sessions in use up to at.
func (s *sim) account(at time.Time) {
	if at.After(s.lastAt) {
		if s.lastAt.After(s.measuredSince) || s.lastAt.Equal(s.measuredSince) {
			s.slotSeconds += float64(s.inUse+s.inbound) * at.Sub(s.lastAt).Seconds()
		}
		s.lastAt = at
	}
	if n := s.inUse + s.inbound; n > s.peakInUse {
		s.peakInUse = n
	}
}

func (s *sim) settle() {
	for i := 0; i < 3; i++ {
		if err := s.d.Sync(context.Background()); err != nil {
			s.t.Fatal(err)
		}
	}
}

// run drives the simulation for d of simulated time. The dialer's timers do
// not fire while it does: a pass follows every event (step), at the event's
// time, which is when a timer that came due since the last one would have
// fired, and a timer firing on its own could wake the dialer while the next
// event is half applied.
func (s *sim) run(d time.Duration) {
	s.t.Helper()
	s.clock.Hold(true)
	defer s.clock.Hold(false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.d.Run(ctx) }()
	defer func() { cancel(); <-done }()

	s.settle()
	end := s.start.Add(d)
	for s.step(end) {
	}
}

// checkTerm fails the test unless the dialer's unstarted term (#640) is the
// answered legs still in flight whose start was never counted: a leg counted
// twice, or taken and never released, shows here. Called after run, when the
// dialer's goroutine has stopped.
func (s *sim) checkTerm() {
	s.t.Helper()
	want := 0
	for _, f := range s.d.flights {
		if f.isAnswered && !f.isSessionCounted {
			want++
		}
	}
	if s.d.unstarted != want {
		s.t.Errorf("the unstarted term is %d, want %d answered-but-uncounted legs in flight", s.d.unstarted, want)
	}
}

// measureFrom starts the utilisation measurement at the current simulated time.
func (s *sim) measureFrom() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.measuredSince = s.clock.Now()
	s.lastAt = s.clock.Now()
	s.slotSeconds = 0
}

func (s *sim) utilisation(until time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account(until)
	span := until.Sub(s.measuredSince).Seconds()
	if span <= 0 {
		return 0
	}
	return s.slotSeconds / span / float64(s.capacity)
}

func (s *sim) overflowRate(id uuid.UUID) float64 {
	ov := s.store.count(id, campaign.OutcomeOverflow)
	answered := ov + s.store.count(id, campaign.OutcomeAnswered)
	if answered == 0 {
		return 0
	}
	return float64(ov) / float64(answered)
}
