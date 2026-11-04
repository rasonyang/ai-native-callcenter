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
	flowJSON := strings.Replace(owedLineFlow, "Thank you {slots.name}, goodbye 👋", cutLine, 1)
	logs := &syncBuffer{}
	h := startToolPathFull(t, provider.QwenProfile(), "en", flowJSON, "",
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
