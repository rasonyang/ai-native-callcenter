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
func (s *simStore) Defer(context.Context, []uuid.UUID, []time.Time) error { return nil }
func (s *simStore) RearmUntried(context.Context, uuid.UUID, time.Time) error {
	return nil
}
func (s *simStore) Pending(context.Context, uuid.UUID, time.Time) (campaign.Pending, error) {
	return campaign.Pending{ActiveTotal: 1, AllTotal: 1, DueNow: 1}, nil
}
func (s *simStore) EarliestPending(context.Context, uuid.UUID) (*campaign.Contact, error) {
	return nil, nil
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
)

type simEvent struct {
	at   time.Time
	seq  int
	kind int
	call uuid.UUID
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

	// accounting
	lastAt        time.Time
	slotSeconds   float64 // integral of bot sessions in use (all kinds)
	measuredSince time.Time
	peakInUse     int
	placements    int
	failPlace     func(req outbound.AIDialRequest) error
	lines         *simLines
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
	s.d = New(cfg, Deps{
		Store: s.store, Placer: s, Slots: s, Lines: s.lines, Clock: s.clock,
		IsLive: s.isLive, Log: slog.New(slog.DiscardHandler), OnChange: sp.onChange,
	})
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
		s.d.Answered(ev.call)
		s.mu.Lock()
		call := s.calls[ev.call]
		if s.inUse+s.inbound < s.capacity {
			s.inUse++
			call.hasSlot = true
			s.push(ev.at.Add(call.talk), evTalkEnd, ev.call)
		} else {
			call.isParked = true
			s.parked = append(s.parked, ev.call)
			s.push(ev.at.Add(s.hold), evHoldExpire, ev.call)
		}
		s.mu.Unlock()
	case evRingOut:
		s.end(ev.call, campaign.OutcomeNoAnswer)
	case evTalkEnd:
		s.mu.Lock()
		s.inUse--
		s.mu.Unlock()
		s.end(ev.call, campaign.OutcomeAnswered)
		s.mu.Lock()
		s.serveParked(ev.at)
		s.mu.Unlock()
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
	}
	s.d.Wake()
	if err := s.d.Sync(context.Background()); err != nil {
		s.t.Fatal(err)
	}
	return true
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
		s.push(at.Add(call.talk), evTalkEnd, id)
	}
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

// run drives the simulation for d of simulated time.
func (s *sim) run(d time.Duration) {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.d.Run(ctx) }()
	defer func() { cancel(); <-done }()

	s.settle()
	end := s.start.Add(d)
	for s.step(end) {
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
