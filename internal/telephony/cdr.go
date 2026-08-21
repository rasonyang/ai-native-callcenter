// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// CDRLedger is where finished calls and queue movements are written.
type CDRLedger interface {
	InsertCDR(ctx context.Context, cdr store.CDR) error
	InsertQueueEvent(ctx context.Context, occurredAt time.Time,
		callID *uuid.UUID, queueID uuid.UUID, event string, agentID *uuid.UUID, waitMs int) error
	InsertRecording(ctx context.Context, r store.Recording) (store.Recording, error)
	MarkRecorded(ctx context.Context, callID uuid.UUID) error
}

// RecordingStorage is where call audio lives; nil disables recording ingestion.
type RecordingStorage interface {
	Backend() string
	Bucket() string
	Ingest(ctx context.Context, key string) (int64, error)
}

// recordingFlushWait gives the switch time to close the file after the last
// leg hangs up. record_session flushes at hangup; a stat racing that flush
// reads a half-written size.
const recordingFlushWait = 2 * time.Second

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
	ledger  CDRLedger
	queues  QueueDirectory
	storage RecordingStorage
	log     *slog.Logger
	// flushWait is how long ingestion waits for the switch to close the file;
	// shortened in tests.
	flushWait time.Duration
}

// NewCDRAssembler builds one. storage may be nil when recordings are off.
func NewCDRAssembler(ledger CDRLedger, queues QueueDirectory, storage RecordingStorage, log *slog.Logger) *CDRAssembler {
	if log == nil {
		log = slog.Default()
	}
	return &CDRAssembler{ledger: ledger, queues: queues, storage: storage,
		log: log, flushWait: recordingFlushWait}
}

// CallFinished receives the final snapshot; safe to set as Registry.OnCallFinished.
func (a *CDRAssembler) CallFinished(snap Snapshot) {
	go func() {
		// The ledger follows the call. A call with a bot leg that was never
		// handed to a person is the bot's story to write — its session holds
		// the transcript, timings and containment this path cannot see. The
		// bot-share stamp is what marks a handover, and only then does this
		// path own the row.
		//
		// "Non-empty" was not that test. The dialplan exports the DID to the
		// leg it dials towards the bot, so a share was never empty and both
		// paths raced for every contained call, settled silently by whichever
		// insert lost the primary key.
		if !hasBotLeg(snap) || snap.Bot.HandedOver() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := a.ledger.InsertCDR(ctx, a.assemble(ctx, snap)); err != nil {
				a.log.Error("could not write the cdr", "callId", snap.CallID, "error", err)
			}
			cancel()
		}
		a.ingestRecording(snap)
	}()
}

// hasBotLeg reports whether the switch dialed this call towards the AI
// gateway at some point.
func hasBotLeg(snap Snapshot) bool {
	for _, p := range snap.Parties {
		if p.IsBotLeg {
			return true
		}
	}
	return false
}

// ingestRecording books the call's audio into the ledger, if any was made.
//
// Absence is normal — recording is per number and per queue — so a missing
// file is silence, not an error. This runs for every finished call, which is
// what makes it the one place recordings are booked regardless of whether the
// call was a bot's, a person's, or both in turn.
func (a *CDRAssembler) ingestRecording(snap Snapshot) {
	if a.storage == nil {
		return
	}
	time.Sleep(a.flushWait)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := recordingKeyFor(snap)
	size, err := a.storage.Ingest(ctx, key)
	if err != nil {
		// Absence is normal — recording is per number and per queue — but the
		// reason must be visible: a silent skip here cost a live debugging
		// session when every ingest was failing for a real cause.
		a.log.Info("no recording ingested", "callId", snap.CallID, "key", key, "reason", err)
		return
	}

	if _, err := a.ledger.InsertRecording(ctx, store.Recording{
		CallID:      snap.CallID,
		Backend:     a.storage.Backend(),
		Bucket:      a.storage.Bucket(),
		ObjectKey:   key,
		SizeBytes:   size,
		DurationSec: recording.DurationSec(size),
	}); err != nil {
		a.log.Error("could not book the recording", "callId", snap.CallID, "error", err)
		return
	}
	if err := a.ledger.MarkRecorded(ctx, snap.CallID); err != nil {
		a.log.Error("could not flag the cdr as recorded", "callId", snap.CallID, "error", err)
	}
	a.log.Info("recording booked", "callId", snap.CallID, "key", key, "sizeBytes", size)
}

// recordingKeyFor is the switch's naming contract: UTC date of call start,
// then the call id — exactly what the Lua templates into record_session.
func recordingKeyFor(snap Snapshot) string {
	return recording.Key(snap.CreatedAt, snap.CallID.String())
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

	// How long the caller was actually with the bot is the bot leg's own
	// bridge, which ends when they are transferred away — after the closing
	// sentence has finished playing. The channel variable the bot stamps is
	// written when it decides to transfer, several seconds earlier, and those
	// seconds were landing in no column at all.
	if bot := botLeg(snap); bot != nil {
		if bridged := BridgedSec([]*PartySnapshot{bot}, cdr.EndedAt); bridged > 0 {
			if drift := bridged - cdr.BotSec; drift > 2 || drift < -2 {
				a.log.Warn("the bot's stamped duration disagrees with its bridge",
					"callId", snap.CallID, "stampedSec", cdr.BotSec, "bridgedSec", bridged)
			}
			cdr.BotSec = bridged
		}
	}

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
		// One agent, however many times the queue dialled them. A delivery
		// the switch cancels and retries is the same person being tried
		// again, and every retry used to be appended here — a caller nobody
		// picked up left twenty-three copies of one agent on the row.
		if leg.AgentID != nil && !slices.Contains(cdr.AgentIDs, *leg.AgentID) {
			cdr.AgentIDs = append(cdr.AgentIDs, *leg.AgentID)
		}
		if leg.AnsweredAt != nil && answered == nil {
			answered = leg
		}
	}

	// A call the agent placed reads the other way round: their own leg is the
	// originator, so the call is theirs no matter who they reached, and
	// whether anybody picked up is decided on the leg dialled out — the
	// agent's own leg auto-answers in front of them and says nothing about
	// the person being called.
	isAgentPlaced := originator != nil && originator.AgentID != nil
	dialled := dialledLegs(snap)
	if isAgentPlaced {
		if !slices.Contains(cdr.AgentIDs, *originator.AgentID) {
			cdr.AgentIDs = append(cdr.AgentIDs, *originator.AgentID)
		}
		for _, leg := range dialled {
			if leg.AnsweredAt != nil && answered == nil {
				answered = leg
			}
		}
	}

	// The switch answering and a person answering are different facts and the
	// ledger keeps them apart. answered_at is when the call became billable,
	// which is a question about the leg facing whoever charges for it — so it
	// is set whenever that leg was answered, including on a call the bot
	// served and nobody took, which the carrier bills all the same.
	billed := billedLeg(snap, originator, dialled)
	switch {
	case billed != nil && billed.AnsweredAt != nil:
		cdr.AnsweredAt = *billed.AnsweredAt
		cdr.BillSec = max(0, int(cdr.EndedAt.Sub(*billed.AnsweredAt).Seconds()))
	case originator != nil && originator.AnsweredAt != nil:
		// Nobody charges for two extensions talking, but the call was still
		// answered and the row should say when.
		cdr.AnsweredAt = *originator.AnsweredAt
	}

	// Whether a *person* was reached is a question the bridge answers and the
	// answer does not: an auto-answer phone picks up in front of nobody, and a
	// leg whose codec cannot meet the caller's returns a clean 200 with no
	// media at all. Only the agent legs' own stretches count — the bot's sit
	// on the bot's leg, and an agent's leg is never bridged to it.
	talking := talkingLegs(snap, agentLegs, isAgentPlaced, dialled)
	agentTalkSec := BridgedSec(talking, cdr.EndedAt)
	firstBridge := FirstBridgeAt(talking)

	// Once the caller joins a queue or a leg goes out towards an agent, they
	// are waiting for a person, and nobody arriving is not an answered call
	// however long the bot spoke first. Reading the bot's own answer as the
	// call's hid every abandoned queue call here, because every inbound call
	// meets the bot first.
	soughtAPerson := len(agentLegs) > 0 || !snap.Queue.JoinedAt.IsZero()

	switch {
	case !firstBridge.IsZero():
		cdr.Status = store.CDRStatusAnswered
		cdr.TalkSec = agentTalkSec
		if reached := firstBridgedLeg(talking); reached != nil {
			cdr.PrimaryAgentID = reached.AgentID
			cdr.RingSec = int(firstBridge.Sub(reached.CreatedAt).Seconds())
		}
		if cdr.PrimaryAgentID == nil && isAgentPlaced {
			cdr.PrimaryAgentID = originator.AgentID
		}
		if cdr.RingSec < 0 {
			cdr.RingSec = 0
		}

	case !isAgentPlaced && !soughtAPerson &&
		(snap.Bot.Sec > 0 || (originator != nil && originator.AnsweredAt != nil)):
		// The bot answered and the call stayed with it, or the call never
		// sought a person at all.
		cdr.Status = store.CDRStatusAnswered

	default:
		cdr.Status = store.CDRStatusNoAnswer
		cdr.MissedReason = a.missedReason(snap, agentLegs)
		cdr.RingSec = ringSpan(agentLegs)
	}

	if isAgentPlaced {
		cdr.Legs = buildLegs(snap, originator, append(slices.Clone(agentLegs), dialled...), cdr.BotSec)
	} else {
		cdr.Legs = buildLegs(snap, originator, agentLegs, cdr.BotSec)
	}
	if snap.Bot.Summary != "" {
		if cdr.UserData == nil {
			cdr.UserData = map[string]any{}
		}
		cdr.UserData["botSummary"] = snap.Bot.Summary
		if snap.Bot.Reason != "" {
			cdr.UserData["botReason"] = snap.Bot.Reason
		}
	}
	a.checkDurations(snap, &cdr)

	cdr.Tech = map[string]any{}
	if originator != nil {
		cdr.Tech["callerChannelId"] = originator.ChannelID
		// The switch counted the same seconds independently. Carrying its
		// figure alongside ours is what turns the billing number from
		// something asserted into something checkable — a charge nobody can
		// check is one nobody can defend either.
		//
		// A second of disagreement is ordinary and means nothing: we truncate
		// where the switch rounds, so 103.57 seconds is our 103 and its 104.
		// Beyond that the two are counting different things and somebody
		// should know which.
		if originator.BilledSec > 0 {
			cdr.Tech["switchBillSec"] = originator.BilledSec
			if drift := cdr.BillSec - originator.BilledSec; drift > billDriftTolerance || drift < -billDriftTolerance {
				a.log.Warn("the ledger and the switch disagree on billable time",
					"callId", snap.CallID, "billSec", cdr.BillSec,
					"switchBillSec", originator.BilledSec)
			}
		}
	}
	return cdr
}

// botLeg is the leg the switch dialled towards the AI gateway, or nil on a
// call that never met a bot.
func botLeg(snap Snapshot) *PartySnapshot {
	for i := range snap.Parties {
		if snap.Parties[i].IsBotLeg {
			return &snap.Parties[i]
		}
	}
	return nil
}

// checkDurations says so when the row's own numbers cannot all be true.
//
// The five durations are each derived separately and nothing used to compare
// them, which is how a bill longer than the call it was for reached the ledger
// and stayed there: arithmetically impossible, and silent. This does not
// correct anything — a number quietly adjusted to look consistent is worse
// than one that is visibly wrong — it reports, and puts what it saw where a
// later reader can find it.
func (a *CDRAssembler) checkDurations(snap Snapshot, cdr *store.CDR) {
	var wrong []string
	if cdr.BillSec > cdr.TotalSec {
		wrong = append(wrong, "billed for longer than the call lasted")
	}
	if cdr.TalkSec > cdr.TotalSec {
		wrong = append(wrong, "talked for longer than the call lasted")
	}
	if cdr.BotSec+cdr.QueueWaitSec+cdr.TalkSec > cdr.TotalSec {
		wrong = append(wrong, "the phases add up to more than the call")
	}
	if len(wrong) == 0 {
		return
	}
	a.log.Warn("the call's durations disagree with each other",
		"callId", snap.CallID, "callType", string(snap.CallType),
		"problems", strings.Join(wrong, "; "),
		"botSec", cdr.BotSec, "queueWaitSec", cdr.QueueWaitSec,
		"ringSec", cdr.RingSec, "talkSec", cdr.TalkSec,
		"billSec", cdr.BillSec, "totalSec", cdr.TotalSec)
}

// billedLeg is the leg the money is on: the one facing whoever charges for the
// call.
//
// On an inbound call that is the caller's own — the switch answered it and the
// carrier has been charging since, whatever happened afterwards. On a call an
// agent placed it is the leg dialled out, because the agent's own leg
// auto-answers in front of them and nobody bills for that; anchoring on it
// produced a bill longer than the call itself. Between two extensions nobody
// bills at all.
func billedLeg(snap Snapshot, originator *PartySnapshot, dialled []*PartySnapshot) *PartySnapshot {
	switch snap.CallType {
	case events.CallTypeInternal:
		return nil
	case events.CallTypeOutbound:
		for _, leg := range dialled {
			if leg.AnsweredAt != nil {
				return leg
			}
		}
		return nil
	default:
		return originator
	}
}

// talkingLegs are the legs whose bridges count as a person on the call: the
// agent legs, plus — on a call an agent placed — the leg dialled out to
// whoever they were calling, since the agent's own leg auto-answers in front
// of them and says nothing about the person being reached.
//
// The bot's leg is deliberately absent. Its bridge is the caller's time with
// the bot, accounted for as the bot's share, and an agent's leg is never
// bridged to it.
func talkingLegs(snap Snapshot, agentLegs []*PartySnapshot, isAgentPlaced bool, dialled []*PartySnapshot) []*PartySnapshot {
	if !isAgentPlaced {
		return agentLegs
	}
	return append(slices.Clone(agentLegs), dialled...)
}

// firstBridgedLeg is the leg that first carried a conversation, which is the
// one whose ring time the caller actually waited through.
func firstBridgedLeg(parties []*PartySnapshot) *PartySnapshot {
	var best *PartySnapshot
	var at time.Time
	for _, p := range parties {
		for _, b := range p.Bridges {
			if at.IsZero() || b.StartedAt.Before(at) {
				at, best = b.StartedAt, p
			}
		}
	}
	return best
}

// ringSpan is how long a call nobody answered spent ringing people: from the
// first leg dialled towards an agent to the last one released. A queue that
// re-offers dials a fresh leg every time, so no single leg holds the answer —
// the span is what the caller sat through. Only for calls that went
// unanswered; once somebody picks up, the ring that counts is theirs.
func ringSpan(agentLegs []*PartySnapshot) int {
	var first, last time.Time
	for _, leg := range agentLegs {
		if first.IsZero() || leg.CreatedAt.Before(first) {
			first = leg.CreatedAt
		}
		end := leg.CreatedAt
		if leg.ReleasedAt != nil {
			end = *leg.ReleasedAt
		}
		if end.After(last) {
			last = end
		}
	}
	if first.IsZero() || !last.After(first) {
		return 0
	}
	return int(last.Sub(first).Seconds())
}

// billDriftTolerance is how far the ledger and the switch may differ on
// billable seconds before it is worth saying so. One second is arithmetic — we
// truncate, the switch rounds — and measured drift on live calls has stayed
// inside it.
const billDriftTolerance = 2

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

// buildLegs writes the journey for the detail view, in order. botSec is the
// reconciled figure rather than the raw stamp, so the journey and the row's
// own bot_sec cannot disagree with each other.
func buildLegs(snap Snapshot, originator *PartySnapshot, agentLegs []*PartySnapshot, botSec int) []store.Leg {
	var legs []store.Leg
	if botSec > 0 || snap.Bot.FlowID != nil {
		legs = append(legs, store.Leg{Kind: "BOT", DurationSec: botSec})
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
		// A leg belonging to an agent is theirs; one the agent dialled out
		// of the building went through a carrier.
		kind := "AGENT"
		if agent.AgentID == nil && snap.CallType == events.CallTypeOutbound {
			kind = "TRUNK"
		}
		leg := store.Leg{Kind: kind, Label: agent.Number}
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

// dialledLegs are the legs a call reached out to that are nobody's agent:
// the person an agent called. On an inbound call there are none — the bot's
// leg is the switch's own and is accounted for as the bot's share.
func dialledLegs(snap Snapshot) []*PartySnapshot {
	var out []*PartySnapshot
	for i := range snap.Parties {
		p := &snap.Parties[i]
		if p.Role == RoleOriginator || p.AgentID != nil || p.IsBotLeg {
			continue
		}
		out = append(out, p)
	}
	return out
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
