// SPDX-License-Identifier: Apache-2.0

// Package seed fills an empty installation with a deterministic demo: a small
// team, two queues, a published bilingual flow behind two numbers, and seven
// days of synthetic history so the wallboard, the CDR explorer and the reports
// render alive on first sight.
package seed

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// flowFiles carries the demo flows into the binary, so a container with
// nothing mounted still answers a call. The same files are the ones an
// operator edits and reloads with `aicc flowadd -file …`.
//
//go:embed flows/*.json
var flowFiles embed.FS

// prngSeed pins the whole history: same seed, same rows, every install.
const prngSeed = 20260814

// demoPassword is the documented password of every seeded account and of the
// SIP extensions behind them — a softphone that cannot register is not a demo.
// The dataset only exists where AICC_SEED=demo was set deliberately, and the
// deployment doc says in as many words that it must not be a public host.
const demoPassword = "demo1234"

// demoFlowFile is the flow both demo numbers answer with. It carries English
// and Chinese personas, so one flow serves both — the number's language picks
// the strings (phase1-decisions A1: language never selects a provider).
const demoFlowFile = "flows/novanet_support.json"

// demoPeople is the cast of the demo. An account with no extension is not an
// agent and gets no presence: the administrator and the supervisor watch.
var demoPeople = []struct {
	username, display, role, ext string
}{
	{"admin", "Ada Ops", "ADMIN", ""},
	{"sam", "Sam Reyes", "SUPERVISOR", ""},
	{"amy", "Amy Zhang", "AGENT", "1000"},
	{"ben", "Ben Liu", "AGENT", "1001"},
	{"cara", "Cara Wu", "AGENT", "1002"},
}

// demoNumbers are the DIDs the demo answers on. Both run the same flow in
// different languages and fall back to the queue of that language.
var demoNumbers = []struct {
	number, language, queue, description string
}{
	{"95001", "en", "support-en", "Demo hotline (English)"},
	{"95002", "zh", "support-zh", "Demo hotline (Chinese)"},
}

// Demo seeds the demo dataset. Existing data always wins: entities are
// inserted with on-conflict-do-nothing on their natural keys, and the
// history is generated only into an empty ledger.
func Demo(ctx context.Context, st *store.Store, log *slog.Logger) error {
	agents, queues, err := ensureEntities(ctx, st, log)
	if err != nil {
		return fmt.Errorf("seed entities: %w", err)
	}
	if err := ensureFlowAndNumbers(ctx, st, log); err != nil {
		return fmt.Errorf("seed flow: %w", err)
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

	// Three agents to fill the wallboard, plus the two accounts a visitor
	// needs to see the whole product: administration and supervision are
	// role-gated, so a demo with agents only hides most of the screens.
	for _, p := range demoPeople {
		userID := uuid.New()
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO users (id, username, password_hash, display_name, role)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (username) DO NOTHING`,
			userID, p.username, demoHash, p.display, p.role); err != nil {
			return nil, nil, err
		}
		if p.ext == "" {
			continue
		}
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO extensions (id, number, kind, password, display_name)
			VALUES ($1, $2, 'AGENT', $3, $4)
			ON CONFLICT (number) DO NOTHING`,
			uuid.New(), p.ext, demoPassword, p.display); err != nil {
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

// ensureFlowAndNumbers publishes the bundled demo flow and points the demo
// numbers at it. Without this a seeded install looks complete and still cannot
// take a call: dids.flow_id is NOT NULL, so a number exists only once a flow
// does.
//
// Existing data wins here too — an operator who has already published a flow
// under this slug, or who owns these numbers, keeps what they have.
func ensureFlowAndNumbers(ctx context.Context, st *store.Store, log *slog.Logger) error {
	spec, err := flowFiles.ReadFile(demoFlowFile)
	if err != nil {
		return err
	}
	slug := specID(spec)

	flows := st.Flows()
	flowID, err := flows.Create(ctx, slug, "NovaNet support", spec)
	if err != nil {
		existing, lookupErr := st.Queries.GetFlowBySlug(ctx, slug)
		if lookupErr != nil {
			return fmt.Errorf("create flow %s: %w", slug, err)
		}
		log.Info("seed: demo flow already present", "slug", slug)
		flowID = existing.ID
	} else if err := flows.Publish(ctx, flowID, "seed"); err != nil {
		return fmt.Errorf("publish flow %s: %w", slug, err)
	}

	for _, n := range demoNumbers {
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO dids (id, number, language, flow_id, fallback_queue_id, description)
			SELECT $1, $2, $3, $4, q.id, $6 FROM queues q WHERE q.name = $5
			ON CONFLICT (number) DO NOTHING`,
			uuid.New(), n.number, n.language, flowID, n.queue, n.description); err != nil {
			return fmt.Errorf("seed number %s: %w", n.number, err)
		}
	}
	log.Info("seed: demo flow published", "slug", slug, "numbers", len(demoNumbers))
	return nil
}

// specID reads a flow spec's own identifier, which is also its slug.
func specID(spec []byte) string {
	var head struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(spec, &head); err != nil || head.ID == "" {
		return "demo"
	}
	return head.ID
}
