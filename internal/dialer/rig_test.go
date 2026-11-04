// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// fakeClock is time under the test's control. Timers fire when Advance passes
// them; the dialer does not depend on that (it also compares the clock at
// every iteration), which is what makes a test deterministic.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at   time.Time
	c    chan time.Time
	done bool
	mu   *sync.Mutex // the clock's, which guards done
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := !t.done
	t.done = true
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), c: make(chan time.Time, 1), mu: &c.mu}
	if d <= 0 {
		t.c <- c.now
		t.done = true
		return t
	}
	c.timers = append(c.timers, t)
	return t
}

// Set moves the clock and fires the timers that are due.
func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
	keep := c.timers[:0]
	for _, t := range c.timers {
		switch {
		case t.done:
		case !t.at.After(now):
			t.done = true
			t.c <- now
		default:
			keep = append(keep, t)
		}
	}
	c.timers = keep
}

func (c *fakeClock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// placed is one call the fake switch was asked for.
type placed struct {
	Req outbound.AIDialRequest
	At  time.Time
}

// fakePlacer stands in for outbound.Service: it records what it is asked,
// refuses the way a script says, and keeps the set of calls that are live.
type fakePlacer struct {
	mu    sync.Mutex
	clock *fakeClock
	calls []placed
	live  map[uuid.UUID]placed
	// byNumber is the live call per number: two at once is a violation.
	byNumber   map[string]uuid.UUID
	seen       map[uuid.UUID]bool
	violations []string
	failures   map[string]int
	fail       func(req outbound.AIDialRequest) error
	gate       chan struct{}
}

func newPlacer(c *fakeClock) *fakePlacer {
	return &fakePlacer{clock: c, live: map[uuid.UUID]placed{}, byNumber: map[string]uuid.UUID{}, seen: map[uuid.UUID]bool{}}
}

func (p *fakePlacer) PlaceAI(ctx context.Context, req outbound.AIDialRequest) (outbound.Placement, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gate != nil {
		select {
		case <-p.gate:
		default:
			p.violations = append(p.violations, "placed before the gate opened: "+req.To)
		}
	}
	if p.fail != nil {
		if err := p.fail(req); err != nil {
			return outbound.Placement{}, err
		}
	}
	if p.seen[req.CallID] {
		p.violations = append(p.violations, "call placed twice: "+req.CallID.String())
	}
	p.seen[req.CallID] = true
	if other, ok := p.byNumber[req.To]; ok {
		p.violations = append(p.violations, fmt.Sprintf("%s dialled while %s is live", req.To, other))
	}
	if req.CallID == uuid.Nil || req.LegID == uuid.Nil || req.CampaignID == uuid.Nil || req.AttemptID == uuid.Nil {
		p.violations = append(p.violations, "placed without its identities")
	}
	pc := placed{Req: req, At: p.clock.Now()}
	p.calls = append(p.calls, pc)
	p.live[req.CallID] = pc
	p.byNumber[req.To] = req.CallID
	return outbound.Placement{CallID: req.CallID, JobID: "job-" + req.CallID.String()[28:]}, nil
}

func (p *fakePlacer) isLive(id uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.live[id]
	return ok
}

// placedCalls is every placement so far, copied under the lock: the dialer
// goroutine appends to p.calls while a test reads it.
func (p *fakePlacer) placedCalls() []placed {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]placed(nil), p.calls...)
}

// setFail swaps the failure hook under the lock, for a test that changes it
// while the dialer runs.
func (p *fakePlacer) setFail(fail func(outbound.AIDialRequest) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = fail
}

func (p *fakePlacer) liveCalls() []placed {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]placed, 0, len(p.live))
	for _, c := range p.live {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Req.To < out[j].Req.To })
	return out
}

func (p *fakePlacer) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *fakePlacer) placedTo(number string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.Req.To == number {
			n++
		}
	}
	return n
}

func (p *fakePlacer) drop(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.live[id]; ok {
		delete(p.byNumber, c.Req.To)
		delete(p.live, id)
	}
}

// fakeSlots is the bot-capacity counter under the test's control.
type fakeSlots struct {
	mu sync.Mutex
	u  voice.Usage
}

func (s *fakeSlots) Usage() voice.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.u
}

func (s *fakeSlots) set(u voice.Usage) {
	s.mu.Lock()
	s.u = u
	s.mu.Unlock()
}

type opts struct {
	maxRinging int
	frequency  time.Duration
	trunkN     int
	providerM  int
	slots      *fakeSlots
	lines      Lines
	// outer calling hours; default 08:00-21:00.
	outer string
	// held keeps the reconcile gate closed until released.
	held bool
	// start is the fake clock's first reading; default Monday 2026-06-01 10:00 New York.
	start time.Time
	// onChange receives the dialer's change hints.
	onChange func(uuid.UUID)
}

type rig struct {
	t      *testing.T
	st     *store.Store
	clock  *fakeClock
	placer *fakePlacer
	slots  *fakeSlots
	cfg    Config
	dep    Deps
	d      atomic.Pointer[Dialer]
	cancel context.CancelFunc
	done   chan struct{}
	gate   chan struct{}
	didID  uuid.UUID
	didNum string
	nDID   int
}

var newYork = func() *time.Location {
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return l
}()

func ny(t *testing.T, local string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", local, newYork)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newRig(t *testing.T, o opts) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "dialer"), 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if o.outer == "" {
		o.outer = "08:00-21:00"
	}
	outer, err := dayrange.Parse(o.outer)
	if err != nil {
		t.Fatal(err)
	}
	st.OutboundHours = outer
	if o.start.IsZero() {
		o.start = ny(t, "2026-06-01 10:00")
	}
	clock := &fakeClock{now: o.start}
	r := &rig{t: t, st: st, clock: clock, placer: newPlacer(clock), slots: o.slots}
	st.OnAttemptFinished = func(f campaign.Finished) {
		if d := r.d.Load(); d != nil {
			d.AttemptFinished(f)
		}
	}
	st.OnDNCAdded = func() {
		if d := r.d.Load(); d != nil {
			d.DNCAdded()
		}
	}
	r.cfg = Config{
		MaxRinging: o.maxRinging, Frequency: o.frequency, OutboundHours: outer,
		TrunkBreakerCount: o.trunkN, ProviderBreakerCount: o.providerM,
	}
	if r.cfg.MaxRinging == 0 {
		r.cfg.MaxRinging = 1000
	}
	if o.trunkN == 0 {
		r.cfg.TrunkBreakerCount = 100000
	}
	if o.providerM == 0 {
		r.cfg.ProviderBreakerCount = 100000
	}
	if o.frequency == 0 {
		r.cfg.Frequency = 24 * time.Hour
	}
	r.dep = Deps{Store: st.Dialer(), Placer: r.placer, IsLive: r.placer.isLive, Clock: clock, Log: slog.New(slog.DiscardHandler), OnChange: o.onChange}
	if o.slots != nil {
		r.dep.Slots = o.slots
	}
	if o.lines != nil {
		r.dep.Lines = o.lines
	}
	if o.held {
		r.gate = make(chan struct{})
		r.placer.gate = r.gate
		r.dep.Reconciled = r.gate
	}
	r.didID, r.didNum = r.newDID("95099")
	r.startDialer()
	t.Cleanup(r.stopDialer)
	return r
}

func (r *rig) startDialer() {
	d := New(r.cfg, r.dep)
	r.d.Store(d)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		d.Run(ctx)
	}()
}

func (r *rig) stopDialer() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
		r.cancel = nil
	}
}

// restart kills the actor and starts a new one on the same database.
func (r *rig) restart() {
	r.stopDialer()
	r.startDialer()
}

func (r *rig) dialer() *Dialer { return r.d.Load() }

// settle returns once the actor has handled everything posted so far and run
// the passes that left work behind.
func (r *rig) settle() {
	r.t.Helper()
	for i := 0; i < 4; i++ {
		if err := r.dialer().Sync(context.Background()); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *rig) tick(d time.Duration) {
	r.t.Helper()
	r.clock.Advance(d)
	r.settle()
}

func (r *rig) at(t time.Time) {
	r.t.Helper()
	r.clock.Set(t)
	r.settle()
}

const flowJSON = `{"id": %q, "specVersion": "v2", "initialNode": "ask",
  "global": {"persona": {"en": "p", "zh": "p"}},
  "nodes": {"ask": {"instruction": {"en": "Ask.", "zh": "Ask."}, "tools": ["hangup"]}}}`

func (r *rig) newDID(number string) (uuid.UUID, string) {
	r.t.Helper()
	ctx := context.Background()
	flowID, err := r.st.Flows().Create(ctx, "flow-"+number, "Flow "+number, []byte(fmt.Sprintf(flowJSON, "flow-"+number)))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.Flows().Publish(ctx, flowID, "test"); err != nil {
		r.t.Fatal(err)
	}
	did, err := r.st.Catalog().CreateDID(ctx, catalog.DID{
		ID: uuid.Must(uuid.NewV7()), Number: number, Language: "en", FlowID: &flowID,
		IsEnabled: true, AllowOutbound: true,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return did.ID, number
}

var everyDay = []string{"MON", "TUE", "WED", "THU", "FRI", "SAT", "SUN"}

type camSpec struct {
	name   string
	zone   string
	window campaign.CallingWindow
	max    int
	ring   int
	policy *campaign.RetryPolicy
	didID  uuid.UUID
	cc     string
}

// newCampaign stores a DRAFT campaign; nil edit takes the defaults: New York,
// 09:00-18:00 every day, five at a time.
func (r *rig) newCampaign(name string, edit func(*camSpec)) campaign.Campaign {
	r.t.Helper()
	s := camSpec{
		name: name, zone: "America/New_York", max: 5, ring: 30, didID: r.didID,
		window: campaign.CallingWindow{Days: everyDay, Ranges: []campaign.TimeRange{{Start: "09:00", End: "18:00"}}},
	}
	if edit != nil {
		edit(&s)
	}
	c := campaign.Campaign{
		ID: uuid.Must(uuid.NewV7()), Name: s.name, DIDID: s.didID, TimeZone: s.zone,
		CallingWindow: s.window, MaxConcurrent: s.max, RingTimeoutSec: s.ring,
		RetryPolicy: campaign.DefaultRetryPolicy(), DefaultCountryCode: s.cc,
	}
	if s.policy != nil {
		c.RetryPolicy = *s.policy
	}
	got, err := r.st.Campaigns().Create(context.Background(), c)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

type contactSpec struct {
	number string
	zone   string
}

func (r *rig) importContacts(c campaign.Campaign, specs []contactSpec) []campaign.Contact {
	r.t.Helper()
	ctx := context.Background()
	rows := make([]campaign.NewContact, len(specs))
	for i, s := range specs {
		rows[i] = campaign.NewContact{
			ID: uuid.Must(uuid.NewV7()), Number: s.number, NumberRaw: s.number, TimeZone: s.zone,
			Slots: map[string]string{"name": fmt.Sprintf("c%d", i)},
		}
	}
	if _, err := r.st.Campaigns().ImportContacts(ctx, c.ID, rows); err != nil {
		r.t.Fatal(err)
	}
	return r.contacts(c)
}

func numbers(prefix string, n int) []contactSpec {
	out := make([]contactSpec, n)
	for i := range out {
		out[i] = contactSpec{number: fmt.Sprintf("+8613%s%07d", prefix, i)}
	}
	return out
}

func (r *rig) start(c campaign.Campaign) campaign.Campaign {
	r.t.Helper()
	got, err := r.dialer().Start(context.Background(), c.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	r.rearm(c)
	r.dialer().Wake()
	r.settle()
	return got
}

// rearm makes the contacts that start (or an import) armed with the database's
// clock due at the fake clock's time, which is the same instant in production.
func (r *rig) rearm(c campaign.Campaign) {
	r.t.Helper()
	if _, err := r.st.Pool.Exec(context.Background(),
		`UPDATE campaign_contacts SET next_attempt_at = $2
		 WHERE campaign_id = $1 AND status = 'ACTIVE' AND next_attempt_at IS NOT NULL
		   AND last_outcome IS NULL AND next_attempt_at > $2`, c.ID, r.clock.Now()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) contacts(c campaign.Campaign) []campaign.Contact {
	r.t.Helper()
	list, _, err := r.st.Campaigns().ListContacts(context.Background(), c.ID, campaign.ContactFilter{Limit: 100000})
	if err != nil {
		r.t.Fatal(err)
	}
	return list
}

func (r *rig) contactOf(c campaign.Campaign, number string) campaign.Contact {
	r.t.Helper()
	for _, ct := range r.contacts(c) {
		if ct.Number == number {
			return ct
		}
	}
	r.t.Fatalf("no contact %s", number)
	return campaign.Contact{}
}

func (r *rig) attempts(c campaign.Campaign) []campaign.Attempt {
	r.t.Helper()
	var all []campaign.Attempt
	for offset := 0; ; offset += 1000 {
		list, _, err := r.st.Campaigns().ListAttempts(context.Background(), c.ID, campaign.AttemptFilter{Limit: 1000, Offset: offset})
		if err != nil {
			r.t.Fatal(err)
		}
		all = append(all, list...)
		if len(list) < 1000 {
			return all
		}
	}
}

func (r *rig) campaignNow(c campaign.Campaign) campaign.Campaign {
	r.t.Helper()
	got, err := r.st.Campaigns().Get(context.Background(), c.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func (r *rig) progress(c campaign.Campaign) campaign.Progress {
	p, _ := r.dialer().Progress(c.ID)
	return p
}

// How a call can end.
const (
	endAnswered  = "answered"
	endHangup1s  = "hangup1s"
	endNoAnswer  = "noanswer"
	endBusy      = "busy"
	endRejected  = "rejected"
	endInvalid   = "invalid"
	endPlacement = "placement"
	endBotFailed = "botfailed"
	// endBotRefused: the customer answered and the bot leg was refused (a
	// 503 from the bot's own UAS, #459). The switch's cause is its own; the
	// call carries botReason BOT_LEG_FAILED and never had a bot.
	endBotRefused = "botrefused"
)

func cdrFor(callID uuid.UUID, to, kind string, start, end time.Time) store.CDR {
	cdr := store.CDR{
		CallID: callID, StartedAt: start, EndedAt: end, CallType: "OUTBOUND",
		FromNumber: "95099", ToNumber: to, DID: "95099",
		Legs: []store.Leg{{Kind: "TRUNK", Label: "customer"}},
	}
	answer := func(cause, status string, talk time.Duration, bot bool) {
		cdr.AnsweredAt = start.Add(2 * time.Second)
		cdr.EndedAt = cdr.AnsweredAt.Add(talk)
		cdr.TalkSec = int(talk.Seconds())
		cdr.HangupCause, cdr.Status = cause, status
		if bot {
			cdr.BotSec = int(talk.Seconds())
			cdr.Legs = append(cdr.Legs, store.Leg{Kind: "BOT", Label: "bot"})
		}
	}
	switch kind {
	case endAnswered:
		answer("NORMAL_CLEARING", store.CDRStatusAnswered, 45*time.Second, true)
	case endHangup1s:
		answer("NORMAL_CLEARING", store.CDRStatusAnswered, 700*time.Millisecond, false)
	case endBotFailed:
		answer("MEDIA_OR_PROVIDER_FAILURE", store.CDRStatusFailed, 20*time.Second, true)
	case endBotRefused:
		answer("NORMAL_TEMPORARY_FAILURE", store.CDRStatusAnswered, time.Second, false)
		cdr.UserData = map[string]any{"botReason": "BOT_LEG_FAILED"}
	case endNoAnswer:
		cdr.HangupCause, cdr.Status = "NO_ANSWER", store.CDRStatusNoAnswer
	case endBusy:
		cdr.HangupCause, cdr.Status = "USER_BUSY", store.CDRStatusBusy
	case endRejected:
		cdr.HangupCause, cdr.Status = "CALL_REJECTED", store.CDRStatusFailed
	case endInvalid:
		cdr.HangupCause, cdr.Status = "UNALLOCATED_NUMBER", store.CDRStatusFailed
	case endPlacement:
		cdr.HangupCause, cdr.Status = "GATEWAY_DOWN", store.CDRStatusFailed
		cdr.Tech = map[string]any{"isPlacementFailure": true}
	default:
		panic("unknown ending " + kind)
	}
	return cdr
}

// end writes the CDR of a live call, at the fake clock's time, the way the
// switch's events would have: the finish routine runs in its transaction and
// the dialer is told after it commits.
func (r *rig) end(c placed, kind string) {
	r.t.Helper()
	cdr := cdrFor(c.Req.CallID, c.Req.To, kind, c.At, r.clock.Now())
	if err := r.st.Ledger().InsertCDR(context.Background(), cdr); err != nil {
		r.t.Fatalf("insert cdr: %v", err)
	}
	r.placer.drop(c.Req.CallID)
}

// endAll ends every live call with kind(number) and settles.
func (r *rig) endAll(kind func(number string) string) int {
	r.t.Helper()
	live := r.placer.liveCalls()
	n := 0
	for _, c := range live {
		if k := kind(c.Req.To); k != "" {
			r.end(c, k)
			n++
		}
	}
	r.settle()
	return n
}

// keepRinging ends every call with kind except the one to anchor, which stays
// live so that the campaign has a contact left and does not complete.
func keepRinging(anchor, kind string) func(string) string {
	return func(number string) string {
		if number == anchor {
			return ""
		}
		return kind
	}
}

func (r *rig) answer(c placed) {
	r.dialer().Answered(c.Req.CallID)
	r.settle()
}

func (r *rig) noViolations() {
	r.t.Helper()
	r.placer.mu.Lock()
	defer r.placer.mu.Unlock()
	if len(r.placer.violations) > 0 {
		r.t.Fatalf("placer violations: %v", r.placer.violations)
	}
}

func (r *rig) openGate() {
	r.t.Helper()
	close(r.gate)
	r.settle()
}
