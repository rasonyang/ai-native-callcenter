// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// How a ledger row reads as a campaign outcome: no database, just the CDR.
func TestACDRReadsAsACampaignOutcome(t *testing.T) {
	start := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	botLeg := []Leg{{Kind: "BOT", Label: "sales", DurationSec: 40}}
	tests := []struct {
		name string
		cdr  CDR
		want campaign.Outcome
	}{
		{"rang out", CDR{Status: CDRStatusNoAnswer, HangupCause: "NO_ANSWER"}, campaign.OutcomeNoAnswer},
		{"busy", CDR{Status: CDRStatusBusy, HangupCause: "USER_BUSY"}, campaign.OutcomeBusy},
		{"talked to the bot", CDR{Status: CDRStatusAnswered, AnsweredAt: start, BotSec: 40, Legs: botLeg, HangupCause: "NORMAL_CLEARING"}, campaign.OutcomeAnswered},
		{"the bot path wrote a failure", CDR{Status: CDRStatusFailed, AnsweredAt: start, BotSec: 3, Legs: botLeg, HangupCause: "MEDIA_OR_PROVIDER_FAILURE"}, campaign.OutcomeBotFailed},
		{"answered on the status alone", CDR{Status: CDRStatusAnswered, HangupCause: "NORMAL_CLEARING"}, campaign.OutcomeAnswered},
		{"overflow", CDR{Status: CDRStatusAnswered, AnsweredAt: start, HangupCause: "OVERFLOW"}, campaign.OutcomeOverflow},
		{"reaped by recovery after an answer", CDR{Status: CDRStatusFailed, AnsweredAt: start, HangupCause: "RECOVERY_ON_TIMER_EXPIRE"}, campaign.OutcomeBotFailed},
		{"a late answer", CDR{Status: CDRStatusFailed, AnsweredAt: start, HangupCause: "ALLOTTED_TIMEOUT"}, campaign.OutcomeBotFailed},
		{"vanished with a bot leg", CDR{Status: CDRStatusAnswered, AnsweredAt: start, Legs: botLeg, HangupCause: "CHANNEL_VANISHED"}, campaign.OutcomeAnswered},
		{"vanished before anyone answered", CDR{Status: CDRStatusFailed, HangupCause: "CHANNEL_VANISHED"}, campaign.OutcomeUnknown},
		{"recovery on an unanswered call", CDR{Status: CDRStatusFailed, HangupCause: "RECOVERY_ON_TIMER_EXPIRE"}, campaign.OutcomeTransient},
		{"a placement failure", CDR{Status: CDRStatusFailed, HangupCause: "GATEWAY_DOWN", Tech: map[string]any{"isPlacementFailure": true}}, campaign.OutcomeOriginateFailed},
		{"a placement failure that was the callee's refusal", CDR{Status: CDRStatusFailed, HangupCause: "USER_BUSY", Tech: map[string]any{"isPlacementFailure": true}}, campaign.OutcomeBusy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cdr.CallID = uuid.Must(uuid.NewV7())
			tt.cdr.TalkSec = 0
			tt.cdr.EndedAt = start.Add(time.Minute)
			got := endingFromCDR(tt.cdr)
			if got.Outcome != tt.want {
				t.Errorf("outcome = %s, want %s", got.Outcome, tt.want)
			}
			if got.HangupCause != tt.cdr.HangupCause || got.TalkSec != tt.cdr.BotSec || !got.EndedAt.Equal(tt.cdr.EndedAt) {
				t.Errorf("ending = %+v", got)
			}
		})
	}
}

// A carrier that ends an unanswered call itself with a 408 after ringing is the
// customer not answering (#576). The row carries what the switch said in its
// tech: a Go int on the row just assembled, a float64 on one read back from
// jsonb, a json.Number from a decoder that asks for them.
func TestACarriers408AfterRingingReadsAsNoAnswer(t *testing.T) {
	start := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	ring := FormatRingStartedAt(start.Add(2 * time.Second))
	tests := []struct {
		name string
		tech map[string]any
		want campaign.Outcome
	}{
		{"status as an int, as just assembled", map[string]any{CDRTechSIPStatus: 408, CDRTechRingStartedAt: ring}, campaign.OutcomeNoAnswer},
		{"status as read back from jsonb", map[string]any{CDRTechSIPStatus: float64(408), CDRTechRingStartedAt: ring}, campaign.OutcomeNoAnswer},
		{"status as an int64", map[string]any{CDRTechSIPStatus: int64(408), CDRTechRingStartedAt: ring}, campaign.OutcomeNoAnswer},
		{"status as a json.Number", map[string]any{CDRTechSIPStatus: json.Number("408"), CDRTechRingStartedAt: ring}, campaign.OutcomeNoAnswer},
		{"408 and the leg never rang", map[string]any{CDRTechSIPStatus: 408}, campaign.OutcomeTransient},
		{"an empty ringStartedAt is no ring progress", map[string]any{CDRTechSIPStatus: 408, CDRTechRingStartedAt: ""}, campaign.OutcomeTransient},
		{"rang and no status: our own reap", map[string]any{CDRTechRingStartedAt: ring}, campaign.OutcomeTransient},
		{"a status that is not a number", map[string]any{CDRTechSIPStatus: "408", CDRTechRingStartedAt: ring}, campaign.OutcomeTransient},
		{"another status", map[string]any{CDRTechSIPStatus: float64(503), CDRTechRingStartedAt: ring}, campaign.OutcomeTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cdr := CDR{CallID: uuid.Must(uuid.NewV7()), Status: CDRStatusNoAnswer, HangupCause: "RECOVERY_ON_TIMER_EXPIRE",
				EndedAt: start.Add(26 * time.Second), Tech: tt.tech}
			if got := endingFromCDR(cdr).Outcome; got != tt.want {
				t.Errorf("outcome = %s, want %s", got, tt.want)
			}
		})
	}
}

// A far end that ends a ringing INVITE with a 487 of its own is reported as
// ORIGINATOR_CANCEL. After ring progress it is the customer not answering, the
// same as the 408 above; the side that ended the leg is read but decides
// nothing, so a row written before it was kept reads the same.
func TestAFarEnds487AfterRingingReadsAsNoAnswer(t *testing.T) {
	start := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	ring := FormatRingStartedAt(start.Add(time.Second))
	tests := []struct {
		name string
		tech map[string]any
		want campaign.Outcome
	}{
		{"as just assembled", map[string]any{CDRTechSIPStatus: 487, CDRTechRingStartedAt: ring, CDRTechEndedBy: "PEER"}, campaign.OutcomeNoAnswer},
		{"as read back from jsonb", map[string]any{CDRTechSIPStatus: float64(487), CDRTechRingStartedAt: ring, CDRTechEndedBy: "PEER"}, campaign.OutcomeNoAnswer},
		{"a row with no endedBy", map[string]any{CDRTechSIPStatus: float64(487), CDRTechRingStartedAt: ring}, campaign.OutcomeNoAnswer},
		{"487 and the leg never rang", map[string]any{CDRTechSIPStatus: 487, CDRTechEndedBy: "PEER"}, campaign.OutcomeTransient},
		{"rang and no status", map[string]any{CDRTechRingStartedAt: ring, CDRTechEndedBy: "PLATFORM"}, campaign.OutcomeTransient},
		{"no tech at all", nil, campaign.OutcomeTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cdr := CDR{CallID: uuid.Must(uuid.NewV7()), Status: CDRStatusNoAnswer, HangupCause: "ORIGINATOR_CANCEL",
				EndedAt: start.Add(13 * time.Second), Tech: tt.tech}
			if got := endingFromCDR(cdr).Outcome; got != tt.want {
				t.Errorf("outcome = %s, want %s", got, tt.want)
			}
		})
	}
}

// The warning of a cause the outcome table does not know names the call and
// what the row's tech says: the far end's status, the ring and who ended it.
//
// Not parallel: it reads the default logger.
func TestAnUnknownCausesWarningCarriesTheRowsFacts(t *testing.T) {
	var buf syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	cdr := CDR{CallID: uuid.Must(uuid.NewV7()), Status: CDRStatusNoAnswer, HangupCause: "ORIGINATOR_CANCEL",
		Tech: map[string]any{CDRTechSIPStatus: float64(487), CDRTechEndedBy: "PEER"}}
	if got := endingFromCDR(cdr).Outcome; got != campaign.OutcomeTransient {
		t.Fatalf("outcome = %s, want TRANSIENT", got)
	}
	var found map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["callId"] == cdr.CallID.String() {
			found = rec
		}
	}
	if found == nil {
		t.Fatalf("no warning carries callId %s:\n%s", cdr.CallID, buf.String())
	}
	for k, v := range map[string]any{"hangupCause": "ORIGINATOR_CANCEL", "sipStatus": float64(487), "hasRingProgress": false, "endedBy": "PEER"} {
		if found[k] != v {
			t.Errorf("%s = %v, want %v", k, found[k], v)
		}
	}
}

// An AI call's conversation is the CDR's BotSec; TalkSec is agent time and is
// zero on a campaign call.
func TestAnAttemptsTalkTimeIsTheConversationWithTheBot(t *testing.T) {
	got := endingFromCDR(CDR{Status: CDRStatusAnswered, BotSec: 41, TalkSec: 0, HangupCause: "NORMAL_CLEARING"})
	if got.TalkSec != 41 {
		t.Fatalf("talk = %d, want 41", got.TalkSec)
	}
}
