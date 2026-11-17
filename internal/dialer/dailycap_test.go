// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// capRig is a rig with a daily cap of three and no frequency window to speak of
// (the window is one nanosecond), so the cap is the only limit under test.
func capRig(t *testing.T, o opts) *rig {
	t.Helper()
	if o.dailyCap == 0 {
		o.dailyCap = 3
	}
	if o.frequency == 0 {
		o.frequency = time.Nanosecond
	}
	return newRig(t, o)
}

// dialed writes a finished attempt of campaign c at the contact with that
// number, as if the dialer had placed it then.
func (r *rig) dialed(c campaign.Campaign, number string, outcome campaign.Outcome, at time.Time) {
	r.t.Helper()
	ct := r.contactOf(c, number)
	if _, err := r.st.Pool.Exec(context.Background(),
		`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id,
		     state, outcome, is_touched, started_at, finished_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'FINISHED', $7, $8, $9, $9)`,
		uuid.Must(uuid.NewV7()), c.ID, ct.ID, number, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()),
		string(outcome), campaign.IsTouched(outcome), at); err != nil {
		r.t.Fatal(err)
	}
}

// marks says whether the contact carries the daily cap's mark, and the
// frequency window's.
func (r *rig) marks(c campaign.Campaign, number string) (dailyCap, frequency bool) {
	r.t.Helper()
	ct := r.contactOf(c, number)
	if err := r.st.Pool.QueryRow(context.Background(),
		`SELECT is_daily_cap_held, is_frequency_held FROM campaign_contacts WHERE id = $1`, ct.ID).Scan(&dailyCap, &frequency); err != nil {
		r.t.Fatal(err)
	}
	return
}

// A draft campaign that only holds contacts, so attempts can be written for a
// number as another campaign's.
func (r *rig) otherCampaign(name string, specs []contactSpec) campaign.Campaign {
	r.t.Helper()
	c := r.newCampaign(name, nil)
	r.importContacts(c, specs)
	return c
}

func (r *rig) adHocCall(number string, at time.Time, kind string) {
	r.t.Helper()
	cdr := cdrFor(uuid.Must(uuid.NewV7()), number, kind, at, at.Add(30*time.Second))
	if err := r.st.Ledger().InsertCDR(context.Background(), cdr); err != nil {
		r.t.Fatal(err)
	}
}

func TestANumberAtTheDailyCapIsNotDialledAndWaitsForTomorrowsWindow(t *testing.T) {
	r := capRig(t, opts{})
	specs := numbers("61", 1)
	n := specs[0].number
	a := r.otherCampaign("A", specs)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	// Three dials today: two in B's own campaign (a no-answer and a system
	// failure), one from A.
	r.dialed(b, n, campaign.OutcomeNoAnswer, ny(t, "2026-06-01 08:00"))
	r.dialed(b, n, campaign.OutcomeTransient, ny(t, "2026-06-01 08:30"))
	r.dialed(a, n, campaign.OutcomeBusy, ny(t, "2026-06-01 09:00"))

	r.start(b)
	if got := r.placer.placedTo(n); got != 0 {
		t.Fatalf("placed %d times, want none: the number is at the cap", got)
	}
	release := ny(t, "2026-06-02 09:00")
	ct := r.contactOf(b, n)
	if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(release) {
		t.Fatalf("the contact is due %v, want the next day's window %v", ct.NextAttemptAt, release)
	}
	if ct.Attempts != 0 || ct.SystemFailures != 0 {
		t.Errorf("the contact spent budget (attempts %d, system failures %d), want none", ct.Attempts, ct.SystemFailures)
	}
	if capHeld, freqHeld := r.marks(b, n); !capHeld || freqHeld {
		t.Errorf("marks: dailyCap %v frequency %v, want only the cap's", capHeld, freqHeld)
	}
	p := r.progress(b)
	if p.Activity != campaign.ActivityWaitingDailyCap || p.NextDialAt == nil || !p.NextDialAt.Equal(release) {
		t.Fatalf("progress = %+v, want WAITING_DAILY_CAP until %v", p, release)
	}

	r.at(release)
	if got := r.placer.placedTo(n); got != 1 {
		t.Errorf("placed %d times when the next day's window opened, want 1", got)
	}
	if capHeld, _ := r.marks(b, n); capHeld {
		t.Error("the claim left the cap's mark")
	}
	r.noViolations()
}

func TestAListedNumberIsDialledPastTheCapAndHeldOnceTakenOffTheList(t *testing.T) {
	r := capRig(t, opts{})
	specs := numbers("62", 1)
	n := specs[0].number
	r.listTestNumber(n)
	a := r.otherCampaign("A", specs)
	for i := 0; i < 3; i++ {
		r.dialed(a, n, campaign.OutcomeNoAnswer, ny(t, fmt.Sprintf("2026-06-01 0%d:00", 6+i)))
	}
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if got := r.placer.placedTo(n); got != 1 {
		t.Fatalf("placed %d times, want the 4th dial of the day for a listed number", got)
	}
	live := r.placer.liveCalls()
	r.end(live[0], endNoAnswer)
	r.settle()

	if err := r.st.TestNumbers().Remove(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	c := r.newCampaign("C", nil)
	r.importContacts(c, specs)
	r.start(c)
	if got := r.placer.placedTo(n); got != 1 {
		t.Errorf("placed %d times after the number left the list, want no further dial today", got)
	}
	p := r.progress(c)
	if want := ny(t, "2026-06-02 09:00"); p.Activity != campaign.ActivityWaitingDailyCap || p.NextDialAt == nil || !p.NextDialAt.Equal(want) {
		t.Errorf("progress = %+v, want WAITING_DAILY_CAP until %v", p, want)
	}
}

func TestListingANumberLiftsADailyCapHoldAndOnlyThatHold(t *testing.T) {
	r := capRig(t, opts{})
	specs := numbers("63", 2)
	held, own := specs[0].number, specs[1].number
	a := r.otherCampaign("A", specs)
	for _, n := range []string{held, own} {
		for i := 0; i < 3; i++ {
			r.dialed(a, n, campaign.OutcomeNoAnswer, ny(t, fmt.Sprintf("2026-06-01 0%d:00", 6+i)))
		}
	}
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if capHeld, _ := r.marks(b, held); !capHeld {
		t.Fatal("the capped contact carries no mark")
	}
	// The other contact waits on its own retry time, later today, with no mark.
	ownAt := ny(t, "2026-06-01 15:00")
	if _, err := r.st.Pool.Exec(context.Background(),
		`UPDATE campaign_contacts SET next_attempt_at = $2, is_daily_cap_held = false WHERE id = $1`, r.contactOf(b, own).ID, ownAt); err != nil {
		t.Fatal(err)
	}
	// Both numbers are placed in the list; only the marked contact is re-armed.
	r.listTestNumber(held)
	r.listTestNumber(own)
	if got := r.placer.placedTo(held); got != 1 {
		t.Errorf("the capped contact was placed %d times after listing, want at once", got)
	}
	if got := r.placer.placedTo(own); got != 0 {
		t.Errorf("the contact on its own retry time was placed %d times, want none", got)
	}
	if ct := r.contactOf(b, own); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(ownAt) {
		t.Errorf("the own retry time moved to %v, want it kept at %v", ct.NextAttemptAt, ownAt)
	}
}

func TestTheDayOfTheCapIsTheContactsLocalDay(t *testing.T) {
	// The campaign is in New York and its window is open all day; the contacts
	// are in Shanghai or in the campaign's zone. 21:00 in New York is 09:00 on
	// the 2nd in Shanghai.
	r := capRig(t, opts{outer: "00:00-24:00", start: ny(t, "2026-06-01 21:00")})
	allDay := func(s *camSpec) {
		s.window = campaign.CallingWindow{Days: everyDay, Ranges: []campaign.TimeRange{{Start: "00:00", End: "24:00"}}}
	}
	specs := []contactSpec{
		{number: "+8613640000001", zone: "Asia/Shanghai"}, // yesterday's dials in its own day
		{number: "+8613640000002"},                        // the campaign's zone
		{number: "+8613640000003", zone: "Asia/Shanghai"}, // today's dials in its own day
	}
	a := r.newCampaign("A", allDay)
	r.importContacts(a, specs)
	b := r.newCampaign("B", allDay)
	r.importContacts(b, specs)
	at := func(n int, local string) {
		for i := 0; i < 3; i++ {
			r.dialed(a, specs[n].number, campaign.OutcomeNoAnswer, ny(t, local).Add(time.Duration(i)*time.Minute))
		}
	}
	at(0, "2026-06-01 11:00") // 23:00 on the 1st in Shanghai, 11:00 on the 1st in New York
	at(1, "2026-06-01 11:00") // the same dials, in New York's today
	at(2, "2026-06-01 12:30") // 00:30 on the 2nd in Shanghai: Shanghai's today

	r.start(b)
	if got := r.placer.placedTo(specs[0].number); got != 1 {
		t.Errorf("the Shanghai contact whose dials were before its midnight was placed %d times, want 1", got)
	}
	if got := r.placer.placedTo(specs[1].number); got != 0 {
		t.Errorf("the New York contact with the same dials was placed %d times, want none", got)
	}
	if got := r.placer.placedTo(specs[2].number); got != 0 {
		t.Errorf("the Shanghai contact with dials after its midnight was placed %d times, want none", got)
	}
	// Each is held to the start of its next local day.
	if ct := r.contactOf(b, specs[1].number); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(ny(t, "2026-06-02 00:00")) {
		t.Errorf("the New York contact is due %v, want its next midnight", ct.NextAttemptAt)
	}
	shanghaiMidnight := time.Date(2026, 6, 3, 0, 0, 0, 0, mustZone(t, "Asia/Shanghai"))
	if ct := r.contactOf(b, specs[2].number); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(shanghaiMidnight) {
		t.Errorf("the Shanghai contact is due %v, want %v", ct.NextAttemptAt, shanghaiMidnight)
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestWhatCountsTowardTheDailyCap(t *testing.T) {
	r := capRig(t, opts{})
	r.st.DefaultCountryCode = "86"
	specs := numbers("65", 3)
	skipped, adHoc, once := specs[0].number, specs[1].number, specs[2].number
	a := r.otherCampaign("A", specs)

	// A DNC skip and an invalid number are no dials: three of them leave the
	// number free.
	for i := 0; i < 2; i++ {
		r.dialed(a, skipped, campaign.OutcomeDNCSkipped, ny(t, "2026-06-01 08:00"))
		r.dialed(a, skipped, campaign.OutcomeInvalidNumber, ny(t, "2026-06-01 08:10"))
	}
	// An ad-hoc call to the number in its national form counts, like an attempt.
	r.dialed(a, adHoc, campaign.OutcomeOriginateFailed, ny(t, "2026-06-01 08:00"))
	r.dialed(a, adHoc, campaign.OutcomeNoAnswer, ny(t, "2026-06-01 08:10"))
	r.adHocCall(adHoc[3:], ny(t, "2026-06-01 08:20"), endNoAnswer) // "+86" removed: 13...
	// An ad-hoc call of yesterday does not.
	r.adHocCall(skipped[3:], ny(t, "2026-05-31 08:20"), endNoAnswer)

	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if got := r.placer.placedTo(skipped); got != 1 {
		t.Errorf("the number with only DNC skips and bad-number attempts was placed %d times, want 1", got)
	}
	if got := r.placer.placedTo(adHoc); got != 0 {
		t.Errorf("the number with two attempts and an ad-hoc call was placed %d times, want none", got)
	}
	if got := r.placer.placedTo(once); got != 1 {
		t.Errorf("an untouched number was placed %d times, want 1", got)
	}
}

func TestACampaignCallIsCountedOnceNotAsAnAttemptAndACDR(t *testing.T) {
	r := capRig(t, opts{dailyCap: 2})
	specs := numbers("66", 1)
	n := specs[0].number
	a := r.newCampaign("A", nil)
	r.importContacts(a, specs)
	r.start(a)
	r.end(r.placer.liveCalls()[0], endNoAnswer) // attempt and CDR: one dial
	r.settle()
	if _, err := r.dialer().Cancel(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	r.tick(time.Minute)

	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if got := r.placer.placedTo(n); got != 2 {
		t.Fatalf("placed %d times, want B's dial too: one dial so far, the cap is 2", got)
	}
	r.end(r.placer.liveCalls()[0], endNoAnswer)
	r.settle()
	if _, err := r.dialer().Cancel(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	c := r.newCampaign("C", nil)
	r.importContacts(c, specs)
	r.start(c)
	if got := r.placer.placedTo(n); got != 2 {
		t.Errorf("placed %d times, want none after the second dial", got)
	}
	if p := r.progress(c); p.Activity != campaign.ActivityWaitingDailyCap {
		t.Errorf("activity = %s, want WAITING_DAILY_CAP", p.Activity)
	}
}

func TestACapOfZeroAdmitsAsBefore(t *testing.T) {
	r := newRig(t, opts{frequency: time.Nanosecond}) // dailyCap 0
	specs := numbers("67", 1)
	n := specs[0].number
	a := r.otherCampaign("A", specs)
	for i := 0; i < 5; i++ {
		r.dialed(a, n, campaign.OutcomeNoAnswer, ny(t, fmt.Sprintf("2026-06-01 0%d:00", 1+i)))
	}
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if got := r.placer.placedTo(n); got != 1 {
		t.Errorf("placed %d times with the cap off, want 1", got)
	}
}

// The campaign waits for the latest of the cap, the frequency window and the
// calling window; a tie goes to the cap, then the frequency window.
func TestTheDailyCapOutranksTheWindowAndTheFrequencyWindowOnATie(t *testing.T) {
	t.Run("cap later than the window", func(t *testing.T) {
		// 00:30: the window opens at 09:00 today, the cap ends at 09:00 tomorrow.
		r := capRig(t, opts{start: ny(t, "2026-06-02 00:30")})
		specs := numbers("68", 1)
		a := r.otherCampaign("A", specs)
		for i := 0; i < 3; i++ {
			r.dialed(a, specs[0].number, campaign.OutcomeNoAnswer, ny(t, fmt.Sprintf("2026-06-02 00:0%d", 1+i)))
		}
		b := r.newCampaign("B", nil)
		r.importContacts(b, specs)
		r.start(b)
		if p := r.progress(b); p.Activity != campaign.ActivityWaitingDailyCap {
			t.Errorf("activity = %s, want WAITING_DAILY_CAP", p.Activity)
		}
	})
	t.Run("tie with the window", func(t *testing.T) {
		// 19:00: closed; both the window and the cap end at 09:00 tomorrow.
		r := capRig(t, opts{start: ny(t, "2026-06-01 19:00")})
		specs := numbers("69", 1)
		a := r.otherCampaign("A", specs)
		for i := 0; i < 3; i++ {
			r.dialed(a, specs[0].number, campaign.OutcomeNoAnswer, ny(t, fmt.Sprintf("2026-06-01 1%d:00", 1+i)))
		}
		b := r.newCampaign("B", nil)
		r.importContacts(b, specs)
		r.start(b)
		// The contact is deferred by the window before the cap is judged, and it
		// is the cap's fact that names the activity.
		if p := r.progress(b); p.Activity != campaign.ActivityWaitingDailyCap {
			t.Errorf("activity = %s, want the cap to win the tie with the window", p.Activity)
		}
	})
	t.Run("tie with the frequency window", func(t *testing.T) {
		r := capRig(t, opts{frequency: 24 * time.Hour, start: ny(t, "2026-06-01 10:00")})
		specs := numbers("70", 1)
		n := specs[0].number
		a := r.otherCampaign("A", specs)
		// A touched the number at 09:00: the window ends tomorrow 09:00, when the
		// cap releases it too.
		r.dialed(a, n, campaign.OutcomeNoAnswer, ny(t, "2026-06-01 09:00"))
		r.dialed(a, n, campaign.OutcomeTransient, ny(t, "2026-06-01 08:00"))
		r.dialed(a, n, campaign.OutcomeTransient, ny(t, "2026-06-01 08:30"))
		b := r.newCampaign("B", nil)
		r.importContacts(b, specs)
		r.start(b)
		if p := r.progress(b); p.Activity != campaign.ActivityWaitingDailyCap {
			t.Errorf("activity = %s, want WAITING_DAILY_CAP on a tie", p.Activity)
		}
	})
	t.Run("frequency window later than the cap", func(t *testing.T) {
		r := capRig(t, opts{frequency: 24 * time.Hour, start: ny(t, "2026-06-01 12:00")})
		specs := numbers("71", 1)
		n := specs[0].number
		a := r.otherCampaign("A", specs)
		r.dialed(a, n, campaign.OutcomeNoAnswer, ny(t, "2026-06-01 11:30")) // ends tomorrow 11:30
		r.dialed(a, n, campaign.OutcomeTransient, ny(t, "2026-06-01 08:00"))
		r.dialed(a, n, campaign.OutcomeTransient, ny(t, "2026-06-01 08:30"))
		b := r.newCampaign("B", nil)
		r.importContacts(b, specs)
		r.start(b)
		if p := r.progress(b); p.Activity != campaign.ActivityWaitingFrequency {
			t.Errorf("activity = %s, want WAITING_FREQUENCY: it ends after the cap", p.Activity)
		}
	})
}
