// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Migrations run at server startup, so a mistake in one is a boot failure for
// every deployment — and until this file existed nothing in the tree ran them
// at all. They were reviewed by reading, which cannot catch a constraint that
// PostgreSQL rejects or a Down block that does not reverse its Up.
//
// These tests need a real PostgreSQL: goose speaks to a server, and the whole
// point is to find what only a server can tell us. Set AICC_TEST_DATABASE_URL
// to a superuser-capable DSN — the dev stack's is
// postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable — and they run.
// Without it they skip, so `go test ./...` stays green on a machine with no
// database, at the cost of saying so loudly.
const testDSNEnv = "AICC_TEST_DATABASE_URL"

// scratchDB creates a throwaway database and returns a DSN for it. Each test
// gets its own, because migrating is a whole-database act and a shared one
// would make the tests order-dependent.
func scratchDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv(testDSNEnv)
	if admin == "" {
		t.Skipf("set %s to run the migration tests (see internal/store/migrate_test.go)", testDSNEnv)
	}

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("%s is not a URL: %v", testDSNEnv, err)
	}
	name := fmt.Sprintf("aicc_migtest_%d", time.Now().UnixNano())

	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})

	u.Path = "/" + name
	return u.String()
}

func openScratch(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// gooseFor points goose at the same embedded migrations the server runs, so
// this exercises the production path rather than a copy of it.
func gooseFor(t *testing.T) {
	t.Helper()
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
}

// The DDL executes against a real server, from nothing. This is the check that
// a migration reviewed only by reading has never had.
func TestMigrationsApplyFromZero(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)

	ctx := context.Background()
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a fresh database failed: %v", err)
	}

	version, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version == 0 {
		t.Fatal("migrations reported success but the database is still at version 0")
	}

	// Applying again must be a no-op, because every server start does it.
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("re-applying migrations failed: %v", err)
	}
}

// Every Down block reverses its Up. A Down that does not is only discovered
// when someone needs it, which is the worst moment to discover it.
func TestMigrationsRollBackAndReapply(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)

	ctx := context.Background()
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("initial migration failed: %v", err)
	}
	if err := goose.DownToContext(ctx, db, "migrations", 0); err != nil {
		t.Fatalf("rolling every migration back failed: %v", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("re-applying after a full rollback failed: %v", err)
	}
}

// A data migration has to run against data. A fresh database cannot detect the
// failure this guards: 00009 both narrows transcripts.speaker's CHECK and
// rewrites CALLER to CUSTOMER, and if the constraint were added before the
// rewrite — or the rewrite forgotten — PostgreSQL rejects the migration with
// "is violated by some row", but only on a database that already holds rows.
//
// The shape generalises: a migration that changes an enum's allowed values
// belongs here with a fixture of the old values, because inspection cannot
// tell you whether the statements are in the right order.
func TestMigrationsRewriteExistingRowsBeforeConstrainingThem(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	// Stop one short of the migration under test, so the fixture can be written
	// in the shape that migration expects to find.
	if err := goose.UpToContext(ctx, db, "migrations", 8); err != nil {
		t.Fatalf("migrating to 8 failed: %v", err)
	}

	const callID = "11111111-1111-1111-1111-111111111111"
	_, err := db.ExecContext(ctx, `
		INSERT INTO transcripts (call_id, seq, occurred_at, role, kind, content) VALUES
		($1, 1, now(), 'BOT',    'TEXT', '{"text":"thanks for calling"}'),
		($1, 2, now(), 'CALLER', 'TEXT', '{"text":"I need help"}'),
		($1, 3, now(), 'CALLER', 'TEXT', '{"text":"order 4471"}')`, callID)
	if err != nil {
		t.Fatalf("seed pre-migration rows: %v", err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database with history failed: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT speaker, count(*) FROM transcripts GROUP BY speaker ORDER BY speaker`)
	if err != nil {
		t.Fatalf("read migrated rows: %v", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var speaker string
		var n int
		if err := rows.Scan(&speaker, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[speaker] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if counts["CUSTOMER"] != 2 {
		t.Errorf("CUSTOMER rows = %d, want 2 — the CALLER rows were not rewritten", counts["CUSTOMER"])
	}
	if counts["BOT"] != 1 {
		t.Errorf("BOT rows = %d, want 1", counts["BOT"])
	}
	if n, ok := counts["CALLER"]; ok {
		t.Errorf("%d rows still say CALLER, which the enum can no longer express", n)
	}
	if total := counts["CUSTOMER"] + counts["BOT"]; total != 3 {
		t.Errorf("%d rows survived the migration, want 3", total)
	}
}

// A column with a default answers for rows that already exist, and the answer
// is wrong here: every wrap-up written before 00013 was an agent pressing the
// button, so it is confirmed by definition. Left to the default they would all
// read unconfirmed, and the day's completion rate would say nobody had ever
// filled one in.
//
// A fresh database cannot detect this. It needs rows from before.
func TestMigrationsBackfillConfirmedWrapUps(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 12); err != nil {
		t.Fatalf("migrating to 12 failed: %v", err)
	}
	const callID = "22222222-2222-2222-2222-222222222222"
	const agentID = "33333333-3333-3333-3333-333333333333"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wrap_ups (call_id, agent_id, disposition_code, disposition_label, note)
		VALUES ($1, $2, 'RESOLVED', 'Resolved', 'filed the old way')`, callID, agentID); err != nil {
		t.Fatalf("seed a pre-migration wrap-up: %v", err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database with history failed: %v", err)
	}

	var confirmed bool
	if err := db.QueryRowContext(ctx,
		`SELECT is_confirmed FROM wrap_ups WHERE call_id = $1`, callID).Scan(&confirmed); err != nil {
		t.Fatalf("read the migrated row: %v", err)
	}
	if !confirmed {
		t.Error("a wrap-up an agent filed reads as unconfirmed after the migration, " +
			"which makes every day before it look like nobody ever filled one in")
	}

	// And a row written afterwards starts unconfirmed, which is the point.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO wrap_ups (call_id, agent_id, disposition_code, disposition_label)
		VALUES ('44444444-4444-4444-4444-444444444444', $1, 'RESOLVED', 'Resolved')`, agentID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT is_confirmed FROM wrap_ups WHERE call_id = '44444444-4444-4444-4444-444444444444'`).
		Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed {
		t.Error("a record the platform opened reads as confirmed; nobody has looked at it yet")
	}
}

// The constraint the migration installs is the one the Go constants and the
// contract agree on. Read from the server rather than from the file, so a
// migration that silently failed to replace the CHECK is caught.
func TestSpeakerConstraintMatchesTheGoConstants(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	var clause string
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'transcripts'::regclass AND conname = 'transcripts_speaker_check'`).
		Scan(&clause)
	if err != nil {
		t.Fatalf("read the speaker constraint: %v", err)
	}

	for _, want := range []string{SpeakerCustomer, SpeakerBot, SpeakerHumanAgent} {
		if !strings.Contains(clause, "'"+want+"'") {
			t.Errorf("the database CHECK does not allow %s: %s", want, clause)
		}
	}
	if strings.Contains(clause, "'CALLER'") {
		t.Errorf("the database still allows CALLER: %s", clause)
	}
}

// bill_sec is backfilled from the answer the old ledger happened to record, so
// a call the switch answered carries a duration afterwards and one it never
// answered carries nothing. The fresh database cannot show this: the column
// has a default of 0 and every new row computes its own.
func TestMigrationsBackfillBillSec(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 13); err != nil {
		t.Fatalf("migrating to 13 failed: %v", err)
	}

	const (
		answered = "55555555-5555-5555-5555-555555555555"
		missed   = "66666666-6666-6666-6666-666666666666"
	)
	// A call the switch answered and that ran 90 seconds after the answer.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cdrs (call_id, started_at, answered_at, ended_at, call_type, status, hangup_cause)
		VALUES ($1, now() - interval '120 seconds', now() - interval '90 seconds', now(),
		        'INBOUND', 'ANSWERED', 'NORMAL_CLEARING')`, answered); err != nil {
		t.Fatalf("seed an answered call: %v", err)
	}
	// One the switch never answered at all.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status, hangup_cause)
		VALUES ($1, now() - interval '30 seconds', now(), 'OUTBOUND', 'NO_ANSWER', 'NO_ANSWER')`,
		missed); err != nil {
		t.Fatalf("seed an unanswered call: %v", err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database with history failed: %v", err)
	}

	var billed, unbilled int
	if err := db.QueryRowContext(ctx,
		`SELECT bill_sec FROM cdrs WHERE call_id = $1`, answered).Scan(&billed); err != nil {
		t.Fatalf("read the answered row: %v", err)
	}
	if billed < 89 || billed > 91 {
		t.Errorf("bill_sec = %d, want about 90 — the carrier billed from the answer, "+
			"and a history that reads 0 makes every earlier call look free", billed)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT bill_sec FROM cdrs WHERE call_id = $1`, missed).Scan(&unbilled); err != nil {
		t.Fatalf("read the unanswered row: %v", err)
	}
	if unbilled != 0 {
		t.Errorf("bill_sec = %d on a call the switch never answered, want 0", unbilled)
	}
}

// The only thing standing between a mistyped delete and a silently unbound
// agent was a foreign key, and it said SET NULL — do the damage quietly. This
// migration turns it into a refusal, and the case that matters is the one a
// fresh database cannot show: a deployment that already has agents bound to
// their phones must revalidate under the stricter rule without a rewrite.
func TestMigrationsRefuseToDeleteAnExtensionAnAgentWorksAt(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	// Stop short of the migration under test, so the binding exists before the
	// constraint changes underneath it.
	if err := goose.UpToContext(ctx, db, "migrations", 14); err != nil {
		t.Fatalf("migrating to 14 failed: %v", err)
	}

	const (
		extID   = "22222222-2222-2222-2222-222222222222"
		spareID = "33333333-3333-3333-3333-333333333333"
		userID  = "44444444-4444-4444-4444-444444444444"
		agentID = "55555555-5555-5555-5555-555555555555"
	)
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO extensions (id, number, kind, password)
		  VALUES ($1, '1099', 'AGENT', 'x'), ($2, '1098', 'AGENT', 'x')`, []any{extID, spareID}},
		{`INSERT INTO users (id, username, password_hash, display_name, role)
		  VALUES ($1, 'probe', 'x', 'Probe', 'AGENT')`, []any{userID}},
		{`INSERT INTO agents (id, user_id, callcenter_name, default_extension_id)
		  VALUES ($1, $2, 'probe', $3)`, []any{agentID, userID, extID}},
	}
	for _, s := range seed {
		if _, err := db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed a bound agent: %v", err)
		}
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database that already binds agents to phones failed: %v", err)
	}

	var bound string
	if err := db.QueryRowContext(ctx,
		`SELECT default_extension_id FROM agents WHERE id = $1`, agentID).Scan(&bound); err != nil {
		t.Fatalf("read the binding back: %v", err)
	}
	if bound != extID {
		t.Errorf("the binding changed to %s; the migration was supposed to rewrite nothing", bound)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM extensions WHERE id = $1`, extID); err == nil {
		t.Error("deleting an extension an agent works at succeeded; they were just unbound in silence")
	} else if !strings.Contains(err.Error(), "fk_agents_extensions") {
		t.Errorf("the delete failed for the wrong reason: %v", err)
	}

	// The refusal has to be about this binding and nothing else: an extension
	// nobody works at still deletes, or the guard would be an obstruction.
	if _, err := db.ExecContext(ctx, `DELETE FROM extensions WHERE id = $1`, spareID); err != nil {
		t.Errorf("deleting an unused extension was refused: %v", err)
	}

	// And unbinding is the deliberate act that makes the delete possible.
	if _, err := db.ExecContext(ctx,
		`UPDATE agents SET default_extension_id = NULL WHERE id = $1`, agentID); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM extensions WHERE id = $1`, extID); err != nil {
		t.Errorf("the extension could not be deleted after the agent was unbound: %v", err)
	}
}

// 00017 widens the extension vocabulary and adds two target columns to a table
// that already holds rows.
//
// A fresh database cannot see what this checks. Every existing extension is an
// AGENT with no target, so it must revalidate under the new kind/target CHECK
// untouched — and the constraint has to accept "no target at all", because
// that is what every row in every deployment currently is.
func TestMigrationsLetExistingExtensionsKeepTheirLabels(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 16); err != nil {
		t.Fatalf("migrating to 16 failed: %v", err)
	}

	const (
		agentExt = "66666666-6666-6666-6666-666666666666"
		plainExt = "77777777-7777-7777-7777-777777777777"
	)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO extensions (id, number, kind, password)
		 VALUES ($1, '1097', 'AGENT', 'x'), ($2, '1096', 'PLAIN', 'x')`,
		agentExt, plainExt); err != nil {
		t.Fatalf("seed extensions of the old vocabulary: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "migrations", 17); err != nil {
		t.Fatalf("migrating a database that already holds extensions failed: %v", err)
	}

	var kinds []string
	rows, err := db.QueryContext(ctx,
		`SELECT kind FROM extensions WHERE id IN ($1, $2) ORDER BY number DESC`, agentExt, plainExt)
	if err != nil {
		t.Fatalf("read the kinds back: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan: %v", err)
		}
		kinds = append(kinds, kind)
	}
	if len(kinds) != 2 || kinds[0] != "AGENT" || kinds[1] != "PLAIN" {
		t.Errorf("kinds = %v, want AGENT and PLAIN — the migration relabelled rows "+
			"it was only supposed to make room beside", kinds)
	}

	// The vocabulary is wider, and the CHECK still refuses a row that claims
	// two things at once.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO extensions (id, number, kind, password)
		 VALUES (gen_random_uuid(), '1095', 'QUEUE', 'x')`); err != nil {
		t.Errorf("a QUEUE extension was refused: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO extensions (id, number, kind, password, queue_id)
		 VALUES (gen_random_uuid(), '1094', 'BOT', 'x', gen_random_uuid())`); err == nil {
		t.Error("a BOT extension was allowed to point at a queue; kind and target " +
			"can now disagree, and whichever one a reader trusts is a coin toss")
	}
}

// 00018 narrows the vocabulary, and a narrowing is the one shape that fails on
// a populated database and passes on a fresh one.
//
// A deployment that labelled a phone BOT or PLAIN must migrate rather than be
// refused with "is violated by some row" on every boot. Both become AGENT,
// which is what they were doing anyway — a registerable number, bound to
// somebody or not.
func TestMigrationsRelabelTheKindsThatNoLongerExist(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 17); err != nil {
		t.Fatalf("migrating to 17 failed: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO extensions (id, number, kind, password)
		 VALUES (gen_random_uuid(), '1093', 'BOT', 'x'),
		        (gen_random_uuid(), '1092', 'PLAIN', 'x'),
		        (gen_random_uuid(), '1091', 'AGENT', 'x')`); err != nil {
		t.Fatalf("seed the retired kinds: %v", err)
	}

	// Stopped at 18 because 19 removes the column this checks: what 18 owes
	// is that a populated database survives the narrowing, and that is a claim
	// about 18 alone.
	if err := goose.UpToContext(ctx, db, "migrations", 18); err != nil {
		t.Fatalf("migrating a database that still labels phones BOT or PLAIN failed: %v", err)
	}

	var others int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM extensions WHERE kind <> 'AGENT'`).Scan(&others); err != nil {
		t.Fatalf("count: %v", err)
	}
	if others != 0 {
		t.Errorf("%d extensions kept a kind that no longer exists", others)
	}

	// The narrowed CHECK holds, and a queue target still needs its kind.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO extensions (id, number, kind, password)
		 VALUES (gen_random_uuid(), '1090', 'BOT', 'x')`); err == nil {
		t.Error("a BOT extension was accepted after the kind was retired")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO extensions (id, number, kind, password, queue_id)
		 VALUES (gen_random_uuid(), '1089', 'AGENT', 'x', gen_random_uuid())`); err == nil {
		t.Error("an AGENT extension was allowed to point at a queue")
	}
}

// 00019 removes kind and queue_id from a table that already holds rows.
//
// The claim is that nothing else moves: an extension is its number, its
// password and who it belongs to, and dropping the two columns must leave
// every binding exactly where it was.
func TestMigrationsDropTheColumnsWithoutLosingABinding(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 18); err != nil {
		t.Fatalf("migrating to 18 failed: %v", err)
	}
	const (
		extID   = "88888888-8888-8888-8888-888888888888"
		userID  = "99999999-9999-9999-9999-999999999999"
		agentID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO extensions (id, number, kind, password) VALUES ($1, '1088', 'AGENT', 'x')`,
			[]any{extID}},
		{`INSERT INTO users (id, username, password_hash, display_name, role)
		  VALUES ($1, 'probe19', 'x', 'Probe', 'AGENT')`, []any{userID}},
		{`INSERT INTO agents (id, user_id, callcenter_name, default_extension_id)
		  VALUES ($1, $2, 'probe19', $3)`, []any{agentID, userID, extID}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database that already holds bound phones failed: %v", err)
	}

	var bound string
	if err := db.QueryRowContext(ctx,
		`SELECT default_extension_id FROM agents WHERE id = $1`, agentID).Scan(&bound); err != nil {
		t.Fatalf("read the binding back: %v", err)
	}
	if bound != extID {
		t.Errorf("the binding is now %s; dropping two unrelated columns moved it", bound)
	}

	// The switch still gets the phone it has always got.
	var served int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM luacc.directory WHERE number = '1088'`).Scan(&served); err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if served != 1 {
		t.Errorf("the directory serves %d rows for 1088; a phone stopped being "+
			"registerable because two columns it never used went away", served)
	}
}

// 00020 narrows what a number may be, on a database that already holds one the
// new rule forbids.
//
// A fresh database has no flowless number, so this is the only place the
// rewrite is exercised: without it every deployment holding one is refused
// with "is violated by some row" on every boot.
func TestMigrationsGiveAFlowlessNumberTheOnlyShapeItMayHave(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 19); err != nil {
		t.Fatalf("migrating to 19 failed: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dids (id, number, language, is_enabled)
		 VALUES (gen_random_uuid(), '95099', 'zh', true)`); err != nil {
		t.Fatalf("seed a flowless number: %v", err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database that holds a flowless number failed: %v", err)
	}

	var inbound, outbound bool
	if err := db.QueryRowContext(ctx,
		`SELECT allow_inbound, allow_outbound FROM dids WHERE number = '95099'`).
		Scan(&inbound, &outbound); err != nil {
		t.Fatalf("read the number back: %v", err)
	}
	if inbound || !outbound {
		t.Errorf("95099 is inbound=%v outbound=%v — a number with no flow cannot "+
			"claim to take calls it has nothing to answer with", inbound, outbound)
	}

	// The rules hold from here on, against psql as well as the API.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dids (id, number, language, allow_inbound)
		 VALUES (gen_random_uuid(), '95098', 'en', true)`); err == nil {
		t.Error("an inbound number was accepted with no flow to answer with")
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound)
		 VALUES (gen_random_uuid(), '95097', 'en', false, false)`); err == nil {
		t.Error("a number was accepted that calls cannot go through in either direction")
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE dids SET is_default_outbound = true WHERE number = '95099'`); err != nil {
		t.Fatalf("mark the only outbound number as the default: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO dids (id, number, language, allow_inbound, allow_outbound, is_default_outbound)
		 VALUES (gen_random_uuid(), '95096', 'en', false, true, true)`); err == nil {
		t.Error("a second default outbound number was accepted; which one a call " +
			"comes from would depend on the order rows are read in")
	}
}

// 00023 narrows missed_reason, on a database that already holds the value it
// removes.
//
// No deployment can actually hold one — nothing has ever decided OUT_OF_HOURS,
// which is why it is going — but a CHECK is a claim about a table rather than
// about the code that filled it, and narrowing one without the rewrite in
// front is the mistake this file exists to catch. The fixture makes the claim
// false first, so the rewrite is the thing being tested rather than a
// statement nobody reaches.
func TestMigrationsDropAReasonNoCallCanHave(t *testing.T) {
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, "migrations", 22); err != nil {
		t.Fatalf("migrating to 22 failed: %v", err)
	}
	const closed = "77777777-7777-7777-7777-777777777777"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status, missed_reason)
		VALUES ($1, now() - interval '10 seconds', now(), 'INBOUND', 'NO_ANSWER', 'OUT_OF_HOURS')`,
		closed); err != nil {
		t.Fatalf("seed a call filed under the departing reason: %v", err)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrating a database holding OUT_OF_HOURS failed: %v", err)
	}

	var reason, status *string
	if err := db.QueryRowContext(ctx,
		`SELECT missed_reason, status FROM cdrs WHERE call_id = $1`, closed).
		Scan(&reason, &status); err != nil {
		t.Fatalf("read the migrated row: %v", err)
	}
	if reason != nil {
		t.Errorf("missed_reason = %q, want none — the word is gone from the vocabulary", *reason)
	}
	// The call is still a missed call. Only the reason for it was withdrawn.
	if status == nil || *status != "NO_ANSWER" {
		t.Errorf("status = %v, want NO_ANSWER — the migration rewrites the reason, not the verdict", status)
	}

	// And the narrowed rule holds against psql, not only against the API.
	for _, reason := range []string{
		"SHORT_ABANDONED", "ABANDONED_RINGING", "ABANDONED_WAITING",
		"AGENTS_DID_NOT_ANSWER", "NO_AVAILABLE_AGENT",
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status, missed_reason)
			VALUES (gen_random_uuid(), now(), now(), 'INBOUND', 'NO_ANSWER', $1)`, reason); err != nil {
			t.Errorf("a call could not be filed under %s, which the assembler decides: %v", reason, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO cdrs (call_id, started_at, ended_at, call_type, status, missed_reason)
		VALUES (gen_random_uuid(), now(), now(), 'INBOUND', 'NO_ANSWER', 'OUT_OF_HOURS')`); err == nil {
		t.Error("OUT_OF_HOURS was accepted after the migration that removed it")
	}
}
