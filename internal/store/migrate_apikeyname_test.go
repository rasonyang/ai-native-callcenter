// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// 00046 widens api_keys.name from 120 to the 128 characters the contract
// allows (#370). A database that already holds keys keeps every name as it
// was; afterwards 128 characters fit, in English or in Chinese. The Down cuts
// a longer name to 120 characters rather than failing, and leaves every other
// name and every key alone.
func TestMigrationWidensAnAPIKeyNameTo128Characters(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 45); err != nil {
		t.Fatalf("migrating to 45 failed: %v", err)
	}
	insert := func(name string) (uuid.UUID, error) {
		t.Helper()
		id := uuid.New()
		_, err := db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, key_prefix, scopes)
		      VALUES ($1, $2, $3, 'aicc_test', '{calls:read:all}')`, id, name, id[:])
		return id, err
	}
	mustInsert := func(name string) uuid.UUID {
		t.Helper()
		id, err := insert(name)
		if err != nil {
			t.Fatalf("insert a key named %d characters: %v", len([]rune(name)), err)
		}
		return id
	}
	nameOf := func(id uuid.UUID) string {
		t.Helper()
		var name string
		// As text: the same statement is asked before and after the column's
		// type changes, and the connection's cached plan for it would not
		// survive a change in its result type.
		if err := db.QueryRowContext(ctx, `SELECT name::text FROM api_keys WHERE id = $1`, id).Scan(&name); err != nil {
			t.Fatalf("read key %s: %v", id, err)
		}
		return name
	}

	// The rows a deployment has today: up to 120 characters, the old width.
	short := mustInsert("NovaNet CRM sync")
	full120 := mustInsert(strings.Repeat("n", 120))
	han120 := mustInsert(strings.Repeat("话", 120))
	if _, err := insert(strings.Repeat("n", 121)); err == nil {
		t.Fatal("before 00046 a 121-character name was stored; the fixture does not show the old width")
	}

	if err := goose.UpToContext(ctx, db, "migrations", 46); err != nil {
		t.Fatalf("migrating a database that already holds keys failed: %v", err)
	}
	for id, want := range map[uuid.UUID]string{
		short: "NovaNet CRM sync", full120: strings.Repeat("n", 120), han120: strings.Repeat("话", 120),
	} {
		if got := nameOf(id); got != want {
			t.Errorf("a name changed going up: got %d characters, want %d", len([]rune(got)), len([]rune(want)))
		}
	}

	ascii128 := mustInsert(strings.Repeat("n", 128))
	han128 := mustInsert(strings.Repeat("话", 128)) // 384 bytes, 128 characters
	if _, err := insert(strings.Repeat("n", 129)); err == nil {
		t.Error("a 129-character name was stored; the column is wider than the contract's 128")
	}

	if err := goose.DownToContext(ctx, db, "migrations", 45); err != nil {
		t.Fatalf("rolling 00046 back with 128-character names in place failed: %v", err)
	}
	for id, want := range map[uuid.UUID]string{
		short:    "NovaNet CRM sync",
		full120:  strings.Repeat("n", 120),
		han120:   strings.Repeat("话", 120),
		ascii128: strings.Repeat("n", 120),
		han128:   strings.Repeat("话", 120),
	} {
		if got := nameOf(id); got != want {
			t.Errorf("after rolling back: a name reads %d characters (%.20q…), want %d (%.20q…)",
				len([]rune(got)), got, len([]rune(want)), want)
		}
	}
	var keys int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM api_keys`).Scan(&keys); err != nil || keys != 5 {
		t.Errorf("keys after rolling back: %d, %v; want all 5", keys, err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 46); err != nil {
		t.Fatalf("re-applying 00046 failed: %v", err)
	}
}
