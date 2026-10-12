// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"errors"
	"testing"
)

// One row of the table of design 10 section 7.2 per case: the cause, and what
// an unanswered call that ended with it is.
func TestEveryRowOfTheCauseTable(t *testing.T) {
	rows := []struct {
		cause string
		want  Outcome
	}{
		{"NO_ANSWER", OutcomeNoAnswer},
		{"NO_USER_RESPONSE", OutcomeNoAnswer},
		{"ALLOTTED_TIMEOUT", OutcomeNoAnswer},
		{"USER_BUSY", OutcomeBusy},
		{"CALL_REJECTED", OutcomeRejected},
		{"CHANNEL_VANISHED", OutcomeUnknown},
		{"NORMAL_TEMPORARY_FAILURE", OutcomeTransient},
		{"NETWORK_OUT_OF_ORDER", OutcomeTransient},
		{"RECOVERY_ON_TIMER_EXPIRE", OutcomeTransient},
		{"SWITCH_CONGESTION", OutcomeTransient},
		{"NORMAL_CIRCUIT_CONGESTION", OutcomeTransient},
		{"UNALLOCATED_NUMBER", OutcomeInvalidNumber},
		{"INVALID_NUMBER_FORMAT", OutcomeInvalidNumber},
		{"NUMBER_CHANGED", OutcomeInvalidNumber},
	}
	if len(rows) != len(CauseTable()) {
		t.Errorf("the table has %d rows and the test %d: every row needs a case", len(CauseTable()), len(rows))
	}
	for _, r := range rows {
		t.Run(r.cause, func(t *testing.T) {
			if got := Classify(CallEnd{HangupCause: r.cause}); got != r.want {
				t.Errorf("Classify(%s) = %s, want %s", r.cause, got, r.want)
			}
		})
	}
}

func TestAnUnmappedCauseIsTransient(t *testing.T) {
	for _, cause := range []string{"SOME_NEW_CAUSE", "", "NORMAL_CLEARING"} {
		if got := Classify(CallEnd{HangupCause: cause}); got != OutcomeTransient {
			t.Errorf("an unanswered call ending %q = %s, want TRANSIENT", cause, got)
		}
	}
}

// The answered outcomes: the customer picked up, so the call is one of the
// three that are never dialled again, and the cause decides which.
func TestAnAnsweredCall(t *testing.T) {
	tests := []struct {
		name string
		end  CallEnd
		want Outcome
	}{
		{"talked and hung up", CallEnd{HangupCause: "NORMAL_CLEARING", IsAnswered: true, HasBot: true}, OutcomeAnswered},
		{"hung up within a second", CallEnd{HangupCause: "NORMAL_CLEARING", IsAnswered: true}, OutcomeAnswered},
		{"hung up before the bot", CallEnd{HangupCause: "ORIGINATOR_CANCEL", IsAnswered: true}, OutcomeAnswered},
		{"ended at the session limit", CallEnd{HangupCause: "SESSION_LIMIT", IsAnswered: true, HasBot: true}, OutcomeAnswered},
		{"provider failed", CallEnd{HangupCause: "MEDIA_OR_PROVIDER_FAILURE", IsAnswered: true, IsFailed: true, HasBot: true}, OutcomeBotFailed},
		{"provider session expired", CallEnd{HangupCause: "PROVIDER_SESSION_EXPIRED", IsAnswered: true, IsFailed: true, HasBot: true}, OutcomeBotFailed},
		{"reaped by recovery", CallEnd{HangupCause: "RECOVERY_ON_TIMER_EXPIRE", IsAnswered: true}, OutcomeBotFailed},
		{"answered after the window closed", CallEnd{HangupCause: "ALLOTTED_TIMEOUT", IsAnswered: true}, OutcomeBotFailed},
		{"vanished after a bot talked", CallEnd{HangupCause: "CHANNEL_VANISHED", IsAnswered: true, HasBot: true}, OutcomeAnswered},
		{"vanished with no bot", CallEnd{HangupCause: "CHANNEL_VANISHED", IsAnswered: true}, OutcomeBotFailed},
		{"no bot slot", CallEnd{HangupCause: "OVERFLOW", IsAnswered: true}, OutcomeOverflow},
		{"overflow even if the ledger lost the answer", CallEnd{HangupCause: "OVERFLOW"}, OutcomeOverflow},
		// A cause that would be a retry for a ring is nothing of the kind once
		// somebody has answered.
		{"busy cause after an answer", CallEnd{HangupCause: "USER_BUSY", IsAnswered: true}, OutcomeAnswered},
		{"temporary failure after an answer", CallEnd{HangupCause: "NORMAL_TEMPORARY_FAILURE", IsAnswered: true}, OutcomeAnswered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.end)
			if got != tt.want {
				t.Fatalf("Classify = %s, want %s", got, tt.want)
			}
			if !IsAnsweredOutcome(got) {
				t.Errorf("%s is not an answered outcome", got)
			}
		})
	}
}

// A failure before any channel is ORIGINATE_FAILED with its cause, unless the
// cause is the customer's own answer, which keeps its outcome.
func TestAPlacementFailure(t *testing.T) {
	tests := []struct {
		cause string
		want  Outcome
	}{
		{"GATEWAY_DOWN", OutcomeOriginateFailed},
		{"NO_ROUTE_DESTINATION", OutcomeOriginateFailed},
		{"DESTINATION_OUT_OF_ORDER", OutcomeOriginateFailed},
		{"INVALID_GATEWAY", OutcomeOriginateFailed},
		{"SOMETHING_NEVER_SEEN", OutcomeOriginateFailed},
		{"", OutcomeOriginateFailed},
		// Mapped as a system failure for a call that existed, but with no
		// channel it is the same failure before one.
		{"NORMAL_TEMPORARY_FAILURE", OutcomeOriginateFailed},
		{"SWITCH_CONGESTION", OutcomeOriginateFailed},
		{"RECOVERY_ON_TIMER_EXPIRE", OutcomeOriginateFailed},
		// The callee's own refusals arrive as -ERR too.
		{"USER_BUSY", OutcomeBusy},
		{"NO_ANSWER", OutcomeNoAnswer},
		{"NO_USER_RESPONSE", OutcomeNoAnswer},
		{"CALL_REJECTED", OutcomeRejected},
		{"UNALLOCATED_NUMBER", OutcomeInvalidNumber},
		{"INVALID_NUMBER_FORMAT", OutcomeInvalidNumber},
		{"NUMBER_CHANGED", OutcomeInvalidNumber},
	}
	for _, tt := range tests {
		t.Run(tt.cause, func(t *testing.T) {
			if got := Classify(CallEnd{HangupCause: tt.cause, IsPlacementFailure: true}); got != tt.want {
				t.Errorf("placement failure %q = %s, want %s", tt.cause, got, tt.want)
			}
		})
	}
}

func TestWhichBudgetEachOutcomePays(t *testing.T) {
	want := map[Outcome]Class{
		OutcomeAnswered: ClassReached, OutcomeBotFailed: ClassReached, OutcomeOverflow: ClassReached,
		OutcomeNoAnswer: ClassReached, OutcomeBusy: ClassReached, OutcomeRejected: ClassReached,
		OutcomeUnknown:   ClassReached,
		OutcomeTransient: ClassSystem, OutcomeOriginateFailed: ClassSystem,
		OutcomeInvalidNumber: ClassNone, OutcomeDNCSkipped: ClassNone,
	}
	for o, class := range want {
		if got := ClassOf(o); got != class {
			t.Errorf("ClassOf(%s) = %d, want %d", o, got, class)
		}
		if got := IsTouched(o); got != (class == ClassReached) {
			t.Errorf("IsTouched(%s) = %v", o, got)
		}
		if !o.IsValid() {
			t.Errorf("%s is not valid", o)
		}
	}
}

func TestTheSyncDialErrors(t *testing.T) {
	dnc, bad, limit := errors.New("dnc"), errors.New("bad"), errors.New("limit")
	kinds := DialErrorKinds{NumberOnDNC: dnc, BadNumber: bad, LineLimit: limit}
	tests := []struct {
		name string
		err  error
		want DialVerdict
	}{
		{"on the do-not-call list", dnc, DialVerdict{Outcome: OutcomeDNCSkipped}},
		{"wrapped", errors.Join(errors.New("dial"), dnc), DialVerdict{Outcome: OutcomeDNCSkipped}},
		{"a bad number", bad, DialVerdict{Outcome: OutcomeInvalidNumber}},
		{"the line limit", limit, DialVerdict{IsVoid: true}},
		{"anything else", errors.New("bgapi send failed"), DialVerdict{Outcome: OutcomeOriginateFailed}},
		{"limiter cancelled", errCanceled, DialVerdict{Outcome: OutcomeOriginateFailed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyDialError(tt.err, kinds); got != tt.want {
				t.Errorf("verdict = %+v, want %+v", got, tt.want)
			}
		})
	}
	// A kind the caller does not have never matches, not even a nil error.
	if got := ClassifyDialError(errors.New("x"), DialErrorKinds{}); got.Outcome != OutcomeOriginateFailed {
		t.Errorf("with no kinds = %+v", got)
	}
}

var errCanceled = errors.New("context canceled")
