// SPDX-License-Identifier: Apache-2.0

package gemini

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

// fakeGemini is a scripted Live API endpoint. It records every frame the client
// sends, in arrival order and as bytes, and replies with whatever the test tells
// it to — which is what lets the whole protocol be exercised without a network
// or a credential.
//
// The arrival order is not a convenience: half of what this client promises is
// about what is NOT on the wire after a given frame, and a decoded map cannot
// answer that.
//
// It answers in BINARY frames and, where a test asks for it, pretty printed,
// because that is how the real service answers. A client that read the opcode
// or the whitespace would pass every test here and fail on the first real call.
type fakeGemini struct {
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
	handshakeURL    string

	closeOnce sync.Once
}

// newFakeGemini starts an endpoint. reply is called for each client frame on the
// server's own goroutine and may send frames back.
func newFakeGemini(t *testing.T, reply func(f *fakeGemini, message map[string]any)) *fakeGemini {
	t.Helper()

	f := &fakeGemini{t: t}
	upgrader := websocket.Upgrader{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.recordHandshake(req)
		conn, err := upgrader.Upgrade(w, req, nil)
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

// acceptSetup is the ordinary reply: the session is configured, and the noise
// the real service sends around it comes with it.
func acceptSetup(f *fakeGemini, message map[string]any) {
	if _, isSetup := message["setup"]; !isSetup {
		return
	}
	f.send(map[string]any{"setupComplete": map[string]any{}})
	// Unsolicited, on a setup that never asked to be resumable. Measured on
	// every session.
	f.send(map[string]any{"sessionResumptionUpdate": map[string]any{
		"newHandle": "fake-handle-1", "resumable": true}})
	f.noise()
}

// refuseSetup closes the socket the way this service refuses a configuration it
// will not have: a code and a sentence, with no frame at all.
func refuseSetup(code int, reason string) func(*fakeGemini, map[string]any) {
	return func(f *fakeGemini, message map[string]any) {
		if _, isSetup := message["setup"]; isSetup {
			f.closeWith(code, reason)
		}
	}
}

func (f *fakeGemini) endpoint() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http")
}

// send pushes one frame to the client, as the service does: binary opcode,
// compact JSON.
func (f *fakeGemini) send(event map[string]any) {
	data, err := json.Marshal(event)
	if err != nil {
		f.t.Fatalf("encode fake event: %v", err)
	}
	f.sendRaw(data)
}

// sendPretty pushes one frame indented, which is how the real service formats
// them. Nothing about a frame's meaning may depend on that.
func (f *fakeGemini) sendPretty(event map[string]any) {
	data, err := json.MarshalIndent(event, "", "  ")
	if err != nil {
		f.t.Fatalf("encode fake event: %v", err)
	}
	f.sendRaw(data)
}

// noise is the frames that mean nothing and arrive constantly: a bare object, an
// empty serverContent, a resumption handle nobody asked for.
func (f *fakeGemini) noise() {
	f.sendRaw([]byte(`{}`))
	f.send(map[string]any{"serverContent": map[string]any{}})
	f.send(map[string]any{"usageMetadata": map[string]any{"totalTokenCount": 1692}})
	f.send(map[string]any{"sessionResumptionUpdate": map[string]any{
		"newHandle": "fake-handle-2", "resumable": true}})
}

// sendRaw pushes a pre-encoded frame, with the opcode the service uses.
func (f *fakeGemini) sendRaw(data []byte) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("the fake provider was asked to send before the client connected")
	}

	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		f.t.Logf("fake provider write failed: %v", err)
	}
}

// closeWith ends the socket with a close code and a reason, which is the only
// error channel this protocol has.
func (f *fakeGemini) closeWith(code int, reason string) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		return
	}

	f.writeMu.Lock()
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	f.writeMu.Unlock()
	time.Sleep(20 * time.Millisecond)
	_ = conn.Close()
}

// hangUp drops the connection, as a provider failure would.
func (f *fakeGemini) hangUp() {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// messages returns everything the client has sent so far.
func (f *fakeGemini) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.received...)
}

// messagesOfKind narrows the record to one kind of client frame. A frame's kind
// is its single top-level key: there is no type field on this protocol.
func (f *fakeGemini) messagesOfKind(kind string) []map[string]any {
	var out []map[string]any
	for _, message := range f.messages() {
		if _, isKind := message[kind]; isKind {
			out = append(out, message)
		}
	}
	return out
}

// awaitMessages waits for n client frames of a kind and returns them, then holds
// still long enough for an n+1th to turn up.
//
// Every assertion about what a frame says is paired with one of these. A frame
// that is right and sent twice is a different bug from one that is wrong — a
// second user turn kills the line the first asked for — and a test that reads
// only the first frame it likes cannot tell them apart. n may be zero, which
// asserts that none is sent at all.
func (f *fakeGemini) awaitMessages(kind string, n int) []map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for n > 0 && time.Now().Before(deadline) {
		if len(f.messagesOfKind(kind)) >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	got := f.messagesOfKind(kind)
	if len(got) != n {
		f.t.Fatalf("the client sent %d %q frames, want %d; it sent %v",
			len(got), kind, n, f.frameKinds())
	}
	return got
}

// awaitFrames waits until n frames in total have reached the provider.
func (f *fakeGemini) awaitFrames(n int) [][]byte {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.rawFrames()) >= n {
			return f.rawFrames()
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("only %d frames arrived, want %d: %v", len(f.rawFrames()), n, f.frameKinds())
	return nil
}

// settledFrames holds still long enough for an n+1th frame to turn up, then
// returns the record.
func (f *fakeGemini) settledFrames(n int) [][]byte {
	f.t.Helper()
	f.awaitFrames(n)
	time.Sleep(150 * time.Millisecond)
	frames := f.rawFrames()
	if len(frames) != n {
		f.t.Fatalf("the client sent %d frames, want %d: %v", len(frames), n, f.frameKinds())
	}
	return frames
}

// frameKinds is the arrival order, by top-level key.
func (f *fakeGemini) frameKinds() []string {
	out := make([]string, 0)
	for _, message := range f.messages() {
		kinds := make([]string, 0, len(message))
		for key := range message {
			kinds = append(kinds, key)
		}
		out = append(out, strings.Join(kinds, "+"))
	}
	return out
}

func (f *fakeGemini) recordHandshake(req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handshakeHeader = req.Header.Clone()
	f.handshakeURL = req.URL.String()
}

// handshake returns the upgrade request's headers.
func (f *fakeGemini) handshake() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handshakeHeader
}

// requestURL is the path and query the client dialled.
func (f *fakeGemini) requestURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handshakeURL
}

func (f *fakeGemini) recordFrame(data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawReceived = append(f.rawReceived, append([]byte(nil), data...))
}

// rawFrames returns every client frame exactly as it arrived, in order.
func (f *fakeGemini) rawFrames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.rawReceived...)
}

// assertNothingFollowed is the promise that no frame of any kind is written once
// the session has begun ending. There is no goodbye frame on this protocol — the
// WebSocket close is the goodbye — so what is checked is that the record has not
// grown.
func (f *fakeGemini) assertNothingFollowed(count int) {
	f.t.Helper()
	time.Sleep(150 * time.Millisecond)
	if got := f.rawFrames(); len(got) != count {
		f.t.Fatalf("%d frames followed the close: %v", len(got)-count, f.frameKinds()[count:])
	}
}

//
// The client under test.
//

// testProfile is a Gemini-shaped profile pointed at the fake. Registration is
// not this package's business, so the test states the values itself.
func testProfile(endpoint string) provider.Profile {
	return provider.Profile{
		Name:      "gemini",
		Endpoint:  endpoint,
		Model:     "a-model-this-client-ignores",
		APIKeyEnv: "GEMINI_API_KEY",
		Voice:     "Kore",

		LinearInput:  media.PCM16Format(media.RateProviderIn),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
	}
}

// testConfig is a flow's worth of configuration, with the tool schema written
// the way every flow in this repository writes one: lowercase JSON Schema types.
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
func testSession(t *testing.T, f *fakeGemini) *Session {
	t.Helper()

	t.Setenv("GEMINI_API_KEY", "test-key")
	session, err := newSession(testProfile(f.endpoint()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session
}

// startedSession is the ordinary opening: the session is configured and the
// opening turn has been asked for.
func startedSession(t *testing.T, f *fakeGemini) *Session {
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

// settledEvents takes everything the session has to say and then waits long
// enough to be sure there is no more of it. It is for the assertions that are
// about a whole conversation rather than a sequence: how many turns there were,
// and how many of them ended.
func settledEvents(t *testing.T, session *Session) []provider.Event {
	t.Helper()
	var out []provider.Event
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				return out
			}
			out = append(out, event)
		case <-time.After(300 * time.Millisecond):
			return out
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

// modelAudio is one audio part, which is the shape every frame of model speech
// arrives in.
func modelAudio(payload string) map[string]any {
	return map[string]any{"serverContent": map[string]any{
		"modelTurn": map[string]any{
			"role": "model",
			"parts": []any{map[string]any{"inlineData": map[string]any{
				"mimeType": "audio/pcm;rate=24000", "data": payload}}},
		}}}
}

// modelAudioParts is several audio parts in one frame, with the transcript of
// them alongside. Every part is speech the caller is owed.
func modelAudioParts(transcript string, payloads ...string) map[string]any {
	parts := make([]any, 0, len(payloads))
	for _, payload := range payloads {
		parts = append(parts, map[string]any{"inlineData": map[string]any{
			"mimeType": "audio/pcm;rate=24000", "data": payload}})
	}
	body := map[string]any{"modelTurn": map[string]any{"role": "model", "parts": parts}}
	if transcript != "" {
		body["outputTranscription"] = map[string]any{"text": transcript}
	}
	return map[string]any{"serverContent": body}
}

func outputTranscript(text string) map[string]any {
	return map[string]any{"serverContent": map[string]any{
		"outputTranscription": map[string]any{"text": text}}}
}

func inputTranscript(text string) map[string]any {
	return map[string]any{"serverContent": map[string]any{
		"inputTranscription": map[string]any{"text": text}}}
}

func generationComplete() map[string]any {
	return map[string]any{"serverContent": map[string]any{"generationComplete": true}}
}

// turnComplete arrives with the usage of the whole turn attached, seconds after
// the model stopped.
func turnComplete() map[string]any {
	return map[string]any{
		"serverContent": map[string]any{"turnComplete": true},
		"usageMetadata": map[string]any{"totalTokenCount": 1692},
	}
}

func interrupted() map[string]any {
	return map[string]any{"serverContent": map[string]any{"interrupted": true}}
}

// interruptedAndComplete is the two flags in one frame. Measured always apart,
// but the protocol describes one message with both and a client that could only
// read them separately would silently stop listening.
func interruptedAndComplete() map[string]any {
	return map[string]any{"serverContent": map[string]any{
		"interrupted": true, "turnComplete": true}}
}

func toolCallFrame(calls ...map[string]any) map[string]any {
	list := make([]any, 0, len(calls))
	for _, call := range calls {
		list = append(list, call)
	}
	return map[string]any{"toolCall": map[string]any{"functionCalls": list}}
}

func functionCallOf(id, name string, args map[string]any) map[string]any {
	call := map[string]any{"id": id, "name": name}
	if args != nil {
		call["args"] = args
	}
	return call
}

func toolCallCancellationFrame(ids ...string) map[string]any {
	list := make([]any, 0, len(ids))
	for _, id := range ids {
		list = append(list, id)
	}
	return map[string]any{"toolCallCancellation": map[string]any{"ids": list}}
}

func goAwayFrame(timeLeft string) map[string]any {
	return map[string]any{"goAway": map[string]any{"timeLeft": timeLeft}}
}

// nested walks a decoded JSON object.
func nested(t *testing.T, value any, path ...string) any {
	t.Helper()
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("expected an object at %q, got %T", key, value)
		}
		value, ok = object[key]
		if !ok {
			t.Fatalf("key %q is missing from %v", key, object)
		}
	}
	return value
}
