// SPDX-License-Identifier: Apache-2.0

// Package transcript owns the order of a call's conversation.
//
// A whole-call transcript has two producers — the conversational engine's own
// transcript of the AI leg, and recognition of the human phase's streamed
// audio — and one invariant that both must respect: `seq` is dense and
// per-call, because it is the total order the client sorts on and the cursor
// the backfill resumes from. Two producers allocating from their own counters
// would collide on uq_transcripts_call_id_seq.
//
// So exactly one goroutine per call allocates seq, writes the row and
// publishes the event, and both producers post to its mailbox. That makes the
// ordering a structural fact rather than a runtime hope, and it is the same
// actor-per-call shape telephony.Registry already uses.
package transcript

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Publisher is the slice of events.Hub this package needs.
type Publisher interface {
	Publish(ctx context.Context, ev events.Event, scope events.Scope) events.Event
}

// Store is the slice of the ledger this package writes to.
type Store interface {
	InsertTranscriptLine(ctx context.Context, callID uuid.UUID, line store.TranscriptLine) error
}

// Line is one thing said or done, as a producer reports it. Seq, occurredAt
// and offsetMs are deliberately absent: they are the actor's to assign, and a
// producer that could set them could break the order.
type Line struct {
	Speaker     string
	Kind        string
	Text        string
	Content     map[string]any
	PartyID     *uuid.UUID
	AgentID     *uuid.UUID
	Language    string
	Source      string
	Provider    string
	UtteranceID string
	// IsFinal false means a guess that will be contradicted within a second:
	// it reaches the stream but never the ledger, and never takes a seq.
	IsFinal bool
}

func (l Line) content() map[string]any {
	if l.Content != nil {
		return l.Content
	}
	return map[string]any{"text": l.Text}
}

// mailbox depth. A transcript line is small and rare next to a media frame;
// this is generous enough that overflow means something upstream is broken,
// not that a caller talked quickly.
const mailboxDepth = 256

// Actor is one call's transcript. Use Registry.For to get one.
type Actor struct {
	callID     uuid.UUID
	callType   events.CallType
	answeredAt time.Time

	store Store
	pub   Publisher
	log   *slog.Logger

	in   chan Line
	done chan struct{}
	once sync.Once

	// audience is read by the actor goroutine and written by whoever learns
	// that an agent joined, so it is guarded rather than owned.
	mu       sync.RWMutex
	agentIDs []uuid.UUID
	queueID  *uuid.UUID

	// seq is owned by the actor goroutine alone. No lock, by construction.
	seq int
}

// SetAudience records who may see this call's transcript. During the bot phase
// there is no agent party, so the events reach supervisors and admins only —
// which is exactly why an agent joining mid-call must backfill over REST
// rather than expect the stream to have carried the bot phase to them.
func (a *Actor) SetAudience(agentIDs []uuid.UUID, queueID *uuid.UUID) {
	a.mu.Lock()
	a.agentIDs = append(a.agentIDs[:0], agentIDs...)
	a.queueID = queueID
	a.mu.Unlock()
}

func (a *Actor) scope() events.Scope {
	a.mu.RLock()
	defer a.mu.RUnlock()
	scope := events.Scope{QueueID: a.queueID}
	if len(a.agentIDs) > 0 {
		scope.AgentIDs = append([]uuid.UUID(nil), a.agentIDs...)
	}
	return scope
}

// Post hands a line to the actor. It never blocks the caller: a producer is
// either a call actor or a media path, and neither may stall on a database.
func (a *Actor) Post(line Line) {
	select {
	case <-a.done:
	case a.in <- line:
	default:
		a.log.Error("transcript mailbox full, line dropped",
			"callId", a.callID, "speaker", line.Speaker)
	}
}

// State publishes how live transcription is faring, so the panel can say so
// rather than silently show nothing.
func (a *Actor) State(state, reason string, degraded []string) {
	payload := map[string]any{"state": state}
	if reason != "" {
		payload["reason"] = reason
	}
	if len(degraded) > 0 {
		payload["degradedSpeakers"] = degraded
	}
	a.publish(events.TypeCallTranscriptionState, payload)
}

// Close stops the actor once its mailbox has drained.
func (a *Actor) Close() {
	a.once.Do(func() { close(a.in) })
	<-a.done
}

func (a *Actor) run() {
	defer close(a.done)
	for line := range a.in {
		a.handle(line)
	}
}

func (a *Actor) handle(line Line) {
	now := time.Now().UTC()

	// A partial is a guess. It reaches the stream so the panel feels live, and
	// stops there: storing it would make the ledger the only record in the
	// system containing text nobody said.
	if !line.IsFinal {
		a.publish(events.TypeCallTranscript, a.payload(line, 0, now, false))
		return
	}

	// An empty final is real — leading silence produces one — and it must not
	// consume a seq. seq is the ordering base and the backfill cursor, so one
	// burned on silence leaves a permanent hole between an agent's snapshot
	// and their live tail, and nothing errors on the way. Checked here because
	// the actor is the only allocator: a guard anywhere else can be bypassed.
	if line.Kind == store.TranscriptKindText && strings.TrimSpace(line.Text) == "" {
		return
	}

	a.seq++
	seq := a.seq
	offset := 0
	if !a.answeredAt.IsZero() && now.After(a.answeredAt) {
		offset = int(now.Sub(a.answeredAt).Milliseconds())
	}

	source := line.Source
	if source == "" {
		source = store.TranscriptSourceModel
	}
	row := store.TranscriptLine{
		Seq:         seq,
		OccurredAt:  now,
		Speaker:     line.Speaker,
		Kind:        line.Kind,
		Content:     line.content(),
		PartyID:     line.PartyID,
		AgentID:     line.AgentID,
		OffsetMs:    offset,
		Language:    line.Language,
		Source:      source,
		Provider:    line.Provider,
		UtteranceID: line.UtteranceID,
	}
	if a.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := a.store.InsertTranscriptLine(ctx, a.callID, row); err != nil {
			// The line is still published: a transcript the agent can read
			// beats one that is durable and invisible.
			a.log.Error("could not write a transcript line",
				"callId", a.callID, "seq", seq, "error", err)
		}
		cancel()
	}
	a.publish(events.TypeCallTranscript, a.payload(line, seq, now, true))
}

func (a *Actor) payload(line Line, seq int, at time.Time, isFinal bool) map[string]any {
	source := line.Source
	if source == "" {
		source = store.TranscriptSourceModel
	}
	payload := map[string]any{
		"utteranceId": line.UtteranceID,
		"speaker":     line.Speaker,
		"kind":        line.Kind,
		"isFinal":     isFinal,
		"text":        line.Text,
		"source":      source,
	}
	if isFinal {
		payload["seq"] = seq
		if !a.answeredAt.IsZero() && at.After(a.answeredAt) {
			payload["offsetMs"] = int(at.Sub(a.answeredAt).Milliseconds())
		}
	}
	if line.AgentID != nil {
		payload["agentId"] = *line.AgentID
	}
	if line.PartyID != nil {
		payload["partyId"] = *line.PartyID
	}
	if line.Language != "" {
		payload["language"] = line.Language
	}
	return payload
}

func (a *Actor) publish(t events.Type, payload map[string]any) {
	if a.pub == nil {
		return
	}
	callID := a.callID
	a.pub.Publish(context.Background(), events.Event{
		Type:     t,
		CallID:   &callID,
		CallType: a.callType,
		Payload:  payload,
	}, a.scope())
}

// Registry hands out one actor per call and closes them when the call is over.
type Registry struct {
	store Store
	pub   Publisher
	log   *slog.Logger

	mu     sync.Mutex
	actors map[uuid.UUID]*Actor
}

func NewRegistry(st Store, pub Publisher, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{store: st, pub: pub, log: log, actors: map[uuid.UUID]*Actor{}}
}

// For returns the call's actor, starting one on first use. answeredAt anchors
// offsetMs at the first answer by anybody, which is also when record_session
// begins — so the offsets line up with the recording's timeline.
func (r *Registry) For(callID uuid.UUID, callType events.CallType, answeredAt time.Time) *Actor {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.actors[callID]; ok {
		return a
	}
	a := &Actor{
		callID:     callID,
		callType:   callType,
		answeredAt: answeredAt,
		store:      r.store,
		pub:        r.pub,
		log:        r.log,
		in:         make(chan Line, mailboxDepth),
		done:       make(chan struct{}),
	}
	go a.run()
	r.actors[callID] = a
	return a
}

// Lookup returns the call's actor if one exists, without starting one.
func (r *Registry) Lookup(callID uuid.UUID) (*Actor, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.actors[callID]
	return a, ok
}

// Close drains and retires the call's actor.
func (r *Registry) Close(callID uuid.UUID) {
	r.mu.Lock()
	a, ok := r.actors[callID]
	delete(r.actors, callID)
	r.mu.Unlock()
	if ok {
		a.Close()
	}
}
