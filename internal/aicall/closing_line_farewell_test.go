// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Issue #568: a caller who talks over a terminal phase's long closing line
// twice, and whose second interruption lands on the re-asked line, ends the
// call when the model's answer has played — with no goodbye heard, because
// what was owed was the whole announce. A terminal phase may therefore split
// its closing line: the announce carries the instructions and a short
// farewell carries the goodbye, and the farewell alone is what the caller is
// owed. The line still speaks as one — announce, then the farewell — but the
// re-ask asks for the short goodbye, short enough to be heard even when the
// next interruption ends the call.

// splitLineFlow is owedLineFlow's closing line split in two: a long
// announce of instructions, and the goodbye it ends in declared apart.
var splitLineFlow = strings.Replace(owedLineFlow,
	`"announce": {
				"en": "Thank you {slots.name}, goodbye 👋",
				"zh": "谢谢您{slots.name}，再见 👋"
			},`,
	`"announce": {
				"en": "Thank you for calling. Your message has been taken and a colleague will call you back tomorrow.",
				"zh": "感谢您的来电，您的留言已记录，明天会有同事给您回电。"
			},
			"farewell": {
				"en": "Goodbye and take care of yourself, {slots.name}.",
				"zh": "再见，请保重，{slots.name}。"
			},`, 1)

// splitAnnounce is the instructions; splitFarewell is the goodbye, rendered
// for Ann; splitLine is the two as the phase speaks them.
const (
	splitAnnounce = "Thank you for calling. Your message has been taken and a colleague will call you back tomorrow."
	splitFarewell = "Goodbye and take care of yourself, Ann."
	splitLine     = splitAnnounce + "\n" + splitFarewell
)

// startFarewellRig is startCutRig on a flow whose terminal phase splits its
// line, leaving the model about to say it.
func startFarewellRig(t *testing.T, graceCap time.Duration) *cutRig {
	t.Helper()
	logs := &syncBuffer{}
	h := startToolPathFull(t, provider.QwenProfile(), "en", splitLineFlow, "",
		map[string]any{"name": "Ann"}, logs)
	h.actions.mu.Lock()
	h.actions.graceCap = graceCap
	h.actions.mu.Unlock()
	h.callTool(t, flow.ToolHangup, goodbyeHangup)
	return &cutRig{toolPath: h, logs: logs}
}

// The phase speaks announce and farewell as one line, and the line its caller
// must have heard is the farewell alone. A cut inside the instructions leaves
// the farewell owed: the answer does not say it, so the short goodbye — not
// the whole line — is asked for, and its playback ends the call.
func TestACutOverTheInstructionsStillEndsOnTheFarewell(t *testing.T) {
	r := startFarewellRig(t, time.Minute)
	// The line rides the tool result whole: instructions, then the goodbye.
	if got := hintOf(t, r.model.recordedToolResults()[0].output); got != provider.SayExactly(splitLine, "en") {
		t.Fatalf("the line's hint = %q, want %q", got, provider.SayExactly(splitLine, "en"))
	}
	// A 15 s line, cut with six seconds — the farewell itself — still queued.
	r.lineTurn(t, lineFrames, lineFrames, splitLine)
	r.interrupt(t, lineFrames-lineFrames*60/100, 1)
	if r.isCallEnded() || !r.actions.isArmed() {
		t.Fatal("the ending ran on a cut inside the instructions")
	}
	var isOwed bool
	r.state(func(a *callActions) { isOwed = a.isLineOwed })
	if !isOwed {
		t.Fatal("a cut inside the instructions counted the farewell as said")
	}
	// The model answers the caller's question; the answer is not the
	// farewell, so the farewell alone is asked for.
	r.say(100, 0, "A colleague calls you back tomorrow morning.")
	if got := r.awaitLinesSpoken(t, 1); got[0] != splitFarewell {
		t.Fatalf("asked for %q, want the farewell alone %q", got[0], splitFarewell)
	}
	r.stayUp(t, "the call ended on an answer that was not the line")
	// The model says the farewell; its playback done, the call ends.
	r.lineTurn(t, 150, 0, splitFarewell)
	r.awaitCallEnded(t, 15*time.Second)
	if n := len(r.asked()); n != 1 {
		t.Errorf("the line was asked for %d times, want 1", n)
	}
}

// The issue's own walk: agree, then a question about five seconds into the
// closing line; the bot answers and repeats the line — now only the farewell.
// Two seconds into the repeat, another question: the second interruption
// counts the farewell as said (rule D, #465) and the call ends when the
// answer has played — but the goodbye was heard, because what repeated was
// three seconds, not fifteen.
func TestASecondCutOnTheFarewellStillEndsAfterTheAnswer(t *testing.T) {
	r := startFarewellRig(t, time.Minute)
	r.lineTurn(t, lineFrames, lineFrames, splitLine)
	r.interrupt(t, lineFrames-lineFrames/3, 1) // about five seconds in
	// The answer to the question; the farewell re-asked, alone.
	r.say(100, 0, "A colleague calls you back tomorrow.")
	if got := r.awaitLinesSpoken(t, 1); got[0] != splitFarewell {
		t.Fatalf("re-ask = %q, want the farewell alone %q", got[0], splitFarewell)
	}
	// Two seconds into the repeat the caller asks again — the second
	// interruption counts the farewell as said, and the cap stops moving.
	r.lineTurn(t, 250, 250, splitFarewell)
	due := r.capDue()
	r.interrupt(t, 150, 2)
	var saidByRule bool
	r.state(func(a *callActions) { saidByRule = a.isLineSaidByRule })
	if !saidByRule {
		t.Error("the second interruption did not count the farewell as said")
	}
	if n := r.interruptions(); n != 2 {
		t.Errorf("interruptions = %d, want 2", n)
	}
	if r.isCallEnded() {
		t.Fatal("the call ended on the interruption, not the answer")
	}
	// The model answers; the ending runs when that answer has played.
	r.say(60, 0, "He will call in the morning.")
	r.awaitCallEnded(t, 15*time.Second)
	if n := len(r.asked()); n != 1 {
		t.Errorf("the line was asked for %d times, want 1", n)
	}
	if after := r.capDue(); after != due {
		t.Errorf("the cap moved on the second interruption: %v -> %v", due, after)
	}
}
