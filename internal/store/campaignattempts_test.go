// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// The campaigns of these tests dial in New York, every day from 09:00 to
// 18:00, inside outer hours of 08:00-21:00. 2026-06-01 is a Monday.
var attemptWindow = campaign.CallingWindow{
	Days:   []string{"MON", "TUE", "WED", "THU", "FRI", "SAT", "SUN"},
	Ranges: []campaign.TimeRange{{Start: "09:00", End: "18:00"}},
}

type attemptEnv struct {
	st       *Store
	cs       *CampaignStore
	attempts *AttemptStore
	ledger   *LedgerStore
	didID    uuid.UUID
	queueID  uuid.UUID
	finished chan campaign.Finished
}

func openAttempts(t *testing.T) *attemptEnv {
	t.Helper()
	ctx := context.Background()
	st, cs := openCampaigns(t)
	st.OutboundHours = dayrange.Range{Start: 8 * 60, End: 21 * 60}
	env := &attemptEnv{st: st, cs: cs, finished: make(chan campaign.Finished, 4096)}
	st.OnAttemptFinished = func(f campaign.Finished) { env.finished <- f }
	env.didID, _ = seedDID(t, st, "95099", false)
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO queues (id, name, ext_number) VALUES ($1, 'follow-up', '7001') RETURNING id`,
		uuid.Must(uuid.NewV7())).Scan(&env.queueID); err != nil {
		t.Fatalf("queue: %v", err)
	}
	env.attempts = st.Attempts()
	env.ledger = st.Ledger()
	return env
}

// campaign makes a RUNNING campaign with n contacts (numbers from prefix).
func (e *attemptEnv) campaign(t *testing.T, name, prefix string, n int, edit func(*campaign.Campaign)) (campaign.Campaign, []campaign.Contact) {
	t.Helper()
	ctx := context.Background()
	in := newCampaign(e.didID, name)
	in.TimeZone = "America/New_York"
	in.CallingWindow = attemptWindow
	in.FollowUpQueueID = &e.queueID
	if edit != nil {
		edit(&in)
	}
	c, err := e.cs.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.cs.ImportContacts(ctx, c.ID, contactsFor(prefix, n)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cs.Start(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	contacts, _, err := e.cs.ListContacts(ctx, c.ID, campaign.ContactFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return c, contacts
}

func (e *attemptEnv) contact(t *testing.T, c campaign.Campaign, id uuid.UUID) campaign.Contact {
	t.Helper()
	list, _, err := e.cs.ListContacts(context.Background(), c.ID, campaign.ContactFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, ct := range list {
		if ct.ID == id {
			return ct
		}
	}
	t.Fatalf("contact %s not found", id)
	return campaign.Contact{}
}

func ny(t *testing.T, local string) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("America/New_York")
	v, err := time.ParseInLocation("2006-01-02 15:04", local, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// cdrOf is a campaign call's ledger row: answered or not, ended at the time.
func cdrOf(callID uuid.UUID, cause, status string, answered bool, startedAt, endedAt time.Time) CDR {
	cdr := CDR{
		CallID: callID, StartedAt: startedAt, EndedAt: endedAt, CallType: "OUTBOUND",
		FromNumber: "95099", ToNumber: "+8613000000000", DID: "95099",
		Status: status, HangupCause: cause,
		Legs: []Leg{{Kind: "TRUNK", Label: "customer"}},
	}
	if answered {
		cdr.AnsweredAt = startedAt.Add(5 * time.Second)
		cdr.TalkSec = int(endedAt.Sub(startedAt).Seconds()) - 5
	}
	return cdr
}

func placementCDR(callID uuid.UUID, cause string, startedAt, endedAt time.Time) CDR {
	cdr := cdrOf(callID, cause, CDRStatusFailed, false, startedAt, endedAt)
	cdr.Tech = map[string]any{"isPlacementFailure": true}
	return cdr
}

func (e *attemptEnv) begin(t *testing.T, ct campaign.Contact, at time.Time) campaign.Attempt {
	t.Helper()
	a, err := e.attempts.Begin(context.Background(), ct.ID, at)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return a
}

func TestTheWriteAheadRowTakesTheContactOutOfThePool(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Write ahead", "0", 2, nil)

	if contacts[0].NextAttemptAt == nil {
		t.Fatal("a started campaign's contacts should be due")
	}
	now := ny(t, "2026-06-01 10:00")
	a := e.begin(t, contacts[0], now)
	if a.State != campaign.AttemptDialing || a.CallID == uuid.Nil || a.LegID == uuid.Nil || a.CallID == a.LegID ||
		a.Number != contacts[0].Number || a.Outcome != "" || a.FinishedAt != nil || !a.StartedAt.Equal(now) {
		t.Errorf("attempt = %+v", a)
	}
	if got := e.contact(t, c, contacts[0].ID); got.NextAttemptAt != nil || got.Status != campaign.ContactActive {
		t.Errorf("contact after begin = %+v, want ACTIVE with no due time", got)
	}
	// The same number again, while the first is in flight.
	if _, err := e.attempts.Begin(ctx, contacts[0].ID, now); !errors.Is(err, campaign.ErrNumberInFlight) {
		t.Errorf("second begin = %v, want ErrNumberInFlight", err)
	}
	var n int
	_ = e.st.Pool.QueryRow(ctx, `SELECT count(*) FROM campaign_attempts`).Scan(&n)
	if n != 1 {
		t.Errorf("%d attempt rows after a refused begin, want 1", n)
	}
	if err := e.attempts.SetJob(ctx, a.CallID, "job-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := e.attempts.Get(ctx, a.CallID); err != nil || got.OriginateJobID != "job-1" {
		t.Errorf("get = %+v %v", got, err)
	}
	if _, err := e.attempts.Get(ctx, uuid.New()); !errors.Is(err, campaign.ErrNoAttempt) {
		t.Errorf("unknown call: %v", err)
	}
	// A contact that is not ACTIVE cannot be begun.
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET status='DONE', next_attempt_at=NULL WHERE id=$1`, contacts[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.attempts.Begin(ctx, contacts[1].ID, now); !errors.Is(err, campaign.ErrContactNotDialable) {
		t.Errorf("a DONE contact: %v", err)
	}
	if _, err := e.attempts.Begin(ctx, uuid.New(), now); !errors.Is(err, campaign.ErrContactNotDialable) {
		t.Errorf("an unknown contact: %v", err)
	}
}

// Two campaigns hold the same number; many admitters race for it. The index
// lets exactly one through.
func TestTwoCampaignsCannotDialOneNumberAtOnce(t *testing.T) {
	e := openAttempts(t)
	_, a := e.campaign(t, "A", "5", 3, nil)
	_, b := e.campaign(t, "B", "5", 3, nil) // the same three numbers

	for i := range a {
		if a[i].Number != b[i].Number {
			t.Fatal("test setup: the campaigns should share numbers")
		}
		var wins, refused atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for k := 0; k < 12; k++ {
			ct := a[i]
			if k%2 == 1 {
				ct = b[i]
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := e.attempts.Begin(context.Background(), ct.ID, ny(t, "2026-06-01 10:00"))
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, campaign.ErrNumberInFlight):
					refused.Add(1)
				default:
					t.Errorf("begin: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 || refused.Load() != 11 {
			t.Errorf("number %d: %d inserts won and %d were refused, want 1 and 11", i, wins.Load(), refused.Load())
		}
	}
	var dialing int
	_ = e.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM campaign_attempts WHERE state='DIALING'`).Scan(&dialing)
	if dialing != 3 {
		t.Errorf("%d attempts in flight, want 3", dialing)
	}
}

func TestACDRFinishesItsAttemptInItsOwnTransaction(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "CDR", "6", 1, nil)
	a := e.begin(t, contacts[0], ny(t, "2026-06-01 10:00"))

	// A CDR for a call that is not a campaign's touches no attempt.
	if err := e.ledger.InsertCDR(ctx, cdrOf(uuid.New(), "NORMAL_CLEARING", CDRStatusAnswered, true,
		ny(t, "2026-06-01 10:00"), ny(t, "2026-06-01 10:03"))); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-e.finished:
		t.Fatalf("a foreign CDR announced %+v", f)
	default:
	}

	end := ny(t, "2026-06-01 10:00").Add(30 * time.Second)
	cdr := cdrOf(a.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, ny(t, "2026-06-01 10:00"), end)
	if err := e.ledger.InsertCDR(ctx, cdr); err != nil {
		t.Fatal(err)
	}
	got, err := e.attempts.Get(ctx, a.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != campaign.AttemptFinished || got.Outcome != campaign.OutcomeNoAnswer || got.HangupCause != "NO_ANSWER" ||
		!got.IsTouched || got.FinishedAt == nil || !got.FinishedAt.Equal(end) {
		t.Errorf("attempt = %+v", got)
	}
	ct := e.contact(t, c, contacts[0].ID)
	if ct.Attempts != 1 || ct.SystemFailures != 0 || ct.LastOutcome != campaign.OutcomeNoAnswer || ct.Status != campaign.ContactActive ||
		ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(end.Add(2*time.Hour)) {
		t.Errorf("contact = %+v, want 1 attempt and a retry two hours after the call ended", ct)
	}
	select {
	case f := <-e.finished:
		if f.CallID != a.CallID || f.Outcome != campaign.OutcomeNoAnswer || f.IsRevision {
			t.Errorf("announced %+v", f)
		}
	default:
		t.Error("the finish was not announced")
	}
	if has, _ := e.ledger.HasCDR(ctx, a.CallID); !has {
		t.Error("the CDR is missing")
	}
}

// The CDR and the attempt commit together or not at all.
func TestTheCDRAndTheAttemptStandOrFallTogether(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Atomic", "7", 2, nil)

	// The CDR insert fails: the attempt is untouched.
	a := e.begin(t, contacts[0], ny(t, "2026-06-01 10:00"))
	bad := cdrOf(a.CallID, "NO_ANSWER", "NOT_A_STATUS", false, ny(t, "2026-06-01 10:00"), ny(t, "2026-06-01 10:01"))
	if err := e.ledger.InsertCDR(ctx, bad); err == nil {
		t.Fatal("a CDR with an impossible status was accepted")
	}
	if got, _ := e.attempts.Get(ctx, a.CallID); got.State != campaign.AttemptDialing || got.Outcome != "" {
		t.Errorf("attempt after a failed CDR = %+v, want DIALING", got)
	}
	if ct := e.contact(t, c, contacts[0].ID); ct.Attempts != 0 || ct.LastOutcome != "" || ct.NextAttemptAt != nil {
		t.Errorf("contact after a failed CDR = %+v", ct)
	}
	select {
	case f := <-e.finished:
		t.Fatalf("a failed write announced %+v", f)
	default:
	}

	// The attempt side fails: the CDR is not written either.
	b := e.begin(t, contacts[1], ny(t, "2026-06-01 10:00"))
	if _, err := e.st.Pool.Exec(ctx, `
		CREATE FUNCTION refuse_contact_update() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'contacts are read-only today'; END $$;
		CREATE TRIGGER refuse_contact_update BEFORE UPDATE ON campaign_contacts
		FOR EACH ROW EXECUTE FUNCTION refuse_contact_update();`); err != nil {
		t.Fatal(err)
	}
	cdr := cdrOf(b.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, ny(t, "2026-06-01 10:00"), ny(t, "2026-06-01 10:01"))
	if err := e.ledger.InsertCDR(ctx, cdr); err == nil {
		t.Fatal("the CDR was accepted although the contact could not be updated")
	}
	if has, _ := e.ledger.HasCDR(ctx, b.CallID); has {
		t.Error("the CDR was written although the attempt's transaction failed")
	}
	if got, _ := e.attempts.Get(ctx, b.CallID); got.State != campaign.AttemptDialing {
		t.Errorf("attempt = %+v, want DIALING", got)
	}
	if _, err := e.st.Pool.Exec(ctx, `DROP TRIGGER refuse_contact_update ON campaign_contacts`); err != nil {
		t.Fatal(err)
	}
	// ...and with the fault gone the same CDR lands.
	if err := e.ledger.InsertCDR(ctx, cdr); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.attempts.Get(ctx, b.CallID); got.State != campaign.AttemptFinished {
		t.Errorf("attempt = %+v, want FINISHED", got)
	}
}

// §7.2 and the #156 rule: a placement failure is written first and a real CDR
// replaces it; the call is counted once.
func TestAPlacementFailureThenTheRealCDRIsCountedOnce(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Placement", "8", 1, nil)
	start := ny(t, "2026-06-01 10:00")
	a := e.begin(t, contacts[0], start)

	// The job result arrives first: -ERR GATEWAY_DOWN.
	if err := e.ledger.InsertCDR(ctx, placementCDR(a.CallID, "GATEWAY_DOWN", start, start.Add(3*time.Second))); err != nil {
		t.Fatal(err)
	}
	ct := e.contact(t, c, contacts[0].ID)
	if ct.Attempts != 0 || ct.SystemFailures != 1 || ct.LastOutcome != campaign.OutcomeOriginateFailed ||
		ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(start.Add(3*time.Second).Add(5*time.Minute)) {
		t.Fatalf("after the placement failure: %+v", ct)
	}
	if got, _ := e.attempts.Get(ctx, a.CallID); got.Outcome != campaign.OutcomeOriginateFailed || got.HangupCause != "GATEWAY_DOWN" || got.IsTouched {
		t.Fatalf("attempt = %+v", got)
	}

	// Then the real row: the channel lived, the customer answered and talked.
	real := cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(2*time.Minute))
	if err := e.ledger.InsertCDR(ctx, real); err != nil {
		t.Fatal(err)
	}
	ct = e.contact(t, c, contacts[0].ID)
	if ct.Attempts != 1 || ct.SystemFailures != 0 || ct.Status != campaign.ContactDone || ct.NextAttemptAt != nil ||
		ct.LastOutcome != campaign.OutcomeAnswered {
		t.Errorf("after the real CDR: %+v, want 1 attempt, 0 system failures, DONE", ct)
	}
	got, _ := e.attempts.Get(ctx, a.CallID)
	if got.Outcome != campaign.OutcomeAnswered || !got.IsTouched || got.TalkSec != 115 {
		t.Errorf("attempt = %+v", got)
	}
	var revisions int
	for len(e.finished) > 0 {
		if (<-e.finished).IsRevision {
			revisions++
		}
	}
	if revisions != 1 {
		t.Errorf("%d revisions announced, want 1", revisions)
	}

	// The placement failure arriving late changes nothing: the store declines
	// the write, and the attempt is not touched either.
	if err := e.ledger.InsertCDR(ctx, placementCDR(a.CallID, "GATEWAY_DOWN", start, start.Add(10*time.Second))); err != nil {
		t.Fatal(err)
	}
	ct = e.contact(t, c, contacts[0].ID)
	if ct.Attempts != 1 || ct.SystemFailures != 0 || ct.Status != campaign.ContactDone {
		t.Errorf("after a stale placement failure: %+v", ct)
	}
	if len(e.finished) != 0 {
		t.Errorf("a declined write announced %d finishes", len(e.finished))
	}
}

func TestARevisedCDRNeverCountsTheCallTwice(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Revisions", "9", 1, nil)
	start := ny(t, "2026-06-01 10:00")
	a := e.begin(t, contacts[0], start)

	steps := []struct {
		cause, status string
		answered      bool
		endsAfter     time.Duration
		wantOutcome   campaign.Outcome
		attempts, sys int
		wantStatus    campaign.ContactStatus
	}{
		{"NORMAL_TEMPORARY_FAILURE", CDRStatusFailed, false, 10 * time.Second, campaign.OutcomeTransient, 0, 1, campaign.ContactActive},
		// The same CDR again: nothing moves.
		{"NORMAL_TEMPORARY_FAILURE", CDRStatusFailed, false, 20 * time.Second, campaign.OutcomeTransient, 0, 1, campaign.ContactActive},
		{"NO_ANSWER", CDRStatusNoAnswer, false, 40 * time.Second, campaign.OutcomeNoAnswer, 1, 0, campaign.ContactActive},
		{"NORMAL_CLEARING", CDRStatusAnswered, true, 3 * time.Minute, campaign.OutcomeAnswered, 1, 0, campaign.ContactDone},
		{"MEDIA_OR_PROVIDER_FAILURE", CDRStatusFailed, true, 4 * time.Minute, campaign.OutcomeBotFailed, 1, 0, campaign.ContactDone},
	}
	for i, st := range steps {
		cdr := cdrOf(a.CallID, st.cause, st.status, st.answered, start, start.Add(st.endsAfter))
		if err := e.ledger.InsertCDR(ctx, cdr); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		ct := e.contact(t, c, contacts[0].ID)
		if ct.Attempts != st.attempts || ct.SystemFailures != st.sys || ct.LastOutcome != st.wantOutcome || ct.Status != st.wantStatus {
			t.Errorf("step %d: contact = attempts %d, system %d, %s, %s; want %d, %d, %s, %s", i,
				ct.Attempts, ct.SystemFailures, ct.LastOutcome, ct.Status, st.attempts, st.sys, st.wantOutcome, st.wantStatus)
		}
		if (ct.Status == campaign.ContactActive) != (ct.NextAttemptAt != nil) {
			t.Errorf("step %d: status %s with next %v", i, ct.Status, ct.NextAttemptAt)
		}
	}
	// Step 1 repeated the same outcome and cause with a later end: a CDR that
	// differs only in time still replaces the row, but the attempt, whose
	// outcome did not change, stays as it is.
	if n := len(e.finished); n != 4 {
		t.Errorf("%d finishes announced, want 4 (the identical revision announces nothing)", n)
	}
}

// An older attempt is revised: its cost moves, the contact's fate (decided by
// its latest attempt) does not.
func TestRevisingAnOlderAttemptOnlyMovesTheCounters(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Older", "1", 1, nil)
	t1 := ny(t, "2026-06-01 10:00")
	first := e.begin(t, contacts[0], t1)
	if err := e.ledger.InsertCDR(ctx, placementCDR(first.CallID, "GATEWAY_DOWN", t1, t1.Add(2*time.Second))); err != nil {
		t.Fatal(err)
	}
	t2 := ny(t, "2026-06-01 10:10")
	second := e.begin(t, contacts[0], t2)
	if err := e.ledger.InsertCDR(ctx, cdrOf(second.CallID, "USER_BUSY", CDRStatusBusy, false, t2, t2.Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	before := e.contact(t, c, contacts[0].ID)
	if before.Attempts != 1 || before.SystemFailures != 1 || before.LastOutcome != campaign.OutcomeBusy {
		t.Fatalf("before = %+v", before)
	}

	// The first call's real row turns up: it had rung and nobody answered.
	if err := e.ledger.InsertCDR(ctx, cdrOf(first.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, t1, t1.Add(35*time.Second))); err != nil {
		t.Fatal(err)
	}
	after := e.contact(t, c, contacts[0].ID)
	if after.Attempts != 2 || after.SystemFailures != 0 {
		t.Errorf("counters = %d/%d, want 2/0", after.Attempts, after.SystemFailures)
	}
	if after.LastOutcome != campaign.OutcomeBusy || after.Status != before.Status ||
		after.NextAttemptAt == nil || !after.NextAttemptAt.Equal(*before.NextAttemptAt) {
		t.Errorf("the latest attempt's fate changed: before %+v, after %+v", before, after)
	}
}

func TestEndingsWithNoCDR(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "No CDR", "2", 5, nil)
	now := ny(t, "2026-06-01 10:00")

	finish := func(i int, out campaign.Outcome, cause string) campaign.Attempt {
		a := e.begin(t, contacts[i], now)
		done, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{Outcome: out, HangupCause: cause, EndedAt: now})
		if err != nil || done == nil {
			t.Fatalf("finish %s: %v %v", out, done, err)
		}
		return a
	}
	finish(0, campaign.OutcomeDNCSkipped, "")
	finish(1, campaign.OutcomeInvalidNumber, "")
	a2 := finish(2, campaign.OutcomeOriginateFailed, "")
	finish(3, campaign.OutcomeUnknown, "")

	want := []struct {
		status        campaign.ContactStatus
		attempts, sys int
		hasNext       bool
		next          time.Time
	}{
		{campaign.ContactDoNotCall, 0, 0, false, time.Time{}},
		{campaign.ContactInvalidNumber, 0, 0, false, time.Time{}},
		{campaign.ContactActive, 0, 1, true, now.Add(5 * time.Minute)},
		{campaign.ContactActive, 1, 0, true, ny(t, "2026-06-02 10:00")}, // UNKNOWN waits 24 h
	}
	for i, w := range want {
		ct := e.contact(t, c, contacts[i].ID)
		if ct.Status != w.status || ct.Attempts != w.attempts || ct.SystemFailures != w.sys || (ct.NextAttemptAt != nil) != w.hasNext ||
			(w.hasNext && !ct.NextAttemptAt.Equal(w.next)) {
			t.Errorf("contact %d = %+v, want %+v", i, ct, w)
		}
	}

	// A second finish does not count the call again, and says so.
	if done, err := e.attempts.Finish(ctx, a2.CallID, campaign.Ending{Outcome: campaign.OutcomeBotFailed, EndedAt: now}); err != nil || done != nil {
		t.Errorf("finishing a finished attempt = %v, %v; want (nil, nil)", done, err)
	}
	if ct := e.contact(t, c, contacts[2].ID); ct.SystemFailures != 1 || ct.Attempts != 0 {
		t.Errorf("counted twice: %+v", ct)
	}
	if _, err := e.attempts.Finish(ctx, uuid.New(), campaign.Ending{Outcome: campaign.OutcomeUnknown}); !errors.Is(err, campaign.ErrNoAttempt) {
		t.Errorf("unknown call: %v", err)
	}
	if _, err := e.attempts.Finish(ctx, e.begin(t, contacts[4], now).CallID, campaign.Ending{Outcome: "NOPE"}); err == nil {
		t.Error("a made-up outcome was accepted")
	}

	// A CDR that arrives after the standalone finish refines it.
	if err := e.ledger.InsertCDR(ctx, cdrOf(a2.CallID, "USER_BUSY", CDRStatusBusy, false, now, now.Add(25*time.Second))); err != nil {
		t.Fatal(err)
	}
	if ct := e.contact(t, c, contacts[2].ID); ct.SystemFailures != 0 || ct.Attempts != 1 || ct.LastOutcome != campaign.OutcomeBusy {
		t.Errorf("after the CDR: %+v", ct)
	}
}

func TestAVoidedAttemptLeavesNoRowAndNoCount(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Void", "3", 1, nil)
	now := ny(t, "2026-06-01 10:00")
	a := e.begin(t, contacts[0], now)

	if err := e.attempts.Void(ctx, a.CallID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.attempts.Get(ctx, a.CallID); !errors.Is(err, campaign.ErrNoAttempt) {
		t.Errorf("the voided row survives: %v", err)
	}
	ct := e.contact(t, c, contacts[0].ID)
	if ct.Attempts != 0 || ct.SystemFailures != 0 || ct.LastOutcome != "" || ct.Status != campaign.ContactActive ||
		ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(now.Add(time.Second)) {
		t.Errorf("contact = %+v, want untouched and due at once", ct)
	}
	if err := e.attempts.Void(ctx, a.CallID, now); !errors.Is(err, campaign.ErrNoAttempt) {
		t.Errorf("voiding twice: %v", err)
	}
	// The number is free again.
	b := e.begin(t, ct, now.Add(2*time.Second))

	// A finished attempt cannot be voided: it has been counted.
	if _, err := e.attempts.Finish(ctx, b.CallID, campaign.Ending{Outcome: campaign.OutcomeBusy, EndedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.attempts.Void(ctx, b.CallID, now); !errors.Is(err, campaign.ErrNoAttempt) {
		t.Errorf("voiding a finished attempt: %v", err)
	}
	if got, err := e.attempts.Get(ctx, b.CallID); err != nil || got.State != campaign.AttemptFinished {
		t.Errorf("the finished attempt must survive: %+v %v", got, err)
	}
}

func TestLeadsAreActedOnOnceWhenTheCallEnds(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Leads", "4", 6, nil)
	start := ny(t, "2026-06-01 10:00")

	callbacks := func(callID uuid.UUID) []Callback {
		rows, err := e.st.Pool.Query(ctx, `SELECT id, call_id, queue_id, phone_number, message FROM callbacks WHERE call_id=$1`, callID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []Callback
		for rows.Next() {
			var cb Callback
			if err := rows.Scan(&cb.ID, &cb.CallID, &cb.QueueID, &cb.PhoneNumber, &cb.Message); err != nil {
				t.Fatal(err)
			}
			out = append(out, cb)
		}
		return out
	}
	answer := func(a campaign.Attempt) {
		t.Helper()
		if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "NORMAL_CLEARING", CDRStatusAnswered, true, start, start.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("HIGH files exactly one callback, even if the CDR is revised", func(t *testing.T) {
		a := e.begin(t, contacts[0], start)
		if ok, err := e.attempts.SetLead(ctx, a.CallID, campaign.LeadHigh, "wants the offer by email"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if got := callbacks(a.CallID); len(got) != 0 {
			t.Fatalf("a callback was filed before the call ended: %+v", got)
		}
		answer(a)
		// A later revision of the same call.
		if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "MEDIA_OR_PROVIDER_FAILURE", CDRStatusFailed, true, start, start.Add(2*time.Minute))); err != nil {
			t.Fatal(err)
		}
		got := callbacks(a.CallID)
		if len(got) != 1 || got[0].PhoneNumber != contacts[0].Number || got[0].Message != "wants the offer by email" ||
			got[0].QueueID == nil || *got[0].QueueID != e.queueID {
			t.Fatalf("callbacks = %+v, want one for %s on the follow-up queue", got, contacts[0].Number)
		}
		ct := e.contact(t, c, contacts[0].ID)
		if ct.LeadIntent != campaign.LeadHigh || ct.LeadNotes != "wants the offer by email" || ct.Status != campaign.ContactDone {
			t.Errorf("contact = %+v", ct)
		}
	})
	t.Run("HIGH then MEDIUM files nothing; MEDIUM then HIGH files one", func(t *testing.T) {
		a := e.begin(t, contacts[1], start)
		_, _ = e.attempts.SetLead(ctx, a.CallID, campaign.LeadHigh, "maybe")
		_, _ = e.attempts.SetLead(ctx, a.CallID, campaign.LeadMedium, "on second thought, later")
		answer(a)
		if got := callbacks(a.CallID); len(got) != 0 {
			t.Errorf("HIGH then MEDIUM filed %+v", got)
		}
		if ct := e.contact(t, c, contacts[1].ID); ct.LeadIntent != campaign.LeadMedium {
			t.Errorf("the last lead should win: %+v", ct)
		}

		b := e.begin(t, contacts[2], start)
		_, _ = e.attempts.SetLead(ctx, b.CallID, campaign.LeadMedium, "")
		_, _ = e.attempts.SetLead(ctx, b.CallID, campaign.LeadHigh, "send the offer")
		answer(b)
		if got := callbacks(b.CallID); len(got) != 1 {
			t.Errorf("MEDIUM then HIGH filed %d callbacks, want 1", len(got))
		}
	})
	t.Run("DO_NOT_CALL goes on the list once and ends the contact", func(t *testing.T) {
		a := e.begin(t, contacts[3], start)
		_, _ = e.attempts.SetLead(ctx, a.CallID, campaign.LeadDoNotCall, "stop calling me")
		answer(a)
		answer(a) // the identical CDR again
		listed, _, err := e.st.DNC().List(ctx, DNCFilter{Prefix: contacts[3].Number})
		if err != nil || len(listed) != 1 {
			t.Fatalf("not on the list: %v %v", listed, err)
		}
		entry := listed[0]
		if entry.Source != DNCSourceConversation || entry.CallID == nil || *entry.CallID != a.CallID {
			t.Errorf("entry = %+v", entry)
		}
		var n int
		_ = e.st.Pool.QueryRow(ctx, `SELECT count(*) FROM dnc_numbers WHERE number_e164=$1`, contacts[3].Number).Scan(&n)
		if n != 1 {
			t.Errorf("%d DNC rows", n)
		}
		if ct := e.contact(t, c, contacts[3].ID); ct.Status != campaign.ContactDoNotCall || ct.NextAttemptAt != nil {
			t.Errorf("contact = %+v, want DNC", ct)
		}
		if got := callbacks(a.CallID); len(got) != 0 {
			t.Errorf("a DO_NOT_CALL filed a callback: %+v", got)
		}
	})
	t.Run("no lead: NULL, not NONE, and an earlier lead survives a call that records none", func(t *testing.T) {
		a := e.begin(t, contacts[4], start)
		answer(a)
		if ct := e.contact(t, c, contacts[4].ID); ct.LeadIntent != "" || ct.Status != campaign.ContactDone {
			t.Errorf("contact = %+v", ct)
		}
		if got := callbacks(a.CallID); len(got) != 0 {
			t.Errorf("callbacks = %+v", got)
		}
		// An earlier lead on a contact that is then dialled again.
		if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET lead_intent='LOW', lead_notes='earlier' WHERE id=$1`, contacts[5].ID); err != nil {
			t.Fatal(err)
		}
		b := e.begin(t, contacts[5], start)
		if err := e.ledger.InsertCDR(ctx, cdrOf(b.CallID, "USER_BUSY", CDRStatusBusy, false, start, start.Add(10*time.Second))); err != nil {
			t.Fatal(err)
		}
		if ct := e.contact(t, c, contacts[5].ID); ct.LeadIntent != campaign.LeadLow || ct.LeadNotes != "earlier" {
			t.Errorf("an earlier lead was erased: %+v", ct)
		}
	})
	t.Run("a lead cannot be written once the attempt is finished", func(t *testing.T) {
		a, _ := e.attempts.Get(ctx, mustCall(t, e, contacts[4].ID))
		if ok, err := e.attempts.SetLead(ctx, a.CallID, campaign.LeadHigh, "late"); err != nil || ok {
			t.Errorf("late lead: ok=%v err=%v, want false", ok, err)
		}
		if _, err := e.attempts.SetLead(ctx, a.CallID, "BANANAS", ""); err == nil {
			t.Error("an invalid intent was accepted")
		}
	})
}

func mustCall(t *testing.T, e *attemptEnv, contactID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.st.Pool.QueryRow(context.Background(),
		`SELECT call_id FROM campaign_attempts WHERE campaign_contact_id=$1 ORDER BY started_at DESC LIMIT 1`, contactID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestARetryIsScheduledInTheContactsOwnZone(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Zones", "2", 2, nil)
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET time_zone='Asia/Shanghai' WHERE id=$1`, contacts[1].ID); err != nil {
		t.Fatal(err)
	}
	// 2026-06-01 21:50:00Z... end both calls at the same instant: 13:55 UTC is
	// 09:55 in New York and 21:55 in Shanghai.
	end := time.Date(2026, 6, 1, 13, 55, 0, 0, time.UTC)
	for _, ct := range contacts {
		a := e.begin(t, ct, end.Add(-time.Minute))
		if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, end.Add(-time.Minute), end)); err != nil {
			t.Fatal(err)
		}
	}
	nyContact := e.contact(t, c, contacts[0].ID)
	if want := end.Add(2 * time.Hour); nyContact.NextAttemptAt == nil || !nyContact.NextAttemptAt.Equal(want) {
		t.Errorf("New York contact: next = %v, want %v (11:55, inside the window)", nyContact.NextAttemptAt, want)
	}
	shContact := e.contact(t, c, contacts[1].ID)
	// 23:55 in Shanghai: outside 09:00-18:00 and past the outer hours, so
	// tomorrow 09:00 there = 01:00Z on the 2nd.
	if want := time.Date(2026, 6, 2, 1, 0, 0, 0, time.UTC); shContact.NextAttemptAt == nil || !shContact.NextAttemptAt.Equal(want) {
		t.Errorf("Shanghai contact: next = %v, want %v (09:00 the next morning, Shanghai)", shContact.NextAttemptAt, want)
	}
}

func TestTheBreakersReadTheNewestOutcomesFirst(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Breakers", "1", 4, func(c *campaign.Campaign) {
		c.RetryPolicy.MaxSystemRetries = 50
	})
	t0 := ny(t, "2026-06-01 10:00")
	seq := []campaign.Outcome{campaign.OutcomeAnswered, campaign.OutcomeTransient, campaign.OutcomeOriginateFailed, campaign.OutcomeTransient}
	for i, o := range seq {
		a := e.begin(t, contacts[i], t0)
		if _, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{Outcome: o, EndedAt: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.attempts.RecentOutcomes(ctx, c.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []campaign.Outcome{campaign.OutcomeTransient, campaign.OutcomeOriginateFailed, campaign.OutcomeTransient}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recent = %v, want %v", got, want)
	}
	if !campaign.TrunkBreakerTrips(got, 3) || campaign.TrunkBreakerTrips(got, 4) {
		t.Error("the trunk breaker should trip at 3 and not at 4 here")
	}
}

// The invariants of the whole machine, over thousands of random histories,
// some through the CDR path and some through the standalone finish, with
// revisions mixed in.
func TestRandomAttemptHistoriesKeepTheInvariants(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)

	type camp struct {
		c        campaign.Campaign
		contacts []campaign.Contact
	}
	rng := rand.New(rand.NewPCG(20261012, 3))
	var camps []camp
	const perCampaign = 400
	for i := 0; i < 5; i++ {
		maxAttempts, maxSys := 1+rng.IntN(4), rng.IntN(6)
		c, contacts := e.campaign(t, fmt.Sprintf("Random %d", i), fmt.Sprintf("%d", 20+i), perCampaign, func(c *campaign.Campaign) {
			c.RetryPolicy.MaxAttempts = maxAttempts
			c.RetryPolicy.MaxSystemRetries = maxSys
			if i == 4 {
				c.RetryPolicy.RetryOutcomes = []campaign.Outcome{campaign.OutcomeNoAnswer, campaign.OutcomeTransient}
			}
		})
		camps = append(camps, camp{c, contacts})
	}

	causes := []struct {
		cause, status string
		answered      bool
	}{
		{"NO_ANSWER", CDRStatusNoAnswer, false}, {"USER_BUSY", CDRStatusBusy, false},
		{"CALL_REJECTED", CDRStatusFailed, false}, {"NORMAL_TEMPORARY_FAILURE", CDRStatusFailed, false},
		{"UNALLOCATED_NUMBER", CDRStatusFailed, false}, {"CHANNEL_VANISHED", CDRStatusFailed, false},
		{"NORMAL_CLEARING", CDRStatusAnswered, true}, {"MEDIA_OR_PROVIDER_FAILURE", CDRStatusFailed, true},
		{"OVERFLOW", CDRStatusAnswered, true}, {"SOME_NEW_CAUSE", CDRStatusFailed, false},
	}
	outcomes := []campaign.Outcome{
		campaign.OutcomeUnknown, campaign.OutcomeOriginateFailed, campaign.OutcomeBotFailed,
		campaign.OutcomeInvalidNumber, campaign.OutcomeDNCSkipped,
	}

	work := make(chan campaign.Contact)
	var wg sync.WaitGroup
	var attemptsRun atomic.Int64
	for w := 0; w < 8; w++ {
		seed := uint64(w) + 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, 99))
			for ct := range work {
				now := ny(t, "2026-06-01 09:00").Add(time.Duration(r.IntN(7*24*60)) * time.Minute)
				for step := 0; step < 60; step++ {
					a, err := e.attempts.Begin(ctx, ct.ID, now)
					if errors.Is(err, campaign.ErrContactNotDialable) {
						break
					}
					if err != nil {
						t.Errorf("begin: %v", err)
						return
					}
					attemptsRun.Add(1)
					end := now.Add(time.Duration(5+r.IntN(300)) * time.Second)
					switch r.IntN(3) {
					case 0: // standalone
						if _, err := e.attempts.Finish(ctx, a.CallID, campaign.Ending{
							Outcome: outcomes[r.IntN(len(outcomes))], EndedAt: end}); err != nil {
							t.Errorf("finish: %v", err)
							return
						}
					default: // CDR, sometimes preceded by a placement failure and sometimes revised
						pick := causes[r.IntN(len(causes))]
						if r.IntN(5) == 0 {
							if err := e.ledger.InsertCDR(ctx, placementCDR(a.CallID, "GATEWAY_DOWN", now, end)); err != nil {
								t.Errorf("cdr: %v", err)
								return
							}
							end = end.Add(time.Second)
						}
						if r.IntN(4) == 0 {
							lead := []campaign.LeadIntent{campaign.LeadHigh, campaign.LeadMedium, campaign.LeadDoNotCall}[r.IntN(3)]
							_, _ = e.attempts.SetLead(ctx, a.CallID, lead, "n")
						}
						if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, pick.cause, pick.status, pick.answered, now, end)); err != nil {
							t.Errorf("cdr: %v", err)
							return
						}
						if r.IntN(4) == 0 { // a fuller revision
							again := causes[r.IntN(len(causes))]
							if err := e.ledger.InsertCDR(ctx, cdrOf(a.CallID, again.cause, again.status, again.answered, now, end.Add(time.Minute))); err != nil {
								t.Errorf("cdr revision: %v", err)
								return
							}
						}
					}
					var next *time.Time
					_ = e.st.Pool.QueryRow(ctx, `SELECT next_attempt_at FROM campaign_contacts WHERE id=$1`, ct.ID).Scan(&next)
					if next == nil {
						break
					}
					now = *next
				}
			}
		}()
	}
	for _, cp := range camps {
		for _, ct := range cp.contacts {
			work <- ct
		}
	}
	close(work)
	wg.Wait()
	if t.Failed() {
		return
	}
	if attemptsRun.Load() < 2000 {
		t.Fatalf("only %d attempts ran; the property test should exercise thousands", attemptsRun.Load())
	}

	for _, cp := range camps {
		pol := cp.c.RetryPolicy
		contacts, _, err := e.cs.ListContacts(ctx, cp.c.ID, campaign.ContactFilter{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, ct := range contacts {
			if ct.Attempts > pol.MaxAttempts || ct.SystemFailures > max(pol.MaxSystemRetries, 1) {
				t.Errorf("%s: attempts %d/%d, system failures %d/%d", ct.Number, ct.Attempts, pol.MaxAttempts, ct.SystemFailures, pol.MaxSystemRetries)
			}
			if ct.Status != campaign.ContactActive && ct.NextAttemptAt != nil {
				t.Errorf("%s: %s contact with next_attempt_at %v", ct.Number, ct.Status, ct.NextAttemptAt)
			}
			if ct.Status == campaign.ContactActive && ct.NextAttemptAt == nil {
				t.Errorf("%s: ACTIVE contact with nothing due", ct.Number)
			}
			// The counters are the attempts' own books.
			var attempts, sys, finished, dialing int
			var answered bool
			if err := e.st.Pool.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE is_touched),
				       count(*) FILTER (WHERE outcome IN ('TRANSIENT','ORIGINATE_FAILED')),
				       count(*) FILTER (WHERE state='FINISHED' AND outcome IS NOT NULL),
				       count(*) FILTER (WHERE state='DIALING'),
				       coalesce(bool_or(outcome IN ('ANSWERED','BOT_FAILED','OVERFLOW')), false)
				FROM campaign_attempts WHERE campaign_contact_id=$1`, ct.ID).Scan(&attempts, &sys, &finished, &dialing, &answered); err != nil {
				t.Fatal(err)
			}
			if attempts != ct.Attempts || sys != ct.SystemFailures {
				t.Errorf("%s: the contact counts %d/%d, its attempts say %d/%d", ct.Number, ct.Attempts, ct.SystemFailures, attempts, sys)
			}
			if dialing != 0 {
				t.Errorf("%s: %d attempts still DIALING", ct.Number, dialing)
			}
			if answered && ct.Status != campaign.ContactDone && ct.Status != campaign.ContactDoNotCall {
				t.Errorf("%s: answered once and now %s", ct.Number, ct.Status)
			}
		}
	}
	var unfinished int
	if err := e.st.Pool.QueryRow(ctx, `SELECT count(*) FROM campaign_attempts WHERE state='FINISHED' AND outcome IS NULL`).Scan(&unfinished); err != nil || unfinished != 0 {
		t.Errorf("%d finished attempts without an outcome (%v)", unfinished, err)
	}
	var callbacks, leads int
	_ = e.st.Pool.QueryRow(ctx, `SELECT count(*) FROM callbacks`).Scan(&callbacks)
	_ = e.st.Pool.QueryRow(ctx, `SELECT count(*) FROM campaign_attempts WHERE lead_intent='HIGH' AND state='FINISHED'`).Scan(&leads)
	if callbacks != leads {
		t.Errorf("%d callbacks for %d finished HIGH leads: exactly one each", callbacks, leads)
	}
}
