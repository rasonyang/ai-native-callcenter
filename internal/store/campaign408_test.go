// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// The whole path of #576 through the database: the CDR the assembler writes
// finishes the attempt in the same transaction, and a CDR already in the
// ledger finishes a DIALING attempt later, read back from jsonb (FinishFromLedger).
func TestACarriers408AfterRingingFinishesTheAttemptAsNoAnswer(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "408", "5", 3, nil)
	start := ny(t, "2026-06-01 10:00")
	end := start.Add(26 * 1e9)
	tech := map[string]any{CDRTechSIPStatus: 408, CDRTechRingStartedAt: FormatRingStartedAt(start.Add(2 * 1e9))}

	// The row as just assembled.
	a := e.begin(t, contacts[0], start)
	cdr := cdrOf(a.CallID, "RECOVERY_ON_TIMER_EXPIRE", CDRStatusNoAnswer, false, start, end)
	cdr.Tech = tech
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
	quiet := cdrOf(b.CallID, "RECOVERY_ON_TIMER_EXPIRE", CDRStatusNoAnswer, false, start, end)
	quiet.Tech = map[string]any{CDRTechSIPStatus: 408}
	if err := e.ledger.InsertCDR(ctx, quiet); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.attempts.Get(ctx, b.CallID); got.Outcome != campaign.OutcomeTransient {
		t.Errorf("408 without ring progress = %s, want TRANSIENT", got.Outcome)
	}

	// A CDR that is already in the ledger when the attempt is finished from it:
	// the tech comes back from jsonb as float64.
	d := e.begin(t, contacts[2], start)
	if _, err := e.st.Pool.Exec(ctx, `INSERT INTO cdrs (call_id, started_at, ended_at, call_type, from_number, to_number, status, hangup_cause, tech)
		VALUES ($1, $2, $3, 'OUTBOUND', '95099', '+8613000000002', 'NO_ANSWER', 'RECOVERY_ON_TIMER_EXPIRE', $4::jsonb)`,
		d.CallID, start, end, `{"sipStatus": 408, "ringStartedAt": "2026-06-01T02:00:02.000000000Z"}`); err != nil {
		t.Fatal(err)
	}
	if found, err := e.attempts.FinishFromLedger(ctx, d.CallID); err != nil || !found {
		t.Fatalf("FinishFromLedger = %v, %v", found, err)
	}
	if got, _ := e.attempts.Get(ctx, d.CallID); got.Outcome != campaign.OutcomeNoAnswer {
		t.Errorf("finished from the ledger = %s, want NO_ANSWER", got.Outcome)
	}
}
