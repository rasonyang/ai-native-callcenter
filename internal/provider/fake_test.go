// SPDX-License-Identifier: Apache-2.0

package provider

import (
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
)

// fakeProvider is a scripted provider endpoint. It records everything the
// client sends and replies with whatever the test tells it to, which lets the
// whole protocol be exercised without a network or a credential.
type fakeProvider struct {
	t      *testing.T
	server *httptest.Server

	mu   sync.Mutex
	conn *websocket.Conn
	// writeMu serialises sends: replies come from the server goroutine while
	// tests push events from their own, and a WebSocket has one writer.
	writeMu  sync.Mutex
	received []map[string]any

	connected chan struct{}
	closeOnce sync.Once
}

// newFakeProvider starts an endpoint. reply is called for each client message
// on the server's own goroutine and may send events back.
func newFakeProvider(t *testing.T, reply func(f *fakeProvider, message map[string]any)) *fakeProvider {
	t.Helper()

	f := &fakeProvider{t: t, connected: make(chan struct{})}
	upgrader := websocket.Upgrader{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		close(f.connected)

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
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

// acceptSession is the ordinary reply: confirm the configuration.
func acceptSession(f *fakeProvider, message map[string]any) {
	if message["type"] == "session.update" {
		f.send(map[string]any{"type": "session.updated"})
	}
}

func (f *fakeProvider) endpoint() string {
	return "ws" + strings.TrimPrefix(f.server.URL, "http")
}

// send pushes one event to the client.
func (f *fakeProvider) send(event map[string]any) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("the fake provider was asked to send before the client connected")
	}
	data, err := json.Marshal(event)
	if err != nil {
		f.t.Fatalf("encode fake event: %v", err)
	}

	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		f.t.Logf("fake provider write failed: %v", err)
	}
}

// sendRaw pushes a pre-encoded frame, for malformed-input tests.
func (f *fakeProvider) sendRaw(data string) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()

	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = conn.WriteMessage(websocket.TextMessage, []byte(data))
}

// hangUp drops the connection, as a provider failure would.
func (f *fakeProvider) hangUp() {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// messages returns everything the client has sent so far.
func (f *fakeProvider) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.received...)
}

// awaitMessage waits for a client message of a type and returns it.
func (f *fakeProvider) awaitMessage(messageType string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range f.messages() {
			if message["type"] == messageType {
				return message
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("the client never sent %q; it sent %v", messageType, typesOf(f.messages()))
	return nil
}

// refuteMessage fails if a message of this type is ever sent.
func (f *fakeProvider) refuteMessage(messageType string) {
	f.t.Helper()
	time.Sleep(150 * time.Millisecond)
	for _, message := range f.messages() {
		if message["type"] == messageType {
			f.t.Errorf("the client sent %q when it should not have", messageType)
		}
	}
}

func typesOf(messages []map[string]any) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, m["type"].(string))
	}
	return out
}

// testSession builds a client pointed at a fake provider.
func testSession(t *testing.T, f *fakeProvider, profile Profile) *Realtime {
	t.Helper()

	t.Setenv(profile.APIKeyEnv, "test-key")
	profile.Endpoint = f.endpoint()

	session, err := New(profile, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(t.Context()) })
	return session
}

// awaitEvent waits for the next event of a type, ignoring others.
func awaitEvent(t *testing.T, session *Realtime, want EventType) Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	var seen []EventType
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
