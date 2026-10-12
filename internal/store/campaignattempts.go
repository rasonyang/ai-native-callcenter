// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// dialingNumberIndex is the partial unique index that allows one attempt in
// flight per number, across every campaign.
const dialingNumberIndex = "uq_campaign_attempts_dialing_number_e164"

// attemptFinisher is the finish routine of design 10 section 5.4. It runs
// inside a transaction somebody else owns (the CDR's, or its own), and says
// what it did so the owner can announce it once the transaction holds.
type attemptFinisher struct {
	// outer is the deployment's outer calling hours (AICC_OUTBOUND_HOURS); the
	// zero Range means none.
	outer dayrange.Range
	now   func() time.Time
}

func (s *Store) finisher() attemptFinisher {
	return attemptFinisher{outer: s.OutboundHours, now: time.Now}
}

// AttemptStore keeps the attempts of campaigns: the write-ahead row, the
// endings that have no CDR, and the reads the dialer's breakers make.
type AttemptStore struct {
	pool     *pgxpool.Pool
	q        *queries.Queries
	finish   attemptFinisher
	onFinish func(campaign.Finished)
}

// Attempts returns the attempt store.
func (s *Store) Attempts() *AttemptStore {
	return &AttemptStore{pool: s.Pool, q: s.Queries, finish: s.finisher(), onFinish: s.OnAttemptFinished}
}

// Begin is the write-ahead step (design 10 section 5.3): in one transaction it
// mints the call and leg ids, inserts the attempt as DIALING and takes the
// contact out of the due pool. Only after it returns may the call be placed.
//
// It returns ErrNumberInFlight when another attempt, in any campaign, is
// dialling the number, and ErrContactNotDialable when the contact is gone or
// no longer ACTIVE; in both cases nothing was written.
func (a *AttemptStore) Begin(ctx context.Context, contactID uuid.UUID, now time.Time) (campaign.Attempt, error) {
	var out campaign.Attempt
	err := txFor(ctx, a.pool, func(q *queries.Queries) error {
		claimed, err := q.ClaimContactForDial(ctx, contactID)
		if errors.Is(err, pgx.ErrNoRows) {
			return campaign.ErrContactNotDialable
		}
		if err != nil {
			return fmt.Errorf("claim contact: %w", err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		row, err := q.InsertCampaignAttempt(ctx, queries.InsertCampaignAttemptParams{
			ID: id, CampaignID: claimed.CampaignID, CampaignContactID: contactID,
			NumberE164: claimed.NumberE164, CallID: uuid.New(), LegID: uuid.New(),
			StartedAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == dialingNumberIndex {
			return campaign.ErrNumberInFlight
		}
		if err != nil {
			return fmt.Errorf("insert attempt: %w", err)
		}
		out = attemptOf(row)
		return nil
	})
	return out, err
}

// SetJob records the Job-UUID the originate's bgapi returned.
func (a *AttemptStore) SetJob(ctx context.Context, callID uuid.UUID, jobID string) error {
	_, err := a.q.SetAttemptJob(ctx, queries.SetAttemptJobParams{CallID: callID, OriginateJobID: &jobID})
	return err
}

// SetLead records the lead a call concluded so far. The last write wins; its
// side effects run once, when the attempt finishes. It reports false when the
// attempt is not in flight (gone, or already finished).
func (a *AttemptStore) SetLead(ctx context.Context, callID uuid.UUID, intent campaign.LeadIntent, notes string) (bool, error) {
	if !intent.IsValid() {
		return false, fmt.Errorf("campaign: %q is not a lead intent", intent)
	}
	n, err := a.q.SetAttemptLead(ctx, queries.SetAttemptLeadParams{
		CallID: callID, LeadIntent: textPtr(string(intent)), LeadNotes: &notes,
	})
	return n > 0, err
}

// Get returns the attempt that carries this call id.
func (a *AttemptStore) Get(ctx context.Context, callID uuid.UUID) (campaign.Attempt, error) {
	row, err := a.q.GetAttemptByCall(ctx, callID)
	if errors.Is(err, pgx.ErrNoRows) {
		return campaign.Attempt{}, campaign.ErrNoAttempt
	}
	if err != nil {
		return campaign.Attempt{}, err
	}
	return attemptOf(row), nil
}

// Finish ends an attempt that has no CDR: a synchronous dial error, an
// originate that never made a channel, a restart's reconciliation. It runs the
// finish routine in a transaction of its own, then announces it.
//
// An attempt that is no longer DIALING is left alone and (nil, nil) comes
// back: a CDR got there first, and the ledger knows more than a caller that
// had to guess.
func (a *AttemptStore) Finish(ctx context.Context, callID uuid.UUID, e campaign.Ending) (*campaign.Finished, error) {
	var done *campaign.Finished
	err := txFor(ctx, a.pool, func(q *queries.Queries) error {
		att, err := q.LockAttemptByCall(ctx, callID)
		if errors.Is(err, pgx.ErrNoRows) {
			return campaign.ErrNoAttempt
		}
		if err != nil {
			return err
		}
		if att.State != string(campaign.AttemptDialing) {
			return nil
		}
		done, err = a.finish.apply(ctx, q, att, e)
		return err
	})
	if err == nil && done != nil && a.onFinish != nil {
		a.onFinish(*done)
	}
	return done, err
}

// Void deletes an attempt that was never dialled (the DID's line limit was
// reached): nothing is counted and the contact is due again at once.
func (a *AttemptStore) Void(ctx context.Context, callID uuid.UUID, now time.Time) error {
	return txFor(ctx, a.pool, func(q *queries.Queries) error {
		contactID, err := q.DeleteDialingAttempt(ctx, callID)
		if errors.Is(err, pgx.ErrNoRows) {
			return campaign.ErrNoAttempt
		}
		if err != nil {
			return err
		}
		_, err = q.MakeContactDue(ctx, queries.MakeContactDueParams{
			ID: contactID, NextAttemptAt: pgtype.Timestamptz{Time: now, Valid: true},
		})
		return err
	})
}

// RecentOutcomes is the campaign's finished attempts, newest first, up to
// limit: the input of the breakers.
func (a *AttemptStore) RecentOutcomes(ctx context.Context, campaignID uuid.UUID, limit int) ([]campaign.Outcome, error) {
	rows, err := a.q.RecentAttemptOutcomes(ctx, queries.RecentAttemptOutcomesParams{
		CampaignID: campaignID, RowLimit: int32(limit),
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

// fromCDR runs the finish routine for a CDR that was just written, when an
// attempt carries its call id. It is the caller's transaction.
func (f attemptFinisher) fromCDR(ctx context.Context, q *queries.Queries, cdr CDR) (*campaign.Finished, error) {
	att, err := q.LockAttemptByCall(ctx, cdr.CallID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock attempt: %w", err)
	}
	return f.apply(ctx, q, att, endingFromCDR(cdr))
}

// endingFromCDR reads how a campaign call went off the ledger row.
func endingFromCDR(cdr CDR) campaign.Ending {
	end := campaign.CallEnd{
		HangupCause: cdr.HangupCause,
		IsAnswered:  !cdr.AnsweredAt.IsZero() || cdr.Status == CDRStatusAnswered,
		IsFailed:    cdr.Status == CDRStatusFailed,
		HasBot:      cdr.BotSec > 0,
	}
	for _, leg := range cdr.Legs {
		if leg.Kind == "BOT" {
			end.HasBot = true
		}
	}
	if v, ok := cdr.Tech["isPlacementFailure"].(bool); ok {
		end.IsPlacementFailure = v
	}
	// A campaign call never reaches an agent (design 10 section 10), so the
	// CDR's TalkSec (agent talk time) is zero; the customer's conversation
	// is the time with the bot, BotSec. Summing both keeps the figure right
	// should a call ever carry agent talk time too.
	return campaign.Ending{
		Outcome:     campaign.Classify(end),
		HangupCause: cdr.HangupCause,
		TalkSec:     cdr.BotSec + cdr.TalkSec,
		EndedAt:     cdr.EndedAt,
	}
}

// apply is the finish routine (design 10 section 5.4) on a locked attempt.
//
// Idempotency. The attempt row is the ledger of what this call has cost its
// contact: its outcome says which budget it was charged to. Finishing it a
// second time (a fuller CDR replaces the first) first hands that cost back,
// then charges the new outcome, so a call is counted once however many
// revisions it goes through; and a revision that changes nothing writes
// nothing and announces nothing. Everything runs under the locks on the
// attempt and the contact, so two writers for one call take turns.
//
// The contact's fate (status, next_attempt_at, last_outcome) follows only its
// latest attempt: a revision of an older one moves the counters and nothing
// else. A contact that is on the do-not-call list stays there.
func (f attemptFinisher) apply(ctx context.Context, q *queries.Queries, att queries.CampaignAttempt, e campaign.Ending) (*campaign.Finished, error) {
	if !e.Outcome.IsValid() {
		return nil, fmt.Errorf("campaign: %q is not an outcome", e.Outcome)
	}
	isRevision := att.State == string(campaign.AttemptFinished)
	if isRevision && att.Outcome != nil && campaign.Outcome(*att.Outcome) == e.Outcome &&
		deref(att.HangupCause) == e.HangupCause && int(derefInt(att.TalkSec)) == e.TalkSec {
		return nil, nil
	}
	endedAt := e.EndedAt
	if endedAt.IsZero() {
		endedAt = f.now()
	}

	contact, err := q.LockCampaignContact(ctx, att.CampaignContactID)
	if err != nil {
		return nil, fmt.Errorf("lock contact: %w", err)
	}
	camRow, err := q.GetCampaign(ctx, att.CampaignID)
	if err != nil {
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	cam, err := campaignOf(camRow.Campaign, camRow.DidNumber)
	if err != nil {
		return nil, err
	}

	counters := campaign.Counters{Attempts: int(contact.Attempts), SystemFailures: int(contact.SystemFailures)}
	if isRevision && att.Outcome != nil {
		counters = counters.Remove(campaign.Outcome(*att.Outcome))
	}
	counters = counters.Add(e.Outcome)

	talk := int32(e.TalkSec)
	cause := textPtr(e.HangupCause)
	if err := q.FinishCampaignAttempt(ctx, queries.FinishCampaignAttemptParams{
		ID: att.ID, Outcome: textPtr(string(e.Outcome)), HangupCause: cause, TalkSec: &talk,
		IsTouched: campaign.IsTouched(e.Outcome), FinishedAt: pgtype.Timestamptz{Time: endedAt, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("finish attempt: %w", err)
	}

	lead := campaign.LeadIntent(deref(att.LeadIntent))
	if err := leadSideEffects(ctx, q, att, cam, lead); err != nil {
		return nil, err
	}

	isLatest, err := q.AttemptIsLatest(ctx, att.ID)
	if err != nil {
		return nil, fmt.Errorf("is latest attempt: %w", err)
	}
	done := &campaign.Finished{
		CampaignID: att.CampaignID, ContactID: att.CampaignContactID, AttemptID: att.ID,
		CallID: att.CallID, Outcome: e.Outcome, IsRevision: isRevision,
		ContactStatus: campaign.ContactStatus(contact.Status), NextAttemptAt: timeOrNil(contact.NextAttemptAt),
	}
	if isLatest != nil && !*isLatest {
		done.NextAttemptAt = timeOrNil(contact.NextAttemptAt)
		return done, q.AdjustCampaignContactCounters(ctx, queries.AdjustCampaignContactCountersParams{
			ID: contact.ID, Attempts: int32(counters.Attempts), SystemFailures: int32(counters.SystemFailures),
		})
	}

	sched, err := campaign.NewSchedule(cam.TimeZone, deref(contact.TimeZone), cam.CallingWindow, f.outer)
	if err != nil {
		// A zone the validation already accepted cannot fail to load; if it
		// does, no window is applied rather than no retry being scheduled.
		slog.Warn("campaign schedule could not be built; the retry ignores the calling window",
			"campaignId", cam.ID, "contactId", contact.ID, "error", err)
		sched = campaign.Schedule{}
	}
	settled := campaign.Settle(cam.RetryPolicy, counters, e.Outcome, lead, endedAt, sched)
	if contact.Status == string(campaign.ContactDoNotCall) {
		settled = campaign.Settlement{Status: campaign.ContactDoNotCall}
	}

	var leadIntent, leadNotes *string
	if lead != "" {
		leadIntent = textPtr(string(lead))
		notes := deref(att.LeadNotes)
		leadNotes = &notes
	}
	var next pgtype.Timestamptz
	if settled.NextAttemptAt != nil {
		next = pgtype.Timestamptz{Time: *settled.NextAttemptAt, Valid: true}
	}
	if err := q.SettleCampaignContact(ctx, queries.SettleCampaignContactParams{
		ID: contact.ID, Attempts: int32(counters.Attempts), SystemFailures: int32(counters.SystemFailures),
		Status: string(settled.Status), NextAttemptAt: next, LastOutcome: textPtr(string(e.Outcome)),
		LeadIntent: leadIntent, LeadNotes: leadNotes,
	}); err != nil {
		return nil, fmt.Errorf("settle contact: %w", err)
	}
	done.ContactStatus = settled.Status
	done.NextAttemptAt = settled.NextAttemptAt
	return done, nil
}

// leadSideEffects applies what the call concluded: a DO_NOT_CALL goes on the
// list, a HIGH lead files one callback. Both are idempotent, because a revised
// CDR runs the routine again for the same attempt.
func leadSideEffects(ctx context.Context, q *queries.Queries, att queries.CampaignAttempt, cam campaign.Campaign, lead campaign.LeadIntent) error {
	switch lead {
	case campaign.LeadDoNotCall:
		callID := att.CallID
		if _, err := q.InsertDNCNumber(ctx, queries.InsertDNCNumberParams{
			NumberE164: att.NumberE164, Source: DNCSourceConversation, CallID: &callID,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("file do-not-call: %w", err)
		}
	case campaign.LeadHigh:
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := q.InsertCallbackForCall(ctx, queries.InsertCallbackForCallParams{
			ID: id, CallID: att.CallID, QueueID: cam.FollowUpQueueID,
			PhoneNumber: att.NumberE164, Message: deref(att.LeadNotes),
		}); err != nil {
			return fmt.Errorf("file callback: %w", err)
		}
	}
	return nil
}

func attemptOf(r queries.CampaignAttempt) campaign.Attempt {
	return campaign.Attempt{
		ID: r.ID, CampaignID: r.CampaignID, ContactID: r.CampaignContactID, Number: r.NumberE164,
		CallID: r.CallID, LegID: r.LegID, OriginateJobID: deref(r.OriginateJobID),
		State: campaign.AttemptState(r.State), Outcome: campaign.Outcome(deref(r.Outcome)),
		HangupCause: deref(r.HangupCause), TalkSec: int(derefInt(r.TalkSec)), IsTouched: r.IsTouched,
		LeadIntent: campaign.LeadIntent(deref(r.LeadIntent)), LeadNotes: deref(r.LeadNotes),
		StartedAt: r.StartedAt.Time, FinishedAt: timeOrNil(r.FinishedAt),
	}
}

func textPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefInt(n *int32) int32 {
	if n == nil {
		return 0
	}
	return *n
}
