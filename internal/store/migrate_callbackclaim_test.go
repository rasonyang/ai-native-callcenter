// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

// 00006 widens callbacks.status to admit CLAIMED, and its Down narrows it
// again. A fresh database cannot show the case that matters: a deployment
// whose agents have claimed callbacks. The Down has to rewrite the CLAIMED
// rows — back to OPEN, the work queue the older vocabulary has, with the
// claimant cleared — before it can install the older CHECK at all, or
// PostgreSQL refuses it with "is violated by some row".
//
// The helpers come from migrate_test.go: scratchDB, openScratch and gooseFor.
func TestMigrationsRollBackAClaimedCallbackToOpen(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 6); err != nil {
		t.Fatalf("migrating to 6 failed: %v", err)
	}

	const (
		claimedID = "11111111-1111-1111-1111-111111111111"
		doneID    = "22222222-2222-2222-2222-222222222222"
		agentID   = "33333333-3333-3333-3333-333333333333"
	)
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO callbacks (id, phone_number, status, handled_by)
		  VALUES ($1, '13800000000', 'CLAIMED', $2)`, []any{claimedID, agentID}},
		{`INSERT INTO callbacks (id, phone_number, status, handled_by, handled_at)
		  VALUES ($1, '13900000000', 'DONE', $2, now())`, []any{doneID, agentID}},
	}
	for _, s := range seed {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed callbacks: %v", err)
		}
	}

	if err := goose.DownToContext(ctx, db, "migrations", 5); err != nil {
		t.Fatalf("rolling 00006 back on a database holding a CLAIMED row failed: %v", err)
	}

	read := func(id string) (string, sql.NullString) {
		t.Helper()
		var status string
		var handledBy sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT status, handled_by::text FROM callbacks WHERE id = $1`, id).
			Scan(&status, &handledBy); err != nil {
			t.Fatalf("read the rolled-back row: %v", err)
		}
		return status, handledBy
	}
	if status, handledBy := read(claimedID); status != "OPEN" || handledBy.Valid {
		t.Errorf("claimed row = (%q, %v) after the rollback, want (OPEN, NULL) — "+
			"the older vocabulary has no claim, so the promise goes back to the queue",
			status, handledBy.String)
	}
	if status, handledBy := read(doneID); status != "DONE" || handledBy.String != agentID {
		t.Errorf("done row = (%q, %v) after the rollback, want it untouched", status, handledBy.String)
	}

	// And forward again, which is the path a deployment takes after a rollback
	// is reversed.
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("re-applying after the rollback failed: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE callbacks SET status = 'CLAIMED', handled_by = $2 WHERE id = $1`,
		claimedID, agentID); err != nil {
		t.Fatalf("CLAIMED was refused after re-applying: %v", err)
	}
}
