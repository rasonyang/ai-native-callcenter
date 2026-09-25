// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// fakeDoubao is a scripted Doubao endpoint. It records every frame the client
// sends, in arrival order and as bytes, and replies with whatever the test
// tells it to — which is what lets the whole protocol be exercised without a
// network or a credential.
//
// The arrival order is not a convenience: half of what this client promises is
// about what is NOT on the wire after a given frame, and a decoded map cannot
// answer that.
type fakeDoubao struct {
	t      *testing.T
	server *httptest.Server

	mu   sync.Mutex
	conn *websocket.Conn
	// writeMu serialises sends: replies come from the server's own goroutine
	// while tests push events from theirs, and a WebSocket has one writer.
	writeMu     sync.Mutex
	received    []map[string]any
	rawReceived [][]byte

	handshakeHeader http.Header

	closeOnce sync.Once
}

// newFakeDoubao starts an endpoint. reply is called for each client frame on
// the server's own goroutine and may send events back.
func newFakeDoubao(t *testing.T, reply func(f *fakeDoubao, message map[string]any)) *fakeDoubao {
	t.Helper()

	f := &fakeDoubao{t: t}
	upgrader := websocket.Upgrader{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.recordHandshake(req.Header)
		// The session's identity rides the upgrade response on this protocol.
		header := http.Header{}
		header.Set("X-Tt-Logid", "fake-logid-1")
		conn, err := upgrader.Upgrade(w, req, header)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f.recordFrame(data)
			var message map[string]any
			if err := json.Unmarshal(data, &message); err != nil {
				continue
			}
			f.mu.Lock()
			f.received = append(f.received, message)
			f.mu.Unlock()
			if reply != nil {
				reply(f, message)
			}
		}
	}))
	t.Cleanup(func() {
		f.closeOnce.Do(func() { f.server.Close() })
	})
	return f
}

// acceptSession is the ordinary reply: the session exists, and it ends when it
// is asked to.
func acceptSession(f *fakeDoubao, message map[string]any) {
	switch message["type"] {
	case "session.create":
		f.send(map[string]any{"type": "session.created", "event_id": "event_1",
			"session": map[string]any{"id": "fake-session"}})
	case "session.update":
		f.send(map[string]any{"type": "session.updated", "event_id": "event_2",
			"session": map[string]any{"id": "fake-session"}})
	case "session.close":
		f.send(map[string]any{"type": "session.closed", "event_id": "event_3"})
	}
}

// acceptSessionButNeverClose answers everything except the close handshake,
// which is how a provider that has stopped listening behaves.
func acceptSessionButNeverClose(f *fakeDoubao, message map[string]any) {
	if message["type"] == "session.create" {
		f.send(map[string]any{"type": "session.created", "event_id": "event_1",
			"session": map[string]any{"id": "fake-session"}})
	}
}

func (f *fakeDoubao) endpoint() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http")
}

// send pushes one event to the client.
func (f *fakeDoubao) send(event map[string]any) {
	data, err := json.Marshal(event)
	if err != nil {
		f.t.Fatalf("encode fake event: %v", err)
	}
	f.sendRaw(string(data))
}

// sendRaw pushes a pre-encoded frame, for malformed-input tests.
func (f *fakeDoubao) sendRaw(data string) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("the fake provider was asked to send before the client connected")
	}

	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
		f.t.Logf("fake provider write failed: %v", err)
	}
}

// hangUp drops the connection, as a provider failure would.
func (f *fakeDoubao) hangUp() {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// messages returns everything the client has sent so far.
func (f *fakeDoubao) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.received...)
}

// messagesOfType narrows the record to one kind of client frame.
func (f *fakeDoubao) messagesOfType(messageType string) []map[string]any {
	var out []map[string]any
	for _, message := range f.messages() {
		if message["type"] == messageType {
			out = append(out, message)
		}
	}
	return out
}

// awaitMessages waits for n client frames of a type and returns them, then
// holds still long enough for an n+1th to turn up.
//
// Every assertion about what a frame says is paired with one of these. On this
// protocol a frame that is right and sent twice is a different bug from one
// that is wrong — a second speech_text_buffer.commit kills the line the first
// asked for — and a test that reads only the first frame it likes cannot tell
// them apart. n may be zero, which asserts that none is sent at all.
func (f *fakeDoubao) awaitMessages(messageType string, n int) []map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for n > 0 && time.Now().Before(deadline) {
		if len(f.messagesOfType(messageType)) >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	got := f.messagesOfType(messageType)
	if len(got) != n {
		f.t.Fatalf("the client sent %d %q frames, want %d; it sent %v",
			len(got), messageType, n, f.frameTypes())
	}
	return got
}

// awaitFrames waits until n frames in total have reached the provider.
func (f *fakeDoubao) awaitFrames(n int) [][]byte {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.rawFrames()) >= n {
			return f.rawFrames()
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("only %d frames arrived, want %d: %v", len(f.rawFrames()), n, f.frameTypes())
	return nil
}

// settledFrames holds still long enough for an n+1th frame to turn up, then
// returns the record.
func (f *fakeDoubao) settledFrames(n int) [][]byte {
	f.t.Helper()
	f.awaitFrames(n)
	time.Sleep(150 * time.Millisecond)
	frames := f.rawFrames()
	if len(frames) != n {
		f.t.Fatalf("the client sent %d frames, want %d: %v", len(frames), n, f.frameTypes())
	}
	return frames
}

// frameTypes is the arrival order, by event type.
func (f *fakeDoubao) frameTypes() []string {
	out := make([]string, 0)
	for _, message := range f.messages() {
		name, _ := message["type"].(string)
		out = append(out, name)
	}
	return out
}

func (f *fakeDoubao) recordHandshake(header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handshakeHeader = header.Clone()
}

// handshake returns the upgrade request's headers.
func (f *fakeDoubao) handshake() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handshakeHeader
}

func (f *fakeDoubao) recordFrame(data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawReceived = append(f.rawReceived, append([]byte(nil), data...))
}

// rawFrames returns every client frame exactly as it arrived, in order.
func (f *fakeDoubao) rawFrames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.rawReceived...)
}

// assertNothingFollowedTheClose is the promise that no frame of any kind is
// written once the session has been told to end.
func (f *fakeDoubao) assertNothingFollowedTheClose() {
	f.t.Helper()
	time.Sleep(100 * time.Millisecond)
	types := f.frameTypes()
	for i, name := range types {
		if name != "session.close" {
			continue
		}
		if i != len(types)-1 {
			f.t.Fatalf("frames followed session.close: %v", types[i+1:])
		}
		return
	}
}

//
// The client under test.
//

// testProfile is a Doubao-shaped profile pointed at the fake. Registration is
// not this package's business, so the test states the values itself.
func testProfile(endpoint string) provider.Profile {
	return provider.Profile{
		Name:      "doubao",
		Endpoint:  endpoint,
		Model:     "a-model-this-client-ignores",
		APIKeyEnv: "DOUBAO_API_KEY",
		Voice:     "zh_female_vv_jupiter_bigtts",

		LinearInput:  media.PCM16Format(media.RateProviderIn),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
	}
}

func testConfig() provider.SessionConfig {
	return provider.SessionConfig{
		Instructions: "You answer the phone for NovaNet.",
		Language:     "en",
		Turn:         provider.DefaultTurnDetection(),
		Tools: []provider.ToolSpec{{
			Name:        "lookup_balance",
			Description: "Read the caller's balance.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"accountId":{"type":"string"}}}`),
		}},
		InputFormat:  media.PCM16Format(media.RateProviderIn),
		OutputFormat: media.PCM16Format(media.RateProviderOut),
	}
}

// testSession builds a client pointed at a fake provider. Nothing is started.
func testSession(t *testing.T, f *fakeDoubao) *Session {
	t.Helper()

	t.Setenv("DOUBAO_API_KEY", "test-key")
	session, err := newSession(testProfile(f.endpoint()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session
}

// startedSession is the ordinary opening: the session exists and has said so.
func startedSession(t *testing.T, f *fakeDoubao) *Session {
	t.Helper()
	session := testSession(t, f)
	start(t, session, testConfig())
	return session
}

// start opens the session and consumes the one event that says it is up.
func start(t *testing.T, session *Session, cfg provider.SessionConfig) {
	t.Helper()
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	expectEvents(t, session, provider.EventTypeSessionReady)
}

//
// Events.
//

// nextEvent takes the next event, whatever it is.
func nextEvent(t *testing.T, session *Session) provider.Event {
	t.Helper()
	select {
	case event, ok := <-session.Events():
		if !ok {
			t.Fatal("the event stream closed while a further event was expected")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("no event arrived")
	}
	return provider.Event{}
}

// expectEvents asserts the next events are exactly these, in this order.
func expectEvents(t *testing.T, session *Session, want ...provider.EventType) []provider.Event {
	t.Helper()
	got := make([]provider.Event, 0, len(want))
	for i, wanted := range want {
		event := nextEvent(t, session)
		if event.Type != wanted {
			t.Fatalf("event %d is %s, want %s (the sequence so far was %v)",
				i, event.Type, wanted, typesOf(got))
		}
		got = append(got, event)
	}
	return got
}

// refuteMoreEvents is the cardinality half of every sequence assertion: the
// events named are the events there were.
func refuteMoreEvents(t *testing.T, session *Session) {
	t.Helper()
	select {
	case event, ok := <-session.Events():
		if !ok {
			t.Fatal("the event stream closed when nothing more was expected")
		}
		t.Fatalf("an unexpected %s event arrived", event.Type)
	case <-time.After(150 * time.Millisecond):
	}
}

// awaitEvent waits for the next event of a type, ignoring others.
func awaitEvent(t *testing.T, session *Session, want provider.EventType) provider.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	var seen []provider.EventType
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatalf("the event stream closed while waiting for %s (saw %v)", want, seen)
			}
			if event.Type == want {
				return event
			}
			seen = append(seen, event.Type)
		case <-deadline:
			t.Fatalf("no %s event arrived (saw %v)", want, seen)
		}
	}
}

// drainEvents reads until the stream closes and returns everything it carried.
func drainEvents(t *testing.T, session *Session) []provider.Event {
	t.Helper()
	var out []provider.Event
	deadline := time.After(4 * time.Second)
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				return out
			}
			out = append(out, event)
		case <-deadline:
			t.Fatalf("the event stream never closed; it carried %v", typesOf(out))
		}
	}
}

func typesOf(events []provider.Event) []provider.EventType {
	out := make([]provider.EventType, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func countOf(events []provider.Event, want provider.EventType) int {
	n := 0
	for _, event := range events {
		if event.Type == want {
			n++
		}
	}
	return n
}

//
// Downstream frames, as the probe recorded them.
//

func transcriptionStarted(itemID string) map[string]any {
	return map[string]any{
		"type": "conversation.item.input_audio_transcription.started", "item_id": itemID}
}

func transcriptionDelta(itemID, text string) map[string]any {
	return map[string]any{
		"type":    "conversation.item.input_audio_transcription.delta",
		"item_id": itemID, "content_index": 0, "delta": text}
}

func transcriptionCompleted(itemID, text string) map[string]any {
	return map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": itemID, "content_index": 1, "text": text}
}

func audioStarted(ttsType string) map[string]any {
	return map[string]any{"type": "response.output_audio.started",
		"question_id": "q-1", "response_id": "r-1", "tts_type": ttsType}
}

func audioDelta(payload string) map[string]any {
	return map[string]any{"type": "response.output_audio.delta", "delta": payload}
}

func audioDone() map[string]any {
	return map[string]any{"type": "response.output_audio.done",
		"question_id": "q-1", "response_id": "r-1"}
}

func outputTextDelta(text string) map[string]any {
	return map[string]any{"type": "response.output_text.delta", "delta": text}
}

func outputTextDone(text string) map[string]any {
	return map[string]any{"type": "response.output_text.done", "text": text}
}

// wireResponseDone is the frame that trails a turn by seconds and carries only
// what it cost.
func wireResponseDone() map[string]any {
	return map[string]any{"type": "response.done", "response": map[string]any{
		"usage": map[string]any{
			"total_tokens": 2321, "input_tokens": 2128, "output_tokens": 193}}}
}

func errorFrame(code, message string) map[string]any {
	return map[string]any{"type": "error", "event_id": "event_9",
		"error": map[string]any{"type": "Bad Request", "code": code, "message": message}}
}
