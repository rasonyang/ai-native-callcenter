// SPDX-License-Identifier: Apache-2.0

// Package seed fills an empty installation with a deterministic demo:
// a small team, two queues, and seven days of synthetic history so the
// wallboard, the CDR explorer and the reports render alive on first sight.
package seed

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// prngSeed pins the whole history: same seed, same rows, every install.
const prngSeed = 20260814

// Demo seeds the demo dataset. Existing data always wins: entities are
// inserted with on-conflict-do-nothing on their natural keys, and the
// history is generated only into an empty ledger.
func Demo(ctx context.Context, st *store.Store, log *slog.Logger) error {
	agents, queues, err := ensureEntities(ctx, st, log)
	if err != nil {
		return fmt.Errorf("seed entities: %w", err)
	}

	var cdrCount int64
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM cdrs`).Scan(&cdrCount); err != nil {
		return err
	}
	if cdrCount > 0 {
		log.Info("seed: the ledger already has calls; history untouched", "cdrs", cdrCount)
		return nil
	}

	plan := planHistory(time.Now(), agents, queues, rand.New(rand.NewSource(prngSeed)))
	ledger := st.Ledger()
	for _, cdr := range plan.CDRs {
		if err := ledger.InsertCDR(ctx, cdr); err != nil {
			return fmt.Errorf("seed cdr: %w", err)
		}
	}
	for _, ev := range plan.QueueEvents {
		if err := ledger.InsertQueueEvent(ctx, ev.OccurredAt, &ev.CallID, ev.QueueID,
			ev.Event, ev.AgentID, ev.WaitMs); err != nil {
			return fmt.Errorf("seed queue event: %w", err)
		}
	}
	for _, entry := range plan.StateLogs {
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO agent_state_logs (agent_id, state, reason, entered_at, exited_at)
			VALUES ($1, $2, $3, $4, $5)`,
			entry.AgentID, entry.State, entry.Reason, entry.EnteredAt, entry.ExitedAt); err != nil {
			return fmt.Errorf("seed state log: %w", err)
		}
	}

	log.Info("seed: demo history written",
		"cdrs", len(plan.CDRs), "queueEvents", len(plan.QueueEvents), "stateLogs", len(plan.StateLogs))
	return nil
}

// QueueRef is a queue the history can reference.
type QueueRef struct {
	ID   uuid.UUID
	Name string
}

// ensureEntities creates the demo team and queues where they do not already
// exist, and returns whatever agents and queues the database ends up with.
func ensureEntities(ctx context.Context, st *store.Store, log *slog.Logger) ([]uuid.UUID, []QueueRef, error) {
	demoHash, err := auth.HashPassword("demo1234")
	if err != nil {
		return nil, nil, err
	}

	type person struct{ username, display, ext string }
	team := []person{
		{"amy", "Amy Zhang", "1000"},
		{"ben", "Ben Liu", "1001"},
		{"cara", "Cara Wu", "1002"},
	}
	for _, p := range team {
		userID := uuid.New()
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO users (id, username, password_hash, display_name, role)
			VALUES ($1, $2, $3, $4, 'AGENT')
			ON CONFLICT (username) DO NOTHING`,
			userID, p.username, demoHash, p.display); err != nil {
			return nil, nil, err
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO extensions (id, number, kind, password, display_name)
			VALUES ($1, $2, 'AGENT', $3, $4)
			ON CONFLICT (number) DO NOTHING`,
			uuid.New(), p.ext, uuid.NewString(), p.display); err != nil {
			return nil, nil, err
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO agents (id, user_id, callcenter_name)
			SELECT $1, u.id, 'agent-' || $2::text FROM users u
			WHERE u.username = $2 AND NOT EXISTS (
				SELECT 1 FROM agents a WHERE a.user_id = u.id)`,
			uuid.New(), p.username); err != nil {
			return nil, nil, err
		}
	}

	for _, q := range []struct{ name, ext, display string }{
		{"support-en", "7001", "Support (EN)"},
		{"support-zh", "7002", "Support (ZH)"},
	} {
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO queues (id, name, ext_number, display_name, sla_threshold_sec)
			VALUES ($1, $2, $3, $4, 20)
			ON CONFLICT (name) DO NOTHING`,
			uuid.New(), q.name, q.ext, q.display); err != nil {
			return nil, nil, err
		}
	}

	// The history references whatever actually exists, seeded or prior.
	var agents []uuid.UUID
	rows, err := st.Pool.Query(ctx, `SELECT id FROM agents ORDER BY created_at LIMIT 8`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		agents = append(agents, id)
	}

	var queues []QueueRef
	qrows, err := st.Pool.Query(ctx, `SELECT id, name FROM queues ORDER BY name LIMIT 8`)
	if err != nil {
		return nil, nil, err
	}
	defer qrows.Close()
	for qrows.Next() {
		var q QueueRef
		if err := qrows.Scan(&q.ID, &q.Name); err != nil {
			return nil, nil, err
		}
		queues = append(queues, q)
	}

	log.Info("seed: entities ensured", "agents", len(agents), "queues", len(queues))
	return agents, queues, nil
}
