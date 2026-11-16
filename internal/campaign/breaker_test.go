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
