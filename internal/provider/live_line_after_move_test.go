// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures, against the real provider, a phase's line asked for after a
// tool has moved the call into a phase the conversation goes on from (#351).
// It costs money, so it is skipped unless AICC_LIVE_PROVIDER_TEST and the
// profile's key are set.
//
// Live call 01a17c7d-b4d4-7870-883e-b82a9553f3c6 (2026-10-27, qwen): an
// outbound flow's entry phase recorded the caller's agreement with a slot tool
// and moved to a phase the conversation goes on from, whose announce was a
// fixed line and whose instruction said that any reply, agreement included,
// means hangup. The line's turn called hangup, the client kept the call back
// (onLineCall), and the caller heard nine seconds of silence until they spoke.
//
// testdata/line_after_move_flow.json is a flow of that shape in the NovaNet
// Broadband world, in both languages: a speed-upgrade call whose entry phase
// has four slot tools, one of which (confirm_upgrade) moves the call to a
// phase whose line confirms the booking and whose instruction says what to
// call on the caller's next words. testdata/line_after_move.json is that flow
// as flow.Runtime renders it (the entry phase's standing instructions, opening
// line and tools, the slot tool's output on the caller's agreement, its hint
// the new phase's instruction, and the new phase's instructions and line),
// written by flow.TestRenderLineAfterMoveSession. How often it draws the
// defect on main is in qwen-findings W-Q10. testdata/q_agree_zh.pcm ("好的。",
// macOS say, voice Tingting) and testdata/q_agree_en.pcm ("Okay, sure.", voice
// Samantha) are the caller, 16 kHz mono PCM16.
//
// One session per run, driven as the orchestrator drives a call: the caller
// agrees; on the slot tool's call the instructions are re-pinned and the
// result is sent; when that response ends the line is asked for (SpeakText,
// not closing, as closeToolBatch does). The caller answers the line 4 s after
// the last line turn ends, with the same agreement, and the phase's
// instruction says that reply is a hangup. A line turn must hand out no call
// and must say the line; what the client did is read from its log. A run in
// which the turn the result asked for calls a tool before the line pre-empts
// it is counted apart and not measured: the orchestrator would run that call
// and leave the phase.
//
//	AICC_LIVE_PROVIDER_TEST=1 AICC_LIVE_RUNS=10 go test -count=1 -run 'LiveQwenLineAfterMove/zh' -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 3) is how many sessions each language opens.
// AICC_LIVE_LINE_FIXTURE names another file of the same shape in place of
// testdata/line_after_move.json: a deployment's own flow rendered the same
// way (flow.TestRenderLineAfterMoveSession), which stays on the machine that
// measures it. A language it lacks is skipped.
func TestLiveQwenLineAfterMove(t *testing.T) {
	for _, language := range []string{"zh", "en"} {
		t.Run(language, func(t *testing.T) { runLineAfterMove(t, QwenProfile(), language) })
	}
}

// lineAfterMoveSession is one language of testdata/line_after_move.json.
type lineAfterMoveSession struct {
	Instructions      string     `json:"instructions"`
	Opening           string     `json:"opening"`
	Tools             []ToolSpec `json:"tools"`
	Temperature       *float64   `json:"temperature,omitempty"`
	AgreeTool         string     `json:"agreeTool"`
	AgreeOutput       string     `json:"agreeOutput"`
	PhaseInstructions string     `json:"phaseInstructions"`
	PhaseLine         string     `json:"phaseLine"`
}

// lineAfterMoveRun is what one session did.
type lineAfterMoveRun struct {
	isMoved     bool          // the model called the slot tool on the agreement
	lineTurns   int           // turns started as a spoken line
	lineAudio   int           // audio bytes of those turns
	lineAfter   time.Duration // from asking for the line to its first audio
	lineText    string        // their transcript
	handedOut   []string      // calls handed out in a line's turn
	isReplied   bool          // the caller answered the line
	isHungUp    bool          // the model called hangup on that answer
	keptBack    int           // calls a line's turn made that the client kept back
	askedAgain  int           // lines the client asked for again
	givenUp     int           // lines asked again whose turn called a tool too
	takenOut    int           // directions of a line asked again taken back out
	otherTools  []string      // calls in no line's turn other than the expected two
	beforeLine  []string      // calls handed out after the move and before any line turn
	replyText   string        // what the model said after the caller's answer
	isLineSaid  bool
	firstSilent bool // the first line turn carried no audio
}

func runLineAfterMove(t *testing.T, profile Profile, language string) {
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	path := "testdata/line_after_move.json"
	if other := os.Getenv("AICC_LIVE_LINE_FIXTURE"); other != "" {
		path = other
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]lineAfterMoveSession
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	fx, ok := fixtures[language]
	if !ok {
		t.Skipf("%s has no %s session", path, language)
	}
	input, output := profile.FormatsFor(media.LawMu)
	agree := readMidClip(t, input, "q_agree_"+language)

	// moved counts the runs that measure the line: the model took the
	// agreement and the result's turn did not act before the line.
	moved, early, said, kept, again, given, silent, hungUp, handedOut := 0, 0, 0, 0, 0, 0, 0, 0, 0
	for run := 1; run <= runs; run++ {
		tap := &liveLogTap{}
		c := openLiveCall(t, profile, slog.New(tap), SessionConfig{
			Instructions: fx.Instructions,
			OpeningText:  fx.Opening,
			Language:     language,
			Turn:         DefaultTurnDetection(),
			InputFormat:  input,
			OutputFormat: output,
			Tools:        fx.Tools,
			Temperature:  fx.Temperature,
		})
		r := c.lineAfterMoveRound(t, fx, agree)
		r.keptBack = tap.count("the turn asked for a line called a tool;")
		r.askedAgain = tap.count("asking for the line once more")
		r.givenUp = tap.count("giving it up")
		r.takenOut = tap.count("took the direction of the line asked again out")
		tap.print(t)
		c.close(t)
		r.isLineSaid = r.lineAudio > 0 &&
			strings.Contains(midNormalize(r.lineText), midNormalize(fx.PhaseLine))
		t.Logf("[%s #%d] moved=%v beforeLine=%v lineTurns=%d firstSilent=%v said=%v after=%v text=%q "+
			"keptBack=%d askedAgain=%d givenUp=%d takenOut=%d handedOut=%v other=%v replied=%v hungUp=%v reply=%q",
			language, run, r.isMoved, r.beforeLine, r.lineTurns, r.firstSilent, r.isLineSaid,
			r.lineAfter.Round(time.Millisecond), r.lineText, r.keptBack, r.askedAgain, r.givenUp, r.takenOut,
			r.handedOut, r.otherTools, r.isReplied, r.isHungUp, r.replyText)
		handedOut += len(r.handedOut)
		if !r.isMoved {
			continue
		}
		if len(r.beforeLine) > 0 {
			early++
			continue
		}
		moved++
		if r.isLineSaid {
			said++
		}
		if r.keptBack > 0 {
			kept++
		}
		if r.askedAgain > 0 {
			again++
		}
		if r.givenUp > 0 {
			given++
		}
		if r.firstSilent {
			silent++
		}
		if r.isHungUp {
			hungUp++
		}
	}
	t.Logf("[%s/%s] RESULT measured %d/%d (the result's turn acted before the line in %d); line said %d/%d; "+
		"first line turn silent %d; a line turn's call kept back %d; asked again %d; given up %d; "+
		"calls handed out in a line's turn %d; hangup on the caller's answer %d/%d",
		profile.Name, language, moved, runs, early, said, moved, silent, kept, again, given, handedOut, hungUp, moved)
	if handedOut != 0 {
		t.Errorf("%d calls were handed out in a line's turn", handedOut)
	}
	if said != moved {
		t.Errorf("the line was said in %d of %d runs that moved", said, moved)
	}
	// What the line leaves in the history must not change the turn after it.
	if hungUp != moved {
		t.Errorf("the caller's answer to the line was met with hangup in %d of %d runs", hungUp, moved)
	}
}

// lineAfterMoveRound plays the greeting out, has the caller agree, drives the
// move and the line as the orchestrator does, has the caller answer the line,
// and reads until 3 s after the turn that answers them ends (or 60 s).
func (c *midCall) lineAfterMoveRound(t *testing.T, fx lineAfterMoveSession, agree []byte) lineAfterMoveRun {
	t.Helper()
	opening := c.expectReply(t, "opening")
	t.Logf("opening: %q", opening.text)
	c.say(agree)

	var r lineAfterMoveRun
	var askedAt time.Time
	var replyAt, quietAt <-chan time.Time
	isLineAsked := false
	turn := "" // "line", "reply", "other"
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Logf("no end within 60 s")
			return r
		case <-quietAt:
			return r
		case <-replyAt:
			replyAt = nil
			r.isReplied = true
			c.say(agree)
			// A model that answers nothing still ends the run.
			quietAt = time.After(12 * time.Second)
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeToolCall:
				t.Logf("TOOL_CALL %s %s (turn %s)", ev.ToolName, ev.ToolArgs, turn)
				switch {
				case turn == "line":
					r.handedOut = append(r.handedOut, ev.ToolName)
				case !r.isMoved && ev.ToolName == fx.AgreeTool:
					r.isMoved = true
					// A turn that ended before the move (an opening the
					// watchdog gave up on, then said late) armed the end of
					// the run; the move is what the run is for.
					quietAt = nil
					if err := c.session.UpdateInstructions(fx.PhaseInstructions); err != nil {
						t.Fatalf("update instructions: %v", err)
					}
					if err := c.session.SendToolResult(ev.ToolCallID, fx.AgreeOutput); err != nil {
						t.Fatalf("send tool result: %v", err)
					}
				case r.isReplied && ev.ToolName == liveHangupTool:
					r.isHungUp = true
				case r.isMoved && r.lineTurns == 0:
					// The turn the slot tool's result asked for acted before
					// the line pre-empted it. The orchestrator would run that
					// call and leave the phase: not the case under test.
					r.beforeLine = append(r.beforeLine, ev.ToolName)
				default:
					r.otherTools = append(r.otherTools, ev.ToolName)
				}
			case EventTypeResponseStarted:
				replyAt = nil
				switch {
				case ev.IsSpokenLine:
					turn = "line"
					r.lineTurns++
				case r.isReplied:
					turn = "reply"
				default:
					turn = "other"
				}
				t.Logf("turn started: %s", turn)
			case EventTypeAudioDelta:
				if turn == "line" {
					if r.lineAudio == 0 {
						r.lineAfter = time.Since(askedAt)
					}
					r.lineAudio += len(ev.Audio)
				}
			case EventTypeOutputTranscript:
				if !ev.IsFinal {
					break
				}
				switch turn {
				case "line":
					r.lineText += ev.Text
				case "reply":
					r.replyText += ev.Text
				default:
					t.Logf("said (%s): %q", turn, ev.Text)
				}
			case EventTypeResponseDone, EventTypeInterrupted:
				t.Logf("turn ended: %s (%s)", turn, ev.Type)
				switch {
				case turn == "line":
					if r.lineTurns == 1 && r.lineAudio == 0 {
						r.firstSilent = true
					}
					if !r.isReplied {
						replyAt = time.After(4 * time.Second)
					}
				case r.isReplied:
					quietAt = time.After(3 * time.Second)
				case r.isMoved && !isLineAsked:
					// closeToolBatch: the batch's response is over, so the
					// line its move left owed is asked for now.
					isLineAsked, askedAt = true, time.Now()
					if err := c.session.SpeakText(fx.PhaseLine, false); err != nil {
						t.Fatalf("speak: %v", err)
					}
				case !r.isMoved:
					// The model did not take the agreement as one: not the
					// case under test.
					quietAt = time.After(2 * time.Second)
				}
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
			}
		}
	}
}

// liveHangupTool is the built-in the phase tells the model to call
// on the caller's answer; the provider package does not know flow's names.
const liveHangupTool = "hangup"
