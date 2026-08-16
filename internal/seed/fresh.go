// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"context"
	"log/slog"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Fresh removes the demo dataset, so the next screen an operator sees is the
// empty product (phase1-decisions O3, the reset flag). It is the counterpart
// of Demo and undoes exactly what Demo creates — the seeded history, the demo
// flow behind the demo numbers, the two demo queues and the five demo
// accounts. Data that arrived any other way is not its business.
//
// A demo container keeps its database in a volume, so unsetting AICC_SEED
// cannot get anyone back to an empty install; something has to actively clear
// it, and this is that something.
//
// Removal is best-effort per step: if real work has grown onto a demo entity
// (an agent taking calls in a demo queue, a number moved onto another flow),
// the foreign key refuses, and refusing to boot over that would be worse than
// leaving the row. Every skip is logged.
func Fresh(ctx context.Context, st *store.Store, log *slog.Logger) error {
	usernames := make([]string, 0, len(demoPeople))
	extensions := make([]string, 0, len(demoPeople))
	for _, p := range demoPeople {
		usernames = append(usernames, p.username)
		if p.ext != "" {
			extensions = append(extensions, p.ext)
		}
	}
	numbers := make([]string, 0, len(demoNumbers))
	queueNames := make([]string, 0, len(demoNumbers))
	for _, n := range demoNumbers {
		numbers = append(numbers, n.number)
		queueNames = append(queueNames, n.queue)
	}
	flowSlug := ""
	if spec, err := flowFiles.ReadFile(demoFlowFile); err == nil {
		flowSlug = specID(spec)
	}

	// Order follows the foreign keys: the ledger before the entities it
	// points at, numbers before the flow they hold down (ON DELETE RESTRICT),
	// accounts last — users cascade into agents, presence and sessions.
	const seededCDRs = `SELECT call_id FROM cdrs WHERE tech->>'isSeeded' = 'true'`
	steps := []struct {
		what string
		sql  string
		args []any
	}{
		{"transcripts", `DELETE FROM transcripts WHERE call_id IN (` + seededCDRs + `)`, nil},
		{"recordings", `DELETE FROM recordings WHERE call_id IN (` + seededCDRs + `)`, nil},
		{"queue events", `DELETE FROM queue_events WHERE call_id IN (` + seededCDRs + `)`, nil},
		{"calls", `DELETE FROM cdrs WHERE tech->>'isSeeded' = 'true'`, nil},
		{"numbers", `DELETE FROM dids WHERE number = ANY($1)`, []any{numbers}},
		{"flow", `DELETE FROM flows WHERE slug = $1`, []any{flowSlug}},
		{"queues", `DELETE FROM queues WHERE name = ANY($1)`, []any{queueNames}},
		{"accounts", `DELETE FROM users WHERE username = ANY($1)`, []any{usernames}},
		{"extensions", `DELETE FROM extensions WHERE number = ANY($1)`, []any{extensions}},
	}

	var removed int64
	for _, step := range steps {
		tag, err := st.Pool.Exec(ctx, step.sql, step.args...)
		if err != nil {
			log.Warn("seed: demo data kept, something else depends on it",
				"what", step.what, "error", err)
			continue
		}
		removed += tag.RowsAffected()
		if tag.RowsAffected() > 0 {
			log.Info("seed: demo data removed", "what", step.what, "rows", tag.RowsAffected())
		}
	}
	if removed == 0 {
		log.Info("seed: nothing to reset; the install is already empty of demo data")
	}
	return nil
}
