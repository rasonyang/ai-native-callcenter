// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
	"time"
)

// The switch's own end to a call that outlasted the deployment's maximum
// duration (#637).
//
// The application ends a bot's call at the flow's time limit, inside
// AICC_BOT_MAX_CALL_DURATION. That ending runs in the application, so it fails
// with it: a process that is dead, or alive with the call's goroutine stuck,
// still sends the bot's silence frames and no RTP timeout ever fires. So the
// switch holds a deadline of its own on the customer's channel, a task it
// schedules itself, which transfers the channel into an extension of the aicc
// dialplan (freeswitch/conf/dialplan/aicc.xml) that stamps the leg and hangs it
// up:
//
//   - the part a bot serves: scheduled when the customer answers an AI call
//     (the originate of an AI outbound leg, aicc_inbound.lua for every DID), at
//     the bot cap plus MaxCallDurationGrace, so the application's own ending
//     always runs first;
//   - the part an agent serves: scheduled at the call's first queue entry
//     (aicc_queue.lua, which keeps that deadline on every later entry) and on
//     the agent leg of a click-to-dial at the agent's answer, at
//     AICC_AGENT_MAX_CALL_DURATION.
//
// It is a transfer and not sched_hangup because the mark must exist only when
// the kill really ran: sched_hangup reads a cause it does not know as
// NORMAL_CLEARING, defaults to the ALLOTTED_TIMEOUT a late answer already
// uses, and nothing in its hangup says the scheduler did it.

// MaxCallDurationCause is the hangupCause of a leg the switch ended because the
// call reached the deployment's maximum duration. The extension stamps it as
// VarHangupCause, so it replaces the switch's own cause on the leg's hangup.
const MaxCallDurationCause = "MAX_CALL_DURATION"

// Which part of a call the switch's maximum cut short.
const (
	// MaxCallDurationSideBot: the bot cap plus the grace ran out.
	MaxCallDurationSideBot = "BOT"
	// MaxCallDurationSideAgent: the agent cap ran out.
	MaxCallDurationSideAgent = "AGENT"
	// MaxCallDurationSideUnknown: a leg stamped MAX_CALL_DURATION without a
	// side this build knows. Nothing ships that stamps one; it is counted
	// rather than lost.
	MaxCallDurationSideUnknown = "UNKNOWN"
)

// MaxCallDurationGrace is how long after the bot cap the switch ends a leg a
// bot still serves: long enough for the application's own ending at the cap
// (a handover line, a goodbye, the transfer) to run first.
const MaxCallDurationGrace = 60 * time.Second

// BotKillAfter is when, after the customer answers, the switch ends a call a
// bot still serves: the bot cap plus MaxCallDurationGrace.
func BotKillAfter(botCap time.Duration) time.Duration { return botCap + MaxCallDurationGrace }

const (
	// varMaxCallDurationSide is the side the extension stamps beside
	// VarHangupCause.
	varMaxCallDurationSide = "aicc_max_call_duration_side"
	// varMaxCallDurationOnAnswer is the execute_on_answer variable that
	// schedules the kill on a leg the application originates. Its own
	// suffix, because the switch runs every variable that starts with
	// execute_on_answer and a leg may carry another (click-to-dial's
	// unset sip_h_Call-Info).
	varMaxCallDurationOnAnswer = "execute_on_answer_aicc_max_call_duration"

	// The extensions of the aicc context the task transfers to.
	maxCallDurationBotExtension   = "aicc_max_call_duration_bot"
	maxCallDurationAgentExtension = "aicc_max_call_duration_agent"

	// The switch's global variables the Lua dialplan reads its two numbers
	// from, whole seconds: the bot kill (cap plus grace) and the agent cap.
	// Not in vars.xml: a reloadxml sets every variable vars.xml names back to
	// the file's value, which would undo the application's push until the
	// next reconnect. The scripts fall back to the shipped defaults.
	globalBotKillSec  = "aicc_max_call_duration_bot_sec"
	globalAgentCapSec = "aicc_max_call_duration_agent_sec"
)

// wholeSeconds is d in whole seconds, rounded up: the switch's scheduler
// counts in seconds, and a cap is never shortened by the rounding.
func wholeSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// BotKillOnAnswer is the channel variable that schedules the bot-side kill on
// a leg the application originates for an AI call, when the customer
// answers: a name and its value for the originate's variables. The value has
// spaces and is quoted on its way into the block, as every value is.
func BotKillOnAnswer(botCap time.Duration) (name, value string) {
	return varMaxCallDurationOnAnswer, scheduleTransfer(BotKillAfter(botCap), maxCallDurationBotExtension)
}

// AgentKillOnAnswer is the channel variable that schedules the agent-side kill
// on the agent leg of a click-to-dial, counted from the agent's answer.
func AgentKillOnAnswer(agentCap time.Duration) (name, value string) {
	return varMaxCallDurationOnAnswer, scheduleTransfer(agentCap, maxCallDurationAgentExtension)
}

// scheduleTransfer is the sched_transfer application's data. The + is always
// written: without it the number is an absolute time, and a small one is in
// the past, so the transfer runs at once.
func scheduleTransfer(after time.Duration, extension string) string {
	return fmt.Sprintf("sched_transfer +%d %s XML aicc", wholeSeconds(after), extension)
}

// SetMaxCallDurations tells the switch the deployment's two maximums, in the
// global variables the Lua dialplan reads: when the bot-side kill fires after
// the customer answers (BotKillAfter of the bot cap) and the agent cap. Sent
// on every connect, so a switch that restarted or never met this process is
// brought to the deployment's settings; until then the scripts' defaults hold.
func (a *Adapter) SetMaxCallDurations(botKillAfter, agentCap time.Duration) error {
	if err := a.exec("global_setvar %s=%d", globalBotKillSec, wholeSeconds(botKillAfter)); err != nil {
		return err
	}
	return a.exec("global_setvar %s=%d", globalAgentCapSec, wholeSeconds(agentCap))
}

// maxCallDurationSide reads the side the extension stamped; a value this
// build does not know is MaxCallDurationSideUnknown.
func maxCallDurationSide(raw string) string {
	switch raw {
	case MaxCallDurationSideBot, MaxCallDurationSideAgent:
		return raw
	}
	return MaxCallDurationSideUnknown
}
