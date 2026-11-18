// SPDX-License-Identifier: Apache-2.0

package dialer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// maxRefills bounds how many batches one campaign may defer in one pass; a
// list of tens of thousands outside its window is worked through over a few
// passes, not in one long stall of the actor.
const maxRefills = 50

// bucket is the contacts fetched for one campaign in one pass, already judged
// against their calling windows.
type bucket struct {
	ready []campaign.Contact
	// done: nothing more to take this pass (empty, or stopped by a limit).
	done bool
	// blocked: a due, in-window contact waits on a limit.
	blocked string
}

// pass is one look at everything: housekeeping, then round-robin admission,
// then completion and the account of what each campaign is doing.
func (d *Dialer) pass(ctx context.Context) {
	d.isDirty = false
	clear(d.stopped)
	now := d.clock.Now()
	d.nextWake = now.Add(d.cfg.SafetyScan)

	running, err := d.st.RunningCampaigns(ctx)
	if err != nil {
		d.log.Error("dialer: cannot list running campaigns", "error", err)
		d.nextWake = now.Add(5 * time.Second)
		return
	}
	nextSample := d.matureSamples(now)
	if !nextSample.IsZero() && nextSample.Before(d.nextWake) {
		d.nextWake = nextSample
	}
	if d.dncDirty {
		d.dncDirty = false
		d.skipDNC(ctx, now)
		// Contacts on the list moved to DNC_SKIPPED in any campaign.
		for _, c := range running {
			d.touch(c.ID)
		}
	}
	if d.testDirty {
		d.testDirty = false
		// A number joined the list of test numbers: contacts of running
		// campaigns that were waiting out the frequency window or the daily dial
		// cap for it are due again. They are judged like any due contact in this
		// pass.
		if d.cfg.Frequency > 0 || d.cfg.MaxDialsPerNumberPerDay > 0 {
			if err := d.st.RearmHeld(ctx, now); err != nil {
				d.log.Error("dialer: cannot re-arm contacts held for a test number", "error", err)
				d.testDirty = true
			}
		}
	}
	if d.rearmAll {
		// The dialer has just started: the outer hours may have changed with the
		// restart, so every running campaign is judged again.
		d.rearmAll = false
		for _, c := range running {
			d.rearm[c.ID] = uuid.Nil
		}
	}
	d.rearmWaiting(ctx, running, now)

	if d.ln != nil {
		if limits, err := d.ln.DIDLimits(ctx); err != nil {
			d.log.Error("dialer: cannot read the DIDs' outbound line limits; the last known ones apply", "error", err)
		} else {
			d.limits = limits
		}
	}

	live := make([]campaign.Campaign, 0, len(running))
	for _, c := range running {
		if !d.halted[c.ID] {
			live = append(live, c)
		}
	}
	buckets := map[uuid.UUID]*bucket{}
	for _, c := range live {
		buckets[c.ID] = &bucket{}
	}
	if n := len(live); n > 0 {
		// Fair between campaigns: the next call goes to the campaign with the
		// fewest in flight, the rotating start breaking ties. Calls end one by
		// one as well as together, so the share each campaign holds must be
		// restored from what is in flight, not from the order of this pass.
		start := d.rot % n
		d.rot++
		for ctx.Err() == nil {
			var pick *campaign.Campaign
			for i := 0; i < n; i++ {
				c := &live[(start+i)%n]
				if buckets[c.ID].done || d.halted[c.ID] || d.stopped[c.ID] {
					continue
				}
				if pick == nil || d.inflight[c.ID] < d.inflight[pick.ID] {
					pick = c
				}
			}
			if pick == nil {
				break
			}
			d.admitOne(ctx, *pick, buckets[pick.ID], now)
		}
	}

	next := time.Time{}
	seen := map[uuid.UUID]bool{}
	for _, c := range live {
		seen[c.ID] = true
		n := d.account(ctx, c, buckets[c.ID], now)
		if !n.IsZero() && (next.IsZero() || n.Before(next)) {
			next = n
		}
	}
	for id := range d.progress {
		if !seen[id] {
			delete(d.progress, id)
		}
	}
	for _, f := range d.flights {
		if f.nextCheck.Before(next) || next.IsZero() {
			next = f.nextCheck
		}
	}
	if !next.IsZero() && next.Before(d.nextWake) {
		d.nextWake = next
	}
	// A pass that stopped short of the whole list asks for another at once.
	for id, b := range buckets {
		if d.stopped[id] {
			// Stopped for this pass only. Asking for another pass at once would
			// retry a failed pause in a tight loop, so the retry waits.
			if retry := now.Add(stoppedRetry); retry.Before(d.nextWake) {
				d.nextWake = retry
			}
			continue
		}
		if b.blocked == "" && !b.done {
			d.isDirty = true
		}
	}
}

// stoppedRetry is how long a campaign whose pause did not land waits before the
// next pass tries again.
const stoppedRetry = 5 * time.Second

// rearmWaiting judges again the waiting contacts of the campaigns in d.rearm:
// one whose calling window a PATCH changed, one that has just started or
// resumed, and every running one when the dialer starts, so a window changed
// while paused and a changed AICC_OUTBOUND_HOURS take effect too (design 10
// section 8.1).
//
// A waiting contact is ACTIVE, due later than now, and carries no hold mark: a
// contact that the frequency window or the daily dial cap moved keeps its time
// and its mark, and admission judges the hold again when it is due. A contact
// being dialled has no due time and is not touched. The new due time is the
// later of now and the contact's retry time (the time its retry policy gave
// it; none for a contact never tried), moved to the next opening of its
// schedule in its own zone exactly as the settle moves a retry: never earlier
// than its retry interval allows, never inside a closed window. It depends on
// nothing but the contact, the schedule and now, so judging a contact again
// changes nothing, and only a due time that differs is written. A window
// narrowed under a contact moves it later, to the opening admission would
// defer it to when it came due.
//
// The pace: one read takes cfg.rearmPage waiting contacts, and one pass makes
// at most cfg.maxRearmPages reads across every campaign. The rest are judged
// by the passes that follow at once, so a list of tens of thousands is never
// held in memory and does not stall admission for long.
//
// A campaign that is not RUNNING is dropped from d.rearm: a PAUSED one is
// judged when it resumes.
func (d *Dialer) rearmWaiting(ctx context.Context, running []campaign.Campaign, now time.Time) {
	if len(d.rearm) == 0 {
		return
	}
	byID := make(map[uuid.UUID]campaign.Campaign, len(running))
	for _, c := range running {
		byID[c.ID] = c
	}
	pages := 0
	for id, after := range d.rearm {
		c, ok := byID[id]
		if !ok {
			delete(d.rearm, id)
			continue
		}
		for {
			if pages >= d.cfg.maxRearmPages {
				d.isDirty = true // the rest in the pass that follows at once
				return
			}
			pages++
			last, err := d.judgePage(ctx, c, after, now)
			if err != nil {
				// The cursor stays: a later pass carries on from it.
				d.log.Error("dialer: cannot judge the campaign's waiting contacts again", "campaignId", c.ID, "error", err)
				break
			}
			if last == uuid.Nil {
				delete(d.rearm, id)
				break
			}
			after = last
			d.rearm[id] = after
		}
	}
}

// judgePage judges the page of the campaign's waiting contacts that follows
// the id after, and writes the due times that changed. It returns the last id
// of a full page, uuid.Nil after the last page.
func (d *Dialer) judgePage(ctx context.Context, c campaign.Campaign, after uuid.UUID, now time.Time) (uuid.UUID, error) {
	waiting, err := d.st.Waiting(ctx, c.ID, now, after, d.cfg.rearmPage)
	if err != nil {
		return uuid.Nil, err
	}
	var judged []campaign.Waiting
	var ats []time.Time
	for _, w := range waiting {
		if at, ok := d.dueAgain(c, w, now); ok && !at.Equal(w.NextAttemptAt) {
			judged, ats = append(judged, w), append(ats, at)
		}
	}
	moved, err := d.st.Rearm(ctx, judged, ats)
	if err != nil {
		return uuid.Nil, err
	}
	if moved > 0 {
		d.log.Info("dialer: waiting contacts judged again against the calling schedule",
			"campaignId", c.ID, "moved", moved)
	}
	if len(waiting) < d.cfg.rearmPage {
		return uuid.Nil, nil
	}
	return waiting[len(waiting)-1].ContactID, nil
}

// dueAgain is when a waiting contact is due under its schedule as it stands:
// the later of now and its retry time, moved to the schedule's next opening.
// False when the schedule has no opening within reach: the contact keeps its
// time. A contact with no schedule has no window applied, as at admission.
func (d *Dialer) dueAgain(c campaign.Campaign, w campaign.Waiting, now time.Time) (time.Time, bool) {
	at := now
	if w.RetryAt != nil && w.RetryAt.After(now) {
		at = *w.RetryAt
	}
	sched, ok := d.scheduleFor(c, campaign.Contact{TimeZone: w.TimeZone})
	if !ok {
		return at, true
	}
	return sched.NextOpen(at)
}

// admitOne tries to place one call for the campaign and reports whether it
// made progress (placed one, or moved contacts out of the way).
func (d *Dialer) admitOne(ctx context.Context, c campaign.Campaign, b *bucket, now time.Time) bool {
	if len(b.ready) == 0 {
		moved, err := d.refill(ctx, c, b, now)
		if err != nil {
			d.log.Error("dialer: cannot read contacts", "campaignId", c.ID, "error", err)
			b.done = true
			return false
		}
		if len(b.ready) == 0 {
			b.done = true
			return moved
		}
	}
	if reason := d.limit(ctx, c); reason != "" {
		b.done, b.blocked = true, reason
		obs.RecordDialerBlocked(reason)
		return false
	}
	contact := b.ready[0]
	b.ready = b.ready[1:]
	d.admit(ctx, c, contact, now)
	return true
}

// limit says which limit forbids one more call for the campaign, if any (design
// 10 section 6):
//
//	ringing < maxRinging
//	campaign in flight < max_concurrent
//	DID outbound lines in use < the DID's limit
//	botInUse + sum over campaigns (p_c x ringing_c) + 1 <= cap - inboundReserve
//
// botInUse counts the slots in use, those reserved for answered legs that have
// not bridged yet, and the answered legs waiting in the overflow hold for one.
// p_c is the campaign's answer-rate bound (CapacityStats.Bound).
func (d *Dialer) limit(ctx context.Context, c campaign.Campaign) string {
	if d.inflight[c.ID] >= c.MaxConcurrent {
		return obs.DialerBlockedCampaign
	}
	if d.ringing >= d.cfg.MaxRinging {
		return obs.DialerBlockedRinging
	}
	if d.ln != nil {
		if limit, ok := d.limits[c.DIDNumber]; ok && d.ln.LinesInUse(c.DIDNumber) >= limit {
			return obs.DialerBlockedDIDLines
		}
	}
	if d.sl != nil {
		u := d.sl.Usage()
		reserve := max(d.cfg.InboundReserve, u.InboundReserve)
		held := float64(u.InUse + u.Reserved + u.Waiting)
		if held+d.expectedAnswers(ctx)+1 > float64(u.Cap-reserve)+1e-9 {
			return obs.DialerBlockedBot
		}
	}
	return ""
}

// expectedAnswers is the sum over campaigns of the bound on the answer rate
// times the legs ringing, which is how many of the ringing legs are expected to
// need a bot slot.
func (d *Dialer) expectedAnswers(ctx context.Context) float64 {
	sum := 0.0
	for id, n := range d.ringingBy {
		bound := d.statsFor(ctx, id).Bound()
		if d.cfg.noOverdial {
			bound = 1
		}
		sum += bound * float64(n)
	}
	return sum
}

// ringBy moves a campaign's ringing count.
func (d *Dialer) ringBy(id uuid.UUID, delta int) {
	if d.ringingBy[id] += delta; d.ringingBy[id] <= 0 {
		delete(d.ringingBy, id)
	}
}

// warmStart is how many of a campaign's latest finished attempts are replayed
// to rebuild its statistics after a restart: three windows, because attempts
// that never rang do not fill the answer-rate window.
const warmStart = 3 * campaign.RateWindow

// statsFor is the campaign's answer-rate and overflow history. It is read from
// the store the first time it is needed and kept current by onFinished, so no
// admission scans the database. A history that cannot be read is a fresh one
// (bound 1, which never over-dials) that is not kept, so the read is tried again.
//
// A finish that lands between the load and its own message being handled is
// counted twice; the effect is one sample in a window of two hundred.
func (d *Dialer) statsFor(ctx context.Context, id uuid.UUID) *campaign.CapacityStats {
	if s, ok := d.stats[id]; ok {
		return s
	}
	s := campaign.NewCapacityStats(d.cfg.OverflowTightenRate, d.cfg.OverflowStopRate)
	recent, err := d.st.RecentOutcomesSince(ctx, id, time.Unix(0, 0), warmStart)
	if err != nil {
		d.log.Warn("dialer: cannot read a campaign's recent outcomes; planning with every ringing leg answering",
			"campaignId", id, "error", err)
		return s
	}
	for i := len(recent) - 1; i >= 0; i-- { // newest first in, oldest first out
		s.Observe(recent[i])
	}
	d.stats[id] = s
	obs.SetCampaignAnswerRateUCB(id.String(), s.Bound())
	return s
}

// refill fetches the next batch of due contacts and judges each against its
// calling window. Contacts held by another campaign's recent touch, or
// outside their window, are moved to the first time they could be dialled and
// never fetched again before then; what remains is ready. It reports whether
// it moved any contact.
func (d *Dialer) refill(ctx context.Context, c campaign.Campaign, b *bucket, now time.Time) (bool, error) {
	free := min(c.MaxConcurrent-d.inflight[c.ID], d.cfg.MaxRinging-d.ringing)
	limit := min(max(free*4, 20), d.cfg.MaxBatch)
	moved := false
	for i := 0; i < maxRefills; i++ {
		var ids []uuid.UUID
		var ats []time.Time

		held, err := d.st.HeldByFrequency(ctx, c.ID, now, d.cfg.Frequency, limit)
		if err != nil {
			return moved, err
		}
		if len(held) > 0 {
			for _, h := range held {
				at := h.LastTouch.Add(d.cfg.Frequency)
				if sched, ok := d.scheduleFor(c, campaign.Contact{TimeZone: h.TimeZone}); ok {
					if open, found := sched.NextOpen(at); found {
						at = open
					}
				}
				ids, ats = append(ids, h.ContactID), append(ats, at)
			}
			if err := d.st.Defer(ctx, ids, ats, campaign.HoldFrequency); err != nil {
				return moved, err
			}
			moved = true
			continue
		}

		due, err := d.st.Due(ctx, c.ID, now, d.cfg.Frequency, limit)
		if err != nil {
			return moved, err
		}
		if len(due) == 0 {
			return moved, nil
		}
		var open []campaign.Contact
		for _, ct := range due {
			sched, ok := d.scheduleFor(c, ct)
			if ok && !sched.IsOpen(now) {
				at, found := sched.NextStart(now)
				if !found {
					at = now.Add(24 * time.Hour)
				}
				ids, ats = append(ids, ct.ID), append(ats, at)
				continue
			}
			open = append(open, ct)
		}
		if len(ids) > 0 {
			if err := d.st.Defer(ctx, ids, ats, campaign.HoldNone); err != nil {
				return moved, err
			}
			moved = true
		}
		// The daily dial cap, judged for the contacts inside their window.
		ready, capped, err := d.holdByDailyCap(ctx, c, open, now)
		if err != nil {
			return moved, err
		}
		b.ready = append(b.ready, ready...)
		moved = moved || capped
		if len(b.ready) > 0 {
			return moved, nil
		}
	}
	// More is waiting than one pass will sort out.
	d.isDirty = true
	return moved, nil
}

// holdByDailyCap splits contacts that are inside their calling window by the
// daily dial cap: those whose number was dialled the cap's number of times in
// the contact's local day are moved to the start of their calling window on the
// next local day (marked is_daily_cap_held; no attempt is made and no budget is
// spent), and the rest are returned as ready. One query counts them all. A
// number on the list of test numbers counts as zero and is never held. It
// reports whether it moved any contact.
func (d *Dialer) holdByDailyCap(ctx context.Context, c campaign.Campaign, contacts []campaign.Contact, now time.Time) ([]campaign.Contact, bool, error) {
	limit := d.cfg.MaxDialsPerNumberPerDay
	if limit <= 0 || len(contacts) == 0 {
		return contacts, false, nil
	}
	numbers := make([]string, len(contacts))
	days := make([]time.Time, len(contacts))
	scheds := make([]campaign.Schedule, len(contacts))
	for i, ct := range contacts {
		scheds[i], _ = d.scheduleFor(c, ct) // no schedule: the zero one, a UTC day
		numbers[i], days[i] = ct.Number, scheds[i].DayStart(now)
	}
	dials, err := d.st.DialsToday(ctx, numbers, days, now)
	if err != nil {
		return nil, false, err
	}
	var ready []campaign.Contact
	var ids []uuid.UUID
	var ats []time.Time
	for i, ct := range contacts {
		if dials[i] < limit {
			ready = append(ready, ct)
			continue
		}
		ids, ats = append(ids, ct.ID), append(ats, scheds[i].DailyCapRelease(now))
	}
	if len(ids) == 0 {
		return ready, false, nil
	}
	if err := d.st.Defer(ctx, ids, ats, campaign.HoldDailyCap); err != nil {
		return nil, false, err
	}
	return ready, true, nil
}

// scheduleFor is the contact's calling schedule: its zone, else the
// campaign's, inside the outer hours. Cached per campaign window and zone.
func (d *Dialer) scheduleFor(c campaign.Campaign, ct campaign.Contact) (campaign.Schedule, bool) {
	key := c.ID.String() + "|" + windowKey(c) + "|" + ct.TimeZone
	if s, ok := d.schedule[key]; ok {
		return s, s.Zone != nil
	}
	if len(d.schedule) > 4096 {
		d.schedule = map[string]campaign.Schedule{}
	}
	s, err := campaign.NewSchedule(c.TimeZone, ct.TimeZone, c.CallingWindow, d.cfg.OutboundHours)
	if err != nil && ct.TimeZone != "" {
		s, err = campaign.NewSchedule(c.TimeZone, "", c.CallingWindow, d.cfg.OutboundHours)
	}
	if err != nil {
		d.log.Warn("dialer: no calling schedule for the contact; its window is not applied",
			"campaignId", c.ID, "zone", ct.TimeZone, "error", err)
		d.schedule[key] = campaign.Schedule{}
		return campaign.Schedule{}, false
	}
	d.schedule[key] = s
	return s, true
}

func windowKey(c campaign.Campaign) string {
	return fmt.Sprintf("%s|%v", c.TimeZone, c.CallingWindow)
}

// admit writes the attempt ahead, then places the call (design 10 section 5.3).
func (d *Dialer) admit(ctx context.Context, c campaign.Campaign, ct campaign.Contact, now time.Time) {
	att, err := d.st.Begin(ctx, ct.ID, now)
	switch {
	case errors.Is(err, campaign.ErrNumberInFlight), errors.Is(err, campaign.ErrContactNotDialable):
		return // someone else has it; it is looked at again when they finish
	case err != nil:
		d.log.Error("dialer: cannot write the attempt ahead", "campaignId", c.ID, "contactId", ct.ID, "error", err)
		return
	}
	ring := time.Duration(c.RingTimeoutSec) * time.Second
	f := &flight{
		attemptID: att.ID, contactID: ct.ID, campaignID: c.ID, callID: att.CallID,
		startedAt: now, ring: ring, nextCheck: now.Add(ring + d.cfg.BackstopMargin),
	}
	d.flights[att.CallID] = f
	d.inflight[c.ID]++
	d.ringing++
	d.ringBy(c.ID, 1)
	obsInFlight(false, 1)

	pctx, cancel := context.WithTimeout(ctx, d.cfg.DialTimeout)
	defer cancel()
	placed, err := d.pl.PlaceAI(pctx, outbound.AIDialRequest{
		CallID: att.CallID, LegID: att.LegID, To: d.dialString(c, ct.Number), CanonicalTo: ct.Number, DIDNumber: c.DIDNumber,
		UserData: ct.Slots, RingTimeout: ring, CampaignID: c.ID, AttemptID: att.ID,
	})
	if err == nil {
		if placed.JobID != "" {
			if err := d.st.SetJob(ctx, att.CallID, placed.JobID); err != nil {
				d.log.Warn("dialer: cannot record the originate job", "callId", att.CallID, "error", err)
			}
		}
		return
	}
	d.dialFailed(ctx, c, f, err, now)
}

// dialFailed is what a synchronous error from the placer does to the attempt
// (design 10 section 5.3).
func (d *Dialer) dialFailed(ctx context.Context, c campaign.Campaign, f *flight, err error, now time.Time) {
	switch {
	case errors.Is(err, outbound.ErrAlreadyPlaced):
		return // an earlier drive of this attempt got through
	case errors.Is(err, outbound.ErrNotReady), errors.Is(err, context.Canceled):
		// Nothing was sent: the attempt never happened.
		d.voidAttempt(ctx, f, now)
		return
	case errors.Is(err, outbound.ErrNoOutboundEndpoint):
		// This deployment has nowhere to dial, so every attempt of every
		// campaign would come back here: no customer is reached, no contact is
		// at fault. Void the attempt (no row, no retry budget spent) and stop
		// the campaign now, on the first attempt, instead of spending the
		// list's system retries on a setting nobody can fix from the list
		// (#293). The start refusal says the same thing while an operator is
		// looking; this covers a campaign that was already running when
		// the application came back without an endpoint (the API refuses to
		// resume one, but the dialer must not depend on that).
		d.voidAttempt(ctx, f, now)
		d.pauseNoEndpoint(ctx, c.ID)
		return
	}
	verdict := campaign.ClassifyDialError(err, campaign.DialErrorKinds{
		NumberOnDNC: outbound.ErrNumberOnDNC, BadNumber: outbound.ErrBadNumber,
		LineLimit: outbound.ErrDIDChannelsExhausted, Unconfirmed: outbound.ErrPlacementUnconfirmed,
	})
	if verdict.IsPending {
		// The switch may still place it. The flight stays, and its backstop
		// (nextCheck) settles it from what the switch did: the CDR of the call
		// if the channel appeared, or of the failed placement if it never did.
		d.log.Warn("dialer: the switch has not confirmed the placement; the attempt stays in flight",
			"campaignId", c.ID, "callId", f.callID, "error", err)
		return
	}
	if verdict.IsVoid {
		d.voidAttempt(ctx, f, now)
		return
	}
	cause := err.Error()
	switch verdict.Outcome {
	case campaign.OutcomeDNCSkipped:
		cause = "NUMBER_ON_DNC"
	case campaign.OutcomeInvalidNumber:
		cause = "INVALID_NUMBER"
	}
	if len(cause) > 200 {
		cause = cause[:200]
	}
	d.log.Warn("dialer: the call could not be placed", "campaignId", c.ID, "callId", f.callID,
		"outcome", verdict.Outcome, "error", err)
	done, ferr := d.st.Finish(ctx, f.callID, campaign.Ending{
		Outcome: verdict.Outcome, HangupCause: cause, EndedAt: now,
	})
	if ferr != nil {
		d.log.Error("dialer: cannot finish the attempt", "callId", f.callID, "error", ferr)
		return
	}
	d.drop(f.callID)
	if done != nil {
		// The breaker is judged now, not when the message comes round: a trunk
		// that is down must stop this pass, not the next one.
		d.judgeBreakers(ctx, *done)
	}
}

func (d *Dialer) voidAttempt(ctx context.Context, f *flight, now time.Time) {
	if err := d.st.Void(ctx, f.callID, now); err != nil {
		d.log.Error("dialer: cannot void the attempt", "callId", f.callID, "error", err)
		return
	}
	d.drop(f.callID)
}

// drop forgets a flight and returns its capacity. It is idempotent.
func (d *Dialer) drop(callID uuid.UUID) *flight {
	f := d.flights[callID]
	if f == nil {
		return nil
	}
	delete(d.flights, callID)
	if d.inflight[f.campaignID]--; d.inflight[f.campaignID] <= 0 {
		delete(d.inflight, f.campaignID)
	}
	if f.isAnswered {
		d.talking--
		obsInFlight(true, -1)
	} else {
		d.ringing--
		d.ringBy(f.campaignID, -1)
		obsInFlight(false, -1)
	}
	return f
}

func obsInFlight(isTalking bool, delta int64) {
	kind := obs.DialerRinging
	if isTalking {
		kind = obs.DialerTalking
	}
	obs.DialerInFlightChanged(kind, delta)
}

// onFinished is an attempt's outcome having been written and committed.
func (d *Dialer) onFinished(ctx context.Context, f campaign.Finished) {
	fl := d.drop(f.CallID)
	d.isDirty = true
	d.touch(f.CampaignID)
	if f.IsRevision {
		return
	}
	obs.RecordCampaignAttempt(string(f.Outcome))
	if f.Outcome == campaign.OutcomeOverflow {
		obs.RecordCampaignOverflow(f.CampaignID.String())
	}
	d.feedStats(f, fl)
	d.judgeBreakers(ctx, f)
}

// feedStats moves the campaign's capacity statistics by a finished attempt, if
// they are loaded (a campaign whose history is read later finds it in the
// store). An attempt the dialer was never told was answered is told now.
func (d *Dialer) feedStats(f campaign.Finished, fl *flight) {
	s, ok := d.stats[f.CampaignID]
	if !ok || !campaign.DoesRing(f.Outcome) {
		return
	}
	now := d.clock.Now()
	if fl == nil {
		fl = &flight{campaignID: f.CampaignID, callID: f.CallID, startedAt: now}
	}
	if campaign.IsAnsweredOutcome(f.Outcome) {
		if !fl.isAnswered {
			s.Answered(f.CallID)
			d.sample(fl, true, now)
		}
		if f.Outcome == campaign.OutcomeOverflow {
			s.Overflowed(f.CallID)
		}
	} else {
		d.sample(fl, false, now)
	}
	obs.SetCampaignAnswerRateUCB(f.CampaignID.String(), s.Bound())
}

// rangSample is one attempt's answer-rate sample, held until its ring timeout
// has passed.
type rangSample struct {
	at         time.Time // when the sample is due
	dialledAt  time.Time
	campaignID uuid.UUID
	isAnswered bool
}

// sampleMargin is how long past the ring timeout an attempt's sample waits for
// the non-answers of its cohort, whose hangups arrive a moment after the
// timeout.
const sampleMargin = 3 * time.Second

// sample queues an attempt's answer-rate sample for the moment its ring timeout
// is over, or now if that has passed: every attempt enters the window in the
// order it was dialled, not in the order its ring happened to resolve.
func (d *Dialer) sample(f *flight, isAnswered bool, now time.Time) {
	at := f.startedAt.Add(f.ring + sampleMargin)
	if at.Before(now) {
		at = now
	}
	d.samples = append(d.samples, rangSample{at: at, dialledAt: f.startedAt, campaignID: f.campaignID, isAnswered: isAnswered})
}

// matureSamples moves the samples whose time has come into their campaigns'
// windows, and returns when the next one is due (zero for none). The window
// orders them by when the attempt was dialled, so the order they arrive in does
// not matter.
func (d *Dialer) matureSamples(now time.Time) time.Time {
	var next time.Time
	touched := map[uuid.UUID]bool{}
	keep := d.samples[:0]
	for _, sm := range d.samples {
		if sm.at.After(now) {
			keep = append(keep, sm)
			if next.IsZero() || sm.at.Before(next) {
				next = sm.at
			}
			continue
		}
		if s, ok := d.stats[sm.campaignID]; ok {
			s.Rang(sm.dialledAt, sm.isAnswered)
			touched[sm.campaignID] = true
		}
	}
	d.samples = keep
	for id := range touched {
		obs.SetCampaignAnswerRateUCB(id.String(), d.stats[id].Bound())
	}
	return next
}

// judgeBreakers pauses the campaign when its run of failures says the trunk or
// the provider is down (design 10 section 7.4). The run starts at the
// campaign's last start or resume, so a resumed campaign is not paused again
// by the failures that paused it.
func (d *Dialer) judgeBreakers(ctx context.Context, f campaign.Finished) {
	if d.halted[f.CampaignID] {
		return
	}
	since := d.since[f.CampaignID]
	var reason campaign.PauseReason
	switch {
	case campaign.ClassOf(f.Outcome) == campaign.ClassSystem:
		recent, err := d.st.RecentOutcomesSince(ctx, f.CampaignID, since, d.cfg.TrunkBreakerCount)
		if err == nil && campaign.TrunkBreakerTrips(recent, d.cfg.TrunkBreakerCount) {
			reason = campaign.PauseTrunkFailure
		}
	case f.Outcome == campaign.OutcomeBotFailed:
		recent, err := d.st.RecentOutcomesSince(ctx, f.CampaignID, since, max(d.cfg.ProviderBreakerCount*50, 200))
		if err == nil && campaign.ProviderBreakerTrips(recent, d.cfg.ProviderBreakerCount) {
			reason = campaign.PauseProviderFailure
		}
	}
	if reason == "" {
		return
	}
	paused, err := d.st.PauseFor(ctx, f.CampaignID, reason)
	if err != nil {
		d.log.Error("dialer: cannot pause the campaign", "campaignId", f.CampaignID, "error", err)
		return
	}
	if paused {
		d.touch(f.CampaignID)
		d.halted[f.CampaignID] = true
		d.isDirty = true
		d.log.Warn("dialer: campaign paused by a breaker", "campaignId", f.CampaignID, "reason", reason)
	}
}

// pauseNoEndpoint pauses a campaign that cannot place a call at all, under its
// own reason, OUTBOUND_NOT_CONFIGURED: the cause is a setting nobody can fix at
// the carrier, which TRUNK_FAILURE would send them to look at. aicc doctor's
// outbound_endpoint warning names the setting to fix (deploy/carrier.md).
//
// Only the first attempt of a pass pauses: PauseFor is conditional on RUNNING,
// so the attempts already in flight find the campaign PAUSED and stop there.
//
// Whatever PauseFor answers, the campaign admits nothing more in this pass
// (stopped). voidAttempt has made the refused contact due again, so a pass that
// kept admitting would refuse the same contacts forever, and a database error
// is enough to start that. halted is deliberately left for the success case
// alone: only a resume clears it, and a campaign whose pause failed is still
// RUNNING, so marking it halted would leave it never dialled again. The next
// pass starts with stopped empty, dials, is refused, and tries the pause again.
func (d *Dialer) pauseNoEndpoint(ctx context.Context, id uuid.UUID) {
	if d.halted[id] || d.stopped[id] {
		return
	}
	d.stopped[id] = true
	paused, err := d.st.PauseFor(ctx, id, campaign.PauseOutboundNotConfigured)
	if err != nil {
		d.log.Error("dialer: cannot pause a campaign that cannot dial", "campaignId", id, "error", err)
		return
	}
	if !paused {
		return
	}
	d.halted[id] = true
	d.isDirty = true
	d.touch(id)
	d.log.Warn("dialer: campaign paused: the deployment has no outbound endpoint (AICC_OUTBOUND_ENDPOINT); "+
		"set it and resume, see deploy/carrier.md", "campaignId", id)
}

// maxEagerCapDefers bounds how many daily-cap-held contacts one account pass
// defers to their release time; see account.
const maxEagerCapDefers = 20

// account computes what the campaign is doing, completes it when the list is
// exhausted, and returns the earliest time it needs another look. When the
// earliest pending contact is held by the daily dial cap but its own retry
// time is earlier than the release, account defers it to the release at once
// (marked as refill marks it), so nextDialAt and the activity describe the
// next contact the dialer would really dial and not a retry time at which
// nothing can be placed.
func (d *Dialer) account(ctx context.Context, c campaign.Campaign, b *bucket, now time.Time) time.Time {
	pending, err := d.st.Pending(ctx, c.ID, now)
	if err != nil {
		d.log.Error("dialer: cannot read the campaign's contacts", "campaignId", c.ID, "error", err)
		return time.Time{}
	}
	if d.halted[c.ID] {
		delete(d.progress, c.ID)
		return time.Time{}
	}
	inflight := d.inflight[c.ID]
	if pending.ActiveTotal == 0 && inflight == 0 && pending.AllTotal > 0 {
		if done, err := d.st.Complete(ctx, c.ID, now); err != nil {
			d.log.Error("dialer: cannot complete the campaign", "campaignId", c.ID, "error", err)
		} else if done {
			d.log.Info("dialer: campaign completed", "campaignId", c.ID)
			d.touch(c.ID)
			delete(d.progress, c.ID)
			return time.Time{}
		}
	}

	p := campaign.Progress{NextDialAt: pending.NextFuture}
	switch {
	case b != nil && b.blocked != "":
		p.Activity = campaign.ActivityAtCapacity
		p.NextDialAt = nil
	case inflight > 0:
		p.Activity = campaign.ActivityDialing
	default:
		p.Activity = campaign.ActivityWaitingRetry
		for deferred := 0; ; deferred++ {
			first, err := d.st.EarliestPending(ctx, c.ID)
			if err != nil || first == nil {
				break
			}
			act, release := d.waitingActivity(ctx, c, *first, now)
			p.Activity = act
			// A contact held by the daily cap whose own retry time comes
			// before the cap lifts would be due with nothing to dial: defer
			// it to the release now, as refill would when it came due, so
			// nextDialAt names a time at which a call can really go out. At
			// most maxEagerCapDefers per pass; the rest wait for the next one,
			// which comes no later than the earliest of their retry times.
			if release == nil || deferred >= maxEagerCapDefers {
				break
			}
			if err := d.st.Defer(ctx, []uuid.UUID{first.ID}, []time.Time{*release}, campaign.HoldDailyCap); err != nil {
				d.log.Error("dialer: cannot defer a capped contact", "campaignId", c.ID, "contactId", first.ID, "error", err)
				break
			}
			if pending, err = d.st.Pending(ctx, c.ID, now); err != nil {
				d.log.Error("dialer: cannot read the campaign's contacts", "campaignId", c.ID, "error", err)
				return time.Time{}
			}
			p.NextDialAt = pending.NextFuture
		}
	}
	d.progress[c.ID] = p
	if pending.NextFuture == nil {
		return time.Time{}
	}
	return *pending.NextFuture
}

// waitingActivity says why a campaign with nothing in flight and nothing at
// capacity is waiting, judged by the contact it would dial first. A hold by
// another campaign's recent call (the frequency window), a hold by the daily
// dial cap and a closed calling window each end at a time; the one that ends
// latest is what the campaign is waiting for. A tie goes to the daily cap, then
// to the frequency window, which are the reasons an operator can do something
// about (the list of test numbers) before the closed window. None of them: the
// contact's own retry time has not come.
//
// The cap is judged from the facts (the number's dials today against the cap),
// not from the contact's mark: after the local midnight the mark may still be
// set while the cap no longer binds.
//
// The second result is the cap's release time when the cap binds and the
// contact's retry time is strictly before it (the caller defers the contact
// there); nil otherwise.
func (d *Dialer) waitingActivity(ctx context.Context, c campaign.Campaign, first campaign.Contact, now time.Time) (campaign.Activity, *time.Time) {
	sched, hasSchedule := d.scheduleFor(c, first)
	windowOpensAt := now
	if hasSchedule && !sched.IsOpen(now) {
		if at, found := sched.NextStart(now); found {
			windowOpensAt = at
		} else {
			// No opening within reach: the window is what holds it.
			return campaign.ActivityWaitingWindow, nil
		}
	}
	// A hold binds unless the contact's own retry time is later than when it
	// could be dialled once the hold is over.
	binds := func(dialAt time.Time) bool {
		return first.NextAttemptAt == nil || !first.NextAttemptAt.After(dialAt.Add(time.Second))
	}
	var freqAt, capAt time.Time
	var deferTo *time.Time
	if d.cfg.Frequency > 0 {
		end, err := d.st.FrequencyHold(ctx, first.ID, now, d.cfg.Frequency)
		if err != nil {
			d.log.Error("dialer: cannot read a contact's frequency hold", "campaignId", c.ID, "contactId", first.ID, "error", err)
		} else if end != nil && end.After(now) && !end.Before(windowOpensAt) {
			dialAt := *end
			if hasSchedule {
				if open, found := sched.NextOpen(dialAt); found {
					dialAt = open
				}
			}
			if binds(dialAt) {
				freqAt = dialAt
			}
		}
	}
	if d.cfg.MaxDialsPerNumberPerDay > 0 {
		dials, err := d.st.DialsToday(ctx, []string{first.Number}, []time.Time{sched.DayStart(now)}, now)
		if err != nil {
			d.log.Error("dialer: cannot read a contact's dials today", "campaignId", c.ID, "contactId", first.ID, "error", err)
		} else if len(dials) == 1 && dials[0] >= d.cfg.MaxDialsPerNumberPerDay {
			release := sched.DailyCapRelease(now)
			if !release.Before(windowOpensAt) && binds(release) {
				capAt = release
				if first.NextAttemptAt != nil && first.NextAttemptAt.Before(release) {
					deferTo = &release
				}
			}
		}
	}
	switch {
	case !capAt.IsZero() && (freqAt.IsZero() || !capAt.Before(freqAt)):
		return campaign.ActivityWaitingDailyCap, deferTo
	case !freqAt.IsZero():
		return campaign.ActivityWaitingFrequency, deferTo
	}
	if windowOpensAt.After(now) {
		return campaign.ActivityWaitingWindow, deferTo
	}
	return campaign.ActivityWaitingRetry, deferTo
}

// publish replaces the read side with the account of each running campaign,
// the in-flight counts as they are now.
func (d *Dialer) publish() {
	prev := *d.view.Load()
	view := make(map[uuid.UUID]campaign.Progress, len(d.progress))
	for id, p := range d.progress {
		p.InFlight = d.inflight[id]
		view[id] = p
	}
	d.view.Store(&view)
	// What the dialer says about a campaign moved: tell the change hook.
	for id, p := range view {
		if old, ok := prev[id]; !ok || !sameProgress(old, p) {
			d.touch(id)
		}
	}
	for id := range prev {
		if _, ok := view[id]; !ok {
			d.touch(id)
		}
	}
}

func sameProgress(a, b campaign.Progress) bool {
	if a.Activity != b.Activity || a.InFlight != b.InFlight {
		return false
	}
	if (a.NextDialAt == nil) != (b.NextDialAt == nil) {
		return false
	}
	return a.NextDialAt == nil || a.NextDialAt.Equal(*b.NextDialAt)
}

// dialString writes the contact's canonical number in the form the trunk
// expects (AICC_OUTBOUND_DIAL_FORM). The attempt and the contact keep the
// canonical number; only the placement carries the dialled string. When the
// NATIONAL form cannot apply (the number is from another country than the
// campaign's), the number is dialled as E.164 and the campaign is warned
// about once.
func (d *Dialer) dialString(c campaign.Campaign, number string) string {
	cc := c.DefaultCountryCode
	if cc == "" {
		cc = d.cfg.DefaultCountryCode
	}
	out, ok := phonenum.DialString(number, d.cfg.DialForm, cc)
	if !ok && d.cfg.DialForm == phonenum.FormNational && !d.warnedCC[c.ID] {
		d.warnedCC[c.ID] = true
		d.log.Warn("dialer: a number is outside the campaign's country code and is dialled as E.164",
			"campaignId", c.ID, "countryCode", cc)
	}
	return out
}
