// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// stallingSwitch is a switch that reads the originate and does not answer it
// in time: the ESL client gives up with esl.ErrTimeout while the command sits
// on the socket (#221). Every originate is recorded, so a test can count what
// reached the switch.
type stallingSwitch struct {
	*fakeSwitch
	// jitter, when set, is how long an originate may take, at random: the
	// interleavings of concurrent requests.
	jitter time.Duration
	mu     sync.Mutex
	rnd    *rand.Rand
}

func (s *stallingSwitch) Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error) {
	_, _ = s.fakeSwitch.Originate(partyID, endpoint, vars)
	if s.jitter > 0 {
		s.mu.Lock()
		d := time.Duration(s.rnd.Int63n(int64(s.jitter)))
		s.mu.Unlock()
		time.Sleep(d)
	}
	return "", fmt.Errorf("%w: bgapi originate {origination_uuid=%s}%s &park()", esl.ErrTimeout, partyID, endpoint)
}

func (s *stallingSwitch) originateCount() int {
	s.fakeSwitch.mu.Lock()
	defer s.fakeSwitch.mu.Unlock()
	return len(s.fakeSwitch.originates)
}

func (s *stallingSwitch) originateList() []originated {
	s.fakeSwitch.mu.Lock()
	defer s.fakeSwitch.mu.Unlock()
	return append([]originated(nil), s.fakeSwitch.originates...)
}

// stallClock is the service's clock under the test's control.
type stallClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stallClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stallClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// liveSet is the registry's answer to "is this call live", under the test's
// control: a leg's CHANNEL_CREATE makes its call live there.
type liveSet struct {
	mu  sync.Mutex
	ids map[uuid.UUID]bool
}

func (l *liveSet) set(id uuid.UUID, isLive bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ids[id] = isLive
}

func (l *liveSet) has(id uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ids[id]
}

type stalled struct {
	s      *Service
	sw     *stallingSwitch
	ledger *recordingLedger
	live   *liveSet
	clock  *stallClock
}

// stalledService is a service in front of a stalled switch. The ledger it
// writes placement failures to is also its idempotency record, as in
// production (the CDR table is both).
func stalledService(t *testing.T, window, margin time.Duration, dids fakeDIDs) stalled {
	t.Helper()
	sw := &stallingSwitch{fakeSwitch: &fakeSwitch{}, rnd: rand.New(rand.NewSource(221))}
	ledger := &recordingLedger{cdrs: map[uuid.UUID][]store.CDR{}}
	live := &liveSet{ids: map[uuid.UUID]bool{}}
	clock := &stallClock{now: time.Date(2026, 10, 21, 9, 51, 0, 0, time.UTC)}
	if dids == nil {
		flowID := uuid.Must(uuid.NewV7())
		dids = fakeDIDs{
			{Number: "95099", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, IsRecordingEnabled: true},
			{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
		}
	}
	s := New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s", AnswerWindow: window, RingTimeout: window / 2},
		sw, dids, provisioned("1007", "1008"),
		func(_ context.Context, id uuid.UUID) (bool, error) { return len(ledger.get(id)) > 0, nil },
		live.has, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.limiter = NewLimiter(1_000_000)
	s.jobMargin = margin
	s.now = clock.Now
	s.SetFailureLedger(ledger)
	return stalled{s: s, sw: sw, ledger: ledger, live: live, clock: clock}
}

func (st stalled) create(channelID string, callID uuid.UUID) {
	st.live.set(callID, true)
	st.s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: channelID})
}

// The incident, end to end at the service: the originate times out, the API is
// told the placement is unconfirmed (not that the switch did not take it), a
// retry with the same callId does not dial again, and the switch's channel,
// created late, is adopted: answered, it is bridged to the bot, and no
// placement-failure row is written beside the call's own.
func TestAnUnconfirmedAICallIsAdoptedWhenItsChannelAppears(t *testing.T) {
	t.Parallel()
	st := stalledService(t, time.Minute, time.Minute, nil)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	req := AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099",
		UserData: map[string]string{"ticket": "T-221"}}

	p, err := s.PlaceAI(context.Background(), req)
	if !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v, want ErrPlacementUnconfirmed", err)
	}
	if !errors.Is(err, esl.ErrTimeout) {
		t.Errorf("the switch's error is lost: %v", err)
	}
	if p.CallID != callID {
		t.Fatalf("CallID = %s, want %s: the client needs the id to retry or look for", p.CallID, callID)
	}
	// Everything a placed call holds is still held.
	if n := s.LinesInUse("95099"); n != 1 {
		t.Errorf("lines in use = %d, want the line still held", n)
	}
	if data := s.Data().CallData(callID); data["ticket"] != "T-221" {
		t.Errorf("business data = %v, want it kept for the call", data)
	}
	if !s.IsPlacing(callID) || s.PendingCount() != 1 {
		t.Errorf("placing = %v, pending = %d; want the placement tracked and its answer armed",
			s.IsPlacing(callID), s.PendingCount())
	}

	// A retry with the same callId, during the stall: no second originate.
	for range 3 {
		p, err := s.PlaceAI(context.Background(), req)
		if !errors.Is(err, ErrAlreadyPlaced) || p.CallID != callID {
			t.Fatalf("retry = %+v, %v; want ErrAlreadyPlaced naming the call", p, err)
		}
	}
	if n := st.sw.originateCount(); n != 1 {
		t.Fatalf("%d originates reached the switch, want 1", n)
	}

	// The switch catches up 50 s later. Its leg rings from now, so it may be
	// answered after the window armed at the timeout would have closed.
	leg := st.sw.originateList()[0].partyID.String()
	st.clock.Advance(50 * time.Second)
	st.create(leg, callID)
	st.clock.Advance(30 * time.Second)
	answer(s, leg)
	waitBridges(t, st.sw.fakeSwitch, 1)
	b := st.sw.lastBridge()
	if b.channelID != leg || b.vars["sip_h_X-AICC-Call-ID"] != callID.String() {
		t.Errorf("bridged %s with %v, want the late leg bridged to the bot for this call", b.channelID, b.vars)
	}
	if h := st.sw.hangupList(); len(h) != 0 {
		t.Errorf("hangups = %v: the adopted leg was treated as late", h)
	}

	// The job's result, matched by the leg its command named.
	s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindBackgroundJob,
		JobID: "job-late", JobChannelID: leg, IsJobOK: true})
	if s.IsPlacing(callID) {
		t.Error("the placement is still tracked after the switch confirmed it")
	}
	// handleJob settles an OK result synchronously, and a failure path would
	// have given the line back before returning: both are decided by now.
	if n := s.LinesInUse("95099"); n != 1 {
		t.Errorf("lines in use = %d: the placement was failed although its call exists", n)
	}
	if rows := st.ledger.get(callID); len(rows) != 0 {
		t.Errorf("a placement-failure row was written for a call that exists: %+v", rows)
	}
}

// The channel may appear at any point of the tracking window, also after the
// answer window that would have run from the timeout: what was armed for its
// answer is still there (a sweep of expired legs does not take it), and runs.
func TestAnUnconfirmedLegCreatedLateInTheWindowIsStillBridged(t *testing.T) {
	t.Parallel()
	st := stalledService(t, time.Minute, 30*time.Second, nil)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()
	st.clock.Advance(80 * time.Second)
	if n := s.PendingCount(); n != 1 {
		t.Fatalf("pending = %d at 80 s, want the continuation kept for the whole window", n)
	}
	st.create(leg, callID)
	st.clock.Advance(45 * time.Second)
	if n := s.PendingCount(); n != 1 {
		t.Fatalf("pending = %d 45 s after the channel appeared, want its answer window running", n)
	}
	answer(s, leg)
	waitBridges(t, st.sw.fakeSwitch, 1)
	if h := st.sw.hangupList(); len(h) != 0 {
		t.Errorf("hangups = %v", h)
	}
}

// The window passes with no channel: exactly one placement-failure CDR, with
// the platform's own cause, the line and the business data are given back,
// and a retry is still answered as placed. A leg the switch creates after
// that is hung up, not left parked on whoever picks up.
func TestAnUnconfirmedPlacementThatNeverAppearsIsRecordedAsFailed(t *testing.T) {
	t.Parallel()
	st := stalledService(t, 40*time.Millisecond, time.Millisecond, nil)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	req := AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099",
		UserData: map[string]string{"ticket": "T-221"}}

	if _, err := s.PlaceAI(context.Background(), req); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()
	eventually(t, "the placement-failure cdr", func() bool { return len(st.ledger.get(callID)) == 1 })
	eventually(t, "the claim to be given back", func() bool { return !s.IsPlacing(callID) })

	cdr := st.ledger.get(callID)[0]
	if cdr.Status != store.CDRStatusFailed || cdr.HangupCause != UnconfirmedCause ||
		cdr.Tech["isPlacementFailure"] != true || cdr.Tech["originatedChannelId"] != leg ||
		cdr.ToNumber != "18688886660" || cdr.DID != "95099" || cdr.UserData["ticket"] != "T-221" {
		t.Errorf("cdr = %+v", cdr)
	}
	if n := s.LinesInUse("95099"); n != 0 {
		t.Errorf("lines in use = %d, want the line given back", n)
	}
	if data := s.Data().CallData(callID); data != nil {
		t.Errorf("business data leaked: %v", data)
	}
	if n := s.PendingCount(); n != 0 {
		t.Errorf("%d continuations still armed", n)
	}

	// A retry after the window: the ledger answers.
	if _, err := s.PlaceAI(context.Background(), req); !errors.Is(err, ErrAlreadyPlaced) {
		t.Errorf("retry after the window = %v, want ErrAlreadyPlaced", err)
	}
	if n := st.sw.originateCount(); n != 1 {
		t.Errorf("%d originates, want 1", n)
	}

	// The switch runs the command even later than that.
	s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: leg})
	eventually(t, "the late leg to be hung up", func() bool { return len(st.sw.hangupList()) == 1 })
	if h := st.sw.hangupList()[0]; h.channelID != leg || h.cause != LateAnswerCause {
		t.Errorf("hangup = %+v, want the late leg ended as %s", h, LateAnswerCause)
	}
	// Once only: a second sighting of the same leg is not a second decision.
	// noteChannel decides synchronously whether to hang up, and only then
	// starts the hangup.
	s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: leg})
	if n := len(st.sw.hangupList()); n != 1 {
		t.Errorf("%d hangups, want 1", n)
	}
	if n := len(st.ledger.get(callID)); n != 1 {
		t.Errorf("%d placement-failure rows, want exactly 1", n)
	}
}

// The reply that timed out arrives late and names the job, and the job then
// fails before any channel: that is the switch's own answer, recorded at once
// with its cause, rather than waiting out the window.
func TestALateJobFailureFinishesAnUnconfirmedPlacementWithItsCause(t *testing.T) {
	t.Parallel()
	st := stalledService(t, time.Minute, time.Minute, nil)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()

	// A result for some other leg is not this placement's.
	s.HandleSwitchEvent(jobEventFor("job-x", uuid.NewString(), "-ERR GATEWAY_DOWN\n"))
	if !s.IsPlacing(callID) {
		t.Fatal("another leg's result finished this placement")
	}
	s.HandleSwitchEvent(jobEventFor("job-late", leg, "-ERR GATEWAY_DOWN\n"))
	eventually(t, "the placement-failure cdr", func() bool { return len(st.ledger.get(callID)) == 1 })
	if cdr := st.ledger.get(callID)[0]; cdr.HangupCause != "GATEWAY_DOWN" || cdr.Tech["originateJobId"] != "job-late" {
		t.Errorf("cdr cause %q, job %v; want the switch's cause and job", cdr.HangupCause, cdr.Tech["originateJobId"])
	}
	eventually(t, "the claim to be given back", func() bool { return !s.IsPlacing(callID) })
	if n := s.LinesInUse("95099"); n != 0 {
		t.Errorf("lines in use = %d", n)
	}
}

func jobEventFor(jobID, channelID, body string) telephony.SwitchEvent {
	ev := jobEvent(jobID, body)
	ev.JobChannelID = channelID
	return ev
}

// Click-to-dial under the same stall (seen live on 2026-10-19): the agent's
// phone auto-answers the late leg, and that leg must go on to the destination
// rather than sit in park() with the agent on a silent call. The agent's phone
// is raised once however often the client retries.
func TestAnUnconfirmedClickToDialGoesOnToTheDestinationWhenTheAgentLegAnswers(t *testing.T) {
	t.Parallel()
	st := stalledService(t, time.Minute, time.Minute, nil)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	req := AgentDialRequest{CallID: callID, AgentExtension: "1008", To: "18688886666"}

	got, err := s.Dial(context.Background(), req)
	if !errors.Is(err, ErrPlacementUnconfirmed) || got != callID {
		t.Fatalf("Dial = %s, %v; want the callId and ErrPlacementUnconfirmed", got, err)
	}
	if n := s.LinesInUse("95011"); n != 1 {
		t.Errorf("lines in use = %d, want the line still held", n)
	}
	for range 3 {
		if got, err := s.Dial(context.Background(), req); !errors.Is(err, ErrAlreadyPlaced) || got != callID {
			t.Fatalf("retry = %s, %v; want ErrAlreadyPlaced", got, err)
		}
	}
	if n := st.sw.originateCount(); n != 1 {
		t.Fatalf("the agent's phone was raised %d times, want once", n)
	}

	leg := st.sw.originateList()[0].partyID.String()
	st.clock.Advance(20 * time.Second)
	st.create(leg, callID)
	answer(s, leg)
	waitTransfers(t, st.sw.fakeSwitch, 1)
	if tr := st.sw.lastTransfer(); tr.channelID != leg || tr.extension != "18688886666" || tr.context != "aicc" {
		t.Errorf("transfer = %+v, want the agent leg sent on to the destination", tr)
	}
}

// Pre-send refusals still win under a stall: a number on the do-not-call list,
// a malformed one, and a DID at its line limit are refused as before, reach no
// switch and are never reported as unconfirmed. And 200 requests across 50
// callIds, retried concurrently with random delays while the switch is
// stalled, put at most one originate per callId on the switch.
func TestConcurrentRetriesDuringAStallDialEachCallIDAtMostOnce(t *testing.T) {
	t.Parallel()
	flowID := uuid.Must(uuid.NewV7())
	zero := 0
	dids := fakeDIDs{
		{Number: "95099", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true},
		{Number: "95098", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, MaxOutboundChannels: &zero},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
	}
	st := stalledService(t, time.Minute, time.Minute, dids)
	st.sw.jitter = 3 * time.Millisecond
	st.s.WithDNC(&lockedDNC{listed: map[string]bool{"+8613800138000": true}})
	st.s.cfg.DefaultCountryCode = "86"

	type kind int
	const (
		normal kind = iota
		onDNC
		malformed
		lineLimit
	)
	kindOf := func(i int) kind {
		switch i % 10 {
		case 0:
			return onDNC
		case 1:
			return malformed
		case 2:
			return lineLimit
		}
		return normal
	}
	const callIDs, perCallID = 50, 4
	ids := make([]uuid.UUID, callIDs)
	reqFor := func(i int) AIDialRequest {
		r := AIDialRequest{CallID: ids[i], To: fmt.Sprintf("1868888%04d", i), DIDNumber: "95099"}
		switch kindOf(i) {
		case onDNC:
			r.To = "13800138000"
		case malformed:
			r.To = "1868888-abc"
		case lineLimit:
			r.DIDNumber = "95098"
		}
		return r
	}
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
	}

	type answer struct {
		i   int
		err error
	}
	answers := make(chan answer, callIDs*perCallID)
	var wg sync.WaitGroup
	rnd := rand.New(rand.NewSource(2210))
	for n := range callIDs * perCallID {
		i := n % callIDs
		delay := time.Duration(rnd.Int63n(int64(5 * time.Millisecond)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(delay)
			_, err := st.s.PlaceAI(context.Background(), reqFor(i))
			answers <- answer{i, err}
		}()
	}
	wg.Wait()
	close(answers)

	unconfirmed := map[int]int{}
	for a := range answers {
		switch kindOf(a.i) {
		case onDNC:
			if !errors.Is(a.err, ErrNumberOnDNC) {
				t.Errorf("callId %d (do-not-call): %v, want ErrNumberOnDNC every time", a.i, a.err)
			}
		case malformed:
			if !errors.Is(a.err, ErrBadNumber) {
				t.Errorf("callId %d (malformed): %v, want ErrBadNumber every time", a.i, a.err)
			}
		case lineLimit:
			// A concurrent duplicate of a refused request may find the first
			// still holding the callId; neither answer is a placement.
			if !errors.Is(a.err, ErrDIDChannelsExhausted) && !errors.Is(a.err, ErrAlreadyPlaced) {
				t.Errorf("callId %d (line limit): %v", a.i, a.err)
			}
		default:
			switch {
			case errors.Is(a.err, ErrPlacementUnconfirmed):
				unconfirmed[a.i]++
			case errors.Is(a.err, ErrAlreadyPlaced):
			default:
				t.Errorf("callId %d: %v, want unconfirmed or already placed", a.i, a.err)
			}
		}
	}

	perLeg := map[string]int{}
	for _, o := range st.sw.originateList() {
		perLeg[o.vars["aicc_call_id"]]++
	}
	wantPlaced := 0
	for i, id := range ids {
		n := perLeg[id.String()]
		if kindOf(i) != normal {
			if n != 0 {
				t.Errorf("callId %d (refused) reached the switch %d times", i, n)
			}
			continue
		}
		wantPlaced++
		if n != 1 || unconfirmed[i] != 1 {
			t.Errorf("callId %d: %d originates, %d unconfirmed answers; want exactly one of each", i, n, unconfirmed[i])
		}
		if !st.s.IsPlacing(id) {
			t.Errorf("callId %d is not tracked", i)
		}
	}
	if got := st.sw.originateCount(); got != wantPlaced {
		t.Errorf("%d originates, want %d", got, wantPlaced)
	}
}

// lockedDNC is a do-not-call list safe for concurrent requests.
type lockedDNC struct {
	mu     sync.Mutex
	listed map[string]bool
}

func (d *lockedDNC) Contains(_ context.Context, number string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listed[number], nil
}

// A real ESL error, or anything else that says nothing was placed, still
// undoes the placement at once: only a timeout is ambiguous.
func TestOnlyATimeoutIsUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, sendErr := range []error{esl.ErrDown, errors.New("write command: broken pipe")} {
		s, _, _ := jobService(t)
		s.sw = failingSwitch{Switch: s.sw, err: sendErr}
		callID := uuid.Must(uuid.NewV7())
		_, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886666", DIDNumber: "95012"})
		if !errors.Is(err, sendErr) || errors.Is(err, ErrPlacementUnconfirmed) {
			t.Errorf("%v: PlaceAI error = %v", sendErr, err)
		}
		if s.IsPlacing(callID) || s.LinesInUse("95012") != 0 || s.PendingCount() != 0 {
			t.Errorf("%v: placement left behind (placing %v, lines %d, pending %d)",
				sendErr, s.IsPlacing(callID), s.LinesInUse("95012"), s.PendingCount())
		}
	}
}

type failingSwitch struct {
	Switch
	err error
}

func (f failingSwitch) Originate(uuid.UUID, string, map[string]string) (string, error) {
	return "", f.err
}

// manualTimers replaces the placement windows' timers: a test fires one when it
// chooses, on its own goroutine, instead of waiting out a real window.
type manualTimers struct {
	mu     sync.Mutex
	timers []*manualTimer
}

type manualTimer struct {
	mu        sync.Mutex
	f         func()
	isStopped bool
}

func (t *manualTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := !t.isStopped
	t.isStopped = true
	return was
}

func (m *manualTimers) after(_ time.Duration, f func()) stopper {
	t := &manualTimer{f: f}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timers = append(m.timers, t)
	return t
}

// fire runs the last timer started, as its expiry would, unless it was stopped.
func (m *manualTimers) fire(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	if len(m.timers) == 0 {
		m.mu.Unlock()
		t.Fatal("no timer was started")
	}
	tm := m.timers[len(m.timers)-1]
	m.mu.Unlock()
	tm.mu.Lock()
	isStopped := tm.isStopped
	tm.isStopped = true
	tm.mu.Unlock()
	if !isStopped {
		tm.f()
	}
}

func manualService(t *testing.T) (stalled, *manualTimers) {
	t.Helper()
	st := stalledService(t, time.Minute, time.Minute, nil)
	timers := &manualTimers{}
	st.s.timers = timers.after
	return st, timers
}

// The window passes after the channel was noted: the call is the record. The
// claim is given back, and nothing of the failure path runs: the line stays
// held, the answer stays armed and no row is written.
func TestAnUnconfirmedWindowThatEndsAfterTheChannelLeavesTheCallAlone(t *testing.T) {
	t.Parallel()
	st, timers := manualService(t)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()
	st.create(leg, callID)

	timers.fire(t)
	if s.IsPlacing(callID) {
		t.Error("the claim outlived the window")
	}
	if n := s.LinesInUse("95099"); n != 1 {
		t.Errorf("lines in use = %d, want the call's line still held", n)
	}
	if n := s.PendingCount(); n != 1 {
		t.Errorf("pending = %d, want the answer still armed", n)
	}
	if rows := st.ledger.get(callID); len(rows) != 0 {
		t.Errorf("rows = %+v, want none: the failure path ran", rows)
	}
	answer(s, leg)
	waitBridges(t, st.sw.fakeSwitch, 1)
}

// The dispatch loop hands CHANNEL_CREATE to the registry before this service.
// A window that ends in between finds the call live but the leg not yet
// noted: it must adopt it, with a fresh answer window, and the CHANNEL_CREATE
// that follows must not hang the leg up as late.
func TestAnUnconfirmedWindowEndingBetweenTheRegistryAndNoteChannelAdoptsTheLeg(t *testing.T) {
	t.Parallel()
	st, timers := manualService(t)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()

	st.clock.Advance(119 * time.Second)                                                           // the armed answer would lapse at 120 s
	st.live.set(callID, true)                                                                     // the registry has the call ...
	timers.fire(t)                                                                                // ... the window ends ...
	s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: leg}) // ... then noteChannel

	if s.IsPlacing(callID) {
		t.Error("the claim outlived the window")
	}
	st.clock.Advance(30 * time.Second)
	answer(s, leg)
	waitBridges(t, st.sw.fakeSwitch, 1)
	if h := st.sw.hangupList(); len(h) != 0 {
		t.Errorf("hangups = %v: a leg whose call was live was taken for a late one", h)
	}
	if rows := st.ledger.get(callID); len(rows) != 0 || s.LinesInUse("95099") != 1 {
		t.Errorf("rows %+v, lines %d: the failure path ran for a live call", rows, s.LinesInUse("95099"))
	}
}

// A leg the switch created after its placement was recorded as failed, while
// the link was down, is found by the reconcile that follows the reconnect. It
// is hung up as late, never armed again: the call is recorded as failed and a
// campaign may already have scheduled the contact again.
func TestRecoveryEndsALateLegItWouldOtherwiseRearm(t *testing.T) {
	t.Parallel()
	st, timers := manualService(t)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()
	timers.fire(t) // no channel: the placement fails
	eventually(t, "the placement-failure cdr", func() bool { return len(st.ledger.get(callID)) == 1 })

	rep := s.Recover(context.Background(), []telephony.LiveChannel{{
		ChannelID: leg, CallID: callID, PlacedBy: telephony.PlacedByDialAI,
		PlacedTo: "18688886660", DID: "95099", Language: "en",
		Age: 5 * time.Second, IsAgeKnown: true,
	}}, nil, false)

	if rep.Rearmed != 0 || rep.WindowSpent != 1 {
		t.Errorf("report = %+v, want the leg counted as window spent and not re-armed", rep)
	}
	if n := s.PendingCount(); n != 0 {
		t.Errorf("pending = %d: the late leg was armed again", n)
	}
	if h := st.sw.hangupList(); len(h) != 1 || h[0].channelID != leg || h[0].cause != LateAnswerCause {
		t.Errorf("hangups = %+v, want the late leg ended as %s", h, LateAnswerCause)
	}
}

// gatedDNC holds the first lookup until the test lets it go.
type gatedDNC struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (d *gatedDNC) Contains(context.Context, string) (bool, error) {
	d.mu.Lock()
	d.calls++
	first := d.calls == 1
	d.mu.Unlock()
	if first {
		close(d.entered)
		<-d.release
	}
	return false, nil
}

// Request B passes the idempotency check, then waits in the do-not-call
// lookup. Meanwhile request A, same callId, places the call, the switch fails
// it before any channel, its CDR is written and the claim is given back. B's
// claim must then find the CDR and answer already placed, not dial again.
func TestARetryThatRacesASettledPlacementDoesNotDialAgain(t *testing.T) {
	t.Parallel()
	s, sw, ledger := jobService(t)
	s.hasCDR = func(_ context.Context, id uuid.UUID) (bool, error) { return len(ledger.get(id)) > 0, nil }
	dnc := &gatedDNC{entered: make(chan struct{}), release: make(chan struct{})}
	s.WithDNC(dnc)
	sw.onSend = func(jobID string) { s.HandleSwitchEvent(jobEvent(jobID, "-ERR GATEWAY_DOWN\n")) }

	callID := uuid.Must(uuid.NewV7())
	req := AIDialRequest{CallID: callID, To: "+8618688886666", DIDNumber: "95012"}
	errB := make(chan error, 1)
	go func() {
		_, err := s.PlaceAI(context.Background(), req)
		errB <- err
	}()
	<-dnc.entered

	if _, err := s.PlaceAI(context.Background(), req); err != nil {
		t.Fatalf("A: %v", err)
	}
	eventually(t, "A's failure cdr", func() bool { return len(ledger.get(callID)) == 1 })
	eventually(t, "A's claim to be given back", func() bool { return !s.IsPlacing(callID) })

	close(dnc.release)
	if err := <-errB; !errors.Is(err, ErrAlreadyPlaced) {
		t.Errorf("B = %v, want ErrAlreadyPlaced", err)
	}
	sw.fakeSwitch.mu.Lock()
	n := len(sw.fakeSwitch.originates)
	sw.fakeSwitch.mu.Unlock()
	if n != 1 {
		t.Errorf("%d originates for one callId, want 1", n)
	}
}
