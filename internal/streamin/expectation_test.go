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

// isExpecting reports whether the watchdog is still armed for a key.
func (s *Server) isExpecting(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[key] != nil
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
	// testServer's Transcripts never has an actor, which is exactly the
	// production case: a transcript actor is created only by the bot path.
	s := testServer(t)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	claim := Claim{
		CallID: uuid.New(), PartyID: uuid.New(), AgentID: uuid.New(),
		Channel: "agent-chan", Expires: time.Now().Add(time.Minute),
	}
	key := claim.CallID.String() + "|" + claim.Channel

	s.Expect(claim)
	if !s.isExpecting(key) {
		t.Fatal("the attach armed no watchdog; the rest of this proves nothing")
	}

	dial := "ws://" + s.Addr() + StreamPath + "?t=" + url.QueryEscape(s.Token(claim))
	conn, _, err := websocket.DefaultDialer.Dial(dial, nil)
	if err != nil {
		t.Fatalf("the tap could not reach the ingest at all: %v", err)
	}
	defer conn.Close()

	waitUntil(t, func() bool { return !s.isExpecting(key) },
		"the refusal to disarm the watchdog")
}
