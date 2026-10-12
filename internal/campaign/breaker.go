// SPDX-License-Identifier: Apache-2.0

package campaign

import "errors"

// TrunkBreakerTrips reports whether the campaign's most recent n finished
// attempts all ended TRANSIENT or ORIGINATE_FAILED (design 10 section 7.4): a
// trunk outage otherwise spends the whole list's system retries in minutes.
// recent is newest first; fewer than n attempts never trip it, nor does one
// that breaks the run. n is AICC_TRUNK_BREAKER_COUNT.
func TrunkBreakerTrips(recent []Outcome, n int) bool {
	if n < 1 || len(recent) < n {
		return false
	}
	for _, o := range recent[:n] {
		if ClassOf(o) != ClassSystem {
			return false
		}
	}
	return true
}

// ProviderBreakerTrips reports whether the last m answered calls all ended
// BOT_FAILED: an answered number is not redialled, so a provider outage burns
// a lead per answered call. recent is newest first. m is
// AICC_PROVIDER_BREAKER_COUNT.
//
// Only answered calls are evidence about the provider. A call nobody answered
// says nothing and is skipped; so is OVERFLOW, which is a shortage of bot
// slots, not a failing provider. An ANSWERED call ends the run: a bot did its
// job.
func ProviderBreakerTrips(recent []Outcome, m int) bool {
	if m < 1 {
		return false
	}
	failed := 0
	for _, o := range recent {
		switch o {
		case OutcomeBotFailed:
			failed++
			if failed == m {
				return true
			}
		case OutcomeAnswered:
			return false
		}
	}
	return false
}

// DialErrorKinds names the synchronous errors of the call placer that mean
// something to an attempt. They are values the caller hands in, so that this
// package does not import the telephony layer. A nil kind never matches.
type DialErrorKinds struct {
	NumberOnDNC error
	BadNumber   error
	// LineLimit: the DID's outbound line limit is reached (design 10 section
	// 6.6); the placer cannot return it before that slice exists.
	LineLimit error
}

// DialVerdict is what a synchronous DialAI error does to the attempt.
type DialVerdict struct {
	// IsVoid: the attempt is deleted, nothing is counted, the contact is due
	// again at once.
	IsVoid bool
	// Outcome is how the attempt finishes; empty when IsVoid.
	Outcome Outcome
}

// ClassifyDialError maps the error DialAI returned before any channel existed
// (design 10 section 5.3):
//
//	number on DNC      -> DNC_SKIPPED
//	bad number         -> INVALID_NUMBER
//	DID line limit     -> void
//	anything else      -> ORIGINATE_FAILED
func ClassifyDialError(err error, k DialErrorKinds) DialVerdict {
	is := func(target error) bool { return target != nil && errors.Is(err, target) }
	switch {
	case is(k.NumberOnDNC):
		return DialVerdict{Outcome: OutcomeDNCSkipped}
	case is(k.BadNumber):
		return DialVerdict{Outcome: OutcomeInvalidNumber}
	case is(k.LineLimit):
		return DialVerdict{IsVoid: true}
	}
	return DialVerdict{Outcome: OutcomeOriginateFailed}
}
