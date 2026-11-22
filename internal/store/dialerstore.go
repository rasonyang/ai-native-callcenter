// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// DialerStore is everything the dialer's actor reads and writes. It adds the
// admission queries to the campaign and attempt stores, and keeps the clock
// out of SQL: every time that decides something is passed in.
type DialerStore struct {
	pool      *pgxpool.Pool
	q         *queries.Queries
	campaigns *CampaignStore
	attempts  *AttemptStore
}

// Dialer returns the dialer's store.
func (s *Store) Dialer() *DialerStore {
	return &DialerStore{pool: s.Pool, q: s.Queries, campaigns: s.Campaigns(), attempts: s.Attempts()}
}

// RunningCampaigns lists the campaigns that dial, oldest start first.
func (d *DialerStore) RunningCampaigns(ctx context.Context) ([]campaign.Campaign, error) {
	rows, err := d.q.ListRunningCampaigns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running campaigns: %w", err)
	}
	out := make([]campaign.Campaign, 0, len(rows))
	for _, r := range rows {
		c, err := campaignOf(r.Campaign, r.DidNumber)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Campaign reads one campaign.
func (d *DialerStore) Campaign(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.campaigns.Get(ctx, id)
}

// Start, Pause, Resume and Cancel are the conditional status updates of the
// campaign store; the dialer's actor is the only caller.
func (d *DialerStore) Start(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.campaigns.Start(ctx, id)
}

func (d *DialerStore) Pause(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.campaigns.Pause(ctx, id)
}

func (d *DialerStore) Resume(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.campaigns.Resume(ctx, id)
}

func (d *DialerStore) Cancel(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return d.campaigns.Cancel(ctx, id)
}

// Due returns the campaign's contacts that are due at now and pass every
// filter that SQL can apply: ACTIVE, not on the do-not-call list, no attempt
// in flight for the number anywhere, and not reached by another campaign
// within frequency. The calling window is the caller's to apply.
func (d *DialerStore) Due(ctx context.Context, campaignID uuid.UUID, now time.Time,
	frequency time.Duration, limit int) ([]campaign.Contact, error) {
	rows, err := d.q.DueCampaignContacts(ctx, queries.DueCampaignContactsParams{
		CampaignID: campaignID, Now: ts(now), FrequencySec: frequency.Seconds(), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("due contacts of %s: %w", campaignID, err)
	}
	out := make([]campaign.Contact, 0, len(rows))
	for _, r := range rows {
		c, err := contactOf(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// HeldByFrequency returns due contacts another campaign reached within the
// window, with the time of that touch.
func (d *DialerStore) HeldByFrequency(ctx context.Context, campaignID uuid.UUID, now time.Time,
	frequency time.Duration, limit int) ([]campaign.Held, error) {
	rows, err := d.q.HeldByFrequency(ctx, queries.HeldByFrequencyParams{
		CampaignID: campaignID, Now: ts(now), FrequencySec: frequency.Seconds(), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("held contacts of %s: %w", campaignID, err)
	}
	out := make([]campaign.Held, len(rows))
	for i, r := range rows {
		out[i] = campaign.Held{ContactID: r.ID, TimeZone: deref(r.TimeZone), LastTouch: r.LastTouch.Time}
	}
	return out, nil
}

// Defer moves contacts to a later due time, leaving the mark of the hold that
// moved them (campaign.HoldNone for a calling window's deferral) and clearing
// the other. A contact that is no longer waiting (claimed for a dial,
// finished) or whose time is already later is left alone.
func (d *DialerStore) Defer(ctx context.Context, ids []uuid.UUID, at []time.Time, hold campaign.HoldKind) error {
	if len(ids) == 0 {
		return nil
	}
	ats := make([]pgtype.Timestamptz, len(at))
	for i, t := range at {
		ats[i] = ts(t)
	}
	_, err := d.q.DeferContacts(ctx, queries.DeferContactsParams{Ids: ids, Ats: ats, Hold: string(hold)})
	return err
}

// Waiting pages the campaign's ACTIVE contacts that are due later than now and
// carry no hold mark, in id order after afterID (uuid.Nil for the first page),
// at most limit of them: what a re-arm judges again after a calling window or
// the outer hours changed.
func (d *DialerStore) Waiting(ctx context.Context, campaignID uuid.UUID, now time.Time,
	afterID uuid.UUID, limit int) ([]campaign.Waiting, error) {
	rows, err := d.q.ListWaitingContacts(ctx, queries.ListWaitingContactsParams{
		CampaignID: campaignID, Now: ts(now), AfterID: afterID, RowLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("waiting contacts of %s: %w", campaignID, err)
	}
	out := make([]campaign.Waiting, len(rows))
	for i, r := range rows {
		out[i] = campaign.Waiting{
			ContactID: r.ID, TimeZone: deref(r.TimeZone), NextAttemptAt: r.NextAttemptAt.Time, RetryAt: timeOrNil(r.RetryAt),
		}
	}
	return out, nil
}

// Rearm moves each judged contact to its new due time (at is parallel to
// judged) and reports how many it moved. A contact whose due time, retry time
// or marks are no longer what was judged (claimed for a dial, settled again,
// held, finished) is left alone, and no contact's retry time or marks are
// written.
func (d *DialerStore) Rearm(ctx context.Context, judged []campaign.Waiting, at []time.Time) (int, error) {
	if len(judged) != len(at) {
		return 0, fmt.Errorf("rearm: %d contacts and %d times", len(judged), len(at))
	}
	if len(judged) == 0 {
		return 0, nil
	}
	p := queries.RearmContactsParams{
		Ids: make([]uuid.UUID, len(judged)), JudgedAts: make([]pgtype.Timestamptz, len(judged)),
		RetryAts: make([]pgtype.Timestamptz, len(judged)), Ats: make([]pgtype.Timestamptz, len(judged)),
	}
	for i, w := range judged {
		p.Ids[i], p.JudgedAts[i], p.Ats[i] = w.ContactID, ts(w.NextAttemptAt), ts(at[i])
		if w.RetryAt != nil {
			p.RetryAts[i] = ts(*w.RetryAt)
		}
	}
	n, err := d.q.RearmContacts(ctx, p)
	if err != nil {
		return 0, fmt.Errorf("rearm contacts: %w", err)
	}
	return int(n), nil
}

// Pending says where the campaign's contact list stands at now.
func (d *DialerStore) Pending(ctx context.Context, campaignID uuid.UUID, now time.Time) (campaign.Pending, error) {
	r, err := d.q.CampaignPending(ctx, queries.CampaignPendingParams{CampaignID: campaignID, Now: ts(now)})
	if err != nil {
		return campaign.Pending{}, fmt.Errorf("pending contacts of %s: %w", campaignID, err)
	}
	return campaign.Pending{
		ActiveTotal: int(r.ActiveTotal), DueNow: int(r.DueNow), AllTotal: int(r.AllTotal),
		NextFuture: timeOrNil(r.NextFuture),
	}, nil
}

// FrequencyHold is when the cross-campaign frequency window stops holding the
// contact back, nil when no other campaign's touch within the window does (a
// number on the list of test numbers is never held).
func (d *DialerStore) FrequencyHold(ctx context.Context, contactID uuid.UUID, now time.Time,
	frequency time.Duration) (*time.Time, error) {
	r, err := d.q.ContactFrequencyTouch(ctx, queries.ContactFrequencyTouchParams{
		ContactID: contactID, Now: ts(now), FrequencySec: frequency.Seconds(),
	})
	if err != nil {
		return nil, fmt.Errorf("frequency hold of contact %s: %w", contactID, err)
	}
	if !r.Valid {
		return nil, nil
	}
	end := r.Time.Add(frequency)
	return &end, nil
}

// RearmHeld makes the contacts of RUNNING campaigns that the frequency window or
// the daily dial cap deferred (and only those: the ones carrying a mark) due at
// now, for a number that joined the list of test numbers.
func (d *DialerStore) RearmHeld(ctx context.Context, now time.Time) error {
	_, err := d.q.RearmHeldContacts(ctx, ts(now))
	return err
}

// DialsToday counts, for each (number, day start) pair, the dials of the number
// since the day start up to now: the campaign attempts in flight or that spent
// a budget, from any campaign, and the ad-hoc outbound calls in the ledger. The
// result is parallel to the input. A number on the list of test numbers is
// never capped, so it counts as zero.
func (d *DialerStore) DialsToday(ctx context.Context, numbers []string, dayStarts []time.Time, now time.Time) ([]int, error) {
	if len(numbers) != len(dayStarts) {
		return nil, fmt.Errorf("dials today: %d numbers and %d day starts", len(numbers), len(dayStarts))
	}
	if len(numbers) == 0 {
		return nil, nil
	}
	starts := make([]pgtype.Timestamptz, len(dayStarts))
	for i, t := range dayStarts {
		starts[i] = ts(t)
	}
	var uncounted []string
	for _, o := range campaign.AllOutcomes() {
		if campaign.ClassOf(o) == campaign.ClassNone {
			uncounted = append(uncounted, string(o))
		}
	}
	rows, err := d.q.DialsTodayByNumber(ctx, queries.DialsTodayByNumberParams{
		Now: ts(now), UncountedOutcomes: uncounted, Numbers: numbers, DayStarts: starts,
	})
	if err != nil {
		return nil, fmt.Errorf("dials today: %w", err)
	}
	type key struct {
		number string
		day    int64
	}
	counts := make(map[key]int, len(rows))
	for _, r := range rows {
		counts[key{r.NumberE164, r.DayStart.Time.UnixMicro()}] = int(r.Dials)
	}
	out := make([]int, len(numbers))
	for i := range numbers {
		out[i] = counts[key{numbers[i], dayStarts[i].UnixMicro()}] // absent: listed, zero
	}
	return out, nil
}

// EarliestPending is the ACTIVE contact that is due first, nil if none waits.
func (d *DialerStore) EarliestPending(ctx context.Context, campaignID uuid.UUID) (*campaign.Contact, error) {
	r, err := d.q.EarliestPendingContact(ctx, campaignID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := contactOf(r)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Complete moves a RUNNING campaign with no ACTIVE contact to COMPLETED. It
// reports whether it did.
func (d *DialerStore) Complete(ctx context.Context, campaignID uuid.UUID, at time.Time) (bool, error) {
	n, err := d.q.CompleteCampaign(ctx, queries.CompleteCampaignParams{CampaignID: campaignID, At: ts(at)})
	return n > 0, err
}

// PauseFor pauses a RUNNING campaign on behalf of a breaker.
func (d *DialerStore) PauseFor(ctx context.Context, campaignID uuid.UUID, reason campaign.PauseReason) (bool, error) {
	r := string(reason)
	n, err := d.q.PauseCampaignFor(ctx, queries.PauseCampaignForParams{CampaignID: campaignID, Reason: &r})
	return n > 0, err
}

// DNCContacts returns up to limit waiting contacts of running campaigns whose
// number is on the do-not-call list.
func (d *DialerStore) DNCContacts(ctx context.Context, limit int) ([]uuid.UUID, error) {
	return d.q.DueDNCContacts(ctx, int32(limit))
}

// MarkDialingDNC makes the contacts of calls already in flight DNC, so no
// retry follows. The calls are not touched.
func (d *DialerStore) MarkDialingDNC(ctx context.Context) error {
	_, err := d.q.MarkDialingContactsDNC(ctx)
	return err
}

// Dialing lists every attempt in flight.
func (d *DialerStore) Dialing(ctx context.Context) ([]campaign.DialingAttempt, error) {
	rows, err := d.q.ListDialingAttempts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dialing attempts: %w", err)
	}
	out := make([]campaign.DialingAttempt, len(rows))
	for i, r := range rows {
		out[i] = campaign.DialingAttempt{Attempt: attemptOf(r.CampaignAttempt), RingTimeoutSec: int(r.RingTimeoutSec)}
	}
	return out, nil
}

// RecentOutcomesSince is the campaign's finished attempts since a time, newest
// first, up to limit.
func (d *DialerStore) RecentOutcomesSince(ctx context.Context, campaignID uuid.UUID,
	since time.Time, limit int) ([]campaign.Outcome, error) {
	rows, err := d.q.RecentAttemptOutcomesSince(ctx, queries.RecentAttemptOutcomesSinceParams{
		CampaignID: campaignID, Since: ts(since), RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]campaign.Outcome, 0, len(rows))
	for _, o := range rows {
		if o != nil {
			out = append(out, campaign.Outcome(*o))
		}
	}
	return out, nil
}

// Begin, SetJob, Finish, Void and FinishFromLedger are the attempt store's.
func (d *DialerStore) Begin(ctx context.Context, contactID uuid.UUID, now time.Time) (campaign.Attempt, error) {
	return d.attempts.Begin(ctx, contactID, now)
}

func (d *DialerStore) SetJob(ctx context.Context, callID uuid.UUID, jobID string) error {
	return d.attempts.SetJob(ctx, callID, jobID)
}

func (d *DialerStore) Finish(ctx context.Context, callID uuid.UUID, e campaign.Ending) (*campaign.Finished, error) {
	return d.attempts.Finish(ctx, callID, e)
}

func (d *DialerStore) Void(ctx context.Context, callID uuid.UUID, now time.Time) error {
	return d.attempts.Void(ctx, callID, now)
}

func (d *DialerStore) FinishFromLedger(ctx context.Context, callID uuid.UUID) (bool, error) {
	return d.attempts.FinishFromLedger(ctx, callID)
}

// FinishFromLedger finishes a DIALING attempt from its call's CDR, when one
// exists, by the same routine the CDR's own insert runs. It reports whether a
// CDR was found. An attempt that is no longer DIALING is left alone.
func (a *AttemptStore) FinishFromLedger(ctx context.Context, callID uuid.UUID) (bool, error) {
	var (
		found bool
		done  *campaign.Finished
	)
	err := txFor(ctx, a.pool, func(q *queries.Queries) error {
		att, err := q.LockAttemptByCall(ctx, callID)
		if errors.Is(err, pgx.ErrNoRows) {
			return campaign.ErrNoAttempt
		}
		if err != nil {
			return err
		}
		row, err := q.GetCDR(ctx, callID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if att.State != string(campaign.AttemptDialing) {
			return nil
		}
		done, err = a.finish.apply(ctx, q, att, endingFromCDR(fromRow(row)))
		return err
	})
	if err == nil && done != nil && a.onFinish != nil {
		a.onFinish(*done)
	}
	return found, err
}

// ListAttempts pages a campaign's attempts, newest first. f.PhoneNumber is
// canonical E.164 by now and is matched exactly against the number dialled.
func (c *CampaignStore) ListAttempts(ctx context.Context, campaignID uuid.UUID,
	f campaign.AttemptFilter) ([]campaign.Attempt, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := c.q.ListCampaignAttempts(ctx, queries.ListCampaignAttemptsParams{
		CampaignID: campaignID, ContactID: f.ContactID, Outcome: string(f.Outcome), IsTestNumber: f.IsTestNumber,
		NumberE164: f.PhoneNumber, PageLimit: int32(f.Limit), PageOffset: int32(f.Offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list attempts of %s: %w", campaignID, err)
	}
	total, err := c.q.CountCampaignAttempts(ctx, queries.CountCampaignAttemptsParams{
		CampaignID: campaignID, ContactID: f.ContactID, Outcome: string(f.Outcome), IsTestNumber: f.IsTestNumber,
		NumberE164: f.PhoneNumber,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count attempts of %s: %w", campaignID, err)
	}
	out := make([]campaign.Attempt, len(rows))
	for i, r := range rows {
		out[i] = attemptOf(r)
	}
	return out, total, nil
}
