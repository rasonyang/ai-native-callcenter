// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"slices"
	"testing"
)

func repeat(o Outcome, n int) []Outcome {
	out := make([]Outcome, n)
	for i := range out {
		out[i] = o
	}
	return out
}

func TestTheTrunkBreakerTripsOnTheNthFailureAndNotTheOneBefore(t *testing.T) {
	const n = 20
	tests := []struct {
		name   string
		recent []Outcome // newest first
		want   bool
	}{
		{"no history", nil, false},
		{"n-1 transient", repeat(OutcomeTransient, n-1), false},
		{"n transient", repeat(OutcomeTransient, n), true},
		{"n originate failed", repeat(OutcomeOriginateFailed, n), true},
		{"a mix of the two", slices.Concat(repeat(OutcomeTransient, 7), repeat(OutcomeOriginateFailed, n-7)), true},
		{"n-1 then a success", slices.Concat(repeat(OutcomeTransient, n-1), []Outcome{OutcomeNoAnswer}, repeat(OutcomeTransient, 5)), false},
		{"a success inside the run", slices.Concat(repeat(OutcomeTransient, 5), []Outcome{OutcomeBusy}, repeat(OutcomeTransient, n)), false},
		{"older failures do not count", slices.Concat([]Outcome{OutcomeAnswered}, repeat(OutcomeTransient, n)), false},
		{"a bad number breaks the run", slices.Concat([]Outcome{OutcomeInvalidNumber}, repeat(OutcomeTransient, n)), false},
		{"more than n", repeat(OutcomeTransient, n+10), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TrunkBreakerTrips(tt.recent, n); got != tt.want {
				t.Errorf("TrunkBreakerTrips = %v, want %v", got, tt.want)
			}
		})
	}
	if TrunkBreakerTrips(repeat(OutcomeTransient, 3), 0) {
		t.Error("a breaker of 0 must not trip on everything")
	}
}

func TestTheProviderBreakerTripsOnTheMthConsecutiveBotFailure(t *testing.T) {
	const m = 5
	tests := []struct {
		name   string
		recent []Outcome // newest first
		want   bool
	}{
		{"no history", nil, false},
		{"m-1 bot failures", repeat(OutcomeBotFailed, m-1), false},
		{"m bot failures", repeat(OutcomeBotFailed, m), true},
		{"an answered call in the run", slices.Concat(repeat(OutcomeBotFailed, 2), []Outcome{OutcomeAnswered}, repeat(OutcomeBotFailed, m)), false},
		{"unanswered calls between do not reset it", []Outcome{
			OutcomeBotFailed, OutcomeNoAnswer, OutcomeBotFailed, OutcomeBusy, OutcomeTransient,
			OutcomeBotFailed, OutcomeBotFailed, OutcomeRejected, OutcomeBotFailed}, true},
		{"unanswered calls alone do not trip it", repeat(OutcomeNoAnswer, 50), false},
		{"an overflow is not the provider's fault", slices.Concat(repeat(OutcomeBotFailed, m-1), []Outcome{OutcomeOverflow}), false},
		{"an overflow does not end the run", slices.Concat(repeat(OutcomeBotFailed, 2), []Outcome{OutcomeOverflow}, repeat(OutcomeBotFailed, m-2)), true},
		{"old failures behind a success are history", slices.Concat([]Outcome{OutcomeAnswered}, repeat(OutcomeBotFailed, m)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProviderBreakerTrips(tt.recent, m); got != tt.want {
				t.Errorf("ProviderBreakerTrips = %v, want %v", got, tt.want)
			}
		})
	}
	if ProviderBreakerTrips(repeat(OutcomeBotFailed, 3), 0) {
		t.Error("a breaker of 0 must not trip on everything")
	}
}

// A carrier that ends every unanswered call with a 408 after ringing is not a
// trunk outage: each of those calls is NO_ANSWER, a customer outcome, and so
// breaks the run. A 408 that never rang, or our own recovery's reap, is still
// the system failure it was (#576). The outcomes come out of Classify.
func TestACarriers408AfterRingingDoesNotTripTheTrunkBreaker(t *testing.T) {
	const n = 20
	classified := func(e CallEnd) []Outcome {
		out := make([]Outcome, n)
		for i := range out {
			out[i] = Classify(e)
		}
		return out
	}
	rang := CallEnd{HangupCause: CauseRecovery, SIPStatus: 408, HasRingProgress: true}
	if got := Classify(rang); got != OutcomeNoAnswer {
		t.Fatalf("Classify(408 after ringing) = %s, want NO_ANSWER", got)
	}
	if TrunkBreakerTrips(classified(rang), n) {
		t.Errorf("%d unanswered calls the carrier ended with a 408 after ringing tripped the trunk breaker", n)
	}
	for name, e := range map[string]CallEnd{
		"a 408 with no ring progress":   {HangupCause: CauseRecovery, SIPStatus: 408},
		"our own reap of a ringing leg": {HangupCause: CauseRecovery, HasRingProgress: true},
	} {
		if !TrunkBreakerTrips(classified(e), n) {
			t.Errorf("%d consecutive endings of %s did not trip the trunk breaker", n, name)
		}
	}
}

// A far end that ends every ringing call with a 487 of its own (a ring timer
// shorter than ours, or callees who decline) is not a trunk outage either:
// each call is NO_ANSWER and breaks the run, as any unanswered call does. A
// real outage still trips the breaker: the carrier's 503s, a route that does
// not exist, originates that never make a channel, and an ORIGINATOR_CANCEL
// that is not a far end's 487 after ringing. The outcomes come out of Classify.
func TestAFarEnds487AfterRingingDoesNotTripTheTrunkBreaker(t *testing.T) {
	const n = 20
	classified := func(e CallEnd, k int) []Outcome {
		out := make([]Outcome, k)
		for i := range out {
			out[i] = Classify(e)
		}
		return out
	}
	declined := CallEnd{HangupCause: CauseOriginatorCancel, SIPStatus: 487, HasRingProgress: true, EndedBy: "PEER"}
	if got := Classify(declined); got != OutcomeNoAnswer {
		t.Fatalf("Classify(a far end's 487 after ringing) = %s, want NO_ANSWER", got)
	}
	if TrunkBreakerTrips(classified(declined, n), n) {
		t.Errorf("%d unanswered calls the far end ended with a 487 after ringing tripped the trunk breaker", n)
	}

	outages := map[string]CallEnd{
		"the carrier's 503":                 {HangupCause: "NORMAL_TEMPORARY_FAILURE", SIPStatus: 503},
		"no route to the number":            {HangupCause: "NO_ROUTE_DESTINATION", SIPStatus: 404},
		"an originate that made no channel": {HangupCause: "GATEWAY_DOWN", IsPlacementFailure: true},
		"a 487 with no ringing before it":   {HangupCause: CauseOriginatorCancel, SIPStatus: 487},
		"a cancel with no status":           {HangupCause: CauseOriginatorCancel, HasRingProgress: true, EndedBy: "PLATFORM"},
	}
	for name, e := range outages {
		if !TrunkBreakerTrips(classified(e, n), n) {
			t.Errorf("%d consecutive endings of %s did not trip the trunk breaker", n, name)
		}
	}

	// It resets the run like any NO_ANSWER: one far end's 487 among the
	// outage's failures, newest first, keeps the breaker shut until n more
	// failures follow it.
	outage := outages["the carrier's 503"]
	broken := slices.Concat(classified(outage, n-1), classified(declined, 1), classified(outage, n))
	if TrunkBreakerTrips(broken, n) {
		t.Error("a far end's 487 after ringing did not break the run of failures")
	}
	if !TrunkBreakerTrips(slices.Concat(classified(outage, n), broken), n) {
		t.Error("n failures after the 487 did not trip the trunk breaker")
	}
}

// A line that refuses every call (a carrier's 402 for an account out of
// credit, before any ring) is a trunk outage: n of them in a row pause the
// campaign. n callees who decline with a 603 are n customers and never trip
// it, and one decline breaks a run of refusals. The outcomes come out of
// Classify.
func TestALineThatRefusesEveryCallTripsTheTrunkBreakerAndDeclinesDoNot(t *testing.T) {
	const n = 20
	classified := func(e CallEnd, k int) []Outcome {
		out := make([]Outcome, k)
		for i := range out {
			out[i] = Classify(e)
		}
		return out
	}
	refused := CallEnd{HangupCause: CauseCallRejected, SIPStatus: 402, EndedBy: "PEER"}
	declined := CallEnd{HangupCause: CauseCallRejected, SIPStatus: 603, HasRingProgress: true, EndedBy: "PEER"}

	if got := classified(refused, n-1); TrunkBreakerTrips(got, n) {
		t.Errorf("%d line refusals tripped the trunk breaker of %d", n-1, n)
	}
	if !TrunkBreakerTrips(classified(refused, n), n) {
		t.Errorf("%d consecutive 402s did not trip the trunk breaker", n)
	}
	if TrunkBreakerTrips(classified(declined, n), n) {
		t.Errorf("%d consecutive 603s tripped the trunk breaker", n)
	}
	for name, e := range map[string]CallEnd{
		"a 402 after ringing": {HangupCause: CauseCallRejected, SIPStatus: 402, HasRingProgress: true},
		"a 403":               {HangupCause: CauseCallRejected, SIPStatus: 403},
		"a 401":               {HangupCause: CauseCallRejected, SIPStatus: 401},
		"a 407":               {HangupCause: CauseCallRejected, SIPStatus: 407},
		"a 608":               {HangupCause: CauseCallRejected, SIPStatus: 608},
	} {
		if !TrunkBreakerTrips(classified(e, n), n) {
			t.Errorf("%d consecutive endings of %s did not trip the trunk breaker", n, name)
		}
	}
	if TrunkBreakerTrips(classified(CallEnd{HangupCause: CauseCallRejected}, n), n) {
		t.Errorf("%d refusals with no status (the callee's REJECTED) tripped the trunk breaker", n)
	}

	broken := slices.Concat(classified(refused, n-1), classified(declined, 1), classified(refused, n-1))
	if TrunkBreakerTrips(broken, n) {
		t.Error("a 603 did not break the run of line refusals")
	}
	// A line refusal joins a run of the other system failures.
	mixed := slices.Concat(classified(refused, n/2), classified(CallEnd{HangupCause: "NORMAL_TEMPORARY_FAILURE", SIPStatus: 503}, n-n/2))
	if !TrunkBreakerTrips(mixed, n) {
		t.Error("line refusals mixed with 503s did not trip the trunk breaker")
	}
}
