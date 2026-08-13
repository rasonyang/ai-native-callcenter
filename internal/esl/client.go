// SPDX-License-Identifier: Apache-2.0

package esl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors returned by the client.
var (
	// ErrDown reports that the link is not currently usable.
	ErrDown = errors.New("esl link down")
	// ErrAuthFailed reports a rejected password.
	ErrAuthFailed = errors.New("esl authentication failed")
)

// commandTimeout bounds a single api/bgapi round trip. FreeSWITCH answers
// these promptly; a longer wait means the switch is in trouble and callers
// should fail fast rather than pile up.
const commandTimeout = 10 * time.Second

// Client is a single Event Socket connection in inbound mode.
//
// One goroutine owns the read side and demultiplexes replies from events;
// commands are serialized by a write mutex and matched to replies in FIFO
// order, which is what the protocol guarantees.
type Client struct {
	conn   net.Conn
	reader *textproto.Reader

	writeMu sync.Mutex

	// pending holds reply channels in command order.
	pendingMu sync.Mutex
	pending   []chan reply

	events chan *Event

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

type reply struct {
	body string
	err  error
}

// Dial connects, authenticates and returns a ready client.
func Dial(ctx context.Context, addr, password string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial esl: %w", err)
	}

	c := &Client{
		conn:   conn,
		reader: textproto.NewReader(bufio.NewReaderSize(conn, 32<<10)),
		events: make(chan *Event, 1024),
		closed: make(chan struct{}),
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	// FreeSWITCH greets with auth/request before anything else.
	greeting, err := c.readMessage()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read greeting: %w", err)
	}
	if greeting.contentType != "auth/request" {
		conn.Close()
		return nil, fmt.Errorf("unexpected greeting %q", greeting.contentType)
	}

	if _, err := fmt.Fprintf(conn, "auth %s\n\n", password); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send auth: %w", err)
	}
	authReply, err := c.readMessage()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read auth reply: %w", err)
	}
	if !strings.HasPrefix(authReply.replyText, "+OK") {
		conn.Close()
		return nil, ErrAuthFailed
	}

	_ = conn.SetDeadline(time.Time{})
	go c.readLoop()
	return c, nil
}

// Events returns the stream of received events. The channel closes when the
// link ends.
func (c *Client) Events() <-chan *Event { return c.events }

// Subscribe asks for the named events in plain format. Names may include
// "CUSTOM sofia::register"-style subclass subscriptions.
func (c *Client) Subscribe(names ...string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := c.command("event plain " + strings.Join(names, " "))
	return err
}

// API runs a blocking api command and returns its response body.
func (c *Client) API(cmd string) (string, error) { return c.command("api " + cmd) }

// BgAPI runs a command in the background and returns the job UUID.
func (c *Client) BgAPI(cmd string) (string, error) {
	body, err := c.command("bgapi " + cmd)
	if err != nil {
		return "", err
	}
	// The reply text carries "+OK Job-UUID: <uuid>".
	if _, uuid, ok := strings.Cut(body, "Job-UUID: "); ok {
		return strings.TrimSpace(uuid), nil
	}
	return "", nil
}

// Close ends the link.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// Done reports link termination.
func (c *Client) Done() <-chan struct{} { return c.closed }

// command writes cmd and waits for its reply in FIFO order.
func (c *Client) command(cmd string) (string, error) {
	select {
	case <-c.closed:
		return "", ErrDown
	default:
	}

	ch := make(chan reply, 1)

	c.writeMu.Lock()
	// Registering the waiter and writing must happen together, or a reply
	// could be matched to the wrong command.
	c.pendingMu.Lock()
	c.pending = append(c.pending, ch)
	c.pendingMu.Unlock()

	_, err := fmt.Fprintf(c.conn, "%s\n\n", cmd)
	c.writeMu.Unlock()

	if err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}

	select {
	case r := <-ch:
		return r.body, r.err
	case <-time.After(commandTimeout):
		return "", fmt.Errorf("esl command timed out: %s", cmd)
	case <-c.closed:
		return "", ErrDown
	}
}

// message is one framed protocol message.
type message struct {
	contentType string
	replyText   string
	body        string
}

func (c *Client) readMessage() (message, error) {
	headers, err := c.reader.ReadMIMEHeader()
	if err != nil {
		return message{}, err
	}

	msg := message{
		contentType: headers.Get("Content-Type"),
		replyText:   headers.Get("Reply-Text"),
	}

	if cl := headers.Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil {
			return message{}, fmt.Errorf("bad content-length %q: %w", cl, err)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.reader.R, buf); err != nil {
			return message{}, fmt.Errorf("read body: %w", err)
		}
		msg.body = string(buf)
	}
	return msg, nil
}

// readLoop demultiplexes replies and events until the link ends.
func (c *Client) readLoop() {
	defer close(c.events)
	defer c.Close()

	for {
		msg, err := c.readMessage()
		if err != nil {
			c.failPending(err)
			return
		}

		switch msg.contentType {
		case "command/reply", "api/response":
			body := msg.body
			if body == "" {
				body = msg.replyText
			}
			c.deliverReply(reply{body: body})

		case "text/event-plain":
			ev := parseEventBody(msg.body)
			select {
			case c.events <- ev:
			case <-c.closed:
				return
			default:
				// The consumer is too slow. Dropping the oldest event keeps
				// the link responsive; the reconciler repairs state.
				select {
				case <-c.events:
				default:
				}
				select {
				case c.events <- ev:
				default:
				}
			}

		case "text/disconnect-notice":
			c.failPending(ErrDown)
			return
		}
	}
}

func (c *Client) deliverReply(r reply) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if len(c.pending) == 0 {
		return
	}
	ch := c.pending[0]
	c.pending = c.pending[1:]
	ch <- r
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for _, ch := range c.pending {
		ch <- reply{err: err}
	}
	c.pending = nil
}
