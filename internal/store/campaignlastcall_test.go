// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// A contact names its latest finished attempt and that attempt's call (design
// 10 section 14): none before the first attempt finishes, and after retries
// the same attempt listCampaignAttempts shows first.
func TestAContactNamesItsLatestAttemptsCall(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Last call", "6", 1, nil)
	ct := contacts[0]
	if got := e.contact(t, c, ct.ID); got.LastCallID != nil || got.LastAttemptID != nil {
		t.Fatalf("before any attempt: %+v", got)
	}

	latestListed := func() campaign.Attempt {
		t.Helper()
		id := ct.ID
		list, _, err := e.cs.ListAttempts(ctx, c.ID, campaign.AttemptFilter{ContactID: &id})
		if err != nil || len(list) == 0 {
			t.Fatalf("attempts: %v %v", list, err)
		}
		return list[0]
	}
	check := func(step string) {
		t.Helper()
		want := latestListed()
		got := e.contact(t, c, ct.ID)
		if got.LastCallID == nil || *got.LastCallID != want.CallID ||
			got.LastAttemptID == nil || *got.LastAttemptID != want.ID {
			t.Errorf("%s: contact names (%v, %v), the latest attempt is %s with call %s",
				step, got.LastAttemptID, got.LastCallID, want.ID, want.CallID)
		}
	}

	t1 := ny(t, "2026-06-01 10:00")
	first := e.begin(t, ct, t1)
	// In flight, nothing has finished yet: still none.
	if got := e.contact(t, c, ct.ID); got.LastCallID != nil {
		t.Errorf("an attempt in flight was named: %+v", got)
	}
	if err := e.ledger.InsertCDR(ctx, placementCDR(first.CallID, "GATEWAY_DOWN", t1, t1.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	check("after a placement failure")

	t2 := ny(t, "2026-06-01 10:10")
	second := e.begin(t, ct, t2)
	if got := e.contact(t, c, ct.ID); got.LastCallID == nil || *got.LastCallID != first.CallID {
		t.Errorf("while the retry is in flight the contact names %v, want the finished %s", got.LastCallID, first.CallID)
	}
	if err := e.ledger.InsertCDR(ctx, cdrOf(second.CallID, "USER_BUSY", CDRStatusBusy, false, t2, t2.Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	check("after a busy retry")

	// The first call's fuller CDR turns up: a revision of an older attempt
	// moves the counters, never the contact's last call.
	if err := e.ledger.InsertCDR(ctx, cdrOf(first.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, t1, t1.Add(35*time.Second))); err != nil {
		t.Fatal(err)
	}
	check("after revising the older attempt")

	t3 := ny(t, "2026-06-01 13:00")
	third := e.begin(t, ct, t3)
	if err := e.ledger.InsertCDR(ctx, cdrOf(third.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, t3, t3.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	check("after the answered call")
	if got := latestListed(); got.CallID != third.CallID {
		t.Errorf("listCampaignAttempts shows %s first, want the third call %s", got.CallID, third.CallID)
	}
}

// A retry can follow a recorded lead only when the first attempt was not seen
// to be answered: reconciliation finished it UNKNOWN with no CDR (design 10
// section 8.2; #264 tracks ending such a contact instead). The link then
// follows the latest call, and the callback the first call filed is left as
// it is: not removed, not filed again.
func TestARetryAfterAHighLeadMovesTheLinkAndKeepsTheFirstCallback(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "High then low", "7", 2, nil)
	t1 := ny(t, "2026-06-01 10:00")
	t2 := ny(t, "2026-06-02 10:00")

	callbacksOf := func(t *testing.T, callID uuid.UUID) []Callback {
		t.Helper()
		rows, err := e.ledger.ListCallbacks(ctx, "", &callID, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	highUnknown := func(t *testing.T, ct campaign.Contact) campaign.Attempt {
		t.Helper()
		a := e.begin(t, ct, t1)
		if ok, err := e.attempts.SetLead(ctx, a.CallID, campaign.LeadHigh, "send the offer"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{
			Outcome: campaign.OutcomeUnknown, HangupCause: "UNKNOWN", EndedAt: t1,
		}); err != nil {
			t.Fatal(err)
		}
		got := e.contact(t, c, ct.ID)
		if got.Status != campaign.ContactActive || got.LeadIntent != campaign.LeadHigh ||
			got.LastCallID == nil || *got.LastCallID != a.CallID {
			t.Fatalf("after the UNKNOWN HIGH call: %+v", got)
		}
		if cbs := callbacksOf(t, a.CallID); len(cbs) != 1 {
			t.Fatalf("the HIGH call filed %d callbacks, want 1", len(cbs))
		}
		return a
	}

	t.Run("HIGH then LOW", func(t *testing.T) {
		ct := contacts[0]
		first := highUnknown(t, ct)
		second := e.begin(t, ct, t2)
		if ok, err := e.attempts.SetLead(ctx, second.CallID, campaign.LeadLow, "not now"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := e.ledger.InsertCDR(ctx, cdrOf(second.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, t2, t2.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
		got := e.contact(t, c, ct.ID)
		if got.LeadIntent != campaign.LeadLow || got.LeadNotes != "not now" ||
			got.LastCallID == nil || *got.LastCallID != second.CallID ||
			got.LastAttemptID == nil || *got.LastAttemptID != second.ID {
			t.Errorf("contact = %+v, want LOW linked to the second call %s", got, second.CallID)
		}
		cbs := callbacksOf(t, first.CallID)
		if len(cbs) != 1 || cbs[0].Status != "OPEN" || cbs[0].Message != "send the offer" {
			t.Errorf("the first call's callback = %+v, want it left OPEN as filed", cbs)
		}
		if cbs := callbacksOf(t, second.CallID); len(cbs) != 0 {
			t.Errorf("the LOW call filed %+v", cbs)
		}
	})

	// The known gap (#264): a retry that records no lead keeps HIGH on the
	// contact while the link names the retry's call, which filed nothing.
	t.Run("HIGH then no lead", func(t *testing.T) {
		ct := contacts[1]
		first := highUnknown(t, ct)
		second := e.begin(t, ct, t2)
		if err := e.ledger.InsertCDR(ctx, cdrOf(second.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, t2, t2.Add(30*time.Second))); err != nil {
			t.Fatal(err)
		}
		got := e.contact(t, c, ct.ID)
		if got.LeadIntent != campaign.LeadHigh || got.LastCallID == nil || *got.LastCallID != second.CallID {
			t.Errorf("contact = %+v, want HIGH kept and the link on the second call", got)
		}
		if cbs := callbacksOf(t, first.CallID); len(cbs) != 1 {
			t.Errorf("the first call's callback = %+v, want it kept", cbs)
		}
		if cbs := callbacksOf(t, *got.LastCallID); len(cbs) != 0 {
			t.Errorf("the second call filed %+v", cbs)
		}
	})
}
