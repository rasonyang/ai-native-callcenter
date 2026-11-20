// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// switchCallCaps gives the switch, at every connect, the deployment's two
// maximum call durations (#637): when the switch ends a call a bot still
// serves (AICC_BOT_MAX_CALL_DURATION plus the grace) and the cap on the part
// an agent serves (AICC_AGENT_MAX_CALL_DURATION). The Lua dialplan schedules
// its kills from them, on inbound calls and at queue entry; the legs this
// application originates carry their own.
//
// Every connect, because a switch that restarted has forgotten them, and it
// always restarts with the link down. A switch that never heard from this
// process still ends calls, at the scripts' defaults, which are the shipped
// defaults of the two settings.
type switchCallCaps struct {
	set                    func(botKillAfter, agentCap time.Duration) error
	botKillAfter, agentCap time.Duration
	log                    *slog.Logger
}

func newSwitchCallCaps(set func(botKillAfter, agentCap time.Duration) error,
	botCap, agentCap time.Duration, log *slog.Logger) *switchCallCaps {
	return &switchCallCaps{set: set, botKillAfter: telephony.BotKillAfter(botCap), agentCap: agentCap, log: log}
}

// onConnect is the link's connect hook.
func (c *switchCallCaps) onConnect(ctx context.Context) {
	attrs := []any{"botKillAfterSec", int(c.botKillAfter.Seconds()), "agentMaxSec", int(c.agentCap.Seconds())}
	if err := c.set(c.botKillAfter, c.agentCap); err != nil {
		c.log.WarnContext(ctx, "could not give the switch the deployment's maximum call durations; "+
			"its scripts end calls at their defaults until the next connect",
			append(attrs, "error", err)...)
		return
	}
	c.log.InfoContext(ctx, "the switch ends calls at the deployment's maximum call durations", attrs...)
}
