// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

// Ledger is where finished AI calls are written down. Nil disables writing,
// which is what tests want.
type Ledger interface {
	InsertCDR(ctx context.Context, cdr store.CDR) error
	InsertCallback(ctx context.Context, callID, queueID *uuid.UUID, phoneNumber, message string) (store.Callback, error)
}

// callRecorder accumulates what one AI call will leave behind: the transcript
// as it happens, and the facts the CDR needs at the end.
//
// Ownership of the CDR follows the call, not the leg. A call the bot finishes
// — hangup, caller hung up, contained or failed — is the bot's to write. A
// call the bot hands to a person is not finished: the human path writes the
// one CDR at the real end, and the bot's share travels there as channel
// variables on the caller's leg.
type callRecorder struct {
	callID     uuid.UUID
	startedAt  time.Time
	answeredAt time.Time

	// transcript is the call's sole seq allocator and transcript writer. The
	// recorder posts to it rather than accumulating, so the bot phase is
	// readable while it happens instead of only after hangup.
	transcript *transcript.Actor

	mu sync.Mutex

	isTransferred bool
	transferQueue *uuid.UUID
	// endReason distinguishes how the conversation closed, for containment:
	// "" (caller hung up or failure), "HANGUP" (the bot closed it properly),
	// "TRANSFER".
	endReason   string
	hangupCause string
}

func newCallRecorder(callID uuid.UUID, startedAt time.Time, actor *transcript.Actor) *callRecorder {
	return &callRecorder{
		callID:     callID,
		startedAt:  startedAt,
		answeredAt: startedAt, // a bot answers the moment the leg is up
		transcript: actor,
	}
}

// say records one line of conversation.
func (r *callRecorder) say(speaker, text string) {
	if text == "" {
		return
	}
	r.add(speaker, store.TranscriptKindText, text, nil)
}

// toolCall records the model asking for something.
//
// The name is both the line's text and part of its content. The ledger reads
// the content; the live stream carries only text, and a tool line whose text
// was empty reached the cockpit as "requested " and " answered" — the sentence
// with the one word that carried its meaning missing.
func (r *callRecorder) toolCall(name, args string) {
	r.add(store.SpeakerBot, store.TranscriptKindToolCall, name,
		map[string]any{"name": name, "args": args})
}

// toolResult records what the tool answered.
func (r *callRecorder) toolResult(name, output string) {
	r.add(store.SpeakerBot, store.TranscriptKindToolResult, name,
		map[string]any{"name": name, "output": output})
}

// add hands the line to the call's transcript actor. The bot's transcript is
// the model's own, not recognition of audio, so every line it writes is a
// final from a MODEL source.
func (r *callRecorder) add(speaker, kind, text string, content map[string]any) {
	if r.transcript == nil {
		return
	}
	r.transcript.Post(transcript.Line{
		Speaker: speaker,
		Kind:    kind,
		Text:    text,
		Content: content,
		Source:  store.TranscriptSourceModel,
		IsFinal: true,
	})
}

func (r *callRecorder) markTransferred(queueID uuid.UUID) {
	r.mu.Lock()
	r.isTransferred = true
	r.transferQueue = &queueID
	r.endReason = "TRANSFER"
	r.mu.Unlock()
}

func (r *callRecorder) markHangup() {
	r.mu.Lock()
	if r.endReason == "" {
		r.endReason = "HANGUP"
	}
	r.mu.Unlock()
}

func (r *callRecorder) markFailed(cause string) {
	r.mu.Lock()
	r.endReason = "FAILED"
	r.hangupCause = cause
	r.mu.Unlock()
}

// finish writes what this call leaves behind.
//
// The transcript is always the bot's to write — no one else saw the
// conversation. The CDR is written only when the call ended here; a transfer
// means the human path owns the single CDR, with the bot's share stamped onto
// the caller's channel before the leg moved.
func (r *callRecorder) finish(ledger Ledger, call *callFacts, log *slog.Logger) {
	if ledger == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r.mu.Lock()
	endReason := r.endReason
	hangupCause := r.hangupCause
	transferQueue := r.transferQueue
	r.mu.Unlock()

	// A transferred call is normally the human path's row to write, and this
	// one is provisional: it is replaced the moment that path writes, because
	// the row that saw the call end later wins (see InsertCDR).
	//
	// It is written all the same, because "transferred" is decided when the
	// bot calls the tool and the caller is not handed on until the closing
	// sentence has been heard. A caller who hangs up during the goodbye leaves
	// the mark set with nothing having happened, and there is no failure to
	// react to — no transfer was ever attempted. Declining to write here lost
	// those calls from the ledger entirely: the bot answered, spoke, decided,
	// and the call appeared nowhere at all.
	endedAt := time.Now()
	status := store.CDRStatusAnswered
	if endReason == "FAILED" {
		status = store.CDRStatusFailed
	}
	if hangupCause == "" {
		hangupCause = "NORMAL_CLEARING"
	}
	botSec := int(endedAt.Sub(r.answeredAt).Seconds())

	// Which end is which depends on who called whom, and the two facts we
	// have do not move: the DID is always this side and the ANI is always the
	// far side. On a call that came in, the far side is the caller and the DID
	// is what they dialled. On a call this platform placed, the far side is
	// the person answering and the DID is the number shown to them.
	//
	// Written as though every call were inbound, an outbound row came out
	// reversed — and its to_number was always the DID, so no AI outbound call
	// could be found by the number it actually called (C43).
	fromNumber, toNumber := call.fromNumber, call.did
	if call.callType == callTypeOutbound {
		fromNumber, toNumber = call.did, call.fromNumber
	}

	cdr := store.CDR{
		CallID:     r.callID,
		StartedAt:  r.startedAt,
		AnsweredAt: r.answeredAt,
		EndedAt:    endedAt,
		CallType:   string(call.callType),
		Language:   call.language,
		FromNumber: fromNumber,
		ToNumber:   toNumber,
		DID:        call.did,
		FlowID:     call.flowID,
		QueueID:    transferQueue,
		BotSec:     botSec,
		// The carrier bills a call the bot answered exactly as it bills one a
		// person answered — from the answer to the end. This path writes the
		// row for calls that never reached a person, and those are billed too;
		// leaving it at zero told the ledger they were free.
		BillSec:     max(0, int(endedAt.Sub(r.answeredAt).Seconds())),
		TotalSec:    int(endedAt.Sub(r.startedAt).Seconds()),
		Status:      status,
		HangupCause: hangupCause,
		// Contained: the bot answered and finished the call itself, properly.
		IsContained:  status == store.CDRStatusAnswered && endReason == "HANGUP",
		HasRecording: call.isRecordingEnabled,
		UserData:     call.userData,
		Tech:         call.tech,
		Legs: []store.Leg{{
			Kind:        "BOT",
			Label:       call.flowSlug,
			DurationSec: botSec,
		}},
	}
	if err := ledger.InsertCDR(ctx, cdr); err != nil {
		log.Error("could not write the cdr", "error", err)
	}
}

// transcriptActor returns the actor that owns this call's transcript order, or
// nil when transcripts are switched off. A bot answers the moment its leg is
// up, so the actor's offsets are anchored there.
func (o *Orchestrator) transcriptActor(callID uuid.UUID, direction callType) *transcript.Actor {
	if o.cfg.Transcripts == nil {
		return nil
	}
	return o.cfg.Transcripts.For(callID, events.CallType(direction), time.Now().UTC())
}

// callFacts is what the orchestrator knows about the call that the recorder
// does not learn from events.
type callFacts struct {
	callType           callType
	language           string
	fromNumber         string
	did                string
	flowID             *uuid.UUID
	flowSlug           string
	isRecordingEnabled bool
	tech               map[string]any
	// userData is business data the request attached when it placed the call.
	userData map[string]any
}

// callType mirrors the domain enum without importing the events package into
// every test.
type callType string

const (
	callTypeInbound  callType = "INBOUND"
	callTypeOutbound callType = "OUTBOUND"
)

// stampBotShare writes the bot's part of the story onto the caller's channel,
// so the CDR the human path writes after a transfer carries it.
func (a *callActions) stampBotShare(facts *callFacts) {
	a.stampChannel("aicc_language", facts.language)
	if facts.flowID != nil {
		a.stampChannel("aicc_flow_id", facts.flowID.String())
	}
	if facts.flowSlug != "" {
		a.stampChannel("aicc_flow_slug", facts.flowSlug)
	}
	a.stampChannel("aicc_did", facts.did)
}

// stampBotSec writes how long the caller was with the bot, and belongs at the
// moment they are handed on rather than the moment the bot decided to hand
// them on: a transfer waits for the closing sentence to be heard, and the
// caller is with the bot while it plays.
//
// Written at the decision it was short by the length of the goodbye, and the
// assembler — which reads the bot leg's own bridge and prefers it — warned
// about the disagreement on every correctly transferred call. A warning that
// fires every time is one nobody reads.
func (a *callActions) stampBotSec(recorder *callRecorder) {
	botSec := int(time.Since(recorder.answeredAt).Seconds())
	a.stampChannel("aicc_bot_sec", strconv.Itoa(botSec))
}
