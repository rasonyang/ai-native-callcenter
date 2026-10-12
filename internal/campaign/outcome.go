// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"log/slog"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// The hangup causes this platform writes itself, as opposed to the switch's
// Q.850 names. They are strings here, not imports of the packages that stamp
// them, because this package knows nothing of telephony; the tests that
// matter pin the words (outcome_test.go).
const (
	// CauseOverflow: an answered call that no bot slot could take
	// (outbound.OverflowCause).
	CauseOverflow = "OVERFLOW"
	// CauseLateAnswer: the customer answered after the answer window closed
	// and the leg was hung up (outbound.LateAnswerCause). FreeSWITCH also
	// uses the word for a ring that outlasted its timeout.
	CauseLateAnswer = "ALLOTTED_TIMEOUT"
	// CauseRecovery: recovery ended a leg the application had lost track of
	// (outbound.ReconcileCause). It is also a genuine switch cause.
	CauseRecovery = "RECOVERY_ON_TIMER_EXPIRE"
	// CauseVanished: the switch no longer had the channel and nothing said
	// how it ended (telephony.VanishedCause).
	CauseVanished = "CHANNEL_VANISHED"
)

// CallEnd is what a finished call tells about how it went: the facts of the
// CDR (or of a job result for a call that never had one), reduced to what the
// outcome depends on.
type CallEnd struct {
	// HangupCause is the CDR's hangupCause, or the cause of a failed
	// originate job.
	HangupCause string
	// IsAnswered: the customer picked up. Taken from the CDR's answeredAt, or
	// from a CDR status of ANSWERED.
	IsAnswered bool
	// IsFailed: the ledger calls the call FAILED. For an answered call that is
	// the bot path saying the conversation could not continue.
	IsFailed bool
	// HasBot: a bot leg or session existed (botSec above zero, or a BOT leg).
	HasBot bool
	// IsPlacementFailure: the originate never made a channel
	// (tech.isPlacementFailure); the cause is the job result's.
	IsPlacementFailure bool
}

// causeOutcomes is the cause to outcome table of design 10 section 7.2, for a
// call the customer did not answer. It is data: one row per cause, one test
// per row.
//
// Our own ring timeout is not a row: it is originate_timeout, and the switch
// reports it as NO_ANSWER. ALLOTTED_TIMEOUT is the switch's word for a leg
// that outlasted its call timeout, and also ours for a late answer; without an
// answer it is a ring that came to nothing.
var causeOutcomes = map[string]Outcome{
	"NO_ANSWER":        OutcomeNoAnswer,
	"NO_USER_RESPONSE": OutcomeNoAnswer,
	CauseLateAnswer:    OutcomeNoAnswer,

	"USER_BUSY": OutcomeBusy,

	"CALL_REJECTED": OutcomeRejected,

	CauseVanished: OutcomeUnknown,

	"NORMAL_TEMPORARY_FAILURE":  OutcomeTransient,
	"NETWORK_OUT_OF_ORDER":      OutcomeTransient,
	CauseRecovery:               OutcomeTransient,
	"SWITCH_CONGESTION":         OutcomeTransient,
	"NORMAL_CIRCUIT_CONGESTION": OutcomeTransient,

	"UNALLOCATED_NUMBER":    OutcomeInvalidNumber,
	"INVALID_NUMBER_FORMAT": OutcomeInvalidNumber,
	"NUMBER_CHANGED":        OutcomeInvalidNumber,
}

// customerSide are the outcomes whose cause says something about the
// customer's line, not about our network. A placement failure carrying one of
// them is a customer answer that arrived as an -ERR before a channel was
// reported (originate returns the callee's refusal that way), so it keeps its
// customer outcome instead of becoming ORIGINATE_FAILED.
var customerSide = map[Outcome]bool{
	OutcomeNoAnswer: true, OutcomeBusy: true, OutcomeRejected: true, OutcomeInvalidNumber: true,
}

// CauseTable returns a copy of the table, for tests and documentation.
func CauseTable() map[string]Outcome {
	out := make(map[string]Outcome, len(causeOutcomes))
	for k, v := range causeOutcomes {
		out[k] = v
	}
	return out
}

// Classify says how an attempt ended.
//
//   - OVERFLOW is its own cause and means the customer answered.
//   - An answered customer is never anything but an answered outcome, because
//     an answered number is never dialled again (owner's decision, design 10
//     section 7.2). The cause only chooses which: BOT_FAILED when the bot side
//     failed (the ledger calls the call FAILED, or recovery or a late answer
//     ended the leg), ANSWERED otherwise. A call lost to CHANNEL_VANISHED is
//     ANSWERED when a bot had been talking and BOT_FAILED when none had.
//   - A hang-up in the first second, a voicemail and a call that recorded no
//     lead are ANSWERED: the customer's own ending is not the bot's failure,
//     so it cannot trip the provider breaker.
//   - A placement failure keeps a customer-side cause's outcome and is
//     ORIGINATE_FAILED for anything else, which is never an unmapped cause:
//     every cause but the customer's is the same failure before a channel.
//   - Otherwise the table decides, and a cause it lacks is TRANSIENT with a
//     warning and a metric, bounded by maxSystemRetries.
func Classify(e CallEnd) Outcome {
	if e.HangupCause == CauseOverflow {
		return OutcomeOverflow
	}
	if e.IsAnswered {
		switch e.HangupCause {
		case CauseRecovery, CauseLateAnswer:
			return OutcomeBotFailed
		case CauseVanished:
			if e.HasBot {
				return OutcomeAnswered
			}
			return OutcomeBotFailed
		}
		if e.IsFailed {
			return OutcomeBotFailed
		}
		return OutcomeAnswered
	}
	mapped, isMapped := causeOutcomes[e.HangupCause]
	if e.IsPlacementFailure {
		if isMapped && customerSide[mapped] {
			return mapped
		}
		return OutcomeOriginateFailed
	}
	if isMapped {
		return mapped
	}
	slog.Warn("campaign attempt ended with a hangup cause the outcome table does not know; treating it as TRANSIENT",
		"hangupCause", e.HangupCause)
	obs.RecordCampaignUnmappedCause(e.HangupCause)
	return OutcomeTransient
}

// Class is how an outcome is paid for.
type Class int

const (
	// ClassNone: the outcome is not counted (a bad number, a DNC skip).
	ClassNone Class = iota
	// ClassReached: the customer was reached; it counts against maxAttempts.
	ClassReached
	// ClassSystem: our or the network's failure; it counts against
	// maxSystemRetries.
	ClassSystem
)

// ClassOf is the budget an outcome spends.
func ClassOf(o Outcome) Class {
	switch o {
	case OutcomeAnswered, OutcomeBotFailed, OutcomeOverflow,
		OutcomeNoAnswer, OutcomeBusy, OutcomeRejected, OutcomeUnknown:
		return ClassReached
	case OutcomeTransient, OutcomeOriginateFailed:
		return ClassSystem
	}
	return ClassNone
}

// IsTouched reports whether the outcome reached the customer: the value of
// campaign_attempts.is_touched, which the cross-campaign frequency check reads.
func IsTouched(o Outcome) bool { return ClassOf(o) == ClassReached }

// IsAnsweredOutcome reports whether the customer picked up. Such a number is
// never dialled again.
func IsAnsweredOutcome(o Outcome) bool {
	return o == OutcomeAnswered || o == OutcomeBotFailed || o == OutcomeOverflow
}

// IsValid reports whether o is one of the eleven outcomes.
func (o Outcome) IsValid() bool {
	switch o {
	case OutcomeAnswered, OutcomeBotFailed, OutcomeOverflow, OutcomeNoAnswer, OutcomeBusy,
		OutcomeRejected, OutcomeUnknown, OutcomeTransient, OutcomeOriginateFailed,
		OutcomeInvalidNumber, OutcomeDNCSkipped:
		return true
	}
	return false
}
