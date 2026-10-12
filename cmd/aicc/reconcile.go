// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
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
		hasBotSession func(uuid.UUID) bool, isColdStart bool) outbound.RecoveryReport
	MarkReconciled()
}

// Retry backoff when the switch's channels cannot be read: 2 s, doubling to a
// cap of 30 s, for as long as the link stays up.
const (
	reconcileBackoffMin = 2 * time.Second
	reconcileBackoffMax = 30 * time.Second
)

// reconcileLive is the hook run on every connect, the first included: adopt
// what the switch holds, then settle it, then let outbound admit calls.
//
// The first attempt runs inside the hook, before the link starts delivering
// events, so in the ordinary case nothing interleaves with it. If it fails the
// hook returns (it must: the link does not pump events until its hooks have
// returned) and a goroutine retries with backoff while the link is up. The
// gate opens on the first success and never otherwise: admitting a call while
// ignorant of those in flight is the failure the gate exists to prevent.
//
// Only the first success after the process started is a cold start, the only
// pass that may end a call for want of a bot session.
func reconcileLive(adopter liveAdopter, recovery liveRecovery, hasBotSession func(uuid.UUID) bool,
	isLinkUp func() bool, log *slog.Logger, backoffMin time.Duration) func(context.Context) {

	var (
		generation atomic.Int64
		hasSucceed atomic.Bool
	)
	attempt := func(ctx context.Context) bool {
		live, err := adopter.AdoptLiveChannels(ctx)
		if err != nil {
			log.ErrorContext(ctx, "could not read the switch's channels; outbound stays closed until they can be",
				"error", err)
			return false
		}
		rep := recovery.Recover(ctx, live, hasBotSession, !hasSucceed.Load())
		hasSucceed.Store(true)
		recovery.MarkReconciled()
		log.InfoContext(ctx, "live channels reconciled",
			"held", rep.Held, "rearmed", rep.Rearmed, "resumed", rep.Resumed,
			"windowSpent", rep.WindowSpent, "answeredParked", rep.AnsweredParked,
			"orphanedBot", rep.OrphanedBot, "unrecoverable", rep.Unrecoverable, "left", rep.Left)
		return true
	}

	return func(ctx context.Context) {
		mine := generation.Add(1) // a newer connect supersedes older retries
		if attempt(ctx) {
			return
		}
		go func() {
			wait := backoffMin
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				if generation.Load() != mine || (isLinkUp != nil && !isLinkUp()) {
					return
				}
				if attempt(ctx) {
					return
				}
				wait = min(wait*2, reconcileBackoffMax)
			}
		}()
	}
}
