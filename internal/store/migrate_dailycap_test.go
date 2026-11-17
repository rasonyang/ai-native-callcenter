// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00049 adds the daily cap's mark to contacts and the canonical callee to the
// ledger on a database that already holds both: old contacts come up unmarked
// (whatever their frequency mark), old CDRs have no canonical callee, the
// constraints hold, and the rollback and a second migration work with rows in
// place.
func TestMigrationAddsTheDailyCapMarkAndTheCanonicalCallee(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()
	if err := goose.UpToContext(ctx, db, "migrations", 48); err != nil {
		t.Fatalf("migrating to 48 failed: %v", err)
	}

	didID, campaignID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	held, plain := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	cdrID := uuid.Must(uuid.NewV7())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
	      VALUES ($1, '95302', 'en', false, true)`, didID)
	exec(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
	      VALUES ($1, 'old', $2, 'UTC', '{}', 1, 30, '{}')`, campaignID, didID)
	exec(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw, next_attempt_at, is_frequency_held)
	      VALUES ($1, $2, '+8613800000001', '+8613800000001', now() + interval '1 hour', true)`, held, campaignID)
	exec(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw, next_attempt_at, is_frequency_held)
	      VALUES ($1, $2, '+8613800000002', '+8613800000002', now(), false)`, plain, campaignID)
	exec(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id,
	          state, outcome, is_touched, started_at, finished_at)
	      VALUES ($1, $2, $3, '+8613800000002', $4, $5, 'FINISHED', 'NO_ANSWER', true, now(), now())`,
		uuid.Must(uuid.NewV7()), campaignID, plain, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()))
	exec(`INSERT INTO cdrs (call_id, started_at, ended_at, call_type, to_number, status)
	      VALUES ($1, now(), now(), 'OUTBOUND', '13800000003', 'NO_ANSWER')`, cdrID)

	if err := goose.UpToContext(ctx, db, "migrations", 49); err != nil {
		t.Fatalf("migrating a database that already holds contacts and CDRs failed: %v", err)
	}

	for _, id := range []uuid.UUID{held, plain} {
		var capMark bool
		if err := db.QueryRowContext(ctx, `SELECT is_daily_cap_held FROM campaign_contacts WHERE id = $1`, id).Scan(&capMark); err != nil || capMark {
			t.Errorf("an old contact carries the cap's mark %v (%v), want false", capMark, err)
		}
	}
	var freq bool
	if err := db.QueryRowContext(ctx, `SELECT is_frequency_held FROM campaign_contacts WHERE id = $1`, held).Scan(&freq); err != nil || !freq {
		t.Errorf("the frequency mark of an old contact is %v (%v), want it kept", freq, err)
	}
	var callee *string
	if err := db.QueryRowContext(ctx, `SELECT to_number_e164 FROM cdrs WHERE call_id = $1`, cdrID).Scan(&callee); err != nil || callee != nil {
		t.Errorf("an old CDR has the canonical callee %v (%v), want NULL: not backfilled", callee, err)
	}

	// The constraints hold.
	if _, err := db.ExecContext(ctx, `UPDATE campaign_contacts SET is_daily_cap_held = true WHERE id = $1`, held); err == nil {
		t.Error("a contact took both marks")
	}
	if _, err := db.ExecContext(ctx, `UPDATE campaign_contacts SET is_frequency_held = false, is_daily_cap_held = true WHERE id = $1`, held); err != nil {
		t.Errorf("a contact refused the cap's mark alone: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE cdrs SET to_number_e164 = '13800000003' WHERE call_id = $1`, cdrID); err == nil {
		t.Error("the ledger took a callee that is not canonical")
	}
	if _, err := db.ExecContext(ctx, `UPDATE cdrs SET to_number_e164 = '+8613800000003' WHERE call_id = $1`, cdrID); err != nil {
		t.Errorf("the ledger refused a canonical callee: %v", err)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 48); err != nil {
		t.Fatalf("rolling back with rows in place: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 49); err != nil {
		t.Fatalf("migrating again: %v", err)
	}
}
