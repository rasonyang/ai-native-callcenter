// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// fakeEngine is a websocket server that replays a scripted set of frames once
// the client has said what it says first.
type fakeEngine struct {
	srv    *httptest.Server
	script func(c *websocket.Conn)
	// onFinish runs when the client sends finish-task, so a test can model a
	// service that flushes a trailing result — which is the only place a
	// trailing result can come from. It runs on the handler goroutine, not the
	// reader: a websocket permits one writer, and the script writes too.
	onFinish   func(c *websocket.Conn)
	finishSeen chan struct{}
	// hold is how long the socket stays open after the script runs. Long
	// enough that a scripted run does not race the client's teardown; a test
	// about the client waiting must set it beyond whatever it waits for, or
	// the socket closing releases the wait and the test proves nothing.
	hold time.Duration
	// received records what the client sent, so the tests can assert on the
	// wire rather than on the client's internals.
	received chan string
	binary   chan int
}

func newFakeEngine(t *testing.T, script func(*websocket.Conn)) *fakeEngine {
	t.Helper()
	f := &fakeEngine{script: script, hold: 200 * time.Millisecond,
		received: make(chan string, 32), binary: make(chan int, 32),
		finishSeen: make(chan struct{}, 1)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		go func() {
			for {
				mt, data, err := c.ReadMessage()
				if err != nil {
					return
				}
				if mt == websocket.BinaryMessage {
					select {
					case f.binary <- len(data):
					default:
					}
					continue
				}
				if strings.Contains(string(data), "finish-task") {
					select {
					case f.finishSeen <- struct{}{}:
					default:
					}
				}
				select {
				case f.received <- string(data):
				default:
				}
			}
		}()
		if f.script != nil {
			f.script(c)
		}
		if f.onFinish != nil {
			select {
			case <-f.finishSeen:
				f.onFinish(c)
			case <-time.After(5 * time.Second):
			}
		}
		time.Sleep(f.hold)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEngine) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func drain(t *testing.T, s Session, want int, timeout time.Duration) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("got %d events, want %d: %+v", len(got), want, got)
		}
	}
	return got
}

func onlyText(events []Event, kind EventType) []string {
	var out []string
	for _, e := range events {
		if e.Type == kind {
			out = append(out, e.Text)
		}
	}
	return out
}

//
// The substitutability suite. Both clients see the same shape of conversation
// in their own dialect and must produce the same events, because the seam
// above is not allowed to know which vendor is behind it.
//

func TestBothClientsReportCumulativeTextAndOneFinal(t *testing.T) {
	cases := []struct {
		name   string
		script func(*websocket.Conn)
		build  func(endpoint string) Session
	}{
		{
			name: "dashscope",
			script: func(c *websocket.Conn) {
				send := func(v any) { _ = c.WriteJSON(v) }
				send(map[string]any{"header": map[string]any{"event": "task-started"}})
				// Cumulative partials, exactly as measured.
				for _, s := range []string{"I", "I need", "I need help"} {
					send(dsResult(1, s, false))
				}
				send(dsResult(1, "I need help.", true))
			},
			build: func(endpoint string) Session {
				return newDashscope(Profile{Name: ProviderQwen, Endpoint: endpoint,
					Model: "m", SampleRate: 16000}, "k", nopLogger{})
			},
		},
		{
			name: "openairt",
			script: func(c *websocket.Conn) {
				send := func(v any) { _ = c.WriteJSON(v) }
				// Incremental deltas, which the client must accumulate.
				for _, d := range []string{"I", " need", " help"} {
					send(map[string]any{
						"type":    "conversation.item.input_audio_transcription.delta",
						"item_id": "item_1", "delta": d,
					})
				}
				send(map[string]any{
					"type":    "conversation.item.input_audio_transcription.completed",
					"item_id": "item_1", "transcript": "I need help.",
				})
			},
			build: func(endpoint string) Session {
				return newOpenAIRT(Profile{Name: ProviderOpenAI, Endpoint: endpoint,
					Model: "m", SampleRate: 24000}, "k", nopLogger{})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine(t, tc.script)
			s := tc.build(engine.url())
			if err := s.Start(context.Background(), Config{Language: "en"}); err != nil {
				t.Fatalf("start: %v", err)
			}
			defer s.Close(context.Background())

			events := drain(t, s, 4, 3*time.Second)

			// Every partial carries the whole utterance so far. A client that
			// leaked one vendor's fragments would fail here — and on screen it
			// would read "II needI need help".
			if got := onlyText(events, EventPartial); len(got) != 3 ||
				got[0] != "I" || got[1] != "I need" || got[2] != "I need help" {
				t.Errorf("partials = %q, want cumulative I / I need / I need help", got)
			}
			if got := onlyText(events, EventFinal); len(got) != 1 || got[0] != "I need help." {
				t.Errorf("finals = %q, want one settled line", got)
			}
		})
	}
}

func TestBothClientsDropAnEmptyFinal(t *testing.T) {
	cases := []struct {
		name   string
		script func(*websocket.Conn)
		build  func(string) Session
	}{
		{
			name: "dashscope",
			script: func(c *websocket.Conn) {
				_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-started"}})
				_ = c.WriteJSON(dsResult(1, "", true)) // leading silence
				_ = c.WriteJSON(dsResult(2, "hello", true))
			},
			build: func(e string) Session {
				return newDashscope(Profile{Name: ProviderQwen, Endpoint: e, Model: "m", SampleRate: 16000}, "k", nopLogger{})
			},
		},
		{
			name: "openairt",
			script: func(c *websocket.Conn) {
				_ = c.WriteJSON(map[string]any{
					"type":    "conversation.item.input_audio_transcription.completed",
					"item_id": "a", "transcript": "   ",
				})
				_ = c.WriteJSON(map[string]any{
					"type":    "conversation.item.input_audio_transcription.completed",
					"item_id": "b", "transcript": "hello",
				})
			},
			build: func(e string) Session {
				return newOpenAIRT(Profile{Name: ProviderOpenAI, Endpoint: e, Model: "m", SampleRate: 24000}, "k", nopLogger{})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine(t, tc.script)
			s := tc.build(engine.url())
			if err := s.Start(context.Background(), Config{}); err != nil {
				t.Fatalf("start: %v", err)
			}
			defer s.Close(context.Background())

			events := drain(t, s, 1, 3*time.Second)
			finals := onlyText(events, EventFinal)
			// The empty final must never reach the seam: the transcript actor
			// allocates a seq for what it accepts, and a seq spent on silence
			// puts a permanent hole between a snapshot and its live tail.
			if len(finals) != 1 || finals[0] != "hello" {
				t.Errorf("finals = %q, want only the spoken one", finals)
			}
		})
	}
}

//
// Per-protocol behaviour, where the two legitimately differ.
//

func TestDashscopeSendsRunTaskAndRawBinaryAudio(t *testing.T) {
	engine := newFakeEngine(t, func(c *websocket.Conn) {
		_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-started"}})
	})
	s := newDashscope(Profile{Name: ProviderQwen, Endpoint: engine.url(),
		Model: "qwen-audio-3.0-asr-flash-streaming", SampleRate: 16000}, "k", nopLogger{})
	if err := s.Start(context.Background(), Config{Language: "zh"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close(context.Background())

	select {
	case raw := <-engine.received:
		var run dsRunTask
		if err := json.Unmarshal([]byte(raw), &run); err != nil {
			t.Fatalf("run-task did not decode: %v", err)
		}
		if run.Header.Action != "run-task" || run.Header.Streaming != "duplex" {
			t.Errorf("header = %+v", run.Header)
		}
		if run.Payload.Task != "asr" || run.Payload.Function != "recognition" {
			t.Errorf("payload = %+v", run.Payload)
		}
		if run.Payload.Parameters["sample_rate"] != float64(16000) {
			t.Errorf("sample_rate = %v, want 16000", run.Payload.Parameters["sample_rate"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no run-task was sent")
	}

	// Audio goes as raw binary: no base64, no JSON envelope per frame.
	if err := s.SendAudio(make([]byte, 640)); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	select {
	case n := <-engine.binary:
		if n != 640 {
			t.Errorf("binary frame = %d bytes, want 640", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("audio did not arrive as a binary frame")
	}
}

func TestDashscopeSkipsHeartbeatsAndStopsOnTaskFailed(t *testing.T) {
	engine := newFakeEngine(t, func(c *websocket.Conn) {
		_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-started"}})
		hb := dsResult(0, "ignore me", true)
		hb["payload"].(map[string]any)["output"].(map[string]any)["sentence"].(map[string]any)["heartbeat"] = true
		_ = c.WriteJSON(hb)
		_ = c.WriteJSON(map[string]any{"header": map[string]any{
			"event": "task-failed", "error_code": "CLIENT_ERROR", "error_message": "boom"}})
	})
	s := newDashscope(Profile{Name: ProviderQwen, Endpoint: engine.url(), Model: "m", SampleRate: 16000}, "k", nopLogger{})
	if err := s.Start(context.Background(), Config{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close(context.Background())

	events := drain(t, s, 1, 3*time.Second)
	if len(onlyText(events, EventFinal)) != 0 {
		t.Error("a heartbeat was surfaced as speech")
	}
	if events[0].Type != EventError || !strings.Contains(events[0].Err.Error(), "boom") {
		t.Fatalf("first event = %+v, want the task failure", events[0])
	}
	// The protocol documents a failed task's connection as unusable, so
	// nothing further is attempted on it.
	if err := s.SendAudio(make([]byte, 640)); err == nil {
		t.Error("audio was accepted after task-failed")
	}
}

func TestOpenAIRTRefusesNothingButOwnsTheBoundary(t *testing.T) {
	engine := newFakeEngine(t, func(c *websocket.Conn) {})
	s := newOpenAIRT(Profile{Name: ProviderOpenAI, Endpoint: engine.url(),
		Model: "gpt-live-transcribe", SampleRate: 24000}, "k", nopLogger{})
	if err := s.Start(context.Background(), Config{Language: "en"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close(context.Background())

	select {
	case raw := <-engine.received:
		var got map[string]any
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("session.update did not decode: %v", err)
		}
		input := got["session"].(map[string]any)["audio"].(map[string]any)["input"].(map[string]any)
		// Measured: this model refuses turn detection, so it must be present
		// and null rather than omitted.
		td, present := input["turn_detection"]
		if !present || td != nil {
			t.Errorf("turn_detection = %v (present=%v), want an explicit null", td, present)
		}
		if rate := input["format"].(map[string]any)["rate"]; rate != float64(24000) {
			t.Errorf("rate = %v, want 24000 — the session refuses anything lower", rate)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no session.update was sent")
	}

	// Because the model will not end an utterance, the client must expose the
	// commit that does. Without it a final never arrives at all.
	if err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	select {
	case raw := <-engine.received:
		if !strings.Contains(raw, "input_audio_buffer.commit") {
			t.Errorf("second frame = %s, want the commit", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commit was not sent")
	}
}

//
// Profiles.
//

func TestProfileForCarriesTheMeasuredConstraints(t *testing.T) {
	openai, err := ProfileFor("openai", Override{})
	if err != nil {
		t.Fatalf("openai: %v", err)
	}
	if openai.SampleRate != 24000 {
		t.Errorf("openai rate = %d, want 24000", openai.SampleRate)
	}
	if !openai.OwnsEndpointing {
		t.Error("openai must own endpointing: the model refuses turn detection")
	}

	qwen, err := ProfileFor("qwen", Override{})
	if err != nil {
		t.Fatalf("qwen: %v", err)
	}
	if qwen.SampleRate != 16000 {
		t.Errorf("qwen rate = %d, want 16000", qwen.SampleRate)
	}
	if qwen.OwnsEndpointing {
		t.Error("qwen segments server-side; we must not commit on it")
	}
	// The workspace id is part of the hostname, so there is no usable default
	// and a deployment that forgets it must fail at startup, not on a call.
	if qwen.Endpoint != "" {
		t.Errorf("qwen endpoint = %q, want empty so Validate refuses it", qwen.Endpoint)
	}
	if err := qwen.Validate(); err == nil {
		t.Error("a qwen profile with no endpoint validated")
	}

	if _, err := ProfileFor("cascade", Override{}); !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown provider error = %v", err)
	}
}

func TestProfileValidateRejectsANonWebsocketEndpoint(t *testing.T) {
	p := Profile{Name: "qwen", Endpoint: "https://example.invalid/x", Model: "m"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "ws://") {
		t.Errorf("error = %v, want a scheme complaint", err)
	}
}

// dsResult builds a result-generated envelope.
func dsResult(sentenceID int, text string, final bool) map[string]any {
	begin := 0
	sentence := map[string]any{
		"begin_time":     begin,
		"text":           text,
		"sentence_begin": !final,
		"sentence_end":   final,
		"sentence_id":    sentenceID,
	}
	if final {
		sentence["end_time"] = 1000
	}
	return map[string]any{
		"header":  map[string]any{"event": "result-generated"},
		"payload": map[string]any{"output": map[string]any{"sentence": sentence}},
	}
}

// No audio may reach the service before it says the task exists.
//
// Measured on a live call (2026-08-18): Start returned as soon as run-task was
// written, so 245 frames — 4.9 seconds of a real conversation — were streamed
// into a task that did not exist yet. DashScope discards them and says nothing:
// the socket stays open, no error arrives, no result ever comes back, and the
// session reads healthy the whole time. Nothing above this could tell.
func TestStartWaitsForTheTaskToExistBeforeAnyAudio(t *testing.T) {
	var mu sync.Mutex
	var binaryBeforeStart int
	started := make(chan struct{})

	f := newFakeEngine(t, func(c *websocket.Conn) {
		// The service takes its time, as the Beijing host measurably does.
		time.Sleep(150 * time.Millisecond)
		_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-started"}})
		close(started)
	})
	go func() {
		for n := range f.binary {
			mu.Lock()
			select {
			case <-started:
			default:
				binaryBeforeStart += n
			}
			mu.Unlock()
		}
	}()

	client := newDashscope(Profile{Name: ProviderQwen, Endpoint: f.url(),
		Model: "m", SampleRate: 16000}, "k", nopLogger{})
	if err := client.Start(t.Context(), Config{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })

	// Start returned, so by contract the task exists.
	select {
	case <-started:
	default:
		t.Fatal("Start returned before task-started arrived")
	}
	if err := client.SendAudio(make([]byte, 640)); err != nil {
		t.Fatalf("send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if binaryBeforeStart != 0 {
		t.Errorf("%d bytes of audio were sent before the task started", binaryBeforeStart)
	}
}

// A task that never starts is a failure with its own name, not a healthy
// session. This is the state that read LIVE through a total outage.
func TestATaskThatNeverStartsIsAnError(t *testing.T) {
	// The service accepts the socket and then says nothing at all.
	f := newFakeEngine(t, func(*websocket.Conn) { time.Sleep(2 * time.Second) })

	client := newDashscope(Profile{Name: ProviderQwen, Endpoint: f.url(),
		Model: "m", SampleRate: 16000}, "k", nopLogger{})

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := client.Start(ctx, Config{})
	if err == nil {
		t.Fatal("Start reported success on a task that never started")
	}
	// Either the caller's deadline or our own bound may win the race; both are
	// failures, and neither is LIVE.
	if !errors.Is(err, ErrTaskNeverStarted) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Start failed with %v, want a never-started or deadline error", err)
	}
}

// A task-failed that beats task-started must release Start with the service's
// own reason rather than leaving it to time out.
func TestATaskFailedDuringStartupIsReportedImmediately(t *testing.T) {
	f := newFakeEngine(t, func(c *websocket.Conn) {
		_ = c.WriteJSON(map[string]any{"header": map[string]any{
			"event": "task-failed", "error_code": "InvalidParameter",
			"error_message": "model not found"}})
	})

	client := newDashscope(Profile{Name: ProviderQwen, Endpoint: f.url(),
		Model: "m", SampleRate: 16000}, "k", nopLogger{})

	done := make(chan error, 1)
	go func() { done <- client.Start(context.Background(), Config{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start reported success on a task the service refused")
		}
		if !strings.Contains(err.Error(), "InvalidParameter") {
			t.Errorf("Start failed with %v, want the service's own reason", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return on task-failed; it waited for the timeout")
	}
}

// The last sentence of a call arrives after finish-task, not before it.
//
// A caller hangs up mid-utterance; finish-task asks the service to flush what
// it is still holding, and the answer comes back on the same socket. Closing
// immediately after the request — which is what the live capture showed — puts
// the final line of every call in the bin.
func TestCloseWaitsForTheServiceToFlushItsLastResult(t *testing.T) {
	f := newFakeEngine(t, nil)
	f.script = func(c *websocket.Conn) {
		_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-started"}})
	}
	// The service answers finish-task with a trailing final, then task-finished.
	f.onFinish = func(c *websocket.Conn) {
		_ = c.WriteJSON(dsResult(7, "and my account number is 4471.", true))
		_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-finished"}})
	}

	client := newDashscope(Profile{Name: ProviderQwen, Endpoint: f.url(),
		Model: "m", SampleRate: 16000}, "k", nopLogger{})
	if err := client.Start(t.Context(), Config{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	got := make(chan Event, 8)
	go func() {
		for ev := range client.Events() {
			got <- ev
		}
	}()

	_ = client.Close(context.Background())

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-got:
			if ev.Type == EventFinal && strings.Contains(ev.Text, "4471") {
				return // the trailing sentence survived the close
			}
		case <-deadline:
			t.Fatal("the service's last result was thrown away with the socket")
		}
	}
}

// A recogniser that will not say goodbye must not hold a call's teardown open.
func TestCloseGivesUpOnAServiceThatNeverFinishes(t *testing.T) {
	f := newFakeEngine(t, func(c *websocket.Conn) {
		_ = c.WriteJSON(map[string]any{"header": map[string]any{"event": "task-started"}})
	})
	// onFinish is nil: finish-task is received and never answered. The socket
	// must outlive the client's wait, or it is the close that releases Close
	// and the timeout is never exercised.
	f.hold = dsFinishTimeout + 5*time.Second

	client := newDashscope(Profile{Name: ProviderQwen, Endpoint: f.url(),
		Model: "m", SampleRate: 16000}, "k", nopLogger{})
	if err := client.Start(t.Context(), Config{}); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan struct{})
	go func() { _ = client.Close(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(dsFinishTimeout + 3*time.Second):
		t.Fatalf("Close did not give up within %s", dsFinishTimeout)
	}
}
