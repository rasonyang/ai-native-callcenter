// SPDX-License-Identifier: Apache-2.0

package e2e

import "strconv"

// The capacity scenarios of issue #247, as data. Each runs in an application
// start of its own (deploy/e2e/e2e.sh capacity recreates the application with
// the scenario's environment), so the limits it lowers never reach the
// campaign scenarios.
//
// K1, overflow. The dialer never lets two legs ring when the bot has one
// slot: it admits a call only while slots held + expected answers of the
// ringing legs + 1 fit in the capacity, and a campaign without fifty rung
// samples plans for every ringing leg to answer (design 10 section 6.3,
// internal/dialer/admit.go). Three contacts can only be ringing together when
// three slots are free at admission. So K1 runs with three slots, lets the
// campaign put its three contacts on the line, and then takes two of the
// slots away the way production loses them, to calls the campaign does not
// see: two single AI calls (POST /calls, kind AI_OUTBOUND) that answer at
// once and stay up. When the three contacts answer, within the same second,
// one slot is free: one call reaches the bot and two are parked for the
// overflow hold and hung up as OVERFLOW.
//
// K2, the DID's outbound line limit. Two campaigns, one per country, each on a
// DID of its own with maxOutboundChannels 2, each with six contacts that ring
// until the 8 s ring timeout and are never answered. The simulator's record
// shows how many dialogs were up at once per caller id.

// CapacityScenario is one capacity run.
type CapacityScenario struct {
	ID string
	// Regions run one campaign each, every one holding all of Cases.
	Regions []Region
	Cases   []Case
	Plan    campaignPlan
	// LineLimit is the most dialogs from one DID that may be up at once,
	// asserted (K2); it is what Plan gives each DID as maxOutboundChannels.
	LineLimit int
	// Direct are the calls the driver places itself, outside any campaign,
	// once every contact is on the line: K1's slot holders.
	Direct []DirectSpec
	// AppEnv is the application environment the scenario needs, as the
	// resolved compose configuration must show it; "" means unset (the
	// application's default).
	AppEnv map[string]string
}

// DirectSpec is a single AI call the driver places through POST /calls.
type DirectSpec struct {
	Region Region
	Suffix string
	// Callee describes what the simulator does, for the report.
	Callee string
}

// Number is the call's canonical number.
func (d DirectSpec) Number() string { return d.Region.Prefix + d.Suffix }

// DialKey is what the switch is handed for the call, the simulator's key. The
// call is placed through POST /calls, which dials its "to" exactly as sent
// (only the campaign dialer applies AICC_OUTBOUND_DIAL_FORM), so the key is the
// canonical number in every dial form.
func (d DirectSpec) DialKey() string { return d.Number() }

// The application settings the capacity scenarios change.
const (
	EnvBotMaxCalls       = "AICC_BOT_MAX_CALLS"
	EnvBotInboundReserve = "AICC_BOT_INBOUND_RESERVE"
	EnvOverflowHold      = "AICC_OVERFLOW_HOLD"
)

// K1's numbers. The contacts answer 5 s after their INVITE, long after the
// slot holders (300 ms), and stay up 6 s unless the platform hangs up first;
// the slot holders stay up 12 s, past the last overflow hold.
const (
	k1RingTimeoutSec  = 8
	k1AnswerAfterMs   = 5000
	k1ByeAfterMs      = 6000
	k1BotSlots        = 3
	k1HolderAnswerMs  = 300
	k1HolderByeMs     = 12000
	k1OverflowHoldStr = "1500ms"
)

// K2's numbers.
const (
	k2RingTimeoutSec = 8
	k2LineLimit      = 2
)

var k2Limit = k2LineLimit

// k1Contact is one of K1's three contacts; which of them reaches the bot is
// decided by which answer reserves the free slot first, so no case names its
// own outcome and the scenario asserts the split instead.
func k1Contact(id, suffix string) Case {
	return Case{
		ID: id, Kind: KindScenario, Suffix: suffix,
		Callee: "180, then 200 after 5 s; hangs up 6 s after answering unless the platform does first",
		Steps:  []Step{{Provisional: 180, SIPStatus: 200, FinalAfterMs: k1AnswerAfterMs, ByeAfterMs: k1ByeAfterMs}},
	}
}

// k2Contact rings until the ring timeout.
func k2Contact(id, suffix string) Case {
	return Case{
		ID: id, Kind: KindScenario, Suffix: suffix, Callee: "180, never answered",
		Provisional: 180, Outcome: "NO_ANSWER", Cause: "NO_ANSWER",
	}
}

// K1 is one country: the bot's capacity is one number for the whole
// deployment, so a second country would share the one free slot and neither
// country could be held to the issue's one ANSWERED and two OVERFLOW.
var K1 = CapacityScenario{
	ID:      "K1",
	Regions: []Region{Regions[0]},
	Cases: []Case{
		k1Contact("K1-A", "21"), k1Contact("K1-B", "22"), k1Contact("K1-C", "23"),
	},
	Plan: campaignPlan{RingTimeoutSec: k1RingTimeoutSec, MaxAttempts: 1},
	Direct: []DirectSpec{
		{Region: Regions[0], Suffix: "31", Callee: "slot holder: 200 after 300 ms, hangs up 12 s later"},
		{Region: Regions[0], Suffix: "32", Callee: "slot holder: 200 after 300 ms, hangs up 12 s later"},
	},
	AppEnv: map[string]string{
		EnvBotMaxCalls: strconv.Itoa(k1BotSlots), EnvBotInboundReserve: "0", EnvOverflowHold: k1OverflowHoldStr,
		EnvBotRTPPortLow: "", EnvBotRTPPortHigh: "",
	},
}

// K2 runs both countries at once, each campaign on its own DID with the
// limit, so the peak is asserted per DID.
var K2 = CapacityScenario{
	ID:      "K2",
	Regions: Regions,
	Cases: []Case{
		k2Contact("K2-A", "41"), k2Contact("K2-B", "42"), k2Contact("K2-C", "43"),
		k2Contact("K2-D", "44"), k2Contact("K2-E", "45"), k2Contact("K2-F", "46"),
	},
	Plan:      campaignPlan{RingTimeoutSec: k2RingTimeoutSec, MaxAttempts: 1, MaxOutboundChannels: &k2Limit},
	LineLimit: k2LineLimit,
	AppEnv:    map[string]string{EnvBotMaxCalls: "", EnvBotInboundReserve: "", EnvOverflowHold: "", EnvBotRTPPortLow: "", EnvBotRTPPortHigh: ""},
}

// K3, the bot leg that is refused (issue #459), has a runner of its own
// (refusal.go): its calls are not all a campaign's.

// CapacityScenarios are the runs `make e2e-capacity` makes for the campaign
// runner, by ID; K3 is RunRefusal's.
var CapacityScenarios = map[string]CapacityScenario{"K1": K1, "K2": K2}
