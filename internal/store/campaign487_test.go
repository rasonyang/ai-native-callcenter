// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// A far end that ends a ringing INVITE with a 487 of its own, through the
// database: the CDR the assembler writes finishes the attempt in the same
// transaction as NO_ANSWER on maxAttempts, the same facts without ring
// progress stay a system failure, and a CDR already in the ledger finishes a
// DIALING attempt later, read back from jsonb (FinishFromLedger) with the side
// that ended the leg kept as written.
func TestAFarEnds487AfterRingingFinishesTheAttemptAsNoAnswer(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "487", "5", 3, nil)
	start := ny(t, "2026-06-01 10:00")
	end := start.Add(13 * 1e9)

	// The row as just assembled.
	a := e.begin(t, contacts[0], start)
	cdr := cdrOf(a.CallID, "ORIGINATOR_CANCEL", CDRStatusNoAnswer, false, start, end)
	cdr.Tech = map[string]any{CDRTechSIPStatus: 487, CDRTechEndedBy: "PEER",
		CDRTechRingStartedAt: FormatRingStartedAt(start.Add(1e9))}
	if err := e.ledger.InsertCDR(ctx, cdr); err != nil {
		t.Fatal(err)
	}
	got, err := e.attempts.Get(ctx, a.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != campaign.OutcomeNoAnswer || !got.IsTouched {
		t.Errorf("attempt = %+v, want a touched NO_ANSWER", got)
	}
	// It spends maxAttempts, not the system-retry budget.
	if ct := e.contact(t, c, contacts[0].ID); ct.Attempts != 1 || ct.SystemFailures != 0 {
		t.Errorf("contact = %+v, want 1 attempt and no system failure", ct)
	}

	// The same facts without ring progress are still a system failure.
	b := e.begin(t, contacts[1], start)
	quiet := cdrOf(b.CallID, "ORIGINATOR_CANCEL", CDRStatusNoAnswer, false, start, end)
	quiet.Tech = map[string]any{CDRTechSIPStatus: 487, CDRTechEndedBy: "PEER"}
	if err := e.ledger.InsertCDR(ctx, quiet); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.attempts.Get(ctx, b.CallID); got.Outcome != campaign.OutcomeTransient {
		t.Errorf("487 without ring progress = %s, want TRANSIENT", got.Outcome)
	}
	if ct := e.contact(t, c, contacts[1].ID); ct.Attempts != 0 || ct.SystemFailures != 1 {
		t.Errorf("contact = %+v, want no attempt and 1 system failure", ct)
	}

	// A CDR that is already in the ledger when the attempt is finished from it:
	// the tech comes back from jsonb, the status as float64.
	d := e.begin(t, contacts[2], start)
	if _, err := e.st.Pool.Exec(ctx, `INSERT INTO cdrs (call_id, started_at, ended_at, call_type, from_number, to_number, status, hangup_cause, tech)
		VALUES ($1, $2, $3, 'OUTBOUND', '95099', '+8613000000002', 'NO_ANSWER', 'ORIGINATOR_CANCEL', $4::jsonb)`,
		d.CallID, start, end, `{"sipStatus": 487, "endedBy": "PEER", "ringStartedAt": "2026-06-01T14:00:01.000000000Z"}`); err != nil {
		t.Fatal(err)
	}
	if found, err := e.attempts.FinishFromLedger(ctx, d.CallID); err != nil || !found {
		t.Fatalf("FinishFromLedger = %v, %v", found, err)
	}
	if got, _ := e.attempts.Get(ctx, d.CallID); got.Outcome != campaign.OutcomeNoAnswer {
		t.Errorf("finished from the ledger = %s, want NO_ANSWER", got.Outcome)
	}
	var endedBy string
	if err := e.st.Pool.QueryRow(ctx, `SELECT tech->>'endedBy' FROM cdrs WHERE call_id = $1`, a.CallID).Scan(&endedBy); err != nil {
		t.Fatal(err)
	}
	if endedBy != "PEER" {
		t.Errorf("the stored tech.endedBy = %q, want PEER", endedBy)
	}
}
