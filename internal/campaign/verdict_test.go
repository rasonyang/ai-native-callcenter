// SPDX-License-Identifier: Apache-2.0

package campaign

import "testing"

// The owner's table (2026-11-24): what each verdict leaves of the lead the
// flow declared.
func TestAVerdictLeavesTheLeadTheOwnerDecided(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		declared LeadIntent
		verdict  LeadVerdict
		want     LeadIntent
	}{
		{LeadHigh, VerdictConfirmed, LeadHigh},
		{LeadMedium, VerdictConfirmed, LeadMedium},
		{LeadHigh, VerdictUndecided, LeadMedium},
		{LeadMedium, VerdictUndecided, LeadMedium},
		{LeadHigh, VerdictRefused, LeadNone},
		{LeadMedium, VerdictRefused, LeadNone},
		{LeadHigh, VerdictMachine, LeadNone},
		{LeadMedium, VerdictMachine, LeadNone},
		{LeadHigh, VerdictNoEvidence, LeadNone},
		{LeadMedium, VerdictNoEvidence, LeadNone},
		{LeadHigh, VerdictNotJudged, LeadHigh},
		{LeadMedium, VerdictNotJudged, LeadMedium},
		{LeadHigh, VerdictPending, LeadHigh},
	} {
		if got := LeadAfterVerdict(c.declared, c.verdict); got != c.want {
			t.Errorf("%s judged %s leaves %s, want %s", c.declared, c.verdict, got, c.want)
		}
	}
}

func TestOnlyHighAndMediumLeadsAreJudged(t *testing.T) {
	t.Parallel()
	for _, i := range AllLeadIntents() {
		if want := i == LeadHigh || i == LeadMedium; IsLeadJudged(i) != want {
			t.Errorf("IsLeadJudged(%s) = %v", i, !want)
		}
	}
	if VerdictPending.IsFinal() || !VerdictNotJudged.IsFinal() || LeadVerdict("MAYBE").IsFinal() {
		t.Error("IsFinal: PENDING and unknown values are not final, every verdict is")
	}
	if LeadJudgeHold <= LeadJudgeDeadline {
		t.Errorf("the hold (%s) must outlast the deadline (%s), or a verdict in time could miss the delivery",
			LeadJudgeHold, LeadJudgeDeadline)
	}
}
