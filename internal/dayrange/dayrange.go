// SPDX-License-Identifier: Apache-2.0

// Package dayrange is wall-clock ranges within a day: "08:00-21:00", or
// "22:00-02:00" across midnight.
//
// A range is only minutes since midnight. It names no date and no time zone;
// whoever evaluates it picks the day (a range that crosses midnight belongs to
// the day it starts on) and the zone (the callee's). Daylight-saving shifts are
// that evaluator's concern too: nothing here adds 24 hours to anything.
package dayrange

import (
	"errors"
	"fmt"
	"strings"
)

// MinutesPerDay is the end of a day, and the largest End of a Range.
const MinutesPerDay = 24 * 60

// Range is [Start, End) in minutes since midnight. Start is 0 to 1439 and End
// 1 to 1440; End 1440 is "24:00", the end of the day. A range whose End is
// below its Start crosses midnight: 22:00-02:00 is 1320-120. Start never
// equals End, so a range is never empty and never the whole day written as
// nothing.
type Range struct {
	Start, End int
}

// ErrFormat is returned for text that is not HH:MM or HH:MM-HH:MM.
var ErrFormat = errors.New("not a time of day")

// ParseClock reads "HH:MM" as minutes since midnight: hours 00 to 23, minutes
// 00 to 59. Exactly two digits each, so "9:00" and "09:0" are refused rather
// than guessed at.
func ParseClock(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("%w: %q, want HH:MM", ErrFormat, s)
	}
	h, ok1 := twoDigits(s[0:2])
	m, ok2 := twoDigits(s[3:5])
	if !ok1 || !ok2 || h > 23 || m > 59 {
		return 0, fmt.Errorf("%w: %q, want HH:MM from 00:00 to 23:59", ErrFormat, s)
	}
	return h*60 + m, nil
}

func twoDigits(s string) (int, bool) {
	if s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}

// New builds a range from its two ends as written. The end may be "24:00" and
// "00:00" as an end means the end of the day, so 22:00-00:00 is 22:00 to
// midnight. Equal ends are refused.
func New(start, end string) (Range, error) {
	s, err := ParseClock(start)
	if err != nil {
		return Range{}, err
	}
	var e int
	if end == "24:00" {
		e = MinutesPerDay
	} else if e, err = ParseClock(end); err != nil {
		return Range{}, err
	}
	if e == 0 && s != 0 {
		e = MinutesPerDay
	}
	if s == e {
		return Range{}, fmt.Errorf("%w: %s-%s starts and ends at the same time", ErrFormat, start, end)
	}
	return Range{Start: s, End: e}, nil
}

// Parse reads "HH:MM-HH:MM".
func Parse(s string) (Range, error) {
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return Range{}, fmt.Errorf("%w: %q, want HH:MM-HH:MM", ErrFormat, s)
	}
	return New(start, end)
}

// Format writes minutes since midnight as HH:MM; the end of the day is 24:00.
func Format(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// Start and End as the text they were written in.
func (r Range) StartText() string { return Format(r.Start) }
func (r Range) EndText() string   { return Format(r.End) }

func (r Range) String() string { return r.StartText() + "-" + r.EndText() }

// CrossesMidnight reports whether the range runs past the end of the day into
// the next one.
func (r Range) CrossesMidnight() bool { return r.End < r.Start }

// segment is a half-open interval inside one day.
type segment struct{ from, to int }

// segments splits the range at midnight into pieces that sit inside one day.
func (r Range) segments() []segment {
	if r.CrossesMidnight() {
		return []segment{{r.Start, MinutesPerDay}, {0, r.End}}
	}
	return []segment{{r.Start, r.End}}
}

// Within reports whether every minute of r lies inside outer. Both may cross
// midnight: 22:00-02:00 is inside 20:00-04:00 and not inside 08:00-21:00,
// and 23:00-23:30 is inside 20:00-04:00 as well.
func (r Range) Within(outer Range) bool {
	for _, piece := range r.segments() {
		inside := false
		for _, o := range outer.segments() {
			if o.from <= piece.from && piece.to <= o.to {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

// Overlaps reports whether r and other share a minute.
func (r Range) Overlaps(other Range) bool {
	for _, a := range r.segments() {
		for _, b := range other.segments() {
			if a.from < b.to && b.from < a.to {
				return true
			}
		}
	}
	return false
}

// Contains reports whether the minute (since midnight) is inside r.
func (r Range) Contains(minute int) bool {
	for _, s := range r.segments() {
		if s.from <= minute && minute < s.to {
			return true
		}
	}
	return false
}
