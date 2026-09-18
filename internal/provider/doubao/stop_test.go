// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// How a session ends.
//
// There are six ways, and a call is released on the strength of which one it
// was. Each is checked for the same four things: the outcome it settled on,
// the events the call saw (exactly one CLOSED, the channel closed once), at
// most one session.close on the wire with nothing after it, and nothing left
// running.
//

// The ordinary end: the caller hung up and the application closed the session.
func TestTheCallerHangsUp(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	assertOneCleanEnding(t, session, f)
	if got := session.outcome(); got != outcomeCleanClose {
		t.Errorf("the session ended as %q, want %q", got, outcomeCleanClose)
	}
}

// A transfer closes the session in the middle of a sentence. The turn that was
// open is not reported as anything: the call is over, and the only thing left
// to say is that it is.
func TestClosingDuringAnOpenTurn(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(audioStarted("default"))
	expectEvents(t, session, provider.EventTypeResponseStarted)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	assertOneCleanEnding(t, session, f)
	if got := session.outcome(); got != outcomeCleanClose {
		t.Errorf("the session ended as %q, want %q", got, outcomeCleanClose)
	}
}

// Closing twice is closing once. Both the call actor's hangup path and its
// transfer path can reach it, and they race.
func TestClosingIsIdempotent(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	for range 3 {
		if err := session.Close(t.Context()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	assertOneCleanEnding(t, session, f)
}

// The server ending a session nobody asked it to end is a failure: the call
// has to be routed somewhere a person can take it.
func TestTheServerEndsTheSessionUnasked(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(map[string]any{"type": "session.closed", "event_id": "event_9"})

	event := awaitEvent(t, session, provider.EventTypeError)
	if !event.IsFatal {
		t.Error("the session ended under us and the call was told it could continue")
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	// There is no session left to close politely.
	f.awaitMessages("session.close", 0)

	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := session.outcome(); got != outcomeClosedByServer {
		t.Errorf("the session ended as %q, want %q", got, outcomeClosedByServer)
	}
}

// An error frame is the end of the session: this provider drops the socket in
// the same millisecond (e8b), so nothing more is written to it either.
func TestAServerErrorIsFatalAndNothingFollowsIt(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.send(errorFrame("45000003", "Abnormal silence audio"))
	f.hangUp()

	event := awaitEvent(t, session, provider.EventTypeError)
	if !event.IsFatal {
		t.Error("a provider error was reported as survivable")
	}
	if event.Err == nil || !strings.Contains(event.Err.Error(), "45000003") {
		t.Errorf("the error carried %v, want the provider's own code", event.Err)
	}
	if !strings.Contains(event.Text, "Abnormal silence audio") {
		t.Errorf("the error read %q, want the provider's own message", event.Text)
	}

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	// The socket was gone before the close: nothing but the session's own
	// creation ever reached the provider.
	if got := f.frameTypes(); len(got) != 1 || got[0] != "session.create" {
		t.Errorf("the provider saw %v, want nothing after the error", got)
	}
	if got := session.outcome(); got != failedOutcome("45000003") {
		t.Errorf("the session ended as %q, want %q", got, failedOutcome("45000003"))
	}
}

// The socket died under the session. There is no reconnect: provider-side
// conversation state cannot be rebuilt, so the call is routed elsewhere.
func TestAnAbruptSocketDropIsFatal(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSession)
	session := startedSession(t, f)

	f.hangUp()

	event := awaitEvent(t, session, provider.EventTypeError)
	if !event.IsFatal || event.Text != "provider connection lost" {
		t.Errorf("the lost socket was reported as %q (fatal=%v)", event.Text, event.IsFatal)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	f.awaitMessages("session.close", 0)
	if got := session.outcome(); got != outcomeConnectionLost {
		t.Errorf("the session ended as %q, want %q", got, outcomeConnectionLost)
	}
}

// A provider that takes the close request and never answers it cannot hold the
// call open: the wait is this client's own, because every caller passes a
// context with no deadline in it.
func TestACloseThatIsNeverAnsweredStillEnds(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSessionButNeverClose)
	session := testSession(t, f)
	session.closeWait = 100 * time.Millisecond
	start(t, session, testConfig())

	started := time.Now()
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the close took %v, want the client's own bound", elapsed)
	}

	f.awaitMessages("session.close", 1)
	f.assertNothingFollowedTheClose()
	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := session.outcome(); got != outcomeCloseTimeout {
		t.Errorf("the session ended as %q, want %q", got, outcomeCloseTimeout)
	}
}

// The call is already gone and its context with it. The provider is still told
// the session is over — that is what stops it billing for one — but nothing
// waits for the answer.
func TestClosingWithAnAlreadyCancelledContext(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, acceptSessionButNeverClose)
	session := startedSession(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	f.awaitMessages("session.close", 1)
	f.assertNothingFollowedTheClose()
	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := session.outcome(); got != outcomeCloseTimeout {
		t.Errorf("the session ended as %q, want %q", got, outcomeCloseTimeout)
	}
}

// A clean close comes with error frames of its own — "the stream is done",
// "no session active" — measured on a session that ended perfectly (e10a).
// Reporting them would fail a call that had already finished.
func TestSpuriousErrorsAroundACleanCloseAreNotReported(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, func(f *fakeDoubao, message map[string]any) {
		switch message["type"] {
		case "session.create":
			f.send(map[string]any{"type": "session.created", "event_id": "event_1",
				"session": map[string]any{"id": "fake-session"}})
		case "session.close":
			f.send(errorFrame("55000000", "rpc error: the stream is done"))
			f.send(map[string]any{"type": "session.closed", "event_id": "event_3"})
			f.send(errorFrame("45000000", "no session active, please create a session first"))
		}
	})
	session := startedSession(t, f)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeError); got != 0 {
		t.Errorf("the call was told about %d errors on a clean close (%v)", got, typesOf(events))
	}
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := session.outcome(); got != outcomeCleanClose {
		t.Errorf("the session ended as %q, want %q", got, outcomeCleanClose)
	}
}

// Everything that can end a session, at once. A call ends once: one outcome,
// one close on the wire, one socket closed, one CLOSED event — however many
// things decided it at the same moment.
func TestEverythingEndsTheSessionAtOnce(t *testing.T) {
	defer noGoroutinesLeft(t)()
	t.Setenv("DOUBAO_API_KEY", "test-key")

	for iteration := range 50 {
		f := newFakeDoubao(t, acceptSession)
		session := testSession(t, f)
		start(t, session, testConfig())

		ctx, cancel := context.WithCancel(context.Background())

		var work sync.WaitGroup
		work.Add(3)
		go func() {
			defer work.Done()
			if err := session.Close(context.Background()); err != nil {
				t.Errorf("iteration %d: close: %v", iteration, err)
			}
		}()
		go func() {
			defer work.Done()
			f.send(errorFrame("55000001", "ContextCanceled"))
			f.hangUp()
		}()
		go func() {
			defer work.Done()
			cancel()
			if err := session.Close(ctx); err != nil {
				t.Errorf("iteration %d: close with a cancelled context: %v", iteration, err)
			}
		}()
		work.Wait()
		cancel()

		events := drainEvents(t, session)
		if got := countOf(events, provider.EventTypeClosed); got != 1 {
			t.Fatalf("iteration %d: the call saw %d CLOSED events, want exactly one (%v)",
				iteration, got, typesOf(events))
		}
		if got := events[len(events)-1].Type; got != provider.EventTypeClosed {
			t.Fatalf("iteration %d: the last event was %s, want CLOSED", iteration, got)
		}
		if got := len(f.messagesOfType("session.close")); got > 1 {
			t.Fatalf("iteration %d: %d close requests reached the provider", iteration, got)
		}
		if session.outcome() == "" {
			t.Fatalf("iteration %d: the session ended without an outcome", iteration)
		}
	}
}

// assertOneCleanEnding is what every polite ending looks like from both sides.
func assertOneCleanEnding(t *testing.T, session *Session, f *fakeDoubao) {
	t.Helper()

	f.awaitMessages("session.close", 1)
	f.assertNothingFollowedTheClose()

	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if len(events) == 0 || events[len(events)-1].Type != provider.EventTypeClosed {
		t.Errorf("the last thing the call heard was %v, want CLOSED", typesOf(events))
	}
	if got := countOf(events, provider.EventTypeError); got != 0 {
		t.Errorf("a clean close reported %d errors (%v)", got, typesOf(events))
	}
}
