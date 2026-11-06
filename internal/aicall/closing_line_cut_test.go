// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Issue #465: a caller who talks over a closing line that has finished
// generating. A cut in the line's final sentence counts as the line said (A),
// and from the second interruption on the line is never asked for again and
// the cap stops moving (D).

// cutLine is a line of three sentences whose last, "Goodbye and take care of
// yourself, Ann.", is 31 of 108 letters: about 29 percent of its audio, so a
// cut 73 percent in lands inside it.
const cutLine = "Thank you for calling. Your message has been taken and a colleague will call you back tomorrow. " +
	"Goodbye and take care of yourself, Ann."

// lineFrames is the line's length as the tests play it: 15 s.
const lineFrames = 750

// shortLineFrames is a 3 s telling for the tests that count interruptions and
// not their place in the line: cheaper to push through -race on a busy CPU, and
// the cap they time is not left to a slow turn.
const shortLineFrames = 150

// qwenFrameBytes is the model audio that makes one 20 ms frame on the leg:
// qwen speaks PCM16 at 24 kHz, three times the leg's rate.
const qwenFrameBytes = media.FrameSamples * 2 * 3

type cutRig struct {
	*toolPath
	logs *syncBuffer
}

// startCutRig ends a call with hangup into a terminal phase whose line is
// cutLine, with the given grace cap, and leaves the model about to say it.
func startCutRig(t *testing.T, graceCap time.Duration) *cutRig {
	t.Helper()
	return startCutRigOn(t, provider.QwenProfile(), graceCap)
}

// startCutRigOn is startCutRig on a deployment running profile.
func startCutRigOn(t *testing.T, profile provider.Profile, graceCap time.Duration) *cutRig {
	t.Helper()
	flowJSON := strings.Replace(owedLineFlow, "Thank you {slots.name}, goodbye 👋", cutLine, 1)
	logs := &syncBuffer{}
	h := startToolPathFull(t, profile, "en", flowJSON, "",
		map[string]any{"name": "Ann"}, logs)
	h.actions.mu.Lock()
	h.actions.graceCap = graceCap
	h.actions.mu.Unlock()
	h.callTool(t, flow.ToolHangup, `{}`)
	return &cutRig{toolPath: h, logs: logs}
}

// await polls until cond holds.
func (r *cutRig) await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second) // 15 s of audio per turn is slow under -race on a busy CPU
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (r *cutRig) state(read func(a *callActions)) {
	r.actions.mu.Lock()
	defer r.actions.mu.Unlock()
	read(r.actions)
}

func (r *cutRig) interruptions() (n int) {
	r.state(func(a *callActions) { n = a.interruptions })
	return n
}

func (r *cutRig) capDue() (due time.Time) {
	r.state(func(a *callActions) { due = a.capDue })
	return due
}

// say has the model produce one turn of frames of audio with words as its
// transcript, of which pending frames are still queued when it finishes
// generating.
func (r *cutRig) say(frames, pending int, words ...string) {
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*frames),
	}
	for _, w := range words {
		r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: w, IsFinal: true}
	}
	r.leg.holdFrames(pending)
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// lineTurn is say for a turn that counts as the line: it waits until the call
// has judged it so.
func (r *cutRig) lineTurn(t *testing.T, frames, pending int, words ...string) {
	t.Helper()
	r.say(frames, pending, words...)
	r.await(t, "the turn to finish generating", r.actions.isLineSpokenForTest)
}

// interrupt has the caller take the floor with pending frames still to play,
// and waits until the call has counted it (n interruptions so far). The flush
// empties the queue.
func (r *cutRig) interrupt(t *testing.T, pending, n int) {
	t.Helper()
	r.leg.holdFrames(pending)
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	r.await(t, "the interruption to be counted", func() bool { return r.interruptions() >= n })
	r.leg.holdFrames(0)
}

func (r *cutRig) asked() []string { return r.model.spokenLines() }

// A cut at 5, 50, 73 and 95 percent of a 15 s line, answered by a decline, a
// question or noise (speech with no transcript). The line is never said in
// full twice: a cut in the final sentence is never followed by a request for
// it, an early cut is followed by one request if the answer is not the line.
func TestACutOverTheClosingLineIsJudgedByWhereItFell(t *testing.T) {
	answers := map[string][]string{
		"decline":  {"Okay, goodbye."},
		"question": {"The trial lasts fourteen days."},
		"noise":    nil,
	}
	for _, percent := range []int{5, 50, 73} {
		for _, kind := range []string{"decline", "question", "noise"} {
			name := map[int]string{5: "5", 50: "50", 73: "73"}[percent] + "/" + kind
			t.Run(name, func(t *testing.T) {
				r := startCutRig(t, time.Minute)
				r.lineTurn(t, lineFrames, lineFrames, cutLine)

				r.interrupt(t, lineFrames-lineFrames*percent/100, 1)
				if r.isCallEnded() || !r.actions.isArmed() {
					t.Fatal("the ending ran on a cut with a long tail unplayed")
				}
				isFinalSentence := percent == 73
				wantAsk := !isFinalSentence && kind != "noise"

				words := answers[kind]
				r.say(1, 0, words...) // the answer
				if wantAsk {
					got := r.awaitLinesSpoken(t, 1)
					if got[0] != cutLine {
						t.Errorf("asked for %q, want the rendered line %q", got[0], cutLine)
					}
					r.stayUp(t, "the call ended on an answer that was not the line")
					r.say(1, 0, cutLine)
				}
				r.awaitCallEnded(t, 15*time.Second)

				if n := len(r.asked()); n != btoi(wantAsk) {
					t.Errorf("the line was asked for %d times, want %d", n, btoi(wantAsk))
				}
				logs := r.logs.String()
				if got := strings.Contains(logs, "counting it as said"); got != isFinalSentence {
					t.Errorf("final-sentence line present = %v, want %v:\n%s", got, isFinalSentence, logs)
				}
				if got := strings.Contains(logs, "the line is asked for once more"); got != wantAsk {
					t.Errorf("ask line present = %v, want %v:\n%s", got, wantAsk, logs)
				}
				if strings.Contains(logs, "a second interruption") {
					t.Errorf("one interruption was logged as a second:\n%s", logs)
				}
				if strings.Contains(logs, "no closing line was generated") {
					t.Errorf("the call ended on the cap:\n%s", logs)
				}
			})
		}
	}
}

// The final-sentence log line carries the numbers the call can be read back by.
func TestACutInTheFinalSentenceLogsWhatItEstimated(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.lineTurn(t, lineFrames, lineFrames, cutLine)
	r.interrupt(t, lineFrames-lineFrames*73/100, 1)
	logs := r.logs.String()
	for _, want := range []string{"unplayedMs=", "playedMs=", "finalSentenceMs="} {
		if !strings.Contains(logs, "counting it as said") || !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	// The ending still waits for the model's answer to the caller.
	if r.isCallEnded() || !r.actions.isArmed() {
		t.Fatal("the cut ran the ending instead of waiting for the answer")
	}
}

// 95 percent in is the caller answering the goodbye: the call ends at once.
func TestACutInTheLastSecondOfTheLineEndsTheCallAtOnce(t *testing.T) {
	for _, kind := range []string{"decline", "question", "noise"} {
		t.Run(kind, func(t *testing.T) {
			r := startCutRig(t, time.Minute)
			r.lineTurn(t, lineFrames, lineFrames, cutLine)
			r.leg.holdFrames(lineFrames - lineFrames*95/100)
			r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
			r.awaitCallEnded(t, 15*time.Second)
			if n := len(r.asked()); n != 0 {
				t.Errorf("the line was asked for %d times", n)
			}
		})
	}
}

// Two early interruptions over a model that says the full line again each
// time: the platform never asks for it, the count is 2 from the second cut on,
// and the answer after it ends the call.
func TestASecondInterruptionStopsTheLineBeingAskedFor(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.lineTurn(t, shortLineFrames, 100, cutLine)
	r.interrupt(t, 100, 1) // "mm-hm" over the line
	r.lineTurn(t, shortLineFrames, 100, cutLine)
	r.interrupt(t, 100, 2) // and again
	due := r.capDue()

	r.state(func(a *callActions) {
		if a.isLineOwed {
			t.Error("the line is still owed after the second interruption")
		}
	})
	if r.isCallEnded() {
		t.Fatal("the call ended on the interruption itself")
	}
	r.say(1, 0, "Okay.") // the answer, played out
	r.awaitCallEnded(t, 15*time.Second)

	if n := len(r.asked()); n != 0 {
		t.Errorf("the line was asked for %d times, want none", n)
	}
	if got := r.capDue(); !got.Equal(due) {
		t.Errorf("the cap moved by %v after the second interruption", got.Sub(due))
	}
	logs := r.logs.String()
	if strings.Count(logs, "a second interruption over the closing line; not asking for it again, cap not restarted") != 1 {
		t.Errorf("the second interruption was not logged once:\n%s", logs)
	}
	if !strings.Contains(logs, "capInMs=") || !strings.Contains(logs, "interruptions=2") {
		t.Errorf("the log lacks the cap or the count:\n%s", logs)
	}
}

// Four interruptions, each answered with the full line, none of it ever
// playing out: nothing moves the cap that was running at the second one, and
// the call ends at it.
func TestFourInterruptionsEndTheCallAtTheCapRunningAtTheSecond(t *testing.T) {
	r := startCutRig(t, 5*time.Second)
	r.lineTurn(t, shortLineFrames, 100, cutLine)
	r.interrupt(t, 100, 1)
	r.lineTurn(t, shortLineFrames, 100, cutLine)
	r.interrupt(t, 100, 2)
	due := r.capDue()
	if r.isCallEnded() {
		t.Fatal("the call ended before the cap")
	}

	// The model says the line again, the queued audio stays; the caller cuts in
	// again, and again.
	for n := 3; n <= 4; n++ {
		r.lineTurn(t, shortLineFrames, 100, cutLine)
		if got := r.capDue(); !got.Equal(due) {
			t.Fatalf("a line turn after interruption %d moved the cap by %v", n-1, got.Sub(due))
		}
		r.interrupt(t, 100, n)
		if got := r.capDue(); !got.Equal(due) {
			t.Fatalf("interruption %d moved the cap by %v", n, got.Sub(due))
		}
	}
	// The slack is for a timer on a busy CPU; a moved cap would be seconds late,
	// and the capDue checks above catch it exactly.
	r.awaitCallEnded(t, time.Until(due)+15*time.Second)
	if over := time.Since(due); over > 10*time.Second {
		t.Errorf("the call ended %v after the cap that was running", over)
	}
	if n := len(r.asked()); n != 0 {
		t.Errorf("the line was asked for %d times, want none", n)
	}
	logs := r.logs.String()
	if strings.Count(logs, "a second interruption over the closing line") != 1 ||
		strings.Count(logs, "a further interruption over the closing line; cap still not restarted") != 2 {
		t.Errorf("the interruptions were not logged as one second and two further:\n%s", logs)
	}
	if strings.Contains(logs, "the line is asked for once more") {
		t.Errorf("the line was asked for after the second interruption:\n%s", logs)
	}
	// Reaching the frozen cap is the rule working, not a stalled line.
	if !strings.Contains(logs, "the cap frozen at the second interruption ran out; running the armed action") ||
		strings.Contains(logs, "running an armed action at the cap") {
		t.Errorf("the frozen cap was not logged as the rule:\n%s", logs)
	}
}

// An early question is the first interruption and is answered; the answer is
// not the line, so the line is asked for once; the caller cuts in on that, and
// there is no third telling: the next answer ends the call.
func TestAQuestionThenASecondInterruptionEndsWithoutAThirdTelling(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.lineTurn(t, lineFrames, lineFrames, cutLine)
	r.interrupt(t, lineFrames*95/100, 1) // 5 percent in

	r.say(1, 0, "The trial lasts fourteen days.")
	got := r.awaitLinesSpoken(t, 1)
	if got[0] != cutLine {
		t.Fatalf("asked for %q, want the line", got[0])
	}
	// The line asked for is said, and the caller cuts in on it too.
	r.lineTurn(t, lineFrames, lineFrames*9/10, cutLine)
	r.interrupt(t, lineFrames*9/10, 2)

	r.say(1, 0, "Of course. Goodbye.")
	r.awaitCallEnded(t, 15*time.Second)
	if n := len(r.asked()); n != 1 {
		t.Errorf("the line was asked for %d times, want exactly one", n)
	}
	logs := r.logs.String()
	if strings.Count(logs, "the line is asked for once more") != 1 {
		t.Errorf("want exactly one ask in the log:\n%s", logs)
	}
	if !strings.Contains(logs, "a second interruption over the closing line") {
		t.Errorf("the second interruption was not logged:\n%s", logs)
	}
}

// The cap of a fresh arming is not frozen by interruptions of an earlier one.
func TestAFreshArmingStartsItsCapAgain(t *testing.T) {
	actions, _, _ := testActions(t, &fakeSwitch{})
	actions.arm(t.Context(), func() {})
	actions.mu.Lock()
	actions.interruptions = 3
	actions.armed = nil
	actions.mu.Unlock()
	before := actions.capDue
	actions.arm(t.Context(), func() {})
	actions.mu.Lock()
	defer actions.mu.Unlock()
	if actions.interruptions != 0 || !actions.capDue.After(before) {
		t.Fatalf("interruptions = %d, cap moved = %v", actions.interruptions, actions.capDue.After(before))
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// goodbyeText is what the answer turn says in the goodbye tests: the full line
// again, as the model did on the live call.
const goodbyeText = cutLine

// sayAndHangUp has the model produce an answer turn that speaks (frames of
// audio, pending of them still queued) and calls hangup in the same turn, and
// waits until the call has answered that hangup. The turn is left open.
func (r *cutRig) sayAndHangUp(t *testing.T, frames, pending int, words ...string) {
	t.Helper()
	answered := len(r.model.recordedToolResults())
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*frames),
	}
	for _, w := range words {
		r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: w, IsFinal: true}
	}
	r.leg.holdFrames(pending)
	r.model.events <- provider.Event{
		Type: provider.EventTypeToolCall, ToolCallID: "fc_goodbye", ToolName: flow.ToolHangup, ToolArgs: `{}`,
	}
	r.await(t, "the hangup to be answered", func() bool {
		return len(r.model.recordedToolResults()) > answered
	})
}

// endTurn ends the open turn and waits until the call has judged it.
func (r *cutRig) endTurn(t *testing.T) {
	t.Helper()
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	r.await(t, "the turn to finish generating", r.actions.isLineSpokenForTest)
}

// followUp is the turn the provider makes after a tool result: it says words
// with frames of audio. It waits until the turn has been ended by the session.
func (r *cutRig) followUp(t *testing.T, frames int, words ...string) (turn int) {
	t.Helper()
	turn = r.session.currentTurn() + 1
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*frames),
	}
	for _, w := range words {
		r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: w, IsFinal: true}
	}
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	r.await(t, "the follow-up turn to end", func() bool {
		return r.session.currentTurn() == turn && !r.session.isTurnGenerating()
	})
	return turn
}

func (r *cutRig) goodbyeTurn() (turn int) {
	r.state(func(a *callActions) { turn = a.goodbyeTurn })
	return turn
}

const (
	goodbyeLog = "the model said goodbye and called hangup over a closing line counted as said"
	capLogA    = "running an armed action at the cap"
	capLogB    = "ran out; running the armed action"
)

func (r *cutRig) assertNotEndedOnTheCap(t *testing.T) {
	t.Helper()
	logs := r.logs.String()
	if strings.Contains(logs, capLogA) || strings.Contains(logs, capLogB) {
		t.Errorf("the call ended on the cap:\n%s", logs)
	}
}

// A cut in the final sentence counts the line as said; the answer says the line
// again and calls hangup. The turn the hangup's result brings says it a third
// time: it is not played and leaves no words, and the call ends when the
// answer has been heard (#465).
func TestTheTurnAfterAGoodbyeHangupIsMutedAndTheCallEndsOnTheGoodbye(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.lineTurn(t, lineFrames, lineFrames, cutLine)
	r.interrupt(t, lineFrames-lineFrames*73/100, 1) // A applies

	r.sayAndHangUp(t, shortLineFrames, 100, goodbyeText)
	answerTurn := r.session.currentTurn()
	if got := r.goodbyeTurn(); got != answerTurn {
		t.Fatalf("goodbye turn = %d, want the answer turn %d", got, answerTurn)
	}
	r.endTurn(t)
	played := len(r.leg.sentFrames())

	turn := r.followUp(t, shortLineFrames, goodbyeText)
	if !r.session.isMutedTurn(turn) {
		t.Errorf("turn %d, the one the hangup's result brought, is not muted", turn)
	}
	if got := len(r.leg.sentFrames()); got != played {
		t.Errorf("%d frames of the muted turn reached the caller", got-played)
	}
	r.await(t, "the muted words to be left out", func() bool {
		return strings.Contains(r.logs.String(), "left a muted turn's words out of the transcript")
	})
	if r.isCallEnded() {
		t.Fatal("the call ended before the goodbye had played")
	}
	r.leg.holdFrames(0) // the goodbye has drained
	r.awaitCallEnded(t, 15*time.Second)

	logs := r.logs.String()
	if strings.Count(logs, goodbyeLog) != 1 {
		t.Errorf("the rule was not logged once:\n%s", logs)
	}
	if n := len(r.asked()); n != 0 {
		t.Errorf("the line was asked for %d times, want none", n)
	}
	r.assertNotEndedOnTheCap(t)
}

// The caller talks over the goodbye after the hangup: the mute is withdrawn,
// the answer to them is played, and the call ends on it.
func TestACallerWhoTalksOverTheGoodbyeGetsTheirAnswerPlayed(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.lineTurn(t, lineFrames, lineFrames, cutLine)
	r.interrupt(t, lineFrames-lineFrames*73/100, 1)

	r.sayAndHangUp(t, shortLineFrames, 100, goodbyeText)
	r.endTurn(t)
	if r.goodbyeTurn() == 0 {
		t.Fatal("the goodbye was not recorded")
	}
	r.interrupt(t, 100, 2)
	if got := r.goodbyeTurn(); got != 0 {
		t.Fatalf("goodbye turn = %d after the caller talked over it, want none", got)
	}
	played := len(r.leg.sentFrames())

	turn := r.session.currentTurn() + 1
	r.say(3, 0, "Of course, thank you.")
	r.awaitCallEnded(t, 15*time.Second)
	if r.session.isMutedTurn(turn) {
		t.Errorf("turn %d, the answer to the caller, was muted", turn)
	}
	if got := len(r.leg.sentFrames()); got <= played {
		t.Error("the answer to the caller did not play")
	}
	logs := r.logs.String()
	if strings.Contains(logs, "left a muted turn's words out") {
		t.Errorf("a turn was muted after the mute was withdrawn:\n%s", logs)
	}
	if !strings.Contains(logs, "the caller spoke over the goodbye") {
		t.Errorf("the withdrawal was not logged:\n%s", logs)
	}
}

// A first interruption that is not in the final sentence leaves the line owed:
// a hangup from the speaking answer changes nothing, and the turn after it is
// played as before.
func TestAHangupAfterAnEarlyFirstInterruptionMutesNothing(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.lineTurn(t, lineFrames, lineFrames, cutLine)
	r.interrupt(t, lineFrames*95/100, 1) // 5 percent in, not A

	r.sayAndHangUp(t, shortLineFrames, 100, goodbyeText)
	if got := r.goodbyeTurn(); got != 0 {
		t.Fatalf("goodbye turn = %d, want none", got)
	}
	r.endTurn(t)
	played := len(r.leg.sentFrames())

	turn := r.followUp(t, shortLineFrames, goodbyeText)
	if r.session.isMutedTurn(turn) {
		t.Errorf("turn %d was muted", turn)
	}
	if got := len(r.leg.sentFrames()); got <= played {
		t.Error("the turn after the hangup did not play")
	}
	r.leg.holdFrames(0)
	r.awaitCallEnded(t, 15*time.Second)
	logs := r.logs.String()
	if strings.Contains(logs, goodbyeLog) ||
		strings.Contains(logs, "left a muted turn's words out") {
		t.Errorf("the goodbye rule engaged on a first early interruption:\n%s", logs)
	}
}

// Two early interruptions (D), then an answer that speaks and calls hangup: the
// follow-up is muted and the call ends on the answer's playback, before the cap
// that was running at the second interruption.
func TestTheGoodbyeRuleAlsoHoldsAfterASecondInterruption(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.lineTurn(t, shortLineFrames, 100, cutLine)
	r.interrupt(t, 100, 1)
	r.lineTurn(t, shortLineFrames, 100, cutLine)
	r.interrupt(t, 100, 2)
	due := r.capDue()

	r.sayAndHangUp(t, shortLineFrames, 100, goodbyeText)
	if r.goodbyeTurn() == 0 {
		t.Fatal("the goodbye was not recorded after the second interruption")
	}
	r.endTurn(t)
	played := len(r.leg.sentFrames())
	turn := r.followUp(t, shortLineFrames, goodbyeText)
	if !r.session.isMutedTurn(turn) {
		t.Errorf("turn %d is not muted", turn)
	}
	if got := len(r.leg.sentFrames()); got != played {
		t.Errorf("%d frames of the muted turn reached the caller", got-played)
	}
	r.leg.holdFrames(0)
	r.awaitCallEnded(t, 15*time.Second)
	if got := r.capDue(); !got.Equal(due) {
		t.Errorf("the cap moved by %v", got.Sub(due))
	}
	if !strings.Contains(r.logs.String(), goodbyeLog) {
		t.Errorf("the rule was not logged:\n%s", r.logs.String())
	}
	r.assertNotEndedOnTheCap(t)
}

// Issue #493: a cut over a closing line that is still being generated. On qwen
// the provider's own turn detection cancels the open response before it
// reports the caller's speech, so the session hears of the cut from the
// cancellation (INTERRUPTED, then SPEECH_STARTED); a provider without one, or a
// barge-in the session sees first, reports it the other way round. Either way
// it is one interruption: counted for D, never the final-sentence rule (A),
// and never the "bye now" that runs the ending.

// noLineFlow is the cut rig's flow with no line in its closing phase.
var noLineFlow = regexp.MustCompile(`(?s)"announce": \{\s*"en": "Thank you.*?"\s*\},\s*`).
	ReplaceAllString(owedLineFlow, "")

// generatingCutLog is the INFO line of a cut over a line still being generated.
const generatingCutLog = "the caller cut in on the closing line while it was still being generated"

// generatingTail is what is still queued for the caller when a line still
// being generated is cut: a model streaming at about real time keeps the queue
// short, here 500 ms, which is less than closingTailAck. Over a line that had
// finished generating that little left would be the caller answering the
// goodbye; over one still being made it is not.
const generatingTail = 25

// endArmingTurn ends the turn the hangup came in, as the provider does before
// it starts the next, and waits until the session has closed it.
func (r *cutRig) endArmingTurn(t *testing.T) {
	t.Helper()
	r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
	r.await(t, "the arming turn to end", func() bool { return !r.isResponding() })
}

func (r *cutRig) isResponding() bool {
	r.session.mu.Lock()
	defer r.session.mu.Unlock()
	return r.session.isResponding
}

// generating has the model begin a turn and stream frames of its audio, of
// which pending frames are still queued for the caller, and leaves the turn
// open: still being generated. It returns the turn once its audio is queued.
func (r *cutRig) generating(t *testing.T, frames, pending int) (turn int) {
	t.Helper()
	turn = r.session.currentTurn() + 1
	r.leg.holdFrames(pending)
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	r.model.events <- provider.Event{
		Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*frames),
	}
	r.await(t, "the turn's audio to be queued", func() bool {
		_, isSpeaking := r.session.audioProgress()
		return r.session.currentTurn() == turn && isSpeaking
	})
	return turn
}

// cutFrom has the caller talk over the open turn, reported in the order one
// side or the other sees it: isProviderFirst is the provider cancelling the
// turn on its own before it reports the speech (qwen's turn detection, or
// doubao's bare response.done ahead of its transcription), otherwise the
// speech comes first and the cancellation confirms it. It waits until the
// call has counted n interruptions.
func (r *cutRig) cutFrom(t *testing.T, isProviderFirst bool, n int) {
	t.Helper()
	cancelled := provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
	speech := provider.Event{Type: provider.EventTypeSpeechStarted}
	if isProviderFirst {
		r.model.events <- cancelled
		r.model.events <- speech
	} else {
		r.model.events <- speech
		r.model.events <- cancelled
	}
	r.await(t, "the cut to be counted", func() bool { return r.interruptions() >= n })
	r.leg.holdFrames(0)
}

// cutsLoggedFor reports how many generating-cut lines name the turn.
func (r *cutRig) cutsLoggedFor(turn int) int {
	n := 0
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, generatingCutLog) && strings.Contains(line, " turn="+strconv.Itoa(turn)+" ") {
			n++
		}
	}
	return n
}

// awaitCutLogged waits until the cut of turn has been logged. Model events
// reach the call in order, so everything sent before that cut has been
// handled once it is.
func (r *cutRig) awaitCutLogged(t *testing.T, turn int) {
	t.Helper()
	r.await(t, "the cut of turn "+strconv.Itoa(turn)+" to be logged", func() bool {
		return r.cutsLoggedFor(turn) > 0
	})
}

// A 15 s line still being generated is cut by the provider at 5, 50, 73 and 95
// percent of it, with the caller declining, asking or saying nothing a
// transcript catches. The cut is counted and restarts the cap; the ending
// stays armed; the line stays owed, even at 73 and 95 percent where a cut of
// the finished line would have counted it as said or ended the call. So the
// model's answer, unless it is the line or carries no words, is followed by
// one request for the line, and the call ends on the line's playback.
func TestACutWhileTheClosingLineIsGeneratingIsCountedButNeverDated(t *testing.T) {
	callerSaid := map[string]string{
		"decline": "No thanks, that's fine.", "question": "Wait, when will they call?", "empty": "",
	}
	answers := map[string][]string{
		"decline": {"Okay, goodbye."}, "question": {"Tomorrow morning."}, "empty": nil,
	}
	for _, percent := range []int{5, 50, 73, 95} {
		for _, kind := range []string{"decline", "question", "empty"} {
			t.Run(strconv.Itoa(percent)+"/"+kind, func(t *testing.T) {
				r := startCutRig(t, time.Minute)
				r.endArmingTurn(t)
				before := r.capDue()
				turn := r.generating(t, lineFrames*percent/100, generatingTail)

				r.cutFrom(t, true, 1)
				if r.isCallEnded() || !r.actions.isArmed() {
					t.Fatal("a cut over a line still being generated ran the ending")
				}
				r.state(func(a *callActions) {
					if !a.isLineOwed || a.isLineSaidByRule {
						t.Errorf("isLineOwed = %v, isLineSaidByRule = %v after a first cut while generating; "+
							"want the line still owed", a.isLineOwed, a.isLineSaidByRule)
					}
					if !a.isInterrupted {
						t.Error("the cut was not handled as an interruption")
					}
				})
				if !r.capDue().After(before) {
					t.Error("the first cut did not restart the cap")
				}

				if said := callerSaid[kind]; said != "" {
					r.model.events <- provider.Event{Type: provider.EventTypeInputTranscript, Text: said, IsFinal: true}
				}
				wantAsk := kind != "empty"
				r.say(1, 0, answers[kind]...)
				if wantAsk {
					got := r.awaitLinesSpoken(t, 1)
					if got[0] != cutLine {
						t.Errorf("asked for %q, want the rendered line %q", got[0], cutLine)
					}
					r.stayUp(t, "the call ended on an answer that was not the line")
					r.say(1, 0, cutLine)
				}
				r.awaitCallEnded(t, 15*time.Second)

				if n := len(r.asked()); n != btoi(wantAsk) {
					t.Errorf("the line was asked for %d times, want %d", n, btoi(wantAsk))
				}
				logs := r.logs.String()
				if r.cutsLoggedFor(turn) != 1 || !strings.Contains(logs, "isByProvider=true") ||
					!strings.Contains(logs, "interruptions=1") {
					t.Errorf("the cut was not logged once as the provider's first:\n%s", logs)
				}
				for _, never := range []string{
					"counting it as said", "running the armed action now", "a second interruption", capLogA, capLogB,
				} {
					if strings.Contains(logs, never) {
						t.Errorf("the log has %q:\n%s", never, logs)
					}
				}
			})
		}
	}
}

// Two cuts the provider reports over lines still being generated: the second
// counts the line as said and freezes the cap, and neither a third cut nor the
// answer moves it. The answer's playback ends the call.
func TestTwoProviderCutsWhileGeneratingFreezeTheCap(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.endArmingTurn(t)
	r.generating(t, shortLineFrames, generatingTail)
	r.cutFrom(t, true, 1)

	r.generating(t, shortLineFrames, generatingTail) // the model says the line again
	due := r.capDue()
	r.cutFrom(t, true, 2)
	if got := r.capDue(); !got.Equal(due) {
		t.Fatalf("the second cut moved the cap by %v", got.Sub(due))
	}
	r.state(func(a *callActions) {
		if a.isLineOwed || !a.isLineSaidByRule {
			t.Errorf("isLineOwed = %v, isLineSaidByRule = %v after the second cut", a.isLineOwed, a.isLineSaidByRule)
		}
	})

	r.generating(t, shortLineFrames, generatingTail)
	r.cutFrom(t, true, 3)
	if got := r.capDue(); !got.Equal(due) {
		t.Fatalf("a third cut moved the cap by %v", got.Sub(due))
	}
	if r.isCallEnded() {
		t.Fatal("the call ended on a cut")
	}
	r.say(1, 0, "Okay.") // the answer, played out
	r.awaitCallEnded(t, 15*time.Second)

	if got := r.capDue(); !got.Equal(due) {
		t.Errorf("the answer moved the cap by %v", got.Sub(due))
	}
	if n := len(r.asked()); n != 0 {
		t.Errorf("the line was asked for %d times, want none", n)
	}
	logs := r.logs.String()
	if strings.Count(logs, generatingCutLog) != 3 {
		t.Errorf("want three generating cuts logged:\n%s", logs)
	}
	if strings.Count(logs, "a second interruption over the closing line; not asking for it again, cap not restarted") != 1 ||
		strings.Count(logs, "a further interruption over the closing line; cap still not restarted") != 1 ||
		!strings.Contains(logs, "isGenerating=true isByProvider=true") {
		t.Errorf("D was not logged for the second and third cuts:\n%s", logs)
	}
	r.assertNotEndedOnTheCap(t)
}

// One cut the provider reports and one the session's barge-in reports, in
// either order: the cap freezes at the second, whichever side reported it.
func TestAProviderCutAndACallerCutFreezeTheCapInEitherOrder(t *testing.T) {
	for _, isProviderFirst := range []bool{true, false} {
		name := map[bool]string{true: "provider then caller", false: "caller then provider"}[isProviderFirst]
		t.Run(name, func(t *testing.T) {
			r := startCutRig(t, time.Minute)
			r.endArmingTurn(t)
			first := r.generating(t, shortLineFrames, generatingTail)
			r.cutFrom(t, isProviderFirst, 1)

			second := r.generating(t, shortLineFrames, generatingTail)
			due := r.capDue()
			r.cutFrom(t, !isProviderFirst, 2)
			if got := r.capDue(); !got.Equal(due) {
				t.Fatalf("the second cut moved the cap by %v", got.Sub(due))
			}
			r.state(func(a *callActions) {
				if a.isLineOwed {
					t.Error("the line is still owed after the second cut")
				}
			})
			r.say(1, 0, "Okay.")
			r.awaitCallEnded(t, 15*time.Second)

			if n := len(r.asked()); n != 0 {
				t.Errorf("the line was asked for %d times, want none", n)
			}
			for _, cut := range []struct {
				turn         int
				isByProvider bool
			}{{first, isProviderFirst}, {second, !isProviderFirst}} {
				want := "isByProvider=" + strconv.FormatBool(cut.isByProvider)
				for _, line := range strings.Split(r.logs.String(), "\n") {
					if strings.Contains(line, generatingCutLog) && strings.Contains(line, " turn="+strconv.Itoa(cut.turn)+" ") &&
						!strings.Contains(line, want) {
						t.Errorf("the cut of turn %d was not logged with %s: %s", cut.turn, want, line)
					}
				}
			}
			if !strings.Contains(r.logs.String(), "a second interruption over the closing line") {
				t.Errorf("the second cut was not logged as D:\n%s", r.logs.String())
			}
			r.assertNotEndedOnTheCap(t)
		})
	}
}

// The session's own barge-in over a line still being generated, with no
// cancellation from the provider at all, is counted the same way.
func TestACallerBargeInWhileTheLineIsGeneratingIsCounted(t *testing.T) {
	r := startCutRig(t, time.Minute)
	r.endArmingTurn(t)
	turn := r.generating(t, lineFrames*73/100, generatingTail)
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	r.await(t, "the cut to be counted", func() bool { return r.interruptions() == 1 })
	r.leg.holdFrames(0)
	if r.isCallEnded() || !r.actions.isArmed() {
		t.Fatal("the barge-in ran the ending")
	}
	r.state(func(a *callActions) {
		if !a.isLineOwed {
			t.Error("A applied to a line still being generated")
		}
	})
	r.awaitCutLogged(t, turn)
	logs := r.logs.String()
	if !strings.Contains(logs, "isByProvider=false") || strings.Contains(logs, "counting it as said") {
		t.Errorf("the barge-in was not logged as the caller's, or A applied:\n%s", logs)
	}
}

// One cut, reported by both sides: by the session's barge-in and by the
// provider's cancellation, in both orders, in the event shapes of the Realtime
// client on qwen (a cancelled response.done, speech_started) and of doubao (a
// bare response.done with the turn open, transcription.started). It is counted
// once, and the provider is told once how much was heard. A second cut after
// it, logged, proves the first pair was handled in full.
func TestOneCutReportedByBothSidesIsCountedOnce(t *testing.T) {
	for _, tt := range []struct {
		name            string
		profile         provider.Profile
		isProviderFirst bool
	}{
		{"qwen/cancellation first", provider.QwenProfile(), true},
		{"qwen/speech first", provider.QwenProfile(), false},
		{"doubao/response.done first", provider.DoubaoProfile(), true},
		{"doubao/transcription first", provider.DoubaoProfile(), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := startCutRigOn(t, tt.profile, time.Minute)
			r.endArmingTurn(t)
			told := len(r.model.recordedInterrupts())
			first := r.generating(t, shortLineFrames, generatingTail)
			r.cutFrom(t, tt.isProviderFirst, 1)

			second := r.generating(t, shortLineFrames, generatingTail)
			r.cutFrom(t, true, 2)
			r.awaitCutLogged(t, second)

			if n := r.interruptions(); n != 2 {
				t.Errorf("interruptions = %d after two cuts, want 2", n)
			}
			if n := r.cutsLoggedFor(first); n != 1 {
				t.Errorf("the first cut was logged %d times, want once:\n%s", n, r.logs.String())
			}
			if n := len(r.model.recordedInterrupts()) - told; n != 2 {
				t.Errorf("the provider was told of %d interruptions for two cuts: %+v",
					n, r.model.recordedInterrupts())
			}
		})
	}
}

// A turn this client stopped to make room for a line (SYSTEM) was not cut by
// the caller, and a cancellation of a turn already replaced (stale) is not
// about the line in progress: neither is counted. Nor is a cut of the turn the
// hangup came in, which is not the line. The cut after each is.
func TestCutsThatAreNotTheCallerCuttingTheLineAreNotCounted(t *testing.T) {
	t.Run("a line pre-empted it", func(t *testing.T) {
		r := startCutRig(t, time.Minute)
		r.endArmingTurn(t)
		r.generating(t, shortLineFrames, generatingTail)
		r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSystem}
		r.leg.holdFrames(0)
		next := r.generating(t, shortLineFrames, generatingTail)
		r.cutFrom(t, true, 1)
		r.awaitCutLogged(t, next)
		if n := r.interruptions(); n != 1 {
			t.Errorf("interruptions = %d, want only the caller's cut", n)
		}
	})
	t.Run("stale", func(t *testing.T) {
		r := startCutRig(t, time.Minute)
		// The turn the hangup came in is still open when the next begins, and
		// its cancellation arrives after: stale.
		turn := r.generating(t, shortLineFrames, generatingTail)
		r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
		r.await(t, "the stale cancellation to flush", func() bool {
			_, isSpeaking := r.session.audioProgress()
			return !isSpeaking
		})
		r.model.events <- provider.Event{
			Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*shortLineFrames),
		}
		r.cutFrom(t, true, 1)
		// The stale cancellation carries the current turn's number too, so a
		// later turn's cut is what shows both before it have been handled.
		next := r.generating(t, shortLineFrames, generatingTail)
		r.cutFrom(t, true, 2)
		r.awaitCutLogged(t, next)
		if n, logged := r.interruptions(), r.cutsLoggedFor(turn); n != 2 || logged != 1 {
			t.Errorf("interruptions = %d, turn %d logged %d times; want its current cut alone, once, "+
				"and the next turn's", n, turn, logged)
		}
	})
	t.Run("the arming turn", func(t *testing.T) {
		// A closing phase with no line of its own does not mute the turn that
		// called hangup (#441): it goes on speaking, and the caller cuts it.
		logs := &syncBuffer{}
		r := &cutRig{toolPath: startToolPathFull(t, provider.QwenProfile(), "en", noLineFlow, "",
			map[string]any{"name": "Ann"}, logs), logs: logs}
		r.actions.mu.Lock()
		r.actions.graceCap = time.Minute
		r.actions.mu.Unlock()
		r.callTool(t, flow.ToolHangup, `{}`)
		armed := r.session.currentTurn()
		r.leg.holdFrames(generatingTail)
		r.model.events <- provider.Event{
			Type: provider.EventTypeAudioDelta, Audio: make([]byte, qwenFrameBytes*shortLineFrames),
		}
		r.await(t, "the arming turn's audio to be queued", func() bool {
			_, isSpeaking := r.session.audioProgress()
			return isSpeaking
		})
		r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
		r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
		r.await(t, "the arming turn's cut to close it", func() bool { return !r.isResponding() })
		r.leg.holdFrames(0)
		next := r.generating(t, shortLineFrames, generatingTail)
		r.cutFrom(t, true, 1)
		r.awaitCutLogged(t, next)
		if n, logged := r.interruptions(), r.cutsLoggedFor(armed); n != 1 || logged != 0 {
			t.Errorf("interruptions = %d, the arming turn's cut logged %d times; want only the cut of the line",
				n, logged)
		}
	})
}

// The rule itself: a cut of a turn still being generated counts only for a
// turn after the arming one with something audible cut, and never runs the
// action, however little was left.
func TestOnlyAnAudibleCutOfALaterGeneratingTurnCounts(t *testing.T) {
	actions, session, _ := testActions(t, &fakeSwitch{})
	fired := make(chan struct{})
	actions.arm(t.Context(), func() { close(fired) })
	armed := session.currentTurn()
	count := func() int {
		actions.mu.Lock()
		defer actions.mu.Unlock()
		return actions.interruptions
	}

	for _, cut := range []struct {
		name  string
		event Event
	}{
		{"the arming turn", Event{Turn: armed, IsGenerating: true, Unplayed: 2 * time.Second, Played: time.Second}},
		{"nothing audible", Event{Turn: armed + 1, IsGenerating: true, Unplayed: 0, Played: -1}},
		{"nothing heard, nothing queued", Event{Turn: armed + 1, IsGenerating: true, Unplayed: 0, Played: 0}},
		{"a finished turn", Event{Turn: armed + 1, Unplayed: 2 * time.Second, Played: time.Second}},
	} {
		actions.onBargeIn(cut.event)
		if n := count(); n != 0 {
			t.Fatalf("%s: counted %d interruptions", cut.name, n)
		}
	}

	actions.onBargeIn(Event{Turn: armed + 1, IsGenerating: true, Unplayed: 100 * time.Millisecond, Played: -1})
	actions.onBargeIn(Event{Turn: armed + 2, IsGenerating: true, Unplayed: 0, Played: 300 * time.Millisecond,
		IsByProvider: true})
	if n := count(); n != 2 {
		t.Fatalf("counted %d interruptions, want 2", n)
	}
	select {
	case <-fired:
		t.Fatal("a cut while generating ran the armed action")
	case <-time.After(50 * time.Millisecond):
	}
}
