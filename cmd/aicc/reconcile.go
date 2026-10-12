// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// liveAdopter reads the switch's channels into the registry.
type liveAdopter interface {
	AdoptLiveChannels(ctx context.Context) ([]telephony.LiveChannel, error)
}

// liveRecovery settles the channels the registry adopted and opens outbound
// admission once the first pass is done.
type liveRecovery interface {
	Recover(ctx context.Context, live []telephony.LiveChannel,
		hasBotSession func(uuid.UUID) bool) outbound.RecoveryReport
	MarkReconciled()
}

// reconcileAttempts is how many times a pass is tried before giving up until
// the next connect.
const reconcileAttempts = 5

// reconcileLive is the hook run on every connect, the first included: adopt
// what the switch holds, then settle it, then let outbound admit calls.
//
// A listing that cannot be had is retried, and if it still cannot the gate
// stays shut: admitting a call while ignorant of those in flight is the
// failure the gate exists to prevent, and the next connect tries again.
func reconcileLive(adopter liveAdopter, recovery liveRecovery,
	hasBotSession func(uuid.UUID) bool, log *slog.Logger, retryEvery time.Duration) func(context.Context) {

	return func(ctx context.Context) {
		for attempt := 1; attempt <= reconcileAttempts; attempt++ {
			live, err := adopter.AdoptLiveChannels(ctx)
			if err == nil {
				rep := recovery.Recover(ctx, live, hasBotSession)
				recovery.MarkReconciled()
				log.InfoContext(ctx, "live channels reconciled",
					"held", rep.Held, "rearmed", rep.Rearmed, "windowSpent", rep.WindowSpent,
					"answeredParked", rep.AnsweredParked, "orphanedBot", rep.OrphanedBot,
					"unrecoverable", rep.Unrecoverable, "left", rep.Left)
				return
			}
			log.WarnContext(ctx, "could not read the switch's channels",
				"attempt", attempt, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryEvery):
			}
		}
		log.ErrorContext(ctx, "live channels not reconciled; outbound stays closed until the next connect")
	}
}
