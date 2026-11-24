// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// judgeEnv is an attempt environment whose store judges leads, with one
// subscription that takes every call.
type judgeEnv struct {
	*attemptEnv
	judges *LeadJudgeStore
	hook   uuid.UUID
}

func openJudged(t *testing.T, isOn bool) *judgeEnv {
	t.Helper()
	e := openAttempts(t)
	e.st.IsLeadJudgeEnabled = isOn
	e.ledger, e.attempts = e.st.Ledger(), e.st.Attempts()
	sub, err := e.st.Webhooks().Create(context.Background(), WebhookSubscriptionWrite{
		Name: "crm", URL: "https://crm.example.com/hook", IsEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &judgeEnv{attemptEnv: e, judges: e.st.LeadJudgements(), hook: sub.SubscriptionID}
}

// call runs one campaign call of the contact: the lead it records (none when
// empty), then its CDR as edit leaves it.
func (e *judgeEnv) call(t *testing.T, ct campaign.Contact, lead campaign.LeadIntent, edit func(*CDR)) campaign.Attempt {
	t.Helper()
	ctx := context.Background()
	start := ny(t, "2026-06-01 10:00")
	a := e.begin(t, ct, start)
	if lead != "" {
		if ok, err := e.attempts.SetLead(ctx, a.CallID, lead, "send the offer"); err != nil || !ok {
			t.Fatalf("set lead: %v %v", ok, err)
		}
	}
	cdr := cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))
	cdr.Legs = append(cdr.Legs, Leg{Kind: "BOT", Label: "bot", DurationSec: 55})
	if edit != nil {
		edit(&cdr)
	}
	if err := e.ledger.InsertCDR(ctx, cdr); err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *judgeEnv) attempt(t *testing.T, callID uuid.UUID) campaign.Attempt {
	t.Helper()
	a, err := e.attempts.Get(context.Background(), callID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// delivery is the call's one delivery: its payload's lead and how long it
// waits from now.
type heldDelivery struct {
	WebhookDelivery
	lead     campaign.LeadIntent
	declared campaign.LeadIntent
	verdict  campaign.LeadVerdict
	reason   string
	dueIn    time.Duration
}

func (e *judgeEnv) delivery(t *testing.T, callID uuid.UUID) heldDelivery {
	t.Helper()
	ctx := context.Background()
	list, err := e.st.Webhooks().Deliveries(ctx, e.hook, 100)
	if err != nil {
		t.Fatal(err)
	}
	var found []WebhookDelivery
	for _, d := range list {
		if d.CallID == callID {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d deliveries for the call, want 1", len(found))
	}
	var body struct {
		LeadIntent         campaign.LeadIntent  `json:"leadIntent"`
		DeclaredLeadIntent campaign.LeadIntent  `json:"declaredLeadIntent"`
		LeadVerdict        campaign.LeadVerdict `json:"leadVerdict"`
		LeadVerdictReason  string               `json:"leadVerdictReason"`
	}
	if err := json.Unmarshal(found[0].Payload, &body); err != nil {
		t.Fatal(err)
	}
	var dbNow time.Time
	if err := e.st.Pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	return heldDelivery{WebhookDelivery: found[0], lead: body.LeadIntent, declared: body.DeclaredLeadIntent,
		verdict: body.LeadVerdict, reason: body.LeadVerdictReason, dueIn: found[0].NextAttemptAt.Sub(dbNow)}
}

func (e *judgeEnv) callbacks(t *testing.T, callID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM callbacks WHERE call_id = $1`, callID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// rewind moves the call's judgement and its deliveries back by d, as if d had
// passed.
func (e *judgeEnv) rewind(t *testing.T, callID uuid.UUID, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	secs := int(d / time.Second)
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_attempts
		SET lead_judge_deadline_at = lead_judge_deadline_at - make_interval(secs => $2) WHERE call_id = $1`, callID, secs); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Pool.Exec(ctx, `UPDATE webhook_deliveries
		SET next_attempt_at = next_attempt_at - make_interval(secs => $2) WHERE call_id = $1`, callID, secs); err != nil {
		t.Fatal(err)
	}
}

func (e *judgeEnv) lastFinished(t *testing.T, callID uuid.UUID) campaign.Finished {
	t.Helper()
	for {
		select {
		case f := <-e.finished:
			if f.CallID == callID {
				return f
			}
		default:
			t.Fatalf("no attempt finished for call %s", callID)
			return campaign.Finished{}
		}
	}
}

func isAbout(got, want time.Duration) bool {
	return got > want-3*time.Second && got <= want+time.Second
}

// Which calls are judged: a campaign call the bot handled alone that recorded
// HIGH or MEDIUM, on a store that judges. The judgement is owed from the CDR's
// own transaction, and the call's delivery is held for it with a payload that
// already says NOT_JUDGED; every other call is delivered at once, as before.
func TestALeadIsOwedAJudgementOnlyWhenItShouldBe(t *testing.T) {
	e := openJudged(t, true)
	_, contacts := e.campaign(t, "Judged", "5", 8, nil)

	t.Run("a bot-only HIGH lead is owed one and held", func(t *testing.T) {
		a := e.call(t, contacts[0], campaign.LeadHigh, nil)
		got := e.attempt(t, a.CallID)
		if got.LeadVerdict != campaign.VerdictPending || got.DeclaredLeadIntent != campaign.LeadHigh || got.LeadIntent != campaign.LeadHigh {
			t.Errorf("attempt = %s declared %s lead %s, want PENDING HIGH HIGH", got.LeadVerdict, got.DeclaredLeadIntent, got.LeadIntent)
		}
		j, err := e.judges.LeadJudgement(context.Background(), a.CallID)
		if err != nil || j.Verdict != campaign.VerdictPending || j.LeadNotes != "send the offer" || j.Language != "" {
			t.Errorf("judgement = %+v, %v", j, err)
		}
		if f := e.lastFinished(t, a.CallID); !f.IsLeadJudgementOwed {
			t.Error("the finish did not say the judgement is owed")
		}
		d := e.delivery(t, a.CallID)
		if !isAbout(d.dueIn, campaign.LeadJudgeHold) {
			t.Errorf("the delivery is due in %s, want about %s", d.dueIn, campaign.LeadJudgeHold)
		}
		if d.verdict != campaign.VerdictNotJudged || d.lead != campaign.LeadHigh || d.declared != campaign.LeadHigh {
			t.Errorf("held payload = %s lead %s declared %s, want NOT_JUDGED HIGH HIGH", d.verdict, d.lead, d.declared)
		}
		if n := e.callbacks(t, a.CallID); n != 0 {
			t.Errorf("%d callbacks filed before the verdict", n)
		}
		var deadlineIn time.Duration
		if err := e.st.Pool.QueryRow(context.Background(), `SELECT extract(epoch FROM lead_judge_deadline_at - now())::float8 * interval '1 second'
			FROM campaign_attempts WHERE call_id = $1`, a.CallID).Scan(&deadlineIn); err != nil {
			t.Fatal(err)
		}
		if !isAbout(deadlineIn, campaign.LeadJudgeDeadline) {
			t.Errorf("the deadline is in %s, want about %s", deadlineIn, campaign.LeadJudgeDeadline)
		}

		// A fuller CDR while it is owed keeps the judgement and the hold.
		before := e.attempt(t, a.CallID)
		cdr := cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, ny(t, "2026-06-01 10:00"), ny(t, "2026-06-01 10:02"))
		cdr.Legs = append(cdr.Legs, Leg{Kind: "BOT", Label: "bot", DurationSec: 115})
		if err := e.ledger.InsertCDR(context.Background(), cdr); err != nil {
			t.Fatal(err)
		}
		if after := e.attempt(t, a.CallID); after.LeadVerdict != campaign.VerdictPending || after.DeclaredLeadIntent != before.DeclaredLeadIntent {
			t.Errorf("after a fuller CDR: %s declared %s", after.LeadVerdict, after.DeclaredLeadIntent)
		}
		if d := e.delivery(t, a.CallID); !isAbout(d.dueIn, campaign.LeadJudgeHold) || d.verdict != campaign.VerdictNotJudged {
			t.Errorf("after a fuller CDR the delivery is due in %s with %s, want still held", d.dueIn, d.verdict)
		}
	})
	t.Run("a MEDIUM lead is owed one", func(t *testing.T) {
		a := e.call(t, contacts[1], campaign.LeadMedium, nil)
		if got := e.attempt(t, a.CallID); got.LeadVerdict != campaign.VerdictPending || got.DeclaredLeadIntent != campaign.LeadMedium {
			t.Errorf("attempt = %s declared %s", got.LeadVerdict, got.DeclaredLeadIntent)
		}
	})
	notJudged := []struct {
		name    string
		contact campaign.Contact
		lead    campaign.LeadIntent
		edit    func(*CDR)
	}{
		{"no lead", contacts[2], "", nil},
		{"LOW", contacts[3], campaign.LeadLow, nil},
		{"DO_NOT_CALL", contacts[4], campaign.LeadDoNotCall, nil},
		{"a call handed to an agent", contacts[5], campaign.LeadHigh, func(c *CDR) {
			agent := uuid.Must(uuid.NewV7())
			c.QueueID, c.AgentIDs, c.PrimaryAgentID, c.TalkSec = &e.queueID, []uuid.UUID{agent}, &agent, 20
			c.Legs = append(c.Legs, Leg{Kind: "QUEUE", Label: "follow-up"}, Leg{Kind: "AGENT", Label: "wei"})
		}},
		{"an unanswered attempt", contacts[6], "", func(c *CDR) {
			*c = cdrOf(c.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, c.StartedAt, c.EndedAt)
		}},
	}
	for _, c := range notJudged {
		t.Run(c.name+" is not judged and is delivered at once", func(t *testing.T) {
			a := e.call(t, c.contact, c.lead, c.edit)
			got := e.attempt(t, a.CallID)
			if got.LeadVerdict != "" || got.DeclaredLeadIntent != "" || got.LeadIntent != c.lead {
				t.Errorf("attempt = %q declared %q lead %q", got.LeadVerdict, got.DeclaredLeadIntent, got.LeadIntent)
			}
			if f := e.lastFinished(t, a.CallID); f.IsLeadJudgementOwed {
				t.Error("the finish says a judgement is owed")
			}
			d := e.delivery(t, a.CallID)
			if d.dueIn > time.Second || d.verdict != "" || d.lead != c.lead {
				t.Errorf("delivery due in %s, verdict %q lead %q", d.dueIn, d.verdict, d.lead)
			}
		})
	}
	t.Run("the HIGH lead of a call handed to an agent files its callback at once", func(t *testing.T) {
		var callID uuid.UUID
		_ = e.st.Pool.QueryRow(context.Background(),
			`SELECT call_id FROM campaign_attempts WHERE campaign_contact_id = $1`, contacts[5].ID).Scan(&callID)
		if n := e.callbacks(t, callID); n != 1 {
			t.Errorf("%d callbacks, want 1", n)
		}
	})
	t.Run("a call that is no campaign's is delivered at once", func(t *testing.T) {
		start := ny(t, "2026-06-01 11:00")
		plain := cdrOf(uuid.Must(uuid.NewV7()), "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))
		if err := e.ledger.InsertCDR(context.Background(), plain); err != nil {
			t.Fatal(err)
		}
		if d := e.delivery(t, plain.CallID); d.dueIn > time.Second || d.lead != "" || d.verdict != "" {
			t.Errorf("delivery due in %s, lead %q verdict %q", d.dueIn, d.lead, d.verdict)
		}
	})
}

// With the judge off nothing changes from before: no judgement is owed, a
// HIGH lead files its callback when the call ends, and the delivery is due at
// once.
func TestWithTheJudgeOffALeadIsRecordedAsBefore(t *testing.T) {
	e := openJudged(t, false)
	_, contacts := e.campaign(t, "Unjudged", "6", 1, nil)
	a := e.call(t, contacts[0], campaign.LeadHigh, nil)
	if got := e.attempt(t, a.CallID); got.LeadVerdict != "" || got.LeadIntent != campaign.LeadHigh {
		t.Errorf("attempt = %q lead %s", got.LeadVerdict, got.LeadIntent)
	}
	if n := e.callbacks(t, a.CallID); n != 1 {
		t.Errorf("%d callbacks, want 1", n)
	}
	if d := e.delivery(t, a.CallID); d.dueIn > time.Second || d.verdict != "" || d.lead != campaign.LeadHigh {
		t.Errorf("delivery due in %s, verdict %q lead %q", d.dueIn, d.verdict, d.lead)
	}
}

// A verdict in time settles everything in one transaction: the attempt's lead,
// the contact's, the callback a lead still HIGH files, and the delivery,
// rebuilt with the final lead and released.
func TestAVerdictInTimeDecidesTheLeadAndReleasesTheDelivery(t *testing.T) {
	e := openJudged(t, true)
	c, contacts := e.campaign(t, "Verdicts", "7", 5, nil)
	for i, k := range []struct {
		declared campaign.LeadIntent
		verdict  campaign.LeadVerdict
		lead     campaign.LeadIntent
		reason   string
	}{
		{campaign.LeadHigh, campaign.VerdictConfirmed, campaign.LeadHigh, "The callee said: sign me up."},
		{campaign.LeadHigh, campaign.VerdictUndecided, campaign.LeadMedium, "The callee only asked the price."},
		{campaign.LeadMedium, campaign.VerdictRefused, campaign.LeadNone, "The callee declined."},
		{campaign.LeadHigh, campaign.VerdictNoEvidence, campaign.LeadNone, "被叫在通话中只说了喂。"},
		{campaign.LeadHigh, campaign.VerdictNotJudged, campaign.LeadHigh, ""},
	} {
		t.Run(string(k.declared)+" judged "+string(k.verdict), func(t *testing.T) {
			ctx := context.Background()
			a := e.call(t, contacts[i], k.declared, nil)
			lead, applied, err := e.judges.ApplyLeadVerdict(ctx, a.CallID, k.verdict, k.reason)
			if err != nil || !applied || lead != k.lead {
				t.Fatalf("apply = %s %v %v, want %s applied", lead, applied, err, k.lead)
			}
			got := e.attempt(t, a.CallID)
			if got.LeadVerdict != k.verdict || got.LeadIntent != k.lead || got.DeclaredLeadIntent != k.declared ||
				got.LeadVerdictReason != k.reason {
				t.Errorf("attempt = %s lead %s declared %s reason %q", got.LeadVerdict, got.LeadIntent, got.DeclaredLeadIntent, got.LeadVerdictReason)
			}
			if ct := e.contact(t, c, contacts[i].ID); ct.LeadIntent != k.lead {
				t.Errorf("contact lead = %s, want %s", ct.LeadIntent, k.lead)
			}
			wantCallbacks := 0
			if k.lead == campaign.LeadHigh {
				wantCallbacks = 1
			}
			if n := e.callbacks(t, a.CallID); n != wantCallbacks {
				t.Errorf("%d callbacks, want %d", n, wantCallbacks)
			}
			d := e.delivery(t, a.CallID)
			if d.dueIn > time.Second || d.verdict != k.verdict || d.lead != k.lead || d.declared != k.declared || d.reason != k.reason {
				t.Errorf("delivery due in %s: %s lead %s declared %s reason %q", d.dueIn, d.verdict, d.lead, d.declared, d.reason)
			}
			if d.Revision != 1 || d.AttemptCount != 0 {
				t.Errorf("delivery revision %d attempts %d, want the held one replaced", d.Revision, d.AttemptCount)
			}
			cdr, err := e.ledger.GetCDR(ctx, a.CallID)
			if err != nil || cdr.LeadVerdict != k.verdict || cdr.LeadIntent != k.lead || cdr.DeclaredLeadIntent != k.declared {
				t.Errorf("GET CDR = %s lead %s declared %s, %v", cdr.LeadVerdict, cdr.LeadIntent, cdr.DeclaredLeadIntent, err)
			}

			// Settled once: a second verdict changes nothing.
			if _, applied, err := e.judges.ApplyLeadVerdict(ctx, a.CallID, campaign.VerdictRefused, "again"); err != nil || applied {
				t.Errorf("a second verdict: applied %v, %v", applied, err)
			}
		})
	}
	if _, _, err := e.judges.ApplyLeadVerdict(context.Background(), uuid.Must(uuid.NewV7()), campaign.VerdictPending, ""); err == nil {
		t.Error("PENDING was accepted as a verdict")
	}
}

// A verdict after the deadline is discarded, the sweep settles the judgement
// as NOT_JUDGED (a HIGH lead's callback filed then), and the held delivery,
// which already said NOT_JUDGED, goes out by itself at its bound, unchanged.
func TestAJudgementPastItsDeadlineIsNotJudgedAndItsDeliveryGoesOutAsHeld(t *testing.T) {
	ctx := context.Background()
	e := openJudged(t, true)
	c, contacts := e.campaign(t, "Late", "8", 2, nil)
	high := e.call(t, contacts[0], campaign.LeadHigh, nil)
	medium := e.call(t, contacts[1], campaign.LeadMedium, nil)
	held := e.delivery(t, high.CallID)

	// Owed and still in time: listed for a worker that starts.
	pending, err := e.judges.PendingLeadJudgements(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if expired, err := e.judges.ExpireLeadJudgements(ctx, 10); err != nil || len(expired) != 0 {
		t.Fatalf("expired before the deadline: %v, %v", expired, err)
	}

	// The deadline passes, and the hold.
	e.rewind(t, high.CallID, campaign.LeadJudgeHold+time.Second)
	e.rewind(t, medium.CallID, campaign.LeadJudgeHold+time.Second)
	if _, applied, err := e.judges.ApplyLeadVerdict(ctx, high.CallID, campaign.VerdictRefused, "late"); err != nil || applied {
		t.Fatalf("a late verdict: applied %v, %v", applied, err)
	}
	if got := e.attempt(t, high.CallID); got.LeadVerdict != campaign.VerdictPending || got.LeadIntent != campaign.LeadHigh {
		t.Errorf("after a late verdict: %s lead %s", got.LeadVerdict, got.LeadIntent)
	}
	if pending, _ := e.judges.PendingLeadJudgements(ctx, 10); len(pending) != 0 {
		t.Errorf("judgements past their deadline are listed as pending: %+v", pending)
	}

	expired, err := e.judges.ExpireLeadJudgements(ctx, 10)
	if err != nil || len(expired) != 2 {
		t.Fatalf("expired = %v, %v", expired, err)
	}
	for _, k := range []struct {
		a         campaign.Attempt
		contact   uuid.UUID
		lead      campaign.LeadIntent
		callbacks int
	}{
		{high, contacts[0].ID, campaign.LeadHigh, 1},
		{medium, contacts[1].ID, campaign.LeadMedium, 0},
	} {
		got := e.attempt(t, k.a.CallID)
		if got.LeadVerdict != campaign.VerdictNotJudged || got.LeadIntent != k.lead || got.LeadVerdictReason != "" {
			t.Errorf("%s: %s lead %s reason %q", k.lead, got.LeadVerdict, got.LeadIntent, got.LeadVerdictReason)
		}
		if ct := e.contact(t, c, k.contact); ct.LeadIntent != k.lead {
			t.Errorf("%s: contact lead %s", k.lead, ct.LeadIntent)
		}
		if n := e.callbacks(t, k.a.CallID); n != k.callbacks {
			t.Errorf("%s: %d callbacks, want %d", k.lead, n, k.callbacks)
		}
	}
	if again, err := e.judges.ExpireLeadJudgements(ctx, 10); err != nil || len(again) != 0 {
		t.Errorf("a second sweep settled %v, %v", again, err)
	}

	// The held delivery was not touched and is now due as it was queued.
	d := e.delivery(t, high.CallID)
	if string(d.Payload) != string(held.Payload) || d.AttemptCount != 0 || d.dueIn > 0 {
		t.Errorf("delivery after the sweep: due in %s, %d attempts, payload changed %v", d.dueIn, d.AttemptCount,
			string(d.Payload) != string(held.Payload))
	}
	claimed, err := e.st.Webhooks().ClaimDue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cl := range claimed {
		found = found || cl.CallID == high.CallID
	}
	if !found {
		t.Error("the held delivery was not handed out at its bound")
	}
}

// A verdict moves the contact's lead only while its call is still the
// contact's latest; the attempt's lead moves either way.
func TestAVerdictLeavesALaterCallsContactAlone(t *testing.T) {
	ctx := context.Background()
	e := openJudged(t, true)
	c, contacts := e.campaign(t, "Later", "9", 1, nil)
	a := e.call(t, contacts[0], campaign.LeadHigh, nil)
	// Another call has settled the contact since.
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET last_attempt_id = $2, last_call_id = $3 WHERE id = $1`,
		contacts[0].ID, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := e.judges.ApplyLeadVerdict(ctx, a.CallID, campaign.VerdictRefused, "Declined."); err != nil || !applied {
		t.Fatalf("apply: %v %v", applied, err)
	}
	if got := e.attempt(t, a.CallID); got.LeadIntent != campaign.LeadNone {
		t.Errorf("attempt lead %s, want NONE", got.LeadIntent)
	}
	if ct := e.contact(t, c, contacts[0].ID); ct.LeadIntent != campaign.LeadHigh {
		t.Errorf("contact lead %s, want the HIGH it was settled with", ct.LeadIntent)
	}
}
