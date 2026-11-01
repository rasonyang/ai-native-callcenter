// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00044 adds the AI draft to wrap-ups. A database that already holds records,
// confirmed and not, keeps every value it had and gains records with no draft;
// the Down drops the columns with a draft in place and leaves the records.
func TestMigrationAddsTheWrapUpDraftWithoutTouchingRecords(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 43); err != nil {
		t.Fatalf("migrating to 43 failed: %v", err)
	}
	confirmed, open, agent := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO wrap_ups (call_id, agent_id, disposition_code, disposition_label, note, is_confirmed)
	      VALUES ($1, $2, 'RESOLVED', 'Resolved', 'filed by hand', true)`, confirmed, agent)
	exec(`INSERT INTO wrap_ups (call_id, agent_id, disposition_code, disposition_label, note, is_confirmed)
	      VALUES ($1, $2, 'RESOLVED', 'Resolved', '', false)`, open, agent)

	if err := goose.UpToContext(ctx, db, "migrations", 44); err != nil {
		t.Fatalf("migrating a database that already holds wrap-ups failed: %v", err)
	}

	type row struct {
		note, code  string
		isConfirmed bool
		draftNull   bool
	}
	read := func(call uuid.UUID) row {
		t.Helper()
		var r row
		var draftCode, draftNote *string
		var generated *string
		if err := db.QueryRowContext(ctx, `SELECT note, disposition_code, is_confirmed,
		        draft_disposition_code, draft_note, draft_generated_at::text
		        FROM wrap_ups WHERE call_id = $1`, call).
			Scan(&r.note, &r.code, &r.isConfirmed, &draftCode, &draftNote, &generated); err != nil {
			t.Fatal(err)
		}
		r.draftNull = draftCode == nil && draftNote == nil && generated == nil
		return r
	}
	if got := read(confirmed); got != (row{"filed by hand", "RESOLVED", true, true}) {
		t.Errorf("confirmed record after migrating: %+v", got)
	}
	if got := read(open); got != (row{"", "RESOLVED", false, true}) {
		t.Errorf("open record after migrating: %+v", got)
	}

	exec(`UPDATE wrap_ups SET draft_disposition_code = 'FOLLOW_UP_REQUIRED', draft_note = 'n', draft_generated_at = now()
	      WHERE call_id = $1`, open)
	if err := goose.DownToContext(ctx, db, "migrations", 43); err != nil {
		t.Fatalf("rolling 00044 back with a draft in place failed: %v", err)
	}
	var note string
	if err := db.QueryRowContext(ctx, `SELECT note FROM wrap_ups WHERE call_id = $1`, confirmed).Scan(&note); err != nil || note != "filed by hand" {
		t.Errorf("record after rolling back: %q, %v", note, err)
	}
	if err := goose.UpToContext(ctx, db, "migrations", 44); err != nil {
		t.Fatalf("re-applying 00044 failed: %v", err)
	}
}
