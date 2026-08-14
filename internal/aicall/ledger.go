// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Ledger is where finished AI calls are written down. Nil disables writing,
// which is what tests want.
type Ledger interface {
	InsertCDR(ctx context.Context, cdr store.CDR) error
	InsertTranscript(ctx context.Context, callID uuid.UUID, entries []store.TranscriptEntry) error
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

	mu      sync.Mutex
	entries []store.TranscriptEntry
	seq     int

	isTransferred bool
	transferQueue *uuid.UUID
	// endReason distinguishes how the conversation closed, for containment:
	// "" (caller hung up or failure), "HANGUP" (the bot closed it properly),
	// "TRANSFER".
	endReason   string
	hangupCause string
}

func newCallRecorder(callID uuid.UUID, startedAt time.Time) *callRecorder {
	return &callRecorder{
		callID:     callID,
		startedAt:  startedAt,
		answeredAt: startedAt, // a bot answers the moment the leg is up
	}
}

// say records one line of conversation.
func (r *callRecorder) say(role, text string) {
	if text == "" {
		return
	}
	r.add(role, store.TranscriptKindText, map[string]any{"text": text})
}

// toolCall records the model asking for something.
func (r *callRecorder) toolCall(name, args string) {
	r.add(store.TranscriptRoleBot, store.TranscriptKindToolCall,
		map[string]any{"name": name, "args": args})
}

// toolResult records what the tool answered.
func (r *callRecorder) toolResult(name, output string) {
	r.add(store.TranscriptRoleBot, store.TranscriptKindToolResult,
		map[string]any{"name": name, "output": output})
}

func (r *callRecorder) add(role, kind string, content map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.entries = append(r.entries, store.TranscriptEntry{
		Seq:        r.seq,
		OccurredAt: time.Now(),
		Role:       role,
		Kind:       kind,
		Content:    content,
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
	entries := r.entries
	isTransferred := r.isTransferred
	endReason := r.endReason
	hangupCause := r.hangupCause
	transferQueue := r.transferQueue
	r.mu.Unlock()

	if err := ledger.InsertTranscript(ctx, r.callID, entries); err != nil {
		log.Error("could not write the transcript", "error", err)
	}
	if isTransferred {
		return
	}

	endedAt := time.Now()
	status := store.CDRStatusAnswered
	if endReason == "FAILED" {
		status = store.CDRStatusFailed
	}
	if hangupCause == "" {
		hangupCause = "NORMAL_CLEARING"
	}
	botSec := int(endedAt.Sub(r.answeredAt).Seconds())

	cdr := store.CDR{
		CallID:      r.callID,
		StartedAt:   r.startedAt,
		AnsweredAt:  r.answeredAt,
		EndedAt:     endedAt,
		CallType:    string(call.callType),
		Language:    call.language,
		FromNumber:  call.fromNumber,
		ToNumber:    call.did,
		DID:         call.did,
		FlowID:      call.flowID,
		QueueID:     transferQueue,
		BotSec:      botSec,
		TotalSec:    int(endedAt.Sub(r.startedAt).Seconds()),
		Status:      status,
		HangupCause: hangupCause,
		// Contained: the bot answered and finished the call itself, properly.
		IsContained:  status == store.CDRStatusAnswered && endReason == "HANGUP",
		HasRecording: call.isRecordingEnabled,
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
}

// callType mirrors the domain enum without importing the events package into
// every test.
type callType string

const callTypeInbound callType = "INBOUND"

// stampBotShare writes the bot's part of the story onto the caller's channel,
// so the CDR the human path writes after a transfer carries it.
func (a *callActions) stampBotShare(recorder *callRecorder, facts *callFacts) {
	botSec := int(time.Since(recorder.answeredAt).Seconds())
	a.stampChannel("aicc_bot_sec", strconv.Itoa(botSec))
	a.stampChannel("aicc_language", facts.language)
	if facts.flowID != nil {
		a.stampChannel("aicc_flow_id", facts.flowID.String())
	}
	a.stampChannel("aicc_did", facts.did)
}
