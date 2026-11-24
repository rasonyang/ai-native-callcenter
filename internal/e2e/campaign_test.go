// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// The simulator's scenario and the campaign cases are two files that must
// agree; the driver refuses a run when they do not, and this catches it
// before an environment is started.
func TestScenarioFileCoversEveryCampaignCase(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		s := loadScenarioFor(t, form)
		if err := CheckScenarioCoversCases(s, Regions, AllCases, form); err != nil {
			t.Fatal(err)
		}
		if len(Regions) != 2 || len(DefaultCases) != 14 || len(AllCases) != 17 {
			t.Fatalf("%d regions, %d unanswered and %d cases in all, want 2, 14 and 17", len(Regions), len(DefaultCases), len(AllCases))
		}
	})
}

// Every contact of a region is from that region's range, and the two ranges
// share nothing: a US campaign never holds a Chinese number, nor the reverse.
func TestRegionsNeverMixNumbers(t *testing.T) {
	want := map[string]string{"US": "+120255501", "CN": "+86139888866"}
	seen := map[string]string{}
	for _, r := range Regions {
		prefix, ok := want[r.Code]
		if !ok || r.Prefix != prefix {
			t.Fatalf("region %s has prefix %q, want %q", r.Code, r.Prefix, prefix)
		}
		for _, cs := range AllCases {
			n := r.Number(cs)
			if !phonenum.IsCanonical(n) || !strings.HasPrefix(n, prefix) || len(n) != len(prefix)+2 {
				t.Errorf("%s %s: %q is not a canonical number of %s", r.Code, cs.ID, n, prefix)
			}
			if prev, dup := seen[n]; dup {
				t.Errorf("%s %s and %s share %s", r.Code, cs.ID, prev, n)
			}
			seen[n] = r.Code + " " + cs.ID
		}
	}
	if got := len(seen); got != len(Regions)*len(AllCases) {
		t.Fatalf("%d distinct numbers, want %d", got, len(Regions)*len(AllCases))
	}
}

// A dialog is attributed to a campaign by the caller id in its From header.
func TestSIPUserOfAFromHeader(t *testing.T) {
	for in, want := range map[string]string{
		`"95012345" <sip:95012345@10.70.1.11>;tag=abc`: "95012345",
		`<sip:+12025550103@10.70.1.13:5080>`:           "+12025550103",
		`sip:95000001@host`:                            "95000001",
		`garbage`:                                      "",
	} {
		if got := sipUser(in); got != want {
			t.Errorf("sipUser(%q) = %q, want %q", in, got, want)
		}
	}
}

// c07Ring runs assessDialog on a C07 dialog whose stamps are set as given:
// the INVITE at t0, the CANCEL wallMs later on the wall clock, the attempt
// started startMs before the INVITE, and the simulator's monotonic reading of
// the ring is monoMs. It returns the failures of the ring check and the
// attempt result.
func c07Ring(t *testing.T, wallMs, monoMs float64, startMs int) ([]string, AttemptResult) {
	t.Helper()
	var cs Case
	for _, c := range AllCases {
		if c.ID == "C07" {
			cs = c
		}
	}
	t0 := time.Date(2026, 11, 9, 0, 52, 34, 468e6, time.UTC)
	rv := RegionView{}
	rv.DID.Number = "95000001"
	d := calleesim.DialogRecord{
		InviteAt:      t0,
		InviteHeaders: map[string]string{"from": "<sip:95000001@10.0.0.11>"},
		Provisionals:  []calleesim.ProvisionalRecord{{Status: 180}},
		Final:         &calleesim.FinalRecord{Status: 487},
		End: &calleesim.EndRecord{
			Method: calleesim.EndedWithCancel, By: calleesim.EndedByCaller,
			At: t0.Add(time.Duration(wallMs * float64(time.Millisecond))), AfterInviteMs: monoMs,
		},
	}
	a := api.CampaignAttempt{StartedAt: t0.Add(-time.Duration(startMs) * time.Millisecond)}
	var col Checks
	var ar AttemptResult
	assessDialog(&col, "", rv, cs, a, &d, nil, &ar)
	var ring []string
	for _, f := range col.failures {
		if strings.Contains(f, "the CANCEL came at the 5 s ring timeout") {
			ring = append(ring, f)
		}
	}
	for _, c := range col.checks {
		if strings.Contains(c, "the CANCEL came at the 5 s ring timeout") {
			return ring, ar
		}
	}
	t.Fatal("the ring check did not run")
	return nil, ar
}

// C07 reads the ring on the simulator's monotonic clock, from 3.75 s (the 4 s
// the switch's whole-second check guarantees, less the INVITE's lag behind the
// originate) to 6 s (the timeout and the tolerance).
func TestC07BoundReadsTheMonotonicRing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wall, mo float64
		pass     bool
	}{
		{"below the lower bound", 5000, 3749, false},
		{"the lower bound", 5000, 3750, true},
		{"a true ring", 5000, 4879, true},
		{"the upper bound", 5000, 6000, true},
		{"above the upper bound", 5000, 6001, false},
		{"no measured value", 5000, 0, false},
		// Measured: the wall clock stepped back 2050 ms inside a 4864 ms ring.
		{"wall 2814 ms, monotonic 4864 ms", 2814, 4864, true},
		// The wall clock reads a perfect 5 s; the ring was only 2.5 s.
		{"wall 5000 ms, monotonic 2500 ms", 5000, 2500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures, ar := c07Ring(t, tc.wall, tc.mo, 23)
			if got := len(failures) == 0; got != tc.pass {
				t.Fatalf("passed = %v, want %v: %v", got, tc.pass, failures)
			}
			if ar.CancelAfterInviteMs == nil || *ar.CancelAfterInviteMs != tc.mo {
				t.Errorf("cancelAfterInviteMs = %v, want %v", ar.CancelAfterInviteMs, tc.mo)
			}
			if ar.CancelAfterInviteWallMs == nil || *ar.CancelAfterInviteWallMs != tc.wall {
				t.Errorf("cancelAfterInviteWallMs = %v, want %v", ar.CancelAfterInviteWallMs, tc.wall)
			}
			if ar.CancelAfterStartMs == nil || *ar.CancelAfterStartMs != tc.wall+23 {
				t.Errorf("cancelAfterStartMs = %v, want %v", ar.CancelAfterStartMs, tc.wall+23)
			}
			if len(failures) > 0 && !strings.Contains(failures[0], "wall clock") {
				t.Errorf("the failure does not give the wall-clock values: %s", failures[0])
			}
		})
	}
}

// C07 and C11 hold the CDR's tech to who ended the dialled leg: our ring
// timeout's CANCEL leaves PLATFORM and no status, the far end's own 487 leaves
// PEER and 487.
func TestAssessTechReadsWhoEndedTheLeg(t *testing.T) {
	tech := func(kv ...any) *map[string]interface{} {
		m := map[string]interface{}{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return &m
	}
	c07, c11 := caseByID(AllCases, "C07"), caseByID(AllCases, "C11")
	if c07.Tech == nil || c11.Tech == nil {
		t.Fatal("C07 and C11 assert the CDR's tech")
	}
	for _, tc := range []struct {
		name string
		want TechExpectation
		tech *map[string]interface{}
		pass bool
	}{
		{"C07 as expected", *c07.Tech, tech("endedBy", "PLATFORM"), true},
		{"C07 with a status", *c07.Tech, tech("endedBy", "PLATFORM", "sipStatus", float64(487)), false},
		{"C07 ended by the peer", *c07.Tech, tech("endedBy", "PEER"), false},
		{"C11 as expected", *c11.Tech, tech("endedBy", "PEER", "sipStatus", float64(487)), true},
		{"C11 without endedBy", *c11.Tech, tech("sipStatus", float64(487)), false},
		{"C11 ended by us", *c11.Tech, tech("endedBy", "PLATFORM", "sipStatus", float64(487)), false},
		{"C11 without a status", *c11.Tech, tech("endedBy", "PEER"), false},
		{"no tech at all", *c11.Tech, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var col Checks
			assessTech(&col, "", tc.want, tc.tech)
			if got := len(col.failures) == 0; got != tc.pass {
				t.Errorf("passed = %v, want %v: %v", got, tc.pass, col.failures)
			}
		})
	}
}

// C11's 487 must be the callee's own: a dialog our CANCEL ended fails it.
func TestC11RefusesA487ThatAnsweredOurCancel(t *testing.T) {
	cs := caseByID(AllCases, "C11")
	t0 := time.Date(2026, 11, 18, 7, 47, 6, 0, time.UTC)
	rv := RegionView{}
	rv.DID.Number = "95000001"
	for _, tc := range []struct {
		name string
		end  *calleesim.EndRecord
		pass bool
	}{
		{"the callee's own 487", nil, true},
		{"a 487 after our CANCEL", &calleesim.EndRecord{Method: calleesim.EndedWithCancel, By: calleesim.EndedByCaller, At: t0.Add(2 * time.Second)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := calleesim.DialogRecord{
				InviteAt:      t0,
				InviteHeaders: map[string]string{"from": "<sip:95000001@10.0.0.11>"},
				Provisionals:  []calleesim.ProvisionalRecord{{Status: 180}},
				Final:         &calleesim.FinalRecord{Status: 487},
				End:           tc.end,
			}
			var col Checks
			var ar AttemptResult
			assessDialog(&col, "", rv, cs, api.CampaignAttempt{StartedAt: t0}, &d, nil, &ar)
			var own []string
			for _, f := range col.failures {
				if strings.Contains(f, "the caller sent no CANCEL") {
					own = append(own, f)
				}
			}
			if got := len(own) == 0; got != tc.pass {
				t.Errorf("passed = %v, want %v: %v", got, tc.pass, col.failures)
			}
		})
	}
}

// C12 is the line refusing the call: a 402 before any ring, under the same
// cause as C05's 603. The case expects LINE_REJECTED, a system outcome the
// e2e policy retries, and the CDR and the attempt to carry the 402; the
// design's rule reads the status, not the ring.
func TestC12IsALineRefusalBesideC05(t *testing.T) {
	c05, c12 := caseByID(AllCases, "C05"), caseByID(AllCases, "C12")
	if c12.Outcome != "LINE_REJECTED" || c12.Cause != causeCallRejected || c12.SIPStatus != 402 || c12.Provisional != 0 {
		t.Fatalf("C12 = %+v", c12)
	}
	if c05.Cause != c12.Cause || c05.Tech == nil || c05.Tech.SIPStatus != 603 || c12.Tech == nil || c12.Tech.SIPStatus != 402 {
		t.Fatalf("C05 and C12 must share the cause and assert their statuses: %+v %+v", c05.Tech, c12.Tech)
	}
	if d := designOutcomes[c12.Outcome]; d.Budget != BudgetSystem || d.TerminalStatus != "" {
		t.Errorf("LINE_REJECTED in the design table = %+v, want a retried system outcome", d)
	}
	if d := designOutcomes[c05.Outcome]; d.Budget != BudgetAttempts {
		t.Errorf("REJECTED in the design table = %+v, want maxAttempts", d)
	}
	for _, tc := range []struct {
		name  string
		final int
		rang  bool
		want  string
	}{
		{"402 before any ring", 402, false, "LINE_REJECTED"},
		{"402 after a 180", 402, true, "LINE_REJECTED"},
		{"403", 403, false, "LINE_REJECTED"},
		{"603 after a 180", 603, true, "REJECTED"},
		{"603 before any ring", 603, false, "REJECTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &calleesim.DialogRecord{Final: &calleesim.FinalRecord{Status: tc.final}}
			if tc.rang {
				d.Provisionals = []calleesim.ProvisionalRecord{{Status: 180}}
			}
			if got, _, ok := expectedOutcome(causeCallRejected, d); !ok || got != tc.want {
				t.Errorf("expectedOutcome = %s (%v), want %s", got, ok, tc.want)
			}
		})
	}
	if _, _, ok := expectedOutcome(causeCallRejected, nil); ok {
		t.Error("a CALL_REJECTED with no dialog to read was given an outcome")
	}
}

// A case whose callee sends no provisional is held to that: a dialog that
// rang first fails it, and the attempt's own sipStatus is checked against the
// case's.
func TestC12AssessesAFinalWithNoRingAndTheAttemptsStatus(t *testing.T) {
	cs := caseByID(AllCases, "C12")
	t0 := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	rv := RegionView{}
	rv.DID.Number = "95000001"
	for _, tc := range []struct {
		name  string
		provs []calleesim.ProvisionalRecord
		pass  bool
	}{
		{"no provisional", nil, true},
		{"a 180 first", []calleesim.ProvisionalRecord{{Status: 180}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := calleesim.DialogRecord{
				InviteAt:      t0,
				InviteHeaders: map[string]string{"from": "<sip:95000001@10.0.0.11>"},
				Provisionals:  tc.provs,
				Final:         &calleesim.FinalRecord{Status: 402},
			}
			var col Checks
			var ar AttemptResult
			assessDialog(&col, "", rv, cs, api.CampaignAttempt{StartedAt: t0}, &d, nil, &ar)
			var ring []string
			for _, f := range col.failures {
				if strings.Contains(f, "no 180 or 183") {
					ring = append(ring, f)
				}
			}
			if got := len(ring) == 0; got != tc.pass {
				t.Errorf("passed = %v, want %v: %v", got, tc.pass, col.failures)
			}
		})
	}
	status := func(n int) *int { return &n }
	for _, tc := range []struct {
		name string
		want int
		got  *int
		pass bool
	}{
		{"the case's status", 402, status(402), true},
		{"another status", 402, status(603), false},
		{"absent", 402, nil, false},
		{"none expected and none carried", 0, nil, true},
		{"none expected and one carried", 0, status(487), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var col Checks
			assessAttemptStatus(&col, "", tc.want, tc.got)
			if got := len(col.failures) == 0; got != tc.pass {
				t.Errorf("passed = %v, want %v: %v", got, tc.pass, col.failures)
			}
		})
	}
}
