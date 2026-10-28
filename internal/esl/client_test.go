// SPDX-License-Identifier: Apache-2.0

package esl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeSwitch is a minimal Event Socket server: it performs the greeting and
// auth handshake, answers commands, and can push events on demand.
type fakeSwitch struct {
	ln       net.Listener
	password string

	conns chan *fakeConn

	// stall, when set, is waited on before a bgapi command is answered: the
	// switch reads the command and then sits on it, as a loaded FreeSWITCH
	// does. Nothing after it on the connection is read until it is closed.
	stall chan struct{}

	// deafen, when set, makes the first connection stop reading after it
	// has answered the event subscription, until deafen is closed: a switch
	// whose socket buffers fill up. It then reads what the client managed to
	// write, to the end of the stream, into the connection's heard.
	deafen   chan struct{}
	isDeafed atomic.Bool

	// hangUpOnBgapi is how many bgapi commands are read and answered by
	// closing the connection, with no reply.
	hangUpOnBgapi atomic.Int32
}

type fakeConn struct {
	net.Conn
	reader *bufio.Reader
	cmds   chan string

	// heard is everything a deafened connection had been sent, closed once
	// it is read.
	heard     []byte
	heardDone chan struct{}
}

func newFakeSwitch(t *testing.T, password string) *fakeSwitch {
	return newFakeSwitchBuffered(t, password, 0)
}

// newFakeSwitchBuffered is a fake switch whose connections have a receive
// buffer of about rcvBuf bytes (0 leaves the system's), set on the listening
// socket so it is in force from the handshake on.
func newFakeSwitchBuffered(t *testing.T, password string, rcvBuf int) *fakeSwitch {
	t.Helper()
	lc := net.ListenConfig{}
	if rcvBuf > 0 {
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBuf)
			}); err != nil {
				return err
			}
			return serr
		}
	}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSwitch{ln: ln, password: password, conns: make(chan *fakeConn, 4)}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSwitch) addr() string { return f.ln.Addr().String() }

func (f *fakeSwitch) serve(conn net.Conn) {
	fc := &fakeConn{Conn: conn, reader: bufio.NewReader(conn), cmds: make(chan string, 16),
		heardDone: make(chan struct{})}

	fmt.Fprint(conn, "Content-Type: auth/request\n\n")

	for {
		line, err := fc.reader.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		if cmd == "" {
			continue
		}

		switch {
		case strings.HasPrefix(cmd, "auth "):
			if strings.TrimPrefix(cmd, "auth ") == f.password {
				fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
				f.conns <- fc
			} else {
				fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: -ERR invalid\n\n")
				return
			}
		case strings.HasPrefix(cmd, "event "):
			fc.cmds <- cmd
			fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: +OK event listener enabled plain\n\n")
			if f.deafen != nil && f.isDeafed.CompareAndSwap(false, true) {
				<-f.deafen
				fc.heard, _ = io.ReadAll(fc.reader)
				close(fc.heardDone)
				return
			}
		case strings.HasPrefix(cmd, "api "):
			fc.cmds <- cmd
			body := "+OK " + strings.TrimPrefix(cmd, "api ")
			fmt.Fprintf(conn, "Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body)
		case strings.HasPrefix(cmd, "bgapi "):
			fc.cmds <- cmd
			if f.hangUpOnBgapi.Add(-1) >= 0 {
				conn.Close()
				return
			}
			if f.stall != nil {
				<-f.stall
			}
			fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: +OK Job-UUID: job-123\n\n")
		}
	}
}

// pushEvent sends a plain event with the given headers.
func (fc *fakeConn) pushEvent(headers string) {
	fmt.Fprintf(fc.Conn, "Content-Type: text/event-plain\nContent-Length: %d\n\n%s",
		len(headers), headers)
}

func (f *fakeSwitch) waitConn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case fc := <-f.conns:
		return fc
	case <-time.After(2 * time.Second):
		t.Fatal("no client connected")
		return nil
	}
}

func TestDialRejectsBadPassword(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := Dial(ctx, f.addr(), "wrong")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Dial() error = %v, want ErrAuthFailed", err)
	}
}

func TestAPIAndBgAPI(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	f.waitConn(t)

	body, err := client.API("status")
	if err != nil {
		t.Fatalf("API() error = %v", err)
	}
	if !strings.Contains(body, "status") {
		t.Errorf("API() = %q, want the echoed command", body)
	}

	job, err := client.BgAPI("originate sofia/gateway/x/1000 &park()")
	if err != nil {
		t.Fatalf("BgAPI() error = %v", err)
	}
	if job != "job-123" {
		t.Errorf("BgAPI() job = %q, want job-123", job)
	}
}

// A command is one line ended by a blank one. A value with a line break in it
// would end the command early, and a blank line inside it would have the
// switch run the rest as a command of its own and answer twice, so every
// later reply would be matched to the wrong command. Such a command is refused
// before a byte of it is written, and the link goes on as if it had never
// been asked.
func TestACommandWithALineBreakIsNeverWritten(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	fc := f.waitConn(t)

	for _, cmd := range []string{
		"uuid_setvar chan-1 aicc_bot_summary wants a refund\n\nbgapi status",
		"uuid_setvar chan-1 aicc_bot_summary wants a refund\nContent-Type: x",
		"uuid_setvar chan-1 aicc_bot_summary wants a refund\r\n\r\napi status",
		"uuid_setvar chan-1 aicc_bot_summary wants\x00a refund",
	} {
		if _, err := client.API(cmd); !errors.Is(err, ErrUnframable) {
			t.Errorf("API(%q) error = %v, want ErrUnframable", cmd, err)
		}
		if _, err := client.BgAPI(cmd); !errors.Is(err, ErrUnframable) {
			t.Errorf("BgAPI(%q) error = %v, want ErrUnframable", cmd, err)
		}
	}

	// The next command is the first the switch hears, and its reply is its own.
	body, err := client.API("status")
	if err != nil {
		t.Fatalf("API(status) error = %v", err)
	}
	if body != "+OK status" {
		t.Errorf("API(status) = %q, want its own reply", body)
	}
	select {
	case got := <-fc.cmds:
		if got != "api status" {
			t.Errorf("the switch first heard %q, want api status", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the switch heard nothing")
	}
	select {
	case got := <-fc.cmds:
		t.Errorf("the switch heard %q as well", got)
	default:
	}
}

func TestCommandsMatchRepliesInOrder(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	f.waitConn(t)

	// Concurrent commands must each receive their own reply body.
	const n = 8
	results := make(chan string, n)
	for i := range n {
		go func() {
			body, err := client.API(fmt.Sprintf("uuid_exists uuid-%d", i))
			if err != nil {
				results <- "error: " + err.Error()
				return
			}
			results <- body
		}()
	}

	seen := make(map[string]bool, n)
	for range n {
		select {
		case body := <-results:
			if seen[body] {
				t.Errorf("duplicate reply body %q: replies were mismatched", body)
			}
			seen[body] = true
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for replies")
		}
	}
}

// A stalled switch reads a bgapi command and answers it late. The caller is
// told the outcome is unknown (ErrTimeout), never that the link is down, and
// the late reply is matched to the command that gave up on it: the next
// command still gets its own reply (#221).
func TestATimedOutCommandIsAmbiguousAndKeepsRepliesAligned(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	f.stall = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	fc := f.waitConn(t)
	client.timeout = 100 * time.Millisecond

	_, err = client.BgAPI("originate {origination_uuid=leg-1}sofia/gateway/x/1000 &park()")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("BgAPI() error = %v, want ErrTimeout", err)
	}
	if errors.Is(err, ErrDown) {
		t.Fatal("a timeout must not read as a link that is down")
	}
	select {
	case cmd := <-fc.cmds:
		if !strings.HasPrefix(cmd, "bgapi originate") {
			t.Fatalf("the switch read %q", cmd)
		}
	case <-time.After(time.Second):
		t.Fatal("the command never reached the switch")
	}

	// The switch recovers: it answers the originate, then the next command.
	close(f.stall)
	client.timeout = 2 * time.Second
	body, err := client.API("uuid_exists leg-2")
	if err != nil {
		t.Fatalf("API() after the stall error = %v", err)
	}
	if body != "+OK uuid_exists leg-2" {
		t.Errorf("API() = %q: the late bgapi reply was handed to the next command", body)
	}
}

func TestEventsAreDelivered(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	fc := f.waitConn(t)

	if err := client.Subscribe("CHANNEL_ANSWER", "CUSTOM sofia::register"); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	fc.pushEvent("Event-Name: CHANNEL_ANSWER\n" +
		"Unique-ID: 019ff973-6b32-7557-b7c4-11b3fdb692f0\n" +
		"Caller-Destination-Number: 9664\n" +
		"Event-Date-Timestamp: 1786596518731948\n")

	select {
	case ev := <-client.Events():
		if ev.Name() != "CHANNEL_ANSWER" {
			t.Errorf("Name() = %q, want CHANNEL_ANSWER", ev.Name())
		}
		if ev.UniqueID() != "019ff973-6b32-7557-b7c4-11b3fdb692f0" {
			t.Errorf("UniqueID() = %q", ev.UniqueID())
		}
		if got := ev.Timestamp().UTC().Format("2006-01-02 15:04:05"); got != "2026-08-13 04:48:38" {
			t.Errorf("Timestamp() = %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}
}

func TestClosedClientFailsCommands(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	f.waitConn(t)
	client.Close()

	if _, err := client.API("status"); !errors.Is(err, ErrDown) {
		t.Errorf("API() after Close error = %v, want ErrDown", err)
	}
}

func TestLinkReconnects(t *testing.T) {
	t.Parallel()
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link := NewLink(f.addr(), "ClueCon", []string{"CHANNEL_ANSWER"})
	connects := make(chan struct{}, 4)
	link.OnConnect(func(context.Context) { connects <- struct{}{} })

	go link.Run(ctx)

	// First connection.
	fc := f.waitConn(t)
	select {
	case <-connects:
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect hook did not fire")
	}
	if !link.IsUp() {
		t.Error("IsUp() = false after connect")
	}

	// Kill the connection; the link must come back on its own.
	fc.Conn.Close()

	select {
	case <-connects:
	case <-time.After(5 * time.Second):
		t.Fatal("link did not reconnect")
	}
	f.waitConn(t)
	if !link.IsUp() {
		t.Error("IsUp() = false after reconnect")
	}
}

func TestLinkReportsDownWhenDisconnected(t *testing.T) {
	t.Parallel()
	link := NewLink("127.0.0.1:1", "ClueCon", nil)
	if link.IsUp() {
		t.Error("IsUp() = true before Run")
	}
	if _, err := link.API("status"); !errors.Is(err, ErrDown) {
		t.Errorf("API() = %v, want ErrDown", err)
	}
}
