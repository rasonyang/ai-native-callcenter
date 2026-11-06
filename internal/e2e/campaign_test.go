// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"

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
		if len(Regions) != 2 || len(DefaultCases) != 12 || len(AllCases) != 15 {
			t.Fatalf("%d regions, %d unanswered and %d cases in all, want 2, 12 and 15", len(Regions), len(DefaultCases), len(AllCases))
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
