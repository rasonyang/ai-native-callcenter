// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// The further cases of #616: other shapes of window change, the outer hours at
// a restart, a call in flight, and the re-arm's own pace.

// countingStore counts the re-arm's reads.
type countingStore struct {
	Store
	waits atomic.Int32
}

func (s *countingStore) Waiting(ctx context.Context, id uuid.UUID, now time.Time, after uuid.UUID, limit int) ([]campaign.Waiting, error) {
	s.waits.Add(1)
	return s.Store.Waiting(ctx, id, now, after, limit)
}

// counted restarts the rig's dialer on a store that counts the re-arm's reads.
func (r *rig) counted() *countingStore {
	r.t.Helper()
	r.stopDialer()
	cs := &countingStore{Store: r.dep.Store}
	r.dep.Store = cs
	r.startDialer()
	r.settle()
	return cs
}

// shortRetries is a policy that retries a no-answer after 15 minutes and a
// busy line after an hour.
func shortRetries() *campaign.RetryPolicy {
	p := campaign.DefaultRetryPolicy()
	p.IntervalSec[campaign.OutcomeNoAnswer] = 15 * 60
	p.IntervalSec[campaign.OutcomeBusy] = 60 * 60
	return &p
}

func (r *rig) wantDue(c campaign.Campaign, number string, at time.Time) {
	r.t.Helper()
	if got := r.row(c, number); !got.due(at) {
		r.t.Errorf("%s: %v, want due %v", number, got, at.In(shanghai).Format("Mon 15:04"))
	}
}

func (r *rig) wantDialledAt(at time.Time, numbers ...string) {
	r.t.Helper()
	for _, n := range numbers {
		if got := r.liveTo(n); !got.At.Equal(at) {
			r.t.Errorf("%s dialled at %v, want %v", n, got.At.In(shanghai), at.In(shanghai))
		}
	}
}

// Adding Saturday on a Friday evening: what waited for Monday's opening is due
// at Saturday's, the retry and the contact never tried alike.
func TestADayAddedToTheWindowBringsWaitingContactsForward(t *testing.T) {
	r := newRig(t, opts{start: sh(t, "2026-06-05 17:00")}) // a Friday
	n := numbers("82", 2)
	tried, untried := n[0].number, n[1].number
	c := r.newCampaign("Saturday", func(s *camSpec) {
		s.zone, s.policy = "Asia/Shanghai", shortRetries()
		s.window = calling(weekdays, "09:00", "17:30")
	})
	r.importContacts(c, n[:1])
	r.start(c)
	r.at(sh(t, "2026-06-05 17:20"))
	r.endAt(endNoAnswer, tried) // retry 17:35: past the close, so Monday 09:00
	r.at(sh(t, "2026-06-05 17:31"))
	r.importAt(c, n[1:])
	monday := sh(t, "2026-06-08 09:00")
	r.wantDue(c, tried, monday)
	r.wantDue(c, untried, monday)

	r.at(sh(t, "2026-06-05 17:40"))
	withSaturday := calling([]string{"MON", "TUE", "WED", "THU", "FRI", "SAT"}, "09:00", "17:30")
	r.patch(c, campaign.Patch{CallingWindow: &withSaturday})
	saturday := sh(t, "2026-06-06 09:00")
	r.wantDue(c, tried, saturday)
	r.wantDue(c, untried, saturday)
	if p := r.progress(c); p.Activity != campaign.ActivityWaitingWindow || p.NextDialAt == nil || !p.NextDialAt.Equal(saturday) {
		t.Errorf("progress = %+v, want WAITING_WINDOW until Saturday 09:00", p)
	}
	r.at(saturday.Add(-time.Minute))
	if got := r.placer.total(); got != 1 {
		t.Fatalf("%d placed before Saturday's opening, want 1", got)
	}
	r.at(saturday)
	r.wantDialledAt(saturday, tried, untried)
	r.noViolations()
}

// A second range added after the first closed: what waited for tomorrow is due
// when the new range opens, not before.
func TestARangeAddedToTheWindowBringsWaitingContactsForward(t *testing.T) {
	r := newRig(t, opts{start: sh(t, "2026-06-02 11:40")})
	n := numbers("83", 2)
	tried, untried := n[0].number, n[1].number
	c := r.newCampaign("Afternoon", func(s *camSpec) {
		s.zone, s.policy = "Asia/Shanghai", shortRetries()
		s.window = calling(weekdays, "09:00", "12:00")
	})
	r.importContacts(c, n[:1])
	r.start(c)
	r.at(sh(t, "2026-06-02 11:50"))
	r.endAt(endNoAnswer, tried) // retry 12:05: past the close
	r.at(sh(t, "2026-06-02 12:05"))
	r.importAt(c, n[1:])
	tomorrow := sh(t, "2026-06-03 09:00")
	r.wantDue(c, tried, tomorrow)
	r.wantDue(c, untried, tomorrow)

	r.at(sh(t, "2026-06-02 12:30"))
	twoRanges := calling(weekdays, "09:00", "12:00", "13:00", "17:30")
	r.patch(c, campaign.Patch{CallingWindow: &twoRanges})
	afternoon := sh(t, "2026-06-02 13:00")
	r.wantDue(c, tried, afternoon)
	r.wantDue(c, untried, afternoon)
	if p := r.progress(c); p.Activity != campaign.ActivityWaitingWindow || p.NextDialAt == nil || !p.NextDialAt.Equal(afternoon) {
		t.Errorf("progress = %+v, want WAITING_WINDOW until 13:00", p)
	}
	r.at(afternoon.Add(-time.Minute))
	if got := r.placer.total(); got != 1 {
		t.Fatalf("%d placed before the new range opened, want 1", got)
	}
	r.at(afternoon)
	r.wantDialledAt(afternoon, tried, untried)
	r.noViolations()
}

// A window that crosses midnight, widened after it closed at 01:00: the range
// that began at 22:00 now runs to 02:00, so the contacts are judged inside it.
func TestAWindowAcrossMidnightWidenedAfterItClosed(t *testing.T) {
	r := newRig(t, opts{outer: "00:00-24:00", start: sh(t, "2026-06-03 00:40")})
	n := numbers("84", 3)
	noAnswer, busy, untried := n[0].number, n[1].number, n[2].number
	c := r.newCampaign("Night", func(s *camSpec) {
		s.zone, s.policy = "Asia/Shanghai", shortRetries()
		s.window = calling(everyDay, "22:00", "01:00")
	})
	r.importContacts(c, n[:2])
	r.start(c)
	r.at(sh(t, "2026-06-03 00:50"))
	r.endAt(endNoAnswer, noAnswer) // retry 01:05
	r.at(sh(t, "2026-06-03 00:55"))
	r.endAt(endBusy, busy) // retry 01:55
	r.at(sh(t, "2026-06-03 01:01"))
	r.importAt(c, n[2:])
	evening := sh(t, "2026-06-03 22:00")
	for _, num := range []string{noAnswer, busy, untried} {
		r.wantDue(c, num, evening)
	}

	r.at(sh(t, "2026-06-03 01:10"))
	later := calling(everyDay, "22:00", "02:00")
	r.patch(c, campaign.Patch{CallingWindow: &later})
	r.wantDialledAt(sh(t, "2026-06-03 01:10"), noAnswer, untried)
	r.wantDue(c, busy, sh(t, "2026-06-03 01:55"))
	r.at(sh(t, "2026-06-03 01:54"))
	if got := r.placer.placedTo(busy); got != 1 {
		t.Fatalf("the busy line was dialled %d times before its hour was up, want 1", got)
	}
	r.at(sh(t, "2026-06-03 01:55"))
	r.wantDialledAt(sh(t, "2026-06-03 01:55"), busy)
	r.noViolations()
}

// The same window sent in another order is no change: nothing is judged again.
// A real change is.
func TestTheSameWindowInAnotherOrderReArmsNothing(t *testing.T) {
	r := newRig(t, opts{start: sh(t, "2026-06-02 18:00")})
	cs := r.counted()
	c := r.newCampaign("Same", func(s *camSpec) {
		s.zone = "Asia/Shanghai"
		s.window = calling(weekdays, "09:00", "12:00", "13:00", "17:30")
	})
	r.importContacts(c, numbers("85", 2))
	r.start(c)
	if r.placer.total() != 0 {
		t.Fatal("dialled outside the window")
	}
	cs.waits.Store(0)

	reordered := calling([]string{"FRI", "THU", "WED", "TUE", "MON"}, "13:00", "17:30", "09:00", "12:00")
	r.patch(c, campaign.Patch{CallingWindow: &reordered})
	if n := cs.waits.Load(); n != 0 {
		t.Errorf("the same window in another order was judged again (%d reads), want none", n)
	}
	if got := r.campaignNow(c).CallingWindow; got.Days[0] != "MON" || got.Ranges[0].Start != "09:00" {
		t.Errorf("stored window %+v, want it normalised", got)
	}

	longer := calling(weekdays, "09:00", "12:00", "13:00", "18:30")
	r.patch(c, campaign.Patch{CallingWindow: &longer})
	if n := cs.waits.Load(); n == 0 {
		t.Error("a changed window was not judged again")
	}
	r.wantDialledAt(sh(t, "2026-06-02 18:00"), numbers("85", 2)[0].number, numbers("85", 2)[1].number)
}

// The outer hours are a deployment setting: a restart with wider ones judges
// every running campaign's waiting contacts again.
func TestWiderOuterHoursTakeEffectWhenTheDialerStarts(t *testing.T) {
	r := newRig(t, opts{outer: "08:00-17:00", start: sh(t, "2026-06-02 16:40")})
	n := numbers("86", 2)
	tried, untried := n[0].number, n[1].number
	c := r.newCampaign("Outer", func(s *camSpec) {
		s.zone, s.policy = "Asia/Shanghai", shortRetries()
		s.window = calling(weekdays, "09:00", "18:00")
	})
	r.importContacts(c, n[:1])
	r.start(c)
	r.at(sh(t, "2026-06-02 16:50"))
	r.endAt(endNoAnswer, tried) // retry 17:05: the outer hours closed at 17:00
	r.at(sh(t, "2026-06-02 17:01"))
	r.importAt(c, n[1:])
	tomorrow := sh(t, "2026-06-03 09:00")
	r.wantDue(c, tried, tomorrow)
	r.wantDue(c, untried, tomorrow)
	r.at(sh(t, "2026-06-02 17:30"))
	if got := r.placer.total(); got != 1 {
		t.Fatalf("%d placed, want 1", got)
	}

	wider, err := dayrange.Parse("08:00-21:00")
	if err != nil {
		t.Fatal(err)
	}
	r.stopDialer()
	r.cfg.OutboundHours, r.st.OutboundHours = wider, wider
	r.dep.Store = r.st.Dialer() // its settle reads the outer hours too
	r.startDialer()
	r.settle()
	r.wantDialledAt(sh(t, "2026-06-02 17:30"), tried, untried)
	r.noViolations()
}

// A call in flight when the window changes is left to finish; its retry is
// then settled in the new window.
func TestACallInFlightIsNotTouchedByTheReArm(t *testing.T) {
	r := newRig(t, opts{start: sh(t, "2026-06-02 17:29")})
	n := numbers("87", 1)
	c := r.newCampaign("Ringing", func(s *camSpec) {
		s.zone, s.policy = "Asia/Shanghai", shortRetries()
		s.window = calling(weekdays, "09:00", "17:30")
	})
	r.importContacts(c, n)
	r.start(c)
	call := r.liveTo(n[0].number)

	r.at(sh(t, "2026-06-02 17:35"))
	wider := calling(weekdays, "09:00", "18:00")
	r.patch(c, campaign.Patch{CallingWindow: &wider})
	if got := r.row(c, n[0].number); got.next != nil || got.status != campaign.ContactActive {
		t.Errorf("the contact being dialled: %v, want ACTIVE with no due time", got)
	}
	if got := r.placer.total(); got != 1 {
		t.Errorf("%d placed, want 1: the call is still ringing", got)
	}
	r.at(sh(t, "2026-06-02 17:36"))
	r.end(call, endNoAnswer)
	r.settle()
	r.wantDue(c, n[0].number, sh(t, "2026-06-02 17:51")) // inside the new window
	r.noViolations()
}

// The re-arm reads a page at a time and a few pages per pass; the passes that
// follow at once finish the list.
func TestTheReArmPagesThroughALongListOverSeveralPasses(t *testing.T) {
	r := newRig(t, opts{start: sh(t, "2026-06-02 17:31")})
	r.stopDialer()
	r.cfg.rearmPage, r.cfg.maxRearmPages = 4, 2
	r.startDialer()
	cs := r.counted()
	c := r.newCampaign("Long", func(s *camSpec) {
		s.zone, s.max = "Asia/Shanghai", 50
		s.window = calling(weekdays, "09:00", "17:30")
	})
	r.importContacts(c, numbers("88", 30))
	r.start(c)
	tomorrow := sh(t, "2026-06-03 09:00")
	for _, ct := range r.contacts(c) {
		if ct.NextAttemptAt == nil || !ct.NextAttemptAt.Equal(tomorrow) {
			t.Fatalf("%s due %v, want deferred to %v", ct.Number, ct.NextAttemptAt, tomorrow)
		}
	}
	cs.waits.Store(0)

	r.at(sh(t, "2026-06-02 17:35"))
	wider := calling(weekdays, "09:00", "18:00")
	r.patch(c, campaign.Patch{CallingWindow: &wider})
	for i := 0; i < 10 && r.placer.total() < 30; i++ {
		r.settle()
	}
	if got := r.placer.total(); got != 30 {
		t.Fatalf("%d placed after the change, want all 30", got)
	}
	// 30 contacts four at a time: seven full pages and a last one of two.
	if n := cs.waits.Load(); n != 8 {
		t.Errorf("%d reads, want 8", n)
	}
	r.noViolations()
}
