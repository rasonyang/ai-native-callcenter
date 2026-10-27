// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00045 lets a campaign be paused as OUTBOUND_NOT_CONFIGURED. Going up keeps
// every row a database already holds; going down rewrites a row that carries
// the new reason to TRUNK_FAILURE before the narrower constraint returns, or
// PostgreSQL would refuse the rollback on a deployment with history.
func TestMigrationAllowsThePauseReasonOfAMissingEndpoint(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 44); err != nil {
		t.Fatalf("migrating to 44 failed: %v", err)
	}
	didID := uuid.New()
	exec := func(q string, args ...any) error {
		_, err := db.ExecContext(ctx, q, args...)
		return err
	}
	if err := exec(`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
	      VALUES ($1, '95302', 'en', false, true)`, didID); err != nil {
		t.Fatal(err)
	}
	insert := func(reason string) (uuid.UUID, error) {
		id := uuid.New()
		return id, exec(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent,
		          ring_timeout_sec, retry_policy, status, pause_reason)
		      VALUES ($1, $2, $3, 'UTC', '{}', 1, 30, '{}', 'PAUSED', $4)`, id, reason, didID, reason)
	}
	oldManual, err := insert("MANUAL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insert("OUTBOUND_NOT_CONFIGURED"); err == nil {
		t.Fatal("00044 accepted a pause reason it should not know")
	}

	if err := goose.UpToContext(ctx, db, "migrations", 45); err != nil {
		t.Fatalf("migrating a database that already holds campaigns failed: %v", err)
	}
	missing, err := insert("OUTBOUND_NOT_CONFIGURED")
	if err != nil {
		t.Fatalf("00045 refused the new reason: %v", err)
	}
	if err := exec(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent,
	          ring_timeout_sec, retry_policy, status, pause_reason)
	      VALUES ($1, 'running', $2, 'UTC', '{}', 1, 30, '{}', 'RUNNING', 'OUTBOUND_NOT_CONFIGURED')`,
		uuid.New(), didID); err == nil {
		t.Error("a campaign that is not PAUSED was given a pause reason")
	}

	if err := goose.DownToContext(ctx, db, "migrations", 44); err != nil {
		t.Fatalf("rolling 00045 back with the new reason in place failed: %v", err)
	}
	reason := func(id uuid.UUID) string {
		t.Helper()
		var r string
		if err := db.QueryRowContext(ctx, `SELECT pause_reason FROM campaigns WHERE id = $1`, id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if got := reason(missing); got != "TRUNK_FAILURE" {
		t.Errorf("the rolled-back row reads %q, want TRUNK_FAILURE", got)
	}
	if got := reason(oldManual); got != "MANUAL" {
		t.Errorf("an untouched row reads %q, want MANUAL", got)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 45); err != nil {
		t.Fatalf("migrating forward again failed: %v", err)
	}
}
