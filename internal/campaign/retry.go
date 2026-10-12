// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"fmt"
	"slices"
)

// Backoff spaces the retries of a failure that never reached the customer
// (TRANSIENT, ORIGINATE_FAILED): the first retry comes BaseSec after the
// failure, each further one Factor times later than the one before, never more
// than CapSec.
type Backoff struct {
	BaseSec int `json:"baseSec"`
	Factor  int `json:"factor"`
	CapSec  int `json:"capSec"`
}

// RetryPolicy is what happens to a contact after an attempt ends without
// settling it (design 10 section 7). The outcomes that settle a contact
// (ANSWERED, BOT_FAILED, OVERFLOW, INVALID_NUMBER, DNC_SKIPPED) are not
// configurable and do not appear here.
type RetryPolicy struct {
	// MaxAttempts is how many times the customer's phone may be rung.
	MaxAttempts int `json:"maxAttempts"`
	// MaxSystemRetries is how many times our own or the network's failures
	// are retried; they never use up MaxAttempts.
	MaxSystemRetries int `json:"maxSystemRetries"`
	// RetryOutcomes are the outcomes that are dialled again.
	RetryOutcomes []Outcome `json:"retryOutcomes"`
	// IntervalSec is the wait after an outcome that reached the customer:
	// NO_ANSWER, BUSY, REJECTED and UNKNOWN.
	IntervalSec map[Outcome]int `json:"intervalSec"`
	// TransientBackoff paces TRANSIENT and ORIGINATE_FAILED.
	TransientBackoff Backoff `json:"transientBackoff"`
}

// Limits on a retry policy. They are sanity bounds, not product rules: a
// retry interval of a second is a harassment bug and one of a year is a
// forgotten contact.
const (
	maxMaxAttempts       = 20
	maxMaxSystemRetries  = 50
	minIntervalSec       = 60
	maxIntervalSec       = 30 * 24 * 3600
	maxBackoffFactor     = 10
	minBackoffBaseSec    = 1
	maxBackoffCapSec     = 24 * 3600
	defaultMaxAttempts   = 3
	defaultSystemRetries = 5
)

// reachedOutcomes are the retryable outcomes that spend MaxAttempts and wait
// IntervalSec; systemOutcomes spend MaxSystemRetries and use TransientBackoff.
var (
	reachedOutcomes = []Outcome{OutcomeNoAnswer, OutcomeBusy, OutcomeRejected, OutcomeUnknown}
	systemOutcomes  = []Outcome{OutcomeTransient, OutcomeOriginateFailed}
)

// DefaultRetryPolicy is the policy of design 10 section 7.2.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:      defaultMaxAttempts,
		MaxSystemRetries: defaultSystemRetries,
		RetryOutcomes: []Outcome{
			OutcomeNoAnswer, OutcomeBusy, OutcomeRejected, OutcomeUnknown,
			OutcomeTransient, OutcomeOriginateFailed,
		},
		IntervalSec: map[Outcome]int{
			OutcomeNoAnswer: 2 * 3600,
			OutcomeBusy:     30 * 60,
			OutcomeRejected: 24 * 3600,
			OutcomeUnknown:  24 * 3600,
		},
		TransientBackoff: Backoff{BaseSec: 300, Factor: 3, CapSec: 4 * 3600},
	}
}

// Normalize checks the policy and fills the intervals it leaves out with the
// defaults, so the stored policy always states every interval the dialer
// reads. RetryOutcomes keep their order and lose duplicates.
func (p RetryPolicy) Normalize() (RetryPolicy, error) {
	const field = "retryPolicy"
	if p.MaxAttempts < 1 || p.MaxAttempts > maxMaxAttempts {
		return RetryPolicy{}, invalid(field+".maxAttempts", "OUT_OF_RANGE",
			"maxAttempts is from 1 to %d, got %d", maxMaxAttempts, p.MaxAttempts)
	}
	if p.MaxSystemRetries < 0 || p.MaxSystemRetries > maxMaxSystemRetries {
		return RetryPolicy{}, invalid(field+".maxSystemRetries", "OUT_OF_RANGE",
			"maxSystemRetries is from 0 to %d, got %d", maxMaxSystemRetries, p.MaxSystemRetries)
	}

	outcomes := make([]Outcome, 0, len(p.RetryOutcomes))
	for _, o := range p.RetryOutcomes {
		if !slices.Contains(reachedOutcomes, o) && !slices.Contains(systemOutcomes, o) {
			return RetryPolicy{}, invalid(field+".retryOutcomes", "NOT_RETRYABLE",
				"%q cannot be retried; the outcomes that can are NO_ANSWER, BUSY, REJECTED, UNKNOWN, TRANSIENT and ORIGINATE_FAILED", o)
		}
		if !slices.Contains(outcomes, o) {
			outcomes = append(outcomes, o)
		}
	}

	defaults := DefaultRetryPolicy()
	intervals := make(map[Outcome]int, len(reachedOutcomes))
	for o := range p.IntervalSec {
		if !slices.Contains(reachedOutcomes, o) {
			return RetryPolicy{}, invalid(field+".intervalSec", "UNKNOWN_OUTCOME",
				"%q has no interval; intervals are for NO_ANSWER, BUSY, REJECTED and UNKNOWN (TRANSIENT and ORIGINATE_FAILED follow transientBackoff)", o)
		}
	}
	for _, o := range reachedOutcomes {
		sec, ok := p.IntervalSec[o]
		if !ok {
			sec = defaults.IntervalSec[o]
		}
		if sec < minIntervalSec || sec > maxIntervalSec {
			return RetryPolicy{}, invalid(fmt.Sprintf("%s.intervalSec.%s", field, o), "OUT_OF_RANGE",
				"the interval after %s is from %d to %d seconds, got %d", o, minIntervalSec, maxIntervalSec, sec)
		}
		intervals[o] = sec
	}

	b := p.TransientBackoff
	if b == (Backoff{}) {
		b = defaults.TransientBackoff
	}
	switch {
	case b.BaseSec < minBackoffBaseSec || b.BaseSec > maxBackoffCapSec:
		return RetryPolicy{}, invalid(field+".transientBackoff.baseSec", "OUT_OF_RANGE",
			"baseSec is from %d to %d, got %d", minBackoffBaseSec, maxBackoffCapSec, b.BaseSec)
	case b.Factor < 1 || b.Factor > maxBackoffFactor:
		return RetryPolicy{}, invalid(field+".transientBackoff.factor", "OUT_OF_RANGE",
			"factor is from 1 to %d, got %d", maxBackoffFactor, b.Factor)
	case b.CapSec < b.BaseSec || b.CapSec > maxBackoffCapSec:
		return RetryPolicy{}, invalid(field+".transientBackoff.capSec", "OUT_OF_RANGE",
			"capSec is from baseSec (%d) to %d, got %d", b.BaseSec, maxBackoffCapSec, b.CapSec)
	}

	return RetryPolicy{
		MaxAttempts:      p.MaxAttempts,
		MaxSystemRetries: p.MaxSystemRetries,
		RetryOutcomes:    outcomes,
		IntervalSec:      intervals,
		TransientBackoff: b,
	}, nil
}
