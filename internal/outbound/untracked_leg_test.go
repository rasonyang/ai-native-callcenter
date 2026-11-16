// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// The leg of an AI outbound call that this process neither placed, armed nor
// remembers as late is hung up when its channel is created (#573). Before it,
// such a leg sat in park() with nothing to bridge it, and a customer who
// answered heard silence.

// restarted is a service as it is after a restart: a fresh process in front of
// a switch that answers every command, with the same kind of ledger and clock
// as stalledService.
type restarted struct {
	s      *Service
	sw     *fakeSwitch
	ledger *recordingLedger
	clock  *stallClock
	live   *liveSet
}

func restartedService(t *testing.T) restarted {
	t.Helper()
	sw := &fakeSwitch{}
	ledger := &recordingLedger{cdrs: map[uuid.UUID][]store.CDR{}}
	live := &liveSet{ids: map[uuid.UUID]bool{}}
	clock := &stallClock{now: time.Date(2026, 10, 21, 9, 52, 0, 0, time.UTC)}
	flowID := uuid.Must(uuid.NewV7())
	dids := fakeDIDs{
		{Number: "95099", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true, IsRecordingEnabled: true},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
	}
	s := New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s", AnswerWindow: time.Minute, RingTimeout: 30 * time.Second},
		sw, dids, provisioned("1007", "1008"),
		func(_ context.Context, id uuid.UUID) (bool, error) { return len(ledger.get(id)) > 0, nil },
		live.has, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.limiter = NewLimiter(1_000_000)
	s.now = clock.Now
	s.SetFailureLedger(ledger)
	return restarted{s: s, sw: sw, ledger: ledger, clock: clock, live: live}
}

// createdAI is the CHANNEL_CREATE of an AI outbound customer leg, as the
// normalizer reads it off the originate's variables.
func createdAI(channelID string, callID uuid.UUID) telephony.SwitchEvent {
	return telephony.SwitchEvent{
		Kind: telephony.KindChannelCreate, ChannelID: channelID,
		PlacedBy: telephony.PlacedByDialAI, PlacedCallID: callID,
	}
}

func ringingAI(channelID string, callID uuid.UUID, age time.Duration) telephony.LiveChannel {
	return telephony.LiveChannel{
		ChannelID: channelID, CallID: callID, PlacedBy: telephony.PlacedByDialAI,
		PlacedTo: "18688886660", DID: "95099", Language: "en",
		CampaignID: uuid.NewString(), AttemptID: uuid.NewString(),
		Age: age, IsAgeKnown: true,
	}
}

// noHangupsAfterBridge is the negative assertion's sync point: the leg was bridged,
// so everything the CHANNEL_CREATE started has had its turn, and nothing may
// have ended it. The reap goroutine is started before the bridge's, and the
// reaped map, which it writes first, is read as well.
func noHangupsAfterBridge(t *testing.T, r restarted, leg string) {
	t.Helper()
	waitBridges(t, r.sw, 1)
	if h := r.sw.hangupList(); len(h) != 0 {
		t.Errorf("hangups = %+v, want none", h)
	}
	r.s.mu.Lock()
	_, isReaped := r.s.reaped[leg]
	r.s.mu.Unlock()
	if isReaped {
		t.Error("the leg was handed to the reaper")
	}
}

// Path 1, outbound half: the placement is recorded as failed, the process
// restarts, and the leg the cold-start listing finds ringing is re-armed and
// bridged exactly as before; the real CDR is what revises the attempt
// (internal/dialer, TestALeadRecordedAfterAnEarlyFinishIsAppliedByTheRealCDR).
func TestARestartedProcessStillRearmsTheLegOfAPlacementRecordedAsFailed(t *testing.T) {
	t.Parallel()
	st, timers := manualService(t)
	callID := uuid.Must(uuid.NewV7())
	if _, err := st.s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()
	timers.fire(t) // no channel within the window
	eventually(t, "the placement-failure cdr", func() bool { return len(st.ledger.get(callID)) == 1 })
	if cdr := st.ledger.get(callID)[0]; cdr.HangupCause != UnconfirmedCause || cdr.Tech["isPlacementFailure"] != true {
		t.Fatalf("cdr = %+v", cdr)
	}

	r := restartedService(t) // the restart: its memory of the late leg is gone
	rep := r.s.Recover(context.Background(), []telephony.LiveChannel{ringingAI(leg, callID, 2*time.Second)}, nil, true)
	r.s.MarkReconciled()
	if rep.Rearmed != 1 || rep.WindowSpent != 0 {
		t.Fatalf("report = %+v, want the leg re-armed", rep)
	}
	if !r.s.holdsLeg(leg) {
		t.Fatal("the listed leg is not armed")
	}
	// Its CHANNEL_CREATE can still be dispatched after the listing.
	r.s.HandleSwitchEvent(createdAI(leg, callID))
	answer(r.s, leg)
	noHangupsAfterBridge(t, r, leg)
	if b := r.sw.lastBridge(); b.channelID != leg {
		t.Errorf("bridged %s, want %s", b.channelID, leg)
	}
}

// Path 2 with no reconnect: the listing is empty, the leg is created after it.
func TestAnAILegCreatedAfterARestartThatNothingHoldsIsHungUp(t *testing.T) {
	t.Parallel()
	r := restartedService(t)
	r.s.Recover(context.Background(), nil, nil, true)
	r.s.MarkReconciled()

	callID, leg := uuid.Must(uuid.NewV7()), uuid.NewString()
	r.s.HandleSwitchEvent(createdAI(leg, callID))
	eventually(t, "the untracked leg to be hung up", func() bool { return len(r.sw.hangupList()) == 1 })
	if h := r.sw.hangupList()[0]; h.channelID != leg || h.cause != LateAnswerCause {
		t.Errorf("hangup = %+v, want %s ended as %s", h, leg, LateAnswerCause)
	}
	if r.s.holdsLeg(leg) {
		t.Error("something is armed for the leg")
	}
	// Answered all the same (the hangup may not have reached the switch): nothing to bridge.
	answer(r.s, leg)
	if n := r.sw.bridgeCount(); n != 0 {
		t.Errorf("%d bridges for a leg nothing armed", n)
	}
}

// Control: the leg the cold-start listing re-armed, then its CHANNEL_CREATE.
func TestAListedRingingLegIsNotHungUpByItsLateCreate(t *testing.T) {
	t.Parallel()
	r := restartedService(t)
	callID, leg := uuid.Must(uuid.NewV7()), uuid.NewString()
	rep := r.s.Recover(context.Background(), []telephony.LiveChannel{ringingAI(leg, callID, 5*time.Second)}, nil, true)
	r.s.MarkReconciled()
	if rep.Rearmed != 1 {
		t.Fatalf("report = %+v, want the leg re-armed", rep)
	}
	r.s.HandleSwitchEvent(createdAI(leg, callID))
	answer(r.s, leg)
	noHangupsAfterBridge(t, r, leg)
}

// Control: this process's own placement.
func TestTheProcesssOwnAILegIsNotHungUpAtItsCreate(t *testing.T) {
	t.Parallel()
	r := restartedService(t)
	r.s.Recover(context.Background(), nil, nil, true)
	r.s.MarkReconciled()
	callID := uuid.Must(uuid.NewV7())
	if _, err := r.s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); err != nil {
		t.Fatal(err)
	}
	leg := r.sw.originates[0].partyID.String()
	r.s.HandleSwitchEvent(createdAI(leg, callID))
	answer(r.s, leg)
	noHangupsAfterBridge(t, r, leg)
}

// Control: an unconfirmed placement the window adopted because the registry saw
// its call live before noteChannel ran keeps its pending entry (its legJobs
// entry is gone), and the CHANNEL_CREATE that follows leaves it alone.
func TestAnAdoptedUnconfirmedAILegIsNotHungUpAtItsCreate(t *testing.T) {
	t.Parallel()
	st, timers := manualService(t)
	s := st.s
	callID := uuid.Must(uuid.NewV7())
	if _, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); !errors.Is(err, ErrPlacementUnconfirmed) {
		t.Fatalf("PlaceAI error = %v", err)
	}
	leg := st.sw.originateList()[0].partyID.String()
	st.clock.Advance(119 * time.Second)
	st.live.set(callID, true)
	timers.fire(t) // adopted: legJobs forgets the leg, pending keeps it
	if !s.holdsLeg(leg) {
		t.Fatal("the adopted leg is not armed")
	}
	s.HandleSwitchEvent(createdAI(leg, callID))
	st.clock.Advance(30 * time.Second)
	answer(s, leg)
	waitBridges(t, st.sw.fakeSwitch, 1)
	if h := st.sw.hangupList(); len(h) != 0 {
		t.Errorf("hangups = %+v, want none", h)
	}
}

// Boundary: a leg that is not an AI outbound leg is not this rule's.
func TestAnUntrackedLegThatIsNotAnAIOutboundLegIsLeftAlone(t *testing.T) {
	t.Parallel()
	r := restartedService(t)
	r.s.Recover(context.Background(), nil, nil, true)
	r.s.MarkReconciled()
	for _, placedBy := range []string{"", telephony.PlacedByDial} {
		leg := uuid.NewString()
		r.s.HandleSwitchEvent(telephony.SwitchEvent{Kind: telephony.KindChannelCreate, ChannelID: leg, PlacedBy: placedBy})
	}
	// Then one that is ours and is hung up, so the check below has a sync
	// point: the reaper has run for it, and for nothing else.
	ours := uuid.NewString()
	r.s.HandleSwitchEvent(createdAI(ours, uuid.Must(uuid.NewV7())))
	eventually(t, "the AI leg to be hung up", func() bool { return len(r.sw.hangupList()) >= 1 })
	if h := r.sw.hangupList(); len(h) != 1 || h[0].channelID != ours {
		t.Errorf("hangups = %+v, want only %s", h, ours)
	}
}

// createsDuringOriginate is a switch that creates the channel before the
// originate returns, as a fast one can: the CHANNEL_CREATE reaches the service
// while the leg is in legJobs and not yet armed (DialAI arms after the
// originate returns).
type createsDuringOriginate struct {
	*fakeSwitch
	svc *Service
}

func (c *createsDuringOriginate) Originate(partyID uuid.UUID, endpoint string, vars map[string]string) (string, error) {
	jobID, err := c.fakeSwitch.Originate(partyID, endpoint, vars)
	c.svc.HandleSwitchEvent(telephony.SwitchEvent{
		Kind: telephony.KindChannelCreate, ChannelID: partyID.String(),
		PlacedBy: vars[telephony.VarPlacedBy], PlacedCallID: uuid.MustParse(vars["aicc_call_id"]),
	})
	return jobID, err
}

// Control: a leg tracked by legJobs alone, before its continuation is armed.
func TestAnOwnAILegCreatedBeforeItsOriginateReturnsIsNotHungUp(t *testing.T) {
	t.Parallel()
	r := restartedService(t)
	r.s.Recover(context.Background(), nil, nil, true)
	r.s.MarkReconciled()
	eager := &createsDuringOriginate{fakeSwitch: r.sw, svc: r.s}
	r.s.sw = eager
	callID := uuid.Must(uuid.NewV7())
	if _, err := r.s.PlaceAI(context.Background(), AIDialRequest{CallID: callID, To: "18688886660", DIDNumber: "95099"}); err != nil {
		t.Fatal(err)
	}
	leg := r.sw.originates[0].partyID.String()
	answer(r.s, leg)
	noHangupsAfterBridge(t, r, leg)
}
