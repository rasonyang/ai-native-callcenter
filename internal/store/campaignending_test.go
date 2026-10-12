// SPDX-License-Identifier: Apache-2.0

package store

import (
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
			tt.cdr.CallID = uuid.New()
			tt.cdr.TalkSec = 12
			tt.cdr.EndedAt = start.Add(time.Minute)
			got := endingFromCDR(tt.cdr)
			if got.Outcome != tt.want {
				t.Errorf("outcome = %s, want %s", got.Outcome, tt.want)
			}
			if got.HangupCause != tt.cdr.HangupCause || got.TalkSec != 12 || !got.EndedAt.Equal(tt.cdr.EndedAt) {
				t.Errorf("ending = %+v", got)
			}
		})
	}
}
