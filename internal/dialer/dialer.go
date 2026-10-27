// SPDX-License-Identifier: Apache-2.0

// Package dialer is the campaign dialer: one actor goroutine that owns
// admission, the count of calls in flight and every campaign transition
// (docs/design/10-campaigns.md section 5).
//
// The actor wakes on events, never on a tick: an attempt finishing, a bot slot
// freeing, a do-not-call insertion, an API command, and one timer re-armed to
// the earliest moment anything can become due. A last 60-second scan exists
// only so that a lost wake-up (an ESL event dropped during a reconnect) costs
// a minute, not a stuck campaign.
//
// Everything the actor decides is written to the database before it is acted
// on (the attempt row precedes the originate), so killing it at any instant and
// starting another on the same database loses no attempt and dials no contact
// twice: the partial unique index on in-flight numbers is the final arbiter.
package dialer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// ErrStopped is what a command gets when the dialer is shut down before it
// could be carried out.
var ErrStopped = errors.New("dialer: stopped")

// Store is the persistence the actor uses (store.DialerStore).
type Store interface {
	RunningCampaigns(ctx context.Context) ([]campaign.Campaign, error)
	Start(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	Pause(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	Resume(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	Cancel(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	Due(ctx context.Context, campaignID uuid.UUID, now time.Time, frequency time.Duration, limit int) ([]campaign.Contact, error)
	HeldByFrequency(ctx context.Context, campaignID uuid.UUID, now time.Time, frequency time.Duration, limit int) ([]campaign.Held, error)
	Defer(ctx context.Context, ids []uuid.UUID, at []time.Time) error
	RearmUntried(ctx context.Context, campaignID uuid.UUID, now time.Time) error
	Pending(ctx context.Context, campaignID uuid.UUID, now time.Time) (campaign.Pending, error)
	EarliestPending(ctx context.Context, campaignID uuid.UUID) (*campaign.Contact, error)
	Complete(ctx context.Context, campaignID uuid.UUID, at time.Time) (bool, error)
	PauseFor(ctx context.Context, campaignID uuid.UUID, reason campaign.PauseReason) (bool, error)
	DNCContacts(ctx context.Context, limit int) ([]uuid.UUID, error)
	MarkDialingDNC(ctx context.Context) error
	Dialing(ctx context.Context) ([]campaign.DialingAttempt, error)
	RecentOutcomesSince(ctx context.Context, campaignID uuid.UUID, since time.Time, limit int) ([]campaign.Outcome, error)
	Begin(ctx context.Context, contactID uuid.UUID, now time.Time) (campaign.Attempt, error)
	SetJob(ctx context.Context, callID uuid.UUID, jobID string) error
	Finish(ctx context.Context, callID uuid.UUID, e campaign.Ending) (*campaign.Finished, error)
	Void(ctx context.Context, callID uuid.UUID, now time.Time) error
	FinishFromLedger(ctx context.Context, callID uuid.UUID) (bool, error)
}

// Placer places the AI outbound call (outbound.Service).
type Placer interface {
	PlaceAI(ctx context.Context, req outbound.AIDialRequest) (outbound.Placement, error)
}

// Slots is the bot-capacity counter (voice.BotSlots); nil means the bot rule
// is not applied.
type Slots interface {
	Usage() voice.Usage
}

// Lines is the DIDs' outbound line account (outbound.Service); nil means no
// DID is limited.
type Lines interface {
	// DIDLimits lists the DIDs that have an outbound line limit.
	DIDLimits(ctx context.Context) (map[string]int, error)
	// LinesInUse is the lines held on a DID now, by calls placed and observed.
	LinesInUse(did string) int
}

// Clock is the time the dialer runs on. Tests replace it.
type Clock interface {
	Now() time.Time
	// NewTimer returns a timer that fires once after d.
	NewTimer(d time.Duration) Timer
}

// Timer is the part of time.Timer the actor uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer {
	return realTimer{time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// Config is what the deployment decides.
type Config struct {
	// MaxRinging caps AI outbound legs ringing at once (AICC_OUTBOUND_MAX_RINGING).
	MaxRinging int
	// Frequency is the cross-campaign frequency window
	// (AICC_CAMPAIGN_FREQUENCY_WINDOW); zero is off.
	Frequency time.Duration
	// OutboundHours are the outer calling hours (AICC_OUTBOUND_HOURS).
	OutboundHours dayrange.Range
	// TrunkBreakerCount and ProviderBreakerCount are the breakers' thresholds.
	TrunkBreakerCount    int
	ProviderBreakerCount int
	// DialForm is how a number is written to the trunk
	// (AICC_OUTBOUND_DIAL_FORM); empty is E164. DefaultCountryCode
	// (AICC_DEFAULT_COUNTRY_CODE) is the NATIONAL form's fallback when a
	// campaign names none.
	DialForm           phonenum.DialForm
	DefaultCountryCode string
	// InboundReserve is how many bot slots admission keeps free for inbound
	// (config.CampaignInboundReserve). The counter's own reserve
	// (Usage.InboundReserve) applies as well; the larger of the two holds.
	InboundReserve int
	// OverflowTightenRate and OverflowStopRate are overflow control's
	// thresholds (design 10 section 6.5). Defaults 0.01 and 0.03.
	OverflowTightenRate float64
	OverflowStopRate    float64
	// noOverdial plans every ringing leg as an answer, whatever the history says:
	// the conservative rule on its own. Unexported on purpose; tests set it to
	// have the baseline the expected-answer rule is compared with.
	noOverdial bool
	// SafetyScan is the longest the actor sleeps. Default 60s.
	SafetyScan time.Duration
	// BackstopMargin is how long past its ring timeout an attempt may stay
	// without a CDR before it is reconciled. Default 30s.
	BackstopMargin time.Duration
	// DialTimeout bounds one placement. Default 15s.
	DialTimeout time.Duration
	// MaxBatch bounds the contacts fetched at once. Default 200.
	MaxBatch int
}

func (c Config) withDefaults() Config {
	if c.MaxRinging < 1 {
		c.MaxRinging = 100
	}
	if c.TrunkBreakerCount < 1 {
		c.TrunkBreakerCount = 20
	}
	if c.ProviderBreakerCount < 1 {
		c.ProviderBreakerCount = 5
	}
	if c.OverflowTightenRate <= 0 || c.OverflowStopRate <= c.OverflowTightenRate {
		c.OverflowTightenRate, c.OverflowStopRate = 0.01, 0.03
	}
	if c.SafetyScan <= 0 {
		c.SafetyScan = 60 * time.Second
	}
	if c.BackstopMargin <= 0 {
		c.BackstopMargin = 30 * time.Second
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 15 * time.Second
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 200
	}
	return c
}

// Deps are the dialer's collaborators.
type Deps struct {
	Store  Store
	Placer Placer
	Slots  Slots
	Lines  Lines
	// IsLive says whether the registry holds the call (a live call is never
	// reconciled away).
	IsLive func(callID uuid.UUID) bool
	// Reconciled is closed once the switch has been read for the calls that
	// outlived a restart; nothing is admitted or settled before. Nil: ready.
	Reconciled <-chan struct{}
	Clock      Clock
	Log        *slog.Logger
	// OnChange is told that something a campaign's supervisors watch may have
	// moved: its status, what it is doing, its in-flight count or its contact
	// counters. It is a hint, not a fact (the receiver compares), and it must
	// not block. Nil: nobody is told.
	OnChange func(campaignID uuid.UUID)
}

// Dialer is the actor.
type Dialer struct {
	cfg Config
	st  Store
	pl  Placer
	sl  Slots
	ln  Lines
	// isLive and the clock are read by the actor only.
	isLive     func(uuid.UUID) bool
	reconciled <-chan struct{}
	clock      Clock
	log        *slog.Logger
	onChange   func(uuid.UUID)

	// inbox is the mailbox; wake tells the actor it is not empty.
	mu    sync.Mutex
	inbox []any
	wake  chan struct{}

	// view is the published read side: a campaign's activity, atomically
	// replaced, so a request never waits for the actor.
	view atomic.Pointer[map[uuid.UUID]campaign.Progress]

	// Everything below belongs to the actor goroutine.
	isReady  bool
	isDirty  bool
	dncDirty bool
	nextWake time.Time
	rot      int
	flights  map[uuid.UUID]*flight // by call id
	inflight map[uuid.UUID]int     // per campaign
	ringing  int
	talking  int
	// ringingBy is the ringing legs of each campaign: the weights of the
	// expected-answer sum. stats is each campaign's answer-rate and overflow
	// history, loaded from the store on first use and moved by each finish.
	ringingBy map[uuid.UUID]int
	stats     map[uuid.UUID]*campaign.CapacityStats
	// samples are the attempts whose ring has resolved but whose sample has not
	// yet entered the answer-rate window (see CapacityStats).
	samples []rangSample
	// limits is the DIDs' outbound line limits, read once per pass.
	limits   map[string]int
	halted   map[uuid.UUID]bool      // paused by a breaker this pass, not yet re-read
	since    map[uuid.UUID]time.Time // breaker run starts
	rearm    map[uuid.UUID]bool
	warnedCC map[uuid.UUID]bool // campaigns already warned about a national dial fallback
	progress map[uuid.UUID]campaign.Progress
	schedule map[string]campaign.Schedule
	timer    Timer
	replies  []chan struct{}

	// stopped holds the campaigns that admit nothing more for the rest of the
	// current pass, whether or not the pause that was meant to stop them
	// landed. Unlike halted, which only began() clears, it is emptied at the
	// start of every pass: a campaign whose pause failed is still RUNNING, so
	// the next pass must be free to dial it, or to try the pause again.
	stopped map[uuid.UUID]bool
}

// flight is a call in flight: an attempt whose outcome is not written yet.
type flight struct {
	attemptID  uuid.UUID
	contactID  uuid.UUID
	campaignID uuid.UUID
	callID     uuid.UUID
	startedAt  time.Time
	ring       time.Duration
	nextCheck  time.Time
	isAnswered bool
}

// New builds the dialer; Run starts it.
func New(cfg Config, d Deps) *Dialer {
	if d.Clock == nil {
		d.Clock = realClock{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	x := &Dialer{
		cfg: cfg.withDefaults(), st: d.Store, pl: d.Placer, sl: d.Slots, ln: d.Lines,
		isLive: d.IsLive, reconciled: d.Reconciled, clock: d.Clock, log: d.Log, onChange: d.OnChange,
		wake:      make(chan struct{}, 1),
		flights:   map[uuid.UUID]*flight{},
		inflight:  map[uuid.UUID]int{},
		ringingBy: map[uuid.UUID]int{},
		stats:     map[uuid.UUID]*campaign.CapacityStats{},
		halted:    map[uuid.UUID]bool{},
		stopped:   map[uuid.UUID]bool{},
		since:     map[uuid.UUID]time.Time{},
		rearm:     map[uuid.UUID]bool{},
		warnedCC:  map[uuid.UUID]bool{},
		progress:  map[uuid.UUID]campaign.Progress{},
		schedule:  map[string]campaign.Schedule{},
		isDirty:   true, dncDirty: true,
	}
	if x.isLive == nil {
		x.isLive = func(uuid.UUID) bool { return false }
	}
	empty := map[uuid.UUID]campaign.Progress{}
	x.view.Store(&empty)
	return x
}

// Messages. Everything that reaches the actor is one of these.
type (
	finishedMsg campaign.Finished
	answeredMsg uuid.UUID
	wakeMsg     struct {
		dnc      bool
		rearmFor uuid.UUID
	}
	cmdMsg struct {
		fn   func(ctx context.Context)
		done chan struct{}
	}
)

func (d *Dialer) post(m any) {
	d.mu.Lock()
	d.inbox = append(d.inbox, m)
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// AttemptFinished is the store's OnAttemptFinished: an attempt's outcome was
// written and committed. It never blocks.
func (d *Dialer) AttemptFinished(f campaign.Finished) { d.post(finishedMsg(f)) }

// Answered tells the dialer a campaign call's customer picked up (the
// outbound service's answer observer). It never blocks.
func (d *Dialer) Answered(callID uuid.UUID) { d.post(answeredMsg(callID)) }

// DNCAdded is the store's OnDNCAdded: waiting contacts on the number must be
// skipped. It never blocks.
func (d *Dialer) DNCAdded() { d.post(wakeMsg{dnc: true}) }

// SlotFreed is BotSlots.SetOnFree: capacity may have appeared. It never blocks.
func (d *Dialer) SlotFreed() { d.post(wakeMsg{}) }

// Wake asks for a fresh look at everything.
func (d *Dialer) Wake() { d.post(wakeMsg{}) }

// Updated implements campaign.Observer: a PATCH may have moved a calling
// window, so the never-tried contacts that were deferred to the old window's
// opening are judged again.
func (d *Dialer) Updated(before, after campaign.Campaign) {
	d.touch(after.ID)
	if before.Status == campaign.StatusRunning && windowKey(before) != windowKey(after) {
		d.post(wakeMsg{rearmFor: after.ID})
		return
	}
	d.post(wakeMsg{})
}

// Imported implements campaign.Observer: new contacts are due.
func (d *Dialer) Imported(id uuid.UUID) {
	d.touch(id)
	d.post(wakeMsg{})
}

// touch tells the change hook that a campaign may have moved.
func (d *Dialer) touch(id uuid.UUID) {
	if d.onChange != nil {
		d.onChange(id)
	}
}

// Progress implements campaign.ProgressSource.
func (d *Dialer) Progress(id uuid.UUID) (campaign.Progress, bool) {
	p, ok := (*d.view.Load())[id]
	return p, ok
}

// Sync returns once every message posted before the call has been handled and
// the pass it caused has run. For tests and orderly shutdown.
func (d *Dialer) Sync(ctx context.Context) error {
	return d.run(ctx, func(context.Context) {})
}

// run queues fn on the actor and waits until it has run (and the pass it
// belongs to has finished).
func (d *Dialer) run(ctx context.Context, fn func(ctx context.Context)) error {
	done := make(chan struct{})
	d.post(cmdMsg{fn: fn, done: done})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start, Pause, Resume and Cancel make the dialer the campaign.Controller: the
// API asks, the actor decides.
func (d *Dialer) Start(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.move(ctx, func(c context.Context) (campaign.Campaign, error) {
		cam, err := d.st.Start(c, id)
		if err == nil {
			d.began(id)
		}
		return cam, err
	})
}

func (d *Dialer) Pause(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.move(ctx, func(c context.Context) (campaign.Campaign, error) { return d.st.Pause(c, id) })
}

func (d *Dialer) Resume(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.move(ctx, func(c context.Context) (campaign.Campaign, error) {
		cam, err := d.st.Resume(c, id)
		if err == nil {
			d.began(id)
		}
		return cam, err
	})
}

func (d *Dialer) Cancel(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.move(ctx, func(c context.Context) (campaign.Campaign, error) { return d.st.Cancel(c, id) })
}

// began notes that a campaign (re)starts dialing: a breaker's run begins anew
// and a pending breaker pause is forgotten.
func (d *Dialer) began(id uuid.UUID) {
	d.since[id] = d.clock.Now()
	delete(d.halted, id)
	d.dncDirty = true
}

func (d *Dialer) move(ctx context.Context, f func(context.Context) (campaign.Campaign, error)) (campaign.Campaign, error) {
	var (
		cam campaign.Campaign
		err error
	)
	if rerr := d.run(ctx, func(c context.Context) {
		cam, err = f(c)
		d.isDirty = true
		if err == nil {
			d.touch(cam.ID)
		}
	}); rerr != nil {
		return campaign.Campaign{}, rerr
	}
	return cam, err
}

// Run is the actor. It returns when ctx is done.
func (d *Dialer) Run(ctx context.Context) {
	reconciled := d.reconciled
	if reconciled == nil {
		closed := make(chan struct{})
		close(closed)
		reconciled = closed
	}
	defer d.shutdown()
	for {
		now := d.clock.Now()
		for _, m := range d.take() {
			d.handle(ctx, m)
		}
		if !d.isReady {
			select {
			case <-reconciled:
				d.isReady = true
				d.recoverAttempts(ctx)
				d.isDirty = true
			default:
			}
		}
		if d.isReady {
			d.checkBackstops(ctx, now)
			if !now.Before(d.nextWake) || d.isDirty {
				d.pass(ctx)
			}
		}
		d.publish()
		d.release()
		if ctx.Err() != nil {
			return
		}
		if d.isReady && d.isDirty {
			continue // a pass that left work behind runs again at once
		}

		wait := d.cfg.SafetyScan
		if d.isReady && !d.nextWake.IsZero() {
			if w := d.nextWake.Sub(d.clock.Now()); w < wait {
				wait = w
			}
		}
		if d.timer != nil {
			d.timer.Stop()
		}
		d.timer = d.clock.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-d.timer.C():
			d.dncDirty = d.dncDirty || wait >= d.cfg.SafetyScan
			d.isDirty = true
		case <-notReady(d.isReady, reconciled):
		}
	}
}

// notReady is the reconciliation signal while it is awaited, and a channel
// that never fires once it has been seen.
func notReady(isReady bool, c <-chan struct{}) <-chan struct{} {
	if isReady {
		return nil
	}
	return c
}

func (d *Dialer) take() []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.inbox
	d.inbox = nil
	return out
}

// release answers the commands handled in this iteration, after the pass.
func (d *Dialer) release() {
	for _, c := range d.replies {
		close(c)
	}
	d.replies = d.replies[:0]
}

func (d *Dialer) shutdown() {
	if d.timer != nil {
		d.timer.Stop()
	}
	for _, m := range d.take() {
		if c, ok := m.(cmdMsg); ok {
			close(c.done)
		}
	}
	d.release()
}

func (d *Dialer) handle(ctx context.Context, m any) {
	switch m := m.(type) {
	case cmdMsg:
		m.fn(ctx)
		d.replies = append(d.replies, m.done)
	case finishedMsg:
		d.onFinished(ctx, campaign.Finished(m))
	case answeredMsg:
		if f := d.flights[uuid.UUID(m)]; f != nil && !f.isAnswered {
			f.isAnswered = true
			d.statsFor(ctx, f.campaignID).Answered(f.callID)
			d.sample(f, true, d.clock.Now())
			d.ringing--
			d.ringBy(f.campaignID, -1)
			d.talking++
			obsInFlight(false, -1)
			obsInFlight(true, 1)
			d.isDirty = true // a leg that stopped ringing frees a place in the ringing cap
		}
	case wakeMsg:
		d.isDirty = true
		if m.dnc {
			d.dncDirty = true
		}
		if m.rearmFor != uuid.Nil {
			d.rearm[m.rearmFor] = true
		}
	}
}
