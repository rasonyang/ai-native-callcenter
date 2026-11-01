// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

// 00047 makes every call id and campaign id a UUIDv7 (#400) without touching
// the legacy v4 call ids that customers reference. Rows written by an older
// version stay writable (a CHECK would have refused their updates); a new id
// that is not a v7 is refused on INSERT and when a call_id is changed.
func TestMigrationRequiresUUIDv7CallAndCampaignIDs(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 46); err != nil {
		t.Fatalf("migrating to 46 failed: %v", err)
	}
	v7 := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	exec := func(q string, args ...any) error {
		_, err := db.ExecContext(ctx, q, args...)
		return err
	}
	must := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	const insertCDR = `INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status)
	      VALUES ($1, now(), now(), 'INBOUND', 'ANSWERED')`
	const insertAttempt = `INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id)
	      VALUES ($1, $2, $3, '+8613800138000', $4, $5)`

	didID, contactID := v7(), v7()
	legacyCampaign := uuid.New() // a v4 can exist before 00047; the CHECK is NOT VALID
	legacyCDR, legacyAttempt := uuid.New(), uuid.New()
	must(`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
	      VALUES ($1, '95401', 'en', false, true)`, didID)
	must(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
	      VALUES ($1, 'old', $2, 'UTC', '{}', 1, 30, '{}')`, legacyCampaign, didID)
	must(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw)
	      VALUES ($1, $2, '+8613800138000', '13800138000')`, contactID, legacyCampaign)
	must(insertCDR, legacyCDR)
	must(insertCDR, legacyAttempt)
	attemptID := v7()
	must(insertAttempt, attemptID, legacyCampaign, contactID, legacyAttempt, uuid.New())

	if err := goose.UpToContext(ctx, db, "migrations", 47); err != nil {
		t.Fatalf("migrating a database holding v4 call ids failed: %v", err)
	}

	wantRejected := func(what string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: want SQLSTATE 23514, got %v", what, err)
		}
	}

	// Legacy rows stay writable.
	if err := exec(`UPDATE cdrs SET has_recording = true WHERE call_id = $1`, legacyCDR); err != nil {
		t.Errorf("updating a legacy CDR: %v", err)
	}
	if err := exec(`UPDATE campaign_attempts SET state = 'FINISHED', outcome = 'ANSWERED', finished_at = now()
	      WHERE id = $1`, attemptID); err != nil {
		t.Errorf("finishing a legacy attempt: %v", err)
	}
	if err := exec(`UPDATE campaign_attempts SET lead_notes = 'again' WHERE id = $1`, attemptID); err != nil {
		t.Errorf("updating a legacy attempt: %v", err)
	}
	if err := exec(`INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status)
	      VALUES ($1, now(), now(), 'INBOUND', 'FAILED')
	      ON CONFLICT (call_id) DO UPDATE SET status = EXCLUDED.status`, legacyCDR); err != nil {
		t.Errorf("upserting over a legacy CDR in SQL: %v", err)
	}

	// New ids that are not a v7 are refused, in each table.
	wantRejected("v4 into cdrs", exec(insertCDR, uuid.New()))
	wantRejected("nil into cdrs", exec(insertCDR, uuid.Nil))
	wantRejected("v4 into campaign_attempts",
		exec(insertAttempt, v7(), legacyCampaign, contactID, uuid.New(), uuid.New()))
	wantRejected("nil into campaign_attempts",
		exec(insertAttempt, v7(), legacyCampaign, contactID, uuid.Nil, uuid.New()))
	// A non-RFC variant with version nibble 7 has no version for PostgreSQL.
	badVariant := v7()
	badVariant[8] = 0xc0
	wantRejected("non-RFC variant into cdrs", exec(insertCDR, badVariant))
	wantRejected("v4 campaign id", exec(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
	      VALUES ($1, 'new', $2, 'UTC', '{}', 1, 30, '{}')`, uuid.New(), didID))
	wantRejected("changing a call_id to a v4", exec(`UPDATE cdrs SET call_id = $2 WHERE call_id = $1`, legacyCDR, uuid.New()))
	wantRejected("changing an attempt call_id to a v4",
		exec(`UPDATE campaign_attempts SET call_id = $2 WHERE id = $1`, attemptID, uuid.New()))

	// v7 ids go in.
	newCDR := v7()
	if err := exec(insertCDR, newCDR); err != nil {
		t.Errorf("v7 into cdrs: %v", err)
	}
	campaign := v7()
	must(`INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
	      VALUES ($1, 'new', $2, 'UTC', '{}', 1, 30, '{}')`, campaign, didID)
	contact2 := v7()
	must(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw)
	      VALUES ($1, $2, '+8613800138001', '13800138001')`, contact2, campaign)
	if err := exec(insertAttempt, v7(), campaign, contact2, v7(), v7()); err != nil {
		t.Errorf("v7 into campaign_attempts: %v", err)
	}
	if err := exec(`UPDATE cdrs SET call_id = $2 WHERE call_id = $1`, newCDR, v7()); err != nil {
		t.Errorf("changing a call_id to a v7: %v", err)
	}

	// The real upsert replaces a legacy v4 CDR.
	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	now := time.Now().UTC()
	cdr := CDR{CallID: legacyCDR, StartedAt: now, EndedAt: now.Add(time.Minute), CallType: "INBOUND", Status: "ANSWERED", TalkSec: 60}
	if err := st.Ledger().InsertCDR(ctx, cdr); err != nil {
		t.Fatalf("InsertCDR over a legacy v4 CDR: %v", err)
	}
	var talk int
	if err := db.QueryRowContext(ctx, `SELECT talk_sec FROM cdrs WHERE call_id = $1`, legacyCDR).Scan(&talk); err != nil || talk != 60 {
		t.Errorf("the legacy CDR after the upsert: talk_sec %d, %v; want 60", talk, err)
	}
	cdr.CallID = uuid.New()
	if err := st.Ledger().InsertCDR(ctx, cdr); err == nil {
		t.Error("InsertCDR accepted a new v4 call id")
	}

	if err := goose.DownToContext(ctx, db, "migrations", 46); err != nil {
		t.Fatalf("rolling 00047 back failed: %v", err)
	}
	if err := exec(insertCDR, uuid.New()); err != nil {
		t.Errorf("after rolling back a v4 id is still refused: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 47); err != nil {
		t.Fatalf("re-applying 00047 failed: %v", err)
	}
}
