// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// 00032 widens agent_states.reason to admit DEVICE_LOST, and the case that
// matters is the one a fresh database cannot show: a deployment that already
// holds presence rows. The Up has to revalidate them under the wider rule
// without rewriting any, and the Down — which narrows — has to rewrite the
// DEVICE_LOST rows before it can install the older CHECK at all, or PostgreSQL
// refuses it with "is violated by some row" on every deployment that ever ran
// the feature.
//
// The helpers come from migrate_test.go: scratchDB, openScratch and gooseFor.
func TestMigrationsAdmitDeviceLostAsANotReadyReason(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	// Stop one short of the migration under test, so the presence row exists
	// under the older vocabulary before the constraint changes underneath it.
	if err := goose.UpToContext(ctx, db, "migrations", 31); err != nil {
		t.Fatalf("migrating to 31 failed: %v", err)
	}

	const (
		userID  = "77777777-7777-7777-7777-777777777777"
		agentID = "88888888-8888-8888-8888-888888888888"
	)
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, username, password_hash, display_name, role)
		  VALUES ($1, 'devicelost', 'x', 'Device Lost', 'AGENT')`, []any{userID}},
		{`INSERT INTO agents (id, user_id, callcenter_name)
		  VALUES ($1, $2, 'devicelost')`, []any{agentID, userID}},
		{`INSERT INTO agent_states (agent_id, state, reason, extension_number)
		  VALUES ($1, 'NOT_READY', 'SYSTEM', '1001')`, []any{agentID}},
	}
	for _, s := range seed {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed an agent with presence: %v", err)
		}
	}

	if err := goose.UpToContext(ctx, db, "migrations", 32); err != nil {
		t.Fatalf("migrating a database that already holds presence failed: %v", err)
	}

	// The reason the feature exists to write. Until 00032 this write is what
	// rolled every device-loss transition back to READY.
	if _, err := db.ExecContext(ctx,
		`UPDATE agent_states SET reason = 'DEVICE_LOST' WHERE agent_id = $1`, agentID); err != nil {
		t.Fatalf("recording DEVICE_LOST was refused by the database: %v", err)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 31); err != nil {
		t.Fatalf("rolling 00032 back on a database holding a DEVICE_LOST row failed: %v", err)
	}

	var reason string
	if err := db.QueryRowContext(ctx,
		`SELECT reason FROM agent_states WHERE agent_id = $1`, agentID).Scan(&reason); err != nil {
		t.Fatalf("read the rolled-back row: %v", err)
	}
	if reason != "SYSTEM" {
		t.Errorf("reason = %q after the rollback, want SYSTEM — the narrower "+
			"vocabulary cannot express DEVICE_LOST, so the row had to be rewritten", reason)
	}

	// And forward again, which is the path a deployment takes after a rollback
	// is reversed.
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("re-applying after the rollback failed: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE agent_states SET reason = 'DEVICE_LOST' WHERE agent_id = $1`, agentID); err != nil {
		t.Fatalf("DEVICE_LOST was refused after re-applying: %v", err)
	}
}
