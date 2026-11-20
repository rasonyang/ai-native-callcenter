// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"log/slog"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
)

// startSession counts this call's provider session start on the process's
// rate meter (#604), just before the session is made and dials. Only a
// campaign call's start waits first, for room in the provider's session window
// (#644): campaign admission paces the calls it places, but their sessions
// open when callees answer, and callees answer together. The wait is FIFO among
// campaign calls and ends when the oldest start leaves the window, or at
// ratemeter.StartWaitBound, past which the start is made as it was before the
// gate and the provider decides. An inbound or internal call, or a call placed
// by anything other than a campaign, is counted and never waits: a caller who
// dialled in is never held. It returns ctx's error when the call ended while
// it waited; nothing was counted then.
//
// What the callee hears meanwhile is the silence of an answered leg whose
// session is not up yet, as during the provider's own setup, which follows.
// The clocks that start at the answer keep running through it: the call's time
// limit counts from the bot's answer (budgetFor, #637) and the switch's from
// the customer's; a call that listens first (#594) counts its opening's wait
// from the answer too, and its listener is not running yet, so a greeting said
// during the wait is not heard as one. The dead-air timer starts only after the
// bot has spoken, so it does not see the wait.
func (o *Orchestrator) startSession(ctx context.Context, isCampaign bool, log *slog.Logger) error {
	starts := o.cfg.SessionStarts
	if starts == nil {
		return nil
	}
	if !isCampaign {
		starts.SessionStarted()
		return nil
	}
	start, err := starts.AwaitStart(ctx, ratemeter.StartWaitBound)
	outcome := obs.SessionStartWaitRoom
	switch {
	case err != nil:
		outcome = obs.SessionStartWaitCallEnded
	case start.IsPastBound:
		outcome = obs.SessionStartWaitBound
	}
	obs.RecordProviderSessionStartWait(o.cfg.Profile.Name, outcome, start.Waited.Milliseconds())
	if err == nil && start.Waited > 0 {
		log.Info("provider session start waited for the session window",
			"waitedMs", start.Waited.Milliseconds(), "isPastBound", start.IsPastBound)
	}
	return err
}
