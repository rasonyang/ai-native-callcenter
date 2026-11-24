// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// A line that refuses a call with a 402 before any ring, through the database:
// the CDR the assembler writes finishes the attempt as LINE_REJECTED, keeps the
// status on the attempt, spends a system retry and no attempt, touches nothing,
// and schedules the retry on the transient backoff. A callee's 603 stays
// REJECTED on maxAttempts and its 24 h interval, its status kept too; a
// refusal with no status keeps none. A CDR already in the ledger, read back
// from jsonb (FinishFromLedger), comes to the same.
func TestALinesRefusalFinishesTheAttemptAsLineRejected(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "402", "6", 4, nil)
	start := ny(t, "2026-06-01 10:00")
	end := start.Add(50 * time.Millisecond)
	policy := campaign.DefaultRetryPolicy()

	a := e.begin(t, contacts[0], start)
	refused := cdrOf(a.CallID, "CALL_REJECTED", CDRStatusNoAnswer, false, start, end)
	refused.Tech = map[string]any{CDRTechSIPStatus: 402, CDRTechEndedBy: "PEER"}
	if err := e.ledger.InsertCDR(ctx, refused); err != nil {
		t.Fatal(err)
	}
	got, err := e.attempts.Get(ctx, a.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != campaign.OutcomeLineRejected || got.IsTouched || got.SIPStatus != 402 || got.HangupCause != "CALL_REJECTED" {
		t.Errorf("attempt = %+v, want an untouched LINE_REJECTED with CALL_REJECTED and status 402", got)
	}
	ct := e.contact(t, c, contacts[0].ID)
	if ct.Attempts != 0 || ct.SystemFailures != 1 || ct.Status != campaign.ContactActive || ct.LastOutcome != campaign.OutcomeLineRejected {
		t.Errorf("contact = %+v, want ACTIVE with no attempt, 1 system failure and last outcome LINE_REJECTED", ct)
	}
	if want := end.Add(policy.TransientBackoff.Wait(1)); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(want) {
		t.Errorf("next attempt = %v, want %v (the end plus the first backoff)", ct.NextAttemptAt, want)
	}

	b := e.begin(t, contacts[1], start)
	declined := cdrOf(b.CallID, "CALL_REJECTED", CDRStatusNoAnswer, false, start, end.Add(3*time.Second))
	declined.Tech = map[string]any{CDRTechSIPStatus: 603, CDRTechEndedBy: "PEER",
		CDRTechRingStartedAt: FormatRingStartedAt(start.Add(time.Second))}
	if err := e.ledger.InsertCDR(ctx, declined); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.attempts.Get(ctx, b.CallID); got.Outcome != campaign.OutcomeRejected || !got.IsTouched || got.SIPStatus != 603 {
		t.Errorf("603 = %+v, want a touched REJECTED with status 603", got)
	}
	ct = e.contact(t, c, contacts[1].ID)
	if ct.Attempts != 1 || ct.SystemFailures != 0 {
		t.Errorf("contact = %+v, want 1 attempt and no system failure", ct)
	}
	if want := end.Add(3 * time.Second).Add(24 * time.Hour); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(want) {
		t.Errorf("next attempt after a 603 = %v, want %v", ct.NextAttemptAt, want)
	}

	d := e.begin(t, contacts[2], start)
	if err := e.ledger.InsertCDR(ctx, cdrOf(d.CallID, "CALL_REJECTED", CDRStatusNoAnswer, false, start, end)); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.attempts.Get(ctx, d.CallID); got.Outcome != campaign.OutcomeRejected || got.SIPStatus != 0 {
		t.Errorf("no status = %+v, want REJECTED and no status", got)
	}

	f := e.begin(t, contacts[3], start)
	if _, err := e.st.Pool.Exec(ctx, `INSERT INTO cdrs (call_id, started_at, ended_at, call_type, from_number, to_number, status, hangup_cause, tech)
		VALUES ($1, $2, $3, 'OUTBOUND', '95099', '+8613000000003', 'NO_ANSWER', 'CALL_REJECTED', $4::jsonb)`,
		f.CallID, start, end, `{"sipStatus": 403, "endedBy": "PEER"}`); err != nil {
		t.Fatal(err)
	}
	if found, err := e.attempts.FinishFromLedger(ctx, f.CallID); err != nil || !found {
		t.Fatalf("FinishFromLedger = %v, %v", found, err)
	}
	if got, _ := e.attempts.Get(ctx, f.CallID); got.Outcome != campaign.OutcomeLineRejected || got.SIPStatus != 403 {
		t.Errorf("finished from the ledger = %+v, want LINE_REJECTED with status 403", got)
	}

	// The listing carries the status the API maps.
	list, _, err := e.cs.ListAttempts(ctx, c.ID, campaign.AttemptFilter{Outcome: campaign.OutcomeLineRejected, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[int]bool{}
	for _, at := range list {
		statuses[at.SIPStatus] = true
	}
	if len(list) != 2 || !statuses[402] || !statuses[403] {
		t.Errorf("listing LINE_REJECTED = %+v, want the 402 and the 403", list)
	}
}

// A revised CDR that only adds the status (same outcome, cause and talk time)
// still writes it: the attempt's status is the one the fullest record gave.
func TestARevisionThatOnlyAddsTheStatusWritesIt(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "603 revised", "7", 1, nil)
	start := ny(t, "2026-06-01 10:00")
	end := start.Add(4 * time.Second)

	a := e.begin(t, contacts[0], start)
	if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "CALL_REJECTED", CDRStatusNoAnswer, false, start, end)); err != nil {
		t.Fatal(err)
	}
	// A record replaces the one before it only when it ends later.
	fuller := cdrOf(a.CallID, "CALL_REJECTED", CDRStatusNoAnswer, false, start, end.Add(time.Second))
	fuller.Tech = map[string]any{CDRTechSIPStatus: 603, CDRTechEndedBy: "PEER"}
	if err := e.ledger.InsertCDR(ctx, fuller); err != nil {
		t.Fatal(err)
	}
	got, _ := e.attempts.Get(ctx, a.CallID)
	if got.Outcome != campaign.OutcomeRejected || got.SIPStatus != 603 {
		t.Errorf("attempt = %+v, want REJECTED with status 603", got)
	}
	if ct := e.contact(t, c, contacts[0].ID); ct.Attempts != 1 || ct.SystemFailures != 0 {
		t.Errorf("contact = %+v, want the call counted once", ct)
	}
}
