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
// on the channel (Session.endResponse), so the watcher cannot publish first by
// construction.
//
// The fake leg drains at once, so a turn's PLAYBACK_DONE follows its marker by
// one 20 ms tick: a delay of more than that between the handoff and the emit
// was enough to swap the two on the old order.

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

	var order []Event
	deadline := time.After(2 * time.Second)
	for len(order) < 2 {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatal("the stream closed")
			}
			if event.Type == EventTypeTurnDone || event.Type == EventTypePlaybackDone {
				order = append(order, event)
			}
		case <-deadline:
			t.Fatalf("saw %v", order)
		}
	}
	if order[0].Type != EventTypeTurnDone || order[1].Type != EventTypePlaybackDone {
		t.Fatalf("order = %v then %v, want TURN_DONE then PLAYBACK_DONE", order[0].Type, order[1].Type)
	}
	if order[0].Turn != order[1].Turn || !order[0].HasAudio || !order[1].HasAudio {
		t.Errorf("TURN_DONE (turn %d, audio %v) and PLAYBACK_DONE (turn %d, audio %v) are not one turn's",
			order[0].Turn, order[0].HasAudio, order[1].Turn, order[1].HasAudio)
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

	waitUntil(t, "the wall to close the call", func() bool { return len(model.spokenLines()) > 0 })
	if got := model.spokenLines(); len(got) != 1 || got[0] != "Thank you for calling, goodbye." {
		t.Fatalf("spoken lines = %v, want the closing announce once", got)
	}
}

// An armed hangup whose next turn says something other than the closing line
// waits for the line (#262): the turn's TURN_DONE judges its words and asks for
// the line again, and that turn's playback does not run the ending. On the old
// order the turn's PLAYBACK_DONE could come first, before the turn was judged
// (notLineThrough), and ran the hangup: the caller never heard the line (#627).
func TestAnArmedHangupIsNotRunOnThePlaybackOfATurnThatIsNotTheLine(t *testing.T) {
	h, sink := startOwedLinePath(t, provider.QwenProfile(), "en", 0)
	line := owedLines["en"]

	h.callTool(t, flow.ToolHangup, goodbyeHangup)
	h.endResponse() // the turn that carried the tool call; no audio

	h.sayTurn(notTheLine["en"])
	// The turn is judged (the line is asked for) or its playback ran the
	// hangup first; whichever came first is the verdict.
	waitUntil(t, "the turn after the hangup to be judged or to end the call", func() bool {
		return len(h.model.spokenLines()) > 0 || h.session.isClosed()
	})
	if h.session.isClosed() {
		t.Fatal("the armed hangup ran on the playback of a turn that was not the closing line")
	}
	if got := h.model.spokenLines(); len(got) != 1 || got[0] != line {
		t.Fatalf("lines asked for = %q, want the closing line %q", got, line)
	}
	// That turn's playback follows its judgement and must not end the call.
	h.stayUp(t, "the playback of a turn that was not the line ran the armed hangup")
	if !h.actions.isArmed() {
		t.Fatal("the ending was disarmed before the line played")
	}

	h.sayTurn(line)
	h.awaitCallEnded(t, 2*time.Second)
	if n := strings.Count(sink.String(), "hanging up after the farewell"); n != 1 {
		t.Errorf("the hangup ran %d times, want once", n)
	}
	if n := len(h.model.spokenLines()); n != 1 {
		t.Errorf("lines asked for = %d, want exactly one", n)
	}
	if strings.Contains(sink.String(), "no closing line was generated") {
		t.Errorf("the call ended on the cap:\n%s", sink.String())
	}
}

// An unhung goodbye (the model says goodbye in words and calls nothing) closes
// the call ownFarewellGrace after its playback (#453): the grace starts at the
// goodbye's PLAYBACK_DONE, from the verdict its TURN_DONE recorded. On the old
// order a PLAYBACK_DONE that won the race found no goodbye to start it for, and
// only the dead-air backstop ended the call, seconds late (#627). The backstop
// is on here, at its default, so the test tells the two closes apart.
func TestAnUnhungFarewellClosesAfterTheGraceNotAtTheBackstop(t *testing.T) {
	clock := &graceClock{}
	h := newClaimsHarnessSetup(t, provider.OpenAIProfile(), farewellFlow, "en", "",
		Config{BargeGuard: -1, NoInput: defaultNoInput},
		func(h *claimsHarness) { h.o.budgetAfter = clock.after })
	r := &farewellRig{claimsHarness: h, clock: clock}

	r.turn(bye("en"), "", "")
	r.clock.awaitWaits(t, ownFarewellGrace, 1) // the goodbye has played
	if r.session.isClosed() {
		t.Fatal("the call was hung up before the grace ran out")
	}
	r.clock.fireNewest(t, ownFarewellGrace)
	r.awaitHungUp(t)

	logs := r.logs.String()
	if !strings.Contains(logs, "rule=GRACE") {
		t.Errorf("the call was not ended by the grace:\n%s", logs)
	}
	if strings.Contains(logs, "rule=DEAD_AIR") {
		t.Errorf("the dead-air backstop ended the call:\n%s", logs)
	}
	r.assertNeverReopened(t)
}
