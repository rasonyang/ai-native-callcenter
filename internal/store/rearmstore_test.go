// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// contactTimes reads what the re-arm and its writers decide on: the due time,
// the retry policy's time and the two marks.
type contactTimes struct {
	next, retry       *time.Time
	capHeld, freqHeld bool
	status            string
}

func readContactTimes(t *testing.T, st *Store, id uuid.UUID) contactTimes {
	t.Helper()
	var next, retry pgtype.Timestamptz
	var out contactTimes
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT next_attempt_at, retry_at, is_daily_cap_held, is_frequency_held, status FROM campaign_contacts WHERE id = $1`, id).
		Scan(&next, &retry, &out.capHeld, &out.freqHeld, &out.status); err != nil {
		t.Fatal(err)
	}
	out.next, out.retry = timeOrNil(next), timeOrNil(retry)
	return out
}

func sameTime(a *time.Time, b time.Time) bool { return a != nil && a.Equal(b) }

// The settle writes the retry policy's time beside the due time the window
// moved it to; the claim spends it, a void leaves the contact with none, and no
// deferral touches it (#616).
func TestTheRetryTimeIsWrittenBySettleAndLeftAloneByDeferrals(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	_, cts := e.campaign(t, "A", "75", 3, nil) // New York, 09:00-18:00 every day
	d := e.st.Dialer()
	settled, claimed, deferred := cts[0], cts[1], cts[2]

	// NO_ANSWER at 17:10 waits two hours: 19:10, outside the window, so due at
	// 09:00 the next morning and the policy's 19:10 kept.
	call, err := e.attempts.Begin(ctx, settled.ID, ny(t, "2026-06-01 17:09"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.attempts.Finish(ctx, call.CallID, campaign.Ending{
		Outcome: campaign.OutcomeNoAnswer, HangupCause: "NO_ANSWER", EndedAt: ny(t, "2026-06-01 17:10"),
	}); err != nil {
		t.Fatal(err)
	}
	got := readContactTimes(t, e.st, settled.ID)
	if !sameTime(got.retry, ny(t, "2026-06-01 19:10")) || !sameTime(got.next, ny(t, "2026-06-02 09:00")) {
		t.Fatalf("settled: retry %v next %v, want 19:10 and the next morning's 09:00", got.retry, got.next)
	}

	// Every kind of deferral moves the due time and leaves the policy's time.
	for _, hold := range []campaign.HoldKind{campaign.HoldNone, campaign.HoldFrequency, campaign.HoldDailyCap} {
		later := got.next.Add(time.Hour)
		if err := d.Defer(ctx, []uuid.UUID{settled.ID}, []time.Time{later}, hold); err != nil {
			t.Fatal(err)
		}
		got = readContactTimes(t, e.st, settled.ID)
		if !sameTime(got.next, later) || !sameTime(got.retry, ny(t, "2026-06-01 19:10")) {
			t.Errorf("deferred (%q): next %v retry %v, want next %v and the retry time kept", hold, got.next, got.retry, later)
		}
	}
	// So does the re-arm for a number put on the list of test numbers.
	if _, _, err := e.st.TestNumbers().Add(ctx, settled.Number, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.RearmHeld(ctx, ny(t, "2026-06-01 17:30")); err != nil {
		t.Fatal(err)
	}
	if got = readContactTimes(t, e.st, settled.ID); !sameTime(got.retry, ny(t, "2026-06-01 19:10")) {
		t.Errorf("after the test-number re-arm the retry time is %v, want it kept", got.retry)
	}

	// The claim spends the retry time; a void leaves the contact due with none.
	call, err = e.attempts.Begin(ctx, settled.ID, ny(t, "2026-06-01 17:31"))
	if err != nil {
		t.Fatal(err)
	}
	if got = readContactTimes(t, e.st, settled.ID); got.next != nil || got.retry != nil {
		t.Errorf("claimed: next %v retry %v, want neither", got.next, got.retry)
	}
	if err := e.attempts.Void(ctx, call.CallID, ny(t, "2026-06-01 17:32")); err != nil {
		t.Fatal(err)
	}
	if got = readContactTimes(t, e.st, settled.ID); !sameTime(got.next, ny(t, "2026-06-01 17:32")) || got.retry != nil {
		t.Errorf("voided: next %v retry %v, want due at the void and no retry time", got.next, got.retry)
	}

	// A settle that ends the contact leaves no retry time.
	call, err = e.attempts.Begin(ctx, claimed.ID, ny(t, "2026-06-01 10:00"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.attempts.Finish(ctx, call.CallID, campaign.Ending{
		Outcome: campaign.OutcomeAnswered, HangupCause: "NORMAL_CLEARING", EndedAt: ny(t, "2026-06-01 10:01"),
	}); err != nil {
		t.Fatal(err)
	}
	if got = readContactTimes(t, e.st, claimed.ID); got.status != "DONE" || got.retry != nil || got.next != nil {
		t.Errorf("answered: %+v, want DONE with neither time", got)
	}

	// A contact never tried has no retry time, deferred or not.
	if err := d.Defer(ctx, []uuid.UUID{deferred.ID}, []time.Time{time.Now().Add(48 * time.Hour)}, campaign.HoldNone); err != nil {
		t.Fatal(err)
	}
	if got = readContactTimes(t, e.st, deferred.ID); got.retry != nil {
		t.Errorf("never tried: retry %v, want none", got.retry)
	}
}

// Waiting lists, page by page in id order, the ACTIVE contacts due later than
// now that carry no hold mark; Rearm writes a judged due time only where the row
// is still what was judged, and never the retry time or a mark.
func TestWaitingAndRearmJudgeOnlyContactsNoHoldMoved(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, cts := e.campaign(t, "A", "76", 8, nil)
	d := e.st.Dialer()
	now := time.Now().Truncate(time.Microsecond)
	later := now.Add(20 * time.Hour)
	retry := now.Add(time.Hour)

	set := func(id uuid.UUID, next, retry *time.Time, capHeld, freqHeld bool) {
		t.Helper()
		if _, err := e.st.Pool.Exec(ctx,
			`UPDATE campaign_contacts SET next_attempt_at = $2, retry_at = $3, is_daily_cap_held = $4, is_frequency_held = $5 WHERE id = $1`,
			id, next, retry, capHeld, freqHeld); err != nil {
			t.Fatal(err)
		}
	}
	past := now.Add(-time.Minute)
	untried, tried, due, capHeld, freqHeld, inFlight, done, tried2 := cts[0], cts[1], cts[2], cts[3], cts[4], cts[5], cts[6], cts[7]
	set(untried.ID, &later, nil, false, false)
	set(tried.ID, &later, &retry, false, false)
	set(tried2.ID, &later, &retry, false, false)
	set(due.ID, &past, nil, false, false)
	set(capHeld.ID, &later, &retry, true, false)
	set(freqHeld.ID, &later, nil, false, true)
	if _, err := e.attempts.Begin(ctx, inFlight.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Pool.Exec(ctx,
		`UPDATE campaign_contacts SET status = 'EXHAUSTED', next_attempt_at = NULL, retry_at = NULL WHERE id = $1`, done.ID); err != nil {
		t.Fatal(err)
	}

	// Page by page: two at a time, in id order, nothing twice.
	var listed []campaign.Waiting
	after := uuid.Nil
	for page := 0; ; page++ {
		got, err := d.Waiting(ctx, c.ID, now, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, got...)
		if len(got) < 2 {
			break
		}
		after = got[len(got)-1].ContactID
		if page > 5 {
			t.Fatal("paging never ended")
		}
	}
	want := []uuid.UUID{untried.ID, tried.ID, tried2.ID} // ids are v7 and imported in order
	if len(listed) != len(want) {
		t.Fatalf("listed %d waiting contacts %+v, want %d: untried, tried and tried2", len(listed), listed, len(want))
	}
	for i, w := range listed {
		if w.ContactID != want[i] {
			t.Errorf("listed[%d] = %s, want %s (id order)", i, w.ContactID, want[i])
		}
	}
	if listed[0].RetryAt != nil || !listed[0].NextAttemptAt.Equal(later) {
		t.Errorf("untried: %+v, want due %v with no retry time", listed[0], later)
	}
	if !sameTime(listed[1].RetryAt, retry) || !listed[1].NextAttemptAt.Equal(later) {
		t.Errorf("tried: %+v, want due %v and retry %v", listed[1], later, retry)
	}

	// Judged: untried due now, tried at its retry time. tried2 was settled
	// again after it was judged, and the cap-held contact is written for too:
	// neither moves.
	tried2Settled := now.Add(3 * time.Hour)
	set(tried2.ID, &tried2Settled, &tried2Settled, false, false)
	capJudged := campaign.Waiting{ContactID: capHeld.ID, NextAttemptAt: later, RetryAt: &retry}
	moved, err := d.Rearm(ctx,
		[]campaign.Waiting{listed[0], listed[1], listed[2], capJudged},
		[]time.Time{now, retry, now, now})
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Errorf("moved %d, want 2: untried and tried", moved)
	}
	if got := readContactTimes(t, e.st, untried.ID); !sameTime(got.next, now) || got.retry != nil {
		t.Errorf("untried: %+v, want due now with no retry time", got)
	}
	if got := readContactTimes(t, e.st, tried.ID); !sameTime(got.next, retry) || !sameTime(got.retry, retry) {
		t.Errorf("tried: %+v, want due at its retry time, the retry time kept", got)
	}
	if got := readContactTimes(t, e.st, tried2.ID); !sameTime(got.next, tried2Settled) {
		t.Errorf("settled again after it was judged: due %v, want %v kept", got.next, tried2Settled)
	}
	if got := readContactTimes(t, e.st, capHeld.ID); !sameTime(got.next, later) || !got.capHeld {
		t.Errorf("cap-held: %+v, want it kept, marked", got)
	}

	// A judgement whose retry time is stale does not land either.
	stale := campaign.Waiting{ContactID: tried.ID, NextAttemptAt: retry, RetryAt: &later}
	if moved, err := d.Rearm(ctx, []campaign.Waiting{stale}, []time.Time{now}); err != nil || moved != 0 {
		t.Errorf("a stale retry time moved %d (%v), want 0", moved, err)
	}
	// A contact claimed in the meantime is not touched.
	claimedJudged := campaign.Waiting{ContactID: inFlight.ID, NextAttemptAt: later}
	if moved, err := d.Rearm(ctx, []campaign.Waiting{claimedJudged}, []time.Time{now}); err != nil || moved != 0 {
		t.Errorf("a claimed contact moved %d (%v), want 0", moved, err)
	}
	if got := readContactTimes(t, e.st, inFlight.ID); got.next != nil {
		t.Errorf("the contact in flight is due %v, want no due time", got.next)
	}
	if _, err := d.Rearm(ctx, []campaign.Waiting{listed[0]}, nil); err == nil {
		t.Error("Rearm took lists of different lengths")
	}
}
