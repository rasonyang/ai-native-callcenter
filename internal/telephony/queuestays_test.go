// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// A call that passes through more than one queue (#283). The call kept one
// queue record, so a second join overwrote the first one's name and join time
// and kept its bridge and cause: a caller answered in one queue and
// transferred to another came out with one QUEUE leg, the second queue's, and
// a wait measured from the second join to the first bridge — negative.
//
// These play the switch's events through the registry, in the order and with
// the stamps mod_callcenter sends them, and assemble the CDR from the
// snapshot the registry hands the ledger.

// liveLeg is one channel on the call, created when the switch created it.
type liveLeg struct {
	channel, number string
	agentID         *uuid.UUID
	created         int
}

// throughTheRegistry plays a call's switch events through a registry and
// returns the final snapshot, the one the ledger is written from.
func throughTheRegistry(t *testing.T, legs []liveLeg, evs []SwitchEvent) Snapshot {
	t.Helper()
	reg := NewRegistry(nullPublisher{})
	finished := make(chan Snapshot, 1)
	reg.OnCallFinished = func(s Snapshot) { finished <- s }
	t.Cleanup(reg.Shutdown)

	callID := uuid.Must(uuid.NewV7())
	call, err := reg.CreateCall(t.Context(), callID, events.CallTypeInbound, "en", false, at(legs[0].created))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range legs {
		p := call.AddParty(l.channel, l.number, at(l.created))
		p.AgentID = l.agentID
		if err := reg.BindChannel(l.channel, callID); err != nil {
			t.Fatal(err)
		}
	}
	for _, ev := range evs {
		if !reg.Dispatch(ev) {
			t.Fatalf("event %s on %q was not routed", ev.Kind, ev.ChannelID)
		}
	}
	select {
	case snap := <-finished:
		return snap
	case <-time.After(5 * time.Second):
		t.Fatal("the call never finished")
		return Snapshot{}
	}
}

func chAnswer(ch string, sec int) SwitchEvent {
	return SwitchEvent{Kind: KindChannelAnswer, ChannelID: ch, OccurredAt: at(sec)}
}

func chBridge(ch, other string, sec int) SwitchEvent {
	return SwitchEvent{Kind: KindChannelBridge, ChannelID: ch, OtherChannelID: other, OccurredAt: at(sec)}
}

func chUnbridge(ch, other string, sec int) SwitchEvent {
	return SwitchEvent{Kind: KindChannelUnbridge, ChannelID: ch, OtherChannelID: other, OccurredAt: at(sec)}
}

func chHangup(ch, cause string, sec int) SwitchEvent {
	return SwitchEvent{Kind: KindChannelHangup, ChannelID: ch, HangupCause: cause, OccurredAt: at(sec)}
}

// The queue events about the caller, raised on the caller's channel (the
// member session), stamped the way mod_callcenter stamps them live.
// member-queue-start carries no CC-Member-Joined-Time, so its JoinedAt is
// zero and the only clock on it is the event's own, to the microsecond; every
// later event carries the joined time in whole seconds. The helpers take the
// join's whole second and land the start event joinLag into it, so a test
// that matched stays by an exact timestamp would fail here as it did live.
const joinLag = 437 * time.Millisecond

// msAt is a moment sec seconds and milli milliseconds into the call.
func msAt(sec, milli int) time.Time {
	return at(sec).Add(time.Duration(milli) * time.Millisecond)
}

// qJoin is a member-queue-start in the joined second sec.
func qJoin(queue string, sec int) SwitchEvent {
	return qJoinAt(queue, at(sec).Add(joinLag))
}

// qJoinAt is a member-queue-start the switch raised at moment t.
func qJoinAt(queue string, t time.Time) SwitchEvent {
	return SwitchEvent{Kind: KindQueueMemberJoined, Queue: queue, ChannelID: "caller",
		MemberChannelID: "caller", OccurredAt: t}
}

func qBridge(queue string, joined, sec int) SwitchEvent {
	return qBridgeAt(queue, joined, at(sec))
}

// qBridgeAt is a bridge-agent-start at moment t for the stay joined in the
// whole second joined.
func qBridgeAt(queue string, joined int, t time.Time) SwitchEvent {
	return SwitchEvent{Kind: KindQueueBridgeStart, Queue: queue, ChannelID: "caller",
		MemberChannelID: "caller", OccurredAt: t, JoinedAt: at(joined)}
}

func qLeft(queue string, joined, sec int, cause, reason string) SwitchEvent {
	return qLeftAt(queue, joined, at(sec), cause, reason)
}

// qLeftAt is a member-queue-end raised at moment t for the stay joined in the
// whole second joined; its leaving time is t's whole second, as the switch
// writes it.
func qLeftAt(queue string, joined int, t time.Time, cause, reason string) SwitchEvent {
	return SwitchEvent{Kind: KindQueueMemberLeft, Queue: queue, ChannelID: "caller",
		MemberChannelID: "caller", OccurredAt: t, JoinedAt: at(joined),
		LeftAt: t.Truncate(time.Second), Cause: cause, CancelReason: reason}
}

// noNegativeDuration fails on any duration below zero on the row or its
// journey.
func noNegativeDuration(t *testing.T, cdr store.CDR) {
	t.Helper()
	if cdr.QueueWaitSec < 0 || cdr.TalkSec < 0 || cdr.RingSec < 0 || cdr.BillSec < 0 {
		t.Errorf("negative duration on the row: wait %d talk %d ring %d bill %d",
			cdr.QueueWaitSec, cdr.TalkSec, cdr.RingSec, cdr.BillSec)
	}
	for _, leg := range cdr.Legs {
		if leg.DurationSec < 0 {
			t.Errorf("leg %s %s lasted %d s", leg.Kind, leg.Label, leg.DurationSec)
		}
	}
}

// The transfer at the heart of #283: answered by wei in sales after a
// colleague's phone rang out, passed by wei to support, answered there by ben.
// Both queues are in the journey with their own waits, in the order the
// caller went through them; the row is the first queue and its wait.
func TestACallAnsweredInTwoQueuesShowsBothStays(t *testing.T) {
	t.Parallel()
	wei, ben, carol := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	sales, support := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	snap := throughTheRegistry(t, []liveLeg{
		{channel: "caller", number: "+8613911112222", created: 0},
		{channel: "carol", number: "1009", agentID: &carol, created: 4},
		{channel: "wei", number: "1008", agentID: &wei, created: 9},
		{channel: "ben", number: "1007", agentID: &ben, created: 95},
	}, []SwitchEvent{
		chAnswer("caller", 0),
		qJoin("sales", 2),
		// The queue tried carol first; she let it ring.
		chHangup("carol", "NO_ANSWER", 9),
		chAnswer("wei", 10),
		chBridge("wei", "caller", 10),
		qBridge("sales", 2, 10),
		// Wei transfers the caller to support and drops off.
		chUnbridge("caller", "wei", 70),
		qLeft("sales", 2, 70, "Terminated", ""),
		chHangup("wei", "NORMAL_CLEARING", 70),
		qJoin("support", 71),
		chAnswer("ben", 100),
		chBridge("ben", "caller", 100),
		qBridge("support", 71, 100),
		qLeft("support", 71, 160, "Terminated", ""),
		chHangup("caller", "NORMAL_CLEARING", 160),
		chHangup("ben", "NORMAL_CLEARING", 160),
	})

	if n := len(snap.QueueStays); n != 2 {
		t.Fatalf("recorded %d queue stays, want 2: %+v", n, snap.QueueStays)
	}
	if got := snap.QueueStays[1]; !got.BridgedAt.Equal(at(100)) || got.Cause != "Terminated" {
		t.Errorf("support stay = %+v; it must carry its own bridge, not sales'", got)
	}
	// The live snapshot's queue is the current stay, and the history stays
	// off the wire: the contract's CallSnapshot.queue is one QueueFacts.
	if snap.Queue.Name != "support" || !snap.Queue.JoinedAt.Equal(at(71).Add(joinLag)) {
		t.Errorf("snapshot queue = %+v, want the current (support) stay", snap.Queue)
	}
	wire, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["queue"]; !ok {
		t.Error("the snapshot lost its queue on the wire")
	}
	for _, key := range []string{"queueStays", "QueueStays"} {
		if _, ok := fields[key]; ok {
			t.Errorf("the snapshot puts %q on the wire, which the contract does not declare", key)
		}
	}

	cdr := newAssembler(&memoryLedger{}, staticQueues{"sales": sales, "support": support}).assemble(t.Context(), snap)
	want := []string{"QUEUE:sales:7", "AGENT:1009:did not answer", "AGENT:1008:60", "QUEUE:support:28", "AGENT:1007:60"}
	if got := journey(cdr.Legs); !slices.Equal(got, want) {
		t.Errorf("journey = %v, want %v", got, want)
	}
	if cdr.QueueID == nil || *cdr.QueueID != sales {
		t.Errorf("row queue = %v, want sales (%s), the first queue joined", cdr.QueueID, sales)
	}
	if cdr.QueueWaitSec != 7 {
		t.Errorf("queueWaitSec = %d, want sales' 7 s", cdr.QueueWaitSec)
	}
	if cdr.Status != store.CDRStatusAnswered || cdr.PrimaryAgentID == nil || *cdr.PrimaryAgentID != wei {
		t.Errorf("status %s, primary %v; want ANSWERED by wei", cdr.Status, cdr.PrimaryAgentID)
	}
	if cdr.TalkSec != 120 {
		t.Errorf("talkSec = %d, want 120 (60 with each agent)", cdr.TalkSec)
	}
	for _, id := range []uuid.UUID{carol, wei, ben} {
		if !slices.Contains(cdr.AgentIDs, id) {
			t.Errorf("agentIds %v is missing %s", cdr.AgentIDs, id)
		}
	}
	noNegativeDuration(t, cdr)
}

// Answered in sales, transferred to support, and the caller gave up there
// while ben's phone rang. Support's leg carries support's cancel reason, and
// the call is ANSWERED: a person was reached, which is the existing rule.
func TestACallerWhoGaveUpInTheSecondQueueAfterBeingAnswered(t *testing.T) {
	t.Parallel()
	wei, ben := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	sales, support := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	snap := throughTheRegistry(t, []liveLeg{
		{channel: "caller", number: "+8613911113333", created: 0},
		{channel: "wei", number: "1008", agentID: &wei, created: 9},
		{channel: "ben", number: "1007", agentID: &ben, created: 95},
	}, []SwitchEvent{
		chAnswer("caller", 0),
		qJoin("sales", 2),
		chAnswer("wei", 10),
		chBridge("wei", "caller", 10),
		qBridge("sales", 2, 10),
		chUnbridge("caller", "wei", 70),
		chHangup("wei", "NORMAL_CLEARING", 70),
		// The switch's member-queue-end for sales lands after the caller is
		// already in support; it still belongs to the sales stay.
		qJoin("support", 71),
		qLeft("sales", 2, 70, "Terminated", ""),
		qLeft("support", 71, 112, "Cancel", "BREAK_OUT"),
		chHangup("ben", "ORIGINATOR_CANCEL", 112),
		chHangup("caller", "NORMAL_CLEARING", 112),
	})

	cdr := newAssembler(&memoryLedger{}, staticQueues{"sales": sales, "support": support}).assemble(t.Context(), snap)
	want := []string{"QUEUE:sales:7", "AGENT:1008:60", "QUEUE:support:BREAK_OUT", "AGENT:1007:did not answer"}
	if got := journey(cdr.Legs); !slices.Equal(got, want) {
		t.Errorf("journey = %v, want %v", got, want)
	}
	var supportLeg *store.Leg
	for i := range cdr.Legs {
		if cdr.Legs[i].Kind == "QUEUE" && cdr.Legs[i].Label == "support" {
			supportLeg = &cdr.Legs[i]
		}
	}
	if supportLeg == nil || supportLeg.DurationSec != 40 {
		t.Errorf("support leg = %+v, want the 40 s the caller waited there", supportLeg)
	}
	if cdr.Status != store.CDRStatusAnswered || cdr.MissedReason != "" {
		t.Errorf("status %s, missed %q; want ANSWERED with no missed reason", cdr.Status, cdr.MissedReason)
	}
	if cdr.QueueID == nil || *cdr.QueueID != sales || cdr.QueueWaitSec != 7 {
		t.Errorf("row queue %v wait %d; want sales and 7 s", cdr.QueueID, cdr.QueueWaitSec)
	}
	noNegativeDuration(t, cdr)
}

// Nobody answered in either queue. The row is still the first queue and its
// wait; the missed reason is how the call ended, which is the last stay: the
// caller gave up in support, and sales timing them out is how they got there.
func TestACallerTimedOutOfOneQueueAndLeftTheNext(t *testing.T) {
	t.Parallel()
	carol, ben := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	sales, support := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	for _, tc := range []struct {
		name        string
		benRinging  bool
		wantMissed  string
		wantJourney []string
	}{
		{
			name:       "waiting in silence",
			wantMissed: MissedAbandonedWaiting,
			wantJourney: []string{"QUEUE:sales:TIMEOUT", "AGENT:1009:did not answer",
				"AGENT:1009:did not answer", "QUEUE:support:58"},
		},
		{
			name:       "while ben's phone rang",
			benRinging: true,
			wantMissed: MissedAbandonedRinging,
			wantJourney: []string{"QUEUE:sales:TIMEOUT", "AGENT:1009:did not answer",
				"AGENT:1009:did not answer", "QUEUE:support:58", "AGENT:1007:did not answer"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			legs := []liveLeg{
				{channel: "caller", number: "+8613911114444", created: 0},
				{channel: "carol-1", number: "1009", agentID: &carol, created: 22},
				{channel: "carol-2", number: "1009", agentID: &carol, created: 40},
			}
			evs := []SwitchEvent{
				chAnswer("caller", 0),
				qJoin("sales", 20),
				chHangup("carol-1", "NO_ANSWER", 37),
				chHangup("carol-2", "NO_ANSWER", 55),
				qLeft("sales", 20, 80, "Timeout", "TIMEOUT"),
				qJoin("support", 81),
			}
			if tc.benRinging {
				legs = append(legs, liveLeg{channel: "ben", number: "1007", agentID: &ben, created: 130})
			}
			evs = append(evs, qLeft("support", 81, 140, "Cancel", ""))
			if tc.benRinging {
				evs = append(evs, chHangup("ben", "ORIGINATOR_CANCEL", 140))
			}
			evs = append(evs, chHangup("caller", "NORMAL_CLEARING", 140))
			snap := throughTheRegistry(t, legs, evs)

			cdr := newAssembler(&memoryLedger{}, staticQueues{"sales": sales, "support": support}).assemble(t.Context(), snap)
			if cdr.Status != store.CDRStatusNoAnswer {
				t.Errorf("status = %s, want NO_ANSWER", cdr.Status)
			}
			if cdr.MissedReason != tc.wantMissed {
				t.Errorf("missedReason = %q, want %q (the last stay; the first would say %q)",
					cdr.MissedReason, tc.wantMissed, MissedNoAvailableAgent)
			}
			if cdr.QueueID == nil || *cdr.QueueID != sales || cdr.QueueWaitSec != 59 {
				t.Errorf("row queue %v wait %d; want sales and its 59 s", cdr.QueueID, cdr.QueueWaitSec)
			}
			if got := journey(cdr.Legs); !slices.Equal(got, tc.wantJourney) {
				t.Errorf("journey = %v, want %v", got, tc.wantJourney)
			}
			noNegativeDuration(t, cdr)
		})
	}
}

// Wei put the caller back into the queue that had just answered them. That is
// a second wait in the same queue, not the first one continued: two stays.
// The switch's events arrive as they do live — the second join before the
// first stay's member-queue-end, and the second join announced twice — and
// carry the stamps they do live: the first stay's late end names the second
// it joined, which is the only thing that tells it from the second stay.
func TestARequeuedCallerHasTwoStaysInOneQueue(t *testing.T) {
	t.Parallel()
	wei, ben := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	support := uuid.Must(uuid.NewV7())

	snap := throughTheRegistry(t, []liveLeg{
		{channel: "caller", number: "+8613911115555", created: 0},
		{channel: "wei", number: "1008", agentID: &wei, created: 18},
		{channel: "ben", number: "1007", agentID: &ben, created: 105},
	}, []SwitchEvent{
		chAnswer("caller", 0),
		qJoin("support", 5),
		chAnswer("wei", 20),
		chBridge("wei", "caller", 20),
		qBridge("support", 5, 20),
		chUnbridge("caller", "wei", 80),
		chHangup("wei", "NORMAL_CLEARING", 80),
		qJoin("support", 81),
		qLeft("support", 5, 80, "Terminated", ""),
		qJoinAt("support", at(81).Add(joinLag+3*time.Millisecond)),
		chAnswer("ben", 110),
		chBridge("ben", "caller", 110),
		qBridge("support", 81, 110),
		qLeft("support", 81, 170, "Terminated", ""),
		chHangup("caller", "NORMAL_CLEARING", 170),
		chHangup("ben", "NORMAL_CLEARING", 170),
	})

	if n := len(snap.QueueStays); n != 2 {
		t.Fatalf("recorded %d stays, want 2: %+v", n, snap.QueueStays)
	}
	first, second := snap.QueueStays[0], snap.QueueStays[1]
	if !first.LeftAt.Equal(at(80)) || !first.BridgedAt.Equal(at(20)) {
		t.Errorf("first stay = %+v; the late member-queue-end belongs to it", first)
	}
	if !second.BridgedAt.Equal(at(110)) || !second.LeftAt.Equal(at(170)) {
		t.Errorf("second stay = %+v", second)
	}

	cdr := newAssembler(&memoryLedger{}, staticQueues{"support": support}).assemble(t.Context(), snap)
	want := []string{"QUEUE:support:14", "AGENT:1008:60", "QUEUE:support:28", "AGENT:1007:60"}
	if got := journey(cdr.Legs); !slices.Equal(got, want) {
		t.Errorf("journey = %v, want %v", got, want)
	}
	if cdr.QueueWaitSec != 14 {
		t.Errorf("queueWaitSec = %d, want the first stay's 14 s", cdr.QueueWaitSec)
	}
	noNegativeDuration(t, cdr)
}

// One queue, answered: the row and journey read exactly as they did before
// stays, and the same as a snapshot that carries only the current stay (the
// shape every older test builds).
func TestASingleQueueCallReadsAsItAlwaysDid(t *testing.T) {
	t.Parallel()
	wei := uuid.Must(uuid.NewV7())
	support := uuid.Must(uuid.NewV7())

	snap := throughTheRegistry(t, []liveLeg{
		{channel: "caller", number: "+8613911116666", created: 0},
		{channel: "wei", number: "1008", agentID: &wei, created: 12},
	}, []SwitchEvent{
		chAnswer("caller", 0),
		qJoin("support", 3),
		chAnswer("wei", 15),
		chBridge("wei", "caller", 15),
		qBridge("support", 3, 15),
		qLeft("support", 3, 75, "Terminated", ""),
		chHangup("caller", "NORMAL_CLEARING", 75),
		chHangup("wei", "NORMAL_CLEARING", 75),
	})

	asm := newAssembler(&memoryLedger{}, staticQueues{"support": support})
	cdr := asm.assemble(t.Context(), snap)
	want := []string{"QUEUE:support:11", "AGENT:1008:60"}
	if got := journey(cdr.Legs); !slices.Equal(got, want) {
		t.Errorf("journey = %v, want %v", got, want)
	}
	if cdr.QueueID == nil || *cdr.QueueID != support || cdr.QueueWaitSec != 11 {
		t.Errorf("row queue %v wait %d; want support and 11 s", cdr.QueueID, cdr.QueueWaitSec)
	}

	current := snap
	current.QueueStays = nil
	old := asm.assemble(t.Context(), current)
	if !slices.Equal(journey(old.Legs), journey(cdr.Legs)) || old.QueueWaitSec != cdr.QueueWaitSec ||
		old.Status != cdr.Status || old.MissedReason != cdr.MissedReason {
		t.Errorf("history %v/%d/%s, current stay only %v/%d/%s: the two readings differ",
			journey(cdr.Legs), cdr.QueueWaitSec, cdr.Status, journey(old.Legs), old.QueueWaitSec, old.Status)
	}
}

// Two halves of one conversation merged: each had been through a queue, and
// both had heard about one stay. Every stay survives, in join order, and the
// shared stay is one stay with what either half knew about it. The merge used
// to copy the absorbed half's queue only when the kept half had none.
//
// The halves heard the shared stay the way live events allow: the kept half
// from its member-queue-start (the event's microsecond clock), the absorbed
// half only from a later event (the switch's whole second). Those are the
// same join, by the identity the events themselves use.
func TestAMergedCallKeepsBothHalvesQueueStays(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, oneAgent{}, &capturingPublisher{})

	ctx := t.Context()
	minted := uuid.Must(uuid.NewV7())
	vars := map[string]string{"variable_aicc_call_id": minted.String()}

	c.Handle(ctx, raw("CHANNEL_CREATE", "caller-chan", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_CREATE", "agent-chan", "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	absorbed, ok := registry.CallForChannel("agent-chan")
	if !ok {
		t.Fatal("the agent's leg was never put on a call of its own")
	}

	// The kept half knows the support stay's bridge.
	if err := registry.Do(minted, func(call *Call) {
		call.JoinQueue("support", time.Time{}, at(71).Add(joinLag))
		call.QueueStay("support", at(71)).BridgedAt = at(100)
	}); err != nil {
		t.Fatal(err)
	}
	// The absorbed half went through sales first, and knows when the support
	// stay ended.
	if err := registry.Do(absorbed, func(call *Call) {
		call.JoinQueue("sales", time.Time{}, at(2).Add(joinLag))
		sales := call.QueueStay("sales", at(2))
		sales.BridgedAt, sales.LeftAt, sales.Cause = at(10), at(70), "Terminated"
		call.QueueStay("support", at(71)).LeftAt = at(160)
	}); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, raw("CHANNEL_BRIDGE", "agent-chan", "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": "caller-chan"})))
	waitFor(t, func() bool {
		snap, err := registry.Snapshot(minted)
		return err == nil && len(snap.QueueStays) >= 2
	})

	snap, err := registry.Snapshot(minted)
	if err != nil {
		t.Fatal(err)
	}
	want := []QueueFacts{
		{Name: "sales", JoinedAt: at(2).Add(joinLag), BridgedAt: at(10), LeftAt: at(70), Cause: "Terminated"},
		{Name: "support", JoinedAt: at(71).Add(joinLag), BridgedAt: at(100), LeftAt: at(160)},
	}
	if len(snap.QueueStays) != len(want) {
		t.Fatalf("stays = %+v, want %+v", snap.QueueStays, want)
	}
	for i := range want {
		got := snap.QueueStays[i]
		if got.Name != want[i].Name || !got.JoinedAt.Equal(want[i].JoinedAt) ||
			!got.BridgedAt.Equal(want[i].BridgedAt) || !got.LeftAt.Equal(want[i].LeftAt) ||
			got.Cause != want[i].Cause {
			t.Errorf("stay %d = %+v, want %+v", i, got, want[i])
		}
	}
	if snap.Queue.Name != "support" {
		t.Errorf("current stay = %+v, want support", snap.Queue)
	}
}

// The switch announcing one join twice on a single-queue call is one stay
// (TestAReannouncedMemberIsNotASecondCaller is the queue panel's side of
// it). member-queue-start carries no joined time, so the two announcements
// differ by the event clock alone; matching stays by that clock opened a
// second stay, and the call came out with two QUEUE legs, one of them 0 s.
func TestAReannouncedJoinIsOneStay(t *testing.T) {
	t.Parallel()
	wei := uuid.Must(uuid.NewV7())
	support := uuid.Must(uuid.NewV7())

	snap := throughTheRegistry(t, []liveLeg{
		{channel: "caller", number: "+8613911117777", created: 0},
		{channel: "wei", number: "1008", agentID: &wei, created: 12},
	}, []SwitchEvent{
		chAnswer("caller", 0),
		qJoin("support", 3),
		qJoinAt("support", at(3).Add(joinLag+800*time.Millisecond)),
		chAnswer("wei", 30),
		chBridge("wei", "caller", 30),
		qBridge("support", 3, 30),
		qLeft("support", 3, 75, "Terminated", ""),
		chHangup("caller", "NORMAL_CLEARING", 75),
		chHangup("wei", "NORMAL_CLEARING", 75),
	})

	if n := len(snap.QueueStays); n != 1 {
		t.Fatalf("recorded %d stays, want 1: %+v", n, snap.QueueStays)
	}
	if got := snap.QueueStays[0]; !got.JoinedAt.Equal(at(3).Add(joinLag)) {
		t.Errorf("stay joined at %s, want the first announcement's %s", got.JoinedAt.Sub(base), joinLag+3*time.Second)
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{"support": support}).assemble(t.Context(), snap)
	want := []string{"QUEUE:support:26", "AGENT:1008:45"}
	if got := journey(cdr.Legs); !slices.Equal(got, want) {
		t.Errorf("journey = %v, want %v", got, want)
	}
	if cdr.QueueWaitSec != 26 || cdr.Status != store.CDRStatusAnswered {
		t.Errorf("queueWaitSec %d status %s; want 26 s, ANSWERED", cdr.QueueWaitSec, cdr.Status)
	}
	noNegativeDuration(t, cdr)
}

// Put back into the queue that answered them, the caller gives up in the
// second stay, and the first stay's member-queue-end (Terminated) arrives
// after that. It names the second the first stay joined, so it closes the
// first stay; it used to land on the latest stay in that queue and overwrite
// the caller's abandonment with Terminated.
func TestALateEndOfAnEarlierStayLeavesTheLaterOneAlone(t *testing.T) {
	t.Parallel()
	wei := uuid.Must(uuid.NewV7())
	support := uuid.Must(uuid.NewV7())

	snap := throughTheRegistry(t, []liveLeg{
		{channel: "caller", number: "+8613911118888", created: 0},
		{channel: "wei", number: "1008", agentID: &wei, created: 18},
	}, []SwitchEvent{
		chAnswer("caller", 0),
		qJoin("support", 5),
		chAnswer("wei", 20),
		chBridge("wei", "caller", 20),
		qBridge("support", 5, 20),
		chUnbridge("caller", "wei", 80),
		chHangup("wei", "NORMAL_CLEARING", 80),
		qJoin("support", 81),
		qLeft("support", 81, 140, "Cancel", "BREAK_OUT"),
		qLeftAt("support", 5, msAt(140, 200), "Terminated", ""),
		chHangup("caller", "NORMAL_CLEARING", 141),
	})

	if n := len(snap.QueueStays); n != 2 {
		t.Fatalf("recorded %d stays, want 2: %+v", n, snap.QueueStays)
	}
	first, second := snap.QueueStays[0], snap.QueueStays[1]
	if first.Cause != "Terminated" || !first.LeftAt.Equal(at(140)) || !first.BridgedAt.Equal(at(20)) {
		t.Errorf("first stay = %+v; the late Terminated end is its", first)
	}
	if second.Cause != "Cancel" || second.CancelReason != "BREAK_OUT" || !second.LeftAt.Equal(at(140)) ||
		!second.BridgedAt.IsZero() {
		t.Errorf("second stay = %+v; want the caller's own BREAK_OUT at 140, never bridged", second)
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{"support": support}).assemble(t.Context(), snap)
	want := []string{"QUEUE:support:14", "AGENT:1008:60", "QUEUE:support:BREAK_OUT"}
	if got := journey(cdr.Legs); !slices.Equal(got, want) {
		t.Errorf("journey = %v, want %v", got, want)
	}
	noNegativeDuration(t, cdr)
}

// The live call of 2026-10-22 (01a16190): the bot handed the caller to
// support-en, an agent answered there five seconds later and transferred them
// to wt_queue, where they gave up. The switch's stamps are the live ones,
// relative to 11:03:30: support-en joined 47.323, bridged 52.54; wt_queue
// joined 04:08.60 a few hundredths before support-en's member-queue-end
// (Terminated, 08.65); wt_queue abandoned 04:56.39. The row is support-en
// with its 5 s wait, and both queues are in the journey.
func TestTheLiveTwoQueueCallKeepsBothQueues(t *testing.T) {
	t.Parallel()
	agent := uuid.Must(uuid.NewV7())
	supportEN, wtQueue := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	snap := throughTheRegistry(t, []liveLeg{
		{channel: "caller", number: "18688886666", created: 0},
		{channel: "agent", number: "1008", agentID: &agent, created: 17},
	}, []SwitchEvent{
		chAnswer("caller", 0),
		qJoinAt("support-en", msAt(17, 323)),
		{Kind: KindChannelAnswer, ChannelID: "agent", OccurredAt: msAt(22, 520)},
		{Kind: KindChannelBridge, ChannelID: "agent", OtherChannelID: "caller", OccurredAt: msAt(22, 540)},
		qBridgeAt("support-en", 17, msAt(22, 540)),
		{Kind: KindChannelUnbridge, ChannelID: "caller", OtherChannelID: "agent", OccurredAt: msAt(38, 590)},
		qJoinAt("wt_queue", msAt(38, 600)),
		{Kind: KindChannelHangup, ChannelID: "agent", HangupCause: "NORMAL_CLEARING", OccurredAt: msAt(38, 620)},
		qLeftAt("support-en", 17, msAt(38, 650), "Terminated", ""),
		qLeftAt("wt_queue", 38, msAt(86, 390), "Cancel", "BREAK_OUT"),
		{Kind: KindChannelHangup, ChannelID: "caller", HangupCause: "NORMAL_CLEARING", OccurredAt: msAt(86, 390)},
	})

	if n := len(snap.QueueStays); n != 2 {
		t.Fatalf("recorded %d stays, want 2: %+v", n, snap.QueueStays)
	}
	if got := snap.QueueStays[0]; got.Name != "support-en" || got.Cause != "Terminated" || !got.BridgedAt.Equal(msAt(22, 540)) {
		t.Errorf("first stay = %+v; want support-en, bridged at 22.54, Terminated", got)
	}
	if got := snap.QueueStays[1]; got.Name != "wt_queue" || got.CancelReason != "BREAK_OUT" || !got.BridgedAt.IsZero() {
		t.Errorf("second stay = %+v; want wt_queue, abandoned, never bridged", got)
	}

	cdr := newAssembler(&memoryLedger{}, staticQueues{"support-en": supportEN, "wt_queue": wtQueue}).assemble(t.Context(), snap)
	if cdr.QueueID == nil || *cdr.QueueID != supportEN || cdr.QueueWaitSec != 5 {
		t.Errorf("row queue %v wait %d; want support-en (%s) and 5 s", cdr.QueueID, cdr.QueueWaitSec, supportEN)
	}
	var queues []string
	for _, leg := range cdr.Legs {
		if leg.Kind == "QUEUE" {
			queues = append(queues, leg.Label)
		}
	}
	if !slices.Equal(queues, []string{"support-en", "wt_queue"}) {
		t.Errorf("QUEUE legs = %v (journey %v), want support-en then wt_queue", queues, journey(cdr.Legs))
	}
	if cdr.Status != store.CDRStatusAnswered {
		t.Errorf("status = %s, want ANSWERED", cdr.Status)
	}
	noNegativeDuration(t, cdr)
}

// The switch's joined seconds are not one reading: member-queue-end raised on
// the caller's own thread reports when the queue application started, the
// agent thread's events the members row written just after the start event.
// Either lands on the stay, a second either side of the start event's clock;
// a reading further out still belongs to the only stay it can mean, and a
// queue the call has no stay in opens one from the event.
func TestAQueueEventFindsItsStayAtTheSwitchsGranularity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		joined time.Time
	}{
		{"same second", at(10)},
		{"a second early", at(9)},
		{"a second late", at(11)},
		{"three seconds early, the only stay", at(7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			call := NewCall(uuid.Must(uuid.NewV7()), events.CallTypeInbound, at(0))
			call.JoinQueue("support", time.Time{}, at(10).Add(joinLag))
			call.QueueStay("support", tc.joined).BridgedAt = at(20)
			if n := len(call.QueueStays); n != 1 || !call.QueueStays[0].BridgedAt.Equal(at(20)) {
				t.Errorf("stays = %+v, want the one stay, bridged", call.QueueStays)
			}
		})
	}

	t.Run("two stays in one queue", func(t *testing.T) {
		t.Parallel()
		call := NewCall(uuid.Must(uuid.NewV7()), events.CallTypeInbound, at(0))
		call.JoinQueue("support", time.Time{}, at(10).Add(joinLag))
		call.QueueStay("support", at(10)).BridgedAt = at(20)
		call.JoinQueue("support", time.Time{}, at(60).Add(joinLag))
		call.QueueStay("support", at(9)).Cause = "Terminated"
		call.QueueStay("support", at(61)).Cause = "Cancel"
		if n := len(call.QueueStays); n != 2 ||
			call.QueueStays[0].Cause != "Terminated" || call.QueueStays[1].Cause != "Cancel" {
			t.Errorf("stays = %+v, want each event on its own stay", call.QueueStays)
		}
	})

	t.Run("a queue with no stay", func(t *testing.T) {
		t.Parallel()
		call := NewCall(uuid.Must(uuid.NewV7()), events.CallTypeInbound, at(0))
		call.JoinQueue("sales", time.Time{}, at(2).Add(joinLag))
		call.QueueStay("support", at(40)).BridgedAt = at(50)
		if n := len(call.QueueStays); n != 2 || call.QueueStays[1].Name != "support" ||
			!call.QueueStays[1].JoinedAt.Equal(at(40)) || !call.QueueStays[0].BridgedAt.IsZero() {
			t.Errorf("stays = %+v, want sales untouched and support opened at 40", call.QueueStays)
		}
	})
}
