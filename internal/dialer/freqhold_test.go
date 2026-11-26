// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// #593: a frequency-held contact whose own retry time comes before the hold
// lifts would sit at the head of the list reporting a nextDialAt at which the
// dialer only defers it. The dialer defers it to the hold's release as soon as
// it sees it, like refill would when the retry time came, so the first report
// already names a time a call can go out.

// The gap case of the issue: B dials, the call ends unanswered and the contact
// is due again at noon; A touches the same number at 10:30, so B may not dial
// it for 24 h. From 10:30 the report is WAITING_FREQUENCY with nextDialAt at
// the hold's end — not noon, a time at which nothing could be placed.
func TestAHeldContactIsDeferredToTheFrequencyReleaseAtTheFirstReport(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("76", 1)
	n := specs[0].number
	a := r.otherCampaign("A", specs)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)

	// B's own dial ends unanswered; the contact is due again at noon.
	r.end(r.placer.liveCalls()[0], endNoAnswer)
	r.settle()
	r.setNextAttempt(b, n, ny(t, "2026-06-01 12:00"))
	// A touches the number at 10:30; the 24 h window binds until 10:30
	// tomorrow.
	r.at(ny(t, "2026-06-01 10:30"))
	r.dialed(a, n, campaign.OutcomeNoAnswer, r.clock.Now())
	r.dialer().Wake()
	r.tick(time.Second)

	release := ny(t, "2026-06-02 10:30")
	p := r.progress(b)
	if p.Activity != campaign.ActivityWaitingFrequency || p.NextDialAt == nil || !p.NextDialAt.Equal(release) {
		t.Fatalf("progress = %+v, want WAITING_FREQUENCY until %v", p, release)
	}
	ct := r.contactOf(b, n)
	if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(release) {
		t.Errorf("the contact is due %v, want the release %v", ct.NextAttemptAt, release)
	}
	if _, freqHeld := r.marks(b, n); !freqHeld {
		t.Error("the deferred contact carries no frequency mark")
	}
	if got := r.placer.placedTo(n); got != 1 {
		t.Errorf("placed %d times, want 1: nothing dialled at a mere retry time", got)
	}

	// Listing the number re-arms the mark: the contact is due at once and is
	// dialled.
	r.listTestNumber(n)
	if got := r.placer.placedTo(n); got != 2 {
		t.Errorf("placed %d times after listing, want the 2nd", got)
	}
	if _, freqHeld := r.marks(b, n); freqHeld {
		t.Error("the mark survived the listing")
	}
	r.noViolations()
}

// The calling window applies to the release as it does to any dial time: a
// hold that ends while the window is shut releases at its next opening.
func TestAFrequencyReleaseOutsideTheCallingHoursMovesToTheOpening(t *testing.T) {
	r := newRig(t, opts{outer: "09:00-18:00", start: ny(t, "2026-06-01 08:00")})
	specs := numbers("77", 1)
	n := specs[0].number
	a := r.otherCampaign("A", specs)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	// The window is shut at 08:00, so B's first dial waits for 09:00.
	r.tick(time.Second)
	if got := r.placer.placedTo(n); got != 0 {
		t.Fatalf("%d placements before the window opens, want 0", got)
	}
	r.at(ny(t, "2026-06-01 09:00"))
	r.end(r.placer.liveCalls()[0], endNoAnswer)
	r.settle()
	r.setNextAttempt(b, n, ny(t, "2026-06-01 12:00"))

	// A touches the number at 10:30: the hold ends at 10:30 tomorrow, inside
	// the window, so the release stays.
	r.at(ny(t, "2026-06-01 10:30"))
	r.dialed(a, n, campaign.OutcomeNoAnswer, r.clock.Now())
	r.dialer().Wake()
	r.tick(time.Second)
	if release := ny(t, "2026-06-02 10:30"); true {
		p := r.progress(b)
		if p.Activity != campaign.ActivityWaitingFrequency || p.NextDialAt == nil || !p.NextDialAt.Equal(release) {
			t.Fatalf("progress = %+v, want WAITING_FREQUENCY until %v", p, release)
		}
	}

	// A second contact, held by a touch at 08:30 this morning (written for
	// another campaign so B's only dial stands): the hold ends at 08:30
	// tomorrow, before the window opens, and the release is the 09:00
	// opening.
	specs2 := numbers("78", 1)
	n2 := specs2[0].number
	a2 := r.otherCampaign("A2", specs2)
	r.importContacts(b, specs2)
	r.dialed(a2, n2, campaign.OutcomeNoAnswer, ny(t, "2026-06-01 08:30"))
	r.setNextAttempt(b, n2, ny(t, "2026-06-01 12:00"))
	r.dialer().Wake()
	r.tick(time.Second)

	release := ny(t, "2026-06-02 09:00")
	ct := r.contactOf(b, n2)
	if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(release) {
		t.Errorf("the window-bound contact is due %v, want the opening %v", ct.NextAttemptAt, release)
	}
	if _, freqHeld := r.marks(b, n2); !freqHeld {
		t.Error("the window-bound contact carries no frequency mark")
	}
	r.noViolations()
}

// With the held contact deferred out of the way, the earliest pending one
// left is an unheld contact and the report is its own retry time.
func TestAnUnheldContactAheadOfTheReleaseKeepsTheRetryReport(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("79", 2)
	held, free := specs[0].number, specs[1].number
	a := r.otherCampaign("A", specs[:1])
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	r.endAll(func(string) string { return endNoAnswer })
	r.settle()

	// B1 retried at noon and held by A's 10:30 touch; B2 retried tomorrow at
	// 10:05 — after B's own touch on it (10:0x + 24 h), before the hold on
	// the shared number lifts (10:30 tomorrow).
	r.setNextAttempt(b, held, ny(t, "2026-06-01 12:00"))
	freeAt := ny(t, "2026-06-02 10:05")
	r.setNextAttempt(b, free, freeAt)
	r.at(ny(t, "2026-06-01 10:30"))
	r.dialed(a, held, campaign.OutcomeNoAnswer, r.clock.Now())
	r.dialer().Wake()
	r.tick(time.Second)

	p := r.progress(b)
	if p.Activity != campaign.ActivityWaitingRetry || p.NextDialAt == nil || !p.NextDialAt.Equal(freeAt) {
		t.Fatalf("progress = %+v, want WAITING_RETRY until the unheld contact's %v", p, freeAt)
	}
	if ct := r.contactOf(b, held); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(ny(t, "2026-06-02 10:30")) {
		t.Errorf("the held contact is due %v, want its release", ct.NextAttemptAt)
	}
	if ct := r.contactOf(b, free); ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(freeAt) {
		t.Errorf("the unheld contact moved to %v, want %v kept", ct.NextAttemptAt, freeAt)
	}
	if _, freqHeld := r.marks(b, free); freqHeld {
		t.Error("the unheld contact was marked")
	}
	r.noViolations()
}

// A contact whose own retry time comes after the hold's release is not moved
// and carries no mark: at its retry time the number is free to dial.
func TestAContactWhoseRetryComesAfterTheFrequencyReleaseIsNotMoved(t *testing.T) {
	r := newRig(t, opts{})
	specs := numbers("80", 1)
	n := specs[0].number
	a := r.otherCampaign("A", specs)
	b := r.newCampaign("B", nil)
	r.importContacts(b, specs)
	r.start(b)
	r.end(r.placer.liveCalls()[0], endNoAnswer)
	r.settle()

	// A touches at 10:30 (hold until tomorrow 10:30); the contact's own retry
	// is tomorrow at 11:00, later still.
	r.at(ny(t, "2026-06-01 10:30"))
	r.dialed(a, n, campaign.OutcomeNoAnswer, r.clock.Now())
	later := ny(t, "2026-06-02 11:00")
	r.setNextAttempt(b, n, later)
	r.dialer().Wake()
	r.tick(time.Second)

	ct := r.contactOf(b, n)
	if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(later) {
		t.Errorf("the contact is due %v, want its own retry time %v kept", ct.NextAttemptAt, later)
	}
	if _, freqHeld := r.marks(b, n); freqHeld {
		t.Error("a contact whose retry is after the release was marked")
	}
	p := r.progress(b)
	if p.Activity != campaign.ActivityWaitingRetry || p.NextDialAt == nil || !p.NextDialAt.Equal(later) {
		t.Errorf("progress = %+v, want WAITING_RETRY until %v", p, later)
	}
	r.noViolations()
}
