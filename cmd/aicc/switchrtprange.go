// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/rasonyang/ai-native-callcenter/internal/httpapi"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// botRTPRangeCheck asserts, at every connect, that the bot's RTP port range and
// the switch's share no port.
//
// Each side binds ports in its range as calls come and go: a port the bot
// holds fails a switch leg that is given it, and a port the switch holds
// narrows the bot's pool, so an overlap fails calls on either side at random
// under load. Like the UUID check, a known violation fails /readyz; anything short of
// an answer (never read, read failed, link lost, range invalid) is unknown,
// which never does.
//
// onLost clears the result, as the UUID check does: with no link the range may
// have changed behind our back (a restarted switch reads its config again), so
// the next connect re-reads it rather than vouching for a stale answer.
type botRTPRangeCheck struct {
	// readRange asks the switch for its configured RTP range.
	readRange func() (telephony.RTPRange, error)
	// low and high are the bot's range; isEnabled says whether the bot runs.
	low, high int
	isEnabled bool
	log       *slog.Logger

	mu     sync.Mutex
	status httpapi.BotRTPRangeStatus
}

func newBotRTPRangeCheck(readRange func() (telephony.RTPRange, error), low, high int, isEnabled bool, log *slog.Logger) *botRTPRangeCheck {
	c := &botRTPRangeCheck{readRange: readRange, low: low, high: high, isEnabled: isEnabled, log: log}
	c.status = c.unknown()
	return c
}

func (c *botRTPRangeCheck) unknown() httpapi.BotRTPRangeStatus {
	return httpapi.BotRTPRangeStatus{State: httpapi.BotRTPRangeUnknown, BotLow: c.low, BotHigh: c.high}
}

// Status is the latest result; safe for concurrent use.
func (c *botRTPRangeCheck) Status() httpapi.BotRTPRangeStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *botRTPRangeCheck) set(s httpapi.BotRTPRangeStatus) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

// onConnect is the link's connect hook. It asks on api, like the UUID check:
// the reply is needed inline.
func (c *botRTPRangeCheck) onConnect(ctx context.Context) {
	if !c.isEnabled {
		c.set(httpapi.BotRTPRangeStatus{State: httpapi.BotRTPRangeOff})
		return
	}
	r, err := c.readRange()
	if err != nil {
		c.set(c.unknown())
		c.log.WarnContext(ctx, "cannot check the switch's RTP range against the bot's", "error", err)
		return
	}
	if r.End < r.Start || r.Legs() == 0 {
		// An unusable range is the doctor's SWITCH_RTP_RANGE_INVALID; it says
		// nothing about an overlap.
		c.set(c.unknown())
		c.log.WarnContext(ctx, "the switch's RTP range is invalid, so it cannot be checked against the bot's",
			"switchStart", r.Start, "switchEnd", r.End)
		return
	}
	st := httpapi.BotRTPRangeStatus{State: httpapi.BotRTPRangeOK, BotLow: c.low, BotHigh: c.high, SwitchStart: r.Start, SwitchEnd: r.End}
	if telephony.RTPRangesOverlap(c.low, c.high, r) {
		st.State = httpapi.BotRTPRangeOverlaps
		c.log.ErrorContext(ctx, "the bot's RTP port range overlaps the switch's: calls on either side will fail at random under load",
			"botStart", c.low, "botEnd", c.high, "switchStart", r.Start, "switchEnd", r.End,
			"switchRangeIsBuiltIn", r.IsBuiltIn(),
			"fix", "move AICC_BOT_RTP_PORT_LOW/AICC_BOT_RTP_PORT_HIGH, or RTP_START/RTP_END for the switch (FS_RTP_START_PORT/FS_RTP_END_PORT on the switch image), so the ranges share no port")
	}
	c.set(st)
}

// onLost is the link's lost hook.
func (c *botRTPRangeCheck) onLost() {
	if c.isEnabled {
		c.set(c.unknown())
	}
}
