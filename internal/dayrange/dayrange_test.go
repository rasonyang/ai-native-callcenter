// SPDX-License-Identifier: Apache-2.0

package dayrange

import "testing"

func TestParse(t *testing.T) {
	t.Parallel()
	good := map[string]Range{
		"08:00-21:00": {480, 1260},
		"22:00-02:00": {1320, 120},
		"00:00-24:00": {0, 1440},
		"22:00-00:00": {1320, 1440}, // 00:00 as an end is the end of the day
		"09:30-09:31": {570, 571},
	}
	for in, want := range good {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
		if again, err := Parse(got.String()); err != nil || again != got {
			t.Errorf("%q does not survive String/Parse: %v %v", in, again, err)
		}
	}
	for _, in := range []string{
		"", "9:00-17:00", "09:00", "09:00-", "-17:00", "09:00-09:00", "00:00-00:00",
		"24:00-02:00", "25:00-26:00", "09:60-10:00", "09:00-17:00-18:00", "09.00-17.00", "ab:cd-ef:gh",
		"09:00 - 17:00", "٠٩:٠٠-١٧:٠٠",
	} {
		if r, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", in, r)
		}
	}
}

func TestWithin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		r, outer string
		want     bool
	}{
		{"09:00-17:00", "08:00-21:00", true},
		{"08:00-21:00", "08:00-21:00", true},
		{"07:59-12:00", "08:00-21:00", false},
		{"20:00-21:01", "08:00-21:00", false},
		// A range across midnight is never inside a same-day outer range.
		{"22:00-02:00", "08:00-21:00", false},
		{"20:00-22:00", "08:00-21:00", false},
		// ... and can be inside one that crosses midnight itself.
		{"22:00-02:00", "20:00-04:00", true},
		{"20:00-04:00", "20:00-04:00", true},
		{"19:00-02:00", "20:00-04:00", false},
		{"22:00-05:00", "20:00-04:00", false},
		{"23:00-23:30", "20:00-04:00", true},
		{"01:00-03:00", "20:00-04:00", true},
		{"12:00-13:00", "20:00-04:00", false},
		// A same-day range cannot span the gap of a crossing outer range.
		{"03:00-21:00", "20:00-04:00", false},
		{"00:00-24:00", "00:00-24:00", true},
		{"22:00-00:00", "08:00-24:00", true},
	} {
		r, outer := mustParse(t, tc.r), mustParse(t, tc.outer)
		if got := r.Within(outer); got != tc.want {
			t.Errorf("%s within %s = %v, want %v", tc.r, tc.outer, got, tc.want)
		}
	}
}

func TestOverlaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"09:00-12:00", "12:00-15:00", false}, // half-open: they touch, they do not overlap
		{"09:00-12:01", "12:00-15:00", true},
		{"22:00-02:00", "01:00-03:00", true},
		{"22:00-02:00", "02:00-04:00", false},
		{"22:00-02:00", "21:00-22:00", false},
		{"22:00-02:00", "23:00-23:30", true},
	} {
		a, b := mustParse(t, tc.a), mustParse(t, tc.b)
		if got := a.Overlaps(b); got != tc.want || b.Overlaps(a) != tc.want {
			t.Errorf("%s overlaps %s = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestContains(t *testing.T) {
	t.Parallel()
	r := mustParse(t, "22:00-02:00")
	for minute, want := range map[int]bool{
		21*60 + 59: false, 22 * 60: true, 23*60 + 59: true, 0: true, 119: true, 120: false, 12 * 60: false,
	} {
		if got := r.Contains(minute); got != want {
			t.Errorf("Contains(%d) = %v, want %v", minute, got, want)
		}
	}
}

func mustParse(t *testing.T, s string) Range {
	t.Helper()
	r, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return r
}
