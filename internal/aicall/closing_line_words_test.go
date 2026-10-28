// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Issue #262: a response that ends the call with another tool and hangup is
// answered by one turn, and that turn can say something other than the line.
// The words of the turn decide whether the caller heard it.

// batchPath starts a call and has the model end it with take_message and
// hangup in one response, which ends without any audio of its own: the turn
// that answers both results follows.
func batchPath(t *testing.T, lang string) (*toolPath, *syncBuffer) {
	t.Helper()
	h, sink := startOwedLinePath(t, provider.QwenProfile(), lang, 0)
	answers := h.callTools(t,
		[2]string{flow.ToolTakeMessage, `{"message":"a"}`},
		[2]string{flow.ToolHangup, `{}`})
	if len(answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(answers))
	}
	h.endResponse()
	return h, sink
}

// sayTurn has the model produce one whole turn that plays audio and whose
// transcript is words, in the order a Realtime provider reports them: the
// transcript before the end of the response.
func (h *toolPath) sayTurn(words ...string) {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*2),
	}
	for _, w := range words {
		h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: w, IsFinal: true}
	}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// awaitLinesSpoken waits until the call has asked for n lines word for word.
func (h *toolPath) awaitLinesSpoken(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.model.spokenLines(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the call asked for %d lines, want %d", len(h.model.spokenLines()), n)
	return nil
}

var notTheLine = map[string]string{
	"en": "Okay, a colleague will be in touch with you shortly.",
	"zh": "好的，会有同事稍后联系您。",
}

// The turn after the batch says something else: the caller hears it whole, the
// line is asked for as a closing line, and the call ends on the line's turn.
func TestABatchTurnThatMissesTheLineIsAnsweredByAskingForIt(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h, sink := batchPath(t, lang)
			line := owedLines[lang]

			h.sayTurn(notTheLine[lang])
			got := h.awaitLinesSpoken(t, 1)
			if got[0] != line {
				t.Errorf("asked for %q, want the rendered line %q", got[0], line)
			}
			if closing := h.model.spokenLinesClosing(); !closing[0] {
				t.Error("the line was not asked for as a closing line")
			}
			if len(h.model.recordedInterrupts()) != 0 {
				t.Errorf("interrupts = %v, want none: the first turn is heard whole",
					h.model.recordedInterrupts())
			}
			h.stayUp(t, "the call ended on the playback of a turn that was not the line")
			if !h.actions.isArmed() {
				t.Fatal("the ending was disarmed")
			}

			h.sayTurn(line)
			h.awaitCallEnded(t, 2*time.Second)
			if n := len(h.model.spokenLines()); n != 1 {
				t.Errorf("lines asked for = %d, want exactly one", n)
			}
			logs := sink.String()
			if !strings.Contains(logs, "did not say the closing line") {
				t.Errorf("log lacks the miss:\n%s", logs)
			}
			if strings.Contains(logs, "level=WARN msg=\"the closing line was not said") {
				t.Errorf("a line that was said was reported missing:\n%s", logs)
			}
			if strings.Contains(logs, "no closing line was generated") {
				t.Errorf("the call ended on the cap:\n%s", logs)
			}
		})
	}
}

// A turn that says the line, bare or behind a preface, is the line: nothing is
// asked for and the call ends on its playback.
func TestABatchTurnThatSaysTheLineIsTheLine(t *testing.T) {
	prefaces := map[string]string{"en": "Sure. ", "zh": "好的，"}
	for _, lang := range []string{"en", "zh"} {
		for name, preface := range map[string]string{"bare": "", "preface": prefaces[lang]} {
			t.Run(lang+"/"+name, func(t *testing.T) {
				h, _ := batchPath(t, lang)
				h.sayTurn(preface + strings.TrimSuffix(owedLines[lang], " 👋"))
				h.awaitCallEnded(t, 2*time.Second)
				if got := h.model.spokenLines(); len(got) != 0 {
					t.Errorf("lines asked for = %v, want none", got)
				}
			})
		}
	}
}

// The line asked for again is missed too: one request only, a warning, and the
// call ends on that turn's playback.
func TestALineThatIsMissedTwiceIsAskedForOnceAndTheCallEndsWithAWarning(t *testing.T) {
	h, sink := batchPath(t, "en")
	h.sayTurn(notTheLine["en"])
	h.awaitLinesSpoken(t, 1)
	h.stayUp(t, "the call ended on the first miss")

	h.sayTurn("Have a nice day.")
	h.awaitCallEnded(t, 2*time.Second)
	if n := len(h.model.spokenLines()); n != 1 {
		t.Errorf("lines asked for = %d, want one", n)
	}
	logs := sink.String()
	if !strings.Contains(logs, "level=WARN msg=\"the closing line was not said; ending the call without it\"") {
		t.Errorf("log lacks the warning:\n%s", logs)
	}
	if n := strings.Count(logs, "hanging up after the farewell"); n != 1 {
		t.Errorf("the hangup ran %d times, want once", n)
	}
}

// A turn with audio and no transcript cannot be judged and counts as the line.
func TestATurnWithoutAnyTranscriptCountsAsTheLine(t *testing.T) {
	h, _ := batchPath(t, "en")
	h.playTurn()
	h.awaitCallEnded(t, 2*time.Second)
	if got := h.model.spokenLines(); len(got) != 0 {
		t.Errorf("lines asked for = %v, want none", got)
	}
}

// Several final transcripts of one turn are one set of words.
func TestTheTranscriptsOfOneTurnAreJoined(t *testing.T) {
	h, _ := batchPath(t, "en")
	h.sayTurn("Thank you Ann,", "goodbye")
	h.awaitCallEnded(t, 2*time.Second)
	if got := h.model.spokenLines(); len(got) != 0 {
		t.Errorf("lines asked for = %v, want none", got)
	}
}

// What the arming turn said before its tools is not the line, and neither are
// its words held against the line of a later turn.
func TestTheArmingTurnsOwnWordsAreNotKept(t *testing.T) {
	actions, session, model := testActions(t, &fakeSwitch{})
	actions.arm(t.Context(), func() {})
	armedTurn := session.currentTurn()
	actions.expectClosingLine("Goodbye.")
	actions.onBotSaid(armedTurn, "Goodbye.")
	actions.mu.Lock()
	n := len(actions.said)
	actions.mu.Unlock()
	if n != 0 {
		t.Fatalf("kept the words of the arming turn")
	}
	if len(model.spokenLines()) != 0 {
		t.Fatal("asked for a line")
	}
}

// A later turn already being made is not talked over: the request waits for
// that turn to be judged, and is made after it if it misses too.
func TestTheLineIsNotAskedOverATurnAlreadyBeingMade(t *testing.T) {
	actions, session, model := testActions(t, &fakeSwitch{})
	actions.arm(t.Context(), func() {})
	armedTurn := session.currentTurn()
	actions.expectClosingLine("Goodbye, Ann.")

	session.beginTurn(false, false) // armedTurn+1
	actions.onBotSaid(armedTurn+1, "Okay, noted.")
	session.beginTurn(false, false) // armedTurn+2 starts before +1 is judged
	actions.onTurnDone(armedTurn+1, false, true)
	if got := model.spokenLines(); len(got) != 0 {
		t.Fatalf("asked for %v over a turn being made", got)
	}
	if actions.isLineSpoken {
		t.Fatal("the missed turn counted as the line")
	}

	// The turn that was on its way says the line.
	actions.onBotSaid(armedTurn+2, "Goodbye, Ann.")
	actions.onPlaybackDone(armedTurn+1, true) // the first turn's tail: not the ending
	if !actions.isArmed() {
		t.Fatal("the missed turn's playback ran the ending")
	}
	session.endTurn()
	actions.onTurnDone(armedTurn+2, false, true)
	if got := model.spokenLines(); len(got) != 0 {
		t.Fatalf("asked for %v although the turn said the line", got)
	}
	actions.onPlaybackDone(armedTurn+2, true)
	if actions.isArmed() {
		t.Fatal("the line's playback did not run the ending")
	}
}

// A transfer owes no line: nothing is compared and nothing is asked for.
func TestWordsAfterATransferAreNotHeldAgainstALine(t *testing.T) {
	actions, session, model := testActions(t, &fakeSwitch{})
	actions.armAs(t.Context(), true, func() {})
	actions.expectClosingLine("Goodbye.")
	armedTurn := session.currentTurn()
	actions.onBotSaid(armedTurn+1, "Putting you through.")
	actions.onTurnDone(armedTurn+1, false, true)
	if got := model.spokenLines(); len(got) != 0 {
		t.Fatalf("asked for %v on a transfer", got)
	}
	if !actions.isLineSpokenForTest() {
		t.Fatal("the transfer's turn did not count as spoken")
	}
}

func (a *callActions) isLineSpokenForTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isLineSpoken
}
