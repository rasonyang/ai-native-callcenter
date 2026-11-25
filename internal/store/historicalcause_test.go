// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// PROVIDER_SESSION_EXPIRED was the hangup cause of a call whose voice
// provider's session ran out (the removed Gemini provider). No code produces it
// any more, but past calls keep it in the ledger and on their campaign
// attempts. hangup_cause is free text in both tables, with no CHECK and no
// enum behind it, so those rows must keep reading: the CDR listing, the
// campaign's attempt listing and counters, and the ledger reports.
//
// Several hundred campaign calls, a third of them ended that way, mixed with
// ordinary causes.
func TestRowsEndedByTheRemovedSessionExpiredCauseStillRead(t *testing.T) {
	const (
		legacyCause = "PROVIDER_SESSION_EXPIRED"
		total       = 300
	)
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Legacy", "5", total, nil)
	origin := ny(t, "2026-06-01 10:00")

	wantCauses := map[string]int{}
	wantAnswered := 0
	for i := 0; i < total; i++ {
		started := origin.Add(time.Duration(i) * time.Second)
		ended := started.Add(90 * time.Second)
		attempt := e.begin(t, contacts[i], started)

		var cdr CDR
		switch i % 3 {
		case 0:
			cdr = cdrOf(attempt.CallID, legacyCause, CDRStatusFailed, true, started, ended)
		case 1:
			cdr = cdrOf(attempt.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, started, ended)
		default:
			cdr = cdrOf(attempt.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, started, started.Add(30*time.Second))
		}
		wantCauses[cdr.HangupCause]++
		if cdr.Status == CDRStatusAnswered {
			wantAnswered++
		}
		if err := e.ledger.InsertCDR(ctx, cdr); err != nil {
			t.Fatalf("InsertCDR %d (%s): %v", i, cdr.HangupCause, err)
		}
	}
	if wantCauses[legacyCause] != total/3 {
		t.Fatalf("seeded %d legacy rows, want %d", wantCauses[legacyCause], total/3)
	}

	// The CDR listing pages through every row and returns the legacy cause
	// verbatim.
	got := map[string]int{}
	seen := map[uuid.UUID]bool{}
	for offset := 0; ; offset += 100 {
		listed, err := e.ledger.ListCDRs(ctx, CDRFilter{
			From: origin.Add(-time.Hour), To: origin.Add(time.Hour),
			CampaignID: &c.ID, Limit: 100, Offset: offset,
		})
		if err != nil {
			t.Fatalf("ListCDRs offset %d: %v", offset, err)
		}
		page, count := listed.Items, listed.Total
		if count != total {
			t.Fatalf("ListCDRs total = %d, want %d", count, total)
		}
		for _, row := range page {
			if seen[row.CallID] {
				t.Fatalf("call %s listed twice", row.CallID)
			}
			seen[row.CallID] = true
			got[row.HangupCause]++
		}
		if len(page) < 100 {
			break
		}
	}
	if len(seen) != total {
		t.Errorf("ListCDRs returned %d distinct rows, want %d", len(seen), total)
	}
	for cause, want := range wantCauses {
		if got[cause] != want {
			t.Errorf("CDR listing holds %d rows with cause %s, want %d", got[cause], cause, want)
		}
	}
	failed, err := e.ledger.ListCDRs(ctx, CDRFilter{
		From: origin.Add(-time.Hour), To: origin.Add(time.Hour), Status: CDRStatusFailed, Limit: 200,
	})
	if err != nil || int(failed.Total) != total/3 || len(failed.Items) != total/3 {
		t.Errorf("failed listing = %d rows, total %d, err %v; want %d", len(failed.Items), failed.Total, err, total/3)
	}

	// The campaign's attempt listing keeps the cause, and the attempts were
	// stored as failures of the bot side.
	seenAttempts, legacyAttempts := 0, 0
	for offset := 0; ; offset += 100 {
		page, count, err := e.cs.ListAttempts(ctx, c.ID, campaign.AttemptFilter{Limit: 100, Offset: offset})
		if err != nil {
			t.Fatalf("ListAttempts offset %d: %v", offset, err)
		}
		if count != total {
			t.Fatalf("ListAttempts total = %d, want %d", count, total)
		}
		for _, a := range page {
			seenAttempts++
			if a.HangupCause == legacyCause {
				legacyAttempts++
				if a.Outcome != campaign.OutcomeBotFailed {
					t.Errorf("attempt %s with cause %s has outcome %s, want %s",
						a.ID, legacyCause, a.Outcome, campaign.OutcomeBotFailed)
				}
			}
		}
		if len(page) < 100 {
			break
		}
	}
	if seenAttempts != total || legacyAttempts != total/3 {
		t.Errorf("attempts read = %d, with the legacy cause = %d; want %d and %d",
			seenAttempts, legacyAttempts, total, total/3)
	}
	byOutcome, outcomeCount, err := e.cs.ListAttempts(ctx, c.ID,
		campaign.AttemptFilter{Outcome: campaign.OutcomeBotFailed, Limit: 200})
	if err != nil || int(outcomeCount) != total/3 || len(byOutcome) != total/3 {
		t.Errorf("BOT_FAILED attempts = %d, total %d, err %v; want %d", len(byOutcome), outcomeCount, err, total/3)
	}

	// The campaign itself and its contact counters still read.
	read, err := e.cs.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get campaign: %v", err)
	}
	if read.Contacts.Total != total {
		t.Errorf("campaign contacts total = %d, want %d", read.Contacts.Total, total)
	}

	// The ledger reports aggregate the same rows without choking on the cause.
	overview, err := e.ledger.ReportOverview(ctx, origin.Add(-time.Hour), origin.Add(time.Hour), nil)
	if err != nil {
		t.Fatalf("ReportOverview: %v", err)
	}
	if overview.TotalCalls != total || overview.AnsweredCalls != wantAnswered {
		t.Errorf("overview = %d calls, %d answered; want %d and %d",
			overview.TotalCalls, overview.AnsweredCalls, total, wantAnswered)
	}
	daily, err := e.ledger.ReportDaily(ctx, origin.Add(-24*time.Hour), origin.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("ReportDaily: %v", err)
	}
	calls := 0
	for _, day := range daily {
		calls += day.TotalCalls
	}
	if calls != total {
		t.Errorf("daily report counts %d calls, want %d", calls, total)
	}
}
