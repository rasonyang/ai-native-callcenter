// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The damage limits of an engine that cuts by itself (qwen here): the cut has
// happened, and the verdict on its words limits what follows from it
// (docs/design/11-backchannels.md).

// verdictWait is a cutWait whose expiry the test fires.
type verdictWait struct {
	mu    sync.Mutex
	asked []time.Duration
	ch    chan time.Time
}

func newVerdictWait() *verdictWait { return &verdictWait{ch: make(chan time.Time, 1)} }

func (w *verdictWait) after(d time.Duration) <-chan time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, d)
	return w.ch
}

func (w *verdictWait) expire() { w.ch <- time.Now() }

func (w *verdictWait) waits() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.asked...)
}

type damageRig struct {
	*quietHarness
	wait *verdictWait
}

func newDamageRig(t *testing.T) *damageRig {
	t.Helper()
	wait := newVerdictWait()
	h := newQuietHarnessBackend(t, quietFlow, false, quietConfig(), "", func(c *claimsHarness) {
		c.session.cutWait = wait.after
		c.session.cfg.OnCutAcknowledged = c.actions.onCutAcknowledged
	})
	return &damageRig{quietHarness: h, wait: wait}
}

// botSpeaksUntilCut plays audio that is still queued, and has the engine cut it
// for the utterance item; the next response is the one that answers the cut.
func (r *damageRig) botSpeaksUntilCut(t *testing.T, item string) {
	t.Helper()
	r.leg.holdFrames(40)
	r.started()
	r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*50)}
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item}
	// The orchestrator's loop reads the session's events, so the cut is waited
	// for in the session's own state.
	waitFor(t, "the cut to be recorded", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil
	})
	r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
	r.leg.holdFrames(0)
}

func (r *damageRig) callerFinal(item, text string) {
	r.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: text, IsFinal: true}
}

// awaitResult waits for the first tool answer to reach the model.
func (r *damageRig) awaitResult(t *testing.T) {
	t.Helper()
	waitFor(t, "the tool call to be answered", func() bool { return len(r.model.recordedToolResults()) >= 1 })
}

func (r *damageRig) node() string { return r.runtime.Engine().NodeID() }

func (r *damageRig) lastResult(t *testing.T) string {
	t.Helper()
	got := r.model.recordedToolResults()
	if len(got) == 0 {
		t.Fatal("no tool result was sent")
	}
	return got[len(got)-1].output
}

// Qwen reports its own cancel ahead of the speech it heard. A final that lands in
// between belongs to something said before the cut and must not judge it.
func TestAnEarlierFinalDoesNotJudgeACutWaitingForItsSpeech(t *testing.T) {
	r := newDamageRig(t)
	r.leg.holdFrames(40)
	r.started()
	r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
		Audio: make([]byte, media.FrameSamples*50)}
	r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
	waitFor(t, "the cut to be recorded", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil
	})
	r.leg.holdFrames(0)

	r.callerFinal("earlier", "mm-hmm")
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"}
	r.callerFinal("u2", "yes, I agree to renew")

	waitFor(t, "the verdict", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut.verdict != cutVerdictUnknown
	})
	r.session.bc.mu.Lock()
	defer r.session.bc.mu.Unlock()
	if got := r.session.bc.cut.verdict; got != cutVerdictContent {
		t.Errorf("verdict = %v, want the words of the speech that cut (content)", got)
	}
}

// A tool that would move the call, in the response that answers a cut for an
// acknowledgement, is refused as a conversation and moves nothing.
func TestAMovingToolIsRefusedWhenTheCutWasAnAcknowledgement(t *testing.T) {
	r := newDamageRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.started()
	r.callerFinal("u1", "mm-hmm")
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	r.ended()
	r.awaitResult(t)

	out := r.lastResult(t)
	if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, "acknowledged") {
		t.Errorf("tool result = %s, want a refusal that says the caller only acknowledged", out)
	}
	if r.node() != "offer" {
		t.Errorf("the call moved to %q on a bare acknowledgement", r.node())
	}
	if !strings.Contains(r.logs.String(), "the caller only acknowledged") {
		t.Errorf("the refusal left no trace in the log:\n%s", r.logs.String())
	}
}

// The same call after words, or with nothing playing when the caller spoke, runs.
func TestAMovingToolRunsWhenTheCallerSaidWordsOrNothingWasPlaying(t *testing.T) {
	t.Run("words", func(t *testing.T) {
		r := newDamageRig(t)
		r.botSpeaksUntilCut(t, "u1")
		r.started()
		r.callerFinal("u1", "yes, I agree to renew")
		r.toolCall("fc_1", "classify", `{"value":"agree"}`)
		r.ended()
		r.awaitResult(t)
		if r.node() != "agreed" {
			t.Errorf("the call is in %q, want agreed", r.node())
		}
	})
	t.Run("nothing was playing", func(t *testing.T) {
		r := newDamageRig(t)
		r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
		r.started()
		r.callerFinal("u1", "mm-hmm")
		r.toolCall("fc_1", "classify", `{"value":"agree"}`)
		r.ended()
		r.awaitResult(t)
		if r.node() != "agreed" {
			t.Errorf("a 嗯 said to a question that had finished playing was refused: the call is in %q", r.node())
		}
	})
}

// A tool that only reads or records is not touched by the verdict.
func TestAToolThatMovesNothingIsNotRefusedAfterAnAcknowledgement(t *testing.T) {
	r := newDamageRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.started()
	r.callerFinal("u1", "mm-hmm")
	r.toolCall("fc_1", "note", `{"value":"called back"}`)
	r.ended()
	r.awaitResult(t)
	if out := r.lastResult(t); !strings.Contains(out, `"ok":"1"`) {
		t.Errorf("tool result = %s, want the note recorded", out)
	}
}

// The window is the response that answers the cut: a later response's call runs.
func TestTheRefusalEndsWithTheResponseThatAnsweredTheCut(t *testing.T) {
	r := newDamageRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.started()
	r.callerFinal("u1", "okay")
	r.ended()
	r.started() // the next response
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	r.ended()
	r.awaitResult(t)
	if r.node() != "agreed" {
		t.Errorf("a later response's call was refused: the call is in %q", r.node())
	}
}

// A call that arrives before the verdict waits for it, and is judged by it.
func TestAMovingToolWaitsForALateVerdict(t *testing.T) {
	r := newDamageRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.started()
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	waitFor(t, "the tool call to wait for the verdict", func() bool { return len(r.wait.waits()) == 1 })
	if got := r.wait.waits()[0]; got != 1000*time.Millisecond {
		t.Errorf("waited %v for the verdict, want 1000ms", got)
	}
	if got := r.model.recordedToolResults(); len(got) != 0 {
		t.Fatalf("the call was answered before the verdict: %v", got)
	}
	r.callerFinal("u1", "yeah")
	r.ended()
	r.awaitResult(t)
	if r.node() != "offer" {
		t.Errorf("the call moved to %q though the verdict arrived as an acknowledgement", r.node())
	}
}

// No verdict within the wait: the call runs as it always did.
func TestAMovingToolRunsWhenTheVerdictNeverComes(t *testing.T) {
	r := newDamageRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.started()
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	waitFor(t, "the tool call to wait for the verdict", func() bool { return len(r.wait.waits()) == 1 })
	r.wait.expire()
	r.ended()
	r.awaitResult(t)
	if r.node() != "agreed" {
		t.Errorf("the call is in %q, want it to have run as it always did", r.node())
	}
}

// An utterance of another item is not the verdict on this cut.
func TestAnotherUtterancesTranscriptIsNotTheVerdict(t *testing.T) {
	r := newDamageRig(t)
	r.botSpeaksUntilCut(t, "u2")
	r.started()
	r.callerFinal("u1", "okay") // an earlier utterance's late final
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	waitFor(t, "the tool call to wait for its own verdict", func() bool { return len(r.wait.waits()) == 1 })
	r.callerFinal("u2", "yes please renew me")
	r.ended()
	r.awaitResult(t)
	if r.node() != "agreed" {
		t.Errorf("the call is in %q, want the words of u2 to be the verdict", r.node())
	}
}

// b. The closing line.

func (r *cutRig) callerFinal(text string) {
	r.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, Text: text, IsFinal: true}
}

func (r *cutRig) lineState() (owed, byRule bool, armed bool) {
	r.state(func(a *callActions) { owed, byRule, armed = a.isLineOwed, a.isLineSaidByRule, a.armed != nil })
	return owed, byRule, armed
}

// A cut over a closing line that had finished generating, for a bare
// acknowledgement, counts as said: the line is not said again and the call ends
// at once, without playing the model's reply.
func TestAnAcknowledgementCutOverTheFinishedLineEndsTheCall(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.session.cfg.OnCutAcknowledged = r.actions.onCutAcknowledged
	r.lineTurn(t, lineFrames, lineFrames, cutLine)

	r.interrupt(t, lineFrames/2, 1)
	owed, _, _ := r.lineState()
	if !owed {
		t.Fatal("the first cut did not leave the line owed")
	}

	r.callerFinal("mm-hmm")
	r.awaitCallEnded(t, 5*time.Second)
	if n := len(r.asked()); n != 0 {
		t.Errorf("the line was asked for %d times, want none", n)
	}
	if !strings.Contains(r.logs.String(), "running the armed action now") {
		t.Errorf("the ending left no trace in the log:\n%s", r.logs.String())
	}
}

// From the second interruption the line counts as said even if it was still
// being generated, so an acknowledgement ends the call rather than letting the
// model answer it (on qwen that answer was a made-up system message, live
// 2026-11-11). The grace cap is a minute: only the verdict can end it here.
func TestAnAcknowledgementAfterTheSecondCutEndsTheCall(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.session.cfg.OnCutAcknowledged = r.actions.onCutAcknowledged
	r.endArmingTurn(t)
	r.generating(t, lineFrames/2, generatingTail)
	r.cutFrom(t, true, 1)
	r.callerFinal("okay")
	settle()
	if r.isCallEnded() {
		t.Fatal("a first cut of a generating line ended the call")
	}

	r.generating(t, lineFrames/2, generatingTail)
	r.cutFrom(t, true, 2)
	r.callerFinal("okay")
	r.awaitCallEnded(t, 5*time.Second)
}

// Words after a second cut are still answered: only an acknowledgement ends it.
func TestWordsAfterTheSecondCutDoNotEndTheCall(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.session.cfg.OnCutAcknowledged = r.actions.onCutAcknowledged
	r.endArmingTurn(t)
	r.generating(t, lineFrames/2, generatingTail)
	r.cutFrom(t, true, 1)
	r.callerFinal("okay")
	settle()

	r.generating(t, lineFrames/2, generatingTail)
	r.cutFrom(t, true, 2)
	r.callerFinal("wait, what is the trial offer")
	settle()
	if r.isCallEnded() {
		t.Error("words over the closing line ended the call")
	}
}

// Words after the cut leave it the first interruption it was.
func TestWordsAfterTheCutLeaveTheFirstInterruptionAlone(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.session.cfg.OnCutAcknowledged = r.actions.onCutAcknowledged
	r.lineTurn(t, lineFrames, lineFrames, cutLine)
	r.interrupt(t, lineFrames/2, 1)

	r.callerFinal("wait, what is the trial offer")
	settle()
	owed, byRule, _ := r.lineState()
	if !owed || byRule || r.interruptions() != 1 {
		t.Errorf("owed=%v byRule=%v interruptions=%d, want the first interruption untouched",
			owed, byRule, r.interruptions())
	}
}

// #493: a cut of a line still being generated never runs the armed action, and
// an acknowledgement does not change that.
func TestAnAcknowledgementCutOfAGeneratingLineStillNeverRunsTheAction(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.session.cfg.OnCutAcknowledged = r.actions.onCutAcknowledged
	r.endArmingTurn(t)
	r.generating(t, lineFrames/2, generatingTail)
	r.cutFrom(t, true, 1)

	r.callerFinal("okay")
	settle()
	owed, byRule, armed := r.lineState()
	if r.isCallEnded() || !armed || !owed || byRule || r.interruptions() != 1 {
		t.Errorf("ended=%v armed=%v owed=%v byRule=%v interruptions=%d, want a first cut of a generating line untouched",
			r.isCallEnded(), armed, owed, byRule, r.interruptions())
	}
}

// The orchestrator records a backchannel as the caller's words and shows it to
// nothing else: the flow does not hear the caller speak.
func TestABackchannelDoesNotReachTheFlow(t *testing.T) {
	for _, tt := range []struct {
		name, said string
		wantCount  any
	}{
		{"acknowledgement", "mm-hmm", 1},
		{"words", "what does it cost", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var model *holdModel
			h := newClaimsHarnessModel(t, provider.OpenAIProfile(), quietFlow, "en", "", quietConfig(),
				func(m *fakeModel) provider.VoiceSession {
					model = &holdModel{fakeModel: m}
					return model
				}, func(c *claimsHarness) {
					c.runtime.OnNoInput() // one silence so far: noInput.count is 1
				})
			h.leg.holdFrames(40)
			model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
			model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*50)}
			model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
			model.events <- provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: "u1"}
			model.events <- provider.Event{Type: provider.EventTypeInputTranscript, ItemID: "u1", Text: tt.said, IsFinal: true}
			waitFor(t, "the utterance to be judged", func() bool {
				return len(model.answers())+len(model.declines()) == 1
			})
			h.session.Close(t.Context())
			select {
			case <-h.done:
			case <-time.After(3 * time.Second):
				t.Fatal("drive did not return")
			}
			count, _ := h.runtime.Engine().Slot("noInput.count")
			if count != tt.wantCount {
				t.Errorf("noInput.count = %v, want %v (an acknowledgement is not the caller speaking)", count, tt.wantCount)
			}
		})
	}
}
