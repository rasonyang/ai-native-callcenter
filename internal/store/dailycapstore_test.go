// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// rawAttempt writes a finished attempt as if the dialer had placed it then.
func rawAttempt(t *testing.T, st *Store, c campaign.Campaign, ct campaign.Contact, outcome campaign.Outcome, at time.Time) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id,
		     state, outcome, is_touched, started_at, finished_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'FINISHED', $7, $8, $9, $9)`,
		uuid.Must(uuid.NewV7()), c.ID, ct.ID, ct.Number, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()),
		string(outcome), campaign.IsTouched(outcome), at); err != nil {
		t.Fatal(err)
	}
}

func adHocCDR(t *testing.T, st *Store, to, callType string, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if err := st.Ledger().InsertCDR(context.Background(), CDR{
		CallID: id, StartedAt: at, EndedAt: at.Add(time.Minute), CallType: callType,
		FromNumber: "95099", ToNumber: to, DID: "95099", Status: CDRStatusNoAnswer,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// The count reads a campaign's attempts in flight or that spent a budget, from
// every campaign, and the ad-hoc outbound calls whose canonical callee is the
// number; it starts at the day start, and a listed number has no dials.
func TestDialsTodayCountsWhatTheCapCounts(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	st := e.st
	st.DefaultCountryCode = "86"
	a, actts := e.campaign(t, "A", "71", 4, nil)
	b, bcts := e.campaign(t, "B", "71", 4, nil)
	_ = a
	now := ny(t, "2026-06-01 15:00")
	dayStart := ny(t, "2026-06-01 00:00")

	// Contact 0: every kind of attempt that counts, one that does not, and one
	// from before the day. Written for both campaigns' contacts of one number.
	n0 := actts[0].Number
	rawAttempt(t, st, a, actts[0], campaign.OutcomeNoAnswer, ny(t, "2026-06-01 08:00"))
	rawAttempt(t, st, b, bcts[0], campaign.OutcomeTransient, ny(t, "2026-06-01 09:00"))
	rawAttempt(t, st, b, bcts[0], campaign.OutcomeOriginateFailed, ny(t, "2026-06-01 10:00"))
	rawAttempt(t, st, b, bcts[0], campaign.OutcomeDNCSkipped, ny(t, "2026-06-01 11:00"))
	rawAttempt(t, st, b, bcts[0], campaign.OutcomeInvalidNumber, ny(t, "2026-06-01 12:00"))
	rawAttempt(t, st, b, bcts[0], campaign.OutcomeNoAnswer, ny(t, "2026-05-31 23:59"))
	// An attempt in flight counts.
	n1 := actts[1].Number
	if _, err := e.attempts.Begin(ctx, actts[1].ID, ny(t, "2026-06-01 14:00")); err != nil {
		t.Fatal(err)
	}
	// An ad-hoc outbound call in the national form counts; an inbound call, a
	// call of yesterday and a call to a number that does not canonicalise do not.
	n2 := actts[2].Number // +86 13 71 0000002 -> national "13710000002"
	adHocCDR(t, st, n2[3:], "OUTBOUND", ny(t, "2026-06-01 09:30"))
	adHocCDR(t, st, n2, "OUTBOUND", ny(t, "2026-06-01 09:40"))
	adHocCDR(t, st, n2, "INBOUND", ny(t, "2026-06-01 09:50"))
	adHocCDR(t, st, n2, "OUTBOUND", ny(t, "2026-05-31 09:50"))
	adHocCDR(t, st, "95001", "OUTBOUND", ny(t, "2026-06-01 09:50"))
	// A campaign call's CDR is the attempt, not a second dial.
	n3 := actts[3].Number
	call, err := e.attempts.Begin(ctx, bcts[3].ID, ny(t, "2026-06-01 13:00"))
	if err != nil {
		t.Fatal(err)
	}
	callCDR := cdrOf(call.CallID, "NO_ANSWER", CDRStatusNoAnswer, false, ny(t, "2026-06-01 13:00"), ny(t, "2026-06-01 13:01"))
	callCDR.ToNumber = n3[3:]
	if err := e.ledger.InsertCDR(ctx, callCDR); err != nil {
		t.Fatal(err)
	}
	// A listed number is not counted at all.
	if _, _, err := st.TestNumbers().Add(ctx, n0, "", nil); err != nil {
		t.Fatal(err)
	}

	got, err := st.Dialer().DialsToday(ctx,
		[]string{n0, n1, n2, n3, "+8613999999999"},
		[]time.Time{dayStart, dayStart, dayStart, dayStart, dayStart}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{0, 1, 2, 1, 0}; !equalInts(got, want) {
		t.Errorf("dials today = %v, want %v (listed, in flight, ad-hoc x2, a campaign call once, none)", got, want)
	}

	// Taken off the list the number's attempts count: the two campaigns' touches
	// and the system failures, but not the skip, the bad number or yesterday.
	if err := st.TestNumbers().Remove(ctx, n0); err != nil {
		t.Fatal(err)
	}
	got, err = st.Dialer().DialsToday(ctx, []string{n0}, []time.Time{dayStart}, now)
	if err != nil || len(got) != 1 || got[0] != 3 {
		t.Errorf("dials today after removal = %v (%v), want 3", got, err)
	}
	// The day start decides: from 08:30 only the later attempts count.
	got, _ = st.Dialer().DialsToday(ctx, []string{n0}, []time.Time{ny(t, "2026-06-01 08:30")}, now)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("dials since 08:30 = %v, want 2", got)
	}
	// ... and the clock bounds it above.
	got, _ = st.Dialer().DialsToday(ctx, []string{n0}, []time.Time{dayStart}, ny(t, "2026-06-01 08:30"))
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("dials by 08:30 = %v, want 1", got)
	}
}

// A line's refusal never reached a phone, so it is no dial of the number for
// the cap; a TRANSIENT, which may have, still is. The rest of the rule is the
// test above.
func TestALineRefusalTodayIsNoDialForTheCap(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	st := e.st
	a, cts := e.campaign(t, "line", "72", 2, nil)
	now := ny(t, "2026-06-01 15:00")
	dayStart := ny(t, "2026-06-01 00:00")

	refused, transient := cts[0].Number, cts[1].Number
	for _, at := range []string{"2026-06-01 09:00", "2026-06-01 09:05", "2026-06-01 09:20"} {
		rawAttempt(t, st, a, cts[0], campaign.OutcomeLineRejected, ny(t, at))
	}
	rawAttempt(t, st, a, cts[1], campaign.OutcomeTransient, ny(t, "2026-06-01 09:00"))
	rawAttempt(t, st, a, cts[1], campaign.OutcomeLineRejected, ny(t, "2026-06-01 09:05"))

	got, err := st.Dialer().DialsToday(ctx, []string{refused, transient}, []time.Time{dayStart, dayStart}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{0, 1}; !equalInts(got, want) {
		t.Errorf("dials today = %v, want %v (three line refusals; a TRANSIENT and a line refusal)", got, want)
	}

	// An attempt in flight counts whatever it comes to.
	if _, err := st.Pool.Exec(ctx, `UPDATE campaign_contacts SET status = 'ACTIVE', next_attempt_at = NULL WHERE id = $1`, cts[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.attempts.Begin(ctx, cts[0].ID, ny(t, "2026-06-01 14:00")); err != nil {
		t.Fatal(err)
	}
	got, err = st.Dialer().DialsToday(ctx, []string{refused}, []time.Time{dayStart}, now)
	if err != nil || len(got) != 1 || got[0] != 1 {
		t.Errorf("dials today with one in flight = %v (%v), want 1", got, err)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Each write of a due time leaves exactly one mark or none.
func TestEachDeferralLeavesOneMarkAndClearsTheOther(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, cts := e.campaign(t, "A", "72", 1, nil)
	d := e.st.Dialer()
	id := cts[0].ID
	marks := func() (capHeld, freqHeld bool) {
		t.Helper()
		if err := e.st.Pool.QueryRow(ctx, `SELECT is_daily_cap_held, is_frequency_held FROM campaign_contacts WHERE id = $1`, id).Scan(&capHeld, &freqHeld); err != nil {
			t.Fatal(err)
		}
		return
	}
	at := time.Now().Add(time.Hour)
	steps := []struct {
		hold     campaign.HoldKind
		wantCap  bool
		wantFreq bool
	}{
		{campaign.HoldDailyCap, true, false},
		{campaign.HoldFrequency, false, true},
		{campaign.HoldDailyCap, true, false},
		{campaign.HoldNone, false, false},
		{campaign.HoldFrequency, false, true},
		{campaign.HoldNone, false, false},
	}
	for i, s := range steps {
		at = at.Add(time.Hour)
		if err := d.Defer(ctx, []uuid.UUID{id}, []time.Time{at}, s.hold); err != nil {
			t.Fatal(err)
		}
		if capHeld, freqHeld := marks(); capHeld != s.wantCap || freqHeld != s.wantFreq {
			t.Errorf("step %d (%q): marks cap=%v frequency=%v, want %v/%v", i, s.hold, capHeld, freqHeld, s.wantCap, s.wantFreq)
		}
	}
	// A contact carries at most one mark.
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET is_daily_cap_held = true, is_frequency_held = true WHERE id = $1`, id); err == nil {
		t.Error("the table accepted a contact with both marks")
	}

	// The claim clears the cap's mark, and so does the settle.
	if err := d.Defer(ctx, []uuid.UUID{id}, []time.Time{at.Add(time.Hour)}, campaign.HoldDailyCap); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	att, err := e.attempts.Begin(ctx, id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if capHeld, _ := marks(); capHeld {
		t.Error("the claim left the cap's mark")
	}
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET is_daily_cap_held = true WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.attempts.Finish(ctx, att.CallID, campaign.Ending{
		Outcome: campaign.OutcomeNoAnswer, HangupCause: "NO_ANSWER", EndedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if capHeld, _ := marks(); capHeld {
		t.Error("the settle left the cap's mark")
	}
	_ = c
}

// The re-arm for a number that joined the list of test numbers lifts either
// mark and nothing else.
func TestRearmHeldLiftsEitherMarkOnAListedNumberOnly(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	_, cts := e.campaign(t, "A", "73", 5, nil)
	d := e.st.Dialer()
	later := time.Now().Add(6 * time.Hour).Truncate(time.Microsecond)
	capHeld, freqHeld, unmarked, notListed := cts[0], cts[1], cts[2], cts[3]
	for _, x := range []struct {
		ct   campaign.Contact
		hold campaign.HoldKind
	}{{capHeld, campaign.HoldDailyCap}, {freqHeld, campaign.HoldFrequency}, {notListed, campaign.HoldDailyCap}} {
		if err := d.Defer(ctx, []uuid.UUID{x.ct.ID}, []time.Time{later}, x.hold); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.Pool.Exec(ctx, `UPDATE campaign_contacts SET next_attempt_at = $2 WHERE id = $1`, unmarked.ID, later); err != nil {
		t.Fatal(err)
	}
	for _, ct := range []campaign.Contact{capHeld, freqHeld, unmarked} {
		if _, _, err := e.st.TestNumbers().Add(ctx, ct.Number, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := d.RearmHeld(ctx, now); err != nil {
		t.Fatal(err)
	}
	state := func(id uuid.UUID) (at time.Time, capMark, freqMark bool) {
		if err := e.st.Pool.QueryRow(ctx,
			`SELECT next_attempt_at, is_daily_cap_held, is_frequency_held FROM campaign_contacts WHERE id = $1`, id).Scan(&at, &capMark, &freqMark); err != nil {
			t.Fatal(err)
		}
		return
	}
	for name, ct := range map[string]campaign.Contact{"cap-held": capHeld, "frequency-held": freqHeld} {
		if at, c, f := state(ct.ID); at.After(now.Add(time.Second)) || c || f {
			t.Errorf("%s contact: due %v marks %v/%v, want due now and unmarked", name, at, c, f)
		}
	}
	if at, _, _ := state(unmarked.ID); !at.Equal(later) {
		t.Errorf("the unmarked contact moved to %v, want %v kept", at, later)
	}
	if at, c, _ := state(notListed.ID); !at.Equal(later) || !c {
		t.Errorf("a contact whose number is not listed: due %v cap mark %v, want it kept held", at, c)
	}
}

// The ledger writes the canonical callee of an outbound call, and a revised
// row rewrites it.
func TestTheLedgerWritesTheCanonicalCalleeOfAnOutboundCall(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	e.st.DefaultCountryCode = "86"
	l := e.st.Ledger()
	read := func(id uuid.UUID) *string {
		t.Helper()
		var v *string
		if err := e.st.Pool.QueryRow(ctx, `SELECT to_number_e164 FROM cdrs WHERE call_id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	national := adHocCDR(t, e.st, "13800138000", "OUTBOUND", ny(t, "2026-06-01 09:00"))
	if v := read(national); v == nil || *v != "+8613800138000" {
		t.Errorf("national callee: %v, want +8613800138000", v)
	}
	if v := read(adHocCDR(t, e.st, "+442071838750", "OUTBOUND", ny(t, "2026-06-01 09:00"))); v == nil || *v != "+442071838750" {
		t.Errorf("E.164 callee: %v", v)
	}
	if v := read(adHocCDR(t, e.st, "95001", "OUTBOUND", ny(t, "2026-06-01 09:00"))); v != nil {
		t.Errorf("an extension-like callee got %q, want NULL", *v)
	}
	if v := read(adHocCDR(t, e.st, "13800138000", "INBOUND", ny(t, "2026-06-01 09:00"))); v != nil {
		t.Errorf("an inbound call got %q, want NULL", *v)
	}

	// A later ending of the same call replaces the row, and its callee with it.
	revised := CDR{
		CallID: national, StartedAt: ny(t, "2026-06-01 09:00"), EndedAt: ny(t, "2026-06-01 09:10"), CallType: "OUTBOUND",
		FromNumber: "95099", ToNumber: "13900139000", DID: "95099", Status: CDRStatusAnswered,
	}
	if err := l.InsertCDR(ctx, revised); err != nil {
		t.Fatal(err)
	}
	if v := read(national); v == nil || *v != "+8613900139000" {
		t.Errorf("revised callee: %v, want +8613900139000", v)
	}
}
