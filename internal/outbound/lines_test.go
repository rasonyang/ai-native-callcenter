// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

func intp(n int) *int { return &n }

// limitedService is a service whose two outbound DIDs (95012, which answers
// with a flow, and 95011, the default outbound one) are limited to n lines.
func limitedService(t *testing.T, n *int) (*Service, *fakeSwitch) {
	t.Helper()
	sw := &fakeSwitch{}
	s := testService(t, sw, nil)
	flowID := uuid.Must(uuid.NewV7())
	s.dids = fakeDIDs{
		{Number: "95012", Language: "zh", FlowID: &flowID, IsEnabled: true, AllowInbound: true,
			AllowOutbound: true, MaxOutboundChannels: n},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true,
			IsDefaultOutbound: true, MaxOutboundChannels: n},
	}
	s.limiter = NewLimiter(1_000_000)
	return s, sw
}

func dialAI(s *Service, id uuid.UUID, did string) error {
	_, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: id, To: "13912345678", DIDNumber: did})
	return err
}

func TestLinesReserveStopsAtTheLimitUnderContention(t *testing.T) {
	t.Parallel()
	l := NewLines()
	var granted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Reserve(uuid.Must(uuid.NewV7()), "95555", intp(3)) {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 3 || l.InUse("95555") != 3 {
		t.Fatalf("granted %d, in use %d, want 3 and 3", granted.Load(), l.InUse("95555"))
	}
}

func TestLinesReserveIsIdempotentPerCallAndPerDID(t *testing.T) {
	t.Parallel()
	l := NewLines()
	id := uuid.Must(uuid.NewV7())
	for i := 0; i < 3; i++ {
		if !l.Reserve(id, "95555", intp(1)) {
			t.Fatal("a retried reservation was refused")
		}
	}
	if l.InUse("95555") != 1 {
		t.Fatalf("in use %d, want 1", l.InUse("95555"))
	}
	if !l.Release(id) || l.Release(id) {
		t.Fatal("release must succeed once")
	}
	if l.InUse("95555") != 0 {
		t.Fatal("line not given back")
	}
	if !l.Reserve(uuid.Must(uuid.NewV7()), "95555", nil) || !l.Reserve(uuid.Must(uuid.NewV7()), "95555", nil) {
		t.Fatal("no limit must mean no refusal")
	}
}

func TestADIDAtItsLimitRefusesTheSingleCallAndSendsNothing(t *testing.T) {
	t.Parallel()
	s, sw := limitedService(t, intp(2))
	for i := 0; i < 2; i++ {
		if err := dialAI(s, uuid.Must(uuid.NewV7()), "95012"); err != nil {
			t.Fatal(err)
		}
	}
	extra := uuid.Must(uuid.NewV7())
	err := dialAI(s, extra, "95012")
	if !errors.Is(err, ErrDIDChannelsExhausted) {
		t.Fatalf("err = %v, want ErrDIDChannelsExhausted", err)
	}
	if len(sw.originates) != 2 {
		t.Fatalf("%d originates, want 2: the refused call must send nothing", len(sw.originates))
	}
	if s.callData.CallData(extra) != nil {
		t.Error("the refused call left its business data behind")
	}
	// The other number is a different account.
	// 95011 is a different account and holds nothing.
	if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1008", To: "13912345678"}); err != nil {
		t.Fatalf("click-to-dial from the other DID = %v, want it placed", err)
	}
}

func TestClickToDialOverTheLimitIsRefused(t *testing.T) {
	t.Parallel()
	s, sw := limitedService(t, intp(1))
	if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1008", To: "13912345678"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1002", To: "13987654321"})
	if !errors.Is(err, ErrDIDChannelsExhausted) {
		t.Fatalf("err = %v, want ErrDIDChannelsExhausted", err)
	}
	if len(sw.originates) != 1 {
		t.Fatalf("%d originates, want 1", len(sw.originates))
	}
}

func TestTheLineIsTakenBeforeTheOriginateIsSent(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(5))
	var seen atomic.Int32
	s.sw = &hookSwitch{fakeSwitch: &fakeSwitch{}, before: func() { seen.Store(int32(s.LinesInUse("95012"))) }}
	if err := dialAI(s, uuid.Must(uuid.NewV7()), "95012"); err != nil {
		t.Fatal(err)
	}
	if seen.Load() != 1 {
		t.Fatalf("lines in use when the originate was sent = %d, want 1", seen.Load())
	}
}

// hookSwitch runs before on every originate and can fail it.
type hookSwitch struct {
	*fakeSwitch
	before func()
	err    error
}

func (h *hookSwitch) Originate(p uuid.UUID, ep string, vars map[string]string) (string, error) {
	if h.before != nil {
		h.before()
	}
	if h.err != nil {
		return "", h.err
	}
	return h.fakeSwitch.Originate(p, ep, vars)
}

func TestOriginatesFiredInTheSameInstantDoNotSlipPastTheLimit(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(3))
	var sent atomic.Int32
	gate := make(chan struct{})
	// A slow switch: every originate waits at the gate, so a limit enforced
	// only once channels exist would let all of them through.
	s.sw = &hookSwitch{fakeSwitch: &fakeSwitch{}, before: func() { sent.Add(1); <-gate }}
	var refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := dialAI(s, uuid.Must(uuid.NewV7()), "95012"); errors.Is(err, ErrDIDChannelsExhausted) {
				refused.Add(1)
			}
		}()
	}
	eventually(t, "the refusals", func() bool { return refused.Load() == 17 })
	close(gate)
	wg.Wait()
	if sent.Load() != 3 {
		t.Fatalf("%d originates went out against a limit of 3", sent.Load())
	}
}

func TestAFailedOriginateGivesTheLineBack(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(1))
	s.sw = &hookSwitch{fakeSwitch: &fakeSwitch{}, err: errors.New("switch down")}
	if err := dialAI(s, uuid.Must(uuid.NewV7()), "95012"); err == nil {
		t.Fatal("setup: the originate should fail")
	}
	if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1008", To: "13912345678"}); err == nil {
		t.Fatal("setup: the click-to-dial originate should fail")
	}
	if n := s.LinesInUse("95012") + s.LinesInUse("95011"); n != 0 {
		t.Fatalf("%d lines still held after failed originates", n)
	}
}

func TestAnOriginateThatNeverMadeAChannelGivesTheLineBack(t *testing.T) {
	t.Parallel()
	s, sw, _ := jobService(t)
	s.dids = fakeDIDs{{Number: "95012", Language: "zh", FlowID: ptrUUID(), IsEnabled: true, AllowOutbound: true,
		MaxOutboundChannels: intp(1)}}
	id := uuid.Must(uuid.NewV7())
	if err := dialAI(s, id, "95012"); err != nil {
		t.Fatal(err)
	}
	if s.LinesInUse("95012") != 1 {
		t.Fatal("setup: no line held")
	}
	jobID := ""
	for j := range sw.legs {
		jobID = j
	}
	s.HandleSwitchEvent(jobEvent(jobID, "-ERR GATEWAY_DOWN\n"))
	if s.LinesInUse("95012") != 0 {
		t.Fatal("the line outlived an originate that never made a channel")
	}
}

func ptrUUID() *uuid.UUID { id := uuid.Must(uuid.NewV7()); return &id }

func TestACallEndingGivesTheLineBackWhateverItsEnding(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(2))
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := dialAI(s, a, "95012"); err != nil {
		t.Fatal(err)
	}
	if err := dialAI(s, b, "95012"); err != nil {
		t.Fatal(err)
	}
	s.CallEnded(a) // unanswered: the ring-out call finished
	if err := dialAI(s, uuid.Must(uuid.NewV7()), "95012"); err != nil {
		t.Fatalf("the freed line was not available: %v", err)
	}
	s.CallEnded(b)
	s.CallEnded(b) // a second signal for the same call is harmless
	if s.LinesInUse("95012") != 1 {
		t.Fatalf("in use %d, want 1", s.LinesInUse("95012"))
	}
}

// A random sequence of placements (single, click-to-dial), failed originates
// and endings never lets a DID go over its limit, never leaks, and ends at zero.
func TestLinesNeverLeakAndNeverExceedTheLimitUnderRandomEndings(t *testing.T) {
	t.Parallel()
	const limit = 4
	for seed := int64(1); seed <= 20; seed++ {
		s, _ := limitedService(t, intp(limit))
		hs := &hookSwitch{fakeSwitch: &fakeSwitch{}}
		s.sw = hs
		rng := rand.New(rand.NewSource(seed))
		var live []uuid.UUID
		for step := 0; step < 400; step++ {
			switch rng.Intn(4) {
			case 0, 1: // place a single call, sometimes with a failing switch
				hs.err = nil
				if rng.Intn(5) == 0 {
					hs.err = errors.New("originate failed")
				}
				id := uuid.Must(uuid.NewV7())
				err := dialAI(s, id, "95012")
				switch {
				case err == nil:
					live = append(live, id)
				case errors.Is(err, ErrDIDChannelsExhausted):
					if s.LinesInUse("95012") < limit {
						t.Fatalf("seed %d: refused with %d of %d lines in use", seed, s.LinesInUse("95012"), limit)
					}
				}
			case 2: // click-to-dial from the other DID
				hs.err = nil
				id := uuid.Must(uuid.NewV7())
				_, err := s.Dial(context.Background(), AgentDialRequest{CallID: id, AgentExtension: "1008", To: "13912345678"})
				if err == nil {
					live = append(live, id)
				}
			case 3: // a call ends, by any of the ending signals
				if len(live) > 0 {
					i := rng.Intn(len(live))
					s.CallEnded(live[i])
					live = append(live[:i], live[i+1:]...)
				}
			}
			for _, did := range []string{"95012", "95011"} {
				if n := s.LinesInUse(did); n > limit {
					t.Fatalf("seed %d step %d: %d lines on %s, limit %d", seed, step, n, did, limit)
				}
			}
			if got, want := s.LinesInUse("95012")+s.LinesInUse("95011"), len(live); got != want {
				t.Fatalf("seed %d step %d: %d lines held for %d live calls", seed, step, got, want)
			}
		}
		for _, id := range live {
			s.CallEnded(id)
		}
		if n := s.LinesInUse("95012") + s.LinesInUse("95011"); n != 0 {
			t.Fatalf("seed %d: %d lines leaked", seed, n)
		}
	}
}

func handDialled(channel, did, destination string, call uuid.UUID, mod func(*telephony.SwitchEvent)) telephony.SwitchEvent {
	headers := map[string]string{"variable_aicc_did": did}
	if call != uuid.Nil {
		headers["variable_aicc_call_id"] = call.String()
	}
	ev := telephony.SwitchEvent{
		Kind: telephony.KindChannelCreate, ChannelID: channel, Direction: telephony.DirectionOutbound,
		DestinationNumber: destination, LegDID: did, Raw: esl.NewEvent(headers, ""),
	}
	if mod != nil {
		mod(&ev)
	}
	return ev
}

func TestAHandDialledTrunkLegIsCountedAndUncounted(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(2))
	s.HandleSwitchEvent(handDialled("ch-1", "95011", "13900000001", uuid.Must(uuid.NewV7()), nil))
	s.HandleSwitchEvent(handDialled("ch-1", "95011", "13900000001", uuid.Must(uuid.NewV7()), nil)) // duplicate event
	if s.LinesInUse("95011") != 1 {
		t.Fatalf("in use %d, want 1", s.LinesInUse("95011"))
	}
	s.HandleSwitchEvent(handDialled("ch-2", "95011", "13900000002", uuid.Must(uuid.NewV7()), nil))
	// The limit now bites a single call, though AICC placed neither of those.
	if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1008", To: "13912345678"}); !errors.Is(err, ErrDIDChannelsExhausted) {
		t.Fatalf("err = %v, want the hand-dialled calls to have taken the lines", err)
	}
	hangup(s, "ch-1")
	hangup(s, "ch-1")
	if s.LinesInUse("95011") != 1 {
		t.Fatalf("in use %d after one hangup, want 1", s.LinesInUse("95011"))
	}
	hangup(s, "ch-2")
	if s.LinesInUse("95011") != 0 {
		t.Fatal("hung-up hand-dialled legs still hold lines")
	}
}

func TestLegsThatAreNotCarrierLinesAreNotObserved(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(5))
	hint := func(h string) func(*telephony.SwitchEvent) {
		return func(ev *telephony.SwitchEvent) { ev.CallTypeHint = h }
	}
	for name, ev := range map[string]telephony.SwitchEvent{
		"the leg towards the bot":   handDialled("a", "95011", "95011", uuid.Must(uuid.NewV7()), nil),
		"a queue delivery":          handDialled("b", "95011", "tok", uuid.Must(uuid.NewV7()), func(e *telephony.SwitchEvent) { e.MemberChannelID = "caller" }),
		"an extension to extension": handDialled("c", "95011", "1007", uuid.Must(uuid.NewV7()), func(e *telephony.SwitchEvent) { e.ParentChannelID = "x" }),
		"an internal destination":   handDialled("d", "95011", "1007", uuid.Must(uuid.NewV7()), nil),
		"a call typed inbound":      handDialled("e", "95011", "13900000001", uuid.Must(uuid.NewV7()), hint("INBOUND")),
		"an inbound direction":      handDialled("f", "95011", "13900000001", uuid.Must(uuid.NewV7()), func(e *telephony.SwitchEvent) { e.Direction = telephony.DirectionInbound }),
		"no DID on it":              handDialled("g", "", "13900000001", uuid.Must(uuid.NewV7()), nil),
	} {
		s.HandleSwitchEvent(ev)
		if n := s.LinesInUse("95011"); n != 0 {
			t.Errorf("%s was counted as a carrier line", name)
		}
	}
}

func TestAClickToDialIsCountedOnceWhenItsTrunkLegAppears(t *testing.T) {
	t.Parallel()
	s, sw := limitedService(t, intp(3))
	if _, err := s.Dial(context.Background(), AgentDialRequest{AgentExtension: "1008", To: "13912345678"}); err != nil {
		t.Fatal(err)
	}
	callID := uuid.MustParse(sw.lastOriginate().vars["aicc_call_id"])
	// The trunk leg the dialplan raises inherits the call id and carries the DID.
	s.HandleSwitchEvent(handDialled("trunk-1", "95011", "13912345678", callID, nil))
	if s.LinesInUse("95011") != 1 {
		t.Fatalf("in use %d, want the click-to-dial counted once", s.LinesInUse("95011"))
	}
	hangup(s, "trunk-1")
	if s.LinesInUse("95011") != 1 {
		t.Fatal("the trunk leg hanging up released the call's own reservation: the call is not over")
	}
	s.CallEnded(callID)
	if s.LinesInUse("95011") != 0 {
		t.Fatal("line leaked")
	}
}

func TestRecoveryRebuildsTheLinesFromTheSwitch(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(5))
	placed := uuid.Must(uuid.NewV7())
	live := []telephony.LiveChannel{
		{ChannelID: "cust-1", CallID: placed, PlacedBy: telephony.PlacedByDialAI, DID: "95012"},
		{ChannelID: "cust-2", CallID: uuid.Must(uuid.NewV7()), PlacedBy: telephony.PlacedByDialAI, DID: "95012", PeerChannelID: "bot-2", PeerIsBotLeg: true, IsAnswered: true},
		{ChannelID: "bot-2", CallID: uuid.Must(uuid.NewV7()), IsBotLeg: true, DID: "95012"},
		{ChannelID: "hand-1", CallID: uuid.Must(uuid.NewV7()), DID: "95011", OutboundLineDID: "95011", DialedNumber: "13900000009"},
		{ChannelID: "in-1", CallID: uuid.Must(uuid.NewV7()), IsInbound: true, DID: "95012"},
		{ChannelID: "ext-1", CallID: uuid.Must(uuid.NewV7()), DID: "95011", OutboundLineDID: "95011", DialedNumber: "1007"},
	}
	s.Recover(context.Background(), live, nil, true)
	if got := s.LinesInUse("95012"); got != 2 {
		t.Errorf("95012 holds %d lines after recovery, want the 2 AI calls", got)
	}
	if got := s.LinesInUse("95011"); got != 1 {
		t.Errorf("95011 holds %d lines after recovery, want the 1 hand-dialled leg", got)
	}
	// A second pass over the same channels (a reconnect) changes nothing.
	s.Recover(context.Background(), live, nil, false)
	if s.LinesInUse("95012") != 2 || s.LinesInUse("95011") != 1 {
		t.Fatal("recovery is not idempotent")
	}
	// A call recovered that ends releases its line; a hand-dialled leg whose
	// hangup was lost while the link was down is dropped by the next listing.
	s.CallEnded(placed)
	s.Recover(context.Background(), live[1:3], nil, false)
	if s.LinesInUse("95012") != 1 || s.LinesInUse("95011") != 0 {
		t.Fatalf("after the ending and the next listing: 95012=%d 95011=%d, want 1 and 0",
			s.LinesInUse("95012"), s.LinesInUse("95011"))
	}
}

func TestAReservationNobodyReleasedIsSweptWhenItsCallIsNotLive(t *testing.T) {
	t.Parallel()
	l := NewLines()
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	gone, alive := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	l.Reserve(gone, "95555", nil)
	l.Reserve(alive, "95555", nil)
	now = now.Add(5 * time.Minute)
	released := l.Sweep(2*time.Minute, func(id uuid.UUID) bool { return id == alive })
	if len(released) != 1 || released[0] != gone || l.InUse("95555") != 1 {
		t.Fatalf("released %v, in use %d: want only the call that is gone", released, l.InUse("95555"))
	}
	// Not again within the minute.
	if got := l.Sweep(0, nil); got != nil {
		t.Fatalf("swept twice within a minute: %v", got)
	}
}

func TestLinesFreedObserverIsToldOnEveryRelease(t *testing.T) {
	t.Parallel()
	l := NewLines()
	var n atomic.Int32
	l.SetOnFree(func() { n.Add(1) })
	id := uuid.Must(uuid.NewV7())
	l.Reserve(id, "1", nil)
	l.Observe("ch", uuid.Nil, "1")
	l.Release(id)
	l.Release(id) // nothing to give back: no signal
	l.Unobserve("ch")
	if n.Load() != 2 {
		t.Fatalf("told %d times, want 2", n.Load())
	}
}

func TestADIDWithoutALimitIsNeverRefused(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, nil)
	for i := 0; i < 50; i++ {
		if err := dialAI(s, uuid.Must(uuid.NewV7()), "95012"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if s.LinesInUse("95012") != 50 {
		t.Fatalf("in use %d, want 50 (counted for the gauge even when unlimited)", s.LinesInUse("95012"))
	}
}

func TestDIDLimitsListsOnlyTheLimitedNumbers(t *testing.T) {
	t.Parallel()
	s, _ := limitedService(t, intp(7))
	s.dids = fakeDIDs{
		{Number: "1", MaxOutboundChannels: intp(7)},
		{Number: "2"},
	}
	got, err := s.DIDLimits(context.Background())
	if err != nil || len(got) != 1 || got["1"] != 7 {
		t.Fatalf("limits = %v, err %v", got, err)
	}
}
