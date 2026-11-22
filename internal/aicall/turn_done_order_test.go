// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// A turn's PLAYBACK_DONE can reach drive before its TURN_DONE when the
// playback watcher wins the race between the marker handoff and the event
// emit (#627): every playback consumer then judges the turn without the facts
// TURN_DONE recorded — the wall waits one reply too many, an armed ending can
// run without the closing line, the claim re-ask is skipped and the unhung
// farewell closes late. The marker is now handed over only after TURN_DONE is
// on the channel, so the watcher cannot publish first by construction.

// The ordering invariant itself: a finished turn's events arrive TURN_DONE
// first, PLAYBACK_DONE second — while the leg drains the frame as fast as it
// can, the scheduling window the bug needed is gone.
func TestATurnsPlaybackDoneFollowsItsTurnDone(t *testing.T) {
	t.Parallel()
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples)}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}

	var order []EventType
	deadline := time.After(2 * time.Second)
	for len(order) < 2 {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatal("the stream closed")
			}
			if event.Type == EventTypeTurnDone || event.Type == EventTypePlaybackDone {
				order = append(order, event.Type)
			}
		case <-deadline:
			t.Fatalf("saw %v", order)
		}
	}
	if order[0] != EventTypeTurnDone || order[1] != EventTypePlaybackDone {
		t.Fatalf("order = %v, want TURN_DONE then PLAYBACK_DONE", order)
	}
}

// The failure the ordering caused: with a one-turn wall, two caller turns each
// answered tool-lessly and heard must close the call through the closing
// line. On the old order a PLAYBACK_DONE that won the race judged the turn
// without crossedInTurn, and the wall waited one more reply (#627).
func TestDriveClosesAfterTwoToolLessTurnsWithAWallOfOne(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	session, _, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := flow.Load([]byte(strings.Replace(wallFlow, "ANNOUNCE",
		`"announce": "Thank you for calling, goodbye.",`, 1)))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	spec.Global.MaxTurnsWithoutTool = 1
	engine := flow.NewEngine(spec, "en", nil, log)
	actions := &callActions{
		orchestrator: o, session: session, log: log, callerChannel: "chan-9",
	}
	actions.recorder = newStartedRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	go o.drive(t.Context(), session, runtime, actions, actions.recorder, sessionBudget{}, log)

	for range 2 {
		model.events <- provider.Event{Type: provider.EventTypeInputTranscript,
			Text: "no", IsFinal: true}
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples)}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(model.spokenLines()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken lines = %v, want the closing announce once", got)
	}
}
