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

// The whole vocabulary of missed_reason: every value a call can be given, and
// no others. The reason is derived from what the call recorded, so this list
// is decided here and nowhere else — the contract's MissedReason enum, the
// cdrs CHECK and the console's two translations all follow it, and
// TestEveryMissedReasonIsOneTheContractNames holds them together.
const (
	MissedShortAbandoned     = "SHORT_ABANDONED"
	MissedAbandonedRinging   = "ABANDONED_RINGING"
	MissedAbandonedWaiting   = "ABANDONED_WAITING"
	MissedAgentsDidNotAnswer = "AGENTS_DID_NOT_ANSWER"
	MissedNoAvailableAgent   = "NO_AVAILABLE_AGENT"
)

// MissedReasons is that vocabulary in full, for the tests that check nothing
// else can express it.
var MissedReasons = []string{
	MissedShortAbandoned, MissedAbandonedRinging, MissedAbandonedWaiting,
	MissedAgentsDidNotAnswer, MissedNoAvailableAgent,
}

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
	// offer, when set, is told about every recording once it is booked. Set
	// before the first call finishes; never changed afterwards.
	offer RecordingOffer
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

// RecordingOffer is told that a call's recording is booked, so it can decide
// whether to do more with it (post-call transcription). It must return at
// once: it is called on the call's retirement goroutine.
type RecordingOffer interface {
	OfferRecording(rec store.Recording, hadBotLeg, hadHumanLeg bool)
}

// SetRecordingOffer attaches the recipient of booked recordings. Nil, the
// default, offers them to nobody.
func (a *CDRAssembler) SetRecordingOffer(o RecordingOffer) { a.offer = o }

// CallFinished receives the final snapshot; safe to set as Registry.OnCallFinished.
func (a *CDRAssembler) CallFinished(snap Snapshot) {
	go func() {
		// The ledger follows the call. A call the bot answered and never
		// handed to a person is the bot's story to write — its session holds
		// the transcript, timings and containment this path cannot see. The
		// bot-share stamp is what marks a handover, and only then does this
		// path own the row. The one other case is a call an agent's phone
		// placed, which is the agent's whoever ended it: that is the leg
		// saying so (isAgentPlaced), never a caller id that resembles an
		// extension, so a carrier call from "1008" stays the bot's.
		//
		// "Non-empty" was not that test. The dialplan exports the DID to the
		// leg it dials towards the bot, so a share was never empty and both
		// paths raced for every contained call, settled silently by whichever
		// insert lost the primary key.
		//
		// One more owner: a bot whose session died with the process (a call
		// recovery adopted and ended) has no one left to write the row.
		//
		// And a bot that never answered was never a writer at all. The switch
		// dials a bot leg before the bot has said anything, and a leg dialled
		// is not a session: the bot answers (200 OK) only once it has admitted
		// the call, and the conversation that writes the row starts on the
		// ACK. A customer who hangs up between an AI outbound call's
		// uuid_transfer and that answer, a bridge the switch makes after its
		// reservation was withdrawn (refused 480), an inbound call refused at
		// capacity (486) or dropped by the peer list, then sent to the
		// fallback queue or told the call cannot be completed: asked "was a bot
		// leg dialled", each of these left the row to a bot that never existed,
		// and the call reached the ledger from neither side (#313). The test is
		// the one the journey and the status ask (botJoined), so a row written
		// here because the bot was not there says so, and a row it leaves to
		// the bot is one the bot's session writes.
		originator, _ := split(snap)
		if !botJoined(snap, snap.Bot.Sec) || snap.Bot.HandedOver() || isAgentPlaced(originator) || snap.IsBotAbandoned {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := a.ledger.InsertCDR(ctx, a.assemble(ctx, snap)); err != nil {
				a.log.Error("could not write the cdr", "callId", snap.CallID, "error", err)
			}
			cancel()
		}
		a.ingestRecording(snap)
	}()
}

// isAgentPlaced reports whether an agent's phone placed the call, which is not
// the same as whether the originator is an agent we can name. They coincided
// until a system could place a call for an agent who never signed into this
// application: presence then has nothing to say about them, the originator
// carries no agent id, and the call was recorded with talk_sec zero and a
// DIALING leg at the agent's own extension instead of the trunk it actually
// reached — every third-party call reading as no conversation at all
// (measured live 2026-08-26, 86 answered seconds recorded as 0). The
// extension is the fact that survives the logout.
//
// The extension, and only the extension: it is what the originating leg itself
// said (aicc_extension, stamped by our own originate and by the directory for
// an authenticated phone), so a carrier leg never has one. Presence is not a
// witness here. A trunk call whose caller id equals a signed-in agent's
// extension is matched to that agent for the screen pop and carries their id,
// and reading the id as "this agent placed it" made the assembler and the bot
// ledger both write the same customer's call, the winner decided by timing
// (#105).
func isAgentPlaced(originator *PartySnapshot) bool {
	return originator != nil && originator.ExtensionNumber != ""
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

	booked, err := a.ledger.InsertRecording(ctx, store.Recording{
		CallID:      snap.CallID,
		Backend:     a.storage.Backend(),
		Bucket:      a.storage.Bucket(),
		ObjectKey:   key,
		SizeBytes:   size,
		DurationSec: recording.DurationSec(size),
	})
	if err != nil {
		a.log.Error("could not book the recording", "callId", snap.CallID, "error", err)
		return
	}
	if err := a.ledger.MarkRecorded(ctx, snap.CallID); err != nil {
		a.log.Error("could not flag the cdr as recorded", "callId", snap.CallID, "error", err)
	}
	a.log.Info("recording booked", "callId", snap.CallID, "key", key, "sizeBytes", size)
	if a.offer != nil {
		hadBot, hadHuman := answeredSides(snap)
		a.offer.OfferRecording(booked, hadBot, hadHuman)
	}
}

// answeredSides reports who answered on the call's far end: a bot, a person,
// or both. It is what lets post-call transcription tell whether the live
// transcript covers everybody who spoke.
func answeredSides(snap Snapshot) (bot, human bool) {
	for _, p := range snap.Parties {
		if p.AnsweredAt == nil {
			continue
		}
		if p.IsBotLeg {
			bot = true
		} else if p.AgentID != nil {
			human = true
		}
	}
	return bot, human
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
	//
	// Only a stamped figure is compared. An agent who hangs up on a bot they
	// dialled leaves it no moment to stamp one, and a 0 against the bridge
	// warned on every such call with nothing wrong.
	if bot := botLeg(snap); bot != nil {
		if bridged := BridgedSec([]*PartySnapshot{bot}, cdr.EndedAt); bridged > 0 {
			if drift := bridged - cdr.BotSec; snap.Bot.IsStamped && (drift > 2 || drift < -2) {
				a.log.Warn("the bot's stamped duration disagrees with its bridge",
					"callId", snap.CallID, "stampedSec", cdr.BotSec, "bridgedSec", bridged)
			}
			cdr.BotSec = bridged
		}
	}

	// A call the agent placed reads the other way round: their own leg is the
	// originator, so the call is theirs no matter who they reached, and
	// whether anybody picked up is decided on the leg dialled out — the
	// agent's own leg auto-answers in front of them and says nothing about
	// the person being called. When what they reached was a bot, there is no
	// leg dialled out and the bot's answer is the call's.
	placed := isAgentPlaced(originator)
	handedOver := placed && placerHandedOver(snap, originator, cdr.EndedAt)

	if originator != nil {
		cdr.FromNumber = originator.Number
		cdr.HangupCause = originator.ReleaseCause
		if handedOver {
			// The placer's leg ended at the hand-over, so its cause says how
			// the transfer went, not how the call did (#282).
			cdr.HangupCause = endingCause(snap, originator)
		}
		switch {
		case cdr.DID == "":
			cdr.ToNumber = originator.OtherNumber
		case snap.CallType == events.CallTypeOutbound && !placed:
			// A DID on a call this platform placed is the number the call
			// went out from, not one anybody dialled — and the leg the
			// registry calls the originator is the customer's, because we
			// created it. Both ends were landing in the other's column.
			// An agent's call to a DID is OUTBOUND too and carries the DID
			// from the bot leg, but there the originator is the agent and
			// the DID is what they dialled (#80).
			cdr.FromNumber, cdr.ToNumber = cdr.DID, originator.Number
		default:
			cdr.ToNumber = cdr.DID
		}
	}

	// The queue's identity and timings: the first queue the caller joined,
	// and that stay's wait (owner decision on #283). The row has one queue
	// and the reports group by it, so a call that passed through two queues
	// counts once, under the queue it first reached; the journey lists every
	// stay. Each stay's wait is its own: the first queue's bridge, never a
	// later queue's join, which is how a two-queue call used to come out
	// with a negative wait. A call the bot handed to a queue the caller never
	// joined (gone before the queue took them in) has no stay and is counted
	// under the bot's queue, with no wait; the contract's CDR.queueId says so.
	stays := queueStays(snap)
	var first QueueFacts
	if len(stays) > 0 {
		first = stays[0]
	}
	queueName := first.Name
	if queueName == "" {
		queueName = snap.Bot.Queue
	}
	if cdr.QueueID == nil && queueName != "" && a.queues != nil {
		if id, ok := a.queues.QueueIDByName(ctx, queueName); ok {
			cdr.QueueID = &id
		}
	}
	if len(stays) > 0 {
		cdr.QueueWaitSec = stayWaitSec(first)
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

	dialled := dialledLegs(snap)
	if placed {
		// Only when there is one to name. An agent's phone placing a call and
		// an agent being on it are the same event only while they are signed
		// in; attribution stays presence's answer to give.
		if originator.AgentID != nil && !slices.Contains(cdr.AgentIDs, *originator.AgentID) {
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
	billed := billedLeg(snap, originator, dialled, placed)
	switch {
	case billed != nil && billed.AnsweredAt != nil:
		cdr.AnsweredAt = *billed.AnsweredAt
		cdr.BillSec = max(0, int(cdr.EndedAt.Sub(*billed.AnsweredAt).Seconds()))
	case placed:
		// On an agent-placed call with no billed leg, the originator's answer
		// is only the phone's auto-answer in front of the agent. Use the
		// earliest answer of a leg other than the originator (the callee extension
		// or the bot); if nobody on the far side answered, leave answeredAt unset.
		var earliest time.Time
		for i := range snap.Parties {
			p := &snap.Parties[i]
			if p.Role == RoleOriginator || p.AnsweredAt == nil {
				continue
			}
			if earliest.IsZero() || p.AnsweredAt.Before(earliest) {
				earliest = *p.AnsweredAt
			}
		}
		if !earliest.IsZero() {
			cdr.AnsweredAt = earliest
		}
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
	talking := talkingLegs(snap, agentLegs, placed, dialled)
	agentTalkSec := BridgedSec(talking, cdr.EndedAt)
	firstBridge := FirstBridgeAt(talking)

	// Once the caller joins a queue or a leg goes out towards an agent, they
	// are waiting for a person, and nobody arriving is not an answered call
	// however long the bot spoke first. Reading the bot's own answer as the
	// call's hid every abandoned queue call here, because every inbound call
	// meets the bot first.
	//
	// Any stay will do: a caller who joined a queue at any point was waiting
	// for a person from then on.
	soughtAPerson := len(agentLegs) > 0 || len(stays) > 0
	metBot := botJoined(snap, cdr.BotSec)

	switch {
	case !firstBridge.IsZero():
		cdr.Status = store.CDRStatusAnswered
		cdr.TalkSec = agentTalkSec
		if reached := firstBridgedLeg(talking); reached != nil {
			cdr.PrimaryAgentID = reached.AgentID
			cdr.RingSec = int(firstBridge.Sub(reached.CreatedAt).Seconds())
		}
		if cdr.PrimaryAgentID == nil && placed {
			cdr.PrimaryAgentID = originator.AgentID
		}
		if cdr.RingSec < 0 {
			cdr.RingSec = 0
		}

	case !soughtAPerson && placed && metBot:
		// An agent dialled a bot number and the bot answered them. The call
		// is the agent's, so they are its primary agent, but no person was
		// reached: talk_sec stays 0 (the bot's time is bot_sec) and the call
		// is not contained, because containment counts customers the bot
		// resolved.
		cdr.Status = store.CDRStatusAnswered
		cdr.PrimaryAgentID = originator.AgentID

	case !placed && !soughtAPerson &&
		(snap.Bot.Sec > 0 || (originator != nil && originator.AnsweredAt != nil)):
		// The bot answered and the call stayed with it, or the call never
		// sought a person at all.
		cdr.Status = store.CDRStatusAnswered

	default:
		cdr.Status = store.CDRStatusNoAnswer
		if calleeWasBusy(snap, originator, dialled, placed) {
			cdr.Status = store.CDRStatusBusy
		}
		cdr.MissedReason = a.missedReason(stays, agentLegs)
		cdr.RingSec = ringSpan(agentLegs)
	}

	if placed {
		cdr.Legs = buildLegs(snap, stays, originator, append(slices.Clone(agentLegs), dialled...), cdr.BotSec,
			placerIsAHop(dialled, metBot, handedOver), cdr.EndedAt)
	} else {
		cdr.Legs = buildLegs(snap, stays, originator, agentLegs, cdr.BotSec, false, cdr.EndedAt)
	}
	if cdr.Legs == nil && originator != nil {
		// Whether anybody answered is the status's answer, not the
		// originator's: an agent's phone auto-answers in front of them on
		// every call it places, whether or not the far end ever comes up.
		cdr.Legs = unreachedJourney(snap, originator, cdr.Status == store.CDRStatusAnswered, cdr.EndedAt)
	}
	cdr.UnbackedClaims = snap.Bot.UnbackedClaims
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
		//
		// Against the leg we billed, not against the caller's. Those are the
		// same leg on an inbound call and on one this platform placed, and a
		// different one whenever an agent dialled out: their own phone
		// auto-answers in front of them, so the switch bills their leg for the
		// whole time the far end rang, while we bill the leg facing the
		// carrier — nothing, if nobody picked up. Comparing our figure for one
		// leg against the switch's for another made this warning fire on every
		// unanswered click-to-dial, with the ledger right every time (C54).
		//
		// It was the third alarm in a week that also rings when nothing is
		// wrong, and this one had already done the damage the pattern
		// threatens: C49 — every AI outbound billed at zero — announced itself
		// in this exact line for two days and went unread, because the same
		// line was firing on calls that were fine.
		//
		// Comparing like with like keeps that alarm rather than muting it: on
		// an AI outbound the billed leg *is* the originator, so C49 would still
		// have been caught here. Where nobody is billed at all — two
		// extensions talking — there is no claim to check.
		if billed != nil && billed.BilledSec > 0 {
			cdr.Tech["switchBillSec"] = billed.BilledSec
			if drift := cdr.BillSec - billed.BilledSec; drift > billDriftTolerance || drift < -billDriftTolerance {
				a.log.Warn("the ledger and the switch disagree on billable time",
					"callId", snap.CallID, "billSec", cdr.BillSec,
					"switchBillSec", billed.BilledSec)
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

// botJoined reports whether the bot was on the call: its leg answered, it has
// time on the call, or it handed the call on, which it can only do from a
// conversation. botSec is the reconciled figure (assemble).
//
// A bot leg the switch dialled is not enough. On an AI outbound call the
// customer can hang up between the uuid_transfer that creates the bot's leg
// and the bot's answer, and on an agent's call to a DID the bot can refuse it
// or the agent can give up first; read as "the bot was here", the first came
// out as a 0 s BOT hop with no trunk and the second as an answered call. Nor
// is a known flow: the outbound originator stamps the DID's flow on the
// customer leg from its first moment (#313).
func botJoined(snap Snapshot, botSec int) bool {
	if bot := botLeg(snap); bot != nil && bot.AnsweredAt != nil {
		return true
	}
	return botSec > 0 || snap.Bot.HandedOver()
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
func billedLeg(snap Snapshot, originator *PartySnapshot, dialled []*PartySnapshot, isAgentPlaced bool) *PartySnapshot {
	switch snap.CallType {
	case events.CallTypeInternal:
		return nil
	case events.CallTypeOutbound:
		// Which leg faces the carrier depends on who placed the call. On a
		// call this platform placed, it *is* the originator — we created that
		// leg towards the trunk. An agent dialling out sits on the originator
		// themselves, so theirs is the one dialled, and their own phone
		// auto-answering in front of them is not the call becoming billable.
		if !isAgentPlaced {
			return originator
		}
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

// queueStays is every queue stay the call made, in the order joined, leaving
// out a stay whose join was never seen: with no start there is no wait to
// measure and no place in the journey, which is how the single record was
// read too. A snapshot built with only the current stay (Snapshot.Queue) and
// no history is read as that one stay.
func queueStays(snap Snapshot) []QueueFacts {
	stays := snap.QueueStays
	if len(stays) == 0 {
		stays = []QueueFacts{snap.Queue}
	}
	var out []QueueFacts
	for _, s := range stays {
		if !s.JoinedAt.IsZero() {
			out = append(out, s)
		}
	}
	return out
}

// stayWaitSec is how long the caller waited in one stay: from joining to the
// bridge, or to leaving when nobody was reached. A stay with neither (the
// call ended before the queue said so) has no wait to report.
func stayWaitSec(stay QueueFacts) int {
	end := stay.BridgedAt
	if end.IsZero() {
		end = stay.LeftAt
	}
	if end.IsZero() {
		return 0
	}
	return int(end.Sub(stay.JoinedAt).Seconds())
}

// billDriftTolerance is how far the ledger and the switch may differ on
// billable seconds before it is worth saying so. One second is arithmetic — we
// truncate, the switch rounds — and measured drift on live calls has stayed
// inside it.
const billDriftTolerance = 2

// missedReason follows the design's precedence: the caller's own phase first.
//
// The phase is the one the caller was in when the call ended, so on a call
// that passed through several queues it is the last stay (#283): a caller the
// first queue timed out on and who then gave up waiting in the second
// abandoned the call; the first queue's timeout is how they got to the
// second, not how the call ended. Every stay is unanswered here — this is only
// asked of a call nobody answered.
func (a *CDRAssembler) missedReason(stays []QueueFacts, agentLegs []*PartySnapshot) string {
	var queue QueueFacts
	if len(stays) > 0 {
		queue = stays[len(stays)-1]
	}

	// Only ever asked about a call nobody answered, so a queue that bridged
	// did not stay bridged, and the phase the caller was in when they gave up
	// is what the reason names.
	//
	// A phone was ringing when they went. The condition used to require that
	// the queue *had* bridged, which is the opposite of what the sentence
	// above it says and made the branch unreachable for the case it describes:
	// a caller who hangs up mid-ring never got as far as a bridge. Left that
	// way, every one of them was filed under AGENTS_DID_NOT_ANSWER — the
	// agent's fault rather than the caller's choice, on the very report a
	// supervisor reads to tell those apart.
	if queue.BridgedAt.IsZero() && isCallerGone(queue) {
		for _, leg := range agentLegs {
			// Ringing *at the moment they went*, which is not the same as
			// having rung at some point: a caller who sat through
			// twenty-three attempts and then waited another two minutes in
			// silence abandoned the wait, not a ring. The ring that was cut
			// short by the hangup is the one that ends no earlier than the
			// caller's own departure.
			if leg.AnsweredAt == nil && leg.ReleasedAt != nil &&
				!leg.ReleasedAt.Before(queue.LeftAt) {
				return MissedAbandonedRinging
			}
		}
	}

	if !queue.JoinedAt.IsZero() && queue.BridgedAt.IsZero() {
		wait := queue.LeftAt.Sub(queue.JoinedAt)
		switch {
		case queue.Cause == "Timeout":
			// The queue gave up on the caller, not the reverse.
			return MissedNoAvailableAgent
		case !isCallerGone(queue):
			// Still queued, or removed for a reason that was not theirs, and
			// phones did ring: the agents are what went wrong here.
			if len(agentLegs) > 0 {
				return MissedAgentsDidNotAnswer
			}
			return MissedNoAvailableAgent
		case wait >= 0 && wait < shortAbandonThreshold:
			return MissedShortAbandoned
		default:
			return MissedAbandonedWaiting
		}
	}
	if len(agentLegs) > 0 {
		return MissedAgentsDidNotAnswer
	}
	// Nothing, and that is the answer rather than the absence of one (owner
	// directive 2026-08-25).
	//
	// This vocabulary describes one journey: an inbound caller who wanted a
	// person. A call that never queued and never rang an agent was never on
	// it — an outbound call this platform placed, a call between two
	// extensions, an inbound call to a number nobody serves, a caller who hung
	// up on a direct extension before it was picked up. Of the 171 such rows
	// in the live ledger, 148 are outbound or internal.
	//
	// They did not go unserved; they did not go. Why they ended is the hangup
	// cause, which says it exactly — USER_BUSY, NO_ANSWER, ORIGINATOR_CANCEL,
	// UNALLOCATED_NUMBER — and inventing a missedReason for them would put a
	// second, vaguer answer beside a precise one. Naming the caller who
	// abandoned a direct extension would be worse still: SHORT_ABANDONED and
	// ABANDONED_WAITING already mean "the caller left", and a vocabulary with
	// two ways to say one thing is the defect DEVICE_IN_SERVICE was.
	return ""
}

// isCallerGone reports whether the caller is the one who ended it, in the
// queue's own vocabulary: Cancel is the caller leaving, Timeout is the queue
// giving up on them. Anything else is read as not the caller's doing — the
// safer way round, because blaming a caller who did not hang up is the error a
// supervisor cannot see in the report.
func isCallerGone(queue QueueFacts) bool {
	return strings.EqualFold(strings.TrimSpace(queue.Cause), "Cancel")
}

// placerIsAHop reports whether the agent who placed the call belongs in its
// journey as well as in its fromNumber.
//
// Ordinarily they do not. The row already names them, and the journey of a
// call they stayed on to the end is the person they reached: one TRUNK leg,
// or one DIALING leg if nobody was. Two shapes need them named all the same.
// A call they placed to a bot has no leg dialled out, so nothing else on the
// row says a person was on it. And a call they handed on is a conversation
// carried by two people: the placer's stretch with the customer came first,
// and leaving it out drew a journey that started with the colleague who took
// over (#267) — while the same transfer on an inbound call names both agents.
//
// handedOver is placerHandedOver's answer, which assemble already has.
func placerIsAHop(dialled []*PartySnapshot, metBot, handedOver bool) bool {
	return (metBot && len(dialled) == 0) || handedOver
}

// placerHandedOver reports whether the agent who placed the call passed it on
// and left before it ended.
//
// Leaving early is not enough on its own. The call ends when its last leg
// does, so whichever side hangs up first leaves a few milliseconds before the
// end — an agent who says goodbye and hangs up would read as a transfer on
// every call they ended themselves. What a transfer adds is somebody new:
// the call went on to a queue, or a leg was dialled, after the placer's own
// conversation had begun. The legs the placer dialled to reach the customer
// were all created before that, so they never count.
//
// Any queue stay joined after the placer's conversation began is somebody
// new: the placer passed the customer to a queue. With one record per call
// that was the latest join, so asking of every stay is the same question.
func placerHandedOver(snap Snapshot, originator *PartySnapshot, end time.Time) bool {
	if originator.ReleasedAt == nil || !originator.ReleasedAt.Before(end) {
		return false
	}
	talked := FirstBridgeAt([]*PartySnapshot{originator})
	if talked.IsZero() {
		// Nobody was reached, so there was nothing to hand over.
		return false
	}
	for _, stay := range queueStays(snap) {
		if stay.JoinedAt.After(talked) {
			return true
		}
	}
	for i := range snap.Parties {
		if p := &snap.Parties[i]; p.Role != RoleOriginator && p.CreatedAt.After(talked) {
			return true
		}
	}
	return false
}

// teardownWindow is how soon after one leg of a bridge releases the switch
// takes its partner down. Propagation is milliseconds; a second is generous
// and still far shorter than any wait a caller sits through on their own.
const teardownWindow = time.Second

// endingCause is the hangup cause of a call whose placer handed it on: the
// cause of the leg whose release ended the call (#282). The placer's own leg
// ended at the hand-over, and its NORMAL_CLEARING read the same whether the
// customer gave up while the colleague's phone rang, the onward number was
// busy or the colleague hung up.
//
// The customer's leg is the one the call is about after the hand-over, so it
// decides: its cause, when it released last or released first and the rest
// followed it down. When a leg that could take the customer down released
// just before them, that leg's release is what did — the colleague hanging
// up, the onward number refusing — and the cause is that leg's
// (couldEndCustomer). Only when the customer's leg cannot be found, or the
// call outlived it, is the answer the last leg to release.
//
// Equal timestamps are the customer's: nothing says the other leg went
// first.
func endingCause(snap Snapshot, originator *PartySnapshot) string {
	var last *PartySnapshot
	for i := range snap.Parties {
		p := &snap.Parties[i]
		if p == originator || p.ReleasedAt == nil {
			continue
		}
		if last == nil || p.ReleasedAt.After(*last.ReleasedAt) {
			last = p
		}
	}
	if last == nil {
		return originator.ReleaseCause
	}
	customer := customerLeg(snap, originator)
	if customer == nil || customer.ReleasedAt == nil {
		return last.ReleaseCause
	}
	gone := *customer.ReleasedAt

	// The last leg able to take the customer down that released before they
	// did, if it went close enough before to have done it.
	var before *PartySnapshot
	for i := range snap.Parties {
		p := &snap.Parties[i]
		if p == originator || p == customer || p.ReleasedAt == nil || !p.ReleasedAt.Before(gone) {
			continue
		}
		if !couldEndCustomer(snap, originator, customer, p) {
			continue
		}
		if before == nil || p.ReleasedAt.After(*before.ReleasedAt) {
			before = p
		}
	}
	if before != nil && gone.Sub(*before.ReleasedAt) <= teardownWindow {
		return before.ReleaseCause
	}
	// Released last, or released first and the rest followed it down.
	if !last.ReleasedAt.After(gone.Add(teardownWindow)) {
		return customer.ReleaseCause
	}
	return last.ReleaseCause
}

// couldEndCustomer reports whether leg p's release can be what took the
// customer's leg down. Only two kinds of leg can:
//
//   - a leg bridged with the customer when it released: the colleague who
//     answered and hung up, or whose phone dropped. The switch tears down the
//     partner of a bridge that ends with a hangup.
//   - the leg the hand-over was dialling straight at: created at the hand-over
//     (no earlier than the placer's release, less the teardown window) and
//     not while the customer was waiting in a queue. Its refusal ends the
//     transfer's bridge application, and the customer with it: an extension
//     rejecting the transfer (CALL_REJECTED, live 01a161a2), an outside
//     number answering USER_BUSY.
//
// A queue's offer is neither. A phone the queue rang that was not answered
// (NO_ANSWER), or lost a ring-all race (LOSE_RACE), leaves the customer
// waiting in the queue; it releasing a moment before the customer gave up is
// a coincidence, not a cause.
func couldEndCustomer(snap Snapshot, originator, customer, p *PartySnapshot) bool {
	released := *p.ReleasedAt
	if bridgedAt(p, customer.ChannelID, released) || bridgedAt(customer, p.ChannelID, released) {
		return true
	}
	if originator.ReleasedAt == nil ||
		p.CreatedAt.Before(originator.ReleasedAt.Add(-teardownWindow)) {
		return false
	}
	for _, stay := range queueStays(snap) {
		if !p.CreatedAt.Before(stay.JoinedAt) && (stay.LeftAt.IsZero() || !p.CreatedAt.After(stay.LeftAt)) {
			return false // offered by the queue the customer was waiting in
		}
	}
	return true
}

// bridgedAt reports whether leg p was bridged to the channel other at moment
// t: a stretch that began by then and had not ended more than the teardown
// window before it. A hangup closes a leg's own stretch at its release, but
// the switch can report the unbridge a few milliseconds ahead of the hangup.
func bridgedAt(p *PartySnapshot, other string, t time.Time) bool {
	if other == "" {
		return false
	}
	for _, b := range p.Bridges {
		if b.OtherChannelID != other || b.StartedAt.After(t) {
			continue
		}
		if b.EndedAt.IsZero() || !b.EndedAt.Before(t.Add(-teardownWindow)) {
			return true
		}
	}
	return false
}

// customerLeg is the leg the placer was talking to: the other end of their
// first bridge. A placer whose bridge names no leg on the call falls back to
// the earliest leg dialled before that bridge began, which is the one they
// dialled to reach the customer.
func customerLeg(snap Snapshot, originator *PartySnapshot) *PartySnapshot {
	var first *BridgeSpan
	for i := range originator.Bridges {
		if b := &originator.Bridges[i]; first == nil || b.StartedAt.Before(first.StartedAt) {
			first = b
		}
	}
	if first == nil {
		return nil
	}
	for i := range snap.Parties {
		if p := &snap.Parties[i]; p != originator && p.ChannelID != "" && p.ChannelID == first.OtherChannelID {
			return p
		}
	}
	var earliest *PartySnapshot
	for _, p := range dialledLegs(snap) {
		if p.CreatedAt.After(first.StartedAt) {
			continue
		}
		if earliest == nil || p.CreatedAt.Before(earliest.CreatedAt) {
			earliest = p
		}
	}
	return earliest
}

// placerLeg is the hop of the agent who placed the call: their own time with
// whoever was on the other side. That is their bridges, not their leg's
// answer — click-to-dial rings the agent's phone first and it auto-answers in
// front of them, so answered-to-released would count the far end's ringing as
// the agent's. A leg the switch never bridged falls back to that span, which
// is all there is.
func placerLeg(originator *PartySnapshot, end time.Time) store.Leg {
	leg := store.Leg{Kind: "AGENT", Label: originator.Number}
	if originator.ReleasedAt != nil {
		end = *originator.ReleasedAt
	}
	if bridged := BridgedSec([]*PartySnapshot{originator}, end); bridged > 0 {
		leg.DurationSec = bridged
	} else if originator.AnsweredAt != nil {
		leg.DurationSec = max(0, int(end.Sub(*originator.AnsweredAt).Seconds()))
	}
	return leg
}

// joinedAt is when a leg joined the conversation, which is what the journey
// is ordered by: its first bridge, since answering is not being heard; else
// its answer; else when it was dialled. A leg that knows none of these is
// placed at the call's start rather than before it.
func joinedAt(snap Snapshot, p *PartySnapshot) time.Time {
	at := FirstBridgeAt([]*PartySnapshot{p})
	if at.IsZero() && p.AnsweredAt != nil {
		at = *p.AnsweredAt
	}
	if at.IsZero() {
		at = p.CreatedAt
	}
	if at.IsZero() {
		at = snap.CreatedAt
	}
	return at
}

// buildLegs writes the journey for the detail view, in the order its legs
// joined the conversation. botSec is the reconciled figure rather than the raw
// stamp, so the journey and the row's own bot_sec cannot disagree with each
// other. withPlacer adds the hop of the agent who placed the call
// (placerIsAHop).
//
// In order of time, not of kind. The list used to be bot, queue, then the
// parties in the order they were handed in, which is the order of time on an
// inbound call that goes bot → queue → agent and on nothing else: an agent's
// call they transferred to a colleague listed the colleague before the
// customer the agent had dialled (#267), and an agent who passed a caller to
// a queue appeared after it.
//
// One QUEUE leg per stay (#283), each with its own wait and its own cancel
// reason: a caller answered in one queue and transferred to another passed
// through both, and the journey says so.
func buildLegs(snap Snapshot, stays []QueueFacts, originator *PartySnapshot, parties []*PartySnapshot, botSec int, withPlacer bool, end time.Time) []store.Leg {
	type hop struct {
		leg    store.Leg
		joined time.Time
	}
	var hops []hop
	if withPlacer {
		// The call is theirs from its first moment: they placed it.
		hops = append(hops, hop{placerLeg(originator, end), snap.CreatedAt})
	}
	if botJoined(snap, botSec) {
		// Labelled with the flow, as the bot labels the row it writes itself.
		// Unlabelled, the same flow read one way on a contained call and
		// another on a transferred one.
		//
		// Only a bot that joined is a hop (botJoined): neither the flow the
		// call was placed for nor a bot leg nobody answered says the journey
		// reached it, and either would give an answered call that never did a
		// BOT hop of 0 s and no trunk.
		joined := snap.CreatedAt
		if bot := botLeg(snap); bot != nil {
			joined = joinedAt(snap, bot)
		}
		hops = append(hops, hop{store.Leg{Kind: "BOT", Label: snap.Bot.FlowSlug, DurationSec: botSec}, joined})
	}
	for _, stay := range stays {
		leg := store.Leg{Kind: "QUEUE", Label: stay.Name, DurationSec: stayWaitSec(stay)}
		if stay.CancelReason != "" {
			leg.Note = stay.CancelReason
		}
		hops = append(hops, hop{leg, stay.JoinedAt})
	}
	for _, agent := range parties {
		kind, label := journeyHop(snap, agent)
		leg := store.Leg{Kind: kind, Label: label}
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
		hops = append(hops, hop{leg, joinedAt(snap, agent)})
	}
	// Stable, so legs that joined at the same moment keep the order above:
	// the placer before anyone they reached, the bot before the queue it
	// handed to, the queue before the phone it rang in the same second.
	slices.SortStableFunc(hops, func(a, b hop) int { return a.joined.Compare(b.joined) })
	var legs []store.Leg
	for _, h := range hops {
		legs = append(legs, h.leg)
	}
	return legs
}

// journeyHop is the kind and label leg p takes in the journey: AGENT for one of
// our own phones, TRUNK for a leg facing a carrier. Which legs face one depends
// on the leg's role.
//
// A leg the call reached out to is an agent's when it belongs to one, and a
// leg at one of our own phones is an agent's leg whether or not anybody is
// signed in there: it never left the building (#284). The extension is what
// the leg itself said, so a carrier caller whose number merely resembles one
// stays a trunk. Any other leg an outbound call dialled went through a
// carrier.
//
// The originator faces a carrier on any call that is not between our own
// extensions — the carrier's caller on an inbound call, the customer leg this
// platform created on an AI outbound one — unless one of our phones placed it
// (isAgentPlaced), when it is labelled as placerLeg labels it. An agent id on
// the originator does not make it ours: a carrier caller whose number matches
// a signed-in agent's extension carries their id for the screen pop (#105). On
// an internal call nothing faces a carrier, so a leg the dialplan spawned is
// not a trunk either (#313).
func journeyHop(snap Snapshot, p *PartySnapshot) (kind, label string) {
	if p.Role == RoleOriginator {
		switch {
		case isAgentPlaced(p), snap.CallType == events.CallTypeInternal:
			return "AGENT", p.Number
		default:
			return "TRUNK", p.Number
		}
	}
	switch {
	case p.AgentID != nil:
		return "AGENT", p.Number
	case p.ExtensionNumber != "":
		return "AGENT", p.ExtensionNumber
	case snap.CallType == events.CallTypeOutbound:
		return "TRUNK", p.Number
	default:
		return "AGENT", p.Number
	}
}

// unreachedJourney is the journey of a call buildLegs could draw no hop for:
// nobody on this side joined it.
//
// DIALING is the number being rung and nobody answering. It is not a state an
// answered call can be in, and writing it on one produced a row that
// contradicted the answered_at and bill_sec beside it: an AI outbound call the
// customer answered, whose bridge to the bot never ran, read as still dialling
// the number it had been billed thirty seconds for (2026-10-22). answered is
// the row's status, never the originator's own answer: an agent's phone
// auto-answers in front of them, so a click-to-dial whose trunk leg never came
// up (a gateway down, the agent hanging up first) was answered on nobody's
// side, and is NO_ANSWER and DIALING both.
//
// An answered call's one hop is the leg it was answered on (journeyHop), for
// as long as it lasted: the trunk facing the carrier — the caller of an
// inbound call, the customer leg of a call this platform placed — whose span
// is the window bill_sec measures, or, on a call between our own extensions,
// the leg it began on, which nobody bills. A call one of our phones placed
// is answered only when it reached somebody, and then has hops of its own; if
// one ever arrives here it is the placer's hop, counted from their bridges as
// everywhere else (placerLeg).
//
// A bot that was meant to take the call and never joined is not a hop, because
// it did not happen: BOT means the journey reached the bot (botJoined), and the
// reader is told what was intended instead by flow_id, which the outbound
// originator stamps on the customer leg from its first moment, and why the call
// ended by hangupCause (BOT_BRIDGE_UNCONFIRMED, OVERFLOW). A 0 s BOT hop would
// have said the bot was there for no time at all (#313).
func unreachedJourney(snap Snapshot, originator *PartySnapshot, answered bool, end time.Time) []store.Leg {
	if !answered {
		return []store.Leg{{Kind: "DIALING", Label: originator.Number}}
	}
	if isAgentPlaced(originator) {
		return []store.Leg{placerLeg(originator, end)}
	}
	kind, label := journeyHop(snap, originator)
	leg := store.Leg{Kind: kind, Label: label}
	if originator.AnsweredAt != nil {
		leg.DurationSec = max(0, int(end.Sub(*originator.AnsweredAt).Seconds()))
	}
	return []store.Leg{leg}
}

// causeUserBusy is the switch's release cause for a callee whose line is in
// use, which is also what most networks report when a mobile declines.
// CALL_REJECTED is deliberately not counted: it is not a statement that the
// line was in use (#180).
const causeUserBusy = "USER_BUSY"

// calleeWasBusy reports whether an unanswered outbound call ended because the
// person dialled was busy. It refines the unanswered branch and is never asked
// of an answered call, so a later leg releasing USER_BUSY cannot undo an
// answer.
//
// The cause that counts is the one on the leg that went out towards the
// callee. On a call this platform placed that is the originator, because we
// created the customer's leg. On a call an agent placed it is a dialled leg:
// the originator is the agent's own leg, which auto-answers in front of them
// and releases with whatever ended their side, so its cause says nothing about
// the far end. Inbound calls never qualify: an agent leg releasing USER_BUSY
// in a queue is a missed delivery, not a busy caller.
func calleeWasBusy(snap Snapshot, originator *PartySnapshot, dialled []*PartySnapshot, placed bool) bool {
	if snap.CallType != events.CallTypeOutbound || originator == nil {
		return false
	}
	if !placed {
		return originator.ReleaseCause == causeUserBusy
	}
	for _, leg := range dialled {
		if leg.AnsweredAt == nil && leg.ReleaseCause == causeUserBusy {
			return true
		}
	}
	return false
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

// QueueEvent records one member movement into the ledger.
//
// Not for the reports — those read cdrs, and this comment used to claim
// otherwise (C7). These rows are the call's journey rather than its outcome:
// which queues it crossed, which agents were offered it and declined, how long
// the caller had been waiting each time. A CDR keeps one queue id and one
// missed reason, so a call offered once and a call offered thirty-three times
// are the same row there. Read back by GET /calls/{callId}/queue-events.
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
