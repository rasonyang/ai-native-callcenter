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

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// AgentStore adapts the generated queries to the agents service, keeping
// database types out of the domain package.
type AgentStore struct{ q *queries.Queries }

// Agents returns the agent-facing view of the store.
func (s *Store) Agents() *AgentStore { return &AgentStore{q: s.Queries} }

// LoadPresence reads an agent's persisted presence. An agent who has never
// signed in reads as logged out rather than as missing.
func (a *AgentStore) LoadPresence(ctx context.Context, agentID uuid.UUID) (agents.Presence, error) {
	row, err := a.q.GetAgentState(ctx, agentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agents.Presence{}, nil
		}
		return agents.Presence{}, fmt.Errorf("get agent state: %w", err)
	}
	return presenceFromRow(row), nil
}

// SavePresence writes an agent's presence.
func (a *AgentStore) SavePresence(ctx context.Context, agentID uuid.UUID, p agents.Presence) error {
	var reason *string
	if p.Reason != "" {
		s := string(p.Reason)
		reason = &s
	}
	var extension *string
	if p.ExtensionNumber != "" {
		e := p.ExtensionNumber
		extension = &e
	}

	_, err := a.q.UpsertAgentState(ctx, queries.UpsertAgentStateParams{
		AgentID:         agentID,
		State:           string(p.CurrentState()),
		Reason:          reason,
		ExtensionNumber: extension,
		EnteredAt:       timestamp(p.EnteredAt),
		WrapUpEndsAt:    timestamp(p.WrapUpEndsAt),
	})
	if err != nil {
		return fmt.Errorf("upsert agent state: %w", err)
	}
	return nil
}

// LogStateChange closes the previous history row and opens a new one, so
// occupancy reporting sees contiguous intervals.
func (a *AgentStore) LogStateChange(ctx context.Context, agentID uuid.UUID, p agents.Presence) error {
	if err := a.q.CloseAgentStateLog(ctx, queries.CloseAgentStateLogParams{
		AgentID:  agentID,
		ExitedAt: timestamp(p.EnteredAt),
	}); err != nil {
		return fmt.Errorf("close agent state log: %w", err)
	}

	var reason *string
	if p.Reason != "" {
		s := string(p.Reason)
		reason = &s
	}
	if _, err := a.q.OpenAgentStateLog(ctx, queries.OpenAgentStateLogParams{
		AgentID:   agentID,
		State:     string(p.CurrentState()),
		Reason:    reason,
		EnteredAt: timestamp(p.EnteredAt),
	}); err != nil {
		return fmt.Errorf("open agent state log: %w", err)
	}
	return nil
}

// AgentProfile reads the configuration behind one agent.
func (a *AgentStore) AgentProfile(ctx context.Context, agentID uuid.UUID) (agents.Profile, error) {
	row, err := a.q.GetAgent(ctx, agentID)
	if err != nil {
		return agents.Profile{}, fmt.Errorf("get agent: %w", err)
	}
	user, err := a.q.GetUserByID(ctx, row.UserID)
	if err != nil {
		return agents.Profile{}, fmt.Errorf("get agent user: %w", err)
	}
	profile := agents.Profile{
		AgentID:        row.ID,
		UserID:         row.UserID,
		CallcenterName: row.CallcenterName,
		DisplayName:    user.DisplayName,
		WrapUpTimeSec:  int(row.WrapUpTimeSec),
		IsAutoAnswer:   row.IsAutoAnswer,
	}
	// The bound phone is configuration, not a sign-in choice. A missing
	// binding is not an error here: the caller reports it when the agent
	// tries to sign in.
	if row.DefaultExtensionID != nil {
		if ext, err := a.q.GetExtension(ctx, *row.DefaultExtensionID); err == nil {
			profile.ExtensionNumber = ext.Number
		}
	}
	return profile, nil
}

// CreateAgent stores a new agent identity.
func (a *AgentStore) CreateAgent(ctx context.Context, cfg agents.AgentConfig) (agents.AgentConfig, error) {
	row, err := a.q.CreateAgent(ctx, queries.CreateAgentParams{
		ID:                 uuid.New(),
		UserID:             cfg.UserID,
		CallcenterName:     cfg.CallcenterName,
		WrapUpTimeSec:      int32(cfg.WrapUpTimeSec),
		IsAutoAnswer:       cfg.IsAutoAnswer,
		DefaultExtensionID: cfg.DefaultExtensionID,
	})
	if err != nil {
		return agents.AgentConfig{}, fmt.Errorf("create agent: %w", err)
	}
	return agentConfigOf(row), nil
}

// UpdateAgent rewrites one agent's configuration.
func (a *AgentStore) UpdateAgent(ctx context.Context, cfg agents.AgentConfig) (agents.AgentConfig, error) {
	row, err := a.q.UpdateAgent(ctx, queries.UpdateAgentParams{
		ID:                 cfg.AgentID,
		CallcenterName:     cfg.CallcenterName,
		WrapUpTimeSec:      int32(cfg.WrapUpTimeSec),
		IsAutoAnswer:       cfg.IsAutoAnswer,
		DefaultExtensionID: cfg.DefaultExtensionID,
	})
	if err != nil {
		return agents.AgentConfig{}, fmt.Errorf("update agent: %w", err)
	}
	return agentConfigOf(row), nil
}

// DeleteAgent removes an agent identity.
func (a *AgentStore) DeleteAgent(ctx context.Context, agentID uuid.UUID) error {
	if err := a.q.DeleteAgent(ctx, agentID); err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return nil
}

func agentConfigOf(row queries.Agent) agents.AgentConfig {
	return agents.AgentConfig{
		AgentID:            row.ID,
		UserID:             row.UserID,
		CallcenterName:     row.CallcenterName,
		WrapUpTimeSec:      int(row.WrapUpTimeSec),
		IsAutoAnswer:       row.IsAutoAnswer,
		DefaultExtensionID: row.DefaultExtensionID,
	}
}

// Roster reads every agent with their persisted presence.
func (a *AgentStore) Roster(ctx context.Context) ([]agents.RosterEntry, error) {
	rows, err := a.q.ListAgentRoster(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agent roster: %w", err)
	}

	out := make([]agents.RosterEntry, 0, len(rows))
	for _, row := range rows {
		entry := agents.RosterEntry{
			AgentID:            row.AgentID,
			UserID:             row.UserID,
			Username:           row.Username,
			DisplayName:        row.DisplayName,
			State:              agents.State(row.State),
			EnteredAt:          row.EnteredAt.Time,
			CallcenterName:     row.CallcenterName,
			WrapUpTimeSec:      int(row.WrapUpTimeSec),
			IsAutoAnswer:       row.IsAutoAnswer,
			DefaultExtensionID: row.DefaultExtensionID,
		}
		if row.DefaultExtensionNumber != nil {
			entry.DefaultExtensionNumber = *row.DefaultExtensionNumber
		}
		if row.Reason != nil {
			entry.Reason = agents.Reason(*row.Reason)
		}
		if row.ExtensionNumber != nil {
			entry.Extension = *row.ExtensionNumber
		}
		if row.WrapUpEndsAt.Valid {
			ends := row.WrapUpEndsAt.Time
			entry.WrapUpEndsAt = &ends
		}
		out = append(out, entry)
	}
	return out, nil
}

func presenceFromRow(row queries.AgentState) agents.Presence {
	p := agents.Presence{
		State:     agents.State(row.State),
		EnteredAt: row.EnteredAt.Time,
	}
	if row.Reason != nil {
		p.Reason = agents.Reason(*row.Reason)
	}
	if row.ExtensionNumber != nil {
		p.ExtensionNumber = *row.ExtensionNumber
	}
	if row.WrapUpEndsAt.Valid {
		p.WrapUpEndsAt = row.WrapUpEndsAt.Time
	}
	return p
}

func timestamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}
