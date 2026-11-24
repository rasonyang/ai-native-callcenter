// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00051 adds the lead's judgement to attempts on a database that already holds
// them (#661): every existing attempt, with a lead or without, owes no
// judgement and keeps its lead; the new CHECKs refuse an unknown verdict, an
// intent that is not judged, and a judgement missing one of its parts; and the
// rollback and a second migration work with rows in place.
func TestMigrationLeavesExistingAttemptsUnjudged(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()
	if err := goose.UpToContext(ctx, db, "migrations", 50); err != nil {
		t.Fatalf("migrating to 50 failed: %v", err)
	}

	didID, campaignID, contactID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
	      VALUES ($1, '95304', 'zh', false, true)`, didID)
	exec(`INSERT INTO campaigns (id, name, did_id, status, time_zone, calling_window, max_concurrent, ring_timeout_sec, retry_policy)
	      VALUES ($1, 'old', $2, 'RUNNING', 'Asia/Shanghai', '{}', 1, 30, '{}')`, campaignID, didID)
	exec(`INSERT INTO campaign_contacts (id, campaign_id, number_e164, number_raw, status)
	      VALUES ($1, $2, '+8613800000021', '+8613800000021', 'DONE')`, contactID, campaignID)
	withLead, withoutLead := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, a := range []struct {
		id   uuid.UUID
		lead *string
	}{{withLead, ptr("HIGH")}, {withoutLead, nil}} {
		exec(`INSERT INTO campaign_attempts (id, campaign_id, campaign_contact_id, number_e164, call_id, leg_id,
		          state, outcome, finished_at, is_touched, lead_intent)
		      VALUES ($1, $2, $3, '+8613800000021', $4, $5, 'FINISHED', 'ANSWERED', now(), true, $6)`,
			a.id, campaignID, contactID, uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), a.lead)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 51); err != nil {
		t.Fatalf("migrating a database that already holds attempts failed: %v", err)
	}
	for name, id := range map[string]uuid.UUID{"with a lead": withLead, "without one": withoutLead} {
		var lead, declared, verdict, reason sql.NullString
		var deadline sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT lead_intent, declared_lead_intent, lead_verdict, lead_verdict_reason,
		        lead_judge_deadline_at FROM campaign_attempts WHERE id = $1`, id).
			Scan(&lead, &declared, &verdict, &reason, &deadline); err != nil {
			t.Fatal(err)
		}
		if declared.Valid || verdict.Valid || reason.Valid || deadline.Valid {
			t.Errorf("%s: a judgement appeared: %v %v %v %v", name, declared, verdict, reason, deadline)
		}
		if id == withLead && lead.String != "HIGH" {
			t.Errorf("%s: lead %v, want HIGH kept", name, lead)
		}
	}

	refused := func(constraint, q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, withLead); err == nil {
			t.Errorf("%s did not refuse: %s", constraint, q)
		}
	}
	refused("ck_campaign_attempts_lead_verdict", `UPDATE campaign_attempts SET declared_lead_intent = 'HIGH',
		lead_verdict = 'MAYBE', lead_judge_deadline_at = now() WHERE id = $1`)
	refused("ck_campaign_attempts_declared_lead_intent", `UPDATE campaign_attempts SET declared_lead_intent = 'LOW',
		lead_verdict = 'PENDING', lead_judge_deadline_at = now() WHERE id = $1`)
	refused("ck_campaign_attempts_lead_judgement", `UPDATE campaign_attempts SET lead_verdict = 'PENDING' WHERE id = $1`)
	refused("ck_campaign_attempts_lead_judgement", `UPDATE campaign_attempts SET declared_lead_intent = 'HIGH',
		lead_verdict = 'PENDING' WHERE id = $1`)
	for _, v := range []string{"PENDING", "CONFIRMED", "UNDECIDED", "REFUSED", "MACHINE", "NO_EVIDENCE", "NOT_JUDGED"} {
		if _, err := db.ExecContext(ctx, `UPDATE campaign_attempts SET declared_lead_intent = 'HIGH',
			lead_verdict = $2, lead_judge_deadline_at = now() WHERE id = $1`, withLead, v); err != nil {
			t.Errorf("verdict %s refused: %v", v, err)
		}
	}

	if err := goose.DownToContext(ctx, db, "migrations", 50); err != nil {
		t.Fatalf("rolling back with rows in place: %v", err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 51); err != nil {
		t.Fatalf("migrating again: %v", err)
	}
}

func ptr(s string) *string { return &s }
