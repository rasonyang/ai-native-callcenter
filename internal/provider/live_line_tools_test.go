// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures, against the real providers, that a line asked for with
// SpeakText while a tool call is still unanswered is said, and is not answered
// with a second call to the same tool. It costs money, so it is skipped unless
// AICC_LIVE_PROVIDER_TEST and the profile's key are set.
//
// The live round of #286 (2026-10-22, qwen, DID 95012) found the wait line's
// response.create answered with a second, identical function call: the
// backend was hit twice and the two results made two answer turns. A line's
// request carries a response-level tool_choice "none" where the profile obeys
// one (Profile.AcceptsResponseToolChoice), on qwen its direction bans a tool
// call in words (Profile.NeedsToolFreeLineDirection), and whatever the
// profile, a call made in a line's turn is never handed out and is taken back
// out of the conversation when the turn ends (Realtime.onLineCall).
//
// One session per run: the caller asks for an order's status (testdata
// q_order, the clip the mid-call measurement uses), the model calls
// lookup_order, and with that call unanswered the test asks for the wait line.
// The line's turn must say the line and hand out no tool call. Then the tool's
// result is sent, and exactly one answer turn must follow. What the provider
// did on the wire (a call made in the line's turn, its delete and the
// provider's answer to it) is read from the client's own log and printed.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenLineCallsNoTool -v ./internal/provider/
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveGrokLineCallsNoTool -v ./internal/provider/
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveOpenAILineCallsNoTool -v ./internal/provider/
const liveWaitLine = "请稍等，我查一下。"

func TestLiveQwenLineCallsNoTool(t *testing.T) { runLineCallsNoTool(t, liveProfile(QwenProfile())) }

func TestLiveGrokLineCallsNoTool(t *testing.T) { runLineCallsNoTool(t, liveProfile(GrokProfile())) }

func TestLiveOpenAILineCallsNoTool(t *testing.T) { runLineCallsNoTool(t, liveProfile(OpenAIProfile())) }

func runLineCallsNoTool(t *testing.T, profile Profile) {
	requireLive(t, profile)
	input, _ := profile.FormatsFor(media.LawMu)
	order := readMidClip(t, input, "q_order")

	tap := &liveLogTap{}
	c := openMidCallLogged(t, profile, true, slog.New(tap))
	defer c.close(t)
	defer tap.print(t)
	opening := c.expectReply(t, "opening")
	t.Logf("opening: %q", opening.text)

	c.say(order)
	asked := c.expectReply(t, "the order question")
	t.Logf("reply to the question: %q, %d tool call(s)", asked.text, len(asked.tools))
	if len(asked.tools) == 0 {
		t.Fatalf("the model did not call %s, so there is no unanswered call to test against", midTool)
	}
	for _, call := range asked.tools {
		t.Logf("TOOL_CALL %s %s (%s)", call.ToolName, call.ToolArgs, call.ToolCallID)
	}

	// The call is unanswered, as a slow backend leaves it at the 1 s mark.
	sentAt := time.Now()
	if err := c.session.SpeakText(liveWaitLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	line := collectTurn(t, c, "the wait line")
	t.Logf("line turn: started=%v isSpokenLine=%v firstAudioAfter=%v audioBytes=%d text=%q tools=%d",
		line.started, line.isSpokenLine, line.firstAudio.Sub(sentAt).Round(time.Millisecond),
		line.audioBytes, line.text, len(line.tools))
	for _, call := range line.tools {
		t.Errorf("the line's turn called %s %s: a line must not call a tool", call.ToolName, call.ToolArgs)
	}
	if line.audioBytes == 0 {
		t.Errorf("the line's turn carried no audio")
	}
	if !line.isSpokenLine {
		t.Errorf("the line's turn was not reported as a spoken line")
	}
	if !strings.Contains(midNormalize(line.text), midNormalize(liveWaitLine)) {
		t.Errorf("the line's turn did not say the line: %q", line.text)
	}

	for _, call := range asked.tools {
		if err := c.session.SendToolResult(call.ToolCallID, `{"status":"已发货，预计明天送达"}`); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
	}
	answer := collectTurn(t, c, "the answer")
	t.Logf("answer turn: audioBytes=%d text=%q tools=%d", answer.audioBytes, answer.text, len(answer.tools))
	if answer.audioBytes == 0 || answer.text == "" {
		t.Errorf("the tool result was not answered")
	}
	for _, call := range answer.tools {
		t.Errorf("the answer called %s %s again", call.ToolName, call.ToolArgs)
	}

	// Exactly one answer: nothing else is started while the caller is silent.
	if extra := c.countTurnsFor(t, 6*time.Second); extra != 0 {
		t.Errorf("%d more turn(s) started after the answer", extra)
	}
	t.Logf("calls the provider made in the line's turn and the client kept back: %d",
		tap.count("the turn asked for a line called a tool"))
}

// liveLogTap keeps what the client logs about function calls and items, and
// passes warnings and worse to stderr as the other live tests do.
type liveLogTap struct {
	mu    sync.Mutex
	lines []string
}

func (h *liveLogTap) Enabled(context.Context, slog.Level) bool { return true }

func (h *liveLogTap) Handle(_ context.Context, record slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", record.Time.Format("15:04:05.000"), record.Level, record.Message)
	record.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	line := b.String()
	if record.Level >= slog.LevelWarn {
		fmt.Fprintln(os.Stderr, line)
	}
	h.mu.Lock()
	h.lines = append(h.lines, line)
	h.mu.Unlock()
	return nil
}

func (h *liveLogTap) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *liveLogTap) WithGroup(string) slog.Handler { return h }

// count is how many lines carry message.
func (h *liveLogTap) count(message string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, line := range h.lines {
		if strings.Contains(line, message) {
			n++
		}
	}
	return n
}

// print logs the lines about calls, items and errors.
func (h *liveLogTap) print(t *testing.T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, line := range h.lines {
		for _, words := range []string{"called a tool", "function call", "removed an item", "error", "refus"} {
			if strings.Contains(line, words) {
				t.Logf("client log: %s", line)
				break
			}
		}
	}
}

// liveTurn is what one model turn carried.
type liveTurn struct {
	started      bool
	isSpokenLine bool
	firstAudio   time.Time
	audioBytes   int
	text         string
	tools        []Event
}

// collectTurn reads one turn: from its start to its end, then a moment longer
// for the final transcript, which can trail the end of the audio.
func collectTurn(t *testing.T, c *midCall, what string) liveTurn {
	t.Helper()
	var turn liveTurn
	deadline := time.After(40 * time.Second)
	var grace <-chan time.Time
	for {
		select {
		case <-deadline:
			t.Fatalf("%s: no complete turn within 40 s (text so far %q)", what, turn.text)
		case <-grace:
			return turn
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatalf("%s: the session ended", what)
			}
			switch ev.Type {
			case EventTypeResponseStarted:
				turn.started = true
				turn.isSpokenLine = turn.isSpokenLine || ev.IsSpokenLine
			case EventTypeAudioDelta:
				if turn.audioBytes == 0 {
					turn.firstAudio = time.Now()
				}
				turn.audioBytes += len(ev.Audio)
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					turn.text += ev.Text
				}
			case EventTypeToolCall:
				turn.tools = append(turn.tools, ev)
			case EventTypeResponseDone:
				if grace == nil {
					grace = time.After(1500 * time.Millisecond)
				}
			case EventTypeError:
				t.Logf("%s: provider error (fatal=%v): %v", what, ev.IsFatal, ev.Err)
				if ev.IsFatal {
					t.Fatalf("%s: fatal provider error", what)
				}
			}
		}
	}
}

// countTurnsFor counts the turns started within d.
func (c *midCall) countTurnsFor(t *testing.T, d time.Duration) int {
	t.Helper()
	n := 0
	until := time.After(d)
	for {
		select {
		case <-until:
			return n
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeResponseStarted:
				n++
				t.Logf("a turn started (isSpokenLine=%v)", ev.IsSpokenLine)
			case EventTypeToolCall:
				t.Logf("TOOL_CALL %s %s", ev.ToolName, ev.ToolArgs)
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					t.Logf("said: %q", ev.Text)
				}
			}
		}
	}
}

// The wait line on the session that failed live. TestLive*LineCallsNoTool
// uses a two-sentence persona, and on it qwen said the line every time; on the
// mobile_support flow of the #286 live round it often said nothing at all.
// testdata/line_session_zh.json is that flow's session as the orchestrator
// built it (Runtime.Instructions, Tools and Announce of the published revision
// on the dev database, 2026-10-22), and testdata/q_contract.pcm is the
// question of live call 01a1622a-950a-766a-af17-47812eae7178, "我的合约机想提前
// 解约，要交多少违约金？" (qwen3-tts-flash, voice Cherry, 16 kHz mono PCM16).
//
// The timing is the call's: the wait line is asked for 1 s after the model's
// call to kb_search_slow, and the result is sent 3 s after the call.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run 'LiveQwenWaitLine/<arm>' -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 3) is how many sessions an arm opens.
type waitLineArm struct {
	name    string
	profile Profile
	// isShipped makes the arm's counts assertions rather than a measurement.
	isShipped bool
}

// Arms: "plain" is the line as it was asked for before W-Q8 (SayExactly),
// "shipped" is QwenProfile as it ships (sayExactlyWithoutTools).
func TestLiveQwenWaitLine(t *testing.T) {
	plain := liveProfile(QwenProfile())
	plain.NeedsToolFreeLineDirection = false
	arms := []waitLineArm{
		{name: "plain", profile: plain},
		{name: "shipped", profile: liveProfile(QwenProfile()), isShipped: true},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) { runWaitLineArm(t, arm) })
	}
}

type waitLineRun struct {
	isLineSaid     bool
	lineAudioAfter time.Duration // from the model's call to the line's first audio
	keptBack       int           // calls the provider made in the line's turn
	answers        int           // turns after the result that were not the line
	lineText       string
	answerText     string
}

func runWaitLineArm(t *testing.T, arm waitLineArm) {
	profile := arm.profile
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

	said, kept, oneAnswer := 0, 0, 0
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
		r := c.waitLineRound(t, question)
		r.keptBack = tap.count("the turn asked for a line called a tool")
		tap.print(t)
		c.close(t)
		t.Logf("[%s #%d] line said=%v after=%v text=%q keptBack=%d answers=%d answer=%q",
			arm.name, run, r.isLineSaid, r.lineAudioAfter.Round(time.Millisecond), r.lineText,
			r.keptBack, r.answers, r.answerText)
		if r.isLineSaid {
			said++
		}
		if r.keptBack > 0 {
			kept++
		}
		if r.answers == 1 {
			oneAnswer++
		}
	}
	t.Logf("[%s/%s] RESULT line said %d/%d, call made in the line's turn %d/%d, exactly one answer %d/%d",
		profile.Name, arm.name, said, runs, kept, runs, oneAnswer, runs)
	if arm.isShipped && (said != runs || oneAnswer != runs) {
		t.Errorf("%s: line said %d/%d, exactly one answer %d/%d", arm.name, said, runs, oneAnswer, runs)
	}
}

// openLiveCall is openMidCall with the whole session configuration chosen by
// the test.
func openLiveCall(t *testing.T, profile Profile, log *slog.Logger, cfg SessionConfig) *midCall {
	t.Helper()
	session, err := New(profile, log)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := session.Start(ctx, cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	enc := midEncoder(cfg.InputFormat)
	c := &midCall{session: session, stop: make(chan struct{}), done: make(chan struct{}),
		encode: enc, frame: len(enc(make([]byte, 640)))}
	go c.feed()
	return c
}

// waitLineRound plays the greeting out, asks the question, and runs the slow
// tool on the call's clock: the wait line 1 s after the model's call, the
// result 3 s after it. It reads until 5 s after the last turn ended.
func (c *midCall) waitLineRound(t *testing.T, question []byte) waitLineRun {
	t.Helper()
	opening := c.expectReply(t, "opening")
	t.Logf("opening: %q", opening.text)
	c.say(question)

	var r waitLineRun
	var call Event
	var calledAt time.Time
	var speakAt, resultAt, quietAt <-chan time.Time
	isResultSent := false
	turn := "" // "line", "answer", "other"
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("no end within 60 s (line %q, answer %q)", r.lineText, r.answerText)
		case <-quietAt:
			return r
		case <-speakAt:
			speakAt = nil
			if err := c.session.SpeakText(liveWaitLine, false); err != nil {
				t.Fatalf("speak: %v", err)
			}
		case <-resultAt:
			resultAt = nil
			isResultSent = true
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
					t.Logf("TOOL_CALL %s %s", ev.ToolName, ev.ToolArgs)
					speakAt = time.After(time.Second)
					resultAt = time.After(3 * time.Second)
				} else {
					t.Logf("another TOOL_CALL handed out: %s %s", ev.ToolName, ev.ToolArgs)
				}
			case EventTypeResponseStarted:
				quietAt = nil
				switch {
				case ev.IsSpokenLine:
					turn = "line"
				case isResultSent:
					turn = "answer"
					r.answers++
				default:
					turn = "other"
				}
				t.Logf("turn started: %s", turn)
			case EventTypeAudioDelta:
				if turn == "line" && r.lineAudioAfter == 0 {
					r.lineAudioAfter = time.Since(calledAt)
				}
			case EventTypeOutputTranscript:
				if !ev.IsFinal {
					break
				}
				switch turn {
				case "line":
					r.lineText += ev.Text
				case "answer":
					r.answerText += ev.Text
				default:
					t.Logf("said (%s): %q", turn, ev.Text)
				}
			case EventTypeResponseDone, EventTypeInterrupted:
				t.Logf("turn ended: %s (%s)", turn, ev.Type)
				if isResultSent {
					quietAt = time.After(5 * time.Second)
				}
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
			}
		}
		r.isLineSaid = r.lineAudioAfter > 0 &&
			strings.Contains(midNormalize(r.lineText), midNormalize(liveWaitLine))
	}
}

// A closing line on the same session, after the answer: the line a dead-air
// or session-limit ending asks for with SpeakText(isClosing). On qwen it goes
// into the conversation as well (NeedsDirectedLineInConversation), so it is
// the line whose direction the caller-side history carries. It must still be
// said as written and call no tool.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenClosingLine -v ./internal/provider/
func TestLiveQwenClosingLine(t *testing.T) {
	profile := liveProfile(QwenProfile())
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	const closing = "感谢来电,再见。"
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

	said := 0
	for run := 1; run <= runs; run++ {
		tap := &liveLogTap{}
		c := openLiveCall(t, profile, slog.New(tap), SessionConfig{
			Instructions: fixture.Instructions, OpeningText: fixture.Announce, Language: "zh",
			Turn: DefaultTurnDetection(), InputFormat: input, OutputFormat: output, Tools: fixture.Tools,
		})
		c.expectReply(t, "opening")
		c.say(question)
		asked := c.expectReply(t, "the question")
		for _, call := range asked.tools {
			if err := c.session.SendToolResult(call.ToolCallID,
				`{"answer":"合约期内变更套餐需支付200元违约金，由人工客服办理。","found":"1","ok":"1"}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
		}
		if len(asked.tools) > 0 {
			c.expectReply(t, "the answer")
		}
		if err := c.session.SpeakText(closing, true); err != nil {
			t.Fatalf("speak: %v", err)
		}
		line := collectTurn(t, c, "the closing line")
		keptBack := tap.count("the turn asked for a line called a tool")
		c.close(t)
		ok := line.audioBytes > 0 && strings.Contains(midNormalize(line.text), midNormalize(closing)) &&
			len(line.tools) == 0 && keptBack == 0
		t.Logf("[closing #%d] text=%q audioBytes=%d keptBack=%d ok=%v", run, line.text, line.audioBytes, keptBack, ok)
		if ok {
			said++
		}
	}
	t.Logf("[qwen/closing] RESULT closing line said as written with no call %d/%d", said, runs)
	if said != runs {
		t.Errorf("closing line said as written %d/%d", said, runs)
	}
}
