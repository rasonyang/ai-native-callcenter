// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// eslSwitch is an Event Socket server for the placement path end to end: the
// real esl.Link and telephony.Adapter in front of it, the service behind them.
// It answers every api command +OK and every bgapi with a Job-UUID, except
// the originates it is told to answer by closing the connection.
type eslSwitch struct {
	ln net.Listener

	mu sync.Mutex
	// hangUps is how many originates are read and then answered by closing
	// the connection, with no reply: the switch may have started the job.
	hangUps int
	// cmds is every command read, on every connection, in order.
	cmds []string

	conns chan net.Conn
}

func newESLSwitch(t *testing.T) *eslSwitch {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &eslSwitch{ln: ln, conns: make(chan net.Conn, 8)}
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

func (f *eslSwitch) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	fmt.Fprint(conn, "Content-Type: auth/request\n\n")
	job := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		if cmd == "" {
			continue
		}
		switch {
		case strings.HasPrefix(cmd, "auth "):
			fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: +OK accepted\n\n")
			f.conns <- conn
		case strings.HasPrefix(cmd, "event "):
			fmt.Fprint(conn, "Content-Type: command/reply\nReply-Text: +OK event listener enabled plain\n\n")
		case strings.HasPrefix(cmd, "bgapi "):
			f.mu.Lock()
			f.cmds = append(f.cmds, cmd)
			isHangUp := strings.HasPrefix(cmd, "bgapi originate ") && f.hangUps > 0
			if isHangUp {
				f.hangUps--
			}
			f.mu.Unlock()
			if isHangUp {
				return
			}
			job++
			fmt.Fprintf(conn, "Content-Type: command/reply\nReply-Text: +OK Job-UUID: job-%d\n\n", job)
		case strings.HasPrefix(cmd, "api "):
			f.mu.Lock()
			f.cmds = append(f.cmds, cmd)
			f.mu.Unlock()
			fmt.Fprintf(conn, "Content-Type: api/response\nContent-Length: 3\n\n+OK")
		}
	}
}

func (f *eslSwitch) waitConn(t *testing.T) net.Conn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the link never connected")
		return nil
	}
}

// commands lists the commands read so far that start with prefix.
func (f *eslSwitch) commands(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func pushESLEvent(t *testing.T, conn net.Conn, headers string) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "Content-Type: text/event-plain\nContent-Length: %d\n\n%s", len(headers), headers); err != nil {
		t.Fatalf("push event: %v", err)
	}
}

var originatedLeg = regexp.MustCompile(`origination_uuid=([0-9a-f-]{36})`)

type eslPlacement struct {
	s        *Service
	sw       *eslSwitch
	ledger   *recordingLedger
	link     *esl.Link
	connects chan struct{}
}

// eslService is a service placing calls through a real esl.Link to sw, with
// the switch's events dispatched to it as the server's dispatch loop does.
func eslService(t *testing.T) eslPlacement {
	t.Helper()
	sw := newESLSwitch(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	link := esl.NewLink(sw.ln.Addr().String(), "ClueCon", []string{"CHANNEL_CREATE", "CHANNEL_ANSWER", "BACKGROUND_JOB"})
	connects := make(chan struct{}, 8)
	link.OnConnect(func(context.Context) { connects <- struct{}{} })
	go link.Run(ctx)

	flowID := uuid.New()
	dids := fakeDIDs{
		{Number: "95099", Language: "en", FlowID: &flowID, IsEnabled: true, AllowOutbound: true},
		{Number: "95011", Language: "en", IsEnabled: true, AllowOutbound: true, IsDefaultOutbound: true},
	}
	ledger := &recordingLedger{cdrs: map[uuid.UUID][]store.CDR{}}
	s := New(Config{EndpointFormat: "sofia/gateway/pstn_gateway/%s", AnswerWindow: time.Minute, RingTimeout: 30 * time.Second},
		telephony.NewAdapter(link, "aicc.test"), dids, provisioned("1008"),
		func(_ context.Context, id uuid.UUID) (bool, error) { return len(ledger.get(id)) > 0, nil },
		func(uuid.UUID) bool { return false }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.limiter = NewLimiter(1_000_000)
	s.SetFailureLedger(ledger)

	go func() {
		for raw := range link.Events() {
			if ev, ok := telephony.Normalize(raw); ok {
				s.HandleSwitchEvent(ev)
			}
		}
	}()
	select {
	case <-connects:
	case <-time.After(5 * time.Second):
		t.Fatal("the link never came up")
	}
	return eslPlacement{s: s, sw: sw, ledger: ledger, link: link, connects: connects}
}

// The switch reads the originate and the connection closes before its reply
// (#302). A bgapi job the switch accepted runs whatever happens to the socket,
// so the placement is unconfirmed (the API answers 202), never SWITCH_DOWN.
// On the reconnected link the leg appears and answers: it is adopted and goes
// where it was meant to go, a retry with the callId dials nothing, and no
// placement-failure row is written beside the call's own record.
func TestAnOriginateWhoseLinkClosesAfterTheWriteIsUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		place func(*Service, uuid.UUID) error
		// done is the command the answered leg is sent on with.
		done func(leg string) string
		did  string
	}{
		{
			name: "AI_OUTBOUND",
			place: func(s *Service, id uuid.UUID) error {
				_, err := s.PlaceAI(context.Background(), AIDialRequest{CallID: id, To: "18688886660", DIDNumber: "95099",
					UserData: map[string]string{"ticket": "T-302"}})
				return err
			},
			done: func(leg string) string { return "api uuid_transfer " + leg + " 'm:^:bridge:" },
			did:  "95099",
		},
		{
			name: "click-to-dial",
			place: func(s *Service, id uuid.UUID) error {
				_, err := s.Dial(context.Background(), AgentDialRequest{CallID: id, AgentExtension: "1008", To: "18688886666"})
				return err
			},
			done: func(leg string) string { return "api uuid_transfer " + leg + " 18688886666 XML aicc" },
			did:  "95011",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := eslService(t)
			p.sw.mu.Lock()
			p.sw.hangUps = 1
			p.sw.mu.Unlock()
			<-p.sw.conns // the first connection, already up
			callID := uuid.Must(uuid.NewV7())

			err := c.place(p.s, callID)
			if !errors.Is(err, ErrPlacementUnconfirmed) || !errors.Is(err, esl.ErrTimeout) || errors.Is(err, esl.ErrDown) {
				t.Fatalf("placement error = %v, want unconfirmed (esl.ErrTimeout), not down", err)
			}
			if !p.s.IsPlacing(callID) || p.s.LinesInUse(c.did) != 1 || p.s.PendingCount() != 1 {
				t.Errorf("placing %v, lines %d, pending %d; want everything held",
					p.s.IsPlacing(callID), p.s.LinesInUse(c.did), p.s.PendingCount())
			}

			select {
			case <-p.connects:
			case <-time.After(5 * time.Second):
				t.Fatal("the link did not reconnect")
			}
			conn := p.sw.waitConn(t)

			if err := c.place(p.s, callID); !errors.Is(err, ErrAlreadyPlaced) {
				t.Errorf("retry = %v, want ErrAlreadyPlaced", err)
			}
			originates := p.sw.commands("bgapi originate ")
			if len(originates) != 1 {
				t.Fatalf("%d originates reached the switch, want 1", len(originates))
			}
			m := originatedLeg.FindStringSubmatch(originates[0])
			if m == nil {
				t.Fatalf("no leg in %q", originates[0])
			}
			leg := m[1]

			// The switch had started the job: its leg comes up on the new
			// connection, rings and answers.
			pushESLEvent(t, conn, "Event-Name: CHANNEL_CREATE\nUnique-ID: "+leg+"\nCall-Direction: outbound\n")
			pushESLEvent(t, conn, "Event-Name: CHANNEL_ANSWER\nUnique-ID: "+leg+"\nCall-Direction: outbound\n")

			// Its job result, matched by the leg its command named: the
			// originate has let go, so the answered leg is sent on.
			pushESLEvent(t, conn, "Event-Name: BACKGROUND_JOB\nJob-UUID: job-late\nJob-Command: originate\n"+
				"Job-Command-Arg: {origination_uuid="+leg+"}sofia/x\nContent-Length: 41\n\n+OK "+leg+"\n")
			eventually(t, "the answered leg to be sent on", func() bool { return len(p.sw.commands(c.done(leg))) == 1 })
			if hangups := p.sw.commands("api uuid_kill "); len(hangups) != 0 {
				t.Errorf("hangups = %v: the adopted leg was taken for a late one", hangups)
			}
			eventually(t, "the placement to be settled", func() bool { return !p.s.IsPlacing(callID) })
			if rows := p.ledger.get(callID); len(rows) != 0 {
				t.Errorf("placement-failure rows %+v for a call that exists", rows)
			}
			if n := p.s.LinesInUse(c.did); n != 1 {
				t.Errorf("lines in use = %d, want the call's line held", n)
			}
		})
	}
}

// The link is down when the originate is issued: nothing reaches the switch,
// so it is SWITCH_DOWN (esl.ErrDown), and the placement is undone at once:
// the line and the business data given back, nothing armed or tracked.
func TestAnOriginateNotSentIsDownAndUndone(t *testing.T) {
	t.Parallel()
	p := eslService(t)
	conn := <-p.sw.conns
	p.sw.ln.Close() // no reconnect
	conn.Close()
	eventually(t, "the link to go down", func() bool { return !p.link.IsUp() })

	for _, place := range []func(uuid.UUID) error{
		func(id uuid.UUID) error {
			_, err := p.s.PlaceAI(context.Background(), AIDialRequest{CallID: id, To: "18688886660", DIDNumber: "95099",
				UserData: map[string]string{"ticket": "T-302"}})
			return err
		},
		func(id uuid.UUID) error {
			_, err := p.s.Dial(context.Background(), AgentDialRequest{CallID: id, AgentExtension: "1008", To: "18688886666"})
			return err
		},
	} {
		callID := uuid.Must(uuid.NewV7())
		err := place(callID)
		if !errors.Is(err, esl.ErrDown) || errors.Is(err, ErrPlacementUnconfirmed) {
			t.Errorf("placement error = %v, want esl.ErrDown and not unconfirmed", err)
		}
		if p.s.IsPlacing(callID) || p.s.PendingCount() != 0 || p.s.Data().CallData(callID) != nil {
			t.Errorf("placing %v, pending %d, data %v: the placement was not undone",
				p.s.IsPlacing(callID), p.s.PendingCount(), p.s.Data().CallData(callID))
		}
		if rows := p.ledger.get(callID); len(rows) != 0 {
			t.Errorf("rows %+v for a call never sent", rows)
		}
	}
	for _, did := range []string{"95099", "95011"} {
		if n := p.s.LinesInUse(did); n != 0 {
			t.Errorf("lines in use on %s = %d, want none", did, n)
		}
	}
	if n := len(p.sw.commands("bgapi originate ")); n != 0 {
		t.Errorf("%d originates reached the switch", n)
	}
}
