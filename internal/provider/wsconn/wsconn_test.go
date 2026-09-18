// SPDX-License-Identifier: Apache-2.0

package wsconn

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testServer accepts one socket and hands it to the test.
//
// handshake is what the upgrade answers with, so the client's view of it can be
// checked; serve runs on the server's own goroutine with the socket.
func testServer(t *testing.T, handshake http.Header, serve func(conn *websocket.Conn)) string {
	t.Helper()

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, handshake)
		if err != nil {
			return
		}
		defer conn.Close()
		if serve != nil {
			serve(conn)
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A provider that refuses the upgrade says why in the status, and that status is
// the whole diagnosis: a wrong credential and a wrong address look identical
// without it.
func TestARefusedDialCarriesTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	_, err := Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil, testLogger())
	if err == nil {
		t.Fatal("a refused upgrade was reported as a connection")
	}
	if !strings.Contains(err.Error(), "http 401") {
		t.Errorf("error = %q, want the refusal's status in it", err)
	}
}

// Some protocols put the session's identity in the handshake rather than in a
// frame, so what the provider answered with has to survive the dial.
func TestTheHandshakeAnswerIsKept(t *testing.T) {
	endpoint := testServer(t, http.Header{"X-Session-Id": []string{"s-1"}}, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
	})

	conn, err := Dial(t.Context(), endpoint, nil, testLogger())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if got := conn.Header().Get("X-Session-Id"); got != "s-1" {
		t.Errorf("handshake header = %q, want the provider's answer", got)
	}
}

// Once the session is over the socket is gone, and a send has to say so rather
// than write into a closed connection.
func TestASendAfterCloseIsRefused(t *testing.T) {
	received := make(chan string, 1)
	endpoint := testServer(t, nil, func(conn *websocket.Conn) {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			received <- string(data)
		}
	})

	conn, err := Dial(t.Context(), endpoint, nil, testLogger())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Send([]byte(`{"type":"hello"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-received:
		if got != `{"type":"hello"}` {
			t.Errorf("the provider read %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the frame never arrived")
	}

	conn.Close()
	conn.Close() // idempotent

	if err := conn.Send([]byte(`{"type":"late"}`)); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("send after close returned %v, want the closed sentinel", err)
	}
}

// The keepalive is started by the dial and belongs to the socket. A closed
// session that leaves it running leaks one goroutine per call.
func TestTheKeepaliveStopsWithTheSocket(t *testing.T) {
	endpoint := testServer(t, nil, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
	})

	before := keepaliveCount()
	conn, err := Dial(t.Context(), endpoint, nil, testLogger())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := awaitKeepaliveCount(t, before+1); got != before+1 {
		t.Fatalf("%d keepalives are running after the dial, want %d", got, before+1)
	}

	conn.Close()
	if got := awaitKeepaliveCount(t, before); got != before {
		t.Errorf("%d keepalives are still running after the close, want %d", got, before)
	}
}

// A quiet socket is killed by the read deadline, and the keepalive's pong is
// what keeps a socket that is merely idle from looking dead.
func TestAPongPushesTheReadDeadlineOut(t *testing.T) {
	pong := make(chan struct{})
	endpoint := testServer(t, nil, func(conn *websocket.Conn) {
		<-pong
		_ = conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second))
		time.Sleep(400 * time.Millisecond)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"late"}`))
		_, _, _ = conn.ReadMessage()
	})

	conn, err := Dial(t.Context(), endpoint, nil, testLogger())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Forty-five seconds is not a thing a test can wait out, so the deadline is
	// moved in to where one can: the frame below arrives well after it, and can
	// only be read if the pong reset it.
	if err := conn.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	close(pong)

	_, data, err := conn.Receive()
	if err != nil {
		t.Fatalf("receive: %v (the pong did not reset the deadline)", err)
	}
	if string(data) != `{"type":"late"}` {
		t.Errorf("read %q", data)
	}
}

// keepaliveCount is how many goroutines are parked in this package's keepalive.
// A stack diff around the socket's life is enough to catch the leak that
// matters here, and it needs nothing this repository does not already have.
func keepaliveCount() int {
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	return bytes.Count(stacks, []byte("wsconn.(*Conn).keepalive("))
}

func awaitKeepaliveCount(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	count := keepaliveCount()
	for count != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		count = keepaliveCount()
	}
	return count
}
