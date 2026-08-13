// SPDX-License-Identifier: Apache-2.0

package esl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSwitch is a minimal Event Socket server: it performs the greeting and
// auth handshake, answers commands, and can push events on demand.
type fakeSwitch struct {
	ln       net.Listener
	password string

	conns chan *fakeConn
}

type fakeConn struct {
	net.Conn
	reader *bufio.Reader
	cmds   chan string
}

func newFakeSwitch(t *testing.T, password string) *fakeSwitch {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
	fc := &fakeConn{Conn: conn, reader: bufio.NewReader(conn), cmds: make(chan string, 16)}

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
		case strings.HasPrefix(cmd, "api "):
			fc.cmds <- cmd
			body := "+OK " + strings.TrimPrefix(cmd, "api ")
			fmt.Fprintf(conn, "Content-Type: api/response\nContent-Length: %d\n\n%s", len(body), body)
		case strings.HasPrefix(cmd, "bgapi "):
			fc.cmds <- cmd
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

func TestDialAuthenticates(t *testing.T) {
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	client, err := Dial(ctx, f.addr(), "ClueCon")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer client.Close()
	f.waitConn(t)
}

func TestDialRejectsBadPassword(t *testing.T) {
	f := newFakeSwitch(t, "ClueCon")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := Dial(ctx, f.addr(), "wrong")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Dial() error = %v, want ErrAuthFailed", err)
	}
}

func TestAPIAndBgAPI(t *testing.T) {
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

func TestCommandsMatchRepliesInOrder(t *testing.T) {
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

func TestEventsAreDelivered(t *testing.T) {
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
	link := NewLink("127.0.0.1:1", "ClueCon", nil)
	if link.IsUp() {
		t.Error("IsUp() = true before Run")
	}
	if _, err := link.API("status"); !errors.Is(err, ErrDown) {
		t.Errorf("API() = %v, want ErrDown", err)
	}
}
