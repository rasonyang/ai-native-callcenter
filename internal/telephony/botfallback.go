// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"strings"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/switchsyntax"
)

// BotLegFailed is the aicc_bot_reason (and so the CDR's userData.botReason) of
// a call whose bot leg never carried a conversation: the bot refused the
// INVITE, the gateway was down, or the leg answered and failed before media.
const BotLegFailed = "BOT_LEG_FAILED"

// BotStartFailed is the hangupCause of a call whose bot leg answered (200 OK)
// but whose session never started, when the caller was not then rescued: no
// switch cause says why the call ended, so the platform names the failure. A
// caller who was rescued to a queue keeps the final leg's cause and carries
// this in userData.botReason instead, the rule SESSION_LIMIT follows.
const BotStartFailed = "BOT_START_FAILED"

// botStartedVar is the channel variable the bot stamps on the caller's channel
// (value "true") once its session has started.
const botStartedVar = "aicc_bot_started"

// botReasonVar is the channel variable the CDR reads the bot's reason from.
const botReasonVar = "aicc_bot_reason"

// botUnavailablePrompt is the recording a customer hears when the bot cannot
// take their call and there is no queue to send them to: one per language
// under the switch's sounds directory (see PromptLanguage). Where the file is
// missing the switch plays nothing and the next action, the hangup, runs.
func botUnavailablePrompt(language string) string {
	return "${sounds_dir}/aicc/" + PromptLanguage(language) + "/bot-unavailable.wav"
}

// PromptLanguage is the fixed map from a call's language to the language of
// the platform's own recordings: zh for zh, en for everything else, including
// an empty or malformed value. A raw language never reaches a dialplan string.
func PromptLanguage(language string) string {
	if language == "zh" {
		return "zh"
	}
	return "en"
}

type fallbackKind int

const (
	fallbackHangup fallbackKind = iota
	fallbackQueue
	fallbackPrompt
)

// BotFallback is what the switch does with a customer whose bridge to the bot
// never came up. The zero value hangs up without a word, which is what a
// campaign call gets (design 10 section 10): the safest default is the one
// that never sends a call to a person.
type BotFallback struct {
	kind     fallbackKind
	queueExt string
	language string
}

// FallbackHangup hangs the customer up with the switch's own cause and plays
// nothing.
func FallbackHangup() BotFallback { return BotFallback{} }

// FallbackPrompt plays the bot-unavailable recording in the call's language
// (PromptLanguage), then hangs up.
func FallbackPrompt(language string) BotFallback {
	return BotFallback{kind: fallbackPrompt, language: PromptLanguage(language)}
}

// FallbackQueue transfers the customer to a queue's extension. An extension
// the inline grammar would read as syntax, or an empty one, degrades to the
// prompt, so a value from the database never reaches the dialplan string
// unchecked.
func FallbackQueue(extension, language string) BotFallback {
	if extension == "" || switchsyntax.Unsafe(extension) != "" {
		return FallbackPrompt(language)
	}
	return BotFallback{kind: fallbackQueue, queueExt: extension, language: PromptLanguage(language)}
}

// actions renders the inline actions that follow a bridge, joined with ^. The
// bridge succeeded and later ended: none of them runs (hangup_after_bridge).
// They run only when the bridge never came up.
func (f BotFallback) actions() string {
	parts := []string{"set:" + botReasonVar + "=" + BotLegFailed}
	switch f.kind {
	case fallbackQueue:
		parts = append(parts, "transfer:"+f.queueExt+" XML aicc")
	case fallbackPrompt:
		parts = append(parts, "playback:"+botUnavailablePrompt(f.language), "hangup:${originate_disposition}")
	default:
		parts = append(parts, "hangup:${originate_disposition}")
	}
	return strings.Join(parts, "^")
}

// BridgeToBot connects an answered customer to the bot gateway with a
// continuation for a bridge that never comes up.
//
// It is BridgeToEndpoint with three inline actions in front of the bridge and
// the fallback's after it:
//
//	set:continue_on_fail=true       the dialplan goes on when the bridge fails
//	set:hangup_after_bridge=true    a bot that answered and ended takes the
//	                                customer down, and never reaches the rest
//	bridge:{…}<endpoint>
//	set:aicc_bot_reason=BOT_LEG_FAILED
//	<transfer | playback + hangup | hangup>
//
// The two sets are inline actions because the {…} block of the bridge only
// reaches the new leg. ${originate_disposition} and ${sounds_dir} are written
// literally: the switch expands an inline action's data when it runs it, not
// when the api command is parsed. They are in the format and never in an
// argument, so buildCommand's refusal of $ in a value does not touch them.
func (a *Adapter) BridgeToBot(channelID string, newLeg uuid.UUID, endpoint string, vars map[string]string, fallback BotFallback) error {
	all := map[string]string{"origination_uuid": newLeg.String()}
	for k, v := range vars {
		all[k] = v
	}
	block, err := renderVars(all)
	if err != nil {
		return err
	}
	return a.exec("uuid_transfer %s 'm:^:set:continue_on_fail=true^set:hangup_after_bridge=true^bridge:{%s}%s^%s' inline",
		channelID, rendered(block), dialValue(endpoint), rendered(fallback.actions()))
}

// StampBotLegFailed records on the customer's channel that the bot leg failed,
// so the CDR carries userData.botReason=BOT_LEG_FAILED whatever happens next.
func (a *Adapter) StampBotLegFailed(channelID string) error {
	return a.SetVariable(channelID, botReasonVar, BotLegFailed)
}

// PlayBotUnavailable transfers a customer who is still on the line into an
// inline prompt and hangup: the recording for their language, then a plain
// hangup (the bridge came up, so there is no originate_disposition to use).
func (a *Adapter) PlayBotUnavailable(channelID, language string) error {
	return a.exec("uuid_transfer %s 'm:^:set:%s=%s^playback:%s^hangup' inline",
		channelID, rendered(botReasonVar), rendered(BotLegFailed), rendered(botUnavailablePrompt(language)))
}
