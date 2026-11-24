// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00052 gives a line's refusal an outcome of its own and keeps an attempt's
// SIP status, on a database that already holds attempts and contacts: every
// existing row keeps its outcome and has no status, both CHECKs take
// LINE_REJECTED and still refuse a made-up outcome, the status CHECK refuses a
// number that is no SIP status, and a retry policy that retries TRANSIENT
// retries LINE_REJECTED too (one that does not is left alone). Going down
// rewrites a LINE_REJECTED attempt and contact to TRANSIENT, the old outcome
// with the same budget and no touch, and takes the value out of the retry
// policies, or PostgreSQL would refuse the narrower constraints.
func TestMigrationAddsTheLineRejectedOutcomeAndTheAttemptsSIPStatus(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()
	if err := goose.UpToContext(ctx, db, "migrations", 51); err != nil {
		t.Fatalf("migrating to 51 failed: %v", err)
	}

	exec := func(q string, args ...any) error {
		_, err := db.ExecContext(ctx, q, args...)
		return err
	}
	must := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	didID := uuid.Must(uuid.NewV7())
	must(`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
	      VALUES ($1, '95305', 'zh', false, true)`, didID)
	retriesTransient := `{"maxAttempts": 3, "maxSystemRetries": 5,
		"retryOutcomes": ["NO_ANSWER", "BUSY", "REJECTED", "UNKNOWN", "TRANSIENT", "ORIGINATE_FAILED"],
		"intervalSec": {"NO_ANSWER": 7200, "BUSY": 1800, "REJECTED": 86400, "UNKNOWN": 86400},
		"transientBackoff": {"baseSec": 300, "factor": 3, "capSec": 14400}}`
	noTransient := `{"maxAttempts": 3, "maxSystemRetries": 5, "retryOutcomes": ["NO_ANSWER", "REJECTED"],
		"intervalSec": {}, "transientBackoff": {"baseSec": 300, "factor": 3, "capSec": 14400}}`
	campaignWith := func(name, policy string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		must(`INSERT INTO campaigns (id, name, did_id, status, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
		      VALUES ($1, $2, $3, 'RUNNING', 'Asia/Shanghai', '{}', 1, 30, $4::jsonb)`, id, name, didID, policy)
		return id
	}
	campaignID := campaignWith("retries transient", retriesTransient)
	quietID := campaignWith("retries no system failure", noTransient)
	bareID := campaignWith("an old policy with no list", `{}`)

	contactID := uuid.Must(uuid.NewV7())
	must(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw, status, attempts, last_outcome)
	      VALUES ($1, $2, '+8613800000052', '+8613800000052', 'ACTIVE', 1, 'REJECTED')`, contactID, campaignID)
	attempt := func(outcome string, touched bool) (uuid.UUID, error) {
		id := uuid.Must(uuid.NewV7())
		return id, exec(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id,
		          state, outcome, hangup_cause, finished_at, is_touched)
		      VALUES ($1, $2, $3, '+8613800000052', $4, $5, 'FINISHED', $6, 'CALL_REJECTED', now(), $7)`,
			id, campaignID, contactID, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), outcome, touched)
	}
	oldRejected, err := attempt("REJECTED", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempt("LINE_REJECTED", false); err == nil {
		t.Fatal("00051 accepted an attempt outcome it should not know")
	}
	if err := exec(`UPDATE campaign_contacts SET last_outcome = 'LINE_REJECTED' WHERE id = $1`, contactID); err == nil {
		t.Fatal("00051 accepted a last outcome it should not know")
	}

	if err := goose.UpToContext(ctx, db, "migrations", 52); err != nil {
		t.Fatalf("migrating a database that already holds attempts failed: %v", err)
	}
	var outcome string
	var status sql.NullInt32
	if err := db.QueryRowContext(ctx, `SELECT outcome, sip_status FROM campaign_attempts WHERE id = $1`, oldRejected).
		Scan(&outcome, &status); err != nil {
		t.Fatal(err)
	}
	if outcome != "REJECTED" || status.Valid {
		t.Errorf("an existing attempt reads %s with status %v, want REJECTED and no status", outcome, status)
	}

	retryOutcomes := func(id uuid.UUID) []string {
		t.Helper()
		var raw []byte
		if err := db.QueryRowContext(ctx, `SELECT retry_policy -> 'retryOutcomes' FROM campaigns WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var out []string
		if raw != nil {
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("retryOutcomes %s: %v", raw, err)
			}
		}
		return out
	}
	if got, want := retryOutcomes(campaignID), []string{"NO_ANSWER", "BUSY", "REJECTED", "UNKNOWN", "TRANSIENT", "LINE_REJECTED", "ORIGINATE_FAILED"}; !slices.Equal(got, want) {
		t.Errorf("a policy that retries TRANSIENT: %v, want %v", got, want)
	}
	if got, want := retryOutcomes(quietID), []string{"NO_ANSWER", "REJECTED"}; !slices.Equal(got, want) {
		t.Errorf("a policy that does not retry TRANSIENT: %v, want it left as %v", got, want)
	}
	if got := retryOutcomes(bareID); got != nil {
		t.Errorf("a policy with no list: %v, want it left without one", got)
	}

	lineRejected, err := attempt("LINE_REJECTED", false)
	if err != nil {
		t.Fatalf("00052 refused LINE_REJECTED on an attempt: %v", err)
	}
	if err := exec(`UPDATE campaign_attempts SET sip_status = 402 WHERE id = $1`, lineRejected); err != nil {
		t.Fatalf("00052 refused a status: %v", err)
	}
	if err := exec(`UPDATE campaign_contacts SET last_outcome = 'LINE_REJECTED', system_failures = 1 WHERE id = $1`, contactID); err != nil {
		t.Fatalf("00052 refused LINE_REJECTED as a last outcome: %v", err)
	}
	if _, err := attempt("LINE_REFUSED", false); err == nil {
		t.Error("00052 accepted a made-up outcome")
	}
	for _, bad := range []int{0, 99, 700} {
		if err := exec(`UPDATE campaign_attempts SET sip_status = $2 WHERE id = $1`, lineRejected, bad); err == nil {
			t.Errorf("ck_campaign_attempts_sip_status accepted %d", bad)
		}
	}
	if err := goose.DownToContext(ctx, db, "migrations", 51); err != nil {
		t.Fatalf("rolling 00052 back with LINE_REJECTED rows in place failed: %v", err)
	}

	if err := db.QueryRowContext(ctx, `SELECT outcome, is_touched FROM campaign_attempts WHERE id = $1`, lineRejected).
		Scan(&outcome, new(bool)); err != nil {
		t.Fatal(err)
	}
	if outcome != "TRANSIENT" {
		t.Errorf("the rolled-back attempt reads %q, want TRANSIENT", outcome)
	}
	var last string
	var failures int
	if err := db.QueryRowContext(ctx, `SELECT last_outcome, system_failures FROM campaign_contacts WHERE id = $1`, contactID).
		Scan(&last, &failures); err != nil {
		t.Fatal(err)
	}
	if last != "TRANSIENT" || failures != 1 {
		t.Errorf("the rolled-back contact reads %q with %d system failures, want TRANSIENT and 1", last, failures)
	}
	if err := db.QueryRowContext(ctx, `SELECT outcome FROM campaign_attempts WHERE id = $1`, oldRejected).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != "REJECTED" {
		t.Errorf("an untouched attempt reads %q, want REJECTED", outcome)
	}
	if got, want := retryOutcomes(campaignID), []string{"NO_ANSWER", "BUSY", "REJECTED", "UNKNOWN", "TRANSIENT", "ORIGINATE_FAILED"}; !slices.Equal(got, want) {
		t.Errorf("the rolled-back policy: %v, want %v", got, want)
	}
	var hasColumn bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
	        WHERE table_name = 'campaign_attempts' AND column_name = 'sip_status')`).Scan(&hasColumn); err != nil {
		t.Fatal(err)
	}
	if hasColumn {
		t.Error("the rollback left campaign_attempts.sip_status")
	}

	if err := goose.UpToContext(ctx, db, "migrations", 52); err != nil {
		t.Fatalf("migrating forward again failed: %v", err)
	}
	if got, want := retryOutcomes(campaignID), []string{"NO_ANSWER", "BUSY", "REJECTED", "UNKNOWN", "TRANSIENT", "LINE_REJECTED", "ORIGINATE_FAILED"}; !slices.Equal(got, want) {
		t.Errorf("migrated forward again: %v, want %v", got, want)
	}
}
