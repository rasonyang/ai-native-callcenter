// SPDX-License-Identifier: Apache-2.0

package telephony

import "testing"

func TestRTPRangesOverlap(t *testing.T) {
	set := func(a, b int) RTPRange { return RTPRange{Start: a, End: b, StartSet: true, EndSet: true} }
	builtIn := RTPRange{Start: DefaultSwitchRTPStart, End: DefaultSwitchRTPEnd}
	cases := []struct {
		name      string
		low, high int
		sw        RTPRange
		want      bool
	}{
		{"identical", 26000, 29999, set(26000, 29999), true},
		{"bot inside the switch", 26000, 29999, set(20000, 32000), true},
		{"switch inside the bot", 20000, 32000, set(26000, 29999), true},
		{"bot low equals switch end", 26000, 29999, set(20000, 26000), true},
		{"bot low just past switch end", 26000, 29999, set(20000, 25999), false},
		{"bot high equals switch start", 26000, 29999, set(29999, 35000), true},
		{"bot high just below switch start", 26000, 29999, set(30000, 35000), false},
		{"unset switch is the built-in range", 26000, 29999, builtIn, true},
		{"built-in range against 40000-40999", 40000, 40999, builtIn, false},
		{"new defaults", 26000, 29999, set(20000, 25999), false},
	}
	for _, tc := range cases {
		if got := RTPRangesOverlap(tc.low, tc.high, tc.sw); got != tc.want {
			t.Errorf("%s: overlap(%d-%d, %d-%d) = %v, want %v", tc.name, tc.low, tc.high, tc.sw.Start, tc.sw.End, got, tc.want)
		}
	}
}

func TestParseSwitchRTPRange(t *testing.T) {
	conf := func(params string) string {
		return `<configuration name="switch.conf"><settings>` + params + `</settings></configuration>`
	}
	r, err := ParseSwitchRTPRange(conf(`<param name="rtp-start-port" value="20000"/><param name="rtp-end-port" value="25999"/>`))
	if err != nil || r != (RTPRange{Start: 20000, End: 25999, StartSet: true, EndSet: true}) || r.Legs() != 3000 {
		t.Fatalf("got %+v legs %d err %v", r, r.Legs(), err)
	}
	r, err = ParseSwitchRTPRange(conf(`<param name="rtp-start-port" value="0"/>`))
	if err != nil || !r.IsBuiltIn() || r.Start != 16384 || r.End != 32768 {
		t.Fatalf("got %+v err %v", r, err)
	}
	if _, err = ParseSwitchRTPRange(conf(`<param name="rtp-end-port" value="70000"/>`)); !IsRTPValueError(err) {
		t.Fatalf("err %v, want a value error", err)
	}
	if _, err = ParseSwitchRTPRange("garbage <<<"); err == nil || IsRTPValueError(err) {
		t.Fatalf("err %v, want an unreadable-reply error", err)
	}
}
