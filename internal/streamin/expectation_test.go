// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// isExpecting reports whether the watchdog is still armed for an attach.
func (s *Server) isExpecting(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[token] != nil
}

// isExpectingAny reports whether any attach is waiting to connect.
func (s *Server) isExpectingAny() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

// hasArrived reports whether a stream dialled back before its attach armed an
// expectation, and is still waiting to be claimed by it.
func (s *Server) hasArrived(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, here := s.arrivals[token]
	return here
}

// expireExpectations fires every armed connect-grace check now, so a test can
// assert what it reports without waiting twelve seconds for a real timer. It
// does what the timer does, in the same order: stop it, then hand the check the
// expectation it was armed for.
func (s *Server) expireExpectations() {
	s.mu.Lock()
	type armed struct {
		token string
		exp   *expectation
	}
	live := make([]armed, 0, len(s.pending))
	for token, exp := range s.pending {
		live = append(live, armed{token: token, exp: exp})
	}
	s.mu.Unlock()
	for _, a := range live {
		a.exp.timer.Stop()
		s.giveUpOn(a.token, a.exp)
	}
}

// tokenOf is the token a tap put on the URL it gave the switch.
func tokenOf(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("t")
}

// A stream that dialled back and was turned away here is not also reported,
// twelve seconds later, as one that never connected.
//
// Those are different faults with different causes — the second means the
// switch never called back at all, which is a network or module problem — and
// an operator handed both accounts of one event goes looking for something
// that is not there. Seen live on 2026-08-26: three click-to-dial calls, each
// refused for want of a transcript actor and each reported three separate
// times.
//
// Asserted on the expectation rather than on log output, and without waiting
// out connectGrace: what the fix does is disarm the watchdog, and the log line
// is downstream of that.
func TestARefusedStreamIsNotAlsoReportedAsNeverConnected(t *testing.T) {
	t.Parallel()
	// testServer's Transcripts never has an actor, which is exactly the
	// production case: a transcript actor is created only by the bot path.
	s := testServer(t)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	claim := Claim{
		CallID: uuid.Must(uuid.NewV7()), PartyID: uuid.Must(uuid.NewV7()), AgentID: uuid.Must(uuid.NewV7()),
		Channel: "agent-chan", Expires: time.Now().Add(time.Minute),
	}
	token := s.Token(claim)

	s.Expect(token, claim)
	if !s.isExpecting(token) {
		t.Fatal("the attach armed no watchdog; the rest of this proves nothing")
	}

	dial := "ws://" + s.Addr() + StreamPath + "?t=" + url.QueryEscape(token)
	conn, _, err := websocket.DefaultDialer.Dial(dial, nil)
	if err != nil {
		t.Fatalf("the tap could not reach the ingest at all: %v", err)
	}
	defer conn.Close()

	waitUntil(t, func() bool { return !s.isExpecting(token) },
		"the refusal to disarm the watchdog")
}

// hasSessionOn reports whether a stream is registered for the call's channel.
// A stream's session is dropped only after its end handler has returned, so
// waiting for this to go false also waits for the tap to have been told.
func (s *Server) hasSessionOn(callID uuid.UUID, channelID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[callID.String()+"|"+channelID] != nil
}

// A refusal can beat the attach's `+OK` just as an acceptance can (#287): the
// module dials back while `start` is still in flight and is turned away — here
// for want of a transcript actor. It did dial back, so the Expect that follows
// must not arm a watchdog that reports it twelve seconds later as a stream
// that never connected.
func TestAStreamRefusedBeforeItsExpectationIsNotReportedMissing(t *testing.T) {
	t.Parallel()
	s := testServer(t) // no transcript actor for any call
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	claim := Claim{
		CallID: uuid.Must(uuid.NewV7()), PartyID: uuid.Must(uuid.NewV7()), AgentID: uuid.Must(uuid.NewV7()),
		Channel: "agent-chan", Expires: time.Now().Add(time.Minute),
	}
	token := s.Token(claim)

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws://"+s.Addr()+StreamPath+"?t="+url.QueryEscape(token), nil)
	if err != nil {
		t.Fatalf("the tap could not reach the ingest at all: %v", err)
	}
	defer conn.Close()
	// The ingest closes the socket only after it has recorded the refusal, so
	// the read returning is the moment the refusal is on the books.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("a stream with no transcript actor was not refused; the rest of this proves nothing")
	}

	s.Expect(token, claim)
	if s.isExpecting(token) {
		t.Error("Expect armed a watchdog for a stream that had already dialled back and been refused")
	}
}
