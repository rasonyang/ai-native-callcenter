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

// hookedStall is a stalled switch that runs a hook with the leg's id once the
// originate has reached it and before it gives up on the reply.
type hookedStall struct {
	*stallingSwitch
	onSend func(legID string)
}

func (h *hookedStall) Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error) {
	_, err := h.stallingSwitch.Originate(partyID, endpoint, vars)
	if h.onSend != nil {
		h.onSend(partyID.String())
	}
	return "", err
}

// fireAll runs every window timer started and not stopped, as their expiry
// would.
func (m *manualTimers) fireAll() {
	m.mu.Lock()
	timers := append([]*manualTimer(nil), m.timers...)
	m.mu.Unlock()
	for _, tm := range timers {
		tm.mu.Lock()
		isStopped := tm.isStopped
		tm.isStopped = true
		tm.mu.Unlock()
		if !isStopped {
			tm.f()
		}
	}
}

// The switch's result for an originate whose reply timed out can be
// dispatched at any moment around the timeout: before the originate returns,
// between its return and the placement being held as unconfirmed, or after.
// In all three the switch's own cause is recorded, at once (no window has
// ended), in exactly one placement-failure row (#302).
func TestAJobResultRacingTheHoldKeepsTheSwitchsCause(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"AI_OUTBOUND", "click-to-dial"} {
		for _, at := range []string{"before the originate returns", "between the return and the hold", "after the hold"} {
			t.Run(kind+"/"+at, func(t *testing.T) {
				t.Parallel()
				st, timers := manualService(t)
				s := st.s
				sw := &hookedStall{stallingSwitch: st.sw}
				s.sw = sw
				fail := func(leg string) {
					s.HandleSwitchEvent(jobEventFor("job-302", leg, "-ERR GATEWAY_DOWN\n"))
				}
				switch at {
				case "before the originate returns":
					sw.onSend = fail
				case "between the return and the hold":
					s.beforeHold = fail
				}

				callID := uuid.Must(uuid.NewV7())
				did := "95099"
				var err error
				if kind == "AI_OUTBOUND" {
					_, err = s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: did,
						UserData: map[string]string{"ticket": "T-302"}})
				} else {
					did = "95011"
					_, err = s.Dial(context.Background(), AgentDialRequest{CallID: callID, AgentExtension: "1008", To: "18688886666"})
				}
				if !errors.Is(err, ErrPlacementUnconfirmed) {
					t.Fatalf("placement error = %v, want unconfirmed", err)
				}
				leg := st.sw.originateList()[0].partyID.String()
				if at == "after the hold" {
					fail(leg)
				}

				// No window has ended: the timers are the test's, and none
				// has fired.
				eventually(t, "the placement-failure cdr", func() bool { return len(st.ledger.get(callID)) == 1 })
				cdr := st.ledger.get(callID)[0]
				if cdr.HangupCause != "GATEWAY_DOWN" || cdr.Tech["originateJobId"] != "job-302" ||
					cdr.Tech["originatedChannelId"] != leg || cdr.Status != store.CDRStatusFailed {
					t.Errorf("cdr = %+v, want the switch's cause and job", cdr)
				}
				eventually(t, "the claim to be given back", func() bool { return !s.IsPlacing(callID) })
				if n := s.LinesInUse(did); n != 0 {
					t.Errorf("lines in use = %d", n)
				}
				if n := s.PendingCount(); n != 0 {
					t.Errorf("%d continuations still armed", n)
				}
				if data := s.Data().CallData(callID); data != nil {
					t.Errorf("business data kept: %v", data)
				}

				// The window ending later changes nothing: one row.
				timers.fireAll()
				if n := len(st.ledger.get(callID)); n != 1 {
					t.Errorf("%d rows, want exactly 1", n)
				}
			})
		}
	}
}

// A result matched by its leg before the originate returned successfully is
// applied once, when the reply's job is attached: a failure finishes the
// placement and drops what was armed, a success leaves the call armed for its
// answer. Neither leaves the job tracked for a second result or a backstop.
func TestAJobResultBeforeASuccessfulReplyIsAppliedOnce(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		body       string
		wantRows   int
		wantLines  int
		wantArmed  int
		wantCauses string
	}{
		{body: "-ERR USER_BUSY\n", wantRows: 1, wantCauses: "USER_BUSY"},
		{body: "+OK 0d4e\n", wantLines: 1, wantArmed: 1},
	} {
		t.Run(c.body, func(t *testing.T) {
			t.Parallel()
			s, sw, ledger := jobService(t)
			sw.onSend = func(jobID string) {
				s.HandleSwitchEvent(jobEventFor(jobID, sw.legs[jobID], c.body))
			}
			callID, err := s.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95012"})
			if err != nil {
				t.Fatal(err)
			}
			if c.wantRows > 0 {
				eventually(t, "the placement-failure cdr", func() bool { return len(ledger.get(callID)) == c.wantRows })
				if cause := ledger.get(callID)[0].HangupCause; cause != c.wantCauses {
					t.Errorf("cause = %q, want %q", cause, c.wantCauses)
				}
			}
			eventually(t, "the claim to be given back", func() bool { return !s.IsPlacing(callID) })
			if n := s.JobCount(); n != 0 {
				t.Errorf("%d jobs still tracked: the result would be applied again", n)
			}
			if n := len(ledger.get(callID)); n != c.wantRows {
				t.Errorf("%d rows, want %d", n, c.wantRows)
			}
			if n := s.LinesInUse("95012"); n != c.wantLines {
				t.Errorf("lines in use = %d, want %d", n, c.wantLines)
			}
			if n := s.PendingCount(); n != c.wantArmed {
				t.Errorf("armed = %d, want %d", n, c.wantArmed)
			}
		})
	}
}

// The outcomes a mixed switch gives one originate: what the ESL client
// reports for it (a reply, ErrTimeout for a reply that never came, a link
// that closed after the write or a write cut off, ErrDown for a command that
// was never written), then what the switch does with it.
type mixOutcome int

const (
	mixAnswered    mixOutcome = iota // replies; the leg rings and is answered
	mixFailed                        // replies; the job fails before any channel
	mixLateAdopted                   // no reply; the leg comes up and is answered within the window
	mixLateFailed                    // no reply; the job fails before any channel
	mixSilent                        // no reply, nothing in the window; the leg turns up after it
	mixNotSent                       // never written: the link was closed before the write
	mixOutcomes
)

// mixRace is when a job result is dispatched relative to the placement.
type mixRace int

const (
	raceBeforeReturn mixRace = iota
	raceBeforeHold
	raceAfter
	mixRaces
)

type mixPlan struct {
	// outcomes are the first attempt's and every later one's. Only the first
	// can be mixNotSent.
	first, later mixOutcome
	race         mixRace
	// Set by the switch when an originate reaches it.
	attempts int
	sent     mixOutcome
	leg      string
	jobID    string
	reached  chan struct{} // closed when the originate reaches the switch
	returned chan struct{} // closed when the placement that reached it returned
	once     sync.Once
}

// mixSwitch answers each originate as its callId's plan says.
type mixSwitch struct {
	*fakeSwitch
	s *Service

	mu    sync.Mutex
	plans map[string]*mixPlan // by callId
	n     int
}

func (m *mixSwitch) Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error) {
	m.mu.Lock()
	p := m.plans[vars["aicc_call_id"]]
	outcome := p.later
	if p.attempts == 0 {
		outcome = p.first
	}
	p.attempts++
	if outcome == mixNotSent {
		m.mu.Unlock()
		return "", fmt.Errorf("%w: write command: use of closed network connection", esl.ErrDown)
	}
	m.n++
	p.sent, p.leg, p.jobID = outcome, partyID.String(), fmt.Sprintf("job-%04d", m.n)
	m.mu.Unlock()
	_, _ = m.fakeSwitch.Originate(partyID, endpoint, vars)
	close(p.reached)

	if p.race == raceBeforeReturn {
		m.dispatchResult(p)
	}
	switch outcome {
	case mixAnswered, mixFailed:
		return p.jobID, nil
	}
	return "", fmt.Errorf("%w: bgapi originate {origination_uuid=%s}", esl.ErrTimeout, partyID)
}

// dispatchResult sends the job's failure, for the outcomes that have one.
func (m *mixSwitch) dispatchResult(p *mixPlan) {
	switch p.sent {
	case mixFailed:
		m.s.HandleSwitchEvent(jobEventFor(p.jobID, p.leg, "-ERR USER_BUSY\n"))
	case mixLateFailed:
		m.s.HandleSwitchEvent(jobEventFor(p.jobID, p.leg, "-ERR GATEWAY_DOWN\n"))
	}
}

func (m *mixSwitch) planOfLeg(leg string) *mixPlan {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plans {
		if p.leg == leg {
			return p
		}
	}
	return nil
}

// 300 placements against a switch that, seeded, replies, times out, closes
// the link after the write or before it, with do-not-call, malformed and
// line-limited numbers mixed in and same-callId retries at random offsets,
// while the switch's results race the placements (#302). The ESL behaviours
// are those the client reports (internal/esl/ambiguity_test.go); here they
// meet the service. At most one originate per callId reaches the switch,
// every answer is one the API maps to 201, 202, 200 isDuplicate or a refusal
// before sending, each placed call ends with exactly one record of the right
// kind and cause, and no leg is left parked.
func TestAMessyMixOfSwitchBehavioursPlacesEachCallOnceAndRecordsItOnce(t *testing.T) {
	t.Parallel()
	const (
		callIDs = 300
		window  = 3 * time.Second
	)
	flowID := uuid.Must(uuid.NewV7())
	zero := 0
	dids := fakeDIDs{
		{Number: "95099", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true},
		{Number: "95098", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, MaxOutboundChannels: &zero},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
	}
	ledger := &recordingLedger{cdrs: map[uuid.UUID][]store.CDR{}}
	live := &liveSet{ids: map[uuid.UUID]bool{}}
	sw := &mixSwitch{fakeSwitch: &fakeSwitch{}, plans: map[string]*mixPlan{}}
	s := New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s", AnswerWindow: window, RingTimeout: window / 2},
		sw, dids, provisioned("1008"),
		func(_ context.Context, id uuid.UUID) (bool, error) { return len(ledger.get(id)) > 0, nil },
		live.has, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sw.s = s
	s.limiter = NewLimiter(1_000_000)
	s.jobMargin = 500 * time.Millisecond
	s.SetFailureLedger(ledger)
	s.WithDNC(&lockedDNC{listed: map[string]bool{"+8613800138000": true}})
	s.cfg.DefaultCountryCode = "86"
	s.beforeHold = func(leg string) {
		if p := sw.planOfLeg(leg); p != nil && p.race == raceBeforeHold {
			sw.dispatchResult(p)
		}
	}

	type kind int
	const (
		normal kind = iota
		onDNC
		malformed
		lineLimit
	)
	rnd := rand.New(rand.NewSource(302))
	ids := make([]uuid.UUID, callIDs)
	kinds := make([]kind, callIDs)
	tries := make([]int, callIDs)
	for i := range ids {
		ids[i] = uuid.Must(uuid.NewV7())
		switch r := rnd.Intn(20); {
		case r < 2:
			kinds[i] = onDNC
		case r < 3:
			kinds[i] = malformed
		case r < 4:
			kinds[i] = lineLimit
		}
		tries[i] = 1 + rnd.Intn(3)
		p := &mixPlan{
			first:    mixOutcome(rnd.Intn(int(mixOutcomes))),
			later:    mixOutcome(rnd.Intn(int(mixNotSent))),
			race:     mixRace(rnd.Intn(int(mixRaces))),
			reached:  make(chan struct{}),
			returned: make(chan struct{}),
		}
		sw.plans[ids[i].String()] = p
	}
	reqFor := func(i int) AIDialRequest {
		r := AIDialRequest{CallID: ids[i], To: fmt.Sprintf("1868888%04d", i), DIDNumber: "95099",
			UserData: map[string]string{"i": fmt.Sprint(i)}}
		switch kinds[i] {
		case onDNC:
			r.To = "13800138000"
		case malformed:
			r.To = "1868888-abc"
		case lineLimit:
			r.DIDNumber = "95098"
		}
		return r
	}

	type response struct {
		i       int
		err     error
		elapsed time.Duration
	}
	answers := make(chan response, callIDs*3)
	var requests, drivers sync.WaitGroup
	allSent := make(chan struct{})
	for i := range ids {
		p := sw.plans[ids[i].String()]
		for range tries[i] {
			delay := time.Duration(rnd.Int63n(int64(20 * time.Millisecond)))
			requests.Go(func() {
				time.Sleep(delay)
				start := time.Now()
				_, err := s.PlaceAI(context.Background(), reqFor(i))
				answers <- response{i, err, time.Since(start)}
				if err == nil || errors.Is(err, ErrPlacementUnconfirmed) {
					p.once.Do(func() { close(p.returned) }) // the placement that reached the switch
				}
			})
		}
		if kinds[i] != normal {
			continue
		}
		// The switch's side of the call, once the placement is in hand.
		drivers.Go(func() {
			select {
			case <-p.reached:
			case <-allSent:
				select {
				case <-p.reached:
				default:
					return // never sent: every attempt found the link closed
				}
			}
			<-p.returned
			switch p.sent {
			case mixAnswered, mixLateAdopted:
				live.set(ids[i], true)
				s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: p.leg})
				answer(s, p.leg)
				s.HandleSwitchEvent(jobEventFor(p.jobID, p.leg, "+OK "+p.leg+"\n"))
			case mixFailed, mixLateFailed:
				if p.race == raceAfter || (p.race == raceBeforeHold && p.sent == mixFailed) {
					sw.dispatchResult(p) // a reply was handled, so there was no hold to race
				}
			case mixSilent:
				// After its window: the placement is recorded as failed, then
				// the switch catches up.
				if !waitFor(10*time.Second, func() bool { return len(ledger.get(ids[i])) == 1 }) {
					t.Errorf("callId %d: no row after its window", i)
					return
				}
				s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: p.leg})
			}
		})
	}
	requests.Wait()
	close(allSent)
	close(answers)
	drivers.Wait()

	// The API answer of every request.
	for a := range answers {
		// Nothing here waits on the switch, so a request that takes this long
		// is blocked on the service itself.
		if a.elapsed > 5*time.Second {
			t.Errorf("callId %d waited %s", a.i, a.elapsed)
		}
		var ok bool
		switch kinds[a.i] {
		case onDNC:
			ok = errors.Is(a.err, ErrNumberOnDNC)
		case malformed:
			ok = errors.Is(a.err, ErrBadNumber)
		case lineLimit:
			ok = errors.Is(a.err, ErrDIDChannelsExhausted) || errors.Is(a.err, ErrAlreadyPlaced)
		default:
			ok = a.err == nil || errors.Is(a.err, ErrPlacementUnconfirmed) ||
				errors.Is(a.err, ErrAlreadyPlaced) || errors.Is(a.err, esl.ErrDown)
		}
		if !ok {
			t.Errorf("callId %d (kind %d): %v", a.i, kinds[a.i], a.err)
		}
	}

	perCall := map[string]int{}
	for _, o := range sw.originateList() {
		perCall[o.vars["aicc_call_id"]]++
	}
	hangups := map[string]string{}
	for _, h := range func() []hungUp {
		eventually(t, "the late legs to be hung up", func() bool {
			n := 0
			for _, p := range sw.plans {
				if p.sent == mixSilent && p.leg != "" {
					n++
				}
			}
			return len(sw.hangupList()) >= n
		})
		return sw.hangupList()
	}() {
		hangups[h.channelID] = h.cause
	}
	wantBridges := 0
	for _, p := range sw.plans {
		if p.leg != "" && (p.sent == mixAnswered || p.sent == mixLateAdopted) {
			wantBridges++
		}
	}
	waitBridges(t, sw.fakeSwitch, wantBridges)
	bridged := map[string]int{}
	sw.fakeSwitch.mu.Lock()
	for _, b := range sw.bridges {
		bridged[b.channelID]++
	}
	sw.fakeSwitch.mu.Unlock()

	var wantLines, placed int
	for i, id := range ids {
		p := sw.plans[id.String()]
		n := perCall[id.String()]
		if kinds[i] != normal || p.leg == "" {
			if n != 0 {
				t.Errorf("callId %d reached the switch %d times without being placed", i, n)
			}
			if rows := ledger.get(id); len(rows) != 0 {
				t.Errorf("callId %d was never placed and has %d rows", i, len(rows))
			}
			continue
		}
		placed++
		if n != 1 {
			t.Errorf("callId %d: %d originates reached the switch, want 1", i, n)
		}
		var wantCause string
		switch p.sent {
		case mixFailed:
			wantCause = "USER_BUSY"
		case mixLateFailed:
			wantCause = "GATEWAY_DOWN"
		case mixSilent:
			wantCause = UnconfirmedCause
		}
		if wantCause == "" {
			wantLines++
			if rows := ledger.get(id); len(rows) != 0 {
				t.Errorf("callId %d (outcome %d) exists and has %d failure rows, first %s", i, p.sent, len(rows), rows[0].HangupCause)
			}
			if bridged[p.leg] != 1 || hangups[p.leg] != "" {
				t.Errorf("callId %d (outcome %d): bridged %d times, hung up %q; want bridged once",
					i, p.sent, bridged[p.leg], hangups[p.leg])
			}
			if data := s.Data().CallData(id); data["i"] != fmt.Sprint(i) {
				t.Errorf("callId %d (outcome %d): business data %v, want the call's own", i, p.sent, data)
			}
		} else {
			eventually(t, "the failure row", func() bool { return len(ledger.get(id)) >= 1 })
			if rows := ledger.get(id); len(rows) != 1 || rows[0].HangupCause != wantCause {
				var causes []string
				for _, r := range rows {
					causes = append(causes, r.HangupCause)
				}
				t.Errorf("callId %d (outcome %d, race %d): rows with %v, want one with %s", i, p.sent, p.race, causes, wantCause)
			}
			if bridged[p.leg] != 0 {
				t.Errorf("callId %d (outcome %d) was bridged", i, p.sent)
			}
			wantHangup := ""
			if p.sent == mixSilent {
				wantHangup = LateAnswerCause
			}
			if hangups[p.leg] != wantHangup {
				t.Errorf("callId %d (outcome %d): hung up %q, want %q", i, p.sent, hangups[p.leg], wantHangup)
			}
		}
		eventually(t, "the claim to be given back", func() bool { return !s.IsPlacing(id) })
	}
	if n := s.LinesInUse("95099"); n != wantLines {
		t.Errorf("lines in use = %d, want %d: one per call that exists", n, wantLines)
	}
	if n := s.PendingCount(); n != 0 {
		t.Errorf("%d continuations still armed: a leg left parked", n)
	}
	if n := s.JobCount(); n != 0 {
		t.Errorf("%d jobs still tracked", n)
	}
	t.Logf("%d callIds, %d placed, %d calls exist", callIDs, placed, wantLines)
}

// waitFor polls cond until it holds or d has passed; for goroutines other than
// the test's, which cannot stop the test.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
