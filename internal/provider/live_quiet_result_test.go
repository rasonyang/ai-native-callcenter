// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// The probe behind the caller-next rule (a phase that says the caller speaks
// next, Node.IsCallerNext): a slot tool the model calls while it answers the
// caller has its result sent with SendToolResultQuietly and no turn is asked
// for. The first measurement (issue #387) stopped once the tool result was
// answered; this one goes on to the caller's NEXT turn, and to what the model
// does on its own in between.
//
// A session, on the real client and the real profile (default qwen, at its
// current default model; AICC_LIVE_QUIET_PROFILE=openai or grok picks another):
//
//  1. The call opens with the renewal-offer's opening line and its standing
//     instructions (testdata/renewal_offer_session.json, a copy of what
//     flow.Runtime renders for internal/flowsim/testdata/flows/renewal_offer.json:
//     the persona, the rules and the fact list, the offer phase, the tools).
//  2. The caller asks a question from the fact list, as speech the server's
//     turn detection ends. Some are an acknowledgement followed by a question
//     ("OK, so how much data do I get?").
//  3. The model answers and calls the slot tool. At the end of that response
//     the client does what the orchestrator does: re-pins the standing
//     instructions to the phase the call moved to, then sends the result with
//     SendToolResultQuietly (the response spoke), or with SendToolResult when
//     it did not (a response that only called the tool gets its turn as ever).
//  4. For quietWindow nothing is sent. Any response that starts with no
//     caller speech since is recorded: that is the follow-up turn this rule
//     removes.
//  5. The caller's next turn rotates through a second question, an agreement,
//     a refusal and silence. What the provider does with it (a reply, which
//     tool, what it says, how long) is recorded.
//
// One JSON line per session (PROBE ...) and a summary (PROBE-SUMMARY ...) are
// logged. They are measurements: the test fails only on a protocol error (a
// fatal error event) or a hang, never on a rate.
//
// The caller's speech is recorded clips (16 kHz mono PCM16 little-endian,
// header stripped), streamed in real time between stretches of room tone, as
// the other live tests do; the test calls no TTS. The agreement and the refusal
// are the existing q_agree_* and q_stop_* clips. The questions are
// testdata/renewal_<lang>_<id>.pcm, which are not in the repository: generate
// them once, from the texts in renewalProbeQuestions, with qwen3-tts-flash
// (voice Cherry, language_type English or Chinese), converted to raw 16 kHz mono
// PCM16 with the silence trimmed. The test skips, naming the missing files,
// until they exist.
//
// Twelve sessions by default, six English and six Chinese, one after the
// other; AICC_LIVE_SESSIONS changes the number (even numbers keep the two
// languages equal). Real money, so run it only on purpose:
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run TestLiveQuietToolResult -v -timeout 30m ./internal/provider/
//
// With ALIYUN_API_KEY set for qwen (OPENAI_API_KEY or XAI_API_KEY with the
// profile variable above).
func TestLiveQuietToolResult(t *testing.T) {
	profile := liveProfile(liveQuietProfile(t))
	requireLive(t, profile)
	sessions := 12
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_SESSIONS")); err == nil && v > 0 {
		sessions = v
	}
	var fixture map[string]struct {
		Announce          string
		Instructions      string
		InstructionsAfter string
		Tools             []ToolSpec
		MovedTo           string
		ResultOutput      string
	}
	data, err := os.ReadFile("testdata/renewal_offer_session.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	input, output := profile.FormatsFor(media.LawMu)

	var missing []string
	clips := map[string][]byte{}
	load := func(name string) {
		raw, err := os.ReadFile("testdata/" + name + ".pcm")
		if err != nil {
			missing = append(missing, name+".pcm")
			return
		}
		clips[name] = midClip(input, raw)
	}
	for lang, questions := range renewalProbeQuestions {
		for _, q := range questions {
			load("renewal_" + lang + "_" + q.id)
		}
		load("q_agree_" + lang)
		load("q_stop_" + lang)
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		t.Skipf("caller clips are missing from internal/provider/testdata: %s (see the test's comment)",
			strings.Join(missing, ", "))
	}

	nextKinds := []string{"second question", "agreement", "refusal", "silence"}
	var results []quietProbeResult
	for i := range sessions {
		lang := []string{"en", "zh"}[i%2]
		n := i / 2
		questions := renewalProbeQuestions[lang]
		first := questions[n%len(questions)]
		second := questions[(n+3)%len(questions)]
		kind := nextKinds[n%len(nextKinds)]
		fx := fixture[lang]

		tap := &liveLogTap{}
		c := openLiveCall(t, profile, slog.New(tap), SessionConfig{
			Instructions: fx.Instructions,
			OpeningText:  fx.Announce,
			Language:     lang,
			Turn:         DefaultTurnDetection(),
			InputFormat:  input,
			OutputFormat: output,
			Tools:        fx.Tools,
		})
		r := quietProbeResult{Session: i + 1, Lang: lang, Question: first.id, Form: first.form, NextKind: kind}

		c.expectReply(t, "the opening")

		// 1. The question.
		p1 := &probeTurn{}
		c.say(clips["renewal_"+lang+"_"+first.id])
		c.watch(t, p1, 30*time.Second, func() bool { return p1.isSettled(1500 * time.Millisecond) })
		if p1.started == 0 {
			t.Fatalf("session %d: no response within 30 s of the question (%v)", i+1, p1.errs)
		}
		r.FirstAudioMs = p1.firstAudioMs()
		r.Spoke = !p1.firstAudioAt.IsZero()
		r.SlotCalled = slices.ContainsFunc(p1.tools, func(e Event) bool { return e.ToolName == "record_question" })
		r.AudioBeforeCall = r.SlotCalled && p1.isAudioBeforeTool
		r.Answer = p1.text
		r.OtherCalls = toolNamesExcept(p1.tools, "record_question")

		if !r.SlotCalled {
			// No call to answer: nothing of the rule to measure. The session
			// still counts, as a session where the model did not call it.
			c.close(t)
			results = append(results, r)
			logQuietProbe(t, r, tap)
			continue
		}

		// 2. The result, as the orchestrator sends it: instructions re-pinned
		// to the phase the call moved to, then the answer.
		call := p1.tools[slices.IndexFunc(p1.tools, func(e Event) bool { return e.ToolName == "record_question" })]
		if err := c.session.UpdateInstructions(fx.InstructionsAfter); err != nil {
			t.Fatalf("update instructions: %v", err)
		}
		r.Quiet = r.Spoke
		var sendErr error
		if r.Quiet {
			sendErr = c.session.SendToolResultQuietly(call.ToolCallID, fx.ResultOutput)
		} else {
			sendErr = c.session.SendToolResult(call.ToolCallID, fx.ResultOutput)
		}
		if sendErr != nil {
			t.Fatalf("send the result: %v", sendErr)
		}

		// 3. Nothing is sent. What starts on its own?
		p2 := &probeTurn{}
		c.watch(t, p2, quietWindow, func() bool { return false })
		r.OwnResponses = p2.started
		if !r.Quiet {
			r.FollowUpText = p2.text
		}
		r.Unprompted = p2.startedWithoutCaller()
		r.UnpromptedText = p2.text
		r.UnpromptedAudio = !p2.firstAudioAt.IsZero()

		// 4. The caller's next turn.
		p3 := &probeTurn{}
		switch kind {
		case "second question":
			c.say(clips["renewal_"+lang+"_"+second.id])
			r.NextQuestion = second.id
		case "agreement":
			c.say(clips["q_agree_"+lang])
		case "refusal":
			c.say(clips["q_stop_"+lang])
		}
		if kind == "silence" {
			c.watch(t, p3, silenceWindow, func() bool { return false })
		} else {
			c.watch(t, p3, 30*time.Second, func() bool {
				return p3.isSettled(1500*time.Millisecond) || (p3.speechStarts > 0 && p3.started == 0 &&
					time.Since(p3.lastSpeechAt) > 12*time.Second)
			})
		}
		r.NextReplied = p3.started > 0
		r.NextTools = toolNames(p3.tools)
		r.NextText = p3.text
		r.NextFirstAudioMs = p3.firstAudioMs()
		r.Errors = append(p1.errs, append(p2.errs, p3.errs...)...)

		c.close(t)
		results = append(results, r)
		logQuietProbe(t, r, tap)
	}

	summariseQuietProbe(t, profile, results)
}

const (
	// quietWindow is how long nothing is sent after the result, to see
	// whether the model takes a turn of its own.
	quietWindow = 4 * time.Second
	// silenceWindow is how long a silent caller is waited on.
	silenceWindow = 8 * time.Second
)

type renewalQuestion struct {
	id, form, text string
}

// renewalProbeQuestions are the caller's questions, each answered by an entry
// of the fact list; the clips are generated from these texts. "ack" is an
// acknowledgement followed by a question.
var renewalProbeQuestions = map[string][]renewalQuestion{
	"en": {
		{"price", "question", "Does the price change if I renew?"},
		{"data", "ack", "OK, so how much data do I get?"},
		{"throttle", "question", "What happens when I use up the data?"},
		{"fiveg", "question", "Is 5G included?"},
		{"number", "ack", "Right, can I keep my phone number?"},
		{"cancel", "question", "Can I cancel if I change my mind?"},
	},
	"zh": {
		{"price", "question", "续约之后月费会变吗？"},
		{"data", "ack", "哦，那每个月有多少流量？"},
		{"throttle", "question", "流量用完了会怎么样？"},
		{"fiveg", "question", "五G要另外收费吗？"},
		{"number", "ack", "好的，那我的手机号码能不能不变？"},
		{"cancel", "question", "我要是反悔了可以取消吗？"},
	},
}

type quietProbeResult struct {
	Session  int    `json:"session"`
	Lang     string `json:"lang"`
	Question string `json:"question"`
	Form     string `json:"form"`
	// FirstAudioMs is speech_stopped to the first audio of the answer.
	FirstAudioMs    int64    `json:"firstAudioMs"`
	Spoke           bool     `json:"spoke"`
	SlotCalled      bool     `json:"slotCalled"`
	AudioBeforeCall bool     `json:"audioBeforeCall"`
	OtherCalls      []string `json:"otherCalls,omitempty"`
	Answer          string   `json:"answer"`
	// Quiet says the result went out with SendToolResultQuietly.
	Quiet bool `json:"quiet"`
	// OwnResponses are the responses that started in the quiet window;
	// Unprompted says one started with no caller speech since the result.
	OwnResponses    int    `json:"ownResponses"`
	Unprompted      bool   `json:"unprompted"`
	UnpromptedAudio bool   `json:"unpromptedAudio"`
	UnpromptedText  string `json:"unpromptedText,omitempty"`
	FollowUpText    string `json:"followUpText,omitempty"`

	NextKind         string   `json:"nextKind"`
	NextQuestion     string   `json:"nextQuestion,omitempty"`
	NextReplied      bool     `json:"nextReplied"`
	NextTools        []string `json:"nextTools,omitempty"`
	NextText         string   `json:"nextText,omitempty"`
	NextFirstAudioMs int64    `json:"nextFirstAudioMs"`
	Errors           []string `json:"errors,omitempty"`
}

func logQuietProbe(t *testing.T, r quietProbeResult, tap *liveLogTap) {
	t.Helper()
	line, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PROBE %s", line)
	if n := tap.count("unheard"); n > 0 {
		t.Logf("PROBE session %d: the client logged %d lines about an unheard turn", r.Session, n)
	}
}

func summariseQuietProbe(t *testing.T, profile Profile, results []quietProbeResult) {
	t.Helper()
	var slot, spoke, before, quiet, unprompted, replied, asked int
	var firsts []int64
	byKind := map[string][2]int{}
	for _, r := range results {
		if r.SlotCalled {
			slot++
		}
		if r.Spoke {
			spoke++
		}
		if r.AudioBeforeCall {
			before++
		}
		if r.Quiet {
			quiet++
		}
		if r.Unprompted {
			unprompted++
		}
		if r.FirstAudioMs > 0 {
			firsts = append(firsts, r.FirstAudioMs)
		}
		if r.SlotCalled {
			asked++
			counts := byKind[r.NextKind]
			counts[1]++
			if r.NextReplied {
				replied++
				counts[0]++
			}
			byKind[r.NextKind] = counts
		}
	}
	slices.Sort(firsts)
	summary := map[string]any{
		"profile": profile.Name, "model": profile.Model, "sessions": len(results),
		"slotCalled": slot, "spoke": spoke, "audioBeforeToolCall": before, "quietResults": quiet,
		"unpromptedResponses": unprompted, "nextTurnReplied": replied, "nextTurnsMeasured": asked,
		"nextReplyByKind":    byKind,
		"firstAudioMsSorted": firsts,
	}
	line, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PROBE-SUMMARY %s", line)
	for _, r := range results {
		for _, e := range r.Errors {
			t.Logf("PROBE session %d provider error: %s", r.Session, e)
		}
	}
}

func toolNames(events []Event) []string {
	var names []string
	for _, e := range events {
		names = append(names, e.ToolName)
	}
	return names
}

func toolNamesExcept(events []Event, except string) []string {
	var names []string
	for _, e := range events {
		if e.ToolName != except {
			names = append(names, e.ToolName)
		}
	}
	return names
}

// liveQuietProfile is the profile the probe runs on.
func liveQuietProfile(t *testing.T) Profile {
	t.Helper()
	switch name := os.Getenv("AICC_LIVE_QUIET_PROFILE"); name {
	case "", "qwen":
		return QwenProfile()
	case "openai":
		return OpenAIProfile()
	case "grok":
		return GrokProfile()
	default:
		t.Fatalf("AICC_LIVE_QUIET_PROFILE=%q: want qwen, openai or grok", name)
		return Profile{}
	}
}

// midClip turns a raw 16 kHz mono PCM16 clip into the profile's input format,
// whole frames only (readMidClip, from bytes).
func midClip(format media.AudioFormat, raw []byte) []byte {
	enc := midEncoder(format)
	out := enc(raw)
	frame := len(enc(make([]byte, 640)))
	if rest := len(out) % frame; rest != 0 {
		out = append(out, enc(make([]byte, 640))[:frame-rest]...)
	}
	return out
}

// probeTurn is what happened on the call over one stretch of time.
type probeTurn struct {
	stoppedAt, firstAudioAt, doneAt, lastSpeechAt time.Time
	text                                          string
	tools                                         []Event
	isAudioBeforeTool, isSpeaking                 bool
	started, done, speechStarts                   int
	errs                                          []string
}

func (p *probeTurn) note(ev Event, now time.Time) {
	switch ev.Type {
	case EventTypeSpeechStarted:
		p.speechStarts++
		p.isSpeaking = true
		p.lastSpeechAt = now
	case EventTypeSpeechStopped:
		p.stoppedAt = now
		p.isSpeaking = false
		p.lastSpeechAt = now
	case EventTypeResponseStarted:
		p.started++
	case EventTypeAudioDelta:
		if p.firstAudioAt.IsZero() {
			p.firstAudioAt = now
		}
	case EventTypeOutputTranscript:
		if ev.IsFinal {
			p.text += ev.Text
		}
	case EventTypeToolCall:
		if len(p.tools) == 0 {
			p.isAudioBeforeTool = !p.firstAudioAt.IsZero()
		}
		p.tools = append(p.tools, ev)
	case EventTypeResponseDone, EventTypeInterrupted:
		p.done++
		p.doneAt = now
	case EventTypeError:
		p.errs = append(p.errs, fmt.Sprintf("fatal=%v: %v", ev.IsFatal, ev.Err))
	}
}

// isSettled says a response has started and ended, the caller is not speaking,
// and nothing has happened for quiet since. A clip is streamed in real time
// behind say, and a caller who pauses after "OK," gets a reply that their own
// next words cut: that response has ended, and the answer to the whole sentence
// is still to come. The first run's rule left the caller's speech out, which is
// the likely reason for its three sessions with a truncated transcript and no
// audio, all acknowledgement forms: the window could close mid-sentence
// (qwen-findings W-Q15).
func (p *probeTurn) isSettled(quiet time.Duration) bool {
	return p.started > 0 && p.done >= p.started && !p.isSpeaking &&
		time.Since(p.doneAt) >= quiet && time.Since(p.lastSpeechAt) >= quiet
}

// startedWithoutCaller says a response started and the caller had not spoken
// since the stretch began.
func (p *probeTurn) startedWithoutCaller() bool { return p.started > 0 && p.speechStarts == 0 }

func (p *probeTurn) firstAudioMs() int64 {
	if p.firstAudioAt.IsZero() || p.stoppedAt.IsZero() {
		return 0
	}
	return p.firstAudioAt.Sub(p.stoppedAt).Milliseconds()
}

// watch reads the call's events into p until isOver says so or maxWait has
// passed. A fatal provider error stops the test; a hang is maxWait on a
// stretch that should have ended.
func (c *midCall) watch(t *testing.T, p *probeTurn, maxWait time.Duration, isOver func() bool) {
	t.Helper()
	deadline := time.After(maxWait)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			return
		case <-tick.C:
			if isOver() {
				return
			}
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			p.note(ev, time.Now())
			if ev.Type == EventTypeError && ev.IsFatal {
				t.Fatalf("fatal provider error: %v", ev.Err)
			}
		}
	}
}
