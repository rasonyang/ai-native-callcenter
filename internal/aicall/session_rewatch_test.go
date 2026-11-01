// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// A silent caller is never closed (#228): an engine that takes no text cue
// fails the reprompt, no turn follows, no playback ends, and nothing watched
// the silence again.

var errNoTextCue = errors.New("text cue unsupported")

const rewatchTimeout = 60 * time.Millisecond

// startSilentCall starts a session whose first turn has been heard, so the
// first dead-air watch is running.
func startSilentCall(t *testing.T, textErr error) (*Session, *fakeModel) {
	t.Helper()
	session, _, model := startBridgeWith(t, provider.OpenAIProfile(),
		Config{BargeGuard: -1, NoInput: rewatchTimeout})
	model.textErr = textErr
	awaitBridgeEvent(t, session, EventTypeReady)
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{
		Type:  provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples),
	}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	awaitBridgeEvent(t, session, EventTypePlaybackDone)
	return session, model
}

// expectNoEvent fails if the session reports typ within d.
func expectNoEvent(t *testing.T, session *Session, typ EventType, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case event, ok := <-session.Events():
			if ok && event.Type == typ {
				t.Fatalf("unexpected %s event", typ)
			}
		case <-deadline:
			return
		}
	}
}

func TestAFailedRepromptStillLetsRepeatedSilenceClose(t *testing.T) {
	session, model := startSilentCall(t, errNoTextCue)

	o := testOrchestrator(t, &fakeSwitch{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(farewellBackstopFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{orchestrator: o, session: session, log: log}
	actions.recorder = newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	// The orchestrator's loop, reduced to its reaction to NO_INPUT.
	for range 3 {
		awaitBridgeEvent(t, session, EventTypeNoInput)
		o.handleDeadAir(session, runtime, actions, false, nil, log)
	}

	if !engine.IsTerminal() {
		t.Fatal("three silences did not reach the closing target")
	}
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Sorry to have bothered you, goodbye." {
		t.Errorf("spoken lines = %v, want the closing line once", got)
	}
	if !actions.isArmed() {
		t.Error("the closing target did not arm the ending")
	}
}

func TestASentRepromptLeavesTheWatchToTheNextTurn(t *testing.T) {
	session, model := startSilentCall(t, nil)

	awaitBridgeEvent(t, session, EventTypeNoInput)
	if err := session.SendCue("are you there"); err != nil {
		t.Fatalf("cue: %v", err)
	}
	// The reprompt starts a turn, which is what restarts the watch; with none
	// here there must be no second NO_INPUT and no second timer.
	expectNoEvent(t, session, EventTypeNoInput, 4*rewatchTimeout)
	if got := model.recordedUserText(); len(got) != 1 {
		t.Errorf("cues = %v, want one", got)
	}
}

func TestARewatchedWatchIsCancelledByTheCallerSpeaking(t *testing.T) {
	session, _ := startSilentCall(t, errNoTextCue)

	awaitBridgeEvent(t, session, EventTypeNoInput)
	session.RewatchDeadAir()
	session.cancelDeadAirWatch() // the caller speaks before the new timer ends
	expectNoEvent(t, session, EventTypeNoInput, 4*rewatchTimeout)
}

func TestARewatchRequestedAfterTheCallerSpokeIsDropped(t *testing.T) {
	session, _ := startSilentCall(t, errNoTextCue)

	awaitBridgeEvent(t, session, EventTypeNoInput)
	// The caller spoke between the timer firing and the request.
	session.cancelDeadAirWatch()
	session.RewatchDeadAir()
	expectNoEvent(t, session, EventTypeNoInput, 4*rewatchTimeout)
}

func TestARewatchOnAFiredWatchReportsAgain(t *testing.T) {
	session, _ := startSilentCall(t, errNoTextCue)

	for range 3 {
		awaitBridgeEvent(t, session, EventTypeNoInput)
		session.RewatchDeadAir()
	}
}

func TestARewatchAfterCloseIsANoOp(t *testing.T) {
	session, _ := startSilentCall(t, errNoTextCue)
	awaitBridgeEvent(t, session, EventTypeNoInput)

	session.Close(t.Context())
	for range 5 {
		session.RewatchDeadAir() // never blocks, nothing listens
	}
	if got := len(session.rearm); got > 1 {
		t.Errorf("%d re-arm requests queued, want at most one", got)
	}
}
