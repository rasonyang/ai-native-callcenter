// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// lockedLog is a log sink the dialer's goroutine writes while the test reads.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// A call the line refuses can be recorded twice: first by the job's -ERR
// CALL_REJECTED as a placement failure, which carries no status and so reads
// REJECTED, then by the channel's own record, which carries the 402 and
// revises the attempt to LINE_REJECTED. The first finish is a customer
// outcome and breaks nothing; the revision must still count towards the trunk
// breaker, or a line refusing every call this way would never pause the
// campaign. The same calls revised to a callee's 603 stay REJECTED and never
// trip it.
func TestARevisionToALineRefusalIsJudgedByTheTrunkBreaker(t *testing.T) {
	const n = 3
	for _, tc := range []struct {
		name     string
		status   int
		want     campaign.Outcome
		wantTrip bool
	}{
		{"revised to a 402", 402, campaign.OutcomeLineRejected, true},
		{"revised to a 603", 603, campaign.OutcomeRejected, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs lockedLog
			r := newRig(t, opts{trunkN: n, log: slog.New(slog.NewTextHandler(&logs, nil))})
			c := r.newCampaign("Line", func(s *camSpec) { s.max = 1 })
			r.importContacts(c, numbers("48", 10))
			r.start(c)

			refuse := func(call placed) {
				t.Helper()
				ctx := context.Background()
				r.clock.Advance(time.Second)
				first := cdrFor(call.Req.CallID, call.Req.To, endRejected, call.At, r.clock.Now())
				first.Tech = map[string]any{"isPlacementFailure": true}
				if err := r.st.Ledger().InsertCDR(ctx, first); err != nil {
					t.Fatal(err)
				}
				r.settle()
				if a := r.attemptOf(c, call); a.Outcome != campaign.OutcomeRejected {
					t.Fatalf("after the placement failure the attempt is %s, want REJECTED", a.Outcome)
				}
				r.clock.Advance(time.Second)
				real := cdrFor(call.Req.CallID, call.Req.To, endRejected, call.At, r.clock.Now())
				real.Tech = map[string]any{store.CDRTechSIPStatus: tc.status, store.CDRTechEndedBy: "PEER"}
				if err := r.st.Ledger().InsertCDR(ctx, real); err != nil {
					t.Fatal(err)
				}
				r.placer.drop(call.Req.CallID)
				r.settle()
				if a := r.attemptOf(c, call); a.Outcome != tc.want || a.SIPStatus != tc.status {
					t.Fatalf("after the channel's record the attempt is %s (%d), want %s (%d)", a.Outcome, a.SIPStatus, tc.want, tc.status)
				}
			}
			for i := 0; i < n-1; i++ {
				refuse(r.placer.liveCalls()[0])
			}
			if got := r.campaignNow(c); got.Status != campaign.StatusRunning {
				t.Fatalf("paused after %d refusals: %s/%s", n-1, got.Status, got.PauseReason)
			}
			refuse(r.placer.liveCalls()[0])
			got := r.campaignNow(c)
			if !tc.wantTrip {
				if got.Status != campaign.StatusRunning {
					t.Fatalf("%d calls revised to %d paused the campaign: %s/%s", n, tc.status, got.Status, got.PauseReason)
				}
				if logs.count("campaign paused by a breaker") != 0 {
					t.Error("a breaker paused the campaign")
				}
				return
			}
			if got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseTrunkFailure {
				t.Fatalf("status %s/%s after %d line refusals, want PAUSED/TRUNK_FAILURE", got.Status, got.PauseReason, n)
			}
			// The call that was already ringing when it tripped ends the same
			// way: the campaign is paused once, and nothing more is dialled.
			placed := r.placer.total()
			for _, call := range r.placer.liveCalls() {
				refuse(call)
			}
			r.tick(time.Hour)
			if got := r.campaignNow(c); got.Status != campaign.StatusPaused || got.PauseReason != campaign.PauseTrunkFailure {
				t.Errorf("status %s/%s after the last refusal, want PAUSED/TRUNK_FAILURE", got.Status, got.PauseReason)
			}
			if r.placer.total() != placed {
				t.Error("a paused campaign dialled")
			}
			if k := logs.count("campaign paused by a breaker"); k != 1 {
				t.Errorf("the campaign was paused by a breaker %d times, want once", k)
			}
			for _, ct := range r.contacts(c) {
				if ct.LastOutcome == campaign.OutcomeLineRejected && (ct.Attempts != 0 || ct.SystemFailures != 1) {
					t.Errorf("contact %s spent %d attempts and %d system failures, want 0 and 1", ct.Number, ct.Attempts, ct.SystemFailures)
				}
			}
		})
	}
}

// attemptOf is the attempt of one placed call.
func (r *rig) attemptOf(c campaign.Campaign, call placed) campaign.Attempt {
	r.t.Helper()
	for _, a := range r.attempts(c) {
		if a.CallID == call.Req.CallID {
			return a
		}
	}
	r.t.Fatalf("no attempt for call %s", call.Req.CallID)
	return campaign.Attempt{}
}
