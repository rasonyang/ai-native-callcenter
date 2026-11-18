// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00050 adds the retry policy's time to contacts on a database that already
// holds deferred ones (#616). A waiting contact with a history keeps its current
// due time as its retry time (the policy's own cannot be recovered, and the due
// time is never earlier, so no interval can be shortened); one never tried, one
// in flight and one that is no longer ACTIVE get none. The CHECK holds, and the
// rollback and a second migration work with rows in place.
func TestMigrationGivesWaitingContactsARetryTime(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()
	if err := goose.UpToContext(ctx, db, "migrations", 49); err != nil {
		t.Fatalf("migrating to 49 failed: %v", err)
	}

	didID, campaignID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
	      VALUES ($1, '95303', 'en', false, true)`, didID)
	exec(`INSERT INTO campaigns (id, name, did_id, status, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
	      VALUES ($1, 'old', $2, 'RUNNING', 'Asia/Shanghai', '{}', 1, 30, '{}')`, campaignID, didID)

	// The old window's opening, where both waiting contacts were deferred.
	opening := time.Date(2026, 6, 3, 1, 0, 0, 0, time.UTC)
	type row struct {
		id          uuid.UUID
		number      string
		status      string
		next        *time.Time
		lastOutcome *string
		capHeld     bool
	}
	str := func(s string) *string { return &s }
	tm := func(t time.Time) *time.Time { return &t }
	untried := row{uuid.Must(uuid.NewV7()), "+8613800000011", "ACTIVE", tm(opening), nil, false}
	tried := row{uuid.Must(uuid.NewV7()), "+8613800000012", "ACTIVE", tm(opening), str("NO_ANSWER"), false}
	capped := row{uuid.Must(uuid.NewV7()), "+8613800000013", "ACTIVE", tm(opening), str("BUSY"), true}
	inFlight := row{uuid.Must(uuid.NewV7()), "+8613800000014", "ACTIVE", nil, str("BUSY"), false}
	exhausted := row{uuid.Must(uuid.NewV7()), "+8613800000015", "EXHAUSTED", nil, str("REJECTED"), false}
	dnc := row{uuid.Must(uuid.NewV7()), "+8613800000016", "DNC", nil, str("DNC_SKIPPED"), false}
	all := []row{untried, tried, capped, inFlight, exhausted, dnc}
	for _, r := range all {
		exec(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw, status, next_attempt_at,
		          last_outcome, is_daily_cap_held)
		      VALUES ($1, $2, $3, $3, $4, $5, $6, $7)`,
			r.id, campaignID, r.number, r.status, r.next, r.lastOutcome, r.capHeld)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 50); err != nil {
		t.Fatalf("migrating a database that already holds deferred contacts failed: %v", err)
	}

	retryOf := func(id uuid.UUID) (retry, next sql.NullTime) {
		t.Helper()
		if err := db.QueryRowContext(ctx,
			`SELECT retry_at, next_attempt_at FROM campaign_contacts WHERE id = $1`, id).Scan(&retry, &next); err != nil {
			t.Fatal(err)
		}
		return
	}
	for name, r := range map[string]row{"tried and waiting": tried, "tried and held by the cap": capped} {
		if retry, next := retryOf(r.id); !retry.Valid || !retry.Time.Equal(opening) || !next.Time.Equal(opening) {
			t.Errorf("%s: retry %v next %v, want both the stored due time %v", name, retry, next, opening)
		}
	}
	for name, r := range map[string]row{"never tried": untried, "in flight": inFlight, "exhausted": exhausted, "DNC": dnc} {
		if retry, _ := retryOf(r.id); retry.Valid {
			t.Errorf("%s: retry %v, want none", name, retry.Time)
		}
	}
	var held bool
	if err := db.QueryRowContext(ctx, `SELECT is_daily_cap_held FROM campaign_contacts WHERE id = $1`, capped.id).Scan(&held); err != nil || !held {
		t.Errorf("the cap's mark is %v (%v), want it kept", held, err)
	}

	// The CHECK: only an ACTIVE contact has a retry time.
	if _, err := db.ExecContext(ctx, `UPDATE campaign_contacts SET retry_at = now() WHERE id = $1`, exhausted.id); err == nil {
		t.Error("an EXHAUSTED contact took a retry time")
	}
	if _, err := db.ExecContext(ctx, `UPDATE campaign_contacts SET status = 'DONE', next_attempt_at = NULL WHERE id = $1`, tried.id); err == nil {
		t.Error("a contact left ACTIVE keeping its retry time")
	}
	if _, err := db.ExecContext(ctx, `UPDATE campaign_contacts SET retry_at = now() WHERE id = $1`, untried.id); err != nil {
		t.Errorf("an ACTIVE contact refused a retry time: %v", err)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 49); err != nil {
		t.Fatalf("rolling back with rows in place: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 50); err != nil {
		t.Fatalf("migrating again: %v", err)
	}
}
