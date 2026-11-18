// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// #610. On an engine that cuts by itself (qwen) a call that listens first
// (global.isCallerFirst) has its opening generated in full while it is held for
// the callee, and released at the end of the wait. A one-word acknowledgement
// ("好") then cuts it 2.8 s into playback: a session-side barge-in over a turn
// that has finished generating (wasGenerating=false). The model answers with a
// tool that moves the call. Damage limit (a) of docs/design/11-backchannels.md
// refuses that tool; in production it ran, with no verdict in the log and no
// CUSTOMER transcript row for the utterance: the engine's transcriber wrote
// nothing for the word, and a blank final was judged words. Words over the
// opening that no transcript makes known are now judged as a greeting is.

// heldOpeningLine is the call's opening, said as written: two sentences, the
// second the offer, so a cut can be dated in its final sentence (#465's
// estimate, cutInFinalSentence).
const heldOpeningLine = "您好，我是安，给您来电是想介绍一下您套餐的续约优惠。现在续约可以享受八折，请问您需要吗？"

// heldOpeningFrames is the opening's length in 20 ms frames (11 s, as on the
// production call), heardFrames how much of it the callee heard before the cut.
const (
	heldOpeningFrames = 550
	heardFrames       = 140 // 2800 ms
)

// heldOpeningRig drives a qwen call that listens first through the
// orchestrator, on quietFlow in zh, with the verdict wait and the replay's
// backstop on clocks the test owns and the uplink clock never ticking, so the
// wait to open ends only when the test ends it.
type heldOpeningRig struct {
	session *Session
	leg     *fakeLeg
	model   *fakeModel
	runtime *flow.Runtime
	actions *callActions
	logs    *syncBuffer
	wait    *verdictWait
	replay  *holdClock
}

func newHeldOpeningRig(t *testing.T) *heldOpeningRig {
	t.Helper()
	profile := provider.QwenProfile()
	r := &heldOpeningRig{leg: newFakeLeg(media.LawMu), model: newFakeModel(), logs: &syncBuffer{},
		wait: newVerdictWait(), replay: &holdClock{}}
	log := slog.New(slog.NewTextHandler(r.logs, nil))

	o, err := NewOrchestrator(OrchestratorConfig{
		Catalog: &fakeCatalog{queues: []catalog.Queue{
			{ID: uuid.Must(uuid.NewV7()), Name: "support", ExtNumber: "7001", IsEnabled: true},
		}},
		Flows:   fakeFlows{},
		Switch:  &orderedSwitch{},
		Profile: profile,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	spec, err := flow.Load([]byte(quietFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "zh", nil, log)
	recorder := newStartedRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	facts := testFacts()
	facts.callType, facts.isPlacedToCustomer = callTypeOutbound, true
	r.actions = &callActions{orchestrator: o, log: log, callerChannel: "chan-9", recorder: recorder, facts: facts}
	r.runtime = flow.NewRuntime(engine, r.actions, flow.NewBackend(""), nil, log)

	// What handle builds for a campaign call on a flow with isCallerFirst
	// (orchestrator.go): the session's logs go to the same buffer, so the
	// verdict lines can be looked for.
	session, err := New(r.leg, r.model, profile, Config{
		Session: provider.SessionConfig{
			Instructions: r.runtime.Instructions(),
			Language:     "zh",
			Turn:         provider.DefaultTurnDetection(),
			OpeningText:  heldOpeningLine,
		},
		BargeGuard:        -1,
		NoInput:           -1,
		IsCallerFirst:     true,
		OnCutAcknowledged: r.actions.onCutAcknowledged,
		IsEndingArmed:     r.actions.isArmed,
		Logger:            log,
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	session.cutWait = r.wait.after
	session.holdAfter = r.replay.after
	session.uplinkTicks = make(chan time.Time)
	r.actions.session = session
	r.session = session
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	awaitBridgeEvent(t, session, EventTypeReady)

	go func() {
		o.drive(t.Context(), session, r.runtime, r.actions, recorder, sessionBudget{}, log)
		r.actions.disarm()
	}()
	return r
}

// heldOpeningCutByAcknowledgement plays the production call up to the model's
// tool call: the opening is generated in full while the call listens, the wait
// ends (TIMEOUT), the opening plays, the callee's speech cuts it after 2.8 s,
// the engine detects the end of the speech and creates its reply, which starts
// to speak. The caller's final transcript is left to the test.
func (r *heldOpeningRig) heldOpeningCutByAcknowledgement(t *testing.T) {
	t.Helper()
	r.cutHeldOpening(t, heardFrames)
}

// cutHeldOpening is heldOpeningCutByAcknowledgement with the cut after heard
// frames of the opening.
func (r *heldOpeningRig) cutHeldOpening(t *testing.T, heard int) {
	t.Helper()
	// Nothing the leg is given plays out by itself: the opening stays queued.
	r.leg.holdFrames(1 << 20)

	// The whole opening, while the call listens: qwen makes it faster than it
	// plays. PCM16 at the provider's output rate, 50 frames per delta.
	frameBytes := 2 * media.RateProviderOut / 50
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	for sent := 0; sent < heldOpeningFrames; sent += 50 {
		r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 50*frameBytes)}
	}
	r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: heldOpeningLine, IsFinal: true}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	// The model's events are buffered: the wait may end only once the pump has
	// taken every one of them, so that all of the opening is held.
	waitLong(t, "the pump to hold the whole opening", func() bool { return len(r.model.events) == 0 })

	// The callee said nothing: the wait ends at its timeout and the opening is
	// released to the leg in order.
	r.session.endListening("TIMEOUT", callerFirstWait, noSpeech)
	// Hundreds of frames go through the downlink converter here: under -race on
	// a loaded runner that takes longer than waitFor's bound.
	waitLong(t, "the held opening to be released and finish", func() bool {
		r.session.mu.Lock()
		defer r.session.mu.Unlock()
		return r.session.turnSeq == 1 && !r.session.isResponding && r.session.framesQueued >= heldOpeningFrames-2
	})
	if !strings.Contains(r.logs.String(), "the opening was held for the callee") {
		t.Fatalf("the opening was not held (log):\n%s", r.logs.String())
	}
	r.session.mu.Lock()
	queued := r.session.framesQueued
	r.session.mu.Unlock()
	// The callee has heard the first 2.8 s; the rest is still queued.
	r.leg.holdFrames(queued - heard)

	// "好": qwen's speech start. The session cuts the opening (its own barge-in:
	// the opening finished generating, its tail is playing).
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
	waitFor(t, "the cut to be recorded", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil
	})
	r.leg.holdFrames(0)
	logs := r.logs.String()
	if want := fmt.Sprintf("playedMs=%d", heard*frameDurationMs); !strings.Contains(logs, "the caller took the floor back") ||
		!strings.Contains(logs, want) || !strings.Contains(logs, "wasGenerating=false") {
		t.Fatalf("the cut is not the one asked for (want %s wasGenerating=false):\n%s", want, logs)
	}

	// The engine's turn detection: the speech ends, the reply is created and
	// starts to say something before it calls the tool.
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: "u1"}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 5*frameBytes)}
}

// waitLong is waitFor with a bound for work that is slow under -race on a busy
// machine; it costs nothing when the condition holds sooner.
func waitLong(t *testing.T, what string, isDone func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !isDone() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *heldOpeningRig) agree(id string) {
	r.model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: id,
		ToolName: "classify", ToolArgs: `{"value":"agree"}`}
}

func (r *heldOpeningRig) callerFinal(item, text string) {
	r.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: text, IsFinal: true}
}

func (r *heldOpeningRig) ended() {
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// awaitResults waits for n tool answers and returns the last.
func (r *heldOpeningRig) awaitResults(t *testing.T, n int) string {
	t.Helper()
	waitFor(t, "the tool call to be answered", func() bool { return len(r.model.recordedToolResults()) >= n })
	got := r.model.recordedToolResults()
	return got[len(got)-1].output
}

func (r *heldOpeningRig) awaitResult(t *testing.T) string {
	t.Helper()
	return r.awaitResults(t, 1)
}

// awaitVerdictWait waits for the tool call to wait for the verdict on the cut.
func (r *heldOpeningRig) awaitVerdictWait(t *testing.T) {
	t.Helper()
	waitFor(t, "the tool call to wait for the verdict", func() bool { return len(r.wait.waits()) == 1 })
}

// probe has a later response call a tool that moves nothing and waits for its
// answer: the orchestrator handles events in order, so everything sent before
// it has been handled.
func (r *heldOpeningRig) probe(t *testing.T) {
	t.Helper()
	before := len(r.model.recordedToolResults())
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.model.events <- provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "fc_probe",
		ToolName: "note", ToolArgs: `{"value":"probe"}`}
	r.ended()
	r.awaitResults(t, before+1)
}

// diagnosis says how the call got where it is: whether the tool call carried
// the cut, the cut's verdict, whether the call waited for it, and which of the
// verdict lines the log has.
func (r *heldOpeningRig) diagnosis() string {
	r.session.bc.mu.Lock()
	cut := r.session.bc.cut
	var verdict cutVerdict
	answerTurn := 0
	if cut != nil {
		verdict, answerTurn = cut.verdict, cut.answerTurn
	}
	r.session.bc.mu.Unlock()
	logs := r.logs.String()
	var lines []string
	for _, line := range []string{
		"the engine cut the bot for an acknowledgement",
		"the engine cut the opening for a greeting",
		"no verdict on the cut within the wait",
		"no verdict on the cut of the opening within the wait",
		"the final transcript of the words that cut the bot is blank",
	} {
		if strings.Contains(logs, line) {
			lines = append(lines, line)
		}
	}
	return fmt.Sprintf("cut answerTurn=%d verdict=%s verdictWaits=%d verdictLines=%q",
		answerTurn, verdict, len(r.wait.waits()), lines)
}

// assertRefused checks the moving tool was refused as a conversation and the
// call stayed in its entry phase.
func (r *heldOpeningRig) assertRefused(t *testing.T, out string) {
	t.Helper()
	if node := r.runtime.Engine().NodeID(); node != "offer" || !strings.Contains(out, `"ok":"0"`) {
		t.Errorf("the moving tool ran on an acknowledgement over the held opening: the call is in %q, tool result %s\n%s",
			node, out, r.diagnosis())
	}
}

// assertRan checks the moving tool ran and moved the call.
func (r *heldOpeningRig) assertRan(t *testing.T, out string) {
	t.Helper()
	if node := r.runtime.Engine().NodeID(); node != "agreed" || !strings.Contains(out, `"ok":"1"`) {
		t.Errorf("the moving tool did not run: the call is in %q, tool result %s\n%s", node, out, r.diagnosis())
	}
}

// assertOpeningSaidAgain checks the refusal tells the model the opening is said
// again, and that it is, once and word for word.
func (r *heldOpeningRig) assertOpeningSaidAgain(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "没有听清") || !strings.Contains(out, "开场白会再对他说一遍") {
		t.Errorf("tool result = %s, want the refusal for words not known over the opening", out)
	}
	r.assertSaidAgainOnce(t)
}

// assertSaidAgainOnce checks the opening is said again, once and word for word.
func (r *heldOpeningRig) assertSaidAgainOnce(t *testing.T) {
	t.Helper()
	waitFor(t, "the opening to be said again", func() bool { return len(r.model.spokenLines()) >= 1 })
	if got := r.model.spokenLines(); !slices.Equal(got, []string{heldOpeningLine}) {
		t.Errorf("spoken = %v, want the opening once, as written", got)
	}
}

// assertAcknowledgementRefusal checks a refusal as an acknowledgement, telling
// the model the opening is said again or not.
func assertAcknowledgementRefusal(t *testing.T, out string, isSaidAgain bool) {
	t.Helper()
	if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, "只是应了一声") {
		t.Errorf("tool result = %s, want a refusal as an acknowledgement", out)
	}
	if got := strings.Contains(out, "开场白会再对他说一遍"); got != isSaidAgain {
		t.Errorf("tool result = %s: says the opening is said again = %v, want %v", out, got, isSaidAgain)
	}
}

// mutedTurn is the turn the session muted, zero for none.
func (r *heldOpeningRig) mutedTurn() int {
	r.session.mu.Lock()
	defer r.session.mu.Unlock()
	return r.session.mutedTurn
}

// isOpeningOwed reports the session's owed-opening mark.
func (r *heldOpeningRig) isOpeningOwed() bool {
	r.session.bc.mu.Lock()
	defer r.session.bc.mu.Unlock()
	return r.session.bc.isOpeningOwed
}

// decisionLine is the orchestrator's line about a moving tool on a cut.
func (r *heldOpeningRig) decisionLine(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "a tool that could move the call answers a cut the engine made") {
			return line
		}
	}
	t.Fatalf("no decision line in the log:\n%s", r.logs.String())
	return ""
}

// 1. No final transcript ever arrives for the utterance; the model calls the
// moving tool 1.3 s after the cut. The callee has heard 2.8 s of an 11 s
// opening: the move is refused and the opening is said again, as for a
// greeting over it (#594).
func TestAMovingToolIsRefusedWhenAHeldOpeningIsCutAndNoFinalComes(t *testing.T) {
	r := newHeldOpeningRig(t)
	r.heldOpeningCutByAcknowledgement(t)
	r.agree("fc_1")
	r.awaitVerdictWait(t)
	r.wait.expire() // cutVerdictWait passes with no transcript
	r.ended()
	out := r.awaitResult(t)
	r.assertRefused(t, out)
	r.assertOpeningSaidAgain(t, out)
	line := r.decisionLine(t)
	for _, want := range []string{"tool=classify", "verdict=WORDS_UNKNOWN", "isOpening=true",
		"playedMs=2800", "waitedMs=", "isRefused=true"} {
		if !strings.Contains(line, want) {
			t.Errorf("decision line %q lacks %s", line, want)
		}
	}
	if !strings.Contains(r.logs.String(), "no verdict on the cut of the opening within the wait") {
		t.Errorf("the settled wait left no trace in the log:\n%s", r.logs.String())
	}
}

// 2. The same, with the final "好。" arriving 200 ms after the tool call, inside
// the verdict wait: an acknowledgement, refused, and the opening, cut before
// its final sentence, is said again with the model's reply muted.
func TestAMovingToolIsRefusedWhenAHeldOpeningIsCutAndTheFinalIsLate(t *testing.T) {
	r := newHeldOpeningRig(t)
	r.heldOpeningCutByAcknowledgement(t)
	r.agree("fc_1")
	r.awaitVerdictWait(t)
	r.callerFinal("u1", "好。")
	out := r.awaitResult(t)
	r.assertSaidAgainOnce(t)
	r.ended() // after the line: the reply was still being made when it was muted
	r.assertRefused(t, out)
	assertAcknowledgementRefusal(t, out, true)
	if line := r.decisionLine(t); !strings.Contains(line, "verdict=ACKNOWLEDGEMENT") ||
		!strings.Contains(line, "isOpeningSaidAgain=true") {
		t.Errorf("decision line %q, want the acknowledgement and the opening said again", line)
	}
	if got := r.mutedTurn(); got != 2 {
		t.Errorf("muted turn = %d, want the model's reply (turn 2)", got)
	}
}

// 3. The production signature: the provider's transcriber returns nothing for
// the "好" the model heard (a blank .completed), and the orchestrator records no
// CUSTOMER row for it. Blank words over an opening the callee has provably not
// heard are no answer to it: refused, and the opening is said again.
func TestAMovingToolIsRefusedWhenAHeldOpeningIsCutAndTheFinalIsEmpty(t *testing.T) {
	t.Run("before the tool call", func(t *testing.T) {
		r := newHeldOpeningRig(t)
		r.heldOpeningCutByAcknowledgement(t)
		r.callerFinal("u1", "")
		r.agree("fc_1")
		r.ended()
		out := r.awaitResult(t)
		r.assertRefused(t, out)
		r.assertOpeningSaidAgain(t, out)
		if !strings.Contains(r.logs.String(), "the final transcript of the words that cut the bot is blank") {
			t.Errorf("the blank final left no trace in the log:\n%s", r.logs.String())
		}
		if got := r.wait.waits(); len(got) != 0 {
			t.Errorf("the tool call waited %v for a verdict that was already in", got)
		}
	})
	t.Run("while the tool call waits", func(t *testing.T) {
		r := newHeldOpeningRig(t)
		r.heldOpeningCutByAcknowledgement(t)
		r.agree("fc_1")
		r.awaitVerdictWait(t)
		r.callerFinal("u1", "")
		r.ended()
		out := r.awaitResult(t)
		r.assertRefused(t, out)
		r.assertOpeningSaidAgain(t, out)
	})
}

// Words over the held opening are an answer: the tool runs and nothing is said
// again.
func TestWordsOverAHeldOpeningStillRunTheTool(t *testing.T) {
	r := newHeldOpeningRig(t)
	r.heldOpeningCutByAcknowledgement(t)
	r.callerFinal("u1", "可以，帮我续约吧。")
	r.agree("fc_1")
	r.ended()
	r.assertRan(t, r.awaitResult(t))
	if got := r.model.spokenLines(); len(got) != 0 {
		t.Errorf("said %v, want no line", got)
	}
}

// A transcript that lands after the wait settled the cut is the caller's words
// and nothing more: the opening is not said a second time and the refusal
// stands.
func TestAFinalAfterTheSettledWaitDoesNotSayTheOpeningTwice(t *testing.T) {
	for _, tt := range []struct{ name, said string }{
		{"words", "好的，我同意续约。"},
		{"a greeting", "喂？"},
		{"blank", ""},
	} {
		said := tt.said
		t.Run(tt.name, func(t *testing.T) {
			r := newHeldOpeningRig(t)
			r.heldOpeningCutByAcknowledgement(t)
			r.agree("fc_1")
			r.awaitVerdictWait(t)
			r.wait.expire()
			r.ended()
			r.assertOpeningSaidAgain(t, r.awaitResult(t))

			r.callerFinal("u1", said)
			r.probe(t)
			if got := r.model.spokenLines(); len(got) != 1 {
				t.Errorf("spoken = %v, want the opening said again once", got)
			}
			if node := r.runtime.Engine().NodeID(); node != "offer" {
				t.Errorf("the call moved to %q on a late transcript", node)
			}
		})
	}
}

// Once the opening has been said again, words no transcript made known are
// judged as on any other turn: a cut of the replay with a blank final lets the
// tool run, and the opening is not said a third time. Refusing without the
// replay would leave the model only to ask again, on an engine whose
// transcriber has just dropped the callee's word.
func TestABlankFinalOverTheReplayRunsTheTool(t *testing.T) {
	r := newHeldOpeningRig(t)
	r.heldOpeningCutByAcknowledgement(t)
	r.callerFinal("u1", "")
	r.agree("fc_1")
	r.ended()
	r.assertOpeningSaidAgain(t, r.awaitResult(t))

	// The replay is a line, a turn of its own; the callee speaks over it.
	frameBytes := 2 * media.RateProviderOut / 50
	r.leg.holdFrames(40)
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
	r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 50*frameBytes)}
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"}
	waitFor(t, "the cut over the replay", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil && r.session.bc.cut.event.Turn == 3 && r.session.bc.cut.isOpening
	})
	r.leg.holdFrames(0)
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: "u2"}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.callerFinal("u2", "")
	r.agree("fc_2")
	r.ended()
	r.assertRan(t, r.awaitResults(t, 2))
	if got := r.model.spokenLines(); len(got) != 1 {
		t.Errorf("spoken = %v, want no third telling", got)
	}
}

// When the opening cannot be said again, words not known are not refused
// either: the tool runs as it always did. An armed ending is the session's to
// see, and it judges the cut as before; an ending decided otherwise (the time
// limit) is the orchestrator's, which declines the refusal.
func TestWordsNotKnownOverTheOpeningRunWhenItCannotBeSaidAgain(t *testing.T) {
	for _, tt := range []struct {
		name, verdict string
		end           func(a *callActions)
	}{
		{"an ending is armed", "verdict=NONE", func(a *callActions) { a.armed = func() {} }},
		{"the ending is decided", "verdict=WORDS_UNKNOWN", func(a *callActions) { a.isEndingLocked = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newHeldOpeningRig(t)
			r.heldOpeningCutByAcknowledgement(t)
			r.actions.mu.Lock()
			tt.end(r.actions)
			r.actions.mu.Unlock()
			r.agree("fc_1")
			r.awaitVerdictWait(t)
			r.wait.expire()
			r.ended()
			r.awaitResult(t)
			if line := r.decisionLine(t); !strings.Contains(line, tt.verdict) ||
				!strings.Contains(line, "isRefused=false") {
				t.Errorf("decision line %q, want %s and not refused", line, tt.verdict)
			}
			if got := r.model.spokenLines(); len(got) != 0 {
				t.Errorf("said %v, want no line", got)
			}
		})
	}
}

// A cut of a later turn is judged as before: a blank final is content, and a
// wait that runs out runs the tool as it always did.
func TestACutOfALaterTurnWithNoWordsStillRunsTheTool(t *testing.T) {
	for _, tt := range []struct {
		name    string
		isBlank bool
	}{{"blank final", true}, {"no final", false}} {
		t.Run(tt.name, func(t *testing.T) {
			r := newOpeningRig(t)
			r.started() // turn 1: the opening, heard
			r.ended()
			r.botSpeaksUntilCut(t, "u1") // turn 2
			r.started()
			if tt.isBlank {
				r.callerFinal("u1", "")
			}
			r.toolCall("fc_1", "classify", `{"value":"agree"}`)
			if !tt.isBlank {
				waitFor(t, "the tool call to wait for the verdict", func() bool { return len(r.wait.waits()) == 1 })
				r.wait.expire()
			}
			r.ended()
			r.awaitResult(t)
			if r.node() != "agreed" {
				t.Errorf("the call is in %q, want agreed", r.node())
			}
			if got := r.model.spokenLines(); len(got) != 0 {
				t.Errorf("said %v, want no line", got)
			}
		})
	}
}

// An acknowledgement that cuts the opening before its final sentence is no
// answer to an offer the callee has not heard (owner decision on #610, after a
// live call): a moving tool is refused, and the opening is said again once,
// with the model's reply muted, whether or not that reply calls a tool. A run
// of acknowledgements (#600) is the same.
func TestAnAcknowledgementOverAHeldOpeningSaysItAgain(t *testing.T) {
	t.Run("before the tool call", func(t *testing.T) {
		r := newHeldOpeningRig(t)
		r.heldOpeningCutByAcknowledgement(t)
		r.callerFinal("u1", "好。")
		r.agree("fc_1")
		out := r.awaitResult(t)
		r.assertSaidAgainOnce(t)
		r.ended()
		r.assertRefused(t, out)
		assertAcknowledgementRefusal(t, out, true)
		if got := r.mutedTurn(); got != 2 {
			t.Errorf("muted turn = %d, want the model's reply (turn 2)", got)
		}
		if !strings.Contains(r.logs.String(), "the opening was cut and is owed; saying it again") ||
			!strings.Contains(r.logs.String(), "verdict=ACKNOWLEDGEMENT") {
			t.Errorf("the replay's line does not name the verdict:\n%s", r.logs.String())
		}
	})
	t.Run("with no tool call", func(t *testing.T) {
		r := newHeldOpeningRig(t)
		r.heldOpeningCutByAcknowledgement(t)
		r.callerFinal("u1", "嗯，好的。")
		r.assertSaidAgainOnce(t)
		r.ended()
		if got := r.mutedTurn(); got != 2 {
			t.Errorf("muted turn = %d, want the model's reply (turn 2)", got)
		}
		r.probe(t)
		if got := r.model.spokenLines(); len(got) != 1 {
			t.Errorf("spoken = %v, want the opening said again once", got)
		}
	})
}

// An acknowledgement in the opening's final sentence, of an opening that had
// finished generating, is heard as the end of it: nothing is said again, the
// tool is refused as an acknowledgement as before, and the opening is no
// longer owed.
func TestAnAcknowledgementInTheOpeningsFinalSentenceIsNotReplayed(t *testing.T) {
	r := newHeldOpeningRig(t)
	r.cutHeldOpening(t, heldOpeningFrames-50) // 1 s of an 11 s opening left
	r.callerFinal("u1", "好。")
	r.agree("fc_1")
	r.ended()
	out := r.awaitResult(t)
	r.assertRefused(t, out)
	assertAcknowledgementRefusal(t, out, false)
	r.probe(t)
	if got := r.model.spokenLines(); len(got) != 0 {
		t.Errorf("said %v, want no line", got)
	}
	if r.isOpeningOwed() {
		t.Error("the opening is still owed after an acknowledgement of its end")
	}
	if !strings.Contains(r.logs.String(), "the acknowledgement fell in the opening's final sentence") {
		t.Errorf("the estimate left no trace in the log:\n%s", r.logs.String())
	}
}

// Once the opening has been said again, an acknowledgement over the replay is
// refused as an acknowledgement and gets no second replay.
func TestAnAcknowledgementOverTheReplayIsNotReplayedAgain(t *testing.T) {
	r := newHeldOpeningRig(t)
	r.heldOpeningCutByAcknowledgement(t)
	r.callerFinal("u1", "好。")
	r.agree("fc_1")
	r.awaitResult(t)
	r.assertSaidAgainOnce(t)
	r.ended()

	// The replay is a line, a turn of its own; the callee acknowledges over it.
	frameBytes := 2 * media.RateProviderOut / 50
	r.leg.holdFrames(40)
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
	r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 50*frameBytes)}
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"}
	waitFor(t, "the cut over the replay", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil && r.session.bc.cut.event.Turn == 3 && r.session.bc.cut.isOpening
	})
	r.leg.holdFrames(0)
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: "u2"}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.callerFinal("u2", "嗯。")
	r.agree("fc_2")
	r.ended()
	out := r.awaitResults(t, 2)
	r.assertRefused(t, out)
	assertAcknowledgementRefusal(t, out, false)
	r.probe(t)
	if got := r.model.spokenLines(); len(got) != 1 {
		t.Errorf("spoken = %v, want no second replay", got)
	}
}

// An acknowledgement over a later turn is what it always was: the tool is
// refused as an acknowledgement and nothing is said again.
func TestAnAcknowledgementOverALaterTurnIsUnchanged(t *testing.T) {
	r := newOpeningRig(t)
	r.started() // turn 1: the opening, heard
	r.ended()
	r.botSpeaksUntilCut(t, "u1") // turn 2
	r.started()
	r.callerFinal("u1", "okay")
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	r.ended()
	r.awaitResult(t)
	out := r.lastResult(t)
	if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, "acknowledged") ||
		strings.Contains(out, "opening is said") {
		t.Errorf("tool result = %s, want a refusal as an acknowledgement, no replay", out)
	}
	if r.node() != "offer" {
		t.Errorf("the call moved to %q", r.node())
	}
	r.assertNothingSpoken(t)
}
