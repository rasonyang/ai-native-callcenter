// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures, against the real provider, what the turn a tool result asks
// for says when one call looks something up three times in a row (#466). It
// costs money, so it is skipped unless AICC_LIVE_PROVIDER_TEST and the
// profile's key are set.
//
// The defect: on qwen-audio-3.1-realtime-flash, in a call on the seed flow
// mobile_support (zh), the model said a filler ("好的，我查一下。") in the turn
// that calls the HTTP tool repair_status, and on the 2nd and later lookups of
// the call the turn the tool result asks for said it again instead of reading
// the result. The fix is in the standing instructions (flow.Runtime's
// lookupSilenceRule and the action-claim rule's example), so the fixture
// already carries it. The counts measured before and after are in
// docs/design/qwen-findings.md.
//
// One session is one call, driven as the orchestrator drives a call
// (answerToolCall, sayWaitLine): the caller gives three order numbers in turn,
// in the order not found (RMA9999), found (RMA1002), found (RMA1001). The tool
// call is answered from the event loop while its own turn may still be open;
// when the lookup moved the call (or changed what the phase's instruction
// renders) the standing instructions are re-pinned before the result is sent.
// A result turn passes when it carries the result's key field and does not
// repeat the previous bot text (the tool-call turn's, or the wait line's).
// Lookup 2 of every session, and lookup 1 of even-numbered sessions, take 2 s,
// so the platform's wait line (the fixture's waitLine, said by SpeakText 1 s
// after the call if the tool-call turn is quiet) enters the history; the other
// lookups answer after repeatFastLatency (default 3 ms).
//
// Fixture: testdata/repeat_lookup.json is mobile_support as flow.Runtime
// renders it (the initial instructions, opening line and tools, and per lookup
// the tool's output, the phase it moved to and the re-rendered instructions),
// the lookups run against internal/mockbackend. It is written by
// flow.TestRenderRepeatLookupSession:
//
//	AICC_REPEAT_FLOW=../seed/flows/mobile_support.json \
//	AICC_REPEAT_OUT=../provider/testdata/repeat_lookup.json \
//	go test -count=1 -run TestRenderRepeatLookupSession ./internal/flow/
//
// Caller clips (testdata/q_rma9999_zh.pcm, q_rma1002_zh.pcm, q_rma1001_zh.pcm,
// q_yes_zh.pcm; 16 kHz mono PCM16 little-endian, header stripped) were made on
// 2026-11-04 with macOS `say -v Tingting "<text>" -o x.aiff`, then
// `afconvert -f WAVE -d LEI16@16000 -c 1 x.aiff x.wav` and the WAVE header
// removed. The order number is spelled out so it is read character by
// character: "我想查一下维修进度，维修单号是 R M A 9 9 9 9。" (5.0 s), "那再帮我查一下
// R M A 1 0 0 2。" (2.9 s), "还有一个，R M A 1 0 0 1。" (2.4 s) and "对，没错。"
// (1.1 s, played once only when the model reads the number back instead of
// calling the tool).
//
// Three ingredients reproduced the defect; the TTS clips alone did not:
//
//   - The owner's own utterances (AICC_LIVE_REPEAT_CLIPS names a directory with
//     the same four file names, recorded on a real call and kept locally).
//   - Caller barge-in (AICC_LIVE_REPEAT_BARGE, default on, "0" for off). The
//     caller speaks over the tail of the previous answer before lookups 2 and
//     3, so lookups 2 and 3 do not wait for the previous result turn to
//     finish: the clip is queued so that the caller's speech starts about 1.0 s
//     before that turn's audio would finish playing (wall clock from its first
//     audio; 200 ms of room tone precede the clip; a turn shorter than 2 s is
//     barged at its midpoint). On the provider's SPEECH_STARTED, while the turn
//     is still "playing", the test does what aicall does (Session.bargeIn):
//     Interrupt with InterruptReasonSpeech and playedMs = the time since the
//     turn's first audio. Its generation has ended, so the client sends no
//     cancel, only conversation.item.truncate. A SPEECH_STARTED after the
//     playback end is not interrupted and is counted as bargeMissed.
//   - A tool that answers within a few milliseconds (the 3 ms default), while
//     the tool-call turn's filler was still being made.
//
// Before the first clip the test waits until the opening would have finished
// playing (bytes received, in the output format, queued behind the turn's
// first audio) plus about 1 s, as a real caller does.
//
//	AICC_LIVE_PROVIDER_TEST=1 AICC_LIVE_RUNS=10 \
//	go test -count=1 -timeout 60m -run TestLiveQwenRepeatLookup -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 3) is the number of sessions, run one after another.
func TestLiveQwenRepeatLookup(t *testing.T) {
	profile := liveProfile(QwenProfile())
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	isBarge := os.Getenv("AICC_LIVE_REPEAT_BARGE") != "0"
	bargeMode := "off"
	if isBarge {
		bargeMode = "on"
	}
	t.Logf("profile %s model %s barge=%s sessions %d", profile.Name, profile.Model, bargeMode, runs)

	data, err := os.ReadFile("testdata/repeat_lookup.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]repeatLookupSession
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	fx, ok := fixtures["zh"]
	if !ok || len(fx.Lookups) != 3 {
		t.Fatalf("testdata/repeat_lookup.json has no zh session of three lookups")
	}
	input, output := profile.FormatsFor(media.LawMu)
	clips := []string{"rma9999", "rma1002", "rma1001"}
	pcm := make([][]byte, len(clips))
	for i, name := range clips {
		pcm[i] = readRepeatClip(t, input, "q_"+name+"_zh")
	}
	yes := readRepeatClip(t, input, "q_yes_zh")

	var total repeatTotals
	for run := 1; run <= runs; run++ {
		tap := &liveLogTap{}
		c := openLiveCall(t, profile, slog.New(tap), SessionConfig{
			Instructions: fx.Instructions,
			OpeningText:  fx.Opening,
			Language:     "zh",
			Turn:         DefaultTurnDetection(),
			InputFormat:  input,
			OutputFormat: output,
			Tools:        fx.Tools,
			Temperature:  fx.Temperature,
		})
		opening := collectTurn(t, c, "opening")
		t.Logf("[#%d] opening: %q", run, opening.text)
		d := &repeatDriver{t: t, c: c, fx: fx, output: output, run: run, yes: yes, isBarge: isBarge}
		if !opening.firstAudio.IsZero() {
			d.playEnd = opening.firstAudio.Add(audioDuration(output, opening.audioBytes))
		}
		var results [3]repeatLookup
		for i := range results {
			results[i] = d.lookup(i, pcm[i])
		}
		tap.print(t)
		c.close(t)

		t.Logf("[#%d] model=%s barge=%s lookups: 1%s 2%s 3%s", run, profile.Model, bargeMode,
			results[0].describe(), results[1].describe(), results[2].describe())
		total.add(results)
	}
	t.Logf("[%s/%s/barge=%s] RESULT sessions=%d sessionsWithRepeat=%d resultTurnsPassed=%d/%d "+
		"(by lookup: %d/%d %d/%d %d/%d) foundZeroAsksToCheck=%d/%d silent=%d notCalled=%d "+
		"argsMismatch=%d waitLinesSaid=%d bargeMissed=%d sessionsFailed=%d noTranscript=%d latency=%v",
		profile.Name, profile.Model, bargeMode, runs, total.sessionsWithRepeat, total.passed(), total.measured(),
		total.pass[0], total.seen[0], total.pass[1], total.seen[1], total.pass[2], total.seen[2],
		total.asksToCheck, total.seen[0], total.silent, total.notCalled, total.argsMismatch, total.waitLines, total.bargeMissed,
		total.sessionsFailed, total.noTranscript, repeatFastLatency())
	if failed := total.measured() - total.passed(); failed > 0 {
		t.Errorf("%d of %d measured result turns failed", failed, total.measured())
	}
}

// repeatLookupSession is one language of testdata/repeat_lookup.json.
type repeatLookupSession struct {
	Instructions string     `json:"instructions"`
	Opening      string     `json:"opening"`
	Tools        []ToolSpec `json:"tools"`
	Temperature  *float64   `json:"temperature,omitempty"`
	Tool         string     `json:"tool"`
	WaitLine     string     `json:"waitLine"`
	Lookups      []struct {
		RMA          string `json:"rma"`
		Output       string `json:"output"`
		Moved        string `json:"moved"`
		Instructions string `json:"instructions"`
	} `json:"lookups"`
}

// audioDuration is how long n bytes of audio in format play.
func audioDuration(format media.AudioFormat, n int) time.Duration {
	bytesPerSample := 2
	if format.Encoding == media.EncodingMuLaw || format.Encoding == media.EncodingALaw {
		bytesPerSample = 1
	}
	return time.Duration(n) * time.Second / time.Duration(format.RateHz*bytesPerSample)
}

// repeatTurn is what one bot turn carried.
type repeatTurn struct {
	started    bool
	ended      bool
	firstAudio time.Time
	audioBytes int
	text       string
	tools      []string
}

// repeatLookup is what one lookup of a session did.
type repeatLookup struct {
	rma           string
	isCalled      bool
	args          string
	isArgsWrong   bool
	isSlow        bool
	isWaitLine    bool
	isCallOpen    bool // the TOOL_CALL arrived while its response was still open: the result was owed
	isSentOpen    bool // the result was sent while that response was still open
	toolTurnText  string
	waitText      string
	resultText    string
	resultAudio   int
	resultTools   []string
	extraTurns    int
	isKeyField    bool
	asksToCheck   bool
	repeat        string // why the result turn counts as a repeat; empty if it does not
	isSilent      bool
	isBarged      bool   // the caller's speech interrupted the previous answer's tail
	isBargeMissed bool   // the caller's speech started after the previous answer's playback end
	readBack      string // what the model said when it read the number back instead of calling
}

func (l repeatLookup) pass() bool { return l.isKeyField && l.repeat == "" && !l.isSilent }

// describe is the lookup's part of the session line, texts in full.
func (l repeatLookup) describe() string {
	return fmt.Sprintf("{called=%v args=%s argsWrong=%v slow=%v waitLine=%v callWhileOpen=%v resultSentWhileOpen=%v "+
		"toolTurnText=%q waitText=%q readBack=%q resultText=%q resultAudio=%d resultTools=%v extraTurns=%d "+
		"keyField=%v asksToCheck=%v repeat=%q silent=%v barged=%v bargeMissed=%v}",
		l.isCalled, l.args, l.isArgsWrong, l.isSlow, l.isWaitLine, l.isCallOpen, l.isSentOpen,
		l.toolTurnText, l.waitText, l.readBack, l.resultText, l.resultAudio, l.resultTools, l.extraTurns,
		l.isKeyField, l.asksToCheck, l.repeat, l.isSilent, l.isBarged, l.isBargeMissed)
}

// repeatTotals accumulates the sessions.
type repeatTotals struct {
	sessionsWithRepeat int
	pass, seen         [3]int
	asksToCheck        int
	silent, notCalled  int
	argsMismatch       int
	waitLines          int
	bargeMissed        int
	sessionsFailed     int // sessions with any measured result turn that failed
	noTranscript       int // result turns with audio and no transcript
}

func (r *repeatTotals) passed() int   { return r.pass[0] + r.pass[1] + r.pass[2] }
func (r *repeatTotals) measured() int { return r.seen[0] + r.seen[1] + r.seen[2] }

func (r *repeatTotals) add(results [3]repeatLookup) {
	isRepeat, isFailed := false, false
	defer func() {
		if isFailed {
			r.sessionsFailed++
		}
	}()
	for i, l := range results {
		if !l.isCalled {
			r.notCalled++
			continue
		}
		r.seen[i]++
		if l.pass() {
			r.pass[i]++
		} else {
			isFailed = true
		}
		// A turn with audio and no transcript cannot be judged by its words;
		// it fails, and is counted apart so the summary shows it.
		if l.resultAudio > 0 && l.resultText == "" {
			r.noTranscript++
		}
		if l.repeat != "" {
			isRepeat = true
		}
		if i == 0 && l.asksToCheck {
			r.asksToCheck++
		}
		if l.isSilent {
			r.silent++
		}
		if l.isArgsWrong {
			r.argsMismatch++
		}
		if l.isWaitLine {
			r.waitLines++
		}
	}
	for _, l := range results {
		if l.isBargeMissed {
			r.bargeMissed++
		}
	}
	if isRepeat {
		r.sessionsWithRepeat++
	}
}

var (
	repeatKeyField = [3]*regexp.Regexp{
		regexp.MustCompile(`查不到|没查到|没找到|没有查到|查询不到|查无|没有找到|找不到|不存在|没有这个|未查到|无法查到`),
		regexp.MustCompile(`Air|寄出|明天|6688|送达|维修完成|已完成`),
		regexp.MustCompile(`X1|维修中|屏幕|测试|工作日`),
	}
	repeatAsksToCheck = regexp.MustCompile(`核对|确认|检查|再.{0,4}(说|报|提供)|重新|是否正确|有没有.{0,3}错`)
	repeatFiller      = regexp.MustCompile(`查一下|稍等|查询一下`)
	repeatReadBack    = regexp.MustCompile(`[0-9]|RMA|对吗|是吗`)
)

// repeatDriver plays one call.
type repeatDriver struct {
	t       *testing.T
	c       *midCall
	fx      repeatLookupSession
	output  media.AudioFormat
	run     int
	yes     []byte
	playEnd time.Time // when everything the bot has said so far has been heard
	isBarge bool
	// The previous result turn: first audio and size, which is what a barge-in
	// is timed against.
	prevFirst time.Time
	prevBytes int
	// isBargeArmed: the clip of a barge-in lookup is queued and its
	// SPEECH_STARTED has not come yet.
	isBargeArmed bool
}

// onAudio extends the playback clock by an audio chunk, queued behind what is
// still playing.
func (d *repeatDriver) onAudio(n int) {
	now := time.Now()
	if d.playEnd.Before(now) {
		d.playEnd = now
	}
	d.playEnd = d.playEnd.Add(audioDuration(d.output, n))
}

// settle waits until the bot's last audio has been heard plus about 1 s, still
// reading the session's events.
func (d *repeatDriver) settle() {
	until := d.playEnd.Add(time.Second)
	if min := time.Now().Add(time.Second); until.Before(min) {
		until = min
	}
	d.drainUntil(until, true)
}

// drainUntil reads the session's events until until. With isSettling, bot
// audio that arrives meanwhile moves the end by its length (the caller waits
// for it); without, the time is fixed (a barge-in's).
func (d *repeatDriver) drainUntil(until time.Time, isSettling bool) {
	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return
		case ev, ok := <-d.c.session.Events():
			if !ok {
				d.t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeAudioDelta:
				d.onAudio(len(ev.Audio))
				if isSettling {
					// New speech moves the end; wait for it too.
					timer.Reset(time.Until(d.playEnd.Add(time.Second)))
				}
			case EventTypeToolCall:
				d.t.Logf("[#%d] stray TOOL_CALL %s %s while waiting for the caller's turn", d.run, ev.ToolName, ev.ToolArgs)
			case EventTypeError:
				d.t.Logf("[#%d] provider error (fatal=%v): %v", d.run, ev.IsFatal, ev.Err)
			}
		}
	}
}

// lookup plays lookup i: the caller's clip, the tool call, its answer and the
// turn the answer asks for.
func (d *repeatDriver) lookup(i int, clip []byte) repeatLookup {
	t, c, fx := d.t, d.c, d.fx
	want := fx.Lookups[i]
	res := repeatLookup{rma: want.RMA}
	slow := (i == 1 || (i == 0 && d.run%2 == 0))
	res.isSlow = slow
	latency, waitAfter := repeatFastLatency(), time.Duration(0)
	if slow {
		latency, waitAfter = 2*time.Second, time.Second
	}

	label := fmt.Sprintf("[#%d lookup %d]", d.run, i+1)
	d.sayClip(label, i, clip)

	var (
		toolTurn, waitTurn, resultTurn, other repeatTurn
		cur                                   *repeatTurn
		callID                                string
		isResultSent                          bool
		isYesPlayed                           bool
		resultTimer, waitTimer, endTimer      <-chan time.Time
		resultDone                            <-chan time.Time
		resultStartBy                         <-chan time.Time
		hardStop                              = time.After(90 * time.Second)
	)
	other = repeatTurn{}
	cur = &other

	sendResult := func() {
		res.isSentOpen = !toolTurn.ended && toolTurn.started
		if want.Instructions != "" {
			if err := c.session.UpdateInstructions(want.Instructions); err != nil {
				t.Fatalf("update instructions: %v", err)
			}
		}
		if err := c.session.SendToolResult(callID, want.Output); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
		isResultSent = true
		resultStartBy = time.After(12 * time.Second)
		t.Logf("%s result sent (re-pinned=%v, tool-call turn still open=%v)", label, want.Instructions != "", res.isSentOpen)
	}

	finish := func() repeatLookup {
		if resultTurn.audioBytes > 0 {
			d.prevFirst, d.prevBytes = resultTurn.firstAudio, resultTurn.audioBytes
		}
		res.toolTurnText = toolTurn.text
		res.resultText = resultTurn.text
		res.resultAudio = resultTurn.audioBytes
		res.resultTools = resultTurn.tools
		res.waitText = waitTurn.text
		if res.isWaitLine && res.waitText == "" {
			res.waitText = fx.WaitLine
		}
		if !res.isCalled {
			return res
		}
		res.isSilent = !resultTurn.started
		res.isKeyField = repeatKeyField[i].MatchString(resultTurn.text)
		if i == 0 {
			res.asksToCheck = repeatAsksToCheck.MatchString(resultTurn.text)
		}
		previous := toolTurn.text
		if res.isWaitLine {
			previous = res.waitText
		}
		got := midNormalize(resultTurn.text)
		switch {
		case res.isSilent:
		case got != "" && strings.Contains(midNormalize(previous), got):
			res.repeat = "same as the previous bot text"
		case !res.isKeyField && repeatFiller.MatchString(resultTurn.text):
			res.repeat = "no key field and a filler"
		}
		t.Logf("%s toolTurn=%q waitLine=%q result=%q keyField=%v asksToCheck=%v repeat=%q silent=%v",
			label, toolTurn.text, res.waitText, resultTurn.text, res.isKeyField, res.asksToCheck, res.repeat, res.isSilent)
		return res
	}

	for {
		select {
		case <-hardStop:
			t.Logf("%s no end within 90 s", label)
			return finish()

		case <-waitTimer:
			waitTimer = nil
			// sayWaitLine: only when the caller would hear nothing else.
			quiet := toolTurn.audioBytes == 0 ||
				!time.Now().Before(toolTurn.firstAudio.Add(audioDuration(d.output, toolTurn.audioBytes)))
			if !quiet || isResultSent {
				t.Logf("%s wait line not said (quiet=%v, result sent=%v)", label, quiet, isResultSent)
				break
			}
			if err := c.session.SpeakText(fx.WaitLine, false); err != nil {
				t.Fatalf("speak: %v", err)
			}
			res.isWaitLine = true
			t.Logf("%s wait line said: %q", label, fx.WaitLine)

		case <-resultTimer:
			resultTimer = nil
			sendResult()

		case <-resultStartBy:
			resultStartBy = nil
			t.Logf("%s no result turn within 12 s", label)
			return finish()

		case <-resultDone:
			return finish()

		case <-endTimer:
			endTimer = nil
			// The turn ended without the lookup being called.
			if !isYesPlayed && repeatReadBack.MatchString(other.text) {
				isYesPlayed = true
				res.readBack = other.text
				t.Logf("%s read the number back (%q); confirming", label, other.text)
				other = repeatTurn{}
				cur = &other
				d.settle()
				c.say(d.yes)
				break
			}
			if res.readBack == "" {
				res.readBack = other.text
			}
			t.Logf("%s the model did not call %s (said %q)", label, fx.Tool, other.text)
			return finish()

		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeResponseStarted:
				switch {
				case ev.IsSpokenLine:
					cur = &waitTurn
				case isResultSent && !resultTurn.started:
					cur = &resultTurn
				case isResultSent:
					cur = &repeatTurn{}
					res.extraTurns++
				case res.isCalled:
					cur = &toolTurn
				default:
					cur = &other
					endTimer = nil
				}
				if !cur.started {
					cur.started = true
				}
				if cur == &resultTurn {
					resultStartBy = nil
				}
			case EventTypeAudioDelta:
				d.onAudio(len(ev.Audio))
				if cur.audioBytes == 0 {
					cur.firstAudio = time.Now()
				}
				cur.audioBytes += len(ev.Audio)
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					cur.text += ev.Text
				}
			case EventTypeToolCall:
				if ev.ToolName != fx.Tool || res.isCalled {
					t.Logf("%s TOOL_CALL %s %s (turn %s)", label, ev.ToolName, ev.ToolArgs, d.which(cur, &toolTurn, &waitTurn, &resultTurn))
					if cur == &resultTurn {
						resultTurn.tools = append(resultTurn.tools, ev.ToolName+" "+ev.ToolArgs)
					}
					if ev.ToolName != fx.Tool {
						if err := c.session.SendToolResult(ev.ToolCallID, `{"ok":"0"}`); err != nil {
							t.Fatalf("send tool result: %v", err)
						}
					}
					break
				}
				var args struct {
					RMA string `json:"rma"`
				}
				_ = json.Unmarshal([]byte(ev.ToolArgs), &args)
				res.isCalled, res.args = true, ev.ToolArgs
				res.isArgsWrong = !strings.EqualFold(strings.TrimSpace(args.RMA), want.RMA)
				callID = ev.ToolCallID
				// The turn that made the call is the one the words so far
				// belong to.
				toolTurn = *cur
				cur = &toolTurn
				endTimer = nil
				res.isCallOpen = !toolTurn.ended
				t.Logf("%s TOOL_CALL %s %s (wrong args: %v; response still open: %v; said so far %q)",
					label, ev.ToolName, ev.ToolArgs, res.isArgsWrong, res.isCallOpen, toolTurn.text)
				resultTimer = time.After(latency)
				if waitAfter > 0 {
					waitTimer = time.After(waitAfter)
				}
			case EventTypeSpeechStarted:
				if d.isBargeArmed {
					d.isBargeArmed = false
					res.isBarged, res.isBargeMissed = d.barge(label)
				}
			case EventTypeResponseDone, EventTypeInterrupted:
				if ev.Type == EventTypeInterrupted {
					t.Logf("%s INTERRUPTED event (turn %s)", label, d.which(cur, &toolTurn, &waitTurn, &resultTurn))
				}
				cur.ended = true
				switch {
				case cur == &resultTurn && resultTurn.started:
					// A moment longer for the final transcript.
					resultDone = time.After(1500 * time.Millisecond)
				case !res.isCalled && cur == &other:
					endTimer = time.After(1500 * time.Millisecond)
				}
			case EventTypeError:
				t.Logf("%s provider error (fatal=%v): %v", label, ev.IsFatal, ev.Err)
				if ev.IsFatal {
					t.Fatalf("%s fatal provider error", label)
				}
			}
		}
	}
}

// which names a turn for the log.
func (d *repeatDriver) which(cur, tool, wait, result *repeatTurn) string {
	switch cur {
	case tool:
		return "tool-call"
	case wait:
		return "wait-line"
	case result:
		return "result"
	}
	return "other"
}

// bargeLead is how long before the previous answer's playback end the caller's
// speech starts; bargeTone is the room tone midCall.say puts ahead of a clip.
const (
	bargeLead = time.Second
	bargeTone = 200 * time.Millisecond
)

// sayClip queues the caller's clip for lookup i. Lookup 1, and every lookup
// with barge-in off, waits for the bot to be heard out plus about 1 s. With
// barge-in, lookups 2 and 3 wait only until the clip's speech would start
// bargeLead before the previous result turn's playback end (wall clock from
// its first audio): the clip is queued bargeTone earlier than that, because
// say puts that much room tone ahead of the speech. A previous turn shorter
// than 2 s is barged at its midpoint. A moment already past is not waited for.
func (d *repeatDriver) sayClip(label string, i int, clip []byte) {
	if !d.isBarge || i == 0 || d.prevBytes == 0 {
		d.settle()
		d.c.say(clip)
		return
	}
	dur := audioDuration(d.output, d.prevBytes)
	speechAt := d.prevFirst.Add(dur - bargeLead)
	if dur < 2*time.Second {
		speechAt = d.prevFirst.Add(dur / 2)
		d.t.Logf("%s previous result turn is %v long; barging at its midpoint", label, dur.Round(time.Millisecond))
	}
	sayAt := speechAt.Add(-bargeTone)
	if wait := time.Until(sayAt); wait > 0 {
		d.drainUntil(sayAt, false)
	}
	d.t.Logf("%s clip queued %v before the previous result turn's playback end (turn is %v long, %v in)",
		label, time.Until(d.prevFirst.Add(dur)).Round(time.Millisecond), dur.Round(time.Millisecond),
		time.Since(d.prevFirst).Round(time.Millisecond))
	d.isBargeArmed = true
	d.c.say(clip)
}

// barge is the caller's SPEECH_STARTED over the previous result turn: while it
// is still playing, interrupt it as aicall does, with what the caller heard.
// It reports whether it interrupted, and whether the speech came too late.
func (d *repeatDriver) barge(label string) (isBarged, isMissed bool) {
	dur := audioDuration(d.output, d.prevBytes)
	played := time.Since(d.prevFirst)
	if played >= dur {
		d.t.Logf("%s bargeMissed: speech started %v after the playback end", label, (played - dur).Round(time.Millisecond))
		return false, true
	}
	playedMs := min(max(int(played.Milliseconds()), 1), int(dur.Milliseconds()))
	if err := d.c.session.Interrupt(InterruptReasonSpeech, playedMs); err != nil {
		d.t.Logf("%s interrupt failed: %v", label, err)
	}
	d.t.Logf("%s barged playedMs=%d ofMs=%d", label, playedMs, dur.Milliseconds())
	return true, false
}

// readRepeatClip reads a caller clip from testdata, or from the directory
// AICC_LIVE_REPEAT_CLIPS names: the same four file names, recorded on a real
// call, which stay on the machine that measures them.
func readRepeatClip(t *testing.T, format media.AudioFormat, name string) []byte {
	t.Helper()
	dir := os.Getenv("AICC_LIVE_REPEAT_CLIPS")
	if dir == "" {
		return readMidClip(t, format, name)
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".pcm"))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	enc := midEncoder(format)
	out := enc(data)
	frame := len(enc(make([]byte, 640)))
	if rest := len(out) % frame; rest != 0 {
		out = append(out, enc(make([]byte, 640))[:frame-rest]...)
	}
	return out
}

// repeatFastLatency is how long a fast lookup takes before its result is
// sent: 3 ms, what the mock backend on the dev box took on the owner's calls
// of 2026-11-03, while the tool-call turn's filler was still being made. At
// 300 ms the defect did not show in 20 sessions. AICC_LIVE_REPEAT_LATENCY_MS
// overrides it.
func repeatFastLatency() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_REPEAT_LATENCY_MS")); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 3 * time.Millisecond
}
