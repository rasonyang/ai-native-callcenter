// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

func TestTheSchemaRefusesWhatTheAttemptModelForbids(t *testing.T) {
	ctx := context.Background()
	e := openAttempts(t)
	c, contacts := e.campaign(t, "Attempt constraints", "3", 2, nil)
	ct := contacts[0]

	ins := func(number, cols, vals string) string {
		return fmt.Sprintf(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id%s)
			VALUES ('%s', '%s', '%s', '%s', '%s', '%s'%s)`,
			cols, uuid.New(), c.ID, ct.ID, number, uuid.New(), uuid.New(), vals)
	}
	violation(t, e.st, "ck_campaign_attempts_number_e164", ins("0138", "", ""))
	violation(t, e.st, "ck_campaign_attempts_outcome", ins(ct.Number, ", state, outcome, finished_at", ", 'FINISHED', 'MAYBE', now()"))
	// DIALING with an outcome, FINISHED with none, touched before it ended.
	violation(t, e.st, "ck_campaign_attempts_finished", ins(ct.Number, ", outcome", ", 'BUSY'"))
	violation(t, e.st, "ck_campaign_attempts_finished", ins(ct.Number, ", state, finished_at", ", 'FINISHED', now()"))
	violation(t, e.st, "ck_campaign_attempts_finished", ins(ct.Number, ", is_touched", ", true"))
	violation(t, e.st, "ck_campaign_attempts_talk_sec", ins(ct.Number, ", state, outcome, finished_at, talk_sec", ", 'FINISHED', 'BUSY', now(), -1"))
	violation(t, e.st, "ck_campaign_attempts_lead_intent", ins(ct.Number, ", lead_intent", ", 'WARM'"))
	violation(t, e.st, "fk_campaign_attempts_campaigns",
		fmt.Sprintf(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id) VALUES ('%s', '%s', '%s', '%s', '%s', '%s')`,
			uuid.New(), uuid.New(), ct.ID, ct.Number, uuid.New(), uuid.New()))

	// Every outcome the contact column accepts, the attempt column accepts,
	// and finished attempts share a number freely.
	for _, o := range []string{"ANSWERED", "BOT_FAILED", "OVERFLOW", "NO_ANSWER", "BUSY", "REJECTED", "UNKNOWN",
		"TRANSIENT", "ORIGINATE_FAILED", "INVALID_NUMBER", "DNC_SKIPPED"} {
		if _, err := e.st.Pool.Exec(ctx, ins(ct.Number, ", state, outcome, finished_at", fmt.Sprintf(", 'FINISHED', '%s', now()", o))); err != nil {
			t.Errorf("outcome %s refused: %v", o, err)
		}
	}

	// Two attempts in flight for one number, in one statement.
	other := contacts[1]
	violation(t, e.st, "uq_campaign_attempts_dialing_number_e164",
		fmt.Sprintf(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id)
			VALUES ('%s','%s','%s','%s','%s','%s'),('%s','%s','%s','%s','%s','%s')`,
			uuid.New(), c.ID, other.ID, other.Number, uuid.New(), uuid.New(),
			uuid.New(), c.ID, other.ID, other.Number, uuid.New(), uuid.New()))

	var callID uuid.UUID
	if err := e.st.Pool.QueryRow(ctx, `SELECT call_id FROM campaign_attempts LIMIT 1`).Scan(&callID); err != nil {
		t.Fatal(err)
	}
	violation(t, e.st, "uq_campaign_attempts_call_id",
		fmt.Sprintf(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id, state, outcome, finished_at)
			VALUES ('%s','%s','%s','%s','%s','%s','FINISHED','BUSY',now())`, uuid.New(), c.ID, ct.ID, ct.Number, callID, uuid.New()))
}

// A deployment that already holds campaigns and their contacts migrates to
// the attempts table with that history intact, and the contacts can then be
// dialled.
func TestTheAttemptsMigrationKeepsExistingCampaigns(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpToContext(ctx, db, "migrations", 37); err != nil {
		t.Fatalf("migrating to 37: %v", err)
	}

	didID, campaignID, contactID := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO dids (id, number, language, allow_inbound, allow_outbound) VALUES ($1, '95099', 'en', false, true)`, didID); err != nil {
		t.Skipf("cannot seed a DID with this schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO campaigns (id, name, did_id, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
		VALUES ($1, 'old', $2, 'UTC', '{}', 1, 30, '{}')`, campaignID, didID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw, attempts, last_outcome)
		VALUES ($1, $2, '+8613800138000', 'x', 2, 'BUSY')`, contactID, campaignID); err != nil {
		t.Fatal(err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database with campaigns: %v", err)
	}
	var attempts int
	var last string
	if err := db.QueryRowContext(ctx, `SELECT attempts, last_outcome FROM campaign_contacts WHERE id=$1`, contactID).Scan(&attempts, &last); err != nil || attempts != 2 || last != "BUSY" {
		t.Errorf("the contact changed: %d %q %v", attempts, last, err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id)
		VALUES ($1, $2, $3, '+8613800138000', $4, $5)`, uuid.New(), campaignID, contactID, uuid.New(), uuid.New()); err != nil {
		t.Errorf("an old contact cannot be dialled after the migration: %v", err)
	}
	// Rolling the migration back leaves the campaigns and contacts alone.
	if err := goose.DownToContext(ctx, db, "migrations", 37); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM campaign_contacts`).Scan(&n); err != nil || n != 1 {
		t.Errorf("contacts after the rollback: %d %v", n, err)
	}
}
