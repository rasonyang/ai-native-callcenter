// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"slices"
	"time"
)

// LeadVerdict is what a text model reading the finished call said about the
// lead the flow declared (design 10 section 11.1). It is set only on an
// attempt whose lead was judged; the values are byte-identical to the
// contract's CampaignLeadVerdict and to campaign_attempts.lead_verdict.
type LeadVerdict string

const (
	// VerdictPending: the judgement is owed and its deadline has not passed.
	VerdictPending LeadVerdict = "PENDING"
	// VerdictConfirmed: a callee line states acceptance in plain words.
	VerdictConfirmed LeadVerdict = "CONFIRMED"
	// VerdictUndecided: a live person talked about the offer and neither
	// accepted nor refused it, or accepted in words that have to be inferred.
	VerdictUndecided LeadVerdict = "UNDECIDED"
	// VerdictRefused: the callee declined, deferred, or took an acceptance back.
	VerdictRefused LeadVerdict = "REFUSED"
	// VerdictMachine: a voicemail prompt, call assistant, IVR or carrier
	// announcement answered.
	VerdictMachine LeadVerdict = "MACHINE"
	// VerdictNoEvidence: no callee words bear on the offer.
	VerdictNoEvidence LeadVerdict = "NO_EVIDENCE"
	// VerdictNotJudged: no verdict was reached before the deadline (the model
	// failed, timed out or answered something unusable); the declared lead
	// stands.
	VerdictNotJudged LeadVerdict = "NOT_JUDGED"
)

// AllLeadVerdicts is every verdict, PENDING included, in declaration order.
func AllLeadVerdicts() []LeadVerdict {
	return []LeadVerdict{VerdictPending, VerdictConfirmed, VerdictUndecided, VerdictRefused,
		VerdictMachine, VerdictNoEvidence, VerdictNotJudged}
}

// IsValid reports whether v is one of the verdicts.
func (v LeadVerdict) IsValid() bool { return slices.Contains(AllLeadVerdicts(), v) }

// IsFinal reports whether v ends a judgement: every verdict but PENDING.
func (v LeadVerdict) IsFinal() bool { return v != VerdictPending && v.IsValid() }

// The bounds of one judgement, from the moment the CDR is written. The
// verdict must land before LeadJudgeDeadline; the call's webhook delivery is
// held LeadJudgeHold, so a verdict that lands in time always replaces the held
// payload before it can go out, and one that never lands lets the held
// payload (which already says NOT_JUDGED) go out by itself.
const (
	LeadJudgeDeadline = 30 * time.Second
	LeadJudgeHold     = 35 * time.Second
)

// IsLeadJudged reports whether a call's lead is one the text model reads:
// HIGH and MEDIUM, the two that record the customer's interest.
func IsLeadJudged(i LeadIntent) bool { return i == LeadHigh || i == LeadMedium }

// LeadAfterVerdict is the effective lead a verdict leaves of the one the flow
// declared (owner decision, 2026-11-24): CONFIRMED keeps it; UNDECIDED lowers
// HIGH to MEDIUM; REFUSED, MACHINE and NO_EVIDENCE leave the call with no lead
// (NONE); NOT_JUDGED, and a judgement still PENDING, keep it.
func LeadAfterVerdict(declared LeadIntent, v LeadVerdict) LeadIntent {
	switch v {
	case VerdictUndecided:
		if declared == LeadHigh {
			return LeadMedium
		}
		return declared
	case VerdictRefused, VerdictMachine, VerdictNoEvidence:
		return LeadNone
	default:
		return declared
	}
}
