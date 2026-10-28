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
	// ErrDown reports that the link is not currently usable and that the
	// command was not sent: none of its bytes reached the connection, so the
	// switch cannot run it. It is safe to retry.
	ErrDown = errors.New("esl link down")
	// ErrAuthFailed reports a rejected password.
	ErrAuthFailed = errors.New("esl authentication failed")
	// ErrTimeout reports a command whose bytes, all or some, reached the
	// connection and whose reply did not arrive: not within the command
	// timeout, or not before the link closed, or the write itself did not
	// finish in time. Unlike ErrDown it does not mean the switch did not take
	// the command: a stalled switch executes what it read when it recovers, and
	// a bgapi job it accepted runs whatever happens to the socket. For a bgapi
	// command the outcome is unknown, and the caller must treat it so.
	ErrTimeout = errors.New("esl command timed out")
	// ErrUnframable reports a command refused before any of it was written,
	// because it holds a line break or a NUL. A command is one line ended by a
	// blank one: a line break inside it would end it early and have the
	// switch read the rest as headers, or as a command of its own, and every
	// reply after it would be matched to the wrong command. Like ErrDown, the
	// switch cannot run it; unlike ErrDown, retrying will not help.
	ErrUnframable = errors.New("esl command holds a line break")
)

// CommandTimeout bounds a single api/bgapi round trip, from the moment the
// command is issued: its turn to write, the write and the reply. FreeSWITCH
// answers these promptly; a longer wait means the switch is in trouble and
// callers should fail fast rather than pile up.
const CommandTimeout = 10 * time.Second

// Client is a single Event Socket connection in inbound mode.
//
// One goroutine owns the read side and demultiplexes replies from events;
// commands are serialized by a write turn and matched to replies in FIFO
// order, which is what the protocol guarantees.
type Client struct {
	conn   net.Conn
	reader *textproto.Reader

	// writeTurn is held (one token) by the command being written. It is a
	// channel rather than a mutex so that waiting for it is bounded by the
	// command's deadline like everything else.
	writeTurn chan struct{}

	// pending holds reply channels in command order.
	pendingMu sync.Mutex
	pending   []chan reply

	events chan *Event

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error

	// timeout overrides CommandTimeout; tests shorten it.
	timeout time.Duration
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
		conn:      conn,
		reader:    textproto.NewReader(bufio.NewReaderSize(conn, 32<<10)),
		writeTurn: make(chan struct{}, 1),
		events:    make(chan *Event, 1024),
		closed:    make(chan struct{}),
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
//
// One deadline, counted from the call, bounds everything the caller waits on:
// its turn to write (behind a writer the switch has stopped reading from), the
// write itself (the kernel buffers are full when the switch stops reading) and
// the reply. No caller waits longer than the command timeout, whatever the
// switch does.
//
// The error says whether the switch can have the command. ErrDown: none of it
// was written, so the switch cannot run it. ErrTimeout: some or all of it was
// written and no reply came, so the switch may run it.
func (c *Client) command(cmd string) (string, error) {
	if strings.ContainsAny(cmd, "\r\n\x00") {
		return "", fmt.Errorf("%w: %q", ErrUnframable, abbrev(cmd))
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = CommandTimeout
	}
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case c.writeTurn <- struct{}{}:
	case <-c.closed:
		return "", ErrDown
	case <-timer.C:
		// The writer ahead is held up by a switch that reads slowly or not at
		// all; its own deadline bounds it, and a write it cannot finish closes
		// the link. This command was never written.
		return "", fmt.Errorf("%w: no turn to write within %s: %s", ErrDown, timeout, abbrev(cmd))
	}
	// The link may have closed while this caller waited for its turn: a writer
	// ahead of it gave up and closed it. Nothing of this command is written.
	select {
	case <-c.closed:
		<-c.writeTurn
		return "", ErrDown
	default:
	}

	// Registering the waiter and writing must happen together, under the
	// turn, or a reply could be matched to the wrong command.
	ch := make(chan reply, 1)
	c.pendingMu.Lock()
	c.pending = append(c.pending, ch)
	c.pendingMu.Unlock()

	_ = c.conn.SetWriteDeadline(deadline)
	frame := cmd + "\n\n"
	n, err := io.WriteString(c.conn, frame)
	if err != nil {
		// The link is closed before the turn is handed on, in every case. A
		// command cut off mid-write leaves the stream unframed: the switch
		// would read the next command as the rest of this one. And a write
		// that could not place a single byte in a full kernel buffer for the
		// whole timeout is a switch that has stopped reading. Closing also
		// fails the waiter registered above (and every earlier one), so no
		// later reply can be matched to a command that was never written;
		// the Link reconnects.
		c.Close()
		<-c.writeTurn
		if n == 0 {
			// The kernel accepted none of the bytes, so none can reach the
			// switch: provably not sent.
			return "", fmt.Errorf("%w: write command: %v", ErrDown, err)
		}
		return "", fmt.Errorf("%w: write cut off after %d of %d bytes, the switch may have read the command: %v: %s",
			ErrTimeout, n, len(frame), err, abbrev(cmd))
	}
	<-c.writeTurn

	select {
	case r := <-ch:
		return r.body, r.err
	case <-timer.C:
		// The waiter stays queued: the late reply, if it comes, is matched to
		// it and dropped, so the replies of later commands stay aligned.
		return "", fmt.Errorf("%w: %s", ErrTimeout, abbrev(cmd))
	case <-c.closed:
		// A reply that arrived as the link closed still counts.
		select {
		case r := <-ch:
			if r.err == nil {
				return r.body, nil
			}
		default:
		}
		return "", fmt.Errorf("%w: the link closed after the command was written: %s", ErrTimeout, abbrev(cmd))
	}
}

// abbrev is a command as an error quotes it: a command's variables can be
// kilobytes long, and they do not help a log.
func abbrev(cmd string) string {
	const limit = 200
	if len(cmd) > limit {
		return cmd[:limit] + "…"
	}
	return cmd
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
			c.failPending(fmt.Errorf("%w: the link closed before the reply: %v", ErrTimeout, err))
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
			c.failPending(fmt.Errorf("%w: the switch disconnected before the reply", ErrTimeout))
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

// failPending answers every waiter when the link ends. Each of them wrote its
// command, so err wraps ErrTimeout: the switch may have read it.
func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for _, ch := range c.pending {
		ch <- reply{err: err}
	}
	c.pending = nil
}
