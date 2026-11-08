// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// records returns each JSON log line as a map.
func (b *lockedBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(b.String(), "\n") {
		if line == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

// An accepted call whose ACK never arrives is released without being reported
// as started, and the line that says so names the call by its domain id (#334).
func TestAnUnackedInviteNeverStartsTheCallAndSaysWhichCall(t *testing.T) {
	logs := &lockedBuffer{}
	callID := uuid.Must(uuid.NewV7())
	uas, hooks, p := startUAS(t, func(c *Config) {
		c.AckTimeout = 200 * time.Millisecond
		c.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	})
	p.aiccLines = []string{"X-AICC-Call-ID: " + callID.String()}

	p.request("INVITE", p.offer())
	p.awaitStatus(200, 2*time.Second)

	deadline := time.Now().Add(3 * time.Second)
	for uas.ActiveCalls() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := uas.ActiveCalls(); got != 0 {
		t.Fatalf("active calls = %d, want the unacked call released", got)
	}
	select {
	case <-hooks.started:
		t.Error("OnCallStarted ran for a call that was never acknowledged")
	case err := <-hooks.failed:
		t.Errorf("OnCallFailed ran for a call that never reached media: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	var found bool
	for _, r := range logs.records(t) {
		if r["msg"] != "no ack for accepted invite" {
			continue
		}
		found = true
		if got := r["callId"]; got != callID.String() {
			t.Errorf("callId = %v, want %s: %v", got, callID, r)
		}
		if got := r["sipCallId"]; got != p.callID {
			t.Errorf("sipCallId = %v, want %s", got, p.callID)
		}
	}
	if !found {
		t.Error("no 'no ack for accepted invite' line")
	}
}

// An ACK that was lost once and arrives after a retransmitted 200 OK, inside
// the timeout, starts the call exactly once.
func TestAnAckAfterARetransmittedAnswerStartsTheCallOnce(t *testing.T) {
	_, hooks, p := startUAS(t, func(c *Config) { c.AckTimeout = 5 * time.Second })
	p.aiccLines = []string{"X-AICC-Call-ID: " + uuid.Must(uuid.NewV7()).String()}

	p.request("INVITE", p.offer())
	p.awaitStatus(200, 2*time.Second)
	// The first ACK is lost; the retransmitted 200 OK is what the peer sees.
	p.awaitStatus(200, 2*time.Second)
	p.request("ACK", "")
	// A further ACK for the same dialog (a second retransmission crossing it).
	p.request("ACK", "")

	select {
	case <-hooks.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the call never started after the late ACK")
	}
	select {
	case <-hooks.started:
		t.Error("the call started twice")
	case <-time.After(300 * time.Millisecond):
	}
}
