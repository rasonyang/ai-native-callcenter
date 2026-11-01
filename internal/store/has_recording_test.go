// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// has_recording means an object was stored. MarkRecorded sets it, and a later
// write of the same call's row (the bot ledger's, or a rewrite by the human
// path) must not clear it (#196).
func TestARecordedFlagSurvivesALaterCDRWrite(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()

	start := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)
	callID := uuid.Must(uuid.NewV7())
	row := func(endedAfter time.Duration) CDR {
		return CDR{
			CallID: callID, StartedAt: start, AnsweredAt: start,
			EndedAt:  start.Add(endedAfter),
			CallType: "OUTBOUND", Status: CDRStatusAnswered, HasRecording: false,
		}
	}
	has := func() bool {
		t.Helper()
		got, err := ledger.GetCDR(ctx, callID)
		if err != nil {
			t.Fatal(err)
		}
		return got.HasRecording
	}

	if err := ledger.InsertCDR(ctx, row(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if has() {
		t.Fatal("a row written without a stored object claims a recording")
	}
	if err := ledger.MarkRecorded(ctx, callID); err != nil {
		t.Fatal(err)
	}
	if !has() {
		t.Fatal("MarkRecorded did not set the flag")
	}
	// A later write that ends later replaces the row; the flag stays.
	if err := ledger.InsertCDR(ctx, row(60*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !has() {
		t.Error("a later CDR write cleared the flag MarkRecorded had set")
	}
}

// 00041 clears has_recording where no recordings row backs it and leaves a
// row that has one alone.
func TestMigrationClearsARecordingFlagNothingBacks(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 40); err != nil {
		t.Fatalf("migrating to 40 failed: %v", err)
	}
	const unbacked = "aaaaaaaa-0000-0000-0000-000000000001"
	const backed = "aaaaaaaa-0000-0000-0000-000000000002"
	for _, id := range []string{unbacked, backed} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status, has_recording)
			VALUES ($1, now() - interval '1 minute', now(), 'OUTBOUND', 'ANSWERED', true)`, id); err != nil {
			t.Fatalf("seed a call claiming a recording: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO recordings (id, call_id, backend, object_key)
		VALUES (gen_random_uuid(), $1, 'S3', '2026/10/12/x.wav')`, backed); err != nil {
		t.Fatalf("seed a recording: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 41); err != nil {
		t.Fatalf("migrating a database holding the false claim failed: %v", err)
	}
	flag := func(id string) bool {
		var v bool
		if err := db.QueryRowContext(ctx, `SELECT has_recording FROM cdrs WHERE call_id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if flag(unbacked) {
		t.Error("a call with no recording row still claims one")
	}
	if !flag(backed) {
		t.Error("a call with a recording row lost its flag")
	}
	if err := goose.DownToContext(ctx, db, "migrations", 40); err != nil {
		t.Fatalf("rolling 41 back failed: %v", err)
	}
}
