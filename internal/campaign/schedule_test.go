// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

var allDays = []string{"MON", "TUE", "WED", "THU", "FRI", "SAT", "SUN"}

func sched(t *testing.T, zone string, days []string, outer string, ranges ...string) Schedule {
	t.Helper()
	w := CallingWindow{Days: days}
	for i := 0; i+1 < len(ranges); i += 2 {
		w.Ranges = append(w.Ranges, TimeRange{Start: ranges[i], End: ranges[i+1]})
	}
	var o dayrange.Range
	if outer != "" {
		var err error
		if o, err = dayrange.Parse(outer); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewSchedule(zone, "", w, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func at(t *testing.T, zone, local string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation("2006-01-02 15:04", local, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAContactIsInsideItsOwnWindow(t *testing.T) {
	w := CallingWindow{Days: []string{"MON", "TUE", "WED", "THU", "FRI"}, Ranges: []TimeRange{{"09:00", "17:00"}}}
	ny, _ := NewSchedule("Asia/Shanghai", "America/New_York", w, dayrange.Range{})
	sh, _ := NewSchedule("Asia/Shanghai", "", w, dayrange.Range{})
	// 2026-06-02 (a Tuesday) 14:00 UTC is 10:00 in New York and 22:00 in
	// Shanghai: the same instant, open for one contact and closed for the
	// other.
	instant := utc(t, "2026-06-02 14:00")
	if !ny.IsOpen(instant) {
		t.Error("10:00 in New York should be open")
	}
	if sh.IsOpen(instant) {
		t.Error("22:00 in Shanghai should be closed")
	}
	// The contact's zone wins over the campaign's.
	if ny.Zone.String() != "America/New_York" || sh.Zone.String() != "Asia/Shanghai" {
		t.Errorf("zones = %s, %s", ny.Zone, sh.Zone)
	}
	if _, err := NewSchedule("", "", w, dayrange.Range{}); err == nil {
		t.Error("no zone at all must be refused, not read as UTC")
	}
}

func TestTheEdgesOfAWindow(t *testing.T) {
	s := sched(t, "Asia/Shanghai", allDays, "", "09:00", "17:00")
	tests := []struct {
		local string
		want  bool
	}{
		{"2026-06-02 08:59", false},
		{"2026-06-02 09:00", true},
		{"2026-06-02 16:59", true},
		{"2026-06-02 17:00", false},
	}
	for _, tt := range tests {
		if got := s.IsOpen(at(t, "Asia/Shanghai", tt.local)); got != tt.want {
			t.Errorf("IsOpen(%s) = %v, want %v", tt.local, got, tt.want)
		}
	}
	// Two touching ranges are one stretch: the next start is not noon.
	two := sched(t, "Asia/Shanghai", allDays, "", "09:00", "12:00", "12:00", "17:00")
	next, ok := two.NextStart(at(t, "Asia/Shanghai", "2026-06-02 10:00"))
	if !ok || !next.Equal(at(t, "Asia/Shanghai", "2026-06-03 09:00")) {
		t.Errorf("next start from inside = %v, %v; want tomorrow 09:00", next, ok)
	}
}

func TestARangeThatCrossesMidnightBelongsToTheDayItStartsOn(t *testing.T) {
	s := sched(t, "America/New_York", []string{"MON"}, "20:00-04:00", "22:00", "02:00")
	tests := []struct {
		local string
		want  bool
	}{
		{"2026-06-01 21:59", false}, // Monday, before it starts
		{"2026-06-01 22:00", true},
		{"2026-06-01 23:59", true},
		{"2026-06-02 00:00", true}, // Tuesday morning belongs to Monday
		{"2026-06-02 01:59", true},
		{"2026-06-02 02:00", false},
		{"2026-06-02 22:30", false}, // Tuesday is not a calling day
		{"2026-06-09 01:00", true},  // the next Monday's range, Tuesday side
		{"2026-06-08 01:00", false}, // Monday 01:00 is Sunday's range, and Sunday is not a day
	}
	for _, tt := range tests {
		if got := s.IsOpen(at(t, "America/New_York", tt.local)); got != tt.want {
			t.Errorf("IsOpen(%s) = %v, want %v", tt.local, got, tt.want)
		}
	}
	// A retry due at Tuesday 02:00 waits for next Monday night.
	next, _ := s.NextOpen(at(t, "America/New_York", "2026-06-02 02:00"))
	if want := at(t, "America/New_York", "2026-06-08 22:00"); !next.Equal(want) {
		t.Errorf("NextOpen = %v, want %v", next, want)
	}
}

func TestTheOuterHoursBindEvenInsideTheWindow(t *testing.T) {
	// The window was valid when it was written; the outer hours have been
	// narrowed since.
	s := sched(t, "Asia/Shanghai", allDays, "10:00-12:00", "09:00", "17:00")
	for local, want := range map[string]bool{
		"2026-06-02 09:30": false, "2026-06-02 10:00": true, "2026-06-02 11:59": true,
		"2026-06-02 12:00": false, "2026-06-02 15:00": false,
	} {
		if got := s.IsOpen(at(t, "Asia/Shanghai", local)); got != want {
			t.Errorf("IsOpen(%s) = %v, want %v", local, got, want)
		}
	}
	next, _ := s.NextOpen(at(t, "Asia/Shanghai", "2026-06-02 15:00"))
	if want := at(t, "Asia/Shanghai", "2026-06-03 10:00"); !next.Equal(want) {
		t.Errorf("NextOpen = %v, want %v", next, want)
	}
}

// The clock changes. A boundary is a wall-clock time on a date, so the day
// the clocks move is 23 or 25 hours long and nothing is 24h from anything.
func TestWindowsOnADaylightSavingChangeoverDay(t *testing.T) {
	t.Run("New York spring forward: 09:00 on Sunday is 13:00Z, not 14:00Z", func(t *testing.T) {
		s := sched(t, "America/New_York", allDays, "", "09:00", "17:00")
		// Saturday 2026-03-07 is still EST (UTC-5): 09:00 = 14:00Z.
		if got, _ := s.NextStart(at(t, "America/New_York", "2026-03-07 17:00")); !got.Equal(utc(t, "2026-03-08 13:00")) {
			t.Errorf("next start after Saturday's window = %v, want 2026-03-08 13:00Z (09:00 EDT)", got.UTC())
		}
		if !s.IsOpen(utc(t, "2026-03-08 13:00")) || s.IsOpen(utc(t, "2026-03-08 12:59")) {
			t.Error("Sunday's window must open at 13:00Z")
		}
	})
	t.Run("New York fall back: 09:00 on Sunday is 14:00Z", func(t *testing.T) {
		s := sched(t, "America/New_York", allDays, "", "09:00", "17:00")
		if got, _ := s.NextStart(at(t, "America/New_York", "2026-10-31 17:00")); !got.Equal(utc(t, "2026-11-01 14:00")) {
			t.Errorf("next start = %v, want 2026-11-01 14:00Z (09:00 EST)", got.UTC())
		}
		// And the window of the 25-hour day closes at 17:00 EST = 22:00Z.
		if !s.IsOpen(utc(t, "2026-11-01 21:59")) || s.IsOpen(utc(t, "2026-11-01 22:00")) {
			t.Error("Sunday's window must close at 22:00Z")
		}
	})
	t.Run("a start inside the gap opens when the clock jumps", func(t *testing.T) {
		// 02:30 does not exist on 2026-03-08 in New York; the clock goes from
		// 02:00 EST to 03:00 EDT, which is 07:00Z.
		s := sched(t, "America/New_York", []string{"SUN"}, "", "02:30", "05:00")
		got, _ := s.NextStart(at(t, "America/New_York", "2026-03-08 00:00"))
		if !got.Equal(utc(t, "2026-03-08 07:00")) {
			t.Errorf("start in the gap = %v, want 07:00Z", got.UTC())
		}
		if !s.IsOpen(utc(t, "2026-03-08 07:00")) || s.IsOpen(utc(t, "2026-03-08 06:59")) {
			t.Error("must be open from the jump and not before it")
		}
		// It closes at 05:00 EDT = 09:00Z.
		if s.IsOpen(utc(t, "2026-03-08 09:00")) || !s.IsOpen(utc(t, "2026-03-08 08:59")) {
			t.Error("must close at 09:00Z")
		}
	})
	t.Run("an end inside the gap closes when the clock jumps", func(t *testing.T) {
		s := sched(t, "America/New_York", []string{"SUN"}, "", "00:30", "02:30")
		if !s.IsOpen(utc(t, "2026-03-08 06:59")) || s.IsOpen(utc(t, "2026-03-08 07:00")) {
			t.Error("a window ending in the gap must close at the jump (07:00Z)")
		}
	})
	t.Run("a repeated hour: the start is its first occurrence, the end its last", func(t *testing.T) {
		// 2026-11-01 in New York: 01:00-02:00 happens twice, EDT (05:00Z-06:00Z)
		// then EST (06:00Z-07:00Z).
		starts := sched(t, "America/New_York", []string{"SUN"}, "", "01:30", "03:00")
		got, _ := starts.NextStart(at(t, "America/New_York", "2026-10-31 12:00"))
		if !got.Equal(utc(t, "2026-11-01 05:30")) {
			t.Errorf("start in the repeated hour = %v, want 05:30Z (first 01:30)", got.UTC())
		}
		if !starts.IsOpen(utc(t, "2026-11-01 06:45")) {
			t.Error("the second 01:00-02:00 must be inside a window that began at the first 01:30")
		}
		ends := sched(t, "America/New_York", []string{"SUN"}, "", "00:00", "01:30")
		if !ends.IsOpen(utc(t, "2026-11-01 06:29")) || ends.IsOpen(utc(t, "2026-11-01 06:30")) {
			t.Error("a window ending in the repeated hour must last until its second 01:30 (06:30Z)")
		}
	})
	t.Run("London spring forward", func(t *testing.T) {
		// 2026-03-29: 01:00 GMT becomes 02:00 BST. 09:00 BST = 08:00Z.
		s := sched(t, "Europe/London", allDays, "", "09:00", "17:00")
		if got, _ := s.NextStart(at(t, "Europe/London", "2026-03-28 17:00")); !got.Equal(utc(t, "2026-03-29 08:00")) {
			t.Errorf("next start = %v, want 2026-03-29 08:00Z", got.UTC())
		}
		gap := sched(t, "Europe/London", []string{"SUN"}, "", "01:30", "04:00")
		if got, _ := gap.NextStart(at(t, "Europe/London", "2026-03-29 00:00")); !got.Equal(utc(t, "2026-03-29 01:00")) {
			t.Errorf("start in London's gap = %v, want 01:00Z (02:00 BST)", got.UTC())
		}
	})
	t.Run("London fall back", func(t *testing.T) {
		// 2026-10-25: 02:00 BST becomes 01:00 GMT. 09:00 GMT = 09:00Z.
		s := sched(t, "Europe/London", allDays, "", "09:00", "17:00")
		if got, _ := s.NextStart(at(t, "Europe/London", "2026-10-24 17:00")); !got.Equal(utc(t, "2026-10-25 09:00")) {
			t.Errorf("next start = %v, want 2026-10-25 09:00Z", got.UTC())
		}
		rep := sched(t, "Europe/London", []string{"SUN"}, "", "00:30", "01:30")
		// 01:30 happens at 00:30Z (BST) and 01:30Z (GMT); the end is the last.
		if !rep.IsOpen(utc(t, "2026-10-25 01:29")) || rep.IsOpen(utc(t, "2026-10-25 01:30")) {
			t.Error("London's repeated hour: the window must close at 01:30Z")
		}
	})
}

func TestAWindowAcrossMidnightOnTheChangeoverNight(t *testing.T) {
	// Saturday night into the clock change: 22:00 EST Saturday to 02:00
	// Sunday is 22:00 to 02:00 wall clock, but the night is only three hours
	// long in elapsed time (03:00 EDT is the first second after 01:59 EST).
	s := sched(t, "America/New_York", []string{"SAT"}, "20:00-04:00", "22:00", "04:00")
	if !s.IsOpen(utc(t, "2026-03-08 06:59")) { // 01:59 EST
		t.Error("01:59 EST is inside")
	}
	if !s.IsOpen(utc(t, "2026-03-08 07:00")) { // 03:00 EDT
		t.Error("03:00 EDT is inside")
	}
	if s.IsOpen(utc(t, "2026-03-08 08:00")) { // 04:00 EDT
		t.Error("04:00 EDT is the end")
	}
}
