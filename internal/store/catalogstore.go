// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// CatalogStore adapts the generated queries to the catalogue service.
//
// It holds the pool as well as the query set because allocating an extension
// number is one transaction: the pool lock, the search for a free number and
// the insert have to be the same unit of work, or the number is stale before
// it is used.
type CatalogStore struct {
	q    *queries.Queries
	pool *pgxpool.Pool
}

// Catalog returns the configuration view of the store.
func (s *Store) Catalog() *CatalogStore { return &CatalogStore{q: s.Queries, pool: s.Pool} }

// AllocateExtension creates an extension on the lowest free number in the
// pool, in one transaction.
//
// One transaction, because a number handed back to a caller is already stale:
// between the search and the insert another allocation can take it. The pool
// lock makes the search and the insert atomic against each other, so parallel
// callers queue instead of colliding on uq_extensions_number — where the
// symptom would be a failed request rather than a duplicate row.
//
// The lowest free number rather than MAX+1: a number a departing agent gave
// back is handed out again. That is safe here and only here — no historical
// row is keyed by an extension, every one names the agent's uuid, and a uuid
// is never reused (verification F12).
func (c *CatalogStore) AllocateExtension(ctx context.Context, e catalog.Extension,
	rangeLow, rangeHigh int) (catalog.Extension, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return catalog.Extension{}, fmt.Errorf("allocate extension: %w", err)
	}
	// Safe after Commit: pgx answers ErrTxClosed, which nothing acts on.
	defer func() { _ = tx.Rollback(ctx) }()

	row, err := allocateExtensionTx(ctx, c.q.WithTx(tx), e, rangeLow, rangeHigh)
	if err != nil {
		return catalog.Extension{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return catalog.Extension{}, fmt.Errorf("commit extension %s: %w", row.Number, err)
	}
	return extensionOf(row), nil
}

// allocateExtensionTx is the allocation itself, on a transaction somebody else
// owns: the pool lock, the search and the insert.
//
// Shared rather than copied, because provisioning an account allocates a phone
// inside a larger transaction. Two copies of a lock discipline is how one of
// them quietly loses it.
func allocateExtensionTx(ctx context.Context, qtx *queries.Queries,
	e catalog.Extension, rangeLow, rangeHigh int) (queries.Extension, error) {
	if err := qtx.LockExtensionPool(ctx, extensionPoolLockKey); err != nil {
		return queries.Extension{}, fmt.Errorf("lock extension pool: %w", err)
	}
	number, err := qtx.LowestFreeExtensionNumber(ctx, queries.LowestFreeExtensionNumberParams{
		RangeLow:  int32(rangeLow),
		RangeHigh: int32(rangeHigh),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return queries.Extension{}, catalog.ErrPoolExhausted
		}
		return queries.Extension{}, fmt.Errorf("find a free extension number: %w", err)
	}
	e.Number = number
	e.NameAfterNumber()

	row, err := qtx.CreateExtension(ctx, queries.CreateExtensionParams{
		ID:          e.ID,
		Number:      e.Number,
		Password:    e.Password,
		DisplayName: e.DisplayName,
		IsEnabled:   e.IsEnabled,
	})
	if err != nil {
		return queries.Extension{}, fmt.Errorf("create extension %s: %w", e.Number, err)
	}
	return row, nil
}

//
// Extensions.
//

func (c *CatalogStore) ListExtensions(ctx context.Context) ([]catalog.Extension, error) {
	rows, err := c.q.ListExtensions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
	}
	out := make([]catalog.Extension, 0, len(rows))
	for _, r := range rows {
		e := extensionOf(queries.Extension{
			ID: r.ID, Number: r.Number, Password: r.Password,
			DisplayName: r.DisplayName, IsEnabled: r.IsEnabled,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
		// Joined rather than stored: the binding belongs to the agent, and a
		// second copy here would be a second thing to keep true.
		e.AgentID = r.AgentID
		out = append(out, e)
	}
	return out, nil
}

func (c *CatalogStore) CreateExtension(ctx context.Context, e catalog.Extension) (catalog.Extension, error) {
	row, err := c.q.CreateExtension(ctx, queries.CreateExtensionParams{
		ID:          e.ID,
		Number:      e.Number,
		Password:    e.Password,
		DisplayName: e.DisplayName,
		IsEnabled:   e.IsEnabled,
	})
	if err != nil {
		return catalog.Extension{}, fmt.Errorf("create extension: %w", err)
	}
	return extensionOf(row), nil
}

// ExtensionPassword reads the stored credential.
func (c *CatalogStore) ExtensionPassword(ctx context.Context, id uuid.UUID) (string, error) {
	row, err := c.q.GetExtension(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", catalog.ErrNotFound
		}
		return "", fmt.Errorf("read extension %s: %w", id, err)
	}
	return row.Password, nil
}

func (c *CatalogStore) UpdateExtension(ctx context.Context, e catalog.Extension) (catalog.Extension, error) {
	row, err := c.q.UpdateExtension(ctx, queries.UpdateExtensionParams{
		ID:          e.ID,
		DisplayName: e.DisplayName,
		IsEnabled:   e.IsEnabled,
	})
	if err != nil {
		return catalog.Extension{}, fmt.Errorf("update extension: %w", err)
	}
	return extensionOf(row), nil
}

func (c *CatalogStore) SetExtensionPassword(ctx context.Context, id uuid.UUID, password string) error {
	return c.q.UpdateExtensionPassword(ctx, queries.UpdateExtensionPasswordParams{ID: id, Password: password})
}

// deleted turns a delete's row count into an answer to the question the API
// asks: was there anything there? DELETE removing nothing is not an error to
// PostgreSQL, so without this every delete reported success and the 404 the
// contract declares was unreachable — an operator who mistyped an id was told
// the extension was gone (C34).
func deleted(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows == 0 {
		return catalog.ErrNotFound
	}
	return nil
}

func (c *CatalogStore) DeleteExtension(ctx context.Context, id uuid.UUID) error {
	return deleted(c.q.DeleteExtension(ctx, id))
}

// extensionOf converts a row, dropping the password: it exists for the switch
// to read through its own view, never for the API to hand back.
func extensionOf(r queries.Extension) catalog.Extension {
	return catalog.Extension{
		ID:          r.ID,
		Number:      r.Number,
		DisplayName: r.DisplayName,
		IsEnabled:   r.IsEnabled,
		CreatedAt:   r.CreatedAt.Time,
	}
}

//
// Queues.
//

func (c *CatalogStore) ListQueues(ctx context.Context) ([]catalog.Queue, error) {
	rows, err := c.q.ListQueues(ctx)
	if err != nil {
		return nil, fmt.Errorf("list queues: %w", err)
	}
	out := make([]catalog.Queue, 0, len(rows))
	for _, r := range rows {
		out = append(out, queueOf(r))
	}
	return out, nil
}

func (c *CatalogStore) QueueByID(ctx context.Context, id uuid.UUID) (catalog.Queue, error) {
	row, err := c.q.GetQueue(ctx, id)
	if err != nil {
		return catalog.Queue{}, fmt.Errorf("get queue: %w", err)
	}
	return queueOf(row), nil
}

// CreateQueue adds a queue, allocating its number when none was named.
//
// The allocation shares the extension pool's lock rather than taking one of
// its own. Two pools, one queue of writers: the contention is nil (a queue is
// created about as often as a person is hired) and a second lock is a second
// thing to reason about when two of them ever meet.
func (c *CatalogStore) CreateQueue(ctx context.Context, q catalog.Queue,
	rangeLow, rangeHigh int) (catalog.Queue, error) {
	tiers, hours, overflow, err := queueJSON(q)
	if err != nil {
		return catalog.Queue{}, err
	}

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return catalog.Queue{}, fmt.Errorf("create queue: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := c.q.WithTx(tx)

	if q.ExtNumber == "" {
		if err := qtx.LockExtensionPool(ctx, extensionPoolLockKey); err != nil {
			return catalog.Queue{}, fmt.Errorf("lock the number pool: %w", err)
		}
		number, err := qtx.LowestFreeQueueNumber(ctx, queries.LowestFreeQueueNumberParams{
			RangeLow:  int32(rangeLow),
			RangeHigh: int32(rangeHigh),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return catalog.Queue{}, catalog.ErrPoolExhausted
			}
			return catalog.Queue{}, fmt.Errorf("find a free queue number: %w", err)
		}
		q.ExtNumber = number
	}

	row, err := qtx.CreateQueue(ctx, queries.CreateQueueParams{
		ID:                       q.ID,
		Name:                     q.Name,
		ExtNumber:                q.ExtNumber,
		DisplayName:              q.DisplayName,
		Strategy:                 string(q.Strategy),
		MohSound:                 q.MohSound,
		MaxWaitSec:               int32(q.MaxWaitSec),
		MaxWaitNoAgentSec:        int32(q.MaxWaitNoAgentSec),
		AnnounceSound:            optional(q.AnnounceSound),
		AnnounceFrequencySec:     int32(q.AnnounceFrequencySec),
		TierRules:                tiers,
		DiscardAbandonedAfterSec: int32(q.DiscardAbandonedAfterSec),
		IsAbandonedResumeAllowed: q.IsAbandonedResumeAllowed,
		RonaDelaySec:             int32(q.RonaDelaySec),
		SlaThresholdSec:          int32(q.SLAThresholdSec),
		IsRecordingEnabled:       q.IsRecordingEnabled,
		Hours:                    hours,
		Overflow:                 overflow,
		IsEnabled:                q.IsEnabled,
	})
	if err != nil {
		return catalog.Queue{}, fmt.Errorf("create queue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return catalog.Queue{}, fmt.Errorf("commit queue %s: %w", q.Name, err)
	}
	return queueOf(row), nil
}

func (c *CatalogStore) UpdateQueue(ctx context.Context, q catalog.Queue) (catalog.Queue, error) {
	tiers, hours, overflow, err := queueJSON(q)
	if err != nil {
		return catalog.Queue{}, err
	}
	row, err := c.q.UpdateQueue(ctx, queries.UpdateQueueParams{
		ID:                       q.ID,
		DisplayName:              q.DisplayName,
		Strategy:                 string(q.Strategy),
		MohSound:                 q.MohSound,
		MaxWaitSec:               int32(q.MaxWaitSec),
		MaxWaitNoAgentSec:        int32(q.MaxWaitNoAgentSec),
		AnnounceSound:            optional(q.AnnounceSound),
		AnnounceFrequencySec:     int32(q.AnnounceFrequencySec),
		TierRules:                tiers,
		DiscardAbandonedAfterSec: int32(q.DiscardAbandonedAfterSec),
		IsAbandonedResumeAllowed: q.IsAbandonedResumeAllowed,
		RonaDelaySec:             int32(q.RonaDelaySec),
		SlaThresholdSec:          int32(q.SLAThresholdSec),
		IsRecordingEnabled:       q.IsRecordingEnabled,
		Hours:                    hours,
		Overflow:                 overflow,
		IsEnabled:                q.IsEnabled,
	})
	if err != nil {
		return catalog.Queue{}, fmt.Errorf("update queue: %w", err)
	}
	return queueOf(row), nil
}

func (c *CatalogStore) DeleteQueue(ctx context.Context, id uuid.UUID) error {
	return deleted(c.q.DeleteQueue(ctx, id))
}

func queueJSON(q catalog.Queue) (tiers, hours, overflow []byte, err error) {
	if tiers, err = json.Marshal(q.TierRules); err != nil {
		return nil, nil, nil, fmt.Errorf("encode tier rules: %w", err)
	}
	if hours, err = json.Marshal(q.Hours); err != nil {
		return nil, nil, nil, fmt.Errorf("encode hours: %w", err)
	}
	if overflow, err = json.Marshal(q.Overflow); err != nil {
		return nil, nil, nil, fmt.Errorf("encode overflow: %w", err)
	}
	return tiers, hours, overflow, nil
}

func queueOf(r queries.Queue) catalog.Queue {
	q := catalog.Queue{
		ID:                       r.ID,
		Name:                     r.Name,
		ExtNumber:                r.ExtNumber,
		DisplayName:              r.DisplayName,
		Strategy:                 catalog.Strategy(r.Strategy),
		MohSound:                 r.MohSound,
		MaxWaitSec:               int(r.MaxWaitSec),
		MaxWaitNoAgentSec:        int(r.MaxWaitNoAgentSec),
		AnnounceFrequencySec:     int(r.AnnounceFrequencySec),
		DiscardAbandonedAfterSec: int(r.DiscardAbandonedAfterSec),
		IsAbandonedResumeAllowed: r.IsAbandonedResumeAllowed,
		RonaDelaySec:             int(r.RonaDelaySec),
		SLAThresholdSec:          int(r.SlaThresholdSec),
		IsRecordingEnabled:       r.IsRecordingEnabled,
		IsEnabled:                r.IsEnabled,
		Hours:                    []catalog.BusinessHours{},
	}
	if r.AnnounceSound != nil {
		q.AnnounceSound = *r.AnnounceSound
	}
	_ = json.Unmarshal(r.TierRules, &q.TierRules)
	_ = json.Unmarshal(r.Hours, &q.Hours)
	_ = json.Unmarshal(r.Overflow, &q.Overflow)
	if q.Hours == nil {
		q.Hours = []catalog.BusinessHours{}
	}
	return q
}

//
// Staffing.
//

func (c *CatalogStore) ListQueueAgents(ctx context.Context, queueID uuid.UUID) ([]catalog.QueueAgent, error) {
	rows, err := c.q.ListQueueAgents(ctx, queueID)
	if err != nil {
		return nil, fmt.Errorf("list queue agents: %w", err)
	}
	out := make([]catalog.QueueAgent, 0, len(rows))
	for _, r := range rows {
		out = append(out, catalog.QueueAgent{
			QueueID:     r.QueueID,
			AgentID:     r.AgentID,
			DisplayName: r.DisplayName,
			Level:       int(r.Level),
			Position:    int(r.Position),
		})
	}
	return out, nil
}

func (c *CatalogStore) SetQueueAgent(ctx context.Context, queueID, agentID uuid.UUID, level, position int) error {
	return c.q.SetQueueAgent(ctx, queries.SetQueueAgentParams{
		QueueID: queueID, AgentID: agentID,
		Level: int32(level), Position: int32(position),
	})
}

// RemoveQueueAgent unstaffs an agent. Not being staffed there is a not-found,
// the same as the queue not existing: the caller asked for a row to go and
// there was no row.
func (c *CatalogStore) RemoveQueueAgent(ctx context.Context, queueID, agentID uuid.UUID) error {
	return deleted(c.q.RemoveQueueAgent(ctx,
		queries.RemoveQueueAgentParams{QueueID: queueID, AgentID: agentID}))
}

// CallcenterName resolves an agent's switch-side name.
func (c *CatalogStore) CallcenterName(ctx context.Context, agentID uuid.UUID) (string, error) {
	row, err := c.q.GetAgent(ctx, agentID)
	if err != nil {
		return "", fmt.Errorf("get agent: %w", err)
	}
	return row.CallcenterName, nil
}

//
// Numbers.
//

func (c *CatalogStore) ListDIDs(ctx context.Context) ([]catalog.DID, error) {
	rows, err := c.q.ListDIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list dids: %w", err)
	}
	out := make([]catalog.DID, 0, len(rows))
	for _, r := range rows {
		out = append(out, didOf(r))
	}
	return out, nil
}

func (c *CatalogStore) CreateDID(ctx context.Context, d catalog.DID) (catalog.DID, error) {
	row, err := c.q.CreateDID(ctx, queries.CreateDIDParams{
		ID:                 d.ID,
		Number:             d.Number,
		Language:           d.Language,
		FlowID:             d.FlowID,
		FallbackQueueID:    d.FallbackQueueID,
		IsRecordingEnabled: d.IsRecordingEnabled,
		Description:        d.Description,
		IsEnabled:          d.IsEnabled,
	})
	if err != nil {
		return catalog.DID{}, fmt.Errorf("create did: %w", err)
	}
	return didOf(row), nil
}

func (c *CatalogStore) UpdateDID(ctx context.Context, d catalog.DID) (catalog.DID, error) {
	row, err := c.q.UpdateDID(ctx, queries.UpdateDIDParams{
		ID:                 d.ID,
		Language:           d.Language,
		FlowID:             d.FlowID,
		FallbackQueueID:    d.FallbackQueueID,
		IsRecordingEnabled: d.IsRecordingEnabled,
		Description:        d.Description,
		IsEnabled:          d.IsEnabled,
	})
	if err != nil {
		return catalog.DID{}, fmt.Errorf("update did: %w", err)
	}
	return didOf(row), nil
}

func (c *CatalogStore) DeleteDID(ctx context.Context, id uuid.UUID) error {
	return deleted(c.q.DeleteDID(ctx, id))
}

func didOf(r queries.Did) catalog.DID {
	return catalog.DID{
		ID:                 r.ID,
		Number:             r.Number,
		Language:           r.Language,
		FlowID:             r.FlowID,
		FallbackQueueID:    r.FallbackQueueID,
		IsRecordingEnabled: r.IsRecordingEnabled,
		Description:        r.Description,
		IsEnabled:          r.IsEnabled,
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
