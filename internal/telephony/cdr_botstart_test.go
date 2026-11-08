// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// flowsByDID is a queue directory that also knows the flow of each number.
type flowsByDID struct {
	staticQueues
	flows map[string]*uuid.UUID
	err   error
	asked int
}

func (d *flowsByDID) FlowIDByDID(_ context.Context, number string) (*uuid.UUID, error) {
	d.asked++
	return d.flows[number], d.err
}

// finishWith runs a snapshot through CallFinished with the given directory and
// returns the rows this path wrote.
func finishWith(t *testing.T, dir QueueDirectory, snap Snapshot) []store.CDR {
	t.Helper()
	ledger := &memoryLedger{}
	storage := finishedStorage{reached: make(chan struct{})}
	a := NewCDRAssembler(ledger, dir, storage, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.flushWait = 0
	a.CallFinished(snap)
	select {
	case <-storage.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("CallFinished never finished deciding")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return slices.Clone(ledger.cdrs)
}

// A bot leg that answered and never started its session has no writer on the
// bot's side: this path writes the row, and names the failure (#334).
func TestABotThatAnsweredAndNeverStartedIsWrittenByThisPath(t *testing.T) {
	t.Parallel()
	flow := uuid.Must(uuid.NewV7())
	stamped := uuid.Must(uuid.NewV7())
	agentID := uuid.Must(uuid.NewV7())

	answeredBot := func(end int) PartySnapshot {
		return PartySnapshot{
			Role: RoleTarget, Number: "95001", ChannelID: "bot", IsBotLeg: true,
			CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(end),
			Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(1), EndedAt: at(end)}},
		}
	}
	inbound := func(end int, did string, parties ...PartySnapshot) Snapshot {
		return Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeInbound,
			CreatedAt: at(0), EndedAt: atPtr(end),
			Bot: BotShare{DID: did},
			Parties: append([]PartySnapshot{{
				Role: RoleOriginator, Number: "13800138000", ChannelID: "caller",
				CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(end),
				ReleaseCause: "NORMAL_CLEARING",
			}}, parties...),
		}
	}
	agentLeg := func(answer, end int) PartySnapshot {
		return PartySnapshot{
			Role: RoleTarget, Number: "1007", AgentID: &agentID, ChannelID: "agent",
			CreatedAt: at(answer - 2), AnsweredAt: atPtr(answer), ReleasedAt: atPtr(end),
			Bridges: []BridgeSpan{{OtherChannelID: "caller", StartedAt: at(answer), EndedAt: at(end)}},
		}
	}
	queued := func(s Snapshot, joined, left int) Snapshot {
		s.QueueStays = []QueueFacts{{Name: "support", JoinedAt: at(joined), LeftAt: at(left)}}
		return s
	}
	outbound := func(end int, bot BotShare, parties ...PartySnapshot) Snapshot {
		return Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(end), Bot: bot,
			Parties: append([]PartySnapshot{{
				Role: RoleOriginator, Number: "18688886666", ChannelID: "caller",
				CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(end),
				ReleaseCause: "NORMAL_CLEARING",
			}}, parties...),
		}
	}

	rescued := queued(inbound(60, "95001", answeredBot(10), agentLeg(14, 60)), 8, 14)
	rescued.QueueStays[0].BridgedAt = at(14)
	abandoned := queued(inbound(26, "95001", answeredBot(10)), 10, 26)
	abandoned.QueueStays[0].Cause = "Cancel"

	outRescued := queued(outbound(60, BotShare{DID: "95099", FlowID: &flow, Reason: BotLegFailed},
		PartySnapshot{
			Role: RoleTarget, Number: "95099", ChannelID: "bot", IsBotLeg: true,
			CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(10),
		}, agentLeg(14, 60)), 8, 14)
	outRescued.QueueStays[0].BridgedAt = at(14)

	outBot := PartySnapshot{
		Role: RoleTarget, Number: "95099", ChannelID: "bot", IsBotLeg: true,
		CreatedAt: at(0), AnsweredAt: atPtr(1), ReleasedAt: atPtr(10),
	}

	type want struct {
		status, cause, reason string
		legs                  []string
		talk                  int
		wait                  int
		missed                bool
		flow                  *uuid.UUID
		lookups               int
	}
	for _, tc := range []struct {
		name string
		dir  *flowsByDID
		snap Snapshot
		want want
	}{
		{
			name: "inbound, rescued to a queue and an agent talked",
			dir:  &flowsByDID{flows: map[string]*uuid.UUID{"95001": &flow}},
			snap: rescued,
			want: want{status: store.CDRStatusAnswered, cause: "NORMAL_CLEARING", reason: BotStartFailed,
				legs: []string{"BOT:", "QUEUE:support:6", "AGENT:1007:46"}, talk: 46, wait: 6,
				flow: &flow, lookups: 1},
		},
		{
			name: "inbound, abandoned in the queue",
			dir:  &flowsByDID{flows: map[string]*uuid.UUID{"95001": &flow}},
			snap: abandoned,
			want: want{status: store.CDRStatusNoAnswer, cause: "NORMAL_CLEARING", reason: BotStartFailed,
				legs: []string{"BOT:", "QUEUE:support:16"}, wait: 16, missed: true, flow: &flow, lookups: 1},
		},
		{
			name: "inbound, no queue, the caller hangs up on the prompt",
			dir:  &flowsByDID{flows: map[string]*uuid.UUID{"95001": &flow}},
			snap: inbound(20, "95001", answeredBot(20)),
			want: want{status: store.CDRStatusAnswered, cause: BotStartFailed,
				legs: []string{"BOT:"}, flow: &flow, lookups: 1},
		},
		{
			name: "inbound, the number has no flow",
			dir:  &flowsByDID{flows: map[string]*uuid.UUID{}},
			snap: inbound(20, "95001", answeredBot(20)),
			want: want{status: store.CDRStatusAnswered, cause: BotStartFailed,
				legs: []string{"BOT:"}, lookups: 1},
		},
		{
			name: "inbound, the lookup fails",
			dir:  &flowsByDID{err: errors.New("db down")},
			snap: inbound(20, "95001", answeredBot(20)),
			want: want{status: store.CDRStatusAnswered, cause: BotStartFailed,
				legs: []string{"BOT:"}, lookups: 1},
		},
		{
			name: "a stamped flow wins and nothing is looked up",
			dir:  &flowsByDID{flows: map[string]*uuid.UUID{"95001": &flow}},
			snap: func() Snapshot {
				s := inbound(20, "95001", answeredBot(20))
				s.Bot.FlowID = &stamped
				return s
			}(),
			want: want{status: store.CDRStatusAnswered, cause: BotStartFailed,
				legs: []string{"BOT:"}, flow: &stamped},
		},
		{
			name: "AI outbound rescued to a queue keeps the stamped reason",
			dir:  &flowsByDID{},
			snap: outRescued,
			want: want{status: store.CDRStatusAnswered, cause: "NORMAL_CLEARING", reason: BotLegFailed,
				legs: []string{"BOT:", "QUEUE:support:6", "AGENT:1007:46"}, talk: 46, wait: 6, flow: &flow},
		},
		{
			name: "AI outbound not rescued: the prompt or a campaign hangup",
			dir:  &flowsByDID{},
			snap: outbound(10, BotShare{DID: "95099", FlowID: &flow, Reason: BotLegFailed}, outBot),
			want: want{status: store.CDRStatusAnswered, cause: BotStartFailed, reason: BotLegFailed,
				legs: []string{"BOT:"}, flow: &flow},
		},
		{
			name: "AI outbound whose ACK never reached the bot: no stamp at all",
			dir:  &flowsByDID{},
			snap: outbound(10, BotShare{DID: "95099", FlowID: &flow}, outBot),
			want: want{status: store.CDRStatusAnswered, cause: BotStartFailed,
				legs: []string{"BOT:"}, flow: &flow},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := finishWith(t, tc.dir, tc.snap)
			if len(rows) != 1 {
				t.Fatalf("wrote %d rows, want exactly one", len(rows))
			}
			got, w := rows[0], tc.want
			if got.Status != w.status {
				t.Errorf("status = %s, want %s", got.Status, w.status)
			}
			if got.HangupCause != w.cause {
				t.Errorf("hangupCause = %q, want %q", got.HangupCause, w.cause)
			}
			if r, _ := got.UserData["botReason"].(string); r != w.reason {
				t.Errorf("userData.botReason = %q, want %q", r, w.reason)
			}
			if j := journey(got.Legs); len(j) != len(w.legs) || !prefixesMatch(j, w.legs) {
				t.Errorf("legs = %v, want %v", j, w.legs)
			}
			if got.TalkSec != w.talk || got.QueueWaitSec != w.wait {
				t.Errorf("talkSec=%d queueWaitSec=%d, want %d and %d", got.TalkSec, got.QueueWaitSec, w.talk, w.wait)
			}
			if (got.MissedReason != "") != w.missed {
				t.Errorf("missedReason = %q, want one: %v", got.MissedReason, w.missed)
			}
			if (got.FlowID == nil) != (w.flow == nil) || (w.flow != nil && *got.FlowID != *w.flow) {
				t.Errorf("flowId = %v, want %v", got.FlowID, w.flow)
			}
			if tc.dir.asked != w.lookups {
				t.Errorf("flow lookups = %d, want %d", tc.dir.asked, w.lookups)
			}
		})
	}
}

// prefixesMatch compares journey hops by the prefix each expectation names, so
// a hop's exact seconds only matter where the expectation states them.
func prefixesMatch(got, want []string) bool {
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			return false
		}
	}
	return true
}

// The controls: every call whose row is not this rule's keeps exactly the
// writer it had.
func TestTheBotStartRuleLeavesOtherCallsAlone(t *testing.T) {
	t.Parallel()
	agentID := uuid.Must(uuid.NewV7())
	flow := uuid.Must(uuid.NewV7())
	bot := func(answered bool) PartySnapshot {
		p := PartySnapshot{
			Role: RoleTarget, Number: "95001", ChannelID: "bot", IsBotLeg: true,
			CreatedAt: at(0), ReleasedAt: atPtr(30), ReleaseCause: "NORMAL_CLEARING",
		}
		if answered {
			p.AnsweredAt = atPtr(1)
		} else {
			p.ReleaseCause = "USER_BUSY"
		}
		return p
	}
	caller := PartySnapshot{
		Role: RoleOriginator, Number: "13800138000", ChannelID: "caller",
		CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(30), ReleaseCause: "NORMAL_CLEARING",
	}
	agentCall := func(share BotShare) Snapshot {
		return Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeOutbound,
			CreatedAt: at(0), EndedAt: atPtr(30), Bot: share,
			Parties: []PartySnapshot{{
				Role: RoleOriginator, Number: "1007", ExtensionNumber: "1007", AgentID: &agentID,
				ChannelID: "agent", CreatedAt: at(0), AnsweredAt: atPtr(0), ReleasedAt: atPtr(30),
				ReleaseCause: "NORMAL_CLEARING",
			}, bot(true)},
		}
	}
	inbound := func(share BotShare, b PartySnapshot) Snapshot {
		return Snapshot{
			CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeInbound,
			CreatedAt: at(0), EndedAt: atPtr(30), Bot: share,
			Parties: []PartySnapshot{caller, b},
		}
	}
	abandoned := inbound(BotShare{DID: "95001"}, bot(true))
	abandoned.IsBotAbandoned = true

	for _, tc := range []struct {
		name  string
		snap  Snapshot
		rows  int
		cause string // when a row is written
	}{
		{"a bot that never answered", inbound(BotShare{DID: "95001"}, bot(false)), 1, "NORMAL_CLEARING"},
		{"a contained call with the marker", inbound(BotShare{DID: "95001", IsSessionStarted: true}, bot(true)), 0, ""},
		{"a transfer with the marker and the tally",
			inbound(BotShare{DID: "95001", Sec: 12, IsStamped: true, IsSessionStarted: true, FlowID: &flow}, bot(true)),
			1, "NORMAL_CLEARING"},
		{"an agent's call to a bot number, no marker", agentCall(BotShare{DID: "95001"}), 1, "NORMAL_CLEARING"},
		{"an agent's call to a bot number, with the marker",
			agentCall(BotShare{DID: "95001", Sec: 20, IsStamped: true, IsSessionStarted: true}), 1, "NORMAL_CLEARING"},
		{"a bot whose session died with the process", abandoned, 1, "NORMAL_CLEARING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := &flowsByDID{flows: map[string]*uuid.UUID{"95001": &flow}}
			rows := finishWith(t, dir, tc.snap)
			if len(rows) != tc.rows {
				t.Fatalf("wrote %d rows, want %d", len(rows), tc.rows)
			}
			if dir.asked != 0 {
				t.Errorf("looked up a flow %d times: only a bot that never started needs one", dir.asked)
			}
			if tc.rows == 1 {
				if rows[0].HangupCause != tc.cause {
					t.Errorf("hangupCause = %q, want %q", rows[0].HangupCause, tc.cause)
				}
				if r := rows[0].UserData["botReason"]; r == BotStartFailed {
					t.Errorf("botReason = %v: this call's bot did not fail to start", r)
				}
			}
		})
	}
}

// The marker is read from an event and from a one-at-a-time lookup alike, and
// a merge carries it from the leg that has it.
func TestTheSessionMarkerIsReadAndMerged(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"aicc_bot_started": "true", "aicc_did": "95001"}
	got := botShareFrom(func(name string) string { return vars[name] })
	if !got.IsSessionStarted || got.IsStamped {
		t.Errorf("share = %+v, want a started session that handed nothing over", got)
	}
	if botShareFrom(func(string) string { return "" }).IsSessionStarted {
		t.Error("an absent marker read as a started session")
	}
	var into BotShare
	into.Merge(got)
	if !into.IsSessionStarted {
		t.Error("Merge dropped the marker")
	}
	into.Merge(BotShare{})
	if !into.IsSessionStarted {
		t.Error("Merge cleared the marker")
	}
}
