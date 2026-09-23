// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// How a session ends.
//
// There are seven ways, and a call is released on the strength of which one it
// was. Each is checked for the same four things: the outcome it settled on, the
// events the call saw (exactly one CLOSED, last, the channel closed once),
// nothing written to the provider after the session began ending, and nothing
// left running.
//

// The ordinary end: the caller hung up and the application closed the session.
func TestTheCallerHangsUp(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	assertOneCleanEnding(t, session, f, before)
	if got := session.outcome(); got != outcomeCleanClose {
		t.Errorf("the session ended as %q, want %q", got, outcomeCleanClose)
	}
}

// A transfer closes the session in the middle of a sentence. The turn that was
// open is not reported as anything: the call is over, and the only thing left to
// say is that it is.
func TestClosingDuringAnOpenTurn(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	f.send(modelAudio(encoded(0x01)))
	expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeAudioDelta)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	assertOneCleanEnding(t, session, f, before)
	if got := session.outcome(); got != outcomeCleanClose {
		t.Errorf("the session ended as %q, want %q", got, outcomeCleanClose)
	}
}

// Closing twice is closing once. Both the call actor's hangup path and its
// transfer path can reach it, and they race.
func TestClosingIsIdempotent(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	for range 3 {
		if err := session.Close(t.Context()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	assertOneCleanEnding(t, session, f, before)
}

// A session the application is still using, closed politely by the provider.
// There is no reconnect — the conversation cannot be rebuilt — so the call has
// to go somewhere a person can take it.
func TestTheServerClosesTheSessionUnasked(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	f.closeWith(websocket.CloseNormalClosure, "")

	event := awaitEvent(t, session, provider.EventTypeError)
	if !event.IsFatal {
		t.Error("the session ended under us and the call was told it could continue")
	}
	if got := event.FailureCause; got != "" {
		t.Errorf("the failure was named %q; only a session that ran out of time has a name", got)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	f.assertNothingFollowed(before)
	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := session.outcome(); got != outcomeClosedByServer {
		t.Errorf("the session ended as %q, want %q", got, outcomeClosedByServer)
	}
}

// The close code and its reason are the whole error channel on this protocol:
// there is no error frame, and the reason is cut off by the server at 123 bytes.
func TestACloseCodeMidCallIsFatalAndCarriesItsReason(t *testing.T) {
	defer noGoroutinesLeft(t)()

	const reason = "Invalid JSON payload received. Unknown name \"nonsense\""
	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	f.closeWith(websocket.CloseInvalidFramePayloadData, reason)

	event := awaitEvent(t, session, provider.EventTypeError)
	if !event.IsFatal {
		t.Error("a close code was reported as survivable")
	}
	if !strings.Contains(event.Text, reason) || !strings.Contains(event.Text, "1007") {
		t.Errorf("the failure read %q, want the code and the provider's own reason", event.Text)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	f.assertNothingFollowed(before)
	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := session.outcome(); got != rejectedOutcome(1007) {
		t.Errorf("the session ended as %q, want %q", got, rejectedOutcome(1007))
	}
}

// A reason longer than the protocol carries arrives already cut off. The bound
// is repeated here so that a log line and an error text say the same thing.
func TestALongCloseReasonIsCutToWhatTheProtocolCarries(t *testing.T) {
	long := strings.Repeat("a", 200)
	if got := truncateReason(long); len(got) != closeReasonLimit {
		t.Errorf("a %d byte reason was carried as %d bytes, want %d",
			len(long), len(got), closeReasonLimit)
	}
	if got := truncateReason("short"); got != "short" {
		t.Errorf("a short reason was changed to %q", got)
	}
}

// The socket died under the session. There is no reconnect: provider-side
// conversation state cannot be rebuilt, so the call is routed elsewhere.
func TestAnAbruptSocketDropIsFatal(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	f.hangUp()

	event := awaitEvent(t, session, provider.EventTypeError)
	if !event.IsFatal || event.Text != "provider connection lost" {
		t.Errorf("the lost socket was reported as %q (fatal=%v)", event.Text, event.IsFatal)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	f.assertNothingFollowed(before)
	events := drainEvents(t, session)
	if got := countOf(events, provider.EventTypeClosed); got != 1 {
		t.Errorf("the call saw %d CLOSED events, want exactly one (%v)", got, typesOf(events))
	}
	if got := session.outcome(); got != outcomeConnectionLost {
		t.Errorf("the session ended as %q, want %q", got, outcomeConnectionLost)
	}
}

// A provider that stops answering altogether cannot hold the call open: the
// wait is this client's own, because every caller passes a context with no
// deadline in it.
func TestAProviderThatStopsAnsweringStillEndsTheSession(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := testSession(t, f)
	session.closeWait = 100 * time.Millisecond
	start(t, session, testConfig())
	before := len(f.settledFrames(2))

	started := time.Now()
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the close took %v, want the client's own bound", elapsed)
	}

	assertOneCleanEnding(t, session, f, before)
}

// The call is already gone and its context with it. The session is still ended
// — that is what stops the provider billing for one — but nothing waits.
func TestClosingWithAnAlreadyCancelledContext(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	assertOneCleanEnding(t, session, f, before)
}

// Audio offered after the session began ending goes nowhere at all, whatever the
// call actor still has in hand.
func TestNoAudioIsTakenOnceTheSessionIsStopping(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeGemini(t, acceptSetup)
	session := startedSession(t, f)
	before := len(f.settledFrames(2))

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := session.SendAudio(frameOf(0x01)); err == nil {
		t.Error("audio after the close was accepted")
	}
	if err := session.SpeakText("Too late.", false); err == nil {
		t.Error("a line after the close was accepted")
	}
	if err := session.SendUserText("2"); err == nil {
		t.Error("a cue after the close was accepted")
	}
	f.assertNothingFollowed(before)
	drainEvents(t, session)
}

// Everything that can end a session, at once. A call ends once: one outcome, one
// CLOSED event, one socket closed — however many things decided it at the same
// moment.
func TestEverythingEndsTheSessionAtOnce(t *testing.T) {
	defer noGoroutinesLeft(t)()
	t.Setenv("GEMINI_API_KEY", "test-key")

	for iteration := range 50 {
		f := newFakeGemini(t, acceptSetup)
		session := testSession(t, f)
		start(t, session, testConfig())

		ctx, cancel := context.WithCancel(context.Background())

		var work sync.WaitGroup
		work.Add(4)
		go func() {
			defer work.Done()
			if err := session.Close(context.Background()); err != nil {
				t.Errorf("iteration %d: close: %v", iteration, err)
			}
		}()
		go func() {
			defer work.Done()
			f.closeWith(websocket.CloseInternalServerErr, "internal error")
		}()
		go func() {
			defer work.Done()
			cancel()
			if err := session.Close(ctx); err != nil {
				t.Errorf("iteration %d: close with a cancelled context: %v", iteration, err)
			}
		}()
		go func() {
			defer work.Done()
			// The call actor is still feeding audio and driving the
			// conversation when the hangup arrives.
			_ = session.SendAudio(frameOf(0x01))
			_ = session.SpeakText("Goodbye.", false)
			_ = session.Interrupt(provider.InterruptReasonSystem, 40)
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
		if session.outcome() == "" {
			t.Fatalf("iteration %d: the session ended without an outcome", iteration)
		}
	}
}

// assertOneCleanEnding is what every polite ending looks like from both sides.
func assertOneCleanEnding(t *testing.T, session *Session, f *fakeGemini, before int) {
	t.Helper()

	f.assertNothingFollowed(before)

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
