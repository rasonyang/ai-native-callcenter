// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

const (
	testBotLeg = "019ffa1d-0dc1-7b9e-b124-cffb41e90a3d"
	// head is everything before the fallback in an outbound bot bridge: the
	// two sets that make the continuation run only on a bridge that never
	// came up, then the bridge itself.
	bridgeHead = "uuid_transfer chan-1 'm:^:set:continue_on_fail=true^set:hangup_after_bridge=true^" +
		"bridge:{origination_uuid=" + testBotLeg + "}sofia/gateway/aicc_bot/95011^"
	stamp = "set:aicc_bot_reason=BOT_LEG_FAILED^"
)

func TestTheBotBridgeCarriesItsContinuation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		fallback BotFallback
		want     string
	}{
		{"a fallback queue", FallbackQueue("7001", "zh"), bridgeHead + stamp + "transfer:7001 XML aicc' inline"},
		{"the prompt in zh", FallbackPrompt("zh"),
			bridgeHead + stamp + "playback:${sounds_dir}/aicc/zh/bot-unavailable.wav^hangup:${originate_disposition}' inline"},
		{"the prompt in en", FallbackPrompt("en"),
			bridgeHead + stamp + "playback:${sounds_dir}/aicc/en/bot-unavailable.wav^hangup:${originate_disposition}' inline"},
		{"an empty language is en", FallbackPrompt(""),
			bridgeHead + stamp + "playback:${sounds_dir}/aicc/en/bot-unavailable.wav^hangup:${originate_disposition}' inline"},
		{"a junk language is en and never reaches the string", FallbackPrompt("zh'^hangup"),
			bridgeHead + stamp + "playback:${sounds_dir}/aicc/en/bot-unavailable.wav^hangup:${originate_disposition}' inline"},
		{"a campaign call hangs up without a word", FallbackHangup(),
			bridgeHead + stamp + "hangup:${originate_disposition}' inline"},
		{"the zero value is the silent hangup", BotFallback{},
			bridgeHead + stamp + "hangup:${originate_disposition}' inline"},
		{"a queue extension that is syntax degrades to the prompt", FallbackQueue("7001^hangup", "zh"),
			bridgeHead + stamp + "playback:${sounds_dir}/aicc/zh/bot-unavailable.wav^hangup:${originate_disposition}' inline"},
		{"an empty queue extension degrades to the prompt", FallbackQueue("", "en"),
			bridgeHead + stamp + "playback:${sounds_dir}/aicc/en/bot-unavailable.wav^hangup:${originate_disposition}' inline"},
		{"a queue extension with a dollar degrades to the prompt", FallbackQueue("${system(id)}", "en"),
			bridgeHead + stamp + "playback:${sounds_dir}/aicc/en/bot-unavailable.wav^hangup:${originate_disposition}' inline"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, c := newTestAdapter()
			if err := a.BridgeToBot("chan-1", uuid.MustParse(testBotLeg), "sofia/gateway/aicc_bot/95011", nil, tt.fallback); err != nil {
				t.Fatalf("BridgeToBot: %v", err)
			}
			if got := c.last(); got != tt.want {
				t.Errorf("sent\n  %s\nwant\n  %s", got, tt.want)
			}
		})
	}
}

// The ${…} the continuation holds are the switch's to expand when it runs the
// action. They are in the command's format, so the check that refuses a $ in
// a value never sees them; a $ in a value still refuses the bridge.
func TestTheContinuationDoesNotLoosenTheValueChecks(t *testing.T) {
	t.Parallel()
	a, c := newTestAdapter()
	err := a.BridgeToBot("chan-1", uuid.MustParse(testBotLeg), "sofia/gateway/aicc_bot/95011",
		map[string]string{"aicc_language": "${system(id)}"}, FallbackPrompt("en"))
	if !errors.Is(err, ErrUnsafeValue) || len(c.sent) != 0 {
		t.Fatalf("err = %v, sent = %v; want the bridge refused and nothing sent", err, c.sent)
	}
	err = a.BridgeToBot("chan-1", uuid.MustParse(testBotLeg), "sofia/gateway/aicc_bot/95011^hangup", nil, FallbackPrompt("en"))
	if !errors.Is(err, ErrUnsafeValue) || len(c.sent) != 0 {
		t.Fatalf("err = %v, sent = %v; want the endpoint refused", err, c.sent)
	}
}

func TestPromptLanguageIsAFixedMap(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"zh": "zh", "en": "en", "": "en", "ZH": "en", "zh-CN": "en", "fr": "en", "zh'^hangup": "en"} {
		if got := PromptLanguage(in); got != want {
			t.Errorf("PromptLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAMidCallRescueIntoThePromptEndsWithAPlainHangup(t *testing.T) {
	t.Parallel()
	a, c := newTestAdapter()
	if err := a.PlayBotUnavailable("chan-1", "zh"); err != nil {
		t.Fatal(err)
	}
	want := "uuid_transfer chan-1 'm:^:set:aicc_bot_reason=BOT_LEG_FAILED^playback:${sounds_dir}/aicc/zh/bot-unavailable.wav^hangup' inline"
	if got := c.last(); got != want {
		t.Errorf("sent\n  %s\nwant\n  %s", got, want)
	}
	if err := a.PlayBotUnavailable("chan 1", "zh"); !errors.Is(err, ErrUnsafeValue) {
		t.Errorf("a channel id holding a space: err = %v, want ErrUnsafeValue", err)
	}
	if err := a.StampBotLegFailed("chan-1"); err != nil || c.last() != "uuid_setvar chan-1 aicc_bot_reason BOT_LEG_FAILED" {
		t.Errorf("stamp: %v, sent %q", err, c.last())
	}
	if strings.Contains(c.last(), "originate_disposition") {
		t.Error("a stamp holds a continuation")
	}
}

// A call that lost its bot leg has a reason and no summary; the reason must
// still reach the CDR, beside the switch's own cause.
func TestABotReasonReachesTheCDRWithoutASummary(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		CallID:    uuid.Must(uuid.NewV7()),
		CallType:  events.CallTypeOutbound,
		CreatedAt: at(0),
		EndedAt:   atPtr(10),
		Bot:       BotShare{Reason: BotLegFailed},
		Parties: []PartySnapshot{
			{Role: RoleOriginator, Number: "13800138000", ChannelID: "chan-a",
				AnsweredAt: atPtr(2), ReleasedAt: atPtr(10), ReleaseCause: "NORMAL_TEMPORARY_FAILURE"},
		},
	}
	cdr := newAssembler(&memoryLedger{}, staticQueues{}).assemble(t.Context(), snap)
	if got := cdr.UserData["botReason"]; got != "BOT_LEG_FAILED" {
		t.Errorf("userData.botReason = %v", got)
	}
	if _, ok := cdr.UserData["botSummary"]; ok {
		t.Error("a summary was invented")
	}
	if cdr.HangupCause != "NORMAL_TEMPORARY_FAILURE" {
		t.Errorf("hangupCause = %q, want the switch's", cdr.HangupCause)
	}
}
