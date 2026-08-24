// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// CatalogStore adapts the generated queries to the catalogue service.
type CatalogStore struct{ q *queries.Queries }

// Catalog returns the configuration view of the store.
func (s *Store) Catalog() *CatalogStore { return &CatalogStore{q: s.Queries} }

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
		out = append(out, extensionOf(r))
	}
	return out, nil
}

func (c *CatalogStore) CreateExtension(ctx context.Context, e catalog.Extension) (catalog.Extension, error) {
	row, err := c.q.CreateExtension(ctx, queries.CreateExtensionParams{
		ID:          e.ID,
		Number:      e.Number,
		Kind:        string(e.Kind),
		Password:    e.Password,
		DisplayName: e.DisplayName,
		IsEnabled:   e.IsEnabled,
	})
	if err != nil {
		return catalog.Extension{}, fmt.Errorf("create extension: %w", err)
	}
	return extensionOf(row), nil
}

func (c *CatalogStore) UpdateExtension(ctx context.Context, e catalog.Extension) (catalog.Extension, error) {
	row, err := c.q.UpdateExtension(ctx, queries.UpdateExtensionParams{
		ID:          e.ID,
		Kind:        string(e.Kind),
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
		Kind:        catalog.ExtensionKind(r.Kind),
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

func (c *CatalogStore) CreateQueue(ctx context.Context, q catalog.Queue) (catalog.Queue, error) {
	tiers, hours, overflow, err := queueJSON(q)
	if err != nil {
		return catalog.Queue{}, err
	}
	row, err := c.q.CreateQueue(ctx, queries.CreateQueueParams{
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
