// SPDX-License-Identifier: Apache-2.0

package transcript

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeStore struct {
	mu    sync.Mutex
	lines []store.TranscriptLine
	err   error
}

func (f *fakeStore) InsertTranscriptLine(_ context.Context, _ uuid.UUID, line store.TranscriptLine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.lines = append(f.lines, line)
	return nil
}

func (f *fakeStore) all() []store.TranscriptLine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.TranscriptLine(nil), f.lines...)
}

type fakePub struct {
	mu     sync.Mutex
	events []events.Event
	scopes []events.Scope
}

func (f *fakePub) Publish(_ context.Context, ev events.Event, scope events.Scope) events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	f.scopes = append(f.scopes, scope)
	return ev
}

func (f *fakePub) all() []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]events.Event(nil), f.events...)
}

func newFixture(t *testing.T) (*Actor, *fakeStore, *fakePub, func()) {
	t.Helper()
	st, pub := &fakeStore{}, &fakePub{}
	reg := NewRegistry(st, pub, discard())
	callID := uuid.New()
	actor := reg.For(callID, "INBOUND", time.Now().UTC().Add(-10*time.Second))
	var once sync.Once
	flush := func() { once.Do(func() { reg.Close(callID) }) }
	t.Cleanup(flush)
	return actor, st, pub, flush
}

func waitForEvents(t *testing.T, pub *fakePub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(pub.all()) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d events published, want %d", len(pub.all()), n)
}

func final(speaker, text string) Line {
	return Line{Speaker: speaker, Kind: store.TranscriptKindText, Text: text, IsFinal: true}
}

// Both producers post to one actor, and the order they are accepted in is the
// order the transcript has. seq is dense from 1 because it is a cursor.
func TestSeqIsDenseAndPerCallAcrossBothProducers(t *testing.T) {
	actor, st, _, flush := newFixture(t)

	actor.Post(final(store.SpeakerBot, "thanks for calling"))
	actor.Post(final(store.SpeakerCustomer, "I need help"))
	actor.Post(Line{Speaker: store.SpeakerHumanAgent, Kind: store.TranscriptKindText,
		Text: "I can see the order", Source: store.TranscriptSourceASR, IsFinal: true})
	flush()

	lines := st.all()
	if len(lines) != 3 {
		t.Fatalf("stored %d lines, want 3", len(lines))
	}
	for i, l := range lines {
		if l.Seq != i+1 {
			t.Errorf("line %d has seq %d, want %d", i, l.Seq, i+1)
		}
	}
	if lines[0].Source != store.TranscriptSourceModel {
		t.Errorf("the bot's line defaulted to source %q, want MODEL", lines[0].Source)
	}
	if lines[2].Source != store.TranscriptSourceASR {
		t.Errorf("the agent's line has source %q, want ASR", lines[2].Source)
	}
}

// An empty final is real: leading silence produces one. It must not consume a
// seq, because seq is both the ordering base and the backfill cursor — one
// burned on silence leaves a permanent hole between snapshot and tail that
// surfaces no error anywhere. Asserting on the row count alone would pass
// while that defect shipped, so this asserts the counter did not move.
func TestAnEmptyFinalTakesNoSeq(t *testing.T) {
	actor, st, pub, flush := newFixture(t)

	actor.Post(final(store.SpeakerCustomer, "hello"))
	actor.Post(final(store.SpeakerCustomer, ""))
	actor.Post(final(store.SpeakerCustomer, "   \t\n "))
	actor.Post(final(store.SpeakerCustomer, "are you there"))
	flush()

	lines := st.all()
	if len(lines) != 2 {
		t.Fatalf("stored %d lines, want 2", len(lines))
	}
	// The decisive assertion: the surviving lines are 1 and 2, not 1 and 4.
	if lines[0].Seq != 1 || lines[1].Seq != 2 {
		t.Fatalf("seq went %d,%d — an empty final consumed a number",
			lines[0].Seq, lines[1].Seq)
	}
	for _, ev := range pub.all() {
		if ev.Type == events.TypeCallTranscript && ev.Payload["text"] == "" {
			t.Error("an empty final was published")
		}
	}
}

// A partial reaches the stream so the panel feels live, and stops there: the
// ledger never holds text nobody said, and a guess takes no place in the order.
func TestAPartialIsPublishedButNeverStoredAndTakesNoSeq(t *testing.T) {
	actor, st, pub, flush := newFixture(t)

	actor.Post(Line{Speaker: store.SpeakerCustomer, Kind: store.TranscriptKindText,
		Text: "I need", Source: store.TranscriptSourceASR})
	actor.Post(Line{Speaker: store.SpeakerCustomer, Kind: store.TranscriptKindText,
		Text: "I need help", Source: store.TranscriptSourceASR, IsFinal: true})
	flush()

	lines := st.all()
	if len(lines) != 1 || lines[0].Seq != 1 {
		t.Fatalf("stored %+v, want one line at seq 1", lines)
	}
	published := pub.all()
	if len(published) != 2 {
		t.Fatalf("published %d events, want 2", len(published))
	}
	if published[0].Payload["isFinal"] != false {
		t.Error("the first event was not marked as a partial")
	}
	if _, ok := published[0].Payload["seq"]; ok {
		t.Error("a partial carried a transcript seq")
	}
	if published[1].Payload["seq"] != 1 {
		t.Errorf("the final carried seq %v, want 1", published[1].Payload["seq"])
	}
}

// During the bot phase there is no agent party, so the events reach
// supervisors and administrators only — which is exactly why an agent joining
// later must backfill.
func TestAudienceIsEmptyUntilAnAgentJoins(t *testing.T) {
	actor, _, pub, flush := newFixture(t)

	actor.Post(final(store.SpeakerBot, "thanks for calling"))
	// The actor evaluates the audience when it publishes, not when the line was
	// posted, so the bot line has to be through before the agent joins for this
	// to assert anything. That ordering is the test's problem, not a rule the
	// actor owes: an audience only ever grows, and a late-joining agent is
	// entitled to the whole transcript anyway — which is what the backfill is.
	waitForEvents(t, pub, 1)

	agentID := uuid.New()
	actor.SetAudience([]uuid.UUID{agentID})
	actor.Post(final(store.SpeakerHumanAgent, "how can I help"))
	flush()

	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.scopes) != 2 {
		t.Fatalf("published %d events, want 2", len(pub.scopes))
	}
	// The decisive part: the bot phase is addressed to supervisors, not left
	// unaddressed. An events.Scope with nothing set is delivered to every
	// subscriber, so "no agent is on this call yet" and "everyone may see
	// this" were the same value, and the second is what the hub acted on.
	if !pub.scopes[0].SupervisorOnly {
		t.Errorf("the bot phase was published with scope %+v, which the hub "+
			"delivers to every agent in the building", pub.scopes[0])
	}
	if len(pub.scopes[0].AgentIDs) != 0 {
		t.Errorf("the bot phase was scoped to %v", pub.scopes[0].AgentIDs)
	}
	if len(pub.scopes[1].AgentIDs) != 1 || pub.scopes[1].AgentIDs[0] != agentID {
		t.Errorf("the human phase was scoped to %v", pub.scopes[1].AgentIDs)
	}
	if pub.scopes[1].SupervisorOnly {
		t.Error("the human phase was withheld from the agent on the call")
	}
}

// A line the database refuses is still published: a transcript the agent can
// read beats one that is durable and invisible.
func TestAStoreFailureStillReachesTheStream(t *testing.T) {
	st, pub := &fakeStore{err: context.DeadlineExceeded}, &fakePub{}
	reg := NewRegistry(st, pub, discard())
	callID := uuid.New()
	actor := reg.For(callID, "INBOUND", time.Now().UTC())
	actor.Post(final(store.SpeakerBot, "still spoken"))
	reg.Close(callID)

	if got := len(pub.all()); got != 1 {
		t.Fatalf("published %d events, want 1", got)
	}
}

// offsetMs is measured from the answer, so it lines up with the recording.
func TestOffsetIsMeasuredFromAnswer(t *testing.T) {
	actor, st, _, flush := newFixture(t) // answered 10s ago
	actor.Post(final(store.SpeakerBot, "hello"))
	flush()

	lines := st.all()
	if len(lines) != 1 {
		t.Fatalf("stored %d lines", len(lines))
	}
	if lines[0].OffsetMs < 9_000 || lines[0].OffsetMs > 60_000 {
		t.Errorf("offsetMs = %d, want ~10000", lines[0].OffsetMs)
	}
}

// The registry hands the same actor to both producers, which is the whole
// point: two allocators would collide on uq_transcripts_call_id_seq.
func TestTheRegistryReturnsOneActorPerCall(t *testing.T) {
	reg := NewRegistry(&fakeStore{}, &fakePub{}, discard())
	callID := uuid.New()
	t.Cleanup(func() { reg.Close(callID) })

	first := reg.For(callID, "INBOUND", time.Now())
	second := reg.For(callID, "INBOUND", time.Now())
	if first != second {
		t.Error("the registry minted a second actor for one call")
	}
	if _, ok := reg.Lookup(callID); !ok {
		t.Error("the actor was not findable by call id")
	}
	if _, ok := reg.Lookup(uuid.New()); ok {
		t.Error("an unknown call produced an actor")
	}
}

// A snapshot must be told the same thing the stream would have told it, so the
// state lives where it is published. IDLE until something says otherwise: a
// call nobody is transcribing is not connecting, and the panel used to invent
// CONNECTING in exactly this gap.
func TestCurrentStateIsIdleUntilSomethingSaysOtherwise(t *testing.T) {
	actor, _, _, flush := newFixture(t)
	if got := actor.CurrentState(); got != StateIdle {
		t.Errorf("CurrentState = %q before anything was published, want IDLE", got)
	}

	actor.State(StateConnecting, "", nil)
	if got := actor.CurrentState(); got != StateConnecting {
		t.Errorf("CurrentState = %q, want CONNECTING", got)
	}

	actor.State(StateDegraded, "ASR_SESSION_FAILED", []string{"CUSTOMER"})
	if got := actor.CurrentState(); got != StateDegraded {
		t.Errorf("CurrentState = %q, want DEGRADED", got)
	}
	flush()
}
