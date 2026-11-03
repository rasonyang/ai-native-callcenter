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

// The caller speaks over the end of the wait line, as on #286 live call
// 01a16256-ddd5-73b7-9335-13322c315bfa: the tool's answer is asked for as the
// caller starts to speak, qwen's turn detection cancels that response with
// its transcript written and none of its audio sent, and the reply to the
// caller is the next thing heard. Before removeUnheardTurn that reply took the
// unheard answer as given, "在的，请问有什么可以帮您？" 3 of 3 (qwen-findings
// W-Q9); with it the reply gave the result 3 of 3. The reply must give it.
//
// Same session and clock as TestLiveQwenWaitLine (testdata/line_session_zh.json,
// q_contract, the line 1 s after the model's call, the result 3 s after it).
// The caller's words, testdata/q_are_you_there.pcm, "哎，喂，你在吗？"
// (qwen3-tts-flash, voice Cherry, 16 kHz mono PCM16, silence trimmed), start
// AICC_LIVE_SPEAK_AT after the call (default 3050ms: 50 ms after the result,
// which put speech_started 180-630 ms after the answer's response.created in
// the three runs with the fix). A run whose answer was not cut before its audio is reported and
// proves nothing either way.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenSpeechOverTheAnswer -v ./internal/provider/
func TestLiveQwenSpeechOverTheAnswer(t *testing.T) {
	runSpeechOverTheAnswer(t, 3050*time.Millisecond, false)
}

// The caller starts over the wait line's tail, before the result, as on live
// call 01a1627d-7eae-77c2-98f7-e997fca088da: speech began about 450 ms before
// the result while the line was still playing, the session's barge-in flushed
// the line's tail (Interrupt SPEECH, 1300 ms played), and qwen cancelled the
// answer's response before any of its audio. The caller's words start 2550 ms
// after the model's call, and the first speech_started after the line began
// gets the session's Interrupt(SPEECH, 1300), as the barge-in on the line's
// tail sends it.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenSpeechOverTheLineTail -v ./internal/provider/
func TestLiveQwenSpeechOverTheLineTail(t *testing.T) {
	runSpeechOverTheAnswer(t, 2550*time.Millisecond, true)
}

func runSpeechOverTheAnswer(t *testing.T, speakAt time.Duration, isBargeIn bool) {
	profile := liveProfile(QwenProfile())
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	if v, err := time.ParseDuration(os.Getenv("AICC_LIVE_SPEAK_AT")); err == nil {
		speakAt = v
	}
	var fixture struct {
		Instructions string
		Announce     string
		Tools        []ToolSpec
	}
	data, err := os.ReadFile("testdata/line_session_zh.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	input, output := profile.FormatsFor(media.LawMu)
	question := readMidClip(t, input, "q_contract")
	overTheLine := readMidClip(t, input, "q_are_you_there")
	// AICC_LIVE_CALLER_CLIP replaces the caller's words with a raw 16 kHz mono
	// PCM16 file (a caller's own audio cut from a call's recording, kept out of
	// the repository).
	if path := os.Getenv("AICC_LIVE_CALLER_CLIP"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		overTheLine = midEncoder(input)(raw)
	}

	cut, given := 0, 0
	for run := 1; run <= runs; run++ {
		tap := &liveLogTap{}
		c := openLiveCall(t, profile, slog.New(tap), SessionConfig{
			Instructions: fixture.Instructions,
			OpeningText:  fixture.Announce,
			Language:     "zh",
			Turn:         DefaultTurnDetection(),
			InputFormat:  input,
			OutputFormat: output,
			Tools:        fixture.Tools,
		})
		r := c.speechOverTheAnswerRound(t, question, overTheLine, speakAt, isBargeIn)
		taken := tap.count("took a turn the caller heard none of")
		tap.print(t)
		c.close(t)
		t.Logf("[#%d] answer cut before audio=%v (written %q) taken out=%d; reply %q; then %q",
			run, r.isAnswerCut, r.cutText, taken, r.replyText, r.laterTexts)
		if !r.isAnswerCut {
			continue
		}
		cut++
		if taken == 0 {
			t.Errorf("[#%d] the unheard answer was not taken out of the conversation", run)
		}
		if strings.Contains(r.replyText, "200") {
			given++
		} else {
			t.Errorf("[#%d] the reply did not give the result: %q", run, r.replyText)
		}
	}
	t.Logf("[qwen] RESULT answer cut before audio %d/%d, result given in the reply %d/%d",
		cut, runs, given, cut)
	if cut == 0 {
		t.Log("no run cut the answer before its audio; move AICC_LIVE_SPEAK_AT")
	}
}

type speechOverTheAnswerRun struct {
	isAnswerCut bool   // the answer's turn ended cancelled with no audio
	cutText     string // what that turn had written
	replyText   string // the next turn with audio after it
	laterTexts  []string
}

// speechOverTheAnswerRound plays the greeting out, asks the question, says
// the wait line over the slow tool and sends its result on the call's clock,
// and has the caller speak at speakAt after the model's call.
func (c *midCall) speechOverTheAnswerRound(t *testing.T, question, overTheLine []byte,
	speakAt time.Duration, isBargeIn bool) speechOverTheAnswerRun {
	t.Helper()
	opening := c.expectReply(t, "opening")
	t.Logf("opening: %q", opening.text)
	c.say(question)

	var r speechOverTheAnswerRun
	var call Event
	var calledAt time.Time
	var lineAt, resultAt, callerAt, quietAt <-chan time.Time
	isResultSent := false
	isLineStarted, isBargedIn := false, false
	turn := ""
	audio := 0
	text := ""
	since := func() time.Duration { return time.Since(calledAt).Round(time.Millisecond) }
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("no end within 60 s (reply %q)", r.replyText)
		case <-quietAt:
			return r
		case <-lineAt:
			lineAt = nil
			if err := c.session.SpeakText(liveWaitLine, false); err != nil {
				t.Fatalf("speak: %v", err)
			}
		case <-callerAt:
			callerAt = nil
			t.Logf("T+%v the caller starts speaking", since())
			c.mu.Lock()
			c.queue = append(c.queue, overTheLine...)
			c.mu.Unlock()
		case <-resultAt:
			resultAt = nil
			isResultSent = true
			t.Logf("T+%v the result is sent", since())
			if err := c.session.SendToolResult(call.ToolCallID,
				`{"answer":"合约期内变更套餐需支付200元违约金，由人工客服办理。","found":"1","ok":"1"}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeToolCall:
				if call.ToolCallID == "" {
					call, calledAt = ev, time.Now()
					lineAt = time.After(time.Second)
					resultAt = time.After(3 * time.Second)
					callerAt = time.After(speakAt)
				}
			case EventTypeSpeechStarted:
				if !calledAt.IsZero() {
					t.Logf("T+%v speech started", since())
				}
				if isBargeIn && isLineStarted && !isBargedIn {
					isBargedIn = true
					if err := c.session.Interrupt(InterruptReasonSpeech, 1300); err != nil {
						t.Fatalf("interrupt: %v", err)
					}
				}
			case EventTypeResponseStarted:
				quietAt = nil
				audio, text = 0, ""
				switch {
				case ev.IsSpokenLine:
					turn = "line"
					isLineStarted = true
				case isResultSent:
					turn = "after the result"
				default:
					turn = "before the result"
				}
				if !calledAt.IsZero() {
					t.Logf("T+%v turn started: %s", since(), turn)
				}
			case EventTypeAudioDelta:
				audio += len(ev.Audio)
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					text += ev.Text
				}
			case EventTypeInterrupted, EventTypeResponseDone:
				if calledAt.IsZero() {
					break
				}
				t.Logf("T+%v turn ended: %s %s audio=%d text=%q", since(), turn, ev.Type, audio, text)
				if turn != "after the result" {
					break
				}
				switch {
				case ev.Type == EventTypeInterrupted && audio == 0 && r.replyText == "":
					if !r.isAnswerCut {
						r.isAnswerCut, r.cutText = true, text
					}
				case audio > 0 && r.replyText == "":
					r.replyText = text
				case audio > 0:
					r.laterTexts = append(r.laterTexts, text)
				}
				quietAt = time.After(5 * time.Second)
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
			}
		}
	}
}

// What the caller's reply says when the answer's turn went unheard, with the
// history in each state removeUnheardTurn and noteUnheardResult can leave it
// in. Built without timing: the model calls kb_search_slow, the wait line is
// said, the result goes into the conversation with no request for its turn,
// the arm's items follow, and the caller's words are answered by the
// provider's own turn detection.
//
// Arms: "kept" leaves the cut answer's words in the history as an assistant
// item ("合约期内变更套餐需支付200元违约金，由", what live call 01a16256-ddd5 kept);
// "kept-note" adds the note after it; "clean" has no answer item (deleted);
// "clean-note" adds the note to that.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run 'LiveQwenUnheardResultNote/<arm>' -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 3) is how many sessions an arm opens;
// AICC_LIVE_CALLER_CLIP replaces the caller's words as above, and
// AICC_LIVE_FOLLOWUP_CLIP (raw 16 kHz PCM16) is a second caller turn after the
// reply, to see whether the note makes the result be said again.
func TestLiveQwenUnheardResultNote(t *testing.T) {
	for _, arm := range []struct {
		name           string
		isKept, isNote bool
	}{
		{"kept", true, false},
		{"kept-note", true, true},
		{"clean", false, false},
		{"clean-note", false, true},
	} {
		t.Run(arm.name, func(t *testing.T) { runUnheardResultNote(t, arm.isKept, arm.isNote) })
	}
}

func runUnheardResultNote(t *testing.T, isKept, isNote bool) {
	profile := liveProfile(QwenProfile())
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	var fixture struct {
		Instructions string
		Announce     string
		Tools        []ToolSpec
	}
	data, err := os.ReadFile("testdata/line_session_zh.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	input, output := profile.FormatsFor(media.LawMu)
	question := readMidClip(t, input, "q_contract")
	caller := readMidClip(t, input, "q_are_you_there")
	if path := os.Getenv("AICC_LIVE_CALLER_CLIP"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		caller = midEncoder(input)(raw)
	}
	var followUp []byte
	if path := os.Getenv("AICC_LIVE_FOLLOWUP_CLIP"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		followUp = midEncoder(input)(raw)
	}

	given := 0
	for run := 1; run <= runs; run++ {
		c := openLiveCall(t, profile, slog.New(&liveLogTap{}), SessionConfig{
			Instructions: fixture.Instructions,
			OpeningText:  fixture.Announce,
			Language:     "zh",
			Turn:         DefaultTurnDetection(),
			InputFormat:  input,
			OutputFormat: output,
			Tools:        fixture.Tools,
		})
		c.expectReply(t, "opening")
		c.say(question)
		call := c.expectReply(t, "the tool call")
		if len(call.tools) == 0 {
			t.Logf("[#%d] no tool call (%q); run skipped", run, call.text)
			c.close(t)
			continue
		}
		if err := c.session.SpeakText(liveWaitLine, false); err != nil {
			t.Fatalf("speak: %v", err)
		}
		c.expectReply(t, "the line")
		send := func(event map[string]any) {
			if err := c.session.sendEvent(event); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
			"type": "function_call_output", "call_id": call.tools[0].ToolCallID,
			"output": `{"answer":"合约期内变更套餐需支付200元违约金，由人工客服办理。","found":"1","ok":"1"}`}})
		if isKept {
			send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
				"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "合约期内变更套餐需支付200元违约金，由"}}}})
		}
		if isNote {
			send(userTextItem(unheardResultNote("zh")))
		}
		time.Sleep(300 * time.Millisecond)
		c.say(caller)
		var reply midReply
		for reply.text == "" {
			reply = c.expectReply(t, "the reply")
		}
		follow := ""
		if followUp != nil {
			c.say(followUp)
			follow = c.expectReply(t, "the follow-up").text
		}
		c.close(t)
		isGiven := strings.Contains(reply.text, "200")
		if isGiven {
			given++
		}
		t.Logf("[#%d] reply %q given=%v; follow-up %q", run, reply.text, isGiven, follow)
	}
	t.Logf("[qwen kept=%v note=%v] RESULT result given in the reply %d/%d", isKept, isNote, given, runs)
}
