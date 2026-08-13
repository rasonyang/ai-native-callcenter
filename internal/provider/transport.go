// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Connection hygiene the reference implementations lacked. Without it a
// half-open socket looks like a model that has simply stopped talking, and the
// caller sits in silence until they hang up.
const (
	// keepaliveInterval is how often a ping goes out.
	keepaliveInterval = 15 * time.Second
	// readTimeout fails the session when nothing at all arrives. It is reset
	// by any frame, including a pong.
	readTimeout = 45 * time.Second
	// writeTimeout bounds a single send. A socket that cannot absorb a frame
	// of audio within this is not going to recover.
	writeTimeout = 5 * time.Second
	// dialTimeout bounds session setup. This is inside a phone call, so a
	// provider that has not answered by now is not usable.
	dialTimeout = 3 * time.Second
)

// wireEvent is the provider protocol's event shape.
//
// It is decoded into a struct rather than a map because audio deltas arrive
// tens of times a second per call, and decoding those into maps was measurable
// allocation churn in the reference implementation.
type wireEvent struct {
	Type string `json:"type"`

	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	ItemID     string `json:"item_id"`

	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	Item     *wireItem     `json:"item"`
	Response *wireResponse `json:"response"`
	Error    *wireError    `json:"error"`
}

type wireItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type wireResponse struct {
	ID     string     `json:"id"`
	Status string     `json:"status"`
	Usage  *wireUsage `json:"usage"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type wireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
}

func (e *wireError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// transport is one WebSocket to a provider.
//
// There is exactly one writer, guarded by a mutex, and one reader. The session
// never reconnects: provider-side conversation state cannot be recovered, so a
// dropped socket ends the session and the call is routed elsewhere instead of
// silently resuming with a model that has forgotten everything.
type transport struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	log     *slog.Logger

	closeOnce sync.Once
	done      chan struct{}
}

func dial(ctx context.Context, endpoint string, headers http.Header, log *slog.Logger) (*transport, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout}
	conn, response, err := dialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("dial %s: %w (http %d)", endpoint, err, response.StatusCode)
		}
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}

	t := &transport{conn: conn, log: log, done: make(chan struct{})}
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readTimeout))
	})
	go t.keepalive()
	return t, nil
}

// send writes one JSON message.
func (t *transport) send(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbound event: %w", err)
	}
	return t.sendRaw(data)
}

// sendRaw writes an already-encoded message, for the audio path where
// marshalling would copy every frame again for nothing.
func (t *transport) sendRaw(data []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	select {
	case <-t.done:
		return errSessionClosed
	default:
	}
	if err := t.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return t.conn.WriteMessage(websocket.TextMessage, data)
}

// errSessionClosed is returned by every send once the session has ended.
var errSessionClosed = errors.New("provider: session closed")

// receive reads and decodes the next event.
func (t *transport) receive() (*wireEvent, []byte, error) {
	messageType, data, err := t.conn.ReadMessage()
	if err != nil {
		return nil, nil, err
	}
	if err := t.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return nil, nil, err
	}
	if messageType != websocket.TextMessage {
		return nil, data, nil
	}

	var event wireEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, data, fmt.Errorf("decode inbound event: %w", err)
	}
	return &event, data, nil
}

func (t *transport) keepalive() {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.writeMu.Lock()
			_ = t.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := t.conn.WriteMessage(websocket.PingMessage, nil)
			t.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (t *transport) close() {
	t.closeOnce.Do(func() {
		close(t.done)
		// A close frame lets the provider release the session promptly; if it
		// cannot be sent, closing the socket says the same thing less politely.
		t.writeMu.Lock()
		_ = t.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = t.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		t.writeMu.Unlock()
		_ = t.conn.Close()
	})
}
