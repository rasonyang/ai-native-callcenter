// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"log/slog"
	"slices"

	"github.com/google/uuid"

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
	// (outbound.ReconcileCause). It is also the switch's cause when the far
	// end ends an unanswered call with a SIP 408, which Classify tells apart
	// by the status (CallEnd.SIPStatus) and the ring progress.
	CauseRecovery = "RECOVERY_ON_TIMER_EXPIRE"
	// CauseVanished: the switch no longer had the channel and nothing said
	// how it ended (telephony.VanishedCause).
	CauseVanished = "CHANNEL_VANISHED"
	// CauseBotBridgeUnconfirmed: the customer answered and the switch never
	// confirmed or ran the bridge to the bot, so the leg was hung up
	// (outbound.BridgeUnconfirmedCause).
	CauseBotBridgeUnconfirmed = "BOT_BRIDGE_UNCONFIRMED"
	// CauseBotStartFailed: the bot leg answered and its session never started,
	// and the caller was not rescued, so the platform names the failure as the
	// call's hangupCause (telephony.BotStartFailed). A campaign call whose ACK
	// never reached the bot ends here with no BOT_LEG_FAILED stamp, because no
	// rescue ran.
	CauseBotStartFailed = "BOT_START_FAILED"
)

// CauseOriginatorCancel is the switch's cause for a dialled leg whose INVITE
// the far end ended with a final 487 Request Terminated of its own. The
// switch uses the word for other endings too, so the cause alone does not say
// whose ending it was. A leg this platform cancelled at its ring timeout is
// NO_ANSWER and leaves no SIP status, so with a 487 the cause is the far
// end's. Classify reads it with the status and the ring progress.
const CauseOriginatorCancel = "ORIGINATOR_CANCEL"

// BotReasonLegFailed is the CDR's userData.botReason of a call whose bot leg
// never carried a conversation: refused, unreachable, or failed before media
// (telephony.BotLegFailed). The switch's cause stays whatever it was.
const BotReasonLegFailed = "BOT_LEG_FAILED"

// CallEnd is what a finished call tells about how it went: the facts of the
// CDR (or of a job result for a call that never had one), reduced to what the
// outcome depends on.
type CallEnd struct {
	// CallID is the call the facts are about, for the log.
	CallID uuid.UUID
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
	// BotReason is the CDR's userData.botReason. BOT_LEG_FAILED on an answered
	// call means the bot side failed, whatever the hangup cause says.
	BotReason string
	// IsPlacementFailure: the originate never made a channel
	// (tech.isPlacementFailure); the cause is the job result's.
	IsPlacementFailure bool
	// SIPStatus is the final SIP status the far end ended the dialled leg
	// with (tech.sipStatus); 0 when the switch reported none, which is what a
	// leg this platform cancelled or reaped itself leaves.
	SIPStatus int
	// HasRingProgress: the dialled leg sent a 180 or a 183 before it ended
	// (tech.ringStartedAt).
	HasRingProgress bool
	// EndedBy is which side ended the dialled leg (tech.endedBy, PEER or
	// PLATFORM); empty when the switch did not say. It is reported, never
	// decided on: the cause and the status already tell the cases apart, on
	// records written before the switch's word was kept too.
	EndedBy string
}

// SIPStatusRequestTimeout is the SIP status a carrier sends when it gives up
// on an unanswered call itself.
const SIPStatusRequestTimeout = 408

// SIPStatusRequestTerminated is the SIP status of an INVITE that ended before
// an answer. Sent by the far end of its own accord, after ringing, it is the
// far end ending an unanswered call; what made it do so (its own ring timer,
// a callee who declined) cannot be told from this side.
const SIPStatusRequestTerminated = 487

// CauseCallRejected is the switch's cause for a dialled leg the far end
// refused. mod_sofia folds six final responses into it
// (sofia_glue_sip_cause_to_freeswitch, FreeSWITCH v1.11.3): 401, 402, 403,
// 407, 603 and 608. Only the 603 is the callee's; Classify reads the cause
// with the status.
const CauseCallRejected = "CALL_REJECTED"

// SIPStatusDecline is the SIP status of a callee who declined the call
// (603 Decline): under CALL_REJECTED, the one status that is the customer.
const SIPStatusDecline = 603

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

	CauseCallRejected: OutcomeRejected,

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
//     failed (the ledger calls the call FAILED, or recovery, a late answer or
//     a bridge to the bot that never ran ended the leg, the bot answered and
//     its session never started (BOT_START_FAILED), or the call carries
//     botReason BOT_LEG_FAILED: the bot refused the leg or failed before
//     media, with the switch's cause left as it was), ANSWERED otherwise.
//     A call lost to CHANNEL_VANISHED is ANSWERED when a bot had been talking
//     and BOT_FAILED when none had.
//   - A hang-up in the first second, a voicemail and a call that recorded no
//     lead are ANSWERED: the customer's own ending is not the bot's failure,
//     so it cannot trip the provider breaker.
//   - A placement failure keeps a customer-side cause's outcome and is
//     ORIGINATE_FAILED for anything else, which is never an unmapped cause:
//     every cause but the customer's is the same failure before a channel.
//   - RECOVERY_ON_TIMER_EXPIRE on a call nobody answered is NO_ANSWER when the
//     far end sent a SIP 408 after the leg rang (a carrier ending the call
//     itself), and TRANSIENT otherwise: no ring progress, or no 408 (our own
//     recovery's reap).
//   - ORIGINATOR_CANCEL on a call nobody answered is NO_ANSWER when the far
//     end ended the INVITE with a SIP 487 after the leg rang. Otherwise it is
//     not a row: without ring progress or without the 487 it is the unknown
//     cause below.
//   - CALL_REJECTED on a call nobody answered is LINE_REJECTED when the far
//     end sent any final status but a 603 (a 402, a 403, a 401 or 407, a 608):
//     the line refused the call before it reached a phone, whether or not the
//     leg rang, and it is counted by status in a metric. A 603, or no status
//     at all, is the table's REJECTED: the callee declined, or nothing says
//     otherwise.
//   - Otherwise the table decides, and a cause it lacks is TRANSIENT with a
//     warning and a metric, bounded by maxSystemRetries.
func Classify(e CallEnd) Outcome {
	if e.HangupCause == CauseOverflow {
		return OutcomeOverflow
	}
	if e.IsAnswered {
		if e.BotReason == BotReasonLegFailed {
			return OutcomeBotFailed
		}
		switch e.HangupCause {
		case CauseRecovery, CauseLateAnswer, CauseBotBridgeUnconfirmed, CauseBotStartFailed:
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
	// A carrier that ends an unanswered call itself sends 408, which the
	// switch reports as RECOVERY_ON_TIMER_EXPIRE. After ringing it is the
	// customer not answering. Ring progress alone does not say so: our own
	// recovery's reap uses the same cause, leaves no 408, and can hit a leg
	// that is ringing. A 408 with no ring progress stays a system failure.
	if e.HangupCause == CauseRecovery && e.SIPStatus == SIPStatusRequestTimeout &&
		e.HasRingProgress && !e.IsPlacementFailure {
		return OutcomeNoAnswer
	}
	// A far end that ends a ringing INVITE with a 487 of its own (no CANCEL
	// is sent by either side) is reported as ORIGINATOR_CANCEL. When the far
	// end's ring timer is shorter than ours, our own timeout (NO_ANSWER, no
	// status) never fires. After ringing it is the customer not answering,
	// exactly as the 408 above. Without ring progress nothing shows the phone
	// was reached.
	if e.HangupCause == CauseOriginatorCancel && e.SIPStatus == SIPStatusRequestTerminated &&
		e.HasRingProgress && !e.IsPlacementFailure {
		return OutcomeNoAnswer
	}
	// The switch reports a carrier's own refusal (402 for a line out of
	// credit, 401/407 for its authentication, 403, 608) with the same cause as
	// a callee's 603 Decline. Only the 603 is a person saying no; every other
	// status is the line, which is why a run of them is a trunk outage. Ring
	// progress does not tell them apart and is not read. No status keeps the
	// table's REJECTED: nothing says the line refused.
	if e.HangupCause == CauseCallRejected && e.SIPStatus != 0 && e.SIPStatus != SIPStatusDecline &&
		!e.IsPlacementFailure {
		obs.RecordCampaignLineRejected(e.SIPStatus)
		return OutcomeLineRejected
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
		"callId", e.CallID, "hangupCause", e.HangupCause, "sipStatus", e.SIPStatus,
		"hasRingProgress", e.HasRingProgress, "endedBy", e.EndedBy)
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
	case OutcomeTransient, OutcomeLineRejected, OutcomeOriginateFailed:
		return ClassSystem
	}
	return ClassNone
}

// IsTouched reports whether the outcome reached the customer: the value of
// campaign_attempts.is_touched, which the cross-campaign frequency check reads.
func IsTouched(o Outcome) bool { return ClassOf(o) == ClassReached }

// CountsTowardsDailyCap reports whether a finished attempt with this outcome
// is a dial of its number for the daily dial cap (design 10 section 5.2). Every
// outcome that spends a budget counts, the system failures included: the
// dialer cannot tell whether the carrier passed the INVITE on to the phone, and
// counting is the safe side. A line's refusal (LINE_REJECTED) is the one
// exception, because the line answered the INVITE itself and the call certainly
// never reached a phone. A skip and a bad number spend nothing and never count.
// An attempt still in flight always counts; that is the store's to say.
func CountsTowardsDailyCap(o Outcome) bool {
	return ClassOf(o) != ClassNone && o != OutcomeLineRejected
}

// IsAnsweredOutcome reports whether the customer picked up. Such a number is
// never dialled again.
func IsAnsweredOutcome(o Outcome) bool {
	return o == OutcomeAnswered || o == OutcomeBotFailed || o == OutcomeOverflow
}

// AllOutcomes is every outcome, in the order of the type's declaration. It is
// the one list IsValid reads, so a test that walks it covers a new outcome.
func AllOutcomes() []Outcome {
	return []Outcome{
		OutcomeAnswered, OutcomeBotFailed, OutcomeOverflow, OutcomeNoAnswer, OutcomeBusy,
		OutcomeRejected, OutcomeUnknown, OutcomeTransient, OutcomeLineRejected, OutcomeOriginateFailed,
		OutcomeInvalidNumber, OutcomeDNCSkipped,
	}
}

// IsValid reports whether o is one of the twelve outcomes.
func (o Outcome) IsValid() bool { return slices.Contains(AllOutcomes(), o) }
