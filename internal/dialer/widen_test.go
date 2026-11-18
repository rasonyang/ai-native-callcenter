// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// A calling window changed after it closed (#616): the contacts the old window
// deferred are judged again, each from the time its retry policy gave it.

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return l
}()

func sh(t *testing.T, local string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", local, shanghai)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var weekdays = []string{"MON", "TUE", "WED", "THU", "FRI"}

// calling is a window on days, from ranges given as start, end, start, end...
func calling(days []string, ranges ...string) campaign.CallingWindow {
	w := campaign.CallingWindow{Days: days}
	for i := 0; i+1 < len(ranges); i += 2 {
		w.Ranges = append(w.Ranges, campaign.TimeRange{Start: ranges[i], End: ranges[i+1]})
	}
	return w
}

// row is a contact as the table reads it: its status, stored due time, marks
// and the last time its row was written.
type row struct {
	status    campaign.ContactStatus
	next      *time.Time
	capHeld   bool
	freqHeld  bool
	updatedAt time.Time
}

func (r *rig) row(c campaign.Campaign, number string) row {
	r.t.Helper()
	var out row
	var status string
	if err := r.st.Pool.QueryRow(context.Background(),
		`SELECT status, next_attempt_at, is_daily_cap_held, is_frequency_held, updated_at
		   FROM campaign_contacts WHERE campaign_id = $1 AND number_e164 = $2`, c.ID, number).
		Scan(&status, &out.next, &out.capHeld, &out.freqHeld, &out.updatedAt); err != nil {
		r.t.Fatalf("contact %s: %v", number, err)
	}
	out.status = campaign.ContactStatus(status)
	return out
}

// importAt adds contacts to a running campaign due at the fake clock's time and
// leaves every other contact as it is. (r.rearm would also move the contacts a
// hold deferred, which these tests read.)
func (r *rig) importAt(c campaign.Campaign, specs []contactSpec) {
	r.t.Helper()
	r.importContacts(c, specs)
	nums := make([]string, len(specs))
	for i, s := range specs {
		nums[i] = s.number
	}
	if _, err := r.st.Pool.Exec(context.Background(),
		`UPDATE campaign_contacts SET next_attempt_at = $3 WHERE campaign_id = $1 AND number_e164 = ANY($2)`,
		c.ID, nums, r.clock.Now()); err != nil {
		r.t.Fatal(err)
	}
	r.dialer().Wake()
	r.settle()
}

// patch changes the campaign through the service, as the API does, so the
// dialer hears of it as an observer.
func (r *rig) patch(c campaign.Campaign, p campaign.Patch) {
	r.t.Helper()
	svc := campaign.NewService(r.st.Campaigns(), r.dialer(), campaign.Env{OutboundHours: r.cfg.OutboundHours}).
		WithDialer(r.dialer(), r.dialer())
	if _, err := svc.Update(context.Background(), c.ID, p); err != nil {
		r.t.Fatal(err)
	}
	r.settle()
}

// liveTo is the live call to the number.
func (r *rig) liveTo(number string) placed {
	r.t.Helper()
	for _, l := range r.placer.liveCalls() {
		if l.Req.To == number {
			return l
		}
	}
	r.t.Fatalf("no live call to %s", number)
	return placed{}
}

// endAt ends the live call to each number with kind at the fake clock's time.
func (r *rig) endAt(kind string, numbers ...string) {
	r.t.Helper()
	for _, n := range numbers {
		r.end(r.liveTo(n), kind)
	}
	r.settle()
}

// due says whether the contact's stored due time is at.
func (rw row) due(at time.Time) bool { return rw.next != nil && rw.next.Equal(at) }

func (rw row) String() string {
	next := "none"
	if rw.next != nil {
		next = rw.next.In(shanghai).Format("Mon 15:04 MST")
	}
	return fmt.Sprintf("%s due %s cap %v freq %v", rw.status, next, rw.capHeld, rw.freqHeld)
}

// The contacts of the issue's table, by the number each one has.
type tableRows struct {
	untried, noAnswer15, noAnswer30, busy, freq, capped, la, exhausted, dnc string
}

func (t tableRows) shanghaiRows() []string {
	return []string{t.untried, t.noAnswer15, t.noAnswer30, t.busy, t.freq, t.capped, t.exhausted, t.dnc}
}

// widenFixture builds the issue's table on Tuesday 2026-06-02 in Shanghai: a
// campaign whose window MON-FRI 09:00-17:30 closed at 17:30, with one contact
// in each state at 17:31. Bot capacity is closed from 17:25 on, so the 15
// minute retry is not admitted before the close, and the stored due times can
// be read after the change before anything is dialled.
func widenFixture(t *testing.T) (*rig, campaign.Campaign, tableRows, *fakeSlots) {
	t.Helper()
	slots := &fakeSlots{u: voice.Usage{Cap: 100}}
	r := newRig(t, opts{start: sh(t, "2026-06-02 10:00"), dailyCap: 3, slots: slots})
	n := numbers("81", 8)
	rows := tableRows{
		untried: n[0].number, noAnswer15: n[1].number, noAnswer30: n[2].number, busy: n[3].number,
		freq: n[4].number, capped: n[5].number, exhausted: n[6].number, dnc: n[7].number,
		la: "+13105550161",
	}
	ctx := context.Background()

	// Another campaign reached the frequency row's number at 10:00, so it is
	// held until 10:00 tomorrow; three ad-hoc calls reached the cap row's
	// number this morning.
	other := r.otherCampaign("Other", []contactSpec{{number: rows.freq}})
	r.dialed(other, rows.freq, campaign.OutcomeNoAnswer, sh(t, "2026-06-02 10:00"))
	for _, at := range []string{"10:00", "11:00", "12:00"} {
		r.adHocCall(rows.capped, sh(t, "2026-06-02 "+at), endNoAnswer)
	}

	policy := campaign.DefaultRetryPolicy()
	policy.RetryOutcomes = []campaign.Outcome{ // REJECTED ends a contact
		campaign.OutcomeNoAnswer, campaign.OutcomeBusy, campaign.OutcomeTransient, campaign.OutcomeOriginateFailed,
	}
	policy.IntervalSec[campaign.OutcomeNoAnswer] = 15 * 60
	policy.IntervalSec[campaign.OutcomeBusy] = 60 * 60
	c := r.newCampaign("Widened", func(s *camSpec) {
		s.zone, s.max, s.policy = "Asia/Shanghai", 20, &policy
		s.window = calling(weekdays, "09:00", "17:30")
	})
	r.importContacts(c, []contactSpec{
		{number: rows.noAnswer15}, {number: rows.noAnswer30}, {number: rows.busy}, {number: rows.exhausted}, {number: rows.dnc},
	})
	if _, _, err := r.st.DNC().Add(ctx, rows.dnc, "MANUAL", nil, nil); err != nil {
		t.Fatal(err)
	}

	r.at(sh(t, "2026-06-02 17:09"))
	r.start(c)
	if got := r.placer.total(); got != 4 {
		t.Fatalf("%d placed at 17:09, want 4 (the do-not-call number skipped)", got)
	}
	r.importAt(c, []contactSpec{{number: rows.freq}, {number: rows.capped}})

	r.at(sh(t, "2026-06-02 17:10"))
	r.endAt(endNoAnswer, rows.noAnswer15) // due 17:25
	r.at(sh(t, "2026-06-02 17:12"))
	r.endAt(endRejected, rows.exhausted)
	r.at(sh(t, "2026-06-02 17:15"))
	// The operator lengthens the no-answer interval; the next settle reads it.
	longer := policy
	longer.IntervalSec = map[campaign.Outcome]int{
		campaign.OutcomeNoAnswer: 30 * 60, campaign.OutcomeBusy: 60 * 60,
		campaign.OutcomeRejected: 24 * 3600, campaign.OutcomeUnknown: 24 * 3600,
	}
	r.patch(c, campaign.Patch{RetryPolicy: &longer})
	r.at(sh(t, "2026-06-02 17:20"))
	r.endAt(endNoAnswer, rows.noAnswer30) // 17:50 is past the close: settle moves it
	slots.set(voice.Usage{Cap: 0})
	r.at(sh(t, "2026-06-02 17:25"))
	r.endAt(endBusy, rows.busy) // 18:25 is past the close
	if got := r.placer.placedTo(rows.noAnswer15); got != 1 {
		t.Fatalf("the 15-minute retry was dialled again before the close (%d dials), want it waiting on capacity", got)
	}

	r.at(sh(t, "2026-06-02 17:31")) // the window closed: the 17:25 retry is deferred
	r.importAt(c, []contactSpec{{number: rows.untried}, {number: rows.la, zone: "America/Los_Angeles"}})
	return r, c, rows, slots
}

// The state before the change: everything waits for the old window's opening,
// or for its hold.
func checkBeforeTheChange(t *testing.T, r *rig, c campaign.Campaign, rows tableRows) {
	t.Helper()
	tomorrow9 := sh(t, "2026-06-03 09:00")
	laOpens := time.Date(2026, 6, 2, 16, 0, 0, 0, time.UTC) // 09:00 PDT
	for name, want := range map[string]struct {
		number    string
		at        time.Time
		cap, freq bool
	}{
		"never tried":        {rows.untried, tomorrow9, false, false},
		"NO_ANSWER at 17:10": {rows.noAnswer15, tomorrow9, false, false},
		"NO_ANSWER at 17:20": {rows.noAnswer30, tomorrow9, false, false},
		"BUSY at 17:25":      {rows.busy, tomorrow9, false, false},
		"frequency-held":     {rows.freq, sh(t, "2026-06-03 10:00"), false, true},
		"cap-held":           {rows.capped, tomorrow9, true, false},
		"Los Angeles":        {rows.la, laOpens, false, false},
	} {
		if got := r.row(c, want.number); got.status != campaign.ContactActive || !got.due(want.at) ||
			got.capHeld != want.cap || got.freqHeld != want.freq {
			t.Fatalf("before the change, %s: %v; want ACTIVE due %v, cap %v freq %v",
				name, got, want.at.In(shanghai), want.cap, want.freq)
		}
	}
	if got := r.row(c, rows.exhausted); got.status != campaign.ContactExhausted || got.next != nil {
		t.Fatalf("before the change, the rejected contact: %v, want EXHAUSTED", got)
	}
	if got := r.row(c, rows.dnc); got.status != campaign.ContactDoNotCall || got.next != nil {
		t.Fatalf("before the change, the listed number: %v, want DNC", got)
	}
	if p := r.progress(c); p.Activity != campaign.ActivityWaitingWindow || p.NextDialAt == nil || !p.NextDialAt.Equal(laOpens) {
		t.Fatalf("before the change: progress %+v, want WAITING_WINDOW until Los Angeles opens (%v)", p, laOpens)
	}
}

// checkAfterTheChange is the table's last column, read once the change has
// taken effect at changedAt with bot capacity still closed, and then the run
// of the campaign: each contact dialled at the first moment its retry
// interval, the new window and its hold allow, and never earlier.
func checkAfterTheChange(t *testing.T, r *rig, c campaign.Campaign, rows tableRows, slots *fakeSlots,
	changedAt time.Time, untouched map[string]row) {
	t.Helper()
	tomorrow9 := sh(t, "2026-06-03 09:00")
	laOpens := time.Date(2026, 6, 2, 16, 0, 0, 0, time.UTC)
	for name, want := range map[string]struct {
		number    string
		at        time.Time
		cap, freq bool
	}{
		"never tried: due now":                                 {rows.untried, changedAt, false, false},
		"NO_ANSWER at 17:10 + 15 min: due now":                 {rows.noAnswer15, changedAt, false, false},
		"NO_ANSWER at 17:20 + 30 min: due 17:50":               {rows.noAnswer30, sh(t, "2026-06-02 17:50"), false, false},
		"BUSY at 17:25 + 60 min: 18:25 is outside, 09:00 kept": {rows.busy, tomorrow9, false, false},
		"frequency-held until 10:00 tomorrow: held, mark kept": {rows.freq, sh(t, "2026-06-03 10:00"), false, true},
		"cap-held: held, mark kept":                            {rows.capped, tomorrow9, true, false},
		"Los Angeles: judged in its own zone, still 09:00 PDT": {rows.la, laOpens, false, false},
	} {
		got := r.row(c, want.number)
		if got.status != campaign.ContactActive || !got.due(want.at) || got.capHeld != want.cap || got.freqHeld != want.freq {
			t.Errorf("after the change, %s: %v; want due %v, cap %v freq %v",
				name, got, want.at.In(shanghai).Format("Mon 15:04"), want.cap, want.freq)
		}
	}
	// Rows the change does not move are not written at all.
	for number, before := range untouched {
		if got := r.row(c, number); got.status != before.status || !got.updatedAt.Equal(before.updatedAt) ||
			(got.next == nil) != (before.next == nil) || (got.next != nil && !got.next.Equal(*before.next)) {
			t.Errorf("%s was written: %v (updated %v), want it untouched: %v (updated %v)",
				number, got, got.updatedAt, before, before.updatedAt)
		}
	}
	if got := r.placer.total(); got != 4 {
		t.Fatalf("%d placed with bot capacity closed, want still 4", got)
	}
	if p := r.progress(c); p.Activity != campaign.ActivityAtCapacity {
		t.Errorf("progress after the change = %+v, want AT_CAPACITY: two contacts are due and wait for a bot", p)
	}

	// Capacity comes back: the two due contacts are dialled at once, and the
	// campaign says when the next one is due without a restart.
	slots.set(voice.Usage{Cap: 100})
	r.dialer().SlotFreed()
	r.settle()
	for _, n := range []string{rows.untried, rows.noAnswer15} {
		if got := r.liveTo(n); !got.At.Equal(changedAt) {
			t.Errorf("%s dialled at %v, want %v", n, got.At, changedAt)
		}
	}
	if p := r.progress(c); p.Activity != campaign.ActivityDialing || p.NextDialAt == nil || !p.NextDialAt.Equal(sh(t, "2026-06-02 17:50")) {
		t.Errorf("progress = %+v, want DIALING with the next dial at 17:50", p)
	}
	r.at(changedAt.Add(time.Minute))
	r.endAt(endAnswered, rows.untried, rows.noAnswer15)
	if p := r.progress(c); p.Activity != campaign.ActivityWaitingRetry || p.NextDialAt == nil || !p.NextDialAt.Equal(sh(t, "2026-06-02 17:50")) {
		t.Errorf("progress = %+v, want WAITING_RETRY until 17:50", p)
	}

	// The rest of the run.
	steps := []struct {
		at   time.Time
		dial []string // dialled at this moment, and nothing else
	}{
		{sh(t, "2026-06-02 17:49"), nil},
		{sh(t, "2026-06-02 17:50"), []string{rows.noAnswer30}},
		{sh(t, "2026-06-02 17:59"), nil},
		{sh(t, "2026-06-02 23:59"), nil},
		{laOpens, []string{rows.la}},
		{sh(t, "2026-06-03 08:59"), nil},
		{tomorrow9, []string{rows.busy, rows.capped}},
		{sh(t, "2026-06-03 09:59"), nil},
		{sh(t, "2026-06-03 10:00"), []string{rows.freq}},
	}
	for _, st := range steps {
		before := r.placer.total()
		r.at(st.at)
		if got := r.placer.total() - before; got != len(st.dial) {
			t.Fatalf("at %v: %d dialled, want %v", st.at.In(shanghai).Format("Mon 15:04"), got, st.dial)
		}
		for _, n := range st.dial {
			r.liveTo(n)
		}
		r.endAll(func(string) string { return endAnswered })
	}

	// Nobody was dialled outside the window as it now is, nor more often than
	// the table says.
	want := map[string]int{
		rows.untried: 1, rows.noAnswer15: 2, rows.noAnswer30: 2, rows.busy: 2, rows.freq: 1, rows.capped: 1,
		rows.la: 1, rows.exhausted: 1,
	}
	got := map[string]int{}
	widened := calling(weekdays, "09:00", "18:00")
	for _, p := range r.placer.placedCalls() {
		got[p.Req.To]++
		zone := ""
		if p.Req.To == rows.la {
			zone = "America/Los_Angeles"
		}
		s, err := campaign.NewSchedule("Asia/Shanghai", zone, widened, r.cfg.OutboundHours)
		if err != nil {
			t.Fatal(err)
		}
		if !s.IsOpen(p.At) {
			t.Errorf("%s dialled at %v, outside its window", p.Req.To, p.At.In(s.Zone))
		}
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s dialled %d times, want %d", n, got[n], w)
		}
	}
	if got[rows.dnc] != 0 {
		t.Errorf("the do-not-call number was dialled %d times", got[rows.dnc])
	}
	if got := r.campaignNow(c); got.Status != campaign.StatusCompleted {
		t.Errorf("status %s at the end, want COMPLETED", got.Status)
	}
	r.noViolations()
}

// untouchedRows are the rows the change must not write, by number: the busy
// retry (its time stands), both holds, the contact whose own zone keeps it
// waiting, and the two that are no longer ACTIVE.
func untouchedRows(r *rig, c campaign.Campaign, rows tableRows) map[string]row {
	out := map[string]row{}
	for _, n := range []string{rows.busy, rows.freq, rows.capped, rows.la, rows.exhausted, rows.dnc} {
		out[n] = r.row(c, n)
	}
	return out
}

func TestAWindowWidenedAfterItClosedReArmsWhatTheOldWindowDeferred(t *testing.T) {
	r, c, rows, slots := widenFixture(t)
	checkBeforeTheChange(t, r, c, rows)
	untouched := untouchedRows(r, c, rows)

	r.at(sh(t, "2026-06-02 17:35"))
	wider := calling(weekdays, "09:00", "18:00")
	r.patch(c, campaign.Patch{CallingWindow: &wider})
	checkAfterTheChange(t, r, c, rows, slots, sh(t, "2026-06-02 17:35"), untouched)
}

func TestAWindowWidenedWhilePausedTakesEffectOnResume(t *testing.T) {
	r, c, rows, slots := widenFixture(t)
	checkBeforeTheChange(t, r, c, rows)
	untouched := untouchedRows(r, c, rows)

	r.at(sh(t, "2026-06-02 17:33"))
	if _, err := r.dialer().Pause(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	r.at(sh(t, "2026-06-02 17:35"))
	wider := calling(weekdays, "09:00", "18:00")
	r.patch(c, campaign.Patch{CallingWindow: &wider})
	// Paused: nothing moves yet.
	for _, n := range rows.shanghaiRows() {
		if got := r.row(c, n); got.next != nil && !got.next.After(r.clock.Now()) {
			t.Errorf("%s is due %v while the campaign is paused", n, got.next.In(shanghai))
		}
	}
	if !r.row(c, rows.untried).due(sh(t, "2026-06-03 09:00")) {
		t.Errorf("the never-tried contact moved while paused: %v", r.row(c, rows.untried))
	}

	r.at(sh(t, "2026-06-02 17:36"))
	if _, err := r.dialer().Resume(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	r.settle()
	checkAfterTheChange(t, r, c, rows, slots, sh(t, "2026-06-02 17:36"), untouched)
}
