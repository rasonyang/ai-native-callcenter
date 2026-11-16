// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// touch has campaign a reach each number once (NO_ANSWER counts against
// maxAttempts) and cancels a, so nothing of a is left to dial. It returns when
// the touch happened.
func (r *rig) touch(a campaign.Campaign, nums []contactSpec) time.Time {
	r.t.Helper()
	r.importContacts(a, nums)
	r.start(a)
	at := r.clock.Now()
	for _, l := range r.placer.liveCalls() {
		r.end(l, endNoAnswer)
	}
	r.settle()
	if _, err := r.dialer().Cancel(context.Background(), a.ID); err != nil {
		r.t.Fatal(err)
	}
	r.settle()
	return at
}

func (r *rig) listTestNumber(number string) {
	r.t.Helper()
	if _, _, err := r.st.TestNumbers().Add(context.Background(), number, "", nil); err != nil {
		r.t.Fatal(err)
	}
	r.settle()
}

// Campaign A reached two numbers; campaign B has both. The one on the list of
// test numbers is dialled at once, the other waits out the frequency window.
func TestATestNumberIsNotHeldByTheFrequencyWindow(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("47", 2)
	listed, plain := specs[0].number, specs[1].number
	r.listTestNumber(listed)
	touchedAt := r.touch(r.newCampaign("A", nil), specs)

	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)

	if got := r.placer.placedTo(listed); got != 2 {
		t.Errorf("the listed number was placed %d times, want 2: A's call and B's at once", got)
	}
	if got := r.placer.placedTo(plain); got != 1 {
		t.Errorf("the other number was placed %d times, want only A's", got)
	}
	ct := r.contactOf(b, plain)
	if want := touchedAt.Add(24 * time.Hour); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(want) {
		t.Errorf("B's held contact is due %v, want the touch plus the window %v", ct.NextAttemptAt, want)
	}
	r.noViolations()
}

// The campaign whose only due contact is held says why, and when it will dial.
// Putting the number on the list lifts the hold at once.
func TestACampaignHeldByTheFrequencyWindowSaysSoAndTheListLiftsIt(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("48", 1)
	touchedAt := r.touch(r.newCampaign("A", nil), specs)

	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	p := r.progress(b)
	want := touchedAt.Add(24 * time.Hour)
	if p.Activity != campaign.ActivityWaitingFrequency || p.NextDialAt == nil || !p.NextDialAt.Equal(want) {
		t.Fatalf("progress = %+v, want WAITING_FREQUENCY until %v", p, want)
	}
	if got := r.placer.placedTo(specs[0].number); got != 1 {
		t.Fatalf("placed %d times, want only A's", got)
	}
	r.listTestNumber(specs[0].number)
	if got := r.placer.placedTo(specs[0].number); got != 2 {
		t.Errorf("placed %d times after listing, want B's call at once", got)
	}
	if a := r.progress(b).Activity; a != campaign.ActivityDialing {
		t.Errorf("activity = %s, want DIALING", a)
	}
	r.noViolations()
}

// A frequency window of zero never holds anything, so never says so.
func TestNoFrequencyWindowNeverWaitsForIt(t *testing.T) {
	r := newRig(t, opts{frequency: time.Nanosecond})
	specs := numbers("49", 1)
	r.touch(r.newCampaign("A", nil), specs)
	r.tick(time.Second)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if got := r.placer.placedTo(specs[0].number); got != 2 {
		t.Errorf("placed %d times, want B to dial once the 1 ns window is over", got)
	}
	if a := r.progress(b).Activity; a == campaign.ActivityWaitingFrequency {
		t.Errorf("activity = %s with nothing held", a)
	}
}

// When the calling window opens later than the hold ends, the campaign is
// waiting for the window.
func TestTheLaterOfTheHoldAndTheCallingWindowIsWhatTheCampaignWaitsFor(t *testing.T) {
	r := newRig(t, opts{frequency: 2 * time.Hour, start: ny(t, "2026-06-01 17:00")})
	specs := numbers("50", 1)
	r.touch(r.newCampaign("A", nil), specs)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if a := r.progress(b).Activity; a != campaign.ActivityWaitingFrequency {
		t.Fatalf("at 17:00 the activity is %s, want WAITING_FREQUENCY until 19:00", a)
	}
	// 18:30: the window has closed and the hold ends at 19:00, before the
	// window reopens at 09:00.
	r.at(ny(t, "2026-06-01 18:30"))
	if a := r.progress(b).Activity; a != campaign.ActivityWaitingWindow {
		t.Errorf("at 18:30 the activity is %s, want WAITING_WINDOW", a)
	}
}

// A listed number is still never dialled when it is on the do-not-call list.
func TestAListedNumberOnTheDoNotCallListIsNeverDialled(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("51", 1)
	n := specs[0].number
	r.touch(r.newCampaign("A", nil), specs)
	r.listTestNumber(n)
	if _, _, err := r.st.DNC().Add(context.Background(), n, "MANUAL", nil, nil); err != nil {
		t.Fatal(err)
	}
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if got := r.placer.placedTo(n); got != 1 {
		t.Errorf("placed %d times, want only A's: DNC outranks the list", got)
	}
	if ct := r.contactOf(b, n); ct.Status != campaign.ContactDoNotCall {
		t.Errorf("B's contact is %s, want DNC", ct.Status)
	}
}

// A listed number that another campaign is dialling right now is skipped until
// that call ends, then dialled at once: the list exempts the window only.
func TestAListedNumberBeingDialledElsewhereWaitsForThatCall(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("52", 1)
	n := specs[0].number
	r.listTestNumber(n)
	a := r.newCampaign("A", nil)
	r.importContacts(a, specs)
	r.start(a)
	live := r.placer.liveCalls()
	if len(live) != 1 {
		t.Fatalf("A has %d calls live, want 1", len(live))
	}

	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if got := r.placer.placedTo(n); got != 1 {
		t.Fatalf("placed %d times while A is dialling it, want only A's", got)
	}
	r.end(live[0], endNoAnswer)
	r.settle()
	if got := r.placer.placedTo(n); got != 2 {
		t.Errorf("placed %d times after A's call ended, want B's at once", got)
	}
	r.noViolations()
}

// A number listed while the campaign that waited for it was paused is dialled
// as soon as the campaign resumes: the nudge of the add reached no running
// campaign, so resuming runs the re-arm itself.
func TestANumberListedWhileTheCampaignIsPausedIsDialledOnResume(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("53", 1)
	n := specs[0].number
	r.touch(r.newCampaign("A", nil), specs)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if a := r.progress(b).Activity; a != campaign.ActivityWaitingFrequency {
		t.Fatalf("activity = %s, want WAITING_FREQUENCY", a)
	}
	if _, err := r.dialer().Pause(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.listTestNumber(n)
	if got := r.placer.placedTo(n); got != 1 {
		t.Fatalf("placed %d times while paused, want only A's", got)
	}
	if _, err := r.dialer().Resume(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	if got := r.placer.placedTo(n); got != 2 {
		t.Errorf("placed %d times after resume, want B's at once", got)
	}
}

// A number listed while the dialer was down (or whose nudge was lost) is
// found by the dialer's first pass.
func TestADialerStartedWithAHeldContactOnAListedNumberDialsIt(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("54", 1)
	n := specs[0].number
	r.touch(r.newCampaign("A", nil), specs)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	if a := r.progress(b).Activity; a != campaign.ActivityWaitingFrequency {
		t.Fatalf("activity = %s, want WAITING_FREQUENCY", a)
	}
	r.stopDialer()
	if _, _, err := r.st.TestNumbers().Add(context.Background(), n, "", nil); err != nil {
		t.Fatal(err)
	}
	r.startDialer()
	r.settle()
	if got := r.placer.placedTo(n); got != 2 {
		t.Errorf("placed %d times after the restart, want B's at once", got)
	}
}
