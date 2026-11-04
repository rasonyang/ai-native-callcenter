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

// A call that recorded a lead and then left no trace (the app died, the
// customer hung up, reconciliation finished it UNKNOWN with no CDR) proves the
// customer talked to the bot: the contact ends, it is not retried (design 10
// section 7.2, #264). The lead's callback and the contact's link name that
// call, and nothing is filed twice.
func TestAnUnknownAttemptThatRecordedALeadEndsItsContact(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	t1 := ny(t, "2026-06-01 10:00")

	unknownWith := func(t *testing.T, ct campaign.Contact, intent campaign.LeadIntent) (campaign.Attempt, *campaign.Finished) {
		t.Helper()
		a := e.begin(t, ct, t1)
		if ok, err := e.attempts.SetLead(ctx, a.CallID, intent, "notes for "+string(intent)); err != nil || !ok {
			t.Fatal(ok, err)
		}
		done, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{
			Outcome: campaign.OutcomeUnknown, HangupCause: "UNKNOWN", EndedAt: t1,
		})
		if err != nil || done == nil {
			t.Fatalf("finish: %+v, %v", done, err)
		}
		return a, done
	}
	callbacksOf := func(t *testing.T, callID uuid.UUID) []Callback {
		t.Helper()
		rows, _, err := e.ledger.ListCallbacks(ctx, "", &callID, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	nextAt := func(t *testing.T, id uuid.UUID) *time.Time {
		t.Helper()
		var at *time.Time
		if err := e.st.Pool.QueryRow(ctx, `SELECT next_attempt_at FROM campaign_contacts WHERE id=$1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}

	t.Run("HIGH is DONE with its one callback", func(t *testing.T) {
		c, contacts := e.campaign(t, "Unknown HIGH", "7", 1, nil)
		a, done := unknownWith(t, contacts[0], campaign.LeadHigh)
		if done.ContactStatus != campaign.ContactDone || done.NextAttemptAt != nil || done.SettleReason != campaign.SettleLeadRecorded {
			t.Errorf("finished = %+v", done)
		}
		got := e.contact(t, c, contacts[0].ID)
		if got.Status != campaign.ContactDone || got.LeadIntent != campaign.LeadHigh ||
			got.LastCallID == nil || *got.LastCallID != a.CallID || got.Attempts != 1 {
			t.Errorf("contact = %+v", got)
		}
		if at := nextAt(t, contacts[0].ID); at != nil {
			t.Errorf("next_attempt_at = %v, want NULL", at)
		}
		if cbs := callbacksOf(t, a.CallID); len(cbs) != 1 || cbs[0].Message != "notes for HIGH" {
			t.Errorf("callbacks = %+v, want the one", cbs)
		}
		// A fuller CDR that arrives later runs the routine again: still one
		// callback, still DONE.
		if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, t1, t1.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
		if cbs := callbacksOf(t, a.CallID); len(cbs) != 1 {
			t.Errorf("after the CDR: %d callbacks", len(cbs))
		}
		if got := e.contact(t, c, contacts[0].ID); got.Status != campaign.ContactDone {
			t.Errorf("after the CDR: %+v", got)
		}
	})

	t.Run("the other intents are DONE and file nothing", func(t *testing.T) {
		c, contacts := e.campaign(t, "Unknown others", "8", 3, nil)
		for i, intent := range []campaign.LeadIntent{campaign.LeadMedium, campaign.LeadLow, campaign.LeadNone} {
			a, _ := unknownWith(t, contacts[i], intent)
			got := e.contact(t, c, contacts[i].ID)
			if got.Status != campaign.ContactDone || got.LeadIntent != intent || nextAt(t, contacts[i].ID) != nil {
				t.Errorf("%s: contact = %+v", intent, got)
			}
			if cbs := callbacksOf(t, a.CallID); len(cbs) != 0 {
				t.Errorf("%s filed %+v", intent, cbs)
			}
		}
	})

	t.Run("DO_NOT_CALL is DNC with one list row", func(t *testing.T) {
		c, contacts := e.campaign(t, "Unknown DNC", "9", 1, nil)
		a, done := unknownWith(t, contacts[0], campaign.LeadDoNotCall)
		if done.ContactStatus != campaign.ContactDoNotCall || done.NextAttemptAt != nil || done.SettleReason != campaign.SettleLeadDoNotCall {
			t.Errorf("finished = %+v", done)
		}
		got := e.contact(t, c, contacts[0].ID)
		if got.Status != campaign.ContactDoNotCall || nextAt(t, contacts[0].ID) != nil {
			t.Errorf("contact = %+v", got)
		}
		listed, _, err := e.st.DNC().List(ctx, DNCFilter{Prefix: contacts[0].Number})
		if err != nil || len(listed) != 1 || listed[0].CallID == nil || *listed[0].CallID != a.CallID {
			t.Errorf("dnc = %+v, %v", listed, err)
		}
	})

	t.Run("an UNKNOWN with no lead is still retried", func(t *testing.T) {
		c, contacts := e.campaign(t, "Unknown none recorded", "10", 1, nil)
		a := e.begin(t, contacts[0], t1)
		done, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{Outcome: campaign.OutcomeUnknown, HangupCause: "UNKNOWN", EndedAt: t1})
		if err != nil || done == nil {
			t.Fatal(done, err)
		}
		got := e.contact(t, c, contacts[0].ID)
		at := nextAt(t, contacts[0].ID)
		if got.Status != campaign.ContactActive || at == nil || at.Before(t1.Add(23*time.Hour)) || done.SettleReason != campaign.SettleRetry {
			t.Errorf("contact = %+v, next %v, reason %s; want ACTIVE a day out", got, at, done.SettleReason)
		}
	})

	t.Run("the last chance with a lead is DONE, without one EXHAUSTED", func(t *testing.T) {
		c, contacts := e.campaign(t, "Unknown last chance", "11", 2, nil)
		// Two attempts already spent: the third is the policy's last.
		for _, ct := range contacts {
			if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET attempts = 2 WHERE id = $1`, ct.ID); err != nil {
				t.Fatal(err)
			}
		}
		unknownWith(t, contacts[0], campaign.LeadHigh)
		if got := e.contact(t, c, contacts[0].ID); got.Status != campaign.ContactDone || got.Attempts != 3 {
			t.Errorf("with a lead: %+v, want DONE after 3 attempts", got)
		}
		a := e.begin(t, contacts[1], t1)
		if _, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{Outcome: campaign.OutcomeUnknown, HangupCause: "UNKNOWN", EndedAt: t1}); err != nil {
			t.Fatal(err)
		}
		if got := e.contact(t, c, contacts[1].ID); got.Status != campaign.ContactExhausted || got.Attempts != 3 {
			t.Errorf("without a lead: %+v, want EXHAUSTED after 3 attempts", got)
		}
	})
}
