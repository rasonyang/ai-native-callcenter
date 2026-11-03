// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures, against the real providers, whether a mid-call
// Realtime.UpdateInstructions (an instructions-only session.update) is OBEYED
// by the next replies. It costs money (OpenAI realtime especially), so it is
// skipped unless AICC_LIVE_PROVIDER_TEST and the profile's key are set. It
// mirrors the doubao measurement (W-D9 in docs/design/doubao-findings.md) so
// the numbers compare directly.
//
// A flow changes phase by appending a paragraph to the persona, so the update
// is the same persona plus a paragraph that makes the model begin every answer
// with a marker it would never say on its own. The client takes no spoken
// questions from a test, so the caller's questions are synthesized speech
// (testdata/q_*.pcm, 16 kHz mono PCM16) streamed at real-time pace in 20 ms
// frames between stretches of room tone, which is what lets the server's VAD
// end each turn. The qwen profile takes 16 kHz PCM16 as it is; the OpenAI
// profile is a G.711 passthrough, so the clips and the room tone are
// downsampled 2:1 (mean of each sample pair) and mu-law encoded at 8 kHz.
//
// The clips were generated on 2026-10-15 with Aliyun qwen TTS (model
// qwen3-tts-flash, voice Cherry, language_type Chinese), converted to raw
// 16 kHz mono PCM16 little-endian, header stripped. Texts: q_hours
// "你们的客服几点下班？", q_broadband "请问宽带怎么办理？", q_price
// "那资费大概是多少钱？", q_order "我想查一下我的订单状态，订单号是一二三四五。".
// The test calls no TTS.
//
// Arms: control (no update; the marker must never appear), idle (update while
// the model is silent, then two more questions: immediacy and persistence),
// speaking (update sent while the first reply is still streaming, then two
// more questions), tools-control (no update; does the order question call the
// tool on its own) and tools (tool lookup_order declared at Start; the update
// adds the marker rule and "call lookup_order on an order question"; the reply
// after the tool result must carry the marker, so a tool lost to an
// instructions-only update shows as no call).
//
// The assertions encode what was measured on 2026-10-15 for both profiles (W-Q7
// in docs/design/qwen-findings.md; docs/provider-extension.md for openai); every
// transcript is logged.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenMidCall -v ./internal/provider/
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveOpenAIMidCall -v ./internal/provider/
//
// AICC_LIVE_RUNS (default 3) is how many sessions each arm opens.
const (
	midPersona   = "你是 NovaNet 的电话客服。请用中文，简短自然地回答，每次不超过两句话。"
	midPhase     = "\n\n从现在开始，你的每一句回答都必须以“好的，王先生”开头，然后再回答问题。"
	midMarker    = "好的王先生"
	midTool      = "lookup_order"
	midToolPhase = "\n\n当来电者询问订单状态时，必须先调用 lookup_order 工具查询，不要直接回答。"
)

func TestLiveQwenMidCallInstructions(t *testing.T) { runMidCall(t, liveProfile(QwenProfile())) }

func TestLiveOpenAIMidCallInstructions(t *testing.T) { runMidCall(t, liveProfile(OpenAIProfile())) }

func runMidCall(t *testing.T, profile Profile) {
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	input, _ := profile.FormatsFor(media.LawMu)
	hours, broadband, price, order := readMidClip(t, input, "q_hours"), readMidClip(t, input, "q_broadband"),
		readMidClip(t, input, "q_price"), readMidClip(t, input, "q_order")

	type arm struct {
		name    string
		tools   bool
		phase   string
		speak   bool // update while the first reply is being spoken
		updates bool
		asks    [][]byte
		// checked is the index of the first ask whose reply must carry the marker.
		checked int
	}
	arms := []arm{
		{name: "control", asks: [][]byte{hours, broadband, price}, checked: -1},
		{name: "idle", updates: true, phase: midPhase, asks: [][]byte{hours, broadband, price}, checked: 1},
		{name: "speaking", updates: true, speak: true, phase: midPhase, asks: [][]byte{hours, broadband, price}, checked: 1},
		{name: "tools-control", tools: true, asks: [][]byte{hours, order}, checked: -1},
		{name: "tools", tools: true, updates: true, phase: midPhase + midToolPhase, asks: [][]byte{hours, order}, checked: -1},
	}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			obeyed, total, toolCalls := 0, 0, 0
			for run := 1; run <= runs; run++ {
				c := openMidCall(t, profile, a.tools)
				opening := c.expectReply(t, "opening")
				t.Logf("[%s #%d] opening: %q", a.name, run, opening.text)

				for i, ask := range a.asks {
					if a.updates && !a.speak && i == 1 {
						c.update(t, midPersona+a.phase)
					}
					c.say(ask)
					if a.updates && a.speak && i == 0 {
						c.waitSpeaking(t)
						c.update(t, midPersona+a.phase)
					}
					reply := c.expectReply(t, fmt.Sprintf("ask %d", i+1))
					afterText := ""
					for _, call := range reply.tools {
						toolCalls++
						t.Logf("[%s #%d] ask %d TOOL_CALL %s %s", a.name, run, i+1, call.ToolName, call.ToolArgs)
						if err := c.session.SendToolResult(call.ToolCallID, `{"status":"已发货，预计明天送达"}`); err != nil {
							t.Fatalf("send tool result: %v", err)
						}
					}
					if len(reply.tools) > 0 {
						after := c.expectReply(t, "after tool result")
						afterText = after.text
						t.Logf("[%s #%d] ask %d after tool: %q", a.name, run, i+1, after.text)
					}
					has := strings.Contains(midNormalize(reply.text), midMarker)
					if len(reply.tools) > 0 {
						has = strings.Contains(midNormalize(afterText), midMarker)
					}
					t.Logf("[%s #%d] ask %d reply: %q marker=%v", a.name, run, i+1, reply.text, has)
					if a.checked >= 0 && i >= a.checked {
						total++
						if has {
							obeyed++
						}
					}
					if a.name == "tools" && i == 1 {
						total++
						if has {
							obeyed++
						}
					}
					if a.checked < 0 && !a.tools && has {
						t.Errorf("%s #%d: the control said the marker on its own: %q", a.name, run, reply.text)
					}
				}
				c.close(t)
			}
			t.Logf("[%s/%s] RESULT marker obeyed %d/%d, tool calls %d over %d sessions",
				profile.Name, a.name, obeyed, total, toolCalls, runs)
			if a.checked >= 0 && obeyed != total {
				t.Errorf("%s: %d of %d post-update replies obeyed the new instructions", a.name, obeyed, total)
			}
			if a.tools && toolCalls != runs {
				t.Errorf("%s: lookup_order called %d times in %d sessions", a.name, toolCalls, runs)
			}
			if a.name == "tools" && obeyed != total {
				t.Errorf("tools: %d of %d replies after the tool result carried the marker", obeyed, total)
			}
		})
	}
}

type midReply struct {
	text  string
	tools []Event
}

type midCall struct {
	session *Realtime
	encode  func(pcm16k []byte) []byte
	frame   int
	mu      sync.Mutex
	queue   []byte
	stop    chan struct{}
	done    chan struct{}
}

func openMidCall(t *testing.T, profile Profile, withTool bool) *midCall {
	t.Helper()
	return openMidCallLogged(t, profile, withTool, slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn})))
}

// openMidCallLogged is openMidCall with the client's logger chosen by the test.
func openMidCallLogged(t *testing.T, profile Profile, withTool bool, log *slog.Logger) *midCall {
	t.Helper()
	session, err := New(profile, log)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	input, output := profile.FormatsFor(media.LawMu)
	cfg := SessionConfig{
		Instructions: midPersona,
		Language:     "zh",
		Turn:         DefaultTurnDetection(),
		InputFormat:  input,
		OutputFormat: output,
	}
	if withTool {
		cfg.Tools = []ToolSpec{{
			Name:        midTool,
			Description: "Look up the status of a customer's order.",
			Parameters: json.RawMessage(`{"type":"object","properties":` +
				`{"orderId":{"type":"string","description":"the order number"}}}`),
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := session.Start(ctx, cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	enc := midEncoder(input)
	c := &midCall{session: session, stop: make(chan struct{}), done: make(chan struct{}),
		encode: enc, frame: len(enc(make([]byte, 640)))}
	go c.feed()
	return c
}

// midEncoder turns 16 kHz mono PCM16 into the format the profile takes.
func midEncoder(format media.AudioFormat) func([]byte) []byte {
	if format.Encoding != media.EncodingMuLaw && format.Encoding != media.EncodingALaw {
		return func(pcm []byte) []byte { return pcm }
	}
	law := media.LawMu
	if format.Encoding == media.EncodingALaw {
		law = media.LawAlaw
	}
	return func(pcm []byte) []byte {
		n := len(pcm) / 4
		samples := make([]int16, n)
		for i := range n {
			a := int(int16(binary.LittleEndian.Uint16(pcm[4*i:])))
			b := int(int16(binary.LittleEndian.Uint16(pcm[4*i+2:])))
			samples[i] = int16((a + b) / 2)
		}
		return law.Encode(nil, samples)
	}
}

func readMidClip(t *testing.T, format media.AudioFormat, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".pcm")
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	enc := midEncoder(format)
	out := enc(data)
	// Whole frames only, so the stream stays on the 20 ms grid.
	frame := len(enc(make([]byte, 640)))
	if rest := len(out) % frame; rest != 0 {
		out = append(out, enc(make([]byte, 640))[:frame-rest]...)
	}
	return out
}

// feed is the caller's microphone: queued speech when there is some, room tone
// otherwise, one 20 ms frame per tick.
func (c *midCall) feed() {
	defer close(c.done)
	ticker := time.NewTicker(FrameInterval)
	defer ticker.Stop()
	phase := 0
	tone := make([]byte, 640)
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
		var frame []byte
		c.mu.Lock()
		if len(c.queue) >= c.frame {
			frame = append([]byte(nil), c.queue[:c.frame]...)
			c.queue = c.queue[c.frame:]
		} else {
			c.queue = nil
		}
		c.mu.Unlock()
		if frame == nil {
			for i := range 320 {
				binary.LittleEndian.PutUint16(tone[2*i:], uint16(int16(30*math.Sin(float64(phase+i)/7.0))))
			}
			phase += 320
			frame = c.encode(tone)
		}
		if c.session.SendAudio(frame) != nil {
			return
		}
	}
}

// say queues a question behind a short stretch of room tone.
func (c *midCall) say(audio []byte) {
	quiet := c.encode(make([]byte, 640))
	c.mu.Lock()
	for range 10 {
		c.queue = append(c.queue, quiet...)
	}
	c.queue = append(c.queue, audio...)
	c.mu.Unlock()
}

// expectReply reads events until the model's turn ends, then a moment longer
// for the final transcript, which can trail the end of the audio.
func (c *midCall) expectReply(t *testing.T, what string) midReply {
	t.Helper()
	var reply midReply
	deadline := time.After(40 * time.Second)
	ended := false
	var grace <-chan time.Time
	for {
		select {
		case <-deadline:
			t.Fatalf("%s: no complete reply within 40 s (text so far %q)", what, reply.text)
		case <-grace:
			return reply
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatalf("%s: the session ended", what)
			}
			switch ev.Type {
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					reply.text += ev.Text
				}
			case EventTypeToolCall:
				reply.tools = append(reply.tools, ev)
			case EventTypeSessionReady:
				t.Logf("session.updated acknowledged")
			case EventTypeResponseDone:
				if !ended {
					ended = true
					grace = time.After(1500 * time.Millisecond)
				}
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
				if ev.IsFatal {
					t.Fatalf("%s: fatal provider error", what)
				}
			}
		}
	}
}

// waitSpeaking returns once the model has started a reply.
func (c *midCall) waitSpeaking(t *testing.T) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("the model never started to answer")
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			if ev.Type == EventTypeAudioDelta {
				return
			}
		}
	}
}

func (c *midCall) update(t *testing.T, text string) {
	t.Helper()
	if err := c.session.UpdateInstructions(text); err != nil {
		t.Fatalf("update instructions: %v", err)
	}
}

func (c *midCall) close(t *testing.T) {
	t.Helper()
	close(c.stop)
	<-c.done
	if err := c.session.Close(context.Background()); err != nil {
		t.Errorf("close: %v", err)
	}
}

// midNormalize drops the punctuation and spaces a transcript is free to vary.
func midNormalize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '，', ',', '。', '.', '！', '!', '？', '?', '、', '“', '”', '"', '：', ':':
			return -1
		}
		return r
	}, s)
}
