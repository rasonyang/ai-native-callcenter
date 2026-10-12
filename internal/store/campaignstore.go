// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// CampaignStore keeps AI outbound campaigns and their contacts. It implements
// campaign.Repository; the meaning of what it stores lives in package campaign.
type CampaignStore struct {
	pool *pgxpool.Pool
	q    *queries.Queries
}

// Campaigns returns the campaign store.
func (s *Store) Campaigns() *CampaignStore {
	return &CampaignStore{pool: s.Pool, q: s.Queries}
}

var _ campaign.Repository = (*CampaignStore)(nil)

// DIDFlow looks a DID up with its published flow.
func (c *CampaignStore) DIDFlow(ctx context.Context, didID uuid.UUID) (campaign.DIDFlow, error) {
	did, err := c.q.GetDIDByID(ctx, didID)
	if errors.Is(err, pgx.ErrNoRows) {
		return campaign.DIDFlow{}, campaign.ErrDIDNotFound
	}
	if err != nil {
		return campaign.DIDFlow{}, fmt.Errorf("get did %s: %w", didID, err)
	}
	out := campaign.DIDFlow{ID: did.ID, Number: did.Number, IsEnabled: did.IsEnabled}
	if did.FlowID == nil {
		return out, nil
	}
	raw, err := c.q.GetPublishedSpec(ctx, *did.FlowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return campaign.DIDFlow{}, fmt.Errorf("get published flow %s: %w", *did.FlowID, err)
	}
	if out.Spec, err = flow.Load(raw); err != nil {
		return campaign.DIDFlow{}, fmt.Errorf("published flow %s: %w", *did.FlowID, err)
	}
	return out, nil
}

// Create stores a DRAFT campaign.
func (c *CampaignStore) Create(ctx context.Context, in campaign.Campaign) (campaign.Campaign, error) {
	window, err := json.Marshal(in.CallingWindow)
	if err != nil {
		return campaign.Campaign{}, err
	}
	policy, err := json.Marshal(in.RetryPolicy)
	if err != nil {
		return campaign.Campaign{}, err
	}
	id, err := c.q.InsertCampaign(ctx, queries.InsertCampaignParams{
		ID: in.ID, Name: in.Name, DidID: in.DIDID, TimeZone: in.TimeZone,
		CallingWindow: window, DefaultCountryCode: optionalText(in.DefaultCountryCode),
		MaxConcurrent: int32(in.MaxConcurrent), RingTimeoutSec: int32(in.RingTimeoutSec),
		RetryPolicy: policy, FollowUpQueueID: in.FollowUpQueueID,
	})
	if err != nil {
		return campaign.Campaign{}, mapCampaignWriteError(err)
	}
	return c.Get(ctx, id)
}

// Get returns one campaign with its contact counts.
func (c *CampaignStore) Get(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	row, err := c.q.GetCampaign(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return campaign.Campaign{}, campaign.NewNotFound()
	}
	if err != nil {
		return campaign.Campaign{}, fmt.Errorf("get campaign %s: %w", id, err)
	}
	out, err := campaignOf(row.Campaign, row.DidNumber)
	if err != nil {
		return campaign.Campaign{}, err
	}
	list := []campaign.Campaign{out}
	if err := c.fillCounts(ctx, list); err != nil {
		return campaign.Campaign{}, err
	}
	return list[0], nil
}

// List pages campaigns, newest first, with the total for the filter.
func (c *CampaignStore) List(ctx context.Context, f campaign.ListFilter) ([]campaign.Campaign, int64, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := c.q.ListCampaigns(ctx, queries.ListCampaignsParams{
		Status: string(f.Status), PageLimit: int32(f.Limit), PageOffset: int32(f.Offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list campaigns: %w", err)
	}
	total, err := c.q.CountCampaigns(ctx, string(f.Status))
	if err != nil {
		return nil, 0, fmt.Errorf("count campaigns: %w", err)
	}
	out := make([]campaign.Campaign, 0, len(rows))
	for _, row := range rows {
		item, err := campaignOf(row.Campaign, row.DidNumber)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	if err := c.fillCounts(ctx, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// fillCounts sets every campaign's contact counts with one query.
func (c *CampaignStore) fillCounts(ctx context.Context, list []campaign.Campaign) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(list))
	at := make(map[uuid.UUID]int, len(list))
	for i := range list {
		ids[i] = list[i].ID
		at[list[i].ID] = i
	}
	rows, err := c.q.CampaignContactCounts(ctx, ids)
	if err != nil {
		return fmt.Errorf("count campaign contacts: %w", err)
	}
	for _, r := range rows {
		counts := &list[at[r.CampaignID]].Contacts
		n := int(r.N)
		counts.Total += n
		switch campaign.ContactStatus(r.Status) {
		case campaign.ContactActive:
			counts.Active = n
		case campaign.ContactDone:
			counts.Done = n
		case campaign.ContactExhausted:
			counts.Exhausted = n
		case campaign.ContactInvalidNumber:
			counts.InvalidNumber = n
		case campaign.ContactDoNotCall:
			counts.DNC = n
		}
	}
	return nil
}

// Update reads the campaign under a row lock and stores what change returns.
func (c *CampaignStore) Update(ctx context.Context, id uuid.UUID,
	change func(campaign.Campaign) (campaign.Campaign, error)) (campaign.Campaign, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return campaign.Campaign{}, fmt.Errorf("update campaign %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := c.q.WithTx(tx)

	row, err := qtx.GetCampaignForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return campaign.Campaign{}, campaign.NewNotFound()
	}
	if err != nil {
		return campaign.Campaign{}, fmt.Errorf("lock campaign %s: %w", id, err)
	}
	cur, err := campaignOf(row.Campaign, row.DidNumber)
	if err != nil {
		return campaign.Campaign{}, err
	}
	next, err := change(cur)
	if err != nil {
		return campaign.Campaign{}, err
	}
	window, err := json.Marshal(next.CallingWindow)
	if err != nil {
		return campaign.Campaign{}, err
	}
	policy, err := json.Marshal(next.RetryPolicy)
	if err != nil {
		return campaign.Campaign{}, err
	}
	if _, err := qtx.UpdateCampaign(ctx, queries.UpdateCampaignParams{
		ID: id, Name: next.Name, DidID: next.DIDID, TimeZone: next.TimeZone,
		CallingWindow: window, DefaultCountryCode: optionalText(next.DefaultCountryCode),
		MaxConcurrent: int32(next.MaxConcurrent), RingTimeoutSec: int32(next.RingTimeoutSec),
		RetryPolicy: policy, FollowUpQueueID: next.FollowUpQueueID,
	}); err != nil {
		return campaign.Campaign{}, mapCampaignWriteError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return campaign.Campaign{}, fmt.Errorf("commit campaign %s: %w", id, err)
	}
	return c.Get(ctx, id)
}

// Delete removes a DRAFT campaign (its contacts go with it).
func (c *CampaignStore) Delete(ctx context.Context, id uuid.UUID) error {
	n, err := c.q.DeleteDraftCampaign(ctx, id)
	if err != nil {
		return fmt.Errorf("delete campaign %s: %w", id, err)
	}
	if n == 0 {
		return c.whyNotMoved(ctx, id, "DELETE")
	}
	return nil
}

// whyNotMoved explains a conditional update that changed nothing: there is no
// such campaign, or it was not in a status the move leaves.
func (c *CampaignStore) whyNotMoved(ctx context.Context, id uuid.UUID, action string) error {
	row, err := c.q.GetCampaign(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return campaign.NewNotFound()
	}
	if err != nil {
		return fmt.Errorf("read campaign %s: %w", id, err)
	}
	return campaign.NewTransitionError(campaign.Status(row.Campaign.Status), action)
}

// Start moves DRAFT to RUNNING and makes every waiting ACTIVE contact due.
func (c *CampaignStore) Start(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return c.move(ctx, id, "START", func(q *queries.Queries) (int64, error) {
		n, err := q.StartCampaign(ctx, id)
		if err != nil || n == 0 {
			return n, err
		}
		_, err = q.ArmCampaignContacts(ctx, id)
		return n, err
	})
}

// Pause moves RUNNING to PAUSED, reason MANUAL.
func (c *CampaignStore) Pause(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return c.move(ctx, id, "PAUSE", func(q *queries.Queries) (int64, error) { return q.PauseCampaign(ctx, id) })
}

// Resume moves PAUSED to RUNNING and clears the reason.
func (c *CampaignStore) Resume(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return c.move(ctx, id, "RESUME", func(q *queries.Queries) (int64, error) { return q.ResumeCampaign(ctx, id) })
}

// Cancel moves DRAFT, RUNNING or PAUSED to CANCELLED.
func (c *CampaignStore) Cancel(ctx context.Context, id uuid.UUID) (campaign.Campaign, error) {
	return c.move(ctx, id, "CANCEL", func(q *queries.Queries) (int64, error) { return q.CancelCampaign(ctx, id) })
}

// move runs one conditional transition in a transaction and returns the
// campaign as it now stands. The UPDATE's WHERE clause is the guard: of two
// racing callers the database lets exactly one through.
func (c *CampaignStore) move(ctx context.Context, id uuid.UUID, action string,
	apply func(*queries.Queries) (int64, error)) (campaign.Campaign, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return campaign.Campaign{}, fmt.Errorf("%s campaign %s: %w", action, id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n, err := apply(c.q.WithTx(tx))
	if err != nil {
		return campaign.Campaign{}, fmt.Errorf("%s campaign %s: %w", action, id, err)
	}
	if n == 0 {
		_ = tx.Rollback(ctx)
		return campaign.Campaign{}, c.whyNotMoved(ctx, id, action)
	}
	if err := tx.Commit(ctx); err != nil {
		return campaign.Campaign{}, fmt.Errorf("commit %s of campaign %s: %w", action, id, err)
	}
	return c.Get(ctx, id)
}

// importRow is a contact as the insert reads it.
type importRow struct {
	ID       uuid.UUID         `json:"id"`
	Number   string            `json:"number"`
	Raw      string            `json:"raw"`
	TimeZone string            `json:"timeZone"`
	Slots    map[string]string `json:"slots"`
}

// ImportContacts stores a batch in one transaction. The campaign is held
// shared while it runs, so a cancel cannot land between the status check and
// the inserts; a number the campaign already holds is left exactly as it is.
func (c *CampaignStore) ImportContacts(ctx context.Context, campaignID uuid.UUID,
	contacts []campaign.NewContact) ([]campaign.Stored, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("import into campaign %s: %w", campaignID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := c.q.WithTx(tx)

	status, err := qtx.LockCampaignStatus(ctx, campaignID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, campaign.NewNotFound()
	}
	if err != nil {
		return nil, fmt.Errorf("lock campaign %s: %w", campaignID, err)
	}
	st := campaign.Status(status)
	if st.IsTerminal() {
		return nil, campaign.NewTransitionError(st, "IMPORT_CONTACTS")
	}

	rows := make([]importRow, len(contacts))
	for i, ct := range contacts {
		slots := ct.Slots
		if slots == nil {
			slots = map[string]string{}
		}
		rows[i] = importRow{ID: ct.ID, Number: ct.Number, Raw: ct.NumberRaw, TimeZone: ct.TimeZone, Slots: slots}
	}
	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	inserted, err := qtx.InsertCampaignContacts(ctx, queries.InsertCampaignContactsParams{
		CampaignID: campaignID, IsArmed: st != campaign.StatusDraft, Rows: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("insert contacts of campaign %s: %w", campaignID, err)
	}
	isNew := make(map[string]uuid.UUID, len(inserted))
	for _, r := range inserted {
		isNew[r.NumberE164] = r.ID
	}
	var held []string
	for _, ct := range contacts {
		if _, ok := isNew[ct.Number]; !ok {
			held = append(held, ct.Number)
		}
	}
	existing := map[string]uuid.UUID{}
	if len(held) > 0 {
		found, err := qtx.ExistingCampaignContacts(ctx, queries.ExistingCampaignContactsParams{
			CampaignID: campaignID, Numbers: held,
		})
		if err != nil {
			return nil, fmt.Errorf("read existing contacts of campaign %s: %w", campaignID, err)
		}
		for _, r := range found {
			existing[r.NumberE164] = r.ID
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit import into campaign %s: %w", campaignID, err)
	}

	out := make([]campaign.Stored, len(contacts))
	for i, ct := range contacts {
		if id, ok := isNew[ct.Number]; ok {
			out[i] = campaign.Stored{ID: id, IsNew: true}
			continue
		}
		id, ok := existing[ct.Number]
		if !ok {
			return nil, fmt.Errorf("contact %s of campaign %s is neither new nor held", ct.Number, campaignID)
		}
		out[i] = campaign.Stored{ID: id}
	}
	return out, nil
}

// ListContacts pages a campaign's contacts in import order.
func (c *CampaignStore) ListContacts(ctx context.Context, campaignID uuid.UUID,
	f campaign.ContactFilter) ([]campaign.Contact, int64, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 50
	}
	rows, err := c.q.ListCampaignContacts(ctx, queries.ListCampaignContactsParams{
		CampaignID: campaignID, Status: string(f.Status), LeadIntent: string(f.LeadIntent),
		PageLimit: int32(f.Limit), PageOffset: int32(f.Offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list contacts of campaign %s: %w", campaignID, err)
	}
	total, err := c.q.CountCampaignContacts(ctx, queries.CountCampaignContactsParams{
		CampaignID: campaignID, Status: string(f.Status), LeadIntent: string(f.LeadIntent),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count contacts of campaign %s: %w", campaignID, err)
	}
	out := make([]campaign.Contact, 0, len(rows))
	for _, r := range rows {
		ct, err := contactOf(r)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, ct)
	}
	return out, total, nil
}

func campaignOf(r queries.Campaign, didNumber string) (campaign.Campaign, error) {
	out := campaign.Campaign{
		ID: r.ID, Name: r.Name, DIDID: r.DidID, DIDNumber: didNumber,
		Status: campaign.Status(r.Status), TimeZone: r.TimeZone,
		MaxConcurrent: int(r.MaxConcurrent), RingTimeoutSec: int(r.RingTimeoutSec),
		FollowUpQueueID: r.FollowUpQueueID,
		CreatedAt:       r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		StartedAt: timeOrNil(r.StartedAt), FinishedAt: timeOrNil(r.FinishedAt),
	}
	if r.PauseReason != nil {
		out.PauseReason = campaign.PauseReason(*r.PauseReason)
	}
	if r.DefaultCountryCode != nil {
		out.DefaultCountryCode = *r.DefaultCountryCode
	}
	if err := json.Unmarshal(r.CallingWindow, &out.CallingWindow); err != nil {
		return campaign.Campaign{}, fmt.Errorf("campaign %s: calling window: %w", r.ID, err)
	}
	if err := json.Unmarshal(r.RetryPolicy, &out.RetryPolicy); err != nil {
		return campaign.Campaign{}, fmt.Errorf("campaign %s: retry policy: %w", r.ID, err)
	}
	return out, nil
}

func contactOf(r queries.CampaignContact) (campaign.Contact, error) {
	out := campaign.Contact{
		ID: r.ID, CampaignID: r.CampaignID, Number: r.NumberE164, NumberRaw: r.NumberRaw,
		Status: campaign.ContactStatus(r.Status), Attempts: int(r.Attempts),
		SystemFailures: int(r.SystemFailures), NextAttemptAt: timeOrNil(r.NextAttemptAt),
		CreatedAt: r.CreatedAt.Time, Slots: map[string]string{},
	}
	if r.TimeZone != nil {
		out.TimeZone = *r.TimeZone
	}
	if r.LastOutcome != nil {
		out.LastOutcome = campaign.Outcome(*r.LastOutcome)
	}
	if r.LeadIntent != nil {
		out.LeadIntent = campaign.LeadIntent(*r.LeadIntent)
	}
	if r.LeadNotes != nil {
		out.LeadNotes = *r.LeadNotes
	}
	if err := json.Unmarshal(r.Slots, &out.Slots); err != nil {
		return campaign.Contact{}, fmt.Errorf("contact %s: slots: %w", r.ID, err)
	}
	return out, nil
}

func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// mapCampaignWriteError turns the two foreign-key failures a write can race
// into the domain's refusals.
func mapCampaignWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		switch pgErr.ConstraintName {
		case "fk_campaigns_queues":
			return campaign.ErrQueueNotFound
		case "fk_campaigns_dids":
			return campaign.ErrDIDNotFound
		}
	}
	return fmt.Errorf("write campaign: %w", err)
}

// errFlowUsedByCampaign is the refusal to give a flow, or a DID, a way to a
// person while a campaign that has not finished depends on it. It is a
// campaign.ErrFlowHasTransfer.
func errFlowUsedByCampaign(rule, message string, campaigns int64) error {
	return &campaign.Error{
		Kind: campaign.ErrFlowHasTransfer, Rule: rule, Message: message,
		Params: map[string]any{"campaigns": campaigns},
	}
}

// refuseTransferWhileCampaigned is the guard behind both entry points: a spec
// that can transfer must not become the flow a live campaign dials with.
func refuseTransferWhileCampaigned(spec *flow.Spec, live func() (int64, error), rule, message string) error {
	if !flow.CanTransfer(spec) {
		return nil
	}
	n, err := live()
	if err != nil {
		return fmt.Errorf("count live campaigns: %w", err)
	}
	if n > 0 {
		return errFlowUsedByCampaign(rule, message, n)
	}
	return nil
}
