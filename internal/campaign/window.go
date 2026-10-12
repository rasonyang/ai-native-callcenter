// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"errors"
	"fmt"
	"slices"
	"time"

	// A campaign's time zones are IANA names. Embedding the database makes
	// them resolve on a host (a minimal container, a stripped image) that has
	// none, instead of every zone failing with an error about a missing file.
	_ "time/tzdata"

	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// Weekdays is the seven days in the order a window lists them.
var Weekdays = []string{"MON", "TUE", "WED", "THU", "FRI", "SAT", "SUN"}

// TimeRange is one wall-clock range of a calling window, "HH:MM" to "HH:MM".
// End may be "24:00", and a range whose end is before its start crosses
// midnight: it belongs to the day it starts on.
type TimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// CallingWindow says when a contact may be dialled, in that contact's own
// local time: on these days, within these ranges. It binds in addition to the
// deployment's outer calling hours, never instead of them.
type CallingWindow struct {
	Days   []string    `json:"days"`
	Ranges []TimeRange `json:"ranges"`
}

// MaxWindowRanges bounds the ranges of one window. A day with more than a
// handful of calling periods is a mistake, not a schedule.
const MaxWindowRanges = 8

// Parsed returns the window's ranges as minutes. It is for the evaluator (the
// dialer): a stored window has already been through Normalize.
func (w CallingWindow) Parsed() ([]dayrange.Range, error) {
	out := make([]dayrange.Range, 0, len(w.Ranges))
	for _, r := range w.Ranges {
		parsed, err := dayrange.New(r.Start, r.End)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

// Normalize checks the window and returns it in canonical form: days in week
// order, ranges sorted by start and written HH:MM. outer is the deployment's
// outer calling hours; a range that is not wholly inside them is refused with
// ErrWindowOutsideLimit, and that is checked on every change, not only on
// create.
func (w CallingWindow) Normalize(outer dayrange.Range) (CallingWindow, error) {
	const field = "callingWindow"
	if len(w.Days) == 0 {
		return CallingWindow{}, invalid(field+".days", "EMPTY", "a calling window needs at least one day")
	}
	days := make([]string, 0, len(w.Days))
	for _, d := range w.Days {
		if !slices.Contains(Weekdays, d) {
			return CallingWindow{}, invalid(field+".days", "UNKNOWN_DAY",
				"%q is not a day; use MON, TUE, WED, THU, FRI, SAT or SUN", d)
		}
		if slices.Contains(days, d) {
			return CallingWindow{}, invalid(field+".days", "DUPLICATE_DAY", "%s is listed twice", d)
		}
		days = append(days, d)
	}
	slices.SortFunc(days, func(a, b string) int {
		return slices.Index(Weekdays, a) - slices.Index(Weekdays, b)
	})

	if len(w.Ranges) == 0 {
		return CallingWindow{}, invalid(field+".ranges", "EMPTY", "a calling window needs at least one time range")
	}
	if len(w.Ranges) > MaxWindowRanges {
		return CallingWindow{}, invalid(field+".ranges", "TOO_MANY",
			"a calling window has at most %d time ranges, got %d", MaxWindowRanges, len(w.Ranges))
	}
	parsed := make([]dayrange.Range, 0, len(w.Ranges))
	for i, r := range w.Ranges {
		at := fmt.Sprintf("%s.ranges[%d]", field, i)
		p, err := dayrange.New(r.Start, r.End)
		if err != nil {
			return CallingWindow{}, invalid(at, "INVALID_TIME", "%v", err)
		}
		if !p.Within(outer) {
			return CallingWindow{}, &Error{
				Kind: ErrWindowOutsideLimit, Field: at, Rule: "OUTSIDE_PERMITTED_HOURS",
				Message: fmt.Sprintf("%s is not inside the permitted calling hours %s", p, outer),
				Params:  map[string]any{"range": p.String(), "permittedHours": outer.String()},
			}
		}
		parsed = append(parsed, p)
	}
	slices.SortFunc(parsed, func(a, b dayrange.Range) int { return a.Start - b.Start })
	for i := 1; i < len(parsed); i++ {
		for j := 0; j < i; j++ {
			if parsed[j].Overlaps(parsed[i]) {
				return CallingWindow{}, invalid(field+".ranges", "OVERLAP",
					"%s and %s overlap", parsed[j], parsed[i])
			}
		}
	}

	out := CallingWindow{Days: days, Ranges: make([]TimeRange, 0, len(parsed))}
	for _, p := range parsed {
		out.Ranges = append(out.Ranges, TimeRange{Start: p.StartText(), End: p.EndText()})
	}
	return out, nil
}

// LoadZone resolves an IANA time zone name. The empty name and "Local" are
// refused: the first would silently mean UTC and the second the server's own
// zone, and neither is a place a customer is in.
func LoadZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, errors.New("not an IANA time zone name")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not a known IANA time zone", name)
	}
	return loc, nil
}
