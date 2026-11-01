// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00043 gives a callback filed with words for its number the caller's number
// from the call's ledger row (#311), on a database that already holds such
// rows. Only a number with no digit in it is repaired; one with digits, even
// an invalid one, is left, and so is a callback with no call, no ledger row,
// or a ledger row with no usable number. The Down is a no-op that leaves the
// repaired rows as they are, and the Up runs again cleanly.
//
// The helpers come from migrate_test.go: scratchDB, openScratch and gooseFor.
func TestMigrationRepairsACallbackNumberMadeOfWords(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 42); err != nil {
		t.Fatalf("migrating to 42 failed: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	cdr := func(callType, from, to, did string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		exec(`INSERT INTO cdrs (call_id, started_at, ended_at, call_type, from_number, to_number, did, status)
		      VALUES ($1, $2, $3, $4, $5, $6, $7, 'ANSWERED')`,
			id, t0, t0.Add(time.Minute), callType, from, to, did)
		return id
	}
	callback := func(callID *uuid.UUID, phone string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		exec(`INSERT INTO callbacks (id, call_id, phone_number, message) VALUES ($1, $2, $3, 'call me back')`,
			id, callID, phone)
		return id
	}

	inbound := cdr("INBOUND", "13800138000", "95012", "95012")
	placed := cdr("OUTBOUND", "95301", "18688886660", "95301")
	agentToBot := cdr("OUTBOUND", "1008", "95012", "95012")
	noNumber := cdr("INBOUND", "anonymous", "95012", "95012")
	missingCDR := uuid.Must(uuid.NewV7())

	cases := []struct {
		name string
		id   uuid.UUID
		want string
	}{
		{"words on an inbound call", callback(&inbound, "当前来电号码"), "13800138000"},
		{"words in English", callback(&inbound, "this number"), "13800138000"},
		{"an empty number", callback(&inbound, ""), "13800138000"},
		{"words on a call placed to a customer", callback(&placed, "本机号码"), "18688886660"},
		{"words on an agent's call to a bot DID", callback(&agentToBot, "当前来电号码"), "1008"},
		{"words with no call", callback(nil, "当前来电号码"), "当前来电号码"},
		{"words whose call has no ledger row", callback(&missingCDR, "当前来电号码"), "当前来电号码"},
		{"words whose call has no number", callback(&noNumber, "当前来电号码"), "当前来电号码"},
		{"digits that are not a valid number", callback(&inbound, "13800138000转123"), "13800138000转123"},
		{"a number a digit short", callback(&inbound, "1380013800"), "1380013800"},
		{"a valid number", callback(&inbound, "13900139000"), "13900139000"},
	}

	if err := goose.UpToContext(ctx, db, "migrations", 43); err != nil {
		t.Fatalf("migrating a database that holds such callbacks failed: %v", err)
	}
	check := func(stage string) {
		t.Helper()
		for _, c := range cases {
			var got string
			if err := db.QueryRowContext(ctx,
				`SELECT phone_number FROM callbacks WHERE id = $1`, c.id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("%s: %s: phone_number = %q, want %q", stage, c.name, got, c.want)
			}
		}
	}
	check("up")

	if err := goose.DownToContext(ctx, db, "migrations", 42); err != nil {
		t.Fatalf("rolling 00043 back failed: %v", err)
	}
	check("down")
	if err := goose.UpToContext(ctx, db, "migrations", 43); err != nil {
		t.Fatalf("re-applying 00043 failed: %v", err)
	}
	check("up again")
}
