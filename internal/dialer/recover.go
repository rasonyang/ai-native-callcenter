// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// liveRecheck is how long a call that is still live past its backstop is left
// before it is looked at again. A live call is somebody talking, not an
// attempt that lost its events.
const liveRecheck = 60 * time.Second

// recoverAttempts is design 10 section 8.2 step 3, run once the switch has
// been read and before anything is admitted: every attempt still DIALING is
// either a call that lives on (it becomes a flight again), a call whose CDR
// is already in the ledger (finished from it), or a call that left no trace
// (UNKNOWN: it counts against maxAttempts and is retried after the normal
// interval, never at once).
func (d *Dialer) recoverAttempts(ctx context.Context) {
	dialing, err := d.st.Dialing(ctx)
	if err != nil {
		d.log.Error("dialer: cannot list the attempts in flight", "error", err)
		return
	}
	now := d.clock.Now()
	var adopted, finished, unknown int
	for _, da := range dialing {
		a := da.Attempt
		if _, held := d.flights[a.CallID]; held {
			continue
		}
		if d.isLive(a.CallID) {
			ring := time.Duration(da.RingTimeoutSec) * time.Second
			d.flights[a.CallID] = &flight{
				attemptID: a.ID, contactID: a.ContactID, campaignID: a.CampaignID, callID: a.CallID,
				startedAt: a.StartedAt, ring: ring, nextCheck: a.StartedAt.Add(ring + d.cfg.BackstopMargin),
			}
			d.inflight[a.CampaignID]++
			d.ringing++
			d.ringBy(a.CampaignID, 1)
			obsInFlight(false, 1)
			adopted++
			continue
		}
		switch d.reconcile(ctx, a.CallID, now) {
		case reconciledFromCDR:
			finished++
		case reconciledUnknown:
			unknown++
		}
	}
	d.log.Info("dialer: attempts in flight reconciled after start",
		"adopted", adopted, "finishedFromCDR", finished, "unknown", unknown)
}

type reconciliation int

const (
	reconciledNone reconciliation = iota
	reconciledFromCDR
	reconciledUnknown
)

// reconcile ends an attempt that has no live call: from its CDR when there is
// one, else as UNKNOWN.
func (d *Dialer) reconcile(ctx context.Context, callID uuid.UUID, now time.Time) reconciliation {
	found, err := d.st.FinishFromLedger(ctx, callID)
	if err != nil {
		if !errors.Is(err, campaign.ErrNoAttempt) {
			d.log.Error("dialer: cannot finish an attempt from its CDR", "callId", callID, "error", err)
		}
		return reconciledNone
	}
	if found {
		d.drop(callID)
		return reconciledFromCDR
	}
	done, err := d.st.Finish(ctx, callID, campaign.Ending{
		Outcome: campaign.OutcomeUnknown, HangupCause: "UNKNOWN", EndedAt: now,
	})
	if err != nil {
		d.log.Error("dialer: cannot finish an unknown attempt", "callId", callID, "error", err)
		return reconciledNone
	}
	d.drop(callID)
	if done != nil {
		attrs := []any{
			"callId", callID, "attemptId", done.AttemptID, "contactId", done.ContactID, "campaignId", done.CampaignID,
			"contactStatus", done.ContactStatus, "settleReason", done.SettleReason,
		}
		if done.NextAttemptAt != nil {
			attrs = append(attrs, "nextAttemptAt", *done.NextAttemptAt)
		}
		d.log.Warn("dialer: an attempt left no trace; counted as UNKNOWN", attrs...)
	}
	return reconciledUnknown
}

// checkBackstops reconciles the attempts that are past ring timeout plus the
// margin with no CDR and no live call: events were lost (design 10 section
// 5.5). One with a live call is left alone.
func (d *Dialer) checkBackstops(ctx context.Context, now time.Time) {
	for _, f := range d.flights {
		if now.Before(f.nextCheck) {
			continue
		}
		if d.isLive(f.callID) {
			f.nextCheck = now.Add(liveRecheck)
			continue
		}
		d.log.Warn("dialer: no CDR and no live call past the ring timeout; reconciling",
			"callId", f.callID, "campaignId", f.campaignID)
		if d.reconcile(ctx, f.callID, now) == reconciledNone {
			f.nextCheck = now.Add(liveRecheck)
		}
		d.isDirty = true
	}
}

// skipDNC is design 10 section 12 for the contacts of running campaigns: one
// still waiting for its dial is skipped (its attempt ends DNC_SKIPPED and the
// contact becomes DNC); one whose call is already ringing or talking is not
// hung up, and becomes DNC at once so that no retry follows.
func (d *Dialer) skipDNC(ctx context.Context, now time.Time) {
	if err := d.st.MarkDialingDNC(ctx); err != nil {
		d.log.Error("dialer: cannot mark the contacts in flight do-not-call", "error", err)
	}
	for round := 0; round < 100; round++ {
		ids, err := d.st.DNCContacts(ctx, 200)
		if err != nil {
			d.log.Error("dialer: cannot list do-not-call contacts", "error", err)
			return
		}
		if len(ids) == 0 {
			return
		}
		skipped := 0
		for _, id := range ids {
			att, err := d.st.Begin(ctx, id, now)
			if err != nil {
				continue // in flight elsewhere, or gone: a later sweep gets it
			}
			if _, err := d.st.Finish(ctx, att.CallID, campaign.Ending{
				Outcome: campaign.OutcomeDNCSkipped, HangupCause: "NUMBER_ON_DNC", EndedAt: now,
			}); err != nil {
				d.log.Error("dialer: cannot skip a do-not-call contact", "contactId", id, "error", err)
				continue
			}
			skipped++
		}
		if skipped == 0 {
			return
		}
	}
}
