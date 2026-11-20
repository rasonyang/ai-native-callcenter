// SPDX-License-Identifier: Apache-2.0

// Package wsconn is the WebSocket a provider client talks over.
//
// It carries frames and knows nothing about what is in them: no protocol event,
// no encoding of one, no vendor. That is what lets clients speaking entirely
// different protocols share one socket's hygiene — the single writer, the
// deadlines, the keepalive, the one close — instead of each growing its own
// half of it.
package wsconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
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
	// DialTimeout bounds session setup. This is inside a phone call, so a
	// provider that has not answered by now is not usable. It is exported
	// because a client waits out the rest of its handshake on the same budget.
	DialTimeout = 3 * time.Second
)

// Conn is one WebSocket to a provider.
//
// There is exactly one writer, guarded by a mutex, and one reader. The session
// never reconnects: provider-side conversation state cannot be recovered, so a
// dropped socket ends the session and the call is routed elsewhere instead of
// silently resuming with a model that has forgotten everything.
type Conn struct {
	conn    *websocket.Conn
	header  http.Header
	writeMu sync.Mutex
	log     *slog.Logger

	closeOnce sync.Once
	done      chan struct{}
}

// Dial opens the socket and starts its keepalive.
func Dial(ctx context.Context, endpoint string, headers http.Header, log *slog.Logger) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()

	dialer := websocket.Dialer{HandshakeTimeout: DialTimeout}
	conn, response, err := dialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		if response != nil {
			return nil, refusal(endpoint, response, err)
		}
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}

	c := &Conn{conn: conn, log: log, done: make(chan struct{})}
	if response != nil {
		c.header = response.Header
	}
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readTimeout))
	})
	go c.keepalive()
	return c, nil
}

// Header is what the provider answered the upgrade with. Some protocols put
// the session's identity there rather than in a frame.
func (c *Conn) Header() http.Header { return c.header }

// HandshakeError is an upgrade the server answered with something other than
// 101: its HTTP status, its headers (where a vendor puts the request id its
// support asks for, and a rate limit if it sends one) and the start of its
// body. What any of it means is the client's business; this package only keeps
// it, because a dial that drops it leaves a quota refusal (429) looking like any
// other failed call (#604).
type HandshakeError struct {
	Endpoint   string
	StatusCode int
	Header     http.Header
	// Body is at most maxRefusalBody bytes of the answer, trimmed.
	Body string
	Err  error
}

// maxRefusalBody bounds what is kept of a refused upgrade's body: enough for a
// vendor's error code and message, never a page.
const maxRefusalBody = 512

// Error is the text a refused dial has always had, so logs and records that
// carry it read as before.
func (e *HandshakeError) Error() string {
	return fmt.Sprintf("dial %s: %v (http %d)", e.Endpoint, e.Err, e.StatusCode)
}

func (e *HandshakeError) Unwrap() error { return e.Err }

// refusal keeps what a server that refused the upgrade answered with. The body
// is already buffered by the WebSocket library (it reads at most 1 KiB of it),
// so reading it does not wait on the network.
func refusal(endpoint string, response *http.Response, err error) *HandshakeError {
	refused := &HandshakeError{
		Endpoint: endpoint, StatusCode: response.StatusCode, Header: response.Header, Err: err,
	}
	if response.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxRefusalBody))
		refused.Body = strings.TrimSpace(string(body))
	}
	return refused
}

// Done closes when the session has ended, however it ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Send writes one already-encoded message. It blocks until the frame is on the
// socket, which is the backpressure: a caller that cannot write cannot go on
// producing.
func (c *Conn) Send(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.done:
		return ErrSessionClosed
	default:
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// ErrSessionClosed is returned by every send once the session has ended. Its
// wording is the caller's, not this package's: what has ended, as far as anyone
// upstream is concerned, is the provider session.
var ErrSessionClosed = errors.New("provider: session closed")

// Receive reads the next frame and returns it as it arrived. What it means is
// the protocol's business, and this package has no opinion about it.
func (c *Conn) Receive() (messageType int, data []byte, err error) {
	messageType, data, err = c.conn.ReadMessage()
	if err != nil {
		return 0, nil, err
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return 0, nil, err
	}
	return messageType, data, nil
}

func (c *Conn) keepalive() {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.writeMu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			err := c.conn.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// Close ends the session. It is idempotent.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		// A close frame lets the provider release the session promptly; if it
		// cannot be sent, closing the socket says the same thing less politely.
		c.writeMu.Lock()
		_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = c.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.writeMu.Unlock()
		_ = c.conn.Close()
	})
}
