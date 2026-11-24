// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// LeadJudgeStore is what the judge of campaign leads reads and writes
// (internal/leadjudge, design 10 section 11.1). The attempt rows are its
// queue: a CDR marks a judgement owed (attemptFinisher.fromCDR), a verdict or
// the deadline settles it.
type LeadJudgeStore struct {
	pool *pgxpool.Pool
	q    *queries.Queries
}

// LeadJudgements returns the store the lead judge works through.
func (s *Store) LeadJudgements() *LeadJudgeStore {
	return &LeadJudgeStore{pool: s.Pool, q: s.Queries}
}

// LeadJudgement is one owed (or settled) judgement and what it reads besides
// the transcript.
type LeadJudgement struct {
	CallID uuid.UUID
	// Verdict is PENDING while the judgement is owed; empty when none was.
	Verdict campaign.LeadVerdict
	// DeclaredLeadIntent and LeadNotes are the lead the flow recorded: the
	// intent of the phase it reached, or of the model's record_lead, and its
	// rendered notes.
	DeclaredLeadIntent campaign.LeadIntent
	LeadNotes          string
	// Language is the call's, from its CDR.
	Language string
	// DeadlineAt is when a verdict stops being accepted.
	DeadlineAt time.Time
}

// LeadJudgement reads the judgement of one call. campaign.ErrNoAttempt when
// no attempt carries the call.
func (j *LeadJudgeStore) LeadJudgement(ctx context.Context, callID uuid.UUID) (LeadJudgement, error) {
	att, err := j.q.GetAttemptByCall(ctx, callID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadJudgement{}, campaign.ErrNoAttempt
	}
	if err != nil {
		return LeadJudgement{}, err
	}
	out := LeadJudgement{
		CallID:             callID,
		Verdict:            campaign.LeadVerdict(deref(att.LeadVerdict)),
		DeclaredLeadIntent: campaign.LeadIntent(deref(att.DeclaredLeadIntent)),
		LeadNotes:          deref(att.LeadNotes),
		DeadlineAt:         att.LeadJudgeDeadlineAt.Time,
	}
	row, err := j.q.GetCDR(ctx, callID)
	switch {
	case err == nil:
		out.Language = row.Language
	case !errors.Is(err, pgx.ErrNoRows):
		return LeadJudgement{}, err
	}
	return out, nil
}

// ListTranscript reads a call's transcript in order.
func (j *LeadJudgeStore) ListTranscript(ctx context.Context, callID uuid.UUID) ([]TranscriptLine, error) {
	rows, err := j.q.ListTranscripts(ctx, callID)
	if err != nil {
		return nil, err
	}
	return transcriptLines(rows), nil
}

// PendingLeadJudgement is an owed judgement still in time. It became owed
// campaign.LeadJudgeDeadline before DeadlineAt, when the call's CDR was
// written.
type PendingLeadJudgement struct {
	CallID     uuid.UUID
	DeadlineAt time.Time
}

// PendingLeadJudgements lists up to limit owed judgements still in time,
// soonest deadline first.
func (j *LeadJudgeStore) PendingLeadJudgements(ctx context.Context, limit int) ([]PendingLeadJudgement, error) {
	rows, err := j.q.PendingLeadJudgements(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	out := make([]PendingLeadJudgement, 0, len(rows))
	for _, r := range rows {
		out = append(out, PendingLeadJudgement{CallID: r.CallID, DeadlineAt: r.LeadJudgeDeadlineAt.Time})
	}
	return out, nil
}

// ApplyLeadVerdict settles an owed judgement with a verdict, in one
// transaction: the verdict and its reason, the effective lead it leaves
// (campaign.LeadAfterVerdict), the contact's lead while this attempt is still
// its latest, the callback a lead still HIGH files, and the call's webhook
// deliveries rebuilt from the rows and released.
//
// It applies only while the judgement is PENDING and its deadline (the
// database's clock) has not passed. applied is false, and nothing is written,
// when it is too late, already settled, or there is no such attempt: a late
// verdict is discarded so the record agrees with what the receiver was sent.
func (j *LeadJudgeStore) ApplyLeadVerdict(ctx context.Context, callID uuid.UUID, verdict campaign.LeadVerdict,
	reason string) (lead campaign.LeadIntent, applied bool, err error) {
	if !verdict.IsFinal() {
		return "", false, fmt.Errorf("campaign: %q is not a verdict", verdict)
	}
	err = txFor(ctx, j.pool, func(q *queries.Queries) error {
		att, err := q.LockAttemptByCall(ctx, callID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock attempt: %w", err)
		}
		if campaign.LeadVerdict(deref(att.LeadVerdict)) != campaign.VerdictPending {
			return nil
		}
		declared := campaign.LeadIntent(deref(att.DeclaredLeadIntent))
		after := campaign.LeadAfterVerdict(declared, verdict)
		row, err := q.SettleLeadVerdict(ctx, queries.SettleLeadVerdictParams{
			ID: att.ID, LeadVerdict: textPtr(string(verdict)), LeadVerdictReason: textPtr(reason),
			LeadIntent: textPtr(string(after)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("settle the verdict: %w", err)
		}
		if err := settleJudgedLead(ctx, q, row, declared); err != nil {
			return err
		}
		if err := releaseJudgedDeliveries(ctx, q, row); err != nil {
			return err
		}
		lead, applied = after, true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return lead, applied, nil
}

// ExpireLeadJudgements settles up to limit owed judgements whose deadline has
// passed as NOT_JUDGED: the declared lead stands and a HIGH one files its
// callback. Their webhook deliveries are left as they are: queued while the
// verdict was awaited, they already say NOT_JUDGED, and go out by themselves
// when their hold ends. It returns the calls it settled.
func (j *LeadJudgeStore) ExpireLeadJudgements(ctx context.Context, limit int) ([]uuid.UUID, error) {
	ids, err := j.q.ExpiredLeadJudgements(ctx, int32(limit))
	if err != nil {
		return nil, err
	}
	var settled []uuid.UUID
	for _, callID := range ids {
		var ok bool
		err := txFor(ctx, j.pool, func(q *queries.Queries) error {
			row, err := q.ExpireLeadJudgement(ctx, callID)
			if errors.Is(err, pgx.ErrNoRows) {
				// A verdict, or another sweep, got there first.
				return nil
			}
			if err != nil {
				return fmt.Errorf("expire the judgement: %w", err)
			}
			ok = true
			return settleJudgedLead(ctx, q, row, campaign.LeadIntent(deref(row.DeclaredLeadIntent)))
		})
		if err != nil {
			return settled, err
		}
		if ok {
			settled = append(settled, callID)
		}
	}
	return settled, nil
}

// settleJudgedLead applies a settled judgement beyond the attempt row, in the
// caller's transaction: the contact's lead when it moved and this attempt is
// still the contact's latest, and the callback of a lead that is still HIGH
// (the one leadSideEffects held back while the judgement was owed). Retry and
// redial state are not recomputed: the call was answered.
func settleJudgedLead(ctx context.Context, q *queries.Queries, row queries.CampaignAttempt, declared campaign.LeadIntent) error {
	lead := campaign.LeadIntent(deref(row.LeadIntent))
	if lead != declared {
		if _, err := q.SetContactLeadOfAttempt(ctx, queries.SetContactLeadOfAttemptParams{
			ID: row.CampaignContactID, LeadIntent: textPtr(string(lead)), AttemptID: row.ID,
		}); err != nil {
			return fmt.Errorf("move the contact's lead: %w", err)
		}
	}
	if lead != campaign.LeadHigh {
		return nil
	}
	camRow, err := q.GetCampaign(ctx, row.CampaignID)
	if err != nil {
		return fmt.Errorf("get campaign: %w", err)
	}
	cam, err := campaignOf(camRow.Campaign, camRow.DidNumber)
	if err != nil {
		return err
	}
	return leadSideEffects(ctx, q, row, cam, lead)
}

// releaseJudgedDeliveries rebuilds the call's webhook payload from the ledger
// row and the settled attempt and queues it due now: a delivery still held
// unattempted has its payload replaced, as a fuller CDR's would be, and goes
// out at once with the final lead.
func releaseJudgedDeliveries(ctx context.Context, q *queries.Queries, row queries.CampaignAttempt) error {
	cdrRow, err := q.GetCDR(ctx, row.CallID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the CDR: %w", err)
	}
	cdr := fromRow(cdrRow)
	campaignID := row.CampaignID
	cdr.CampaignID = &campaignID
	setCDRLead(&cdr, row.LeadIntent, row.DeclaredLeadIntent, row.LeadVerdict, row.LeadVerdictReason)
	return enqueueFor(ctx, q, deliveredCDR(cdr), nil)
}
