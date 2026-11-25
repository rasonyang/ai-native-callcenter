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
		if want := i == LeadHigh || i == LeadMedium; IsLeadJudgeable(i) != want {
			t.Errorf("IsLeadJudgeable(%s) = %v", i, !want)
		}
	}
	// A deployment judges the leads it lists (AICC_LEAD_JUDGE_INTENTS), and
	// never one a judge cannot read, whatever the list says.
	for _, c := range []struct {
		name    string
		intents LeadJudgeIntents
		judged  []LeadIntent
	}{
		{"HIGH, the default", LeadJudgeIntents{LeadHigh}, []LeadIntent{LeadHigh}},
		{"MEDIUM alone", LeadJudgeIntents{LeadMedium}, []LeadIntent{LeadMedium}},
		{"both, as v0.9.2 did", LeadJudgeIntents{LeadHigh, LeadMedium}, []LeadIntent{LeadHigh, LeadMedium}},
		{"both, in the other order", LeadJudgeIntents{LeadMedium, LeadHigh}, []LeadIntent{LeadHigh, LeadMedium}},
		{"none: the judge is off", nil, nil},
		{"a lead no judge reads", LeadJudgeIntents{LeadLow, LeadHigh}, []LeadIntent{LeadHigh}},
	} {
		for _, i := range AllLeadIntents() {
			want := false
			for _, j := range c.judged {
				want = want || i == j
			}
			if got := c.intents.IsJudged(i); got != want {
				t.Errorf("%s: IsJudged(%s) = %v, want %v", c.name, i, got, want)
			}
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
