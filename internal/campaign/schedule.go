// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"fmt"
	"slices"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// Schedule is when one contact may be dialled: the campaign's calling window
// and, on top of it, the deployment's outer calling hours, both read as
// wall-clock time in the contact's own zone (design 10 section 8.1).
//
// Everything is computed per local calendar day. A boundary is a wall-clock
// time on a date turned into an instant with the zone's rules, so a day that
// is 23 or 25 hours long needs no special case, and nothing here adds 24 hours
// to an instant.
type Schedule struct {
	Zone   *time.Location
	Window CallingWindow
	// Outer, when set, must also allow the call. The zero value (End 0) is
	// unset.
	Outer dayrange.Range
}

// NewSchedule builds a contact's schedule. The contact's own zone wins; the
// campaign's is the fallback. Both are IANA names.
func NewSchedule(campaignZone, contactZone string, w CallingWindow, outer dayrange.Range) (Schedule, error) {
	name := contactZone
	if name == "" {
		name = campaignZone
	}
	loc, err := LoadZone(name)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{Zone: loc, Window: w, Outer: outer}, nil
}

// scanDays is how many local days ahead the search looks. The window is
// weekly and has at least one day, so a week and a day always contains a
// start; the rest is slack for ranges that clip each other.
const scanDays = 10

// span is a half-open interval of instants.
type span struct{ from, to time.Time }

// IsOpen reports whether a call may start at t.
func (s Schedule) IsOpen(t time.Time) bool {
	for _, sp := range s.spans(t) {
		if !t.Before(sp.from) && t.Before(sp.to) {
			return true
		}
	}
	return false
}

// NextStart is the first instant after t at which the schedule opens: the
// start of the next window, which is not t even when t is inside one. It
// reports false if the window never opens within the horizon, which a
// normalised window cannot do.
func (s Schedule) NextStart(t time.Time) (time.Time, bool) {
	for _, sp := range s.spans(t) {
		if sp.from.After(t) {
			return sp.from, true
		}
	}
	return time.Time{}, false
}

// NextOpen is t itself when a call may start at t, and the next window's start
// otherwise.
func (s Schedule) NextOpen(t time.Time) (time.Time, bool) {
	if s.IsOpen(t) {
		return t, true
	}
	return s.NextStart(t)
}

// spans lists, in order and merged, the intervals the schedule is open for
// around t: from the day before t's local date (a range that crosses midnight
// belongs to the day it starts on) for scanDays days.
func (s Schedule) spans(t time.Time) []span {
	loc := s.Zone
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := t.In(loc).Date()

	var windows, outers []span
	for i := -1; i < scanDays; i++ {
		// Date normalises an out-of-range day, and noon is clear of any
		// transition, so the weekday and date read off it are the local
		// calendar's.
		day := time.Date(y, m, d+i, 12, 0, 0, 0, loc)
		dy, dm, dd := day.Date()
		if s.Outer.End != 0 {
			outers = append(outers, rangeSpans(s.Outer, dy, dm, dd, loc)...)
		}
		if !slices.Contains(s.Window.Days, weekdayName(day.Weekday())) {
			continue
		}
		for _, r := range s.Window.Ranges {
			parsed, err := dayrange.New(r.Start, r.End)
			if err != nil {
				continue
			}
			windows = append(windows, rangeSpans(parsed, dy, dm, dd, loc)...)
		}
	}
	if s.Outer.End != 0 {
		windows = intersect(windows, merge(outers))
	}
	return merge(windows)
}

// rangeSpans is the range as it falls on the local date y-m-d, starting there.
func rangeSpans(r dayrange.Range, y int, m time.Month, d int, loc *time.Location) []span {
	endDay := d
	if r.CrossesMidnight() {
		endDay++
	}
	from := wallClock(y, m, d, r.Start, true, loc)
	to := wallClock(y, m, endDay, r.End, false, loc)
	if !to.After(from) {
		return nil
	}
	return []span{{from, to}}
}

// wallClock turns a wall-clock time on a local date into an instant, and does
// it the same way every time even where the zone's clock makes that
// ambiguous.
//
//   - A time that exists once is that instant.
//   - A time that exists twice (the hour a clock is turned back) is the first
//     occurrence for the start of a range and the last for its end, so a range
//     lasts as long as its wall-clock text suggests or longer, never shorter.
//   - A time that does not exist (the hour a clock skips) is the instant the
//     clock jumps, for a start and for an end alike: the range opens, or
//     closes, at the first moment the wall clock reads past the gap.
func wallClock(y int, m time.Month, d, minutes int, isStart bool, loc *time.Location) time.Time {
	// Minutes may be 1440 ("24:00"); Date rolls that into the next day.
	naive := time.Date(y, m, d, minutes/60, minutes%60, 0, 0, time.UTC)
	wy, wm, wd := naive.Date()
	wh, wmin, _ := naive.Clock()

	// The offsets in force around the day: any instant of the wanted wall
	// time is the naive time minus one of them.
	offsets := map[int]bool{}
	for _, probe := range []time.Time{naive.Add(-24 * time.Hour), naive, naive.Add(24 * time.Hour)} {
		_, off := probe.In(loc).Zone()
		offsets[off] = true
	}
	var candidates []time.Time
	for off := range offsets {
		inst := naive.Add(-time.Duration(off) * time.Second)
		cy, cm, cd := inst.In(loc).Date()
		ch, cmin, _ := inst.In(loc).Clock()
		if cy == wy && cm == wm && cd == wd && ch == wh && cmin == wmin {
			candidates = append(candidates, inst)
		}
	}
	if len(candidates) > 0 {
		slices.SortFunc(candidates, func(a, b time.Time) int { return a.Compare(b) })
		if isStart {
			return candidates[0]
		}
		return candidates[len(candidates)-1]
	}

	// The wall time falls in a gap. Date picks an instant on one side of the
	// jump; the jump itself is the edge of that instant's zone nearest to it.
	guess := time.Date(y, m, d, minutes/60, minutes%60, 0, 0, loc)
	zoneStart, zoneEnd := guess.ZoneBounds()
	switch {
	case zoneStart.IsZero():
		return zoneEnd
	case zoneEnd.IsZero():
		return zoneStart
	case guess.Sub(zoneStart) <= zoneEnd.Sub(guess):
		return zoneStart
	}
	return zoneEnd
}

func weekdayName(w time.Weekday) string {
	return Weekdays[(int(w)+6)%7] // Weekdays starts on Monday; time.Sunday is 0
}

// merge sorts spans and joins those that touch or overlap, so that "the next
// start" never lands inside a window the contact is already in (09:00-12:00
// followed by 12:00-17:00 is one stretch).
func merge(in []span) []span {
	if len(in) == 0 {
		return nil
	}
	sorted := slices.Clone(in)
	slices.SortFunc(sorted, func(a, b span) int { return a.from.Compare(b.from) })
	out := []span{sorted[0]}
	for _, sp := range sorted[1:] {
		last := &out[len(out)-1]
		if !sp.from.After(last.to) {
			if sp.to.After(last.to) {
				last.to = sp.to
			}
			continue
		}
		out = append(out, sp)
	}
	return out
}

// intersect is the instants in both lists. Both must be merged.
func intersect(a, b []span) []span {
	b = merge(b)
	a = merge(a)
	var out []span
	for _, x := range a {
		for _, y := range b {
			from, to := x.from, x.to
			if y.from.After(from) {
				from = y.from
			}
			if y.to.Before(to) {
				to = y.to
			}
			if from.Before(to) {
				out = append(out, span{from, to})
			}
		}
	}
	return out
}

// String names the schedule for logs.
func (s Schedule) String() string {
	zone := "UTC"
	if s.Zone != nil {
		zone = s.Zone.String()
	}
	return fmt.Sprintf("%s %v %v", zone, s.Window.Days, s.Window.Ranges)
}
