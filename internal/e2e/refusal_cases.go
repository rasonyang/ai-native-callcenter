// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strconv"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// K3 is the scenario of issue #459: an answered AI outbound call whose bot
// leg is refused. The bot's INVITE is refused when the UAS cannot bind an RTP
// pair (it answers 503, which the switch maps to NORMAL_TEMPORARY_FAILURE,
// issue #450). K3 narrows the bot's RTP range to exactly one pair, holds the
// pair with a call, and places the calls that then meet no bot:
//
//	R1  a single AI call on a DID with an enabled fallback queue: the customer
//	    is moved to the queue and stays up until the callee hangs up;
//	R2  a single AI call on a DID with no fallback queue, language zh: the
//	    platform plays the short prompt and hangs up;
//	R3  a campaign call on a DID that has a fallback queue: the platform hangs
//	    up with no prompt and never queues, the attempt finishes BOT_FAILED;
//	N1  after the pair is free again, a normal AI call reaches the bot.
//
// The pair is held by a call, not by an external process: a single AI call
// from a DID of its own whose callee answers at once and stays up past the
// last refused case. The run holds itself to that order and fails rather than
// report a split it did not set up.

// The application settings K3 changes, besides the capacity ones.
const (
	EnvBotRTPPortLow  = "AICC_BOT_RTP_PORT_LOW"
	EnvBotRTPPortHigh = "AICC_BOT_RTP_PORT_HIGH"
)

// K3's numbers. The bot's range is exactly one pair: the UAS takes an even
// port for RTP and the next for RTCP, and the range is inclusive, so 26000-26001
// is one pair. It sits just above the switch image's own range (20000-25999),
// and the compose overlay's switch range (20000-20099) is checked against it
// at the start of the run.
const (
	k3RTPLow  = 26000
	k3RTPHigh = 26001

	// The callees answer 300 ms after the INVITE. The holder hangs up 28 s
	// after answering, long past the last refused case (R1 ends 8 s after its
	// answer, the others within a few seconds); the others' hang-up times are
	// the scripted behaviour the assertions read.
	k3AnswerMs         = 300
	k3HolderByeMs      = 28000
	k3QueuedByeMs      = 8000
	k3StaysUpByeMs     = 20000
	k3NormalByeMs      = 6000
	k3RingTimeoutSec   = 8
	k3PlatformByeMaxMs = 6000
	k3QueuedByeSlackMs = 1000
)

// RefusalRole is what a direct call is in K3.
type RefusalRole string

const (
	// RoleHolder holds the bot's one RTP pair.
	RoleHolder RefusalRole = "HOLDER"
	// RoleRefused is placed while the pair is held and meets no bot.
	RoleRefused RefusalRole = "REFUSED"
	// RoleNormal is placed after the holder ended and reaches the bot.
	RoleNormal RefusalRole = "NORMAL"
)

// RefusalCall is a single AI call (POST /calls, AI_OUTBOUND) K3 places.
type RefusalCall struct {
	ID     string
	Role   RefusalRole
	Suffix string
	// Language is the language of the call's DID; empty is the default.
	Language string
	// HasFallbackQueue gives the call's DID an enabled fallback queue.
	HasFallbackQueue bool
	// Callee describes the simulator's behaviour, for the report.
	Callee string
	// Step is the callee's behaviour, the simulator's scenario must hold.
	Step Step
}

// Number is the call's canonical number.
func (c RefusalCall) Number(r Region) string { return r.Prefix + c.Suffix }

// DialKey is what the switch is handed for the call, the simulator's key.
func (c RefusalCall) DialKey(r Region, form phonenum.DialForm) string {
	out, _ := phonenum.DialString(c.Number(r), form, r.CountryCode)
	return out
}

// RefusalScenario is a run of K3.
type RefusalScenario struct {
	ID     string
	Region Region
	// Direct are the single AI calls in the order the run places them: the
	// holder, the refused ones, then the normal one.
	Direct []RefusalCall
	// Campaign is R3, the one contact of the run's campaign; its DID has a
	// fallback queue.
	Campaign Case
	Plan     campaignPlan
	// AppEnv is the application environment the scenario needs.
	AppEnv map[string]string
}

func callee(finalAfterMs, byeAfterMs int) Step {
	return Step{Provisional: 180, SIPStatus: 200, FinalAfterMs: finalAfterMs, ByeAfterMs: byeAfterMs}
}

// K3 is one country, the US range: the numbers are the harness's fictional
// 202 555 01xx, suffixes 51 and up.
var K3 = RefusalScenario{
	ID:     "K3",
	Region: Regions[0],
	Direct: []RefusalCall{
		{ID: "HOLDER", Role: RoleHolder, Suffix: "51", Callee: "200 after 300 ms, hangs up 28 s later", Step: callee(k3AnswerMs, k3HolderByeMs)},
		{ID: "R1", Role: RoleRefused, Suffix: "61", HasFallbackQueue: true, Callee: "200 after 300 ms, hangs up 8 s later", Step: callee(k3AnswerMs, k3QueuedByeMs)},
		{ID: "R2", Role: RoleRefused, Suffix: "62", Language: "zh", Callee: "200 after 300 ms, stays up 20 s unless the platform hangs up", Step: callee(k3AnswerMs, k3StaysUpByeMs)},
		{ID: "N1", Role: RoleNormal, Suffix: "71", Callee: "200 after 300 ms, hangs up 6 s later", Step: callee(k3AnswerMs, k3NormalByeMs)},
	},
	Campaign: Case{
		ID: "R3", Kind: KindScenario, Suffix: "63",
		Callee: "200 after 300 ms, stays up 20 s unless the platform hangs up",
		Steps:  []Step{callee(k3AnswerMs, k3StaysUpByeMs)},
	},
	Plan: campaignPlan{RingTimeoutSec: k3RingTimeoutSec, MaxAttempts: 1},
	AppEnv: map[string]string{
		EnvBotMaxCalls: "", EnvBotInboundReserve: "", EnvOverflowHold: "",
		EnvBotRTPPortLow: strconv.Itoa(k3RTPLow), EnvBotRTPPortHigh: strconv.Itoa(k3RTPHigh),
	},
}

// CampaignCases are the cases of K3's campaign, for the shared preflight.
func (s RefusalScenario) CampaignCases() []Case { return []Case{s.Campaign} }
