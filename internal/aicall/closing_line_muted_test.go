// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Issue #441: the model's own goodbye, said in the response that calls hangup,
// and the terminal phase's announce were both heard. Into a terminal phase that
// has an announce, the announce is the only goodbye: the response that made the
// call is muted locally, from the call on.

// mutedPath is a toolPath whose transcript is kept, with the call's log.
type mutedPath struct {
	*toolPath
	logs        *syncBuffer
	callID      uuid.UUID
	transcripts *fakeTranscripts
	flush       func()
}

func startMutedPath(t *testing.T, profile provider.Profile, lang, flowJSON string) *mutedPath {
	t.Helper()
	logs := &syncBuffer{}
	var actionsRef atomic.Pointer[callActions]
	log := slog.New(slog.NewTextHandler(logs, nil))
	// startBridgeWith, with the call's log on the session too: the mute line
	// is the session's.
	leg := newFakeLeg(media.LawMu)
	model := newFakeModel()
	session, err := New(leg, model, profile, Config{
		BargeGuard: -1, NoInput: -1, Logger: log,
		Session: provider.SessionConfig{
			Instructions: "answer the phone", Turn: provider.DefaultTurnDetection(),
		},
		IsEndingArmed: func() bool {
			a := actionsRef.Load()
			return a != nil && a.isArmed()
		},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, &fakeSwitch{})
	o.cfg.Profile = profile
	spec, err := flow.Load([]byte(flowJSON))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, lang, map[string]any{"name": "Ann"}, log)
	actions := &callActions{orchestrator: o, session: session, log: log, callerChannel: "chan-9"}
	callID := uuid.Must(uuid.NewV7())
	recorder, transcripts, flush := recorderWithTranscript(t, callID, time.Now())
	actions.recorder = recorder
	actionsRef.Store(actions)
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	h := &toolPath{session: session, leg: leg, model: model, actions: actions,
		engine: engine, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		o.drive(t.Context(), session, runtime, actions, recorder, sessionBudget{}, log)
	}()
	return &mutedPath{toolPath: h, logs: logs, callID: callID, transcripts: transcripts, flush: flush}
}

// botLines is every bot line of the transcript, joined.
func (h *mutedPath) botLines() string {
	// The transcript is closed only once the drive loop has stopped posting.
	h.session.Close(context.Background())
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		panic("the drive loop did not stop")
	}
	h.flush()
	var all []string
	for _, line := range h.transcripts.get(h.callID) {
		if line.Speaker != store.SpeakerBot || line.Kind != store.TranscriptKindText {
			continue
		}
		raw, _ := json.Marshal(line)
		all = append(all, string(raw))
	}
	return strings.Join(all, "\n")
}

// goodbyeFrames is how many frames of audio n milliseconds are.
func frames(n int) []byte { return make([]byte, n/frameDurationMs*media.FrameSamples) }

// callAndAwait has the model start a response that calls the tool, and waits
// for the answer, so what follows arrives after the call has been judged.
func (h *mutedPath) callAndAwait(t *testing.T, tool, args string) {
	t.Helper()
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
		ToolCallID: "fc_1", ToolName: tool, ToolArgs: args}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.model.recordedToolResults()) > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the call was never answered")
}

// awaitFrames waits until the leg has been handed n frames in all.
func (h *mutedPath) awaitFrames(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.leg.sentFrames()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the leg was handed %d frames, want %d", len(h.leg.sentFrames()), n)
}

// settle waits for the events sent so far to have been handled.
func (h *mutedPath) settle() { time.Sleep(60 * time.Millisecond) }

// announceTurn is the turn that answers the result: the announce, said once.
func (h *mutedPath) announceTurn(words string) {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(100)}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: words, IsFinal: true}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

var mutedWords = map[string][2]string{
	"en": {"Goodbye, take care!", "OK, I've noted that."},
	"zh": {"好的，再见！", "好的，我帮您记下了。"},
}

// The audio of the response that made the call arrives after it: none of it is
// played, whether it is a goodbye or not, and the announce plays once and ends
// the call.
func TestTheAnnounceIsTheOnlyGoodbye(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		for i, kind := range []string{"a goodbye", "not a goodbye"} {
			t.Run(lang+" "+kind, func(t *testing.T) {
				h := startMutedPath(t, provider.OpenAIProfile(), lang, owedLineFlow)
				words := mutedWords[lang][i]

				h.callAndAwait(t, flow.ToolHangup, `{}`)
				out := h.model.recordedToolResults()[0].output
				if got, want := hintOf(t, out), provider.SayExactly(owedLines[lang], lang); got != want {
					t.Fatalf("hint = %q, want the line %q", got, want)
				}
				h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(600)}
				h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: words, IsFinal: true}
				h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
				h.settle()
				if n := len(h.leg.sentFrames()); n != 0 {
					t.Fatalf("%d frames of the muted turn were played, want 0", n)
				}
				if h.isCallEnded() {
					t.Fatal("the call ended before the announce was heard")
				}

				h.announceTurn(owedLines[lang])
				h.awaitCallEnded(t, 2*time.Second)
				if n := len(h.leg.sentFrames()); n != 5 {
					t.Errorf("%d frames played, want the announce's 5", n)
				}
				if n := len(h.model.recordedToolResults()); n != 1 {
					t.Errorf("tool results = %d, want 1", n)
				}
				if got := h.model.spokenLines(); len(got) != 0 {
					t.Errorf("spoken lines = %v, want none: the line rides in the result", got)
				}
				if got := h.model.recordedInterrupts(); len(got) != 0 {
					t.Errorf("the provider was told of an interruption: %v (no cancel, no trim)", got)
				}
				// The words nobody heard are not lines the bot said.
				lines := h.botLines()
				if strings.Contains(lines, words) {
					t.Errorf("the transcript has the muted words %q:\n%s", words, lines)
				}
				if !strings.Contains(lines, owedLines[lang][:6]) {
					t.Errorf("the transcript lost the announce:\n%s", lines)
				}
				logs := h.logs.String()
				if n := strings.Count(logs, "muted the turn"); n != 1 {
					t.Errorf("%d mute lines, want 1:\n%s", n, logs)
				}
				for _, want := range []string{"tool=hangup", "playedMs=0", "flushedMs=0", "droppedChunks=1",
					"left a muted turn's words out of the transcript", words} {
					if !strings.Contains(logs, want) {
						t.Errorf("the mute line lacks %s:\n%s", want, logs)
					}
				}
			})
		}
	}
}

// Part of the goodbye is playing when the call arrives: the rest is flushed and
// the log says how much the caller heard.
func TestThePartOfAGoodbyeAfterTheCallIsFlushed(t *testing.T) {
	h := startMutedPath(t, provider.OpenAIProfile(), "en", owedLineFlow)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(600)}
	h.awaitFrames(t, 30)
	// 10 of the 30 frames are still queued when the call arrives: 400 ms played.
	h.leg.holdFrames(10)
	h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
		ToolCallID: "fc_1", ToolName: flow.ToolHangup, ToolArgs: `{}`}
	deadline := time.Now().Add(2 * time.Second)
	for h.leg.clears() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	h.leg.holdFrames(0)
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(200)}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.settle()
	if n := len(h.leg.sentFrames()); n != 30 {
		t.Fatalf("%d frames handed to the leg, want the 30 before the call", n)
	}
	logs := h.logs.String()
	for _, want := range []string{"playedMs=400", "flushedMs=200", "droppedChunks=1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the mute line lacks %s:\n%s", want, logs)
		}
	}
	h.announceTurn(owedLines["en"])
	h.awaitCallEnded(t, 2*time.Second)
	if n := len(h.leg.sentFrames()); n != 35 {
		t.Errorf("%d frames in all, want 30 + the announce's 5", n)
	}
}

// A response that calls hangup with no audio at all: nothing to mute, and the
// call still ends, here on the grace cap because the announce never plays.
func TestAHangupWithNoAudioEndsInsideTheGraceCap(t *testing.T) {
	h := startMutedPath(t, provider.OpenAIProfile(), "en", owedLineFlow)
	h.actions.mu.Lock()
	h.actions.graceCap = 400 * time.Millisecond
	h.actions.mu.Unlock()
	start := time.Now()
	h.callAndAwait(t, flow.ToolHangup, `{}`)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.awaitCallEnded(t, 3*time.Second)
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("the call ended after %v, before the cap", time.Since(start))
	}
	if n := len(h.leg.sentFrames()); n != 0 {
		t.Errorf("%d frames played, want none", n)
	}
	if got := h.model.spokenLines(); len(got) != 0 {
		t.Errorf("spoken lines = %v, want none", got)
	}
}

// The goodbye transcript lands after the muted turn's TURN_DONE: it is not a
// line the bot said, and it is not the closing line.
func TestAMutedTurnsLateTranscriptIsNotHeardNorTheClosingLine(t *testing.T) {
	h := startMutedPath(t, provider.OpenAIProfile(), "en", owedLineFlow)
	h.callAndAwait(t, flow.ToolHangup, `{}`)
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.settle()
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
		Text: "Goodbye, take care!", IsFinal: true}
	h.settle()
	h.actions.mu.Lock()
	kept := len(h.actions.said)
	h.actions.mu.Unlock()
	if kept != 0 {
		t.Errorf("the late goodbye was kept as words held against the line")
	}
	h.announceTurn(owedLines["en"])
	h.awaitCallEnded(t, 2*time.Second)
	if lines := h.botLines(); strings.Contains(lines, "take care") {
		t.Errorf("the transcript has the late goodbye:\n%s", lines)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "left a muted turn's words out of the transcript") ||
		!strings.Contains(logs, `text="Goodbye, take care!"`) {
		t.Errorf("the dropped words were not logged:\n%s", logs)
	}
}

// Doubao: the same muting, and the line goes out through SpeakText once the
// batch closes.
func TestOnDoubaoTheTurnIsMutedAndTheLineIsSpoken(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h := startMutedPath(t, provider.DoubaoProfile(), lang, owedLineFlow)
			h.callAndAwait(t, flow.ToolHangup, `{}`)
			h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(600)}
			h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
				Text: mutedWords[lang][0], IsFinal: true}
			h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
			lines := h.awaitLinesSpoken(t, 1)
			if lines[0] != owedLines[lang] {
				t.Errorf("spoken line = %q, want %q", lines[0], owedLines[lang])
			}
			if got := h.model.spokenLinesClosing(); len(got) != 1 || !got[0] {
				t.Errorf("closing flags = %v, want one closing line", got)
			}
			if n := len(h.leg.sentFrames()); n != 0 {
				t.Errorf("%d frames of the muted turn were played, want 0", n)
			}
			h.announceTurn(owedLines[lang])
			h.awaitCallEnded(t, 2*time.Second)
			if n := len(h.leg.sentFrames()); n == 0 {
				t.Error("the announce was not played")
			}
			if strings.Contains(h.botLines(), mutedWords[lang][0]) {
				t.Error("the transcript has the muted words")
			}
		})
	}
}

// A terminal phase without an announce: the model's own words are the
// goodbye, and nothing is muted.
func TestATerminalPhaseWithoutAnAnnounceIsNotMuted(t *testing.T) {
	noLine := strings.Replace(owedLineFlow, `"announce": {
				"en": "Thank you {slots.name}, goodbye 👋",
				"zh": "谢谢您{slots.name}，再见 👋"
			},`, "", 1)
	h := startMutedPath(t, provider.OpenAIProfile(), "en", noLine)
	h.callAndAwait(t, flow.ToolHangup, `{}`)
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(200)}
	h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
		Text: "Goodbye, take care!", IsFinal: true}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.settle()
	if n := len(h.leg.sentFrames()); n != 10 {
		t.Errorf("%d frames played, want the model's own 10", n)
	}
	if strings.Contains(h.logs.String(), "muted the turn") {
		t.Error("a turn was muted")
	}
	if !strings.Contains(h.botLines(), "take care") {
		t.Error("the model's goodbye is missing from the transcript")
	}
}

// A move into a phase that is not terminal is unchanged even with an announce.
func TestAMoveIntoANonTerminalPhaseIsNotMuted(t *testing.T) {
	h := startMutedPath(t, provider.OpenAIProfile(), "en", owedLineFlow)
	h.callAndAwait(t, flow.ToolTakeMessage, `{"message":"a"}`)
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(200)}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.settle()
	if n := len(h.leg.sentFrames()); n != 10 {
		t.Errorf("%d frames played, want 10", n)
	}
	if strings.Contains(h.logs.String(), "muted the turn") {
		t.Error("a turn was muted")
	}
}

// The announce's own turn is never muted, and the caller talking over it is a
// barge-in like any other.
func TestTheCallerCanTalkOverTheAnnounce(t *testing.T) {
	h := startMutedPath(t, provider.OpenAIProfile(), "en", owedLineFlow)
	h.callAndAwait(t, flow.ToolHangup, `{}`)
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(400)}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	h.settle()

	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(1000)}
	h.awaitFrames(t, 50)
	h.leg.holdFrames(30)
	h.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	deadline := time.Now().Add(2 * time.Second)
	for len(h.model.recordedInterrupts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	got := h.model.recordedInterrupts()
	if len(got) != 1 || got[0].reason != provider.InterruptReasonSpeech || got[0].playedMs != 400 {
		t.Fatalf("interrupts = %v, want one SPEECH at 400 ms of the announce", got)
	}
}

// Turn identity: only the turn that made the call is muted. A turn that has
// moved on is left alone, and so are the announce's and every later turn.
func TestMuteTurnTouchesOnlyItsOwnTurn(t *testing.T) {
	session, leg, model := startBridgeWith(t, provider.OpenAIProfile(), Config{BargeGuard: -1, NoInput: -1})
	awaitBridgeEvent(t, session, EventTypeReady)
	play := func(n int) {
		model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(n * 20)}
		model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	}
	waitFrames := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for len(leg.sentFrames()) < want && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		if got := len(leg.sentFrames()); got != want {
			t.Fatalf("%d frames played, want %d", got, want)
		}
	}

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	for session.currentTurn() != 1 {
		time.Sleep(time.Millisecond)
	}
	session.MuteTurn(1, "hangup")
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(200)}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	time.Sleep(40 * time.Millisecond)
	waitFrames(0)

	play(4) // the announce's turn
	waitFrames(4)
	play(3) // a later one
	waitFrames(7)
	session.MuteTurn(1, "hangup") // stale: turn 1 is long over
	play(2)
	waitFrames(9)
	if !session.isMutedTurn(1) || session.isMutedTurn(2) {
		t.Error("the muted turn is not exactly turn 1")
	}
}

// A stray isFarewellSpoken from a model that still believes in it is ignored.
func TestAStrayFarewellArgumentIsIgnored(t *testing.T) {
	for _, arg := range []string{`{"isFarewellSpoken":true}`, `{"isFarewellSpoken":false}`} {
		h := startMutedPath(t, provider.OpenAIProfile(), "en", owedLineFlow)
		h.callAndAwait(t, flow.ToolHangup, arg)
		out := h.model.recordedToolResults()[0].output
		if got, want := hintOf(t, out), provider.SayExactly(owedLines["en"], "en"); got != want {
			t.Fatalf("hint = %q, want %q", got, want)
		}
		if !h.actions.isArmed() {
			t.Fatal("the hangup was not armed")
		}
		h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(200)}
		h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		h.settle()
		h.announceTurn(owedLines["en"])
		h.awaitCallEnded(t, 2*time.Second)
		if n := len(h.leg.sentFrames()); n != 5 {
			t.Errorf("%d frames played, want the announce's 5", n)
		}
	}
}

// The session pump runs ahead of the orchestrator: the muted response can have
// ended, its whole audio queued and its TURN_DONE and playback marker out,
// when the mute runs. The queue is flushed, the mute is logged once with what
// was heard, the muted turn's playback never reports done, and the announce's
// turn plays in full and is the one whose PLAYBACK_DONE comes.
func TestAMuteAfterTheTurnHasEndedFlushesItsQueueAndItsPlaybackNeverReports(t *testing.T) {
	logs := &syncBuffer{}
	leg := newFakeLeg(media.LawMu)
	model := newFakeModel()
	session, err := New(leg, model, provider.OpenAIProfile(), Config{
		BargeGuard: -1, NoInput: -1,
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
		Session: provider.SessionConfig{Instructions: "x", Turn: provider.DefaultTurnDetection()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	awaitBridgeEvent(t, session, EventTypeReady)

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(600)}
	// 10 of the 30 frames are still queued when the response ends and when the
	// mute arrives.
	deadline := time.Now().Add(2 * time.Second)
	for len(leg.sentFrames()) < 30 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	leg.holdFrames(10)
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	done := awaitBridgeEvent(t, session, EventTypeTurnDone)
	if !done.HasAudio || done.Turn != 1 {
		t.Fatalf("TURN_DONE = %+v, want turn 1 with audio", done)
	}

	session.MuteTurn(1, "hangup")
	if leg.clears() != 1 {
		t.Errorf("%d flushes, want 1", leg.clears())
	}
	leg.holdFrames(0) // what was queued has gone: flushed, not played
	logged := logs.String()
	if n := strings.Count(logged, "muted the turn"); n != 1 || !strings.Contains(logged, "playedMs=400") ||
		!strings.Contains(logged, "flushedMs=200") {
		t.Errorf("mute log wrong (%d lines):\n%s", n, logged)
	}
	// The muted turn's playback is stale: nothing reports it.
	select {
	case event := <-session.Events():
		if event.Type == EventTypePlaybackDone {
			t.Fatalf("PLAYBACK_DONE for the muted turn: %+v", event)
		}
	case <-time.After(200 * time.Millisecond):
	}

	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: frames(100)}
	model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	playback := awaitBridgeEvent(t, session, EventTypePlaybackDone)
	if playback.Turn != 2 || !playback.HasAudio {
		t.Errorf("PLAYBACK_DONE = %+v, want the announce's turn 2", playback)
	}
	if n := len(leg.sentFrames()); n != 35 {
		t.Errorf("%d frames in all, want 30 + the announce's 5", n)
	}
}
