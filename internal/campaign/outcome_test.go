// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// RECOVERY_ON_TIMER_EXPIRE is the switch's word for a far end that sent 408,
// and our own recovery's word for a leg it reaped. Only the 408 after ringing
// is the customer not answering (#576); the reap leaves no SIP status and can
// hit a ringing leg, so ring progress alone is not enough.
func TestARecoveryCauseIsNoAnswerOnlyForA408AfterRinging(t *testing.T) {
	tests := []struct {
		name string
		end  CallEnd
		want Outcome
	}{
		{"408 after ring progress", CallEnd{HangupCause: CauseRecovery, SIPStatus: 408, HasRingProgress: true}, OutcomeNoAnswer},
		{"408 without ring progress", CallEnd{HangupCause: CauseRecovery, SIPStatus: 408}, OutcomeTransient},
		{"ring progress and no SIP status (our own reap)", CallEnd{HangupCause: CauseRecovery, HasRingProgress: true}, OutcomeTransient},
		{"neither", CallEnd{HangupCause: CauseRecovery}, OutcomeTransient},
		{"another status after ring progress", CallEnd{HangupCause: CauseRecovery, SIPStatus: 503, HasRingProgress: true}, OutcomeTransient},
		{"answered, whatever the status", CallEnd{HangupCause: CauseRecovery, IsAnswered: true, SIPStatus: 408, HasRingProgress: true}, OutcomeBotFailed},
		{"answered without a status", CallEnd{HangupCause: CauseRecovery, IsAnswered: true}, OutcomeBotFailed},
		{"a placement failure keeps its handling", CallEnd{HangupCause: CauseRecovery, SIPStatus: 408, HasRingProgress: true, IsPlacementFailure: true}, OutcomeOriginateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.end); got != tt.want {
				t.Errorf("Classify = %s, want %s", got, tt.want)
			}
		})
	}
}

// ORIGINATOR_CANCEL with a 487 is a far end that ended the ringing INVITE by
// itself (our own ring timeout is NO_ANSWER and leaves no status). After ring
// progress it is the customer not answering, as the 408 above; anything else
// with this cause is not a row of the table, so it is the unknown cause.
func TestAFarEnds487AfterRingingIsNoAnswer(t *testing.T) {
	tests := []struct {
		name string
		end  CallEnd
		want Outcome
	}{
		{"487 after ring progress", CallEnd{HangupCause: CauseOriginatorCancel, SIPStatus: 487, HasRingProgress: true, EndedBy: "PEER"}, OutcomeNoAnswer},
		{"the same with no word on who ended it", CallEnd{HangupCause: CauseOriginatorCancel, SIPStatus: 487, HasRingProgress: true}, OutcomeNoAnswer},
		{"487 without ring progress", CallEnd{HangupCause: CauseOriginatorCancel, SIPStatus: 487}, OutcomeTransient},
		{"ring progress and no SIP status", CallEnd{HangupCause: CauseOriginatorCancel, HasRingProgress: true}, OutcomeTransient},
		{"ring progress, no status, ended by us", CallEnd{HangupCause: CauseOriginatorCancel, HasRingProgress: true, EndedBy: "PLATFORM"}, OutcomeTransient},
		{"another status after ring progress", CallEnd{HangupCause: CauseOriginatorCancel, SIPStatus: 408, HasRingProgress: true}, OutcomeTransient},
		{"neither", CallEnd{HangupCause: CauseOriginatorCancel}, OutcomeTransient},
		{"a 487 with another cause is that cause's", CallEnd{HangupCause: "NORMAL_TEMPORARY_FAILURE", SIPStatus: 487, HasRingProgress: true}, OutcomeTransient},
		{"answered, whatever the status", CallEnd{HangupCause: CauseOriginatorCancel, IsAnswered: true, SIPStatus: 487, HasRingProgress: true}, OutcomeAnswered},
		{"answered with a bot that failed", CallEnd{HangupCause: CauseOriginatorCancel, IsAnswered: true, IsFailed: true, SIPStatus: 487, HasRingProgress: true}, OutcomeBotFailed},
		{"a placement failure keeps its handling", CallEnd{HangupCause: CauseOriginatorCancel, SIPStatus: 487, HasRingProgress: true, IsPlacementFailure: true}, OutcomeOriginateFailed},
		// #576's rows are untouched.
		{"#576: 408 after ring progress", CallEnd{HangupCause: CauseRecovery, SIPStatus: 408, HasRingProgress: true}, OutcomeNoAnswer},
		{"#576: 487 with the recovery cause", CallEnd{HangupCause: CauseRecovery, SIPStatus: 487, HasRingProgress: true}, OutcomeTransient},
		{"our own ring timeout", CallEnd{HangupCause: "NO_ANSWER", HasRingProgress: true, EndedBy: "PLATFORM"}, OutcomeNoAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.end); got != tt.want {
				t.Errorf("Classify = %s, want %s", got, tt.want)
			}
		})
	}
}

// CALL_REJECTED is the switch's word for a callee's 603 Decline and for the
// line's own refusals (402, 401/407, 403, 608). Only the 603 is the customer;
// every other status is the line, whether or not the leg rang. No status
// leaves nothing to tell, so it keeps the table's REJECTED.
func TestACallRejectedIsTheLineUnlessItIsADeclineOrHasNoStatus(t *testing.T) {
	tests := []struct {
		name string
		end  CallEnd
		want Outcome
	}{
		{"402 before any ring progress", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 402, EndedBy: "PEER"}, OutcomeLineRejected},
		{"402 after ring progress", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 402, HasRingProgress: true, EndedBy: "PEER"}, OutcomeLineRejected},
		{"403 before any ring progress", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 403}, OutcomeLineRejected},
		{"403 after ring progress", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 403, HasRingProgress: true}, OutcomeLineRejected},
		{"401", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 401}, OutcomeLineRejected},
		{"407", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 407}, OutcomeLineRejected},
		{"608", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 608, HasRingProgress: true}, OutcomeLineRejected},
		{"603 before any ring progress", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 603, EndedBy: "PEER"}, OutcomeRejected},
		{"603 after ring progress", CallEnd{HangupCause: CauseCallRejected, SIPStatus: 603, HasRingProgress: true, EndedBy: "PEER"}, OutcomeRejected},
		{"no status, before any ring progress", CallEnd{HangupCause: CauseCallRejected}, OutcomeRejected},
		{"no status, after ring progress", CallEnd{HangupCause: CauseCallRejected, HasRingProgress: true}, OutcomeRejected},
		// A placement failure carries no status; with the cause alone it is
		// the callee's refusal, as before.
		{"a placement failure", CallEnd{HangupCause: CauseCallRejected, IsPlacementFailure: true}, OutcomeRejected},
		{"answered, whatever the status", CallEnd{HangupCause: CauseCallRejected, IsAnswered: true, SIPStatus: 402}, OutcomeAnswered},
		// The status says nothing under another cause.
		{"a 402 under another cause is that cause's", CallEnd{HangupCause: "USER_BUSY", SIPStatus: 402}, OutcomeBusy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.end); got != tt.want {
				t.Errorf("Classify = %s, want %s", got, tt.want)
			}
		})
	}
}

// A line refusal spends what a TRANSIENT spends: the system-retry budget on
// the transient backoff, never maxAttempts, and it is not a touch of the
// number. A 603 stays the customer's outcome on its 24 h interval.
func TestALineRefusalIsPaidForAsASystemFailure(t *testing.T) {
	line := Classify(CallEnd{HangupCause: CauseCallRejected, SIPStatus: 402})
	if ClassOf(line) != ClassSystem || IsTouched(line) {
		t.Errorf("%s: class %d, touched %v; want a system failure that touches nothing", line, ClassOf(line), IsTouched(line))
	}
	declined := Classify(CallEnd{HangupCause: CauseCallRejected, SIPStatus: 603, HasRingProgress: true})
	if ClassOf(declined) != ClassReached || !IsTouched(declined) {
		t.Errorf("%s: class %d, touched %v; want the customer reached", declined, ClassOf(declined), IsTouched(declined))
	}
	p := DefaultRetryPolicy()
	if !slices.Contains(p.RetryOutcomes, OutcomeLineRejected) {
		t.Errorf("the default policy does not retry %s: %v", OutcomeLineRejected, p.RetryOutcomes)
	}
	if _, ok := p.IntervalSec[OutcomeLineRejected]; ok {
		t.Errorf("%s has an interval; it follows the transient backoff", OutcomeLineRejected)
	}
	end := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	got := Settle(p, Counters{}.Add(line), line, "", end, Schedule{})
	if got.Status != ContactActive || got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(end.Add(p.TransientBackoff.Wait(1))) {
		t.Errorf("after one refusal: %+v, want ACTIVE at the end + the first backoff (%s)", got, p.TransientBackoff.Wait(1))
	}
	if c := (Counters{}).Add(line); c.Attempts != 0 || c.SystemFailures != 1 {
		t.Errorf("a refusal spent %+v, want one system failure and no attempt", c)
	}
	spent := Counters{Attempts: 0, SystemFailures: p.MaxSystemRetries}
	if got := Settle(p, spent, line, "", end, Schedule{}); got.Status != ContactExhausted || got.Reason != SettleBudgetSpent {
		t.Errorf("at maxSystemRetries: %+v, want EXHAUSTED by the budget", got)
	}
	got = Settle(p, Counters{}.Add(declined), declined, "", end, Schedule{})
	if got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(end.Add(24*time.Hour)) {
		t.Errorf("after a 603: %+v, want ACTIVE at the end + 24 h", got)
	}
}

// The global meter provider takes its delegate once, so every run of the test
// below reads the same reader and compares counts before and after.
var (
	metricReader    = sdkmetric.NewManualReader()
	setMetricReader sync.Once
)

// lineRejectedCounts reads aicc_campaign_line_rejected_total by sip_status.
func lineRejectedCounts(t *testing.T) map[string]int64 {
	t.Helper()
	setMetricReader.Do(func() {
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader)))
	})
	var rm metricdata.ResourceMetrics
	if err := metricReader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "aicc_campaign_line_rejected_total" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value("sip_status")
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// The daily dial cap counts every outcome that spends a budget but a line's
// refusal, which certainly never reached a phone; a TRANSIENT still counts.
func TestWhichOutcomesCountTowardsTheDailyCap(t *testing.T) {
	for _, o := range AllOutcomes() {
		want := ClassOf(o) != ClassNone && o != OutcomeLineRejected
		if got := CountsTowardsDailyCap(o); got != want {
			t.Errorf("CountsTowardsDailyCap(%s) = %v, want %v", o, got, want)
		}
	}
	for o, want := range map[Outcome]bool{
		OutcomeLineRejected: false, OutcomeTransient: true, OutcomeOriginateFailed: true,
		OutcomeRejected: true, OutcomeDNCSkipped: false, OutcomeInvalidNumber: false,
	} {
		if got := CountsTowardsDailyCap(o); got != want {
			t.Errorf("CountsTowardsDailyCap(%s) = %v, want %v", o, got, want)
		}
	}
}

// Every line refusal is counted by its status as it is classified, so a burst
// shows while it happens; a decline and a refusal with no status are not.
func TestALineRefusalIsCountedByStatus(t *testing.T) {
	before := lineRejectedCounts(t)
	for _, e := range []CallEnd{
		{HangupCause: CauseCallRejected, SIPStatus: 402},
		{HangupCause: CauseCallRejected, SIPStatus: 402, HasRingProgress: true},
		{HangupCause: CauseCallRejected, SIPStatus: 403},
		{HangupCause: CauseCallRejected, SIPStatus: 603},
		{HangupCause: CauseCallRejected},
	} {
		Classify(e)
	}
	after := lineRejectedCounts(t)
	got := map[string]int64{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			got[k] = d
		}
	}
	if want := map[string]int64{"402": 2, "403": 1}; !maps.Equal(got, want) {
		t.Errorf("aicc_campaign_line_rejected_total gained %v, want %v", got, want)
	}
}

// The warning of an unknown cause carries what is needed to find the call and
// tell who ended it: the call id, the far end's status, whether the leg rang
// and which side ended it.
func TestTheUnknownCauseWarningNamesTheCallAndWhatTheSwitchSaid(t *testing.T) {
	var buf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	callID := uuid.Must(uuid.NewV7())
	if got := Classify(CallEnd{CallID: callID, HangupCause: CauseOriginatorCancel, SIPStatus: 487, EndedBy: "PEER"}); got != OutcomeTransient {
		t.Fatalf("Classify = %s, want TRANSIENT", got)
	}
	var found map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil && rec["callId"] == callID.String() {
			found = rec
		}
	}
	if found == nil {
		t.Fatalf("no record carries callId %s:\n%s", callID, buf.Bytes())
	}
	want := map[string]any{
		"level": "WARN", "hangupCause": CauseOriginatorCancel, "sipStatus": float64(487),
		"hasRingProgress": false, "endedBy": "PEER",
	}
	for k, v := range want {
		if found[k] != v {
			t.Errorf("%s = %v, want %v", k, found[k], v)
		}
	}
	if !strings.Contains(found["msg"].(string), "does not know") {
		t.Errorf("msg = %v", found["msg"])
	}

	// A cause the table knows says nothing.
	buf.Reset()
	Classify(CallEnd{CallID: callID, HangupCause: CauseOriginatorCancel, SIPStatus: 487, HasRingProgress: true})
	if bytes.Contains(buf.Bytes(), []byte(callID.String())) {
		t.Errorf("a far end's 487 after ringing warned:\n%s", buf.Bytes())
	}
}

// lockedBuffer is a log sink that other goroutines may write to while a test
// reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.b.Bytes())
}

func (l *lockedBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
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
		{"reaped by recovery", CallEnd{HangupCause: "RECOVERY_ON_TIMER_EXPIRE", IsAnswered: true}, OutcomeBotFailed},
		{"answered after the window closed", CallEnd{HangupCause: "ALLOTTED_TIMEOUT", IsAnswered: true}, OutcomeBotFailed},
		{"the bridge to the bot never ran", CallEnd{HangupCause: "BOT_BRIDGE_UNCONFIRMED", IsAnswered: true}, OutcomeBotFailed},
		{"the bot answered and its session never started", CallEnd{HangupCause: "BOT_START_FAILED", IsAnswered: true}, OutcomeBotFailed},
		{"vanished after a bot talked", CallEnd{HangupCause: "CHANNEL_VANISHED", IsAnswered: true, HasBot: true}, OutcomeAnswered},
		{"vanished with no bot", CallEnd{HangupCause: "CHANNEL_VANISHED", IsAnswered: true}, OutcomeBotFailed},
		{"no bot slot", CallEnd{HangupCause: "OVERFLOW", IsAnswered: true}, OutcomeOverflow},
		{"overflow even if the ledger lost the answer", CallEnd{HangupCause: "OVERFLOW"}, OutcomeOverflow},
		// A cause that would be a retry for a ring is nothing of the kind once
		// somebody has answered.
		{"busy cause after an answer", CallEnd{HangupCause: "USER_BUSY", IsAnswered: true}, OutcomeAnswered},
		{"temporary failure after an answer", CallEnd{HangupCause: "NORMAL_TEMPORARY_FAILURE", IsAnswered: true}, OutcomeAnswered},
		// A refused or pre-media-failed bot leg keeps the switch's cause (a 503
		// is NORMAL_TEMPORARY_FAILURE, which alone is ANSWERED, above) and
		// carries botReason BOT_LEG_FAILED, which is what makes it the bot's
		// failure (#459).
		{"bot leg refused: temporary failure", CallEnd{HangupCause: "NORMAL_TEMPORARY_FAILURE", IsAnswered: true, BotReason: "BOT_LEG_FAILED"}, OutcomeBotFailed},
		{"bot leg refused: busy", CallEnd{HangupCause: "USER_BUSY", IsAnswered: true, BotReason: "BOT_LEG_FAILED"}, OutcomeBotFailed},
		{"bot leg refused: rejected", CallEnd{HangupCause: "CALL_REJECTED", IsAnswered: true, BotReason: "BOT_LEG_FAILED"}, OutcomeBotFailed},
		{"bot leg refused: gateway down", CallEnd{HangupCause: "GATEWAY_DOWN", IsAnswered: true, BotReason: "BOT_LEG_FAILED"}, OutcomeBotFailed},
		{"bot leg failed before media: normal clearing", CallEnd{HangupCause: "NORMAL_CLEARING", IsAnswered: true, BotReason: "BOT_LEG_FAILED"}, OutcomeBotFailed},
		{"another bot reason changes nothing", CallEnd{HangupCause: "NORMAL_TEMPORARY_FAILURE", IsAnswered: true, BotReason: "SESSION_LIMIT"}, OutcomeAnswered},
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
		OutcomeTransient: ClassSystem, OutcomeLineRejected: ClassSystem, OutcomeOriginateFailed: ClassSystem,
		OutcomeInvalidNumber: ClassNone, OutcomeDNCSkipped: ClassNone,
	}
	if len(want) != len(AllOutcomes()) {
		t.Errorf("the test covers %d outcomes and there are %d", len(want), len(AllOutcomes()))
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
	dnc, bad, limit, unconfirmed := errors.New("dnc"), errors.New("bad"), errors.New("limit"), errors.New("unconfirmed")
	kinds := DialErrorKinds{NumberOnDNC: dnc, BadNumber: bad, LineLimit: limit, Unconfirmed: unconfirmed}
	tests := []struct {
		name string
		err  error
		want DialVerdict
	}{
		{"on the do-not-call list", dnc, DialVerdict{Outcome: OutcomeDNCSkipped}},
		{"wrapped", errors.Join(errors.New("dial"), dnc), DialVerdict{Outcome: OutcomeDNCSkipped}},
		{"a bad number", bad, DialVerdict{Outcome: OutcomeInvalidNumber}},
		{"the line limit", limit, DialVerdict{IsVoid: true}},
		{"unconfirmed by the switch", errors.Join(unconfirmed, errors.New("esl command timed out")), DialVerdict{IsPending: true}},
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
