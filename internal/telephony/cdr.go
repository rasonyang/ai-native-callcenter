// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// CDRLedger is where finished calls and queue movements are written.
type CDRLedger interface {
	InsertCDR(ctx context.Context, cdr store.CDR) error
	InsertQueueEvent(ctx context.Context, occurredAt time.Time,
		callID *uuid.UUID, queueID uuid.UUID, event string, agentID *uuid.UUID, waitMs int) error
}

// QueueDirectory resolves a queue's name to its identity. The switch speaks in
// names; the ledger speaks in ids.
type QueueDirectory interface {
	QueueIDByName(ctx context.Context, name string) (uuid.UUID, bool)
}

// shortAbandonThreshold separates a caller who gave the queue a real chance
// from one who dialled and thought better of it.
const shortAbandonThreshold = 5 * time.Second

// CDRAssembler turns finished calls into ledger rows.
//
// It hangs off the registry's finish hook and the coordinator's queue events.
// Writes happen on their own goroutine: the finish hook runs on the call's
// actor, which must never wait on a database.
type CDRAssembler struct {
	ledger CDRLedger
	queues QueueDirectory
	log    *slog.Logger
}

// NewCDRAssembler builds one.
func NewCDRAssembler(ledger CDRLedger, queues QueueDirectory, log *slog.Logger) *CDRAssembler {
	if log == nil {
		log = slog.Default()
	}
	return &CDRAssembler{ledger: ledger, queues: queues, log: log}
}

// CallFinished receives the final snapshot; safe to set as Registry.OnCallFinished.
func (a *CDRAssembler) CallFinished(snap Snapshot) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.ledger.InsertCDR(ctx, a.assemble(ctx, snap)); err != nil {
			a.log.Error("could not write the cdr", "callId", snap.CallID, "error", err)
		}
	}()
}

// assemble derives the ledger row from recorded facts — never from parsing
// cause strings.
func (a *CDRAssembler) assemble(ctx context.Context, snap Snapshot) store.CDR {
	originator, agentLegs := split(snap)

	cdr := store.CDR{
		CallID:    snap.CallID,
		CallType:  string(snap.CallType),
		Language:  snap.Language,
		StartedAt: snap.CreatedAt,
		QueueID:   snap.QueueID,
		UserData:  snap.UserData,
		DID:       snap.Bot.DID,
		FlowID:    snap.Bot.FlowID,
		BotSec:    snap.Bot.Sec,
	}
	if snap.EndedAt != nil {
		cdr.EndedAt = *snap.EndedAt
	} else {
		cdr.EndedAt = time.Now()
	}
	cdr.TotalSec = int(cdr.EndedAt.Sub(cdr.StartedAt).Seconds())

	if originator != nil {
		cdr.FromNumber = originator.Number
		cdr.HangupCause = originator.ReleaseCause
		if cdr.DID == "" {
			cdr.ToNumber = originator.OtherNumber
		} else {
			cdr.ToNumber = cdr.DID
		}
	}

	// The queue's identity and timings.
	queueName := snap.Queue.Name
	if queueName == "" {
		queueName = snap.Bot.Queue
	}
	if cdr.QueueID == nil && queueName != "" && a.queues != nil {
		if id, ok := a.queues.QueueIDByName(ctx, queueName); ok {
			cdr.QueueID = &id
		}
	}
	if !snap.Queue.JoinedAt.IsZero() {
		waitEnd := snap.Queue.BridgedAt
		if waitEnd.IsZero() {
			waitEnd = snap.Queue.LeftAt
		}
		if !waitEnd.IsZero() {
			cdr.QueueWaitSec = int(waitEnd.Sub(snap.Queue.JoinedAt).Seconds())
		}
	}

	// The people involved, and how long they talked.
	var answered *PartySnapshot
	for _, leg := range agentLegs {
		if leg.AgentID != nil {
			cdr.AgentIDs = append(cdr.AgentIDs, *leg.AgentID)
		}
		if leg.AnsweredAt != nil && answered == nil {
			answered = leg
		}
	}
	if answered != nil {
		cdr.Status = store.CDRStatusAnswered
		cdr.AnsweredAt = *answered.AnsweredAt
		cdr.PrimaryAgentID = answered.AgentID
		cdr.RingSec = int(answered.AnsweredAt.Sub(answered.CreatedAt).Seconds())
		talkEnd := cdr.EndedAt
		if answered.ReleasedAt != nil {
			talkEnd = *answered.ReleasedAt
		}
		cdr.TalkSec = int(talkEnd.Sub(*answered.AnsweredAt).Seconds())
	} else if snap.Bot.Sec > 0 || (originator != nil && originator.AnsweredAt != nil && len(agentLegs) == 0 && snap.Queue.JoinedAt.IsZero()) {
		// The bot answered, or the call never sought a person at all.
		cdr.Status = store.CDRStatusAnswered
		if originator != nil && originator.AnsweredAt != nil {
			cdr.AnsweredAt = *originator.AnsweredAt
		}
	} else {
		cdr.Status = store.CDRStatusNoAnswer
		cdr.MissedReason = a.missedReason(snap, agentLegs)
	}

	cdr.Legs = buildLegs(snap, originator, agentLegs)
	if snap.Bot.Summary != "" {
		if cdr.UserData == nil {
			cdr.UserData = map[string]any{}
		}
		cdr.UserData["botSummary"] = snap.Bot.Summary
		if snap.Bot.Reason != "" {
			cdr.UserData["botReason"] = snap.Bot.Reason
		}
	}
	cdr.Tech = map[string]any{}
	if originator != nil {
		cdr.Tech["callerChannelId"] = originator.ChannelID
	}
	return cdr
}

// missedReason follows the design's precedence: the caller's own phase first.
func (a *CDRAssembler) missedReason(snap Snapshot, agentLegs []*PartySnapshot) string {
	queue := snap.Queue

	// The caller abandoned while an agent's phone was ringing.
	for _, leg := range agentLegs {
		if leg.AnsweredAt == nil && leg.ReleasedAt != nil && !queue.BridgedAt.IsZero() {
			return "ABANDONED_RINGING"
		}
	}

	if !queue.JoinedAt.IsZero() && queue.BridgedAt.IsZero() {
		wait := queue.LeftAt.Sub(queue.JoinedAt)
		switch {
		case queue.Cause == "Timeout":
			// The queue gave up on the caller, not the reverse.
			return "NO_AVAILABLE_AGENT"
		case wait >= 0 && wait < shortAbandonThreshold:
			return "SHORT_ABANDONED"
		default:
			return "ABANDONED_WAITING"
		}
	}
	if len(agentLegs) > 0 {
		return "AGENTS_DID_NOT_ANSWER"
	}
	return ""
}

// buildLegs writes the journey for the detail view, in order.
func buildLegs(snap Snapshot, originator *PartySnapshot, agentLegs []*PartySnapshot) []store.Leg {
	var legs []store.Leg
	if snap.Bot.Sec > 0 || snap.Bot.FlowID != nil {
		legs = append(legs, store.Leg{Kind: "BOT", DurationSec: snap.Bot.Sec})
	}
	if !snap.Queue.JoinedAt.IsZero() {
		leg := store.Leg{Kind: "QUEUE", Label: snap.Queue.Name}
		end := snap.Queue.BridgedAt
		if end.IsZero() {
			end = snap.Queue.LeftAt
		}
		if !end.IsZero() {
			leg.DurationSec = int(end.Sub(snap.Queue.JoinedAt).Seconds())
		}
		if snap.Queue.CancelReason != "" {
			leg.Note = snap.Queue.CancelReason
		}
		legs = append(legs, leg)
	}
	for _, agent := range agentLegs {
		leg := store.Leg{Kind: "AGENT", Label: agent.Number}
		if agent.AnsweredAt != nil {
			end := snap.CreatedAt
			if agent.ReleasedAt != nil {
				end = *agent.ReleasedAt
			} else if snap.EndedAt != nil {
				end = *snap.EndedAt
			}
			leg.DurationSec = int(end.Sub(*agent.AnsweredAt).Seconds())
		} else {
			leg.Note = "did not answer"
		}
		legs = append(legs, leg)
	}
	if legs == nil && originator != nil {
		legs = append(legs, store.Leg{Kind: "DIALING", Label: originator.Number})
	}
	return legs
}

// split separates the caller's leg from the agents'.
func split(snap Snapshot) (originator *PartySnapshot, agentLegs []*PartySnapshot) {
	for i := range snap.Parties {
		p := &snap.Parties[i]
		if p.Role == RoleOriginator {
			originator = p
			continue
		}
		if p.AgentID != nil {
			agentLegs = append(agentLegs, p)
		}
	}
	return originator, agentLegs
}

// QueueEvent records one member movement into the ledger. The coordinator
// calls it for every callcenter fact worth reporting on.
func (a *CDRAssembler) QueueEvent(ctx context.Context, ev SwitchEvent, callID *uuid.UUID, agentID *uuid.UUID) {
	if a.queues == nil {
		return
	}
	queueID, ok := a.queues.QueueIDByName(ctx, ev.Queue)
	if !ok {
		return
	}

	var name string
	waitMs := 0
	switch ev.Kind {
	case KindQueueMemberJoined:
		name = store.QueueEventJoined
	case KindQueueAgentOffered:
		name = store.QueueEventOffered
	case KindQueueBridgeStart:
		name = store.QueueEventBridged
		if !ev.JoinedAt.IsZero() {
			waitMs = int(ev.OccurredAt.Sub(ev.JoinedAt).Milliseconds())
		}
	case KindQueueMemberLeft:
		// A member who left without being bridged abandoned or timed out;
		// one who was bridged simply finished.
		if ev.Cause == "Cancel" {
			name = store.QueueEventAbandoned
		} else {
			name = store.QueueEventLeft
		}
		if !ev.JoinedAt.IsZero() && !ev.LeftAt.IsZero() {
			waitMs = int(ev.LeftAt.Sub(ev.JoinedAt).Milliseconds())
		}
	default:
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.ledger.InsertQueueEvent(ctx, ev.OccurredAt, callID, queueID,
			name, agentID, waitMs); err != nil {
			a.log.Error("could not write the queue event",
				"queue", ev.Queue, "event", name, "error", err)
		}
	}()
}

// Interface conformance documented where it is relied on.
