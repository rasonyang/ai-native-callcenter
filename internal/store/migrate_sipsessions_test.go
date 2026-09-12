// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// 00031 removes the last plaintext SIP password from the switch's contract and
// replaces it with a session-scoped digest.
//
// Reading the file cannot tell you whether the view really lost a column —
// PostgreSQL will not drop one through CREATE OR REPLACE, so the migration has
// to drop and recreate, and a drop silently takes the Lua role's grant with it.
// Nor can reading tell you whether the join actually filters on expiry. Both
// are checked here against a server.
//
// Needs AICC_TEST_DATABASE_URL; see migrate_test.go.

// columnsOf returns the column names of a relation, table or view alike.
func columnsOf(t *testing.T, db *sql.DB, schema, name string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT column_name FROM information_schema.columns
		  WHERE table_schema = $1 AND table_name = $2
		  ORDER BY ordinal_position`, schema, name)
	if err != nil {
		t.Fatalf("read the columns of %s.%s: %v", schema, name, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan a column name: %v", err)
		}
		out = append(out, col)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the columns of %s.%s: %v", schema, name, err)
	}
	return out
}

// The table holds a digest and nothing that could be replayed as a password.
func TestSIPSessionsHoldNoPlaintext(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a fresh database failed: %v", err)
	}

	got := columnsOf(t, db, "public", "sip_sessions")
	want := []string{"agent_id", "extension", "a1_hash", "created_at", "expires_at"}
	if len(got) != len(want) {
		t.Fatalf("sip_sessions has %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d is %q, want %q", i, got[i], want[i])
		}
	}
	// Named separately from the list above, because the list is what a future
	// change edits and this is the rule that must survive editing it.
	for _, col := range got {
		if strings.Contains(col, "password") || strings.Contains(col, "secret") {
			t.Errorf("sip_sessions.%s reads like a plaintext credential", col)
		}
	}
}

// The CHECK is the second half of "no plaintext": a column that would accept
// anything is a column somebody can put a password in.
func TestSIPSessionsRefuseAnythingThatIsNotAnMD5Hex(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ids := seedAgentAtExtension(t, db, "1001")

	for _, bad := range []string{"", "swordfish", "ABCDEF0123456789ABCDEF0123456789", strings.Repeat("a", 31)} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO sip_sessions (agent_id, extension, a1_hash, expires_at)
			 VALUES ($1, '1001', $2, now() + interval '1 hour')`, ids.agentID, bad)
		if err == nil {
			t.Errorf("sip_sessions accepted %q as an a1-hash", bad)
			if _, err := db.ExecContext(ctx, `DELETE FROM sip_sessions`); err != nil {
				t.Fatalf("clean up: %v", err)
			}
		}
	}
}

// The switch's contract: no password column, and the hash of whichever session
// is valid right now.
func TestTheDirectoryHandsOutASessionHashRatherThanAPassword(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cols := columnsOf(t, db, "luacc", "directory")
	if slicesContains(cols, "password") {
		t.Error("luacc.directory still hands FreeSWITCH a plaintext password")
	}
	if !slicesContains(cols, "a1_hash") {
		t.Fatalf("luacc.directory has %v, want an a1_hash", cols)
	}

	ids := seedAgentAtExtension(t, db, "1001")

	// No session: the number is in the directory, but nothing may register as
	// it. NULL is how Lua is told so.
	if got := directoryHash(t, db, "1001"); got != nil {
		t.Errorf("a1_hash = %q with no session at all, want NULL", *got)
	}

	const hash = "0123456789abcdef0123456789abcdef"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sip_sessions (agent_id, extension, a1_hash, expires_at)
		 VALUES ($1, '1001', $2, now() + interval '1 hour')`, ids.agentID, hash); err != nil {
		t.Fatalf("insert a live session: %v", err)
	}
	got := directoryHash(t, db, "1001")
	if got == nil || *got != hash {
		t.Errorf("a1_hash = %v, want the live session's %q", got, hash)
	}

	// Expiry is enforced by the view, so a credential stops working at its
	// expiry whether or not the sweeper has run.
	if _, err := db.ExecContext(ctx,
		`UPDATE sip_sessions SET expires_at = now() - interval '1 second' WHERE agent_id = $1`,
		ids.agentID); err != nil {
		t.Fatalf("expire the session: %v", err)
	}
	if got := directoryHash(t, db, "1001"); got != nil {
		t.Errorf("a1_hash = %q for an expired session, want NULL", *got)
	}
}

// A session belongs to an agent and goes when they do. Otherwise deleting an
// agent would leave a credential behind that still registers.
func TestASessionDiesWithItsAgent(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ids := seedAgentAtExtension(t, db, "1001")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sip_sessions (agent_id, extension, a1_hash, expires_at)
		 VALUES ($1, '1001', '0123456789abcdef0123456789abcdef', now() + interval '1 hour')`,
		ids.agentID); err != nil {
		t.Fatalf("insert a session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM agents WHERE id = $1`, ids.agentID); err != nil {
		t.Fatalf("delete the agent: %v", err)
	}

	var left int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sip_sessions`).Scan(&left); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if left != 0 {
		t.Errorf("%d session(s) outlived the agent they belong to", left)
	}
}

// Rolling 00031 back restores the column the switch used to read, on a
// database that already holds a session — which is the state a rollback finds
// in practice, and the one a naive DROP TABLE ordering would fail on.
func TestRollingBackTheSessionTableRestoresTheOldDirectory(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ids := seedAgentAtExtension(t, db, "1001")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sip_sessions (agent_id, extension, a1_hash, expires_at)
		 VALUES ($1, '1001', '0123456789abcdef0123456789abcdef', now() + interval '1 hour')`,
		ids.agentID); err != nil {
		t.Fatalf("insert a session: %v", err)
	}

	if err := goose.DownToContext(ctx, db, "migrations", 30); err != nil {
		t.Fatalf("rolling 00031 back on a database with sessions failed: %v", err)
	}
	cols := columnsOf(t, db, "luacc", "directory")
	if !slicesContains(cols, "password") {
		t.Errorf("after the rollback luacc.directory has %v, want the password column back", cols)
	}
	if len(columnsOf(t, db, "public", "sip_sessions")) != 0 {
		t.Error("sip_sessions survived the rollback")
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("re-applying failed: %v", err)
	}
}

type seededAgent struct{ extID, userID, agentID string }

// seedAgentAtExtension writes the smallest complete row set a session needs:
// an extension, an account, and the agent that binds them.
func seedAgentAtExtension(t *testing.T, db *sql.DB, number string) seededAgent {
	t.Helper()
	ids := seededAgent{
		extID:   "aaaaaaaa-0000-4000-8000-" + strings.Repeat("0", 11) + "1",
		userID:  "aaaaaaaa-0000-4000-8000-" + strings.Repeat("0", 11) + "2",
		agentID: "aaaaaaaa-0000-4000-8000-" + strings.Repeat("0", 11) + "3",
	}
	ctx := context.Background()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO extensions (id, number, password) VALUES ($1, $2, 'x')`,
			[]any{ids.extID, number}},
		{`INSERT INTO users (id, username, password_hash, display_name, role)
		  VALUES ($1, 'probe', 'x', 'Probe', 'AGENT')`, []any{ids.userID}},
		{`INSERT INTO agents (id, user_id, callcenter_name, default_extension_id)
		  VALUES ($1, $2, 'probe', $3)`, []any{ids.agentID, ids.userID, ids.extID}},
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed an agent at %s: %v", number, err)
		}
	}
	return ids
}

// directoryHash is what the switch would read for a number: the hash, or NULL.
func directoryHash(t *testing.T, db *sql.DB, number string) *string {
	t.Helper()
	var hash *string
	err := db.QueryRowContext(context.Background(),
		`SELECT a1_hash FROM luacc.directory WHERE number = $1`, number).Scan(&hash)
	if err != nil {
		t.Fatalf("read luacc.directory for %s: %v", number, err)
	}
	return hash
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
