// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"slices"
	"time"
)

// Counters are a contact's two budgets, spent.
type Counters struct {
	// Attempts are the attempts that reached the customer.
	Attempts int
	// SystemFailures are the attempts that never did.
	SystemFailures int
}

// Add spends what an outcome costs.
func (c Counters) Add(o Outcome) Counters {
	switch ClassOf(o) {
	case ClassReached:
		c.Attempts++
	case ClassSystem:
		c.SystemFailures++
	}
	return c
}

// Remove gives back what an outcome cost: the inverse of Add, never below
// zero. It is how a later, fuller CDR replaces the outcome of an attempt
// without counting the attempt twice.
func (c Counters) Remove(o Outcome) Counters {
	switch ClassOf(o) {
	case ClassReached:
		c.Attempts = max(0, c.Attempts-1)
	case ClassSystem:
		c.SystemFailures = max(0, c.SystemFailures-1)
	}
	return c
}

// Settlement is what an ended attempt does to its contact.
type Settlement struct {
	Status ContactStatus
	// NextAttemptAt is when the contact may be dialled again; nil unless the
	// contact is ACTIVE.
	NextAttemptAt *time.Time
}

// Settle decides a contact's fate after an attempt ended with outcome.
// counters already include that attempt. lead is the intent the call recorded
// ("" for none). finishedAt is when the call ended; the retry is measured from
// it. s is the contact's schedule: a retry that falls outside it moves to the
// next window's start and is never dropped.
//
// The rules, in order:
//
//   - a lead of DO_NOT_CALL ends the contact as DNC, whatever the outcome;
//   - INVALID_NUMBER and DNC_SKIPPED end it as INVALID_NUMBER and DNC;
//   - an answered outcome (ANSWERED, BOT_FAILED, OVERFLOW) ends it as DONE and
//     never has a next attempt, however the call went;
//   - an outcome the policy does not retry ends it as EXHAUSTED: nothing more
//     will be tried, and last_outcome says why;
//   - a budget spent (attempts at maxAttempts for a reached outcome,
//     system_failures at maxSystemRetries for a system one) ends it as
//     EXHAUSTED;
//   - otherwise it stays ACTIVE, due at finishedAt plus the outcome's
//     interval (the transient backoff for TRANSIENT and ORIGINATE_FAILED,
//     counted by system_failures) and moved into the schedule.
func Settle(p RetryPolicy, c Counters, o Outcome, lead LeadIntent, finishedAt time.Time, s Schedule) Settlement {
	switch {
	case lead == LeadDoNotCall:
		return Settlement{Status: ContactDoNotCall}
	case o == OutcomeInvalidNumber:
		return Settlement{Status: ContactInvalidNumber}
	case o == OutcomeDNCSkipped:
		return Settlement{Status: ContactDoNotCall}
	case IsAnsweredOutcome(o):
		return Settlement{Status: ContactDone}
	case !slices.Contains(p.RetryOutcomes, o):
		return Settlement{Status: ContactExhausted}
	}

	var wait time.Duration
	switch ClassOf(o) {
	case ClassReached:
		if c.Attempts >= p.MaxAttempts {
			return Settlement{Status: ContactExhausted}
		}
		sec, ok := p.IntervalSec[o]
		if !ok {
			sec = DefaultRetryPolicy().IntervalSec[o]
		}
		wait = time.Duration(sec) * time.Second
	case ClassSystem:
		if c.SystemFailures >= p.MaxSystemRetries {
			return Settlement{Status: ContactExhausted}
		}
		wait = p.TransientBackoff.Wait(c.SystemFailures)
	default:
		return Settlement{Status: ContactExhausted}
	}

	due := finishedAt.Add(wait)
	if open, ok := s.NextOpen(due); ok {
		due = open
	}
	return Settlement{Status: ContactActive, NextAttemptAt: &due}
}

// Wait is the delay before retry number n (1 for the first): BaseSec times
// Factor to the power n-1, never more than CapSec.
func (b Backoff) Wait(n int) time.Duration {
	capped := time.Duration(b.CapSec) * time.Second
	wait := time.Duration(b.BaseSec) * time.Second
	for i := 1; i < n; i++ {
		if wait >= capped || b.Factor <= 1 {
			break
		}
		wait *= time.Duration(b.Factor)
	}
	return min(wait, capped)
}
