// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// Deleting a row that is not there is not an error to PostgreSQL, so every
// catalogue delete reported success and the 404 the contract declares was
// unreachable code (C34). An operator who mistyped an id was told the
// extension was gone.
//
// This needs a real server: the whole defect is what `DELETE … WHERE id = $1`
// does when it matches nothing, which no fake reproduces. Set
// AICC_TEST_DATABASE_URL to run it (see migrate_test.go); without it, it skips.
func TestDeletingWhatIsNotThereSaysSoRatherThanReportingSuccess(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	cat := st.Catalog()
	absent := uuid.New()

	for _, tc := range []struct {
		name string
		del  func() error
	}{
		{"extension", func() error { return cat.DeleteExtension(ctx, absent) }},
		{"queue", func() error { return cat.DeleteQueue(ctx, absent) }},
		{"did", func() error { return cat.DeleteDID(ctx, absent) }},
		{"queue staffing", func() error { return cat.RemoveQueueAgent(ctx, absent, absent) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.del(); !errors.Is(err, catalog.ErrNotFound) {
				t.Errorf("error = %v, want catalog.ErrNotFound — nothing was removed", err)
			}
		})
	}

	t.Run("agent", func(t *testing.T) {
		// The agents package speaks pgx.ErrNoRows where the catalogue speaks
		// ErrNotFound; both reach 404, each through its own handler.
		if err := st.Agents().DeleteAgent(ctx, absent); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("error = %v, want pgx.ErrNoRows — no agent was removed", err)
		}
	})
}

// C7's read path, against a real server: the index is partial and the ordering
// is (occurred_at, id), neither of which a fake reproduces. The rows in this
// fixture are the shape that made the read worth building — one call offered
// to three agents in turn before being abandoned, which the CDR would record
// as a single missed reason.
func TestACallCanBeAskedForItsQueueJourney(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	ledger := st.Ledger()
	callID, queueID := uuid.New(), uuid.New()
	otherCall := uuid.New()
	agentA, agentB := uuid.New(), uuid.New()
	at := func(sec int) time.Time {
		return time.Date(2026, 8, 24, 10, 0, sec, 0, time.UTC)
	}

	// Deliberately inserted out of order, so the ordering is the query's doing.
	for _, ev := range []struct {
		at    time.Time
		name  string
		agent *uuid.UUID
		wait  int
	}{
		{at(30), "ABANDONED", nil, 30000},
		{at(0), "JOINED", nil, 0},
		{at(10), "OFFERED", &agentA, 10000},
		{at(20), "OFFERED", &agentB, 20000},
	} {
		if err := ledger.InsertQueueEvent(ctx, ev.at, &callID, queueID, ev.name, ev.agent, ev.wait); err != nil {
			t.Fatalf("insert %s: %v", ev.name, err)
		}
	}
	// Another call's movement, and one the switch could not attribute at all.
	if err := ledger.InsertQueueEvent(ctx, at(5), &otherCall, queueID, "JOINED", nil, 0); err != nil {
		t.Fatalf("insert other call: %v", err)
	}
	if err := ledger.InsertQueueEvent(ctx, at(6), nil, queueID, "JOINED", nil, 0); err != nil {
		t.Fatalf("insert unattributed: %v", err)
	}

	got, err := ledger.QueueEventsByCall(ctx, callID)
	if err != nil {
		t.Fatalf("read the journey: %v", err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.Event)
	}
	want := []string{"JOINED", "OFFERED", "OFFERED", "ABANDONED"}
	if len(names) != len(want) {
		t.Fatalf("journey = %v, want %v — another call's movements or an "+
			"unattributed one leaked in, or the call's own did not", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("journey = %v, want %v — oldest first", names, want)
		}
	}
	// The offers name who declined, which is the whole reason to read this
	// rather than the CDR: the CDR keeps one missed reason for all of them.
	if got[1].AgentID == nil || *got[1].AgentID != agentA {
		t.Errorf("first offer names %v, want agent A", got[1].AgentID)
	}
	if got[2].AgentID == nil || *got[2].AgentID != agentB {
		t.Errorf("second offer names %v, want agent B", got[2].AgentID)
	}
	if got[3].WaitMs != 30000 {
		t.Errorf("waitMs on the abandon = %d, want 30000", got[3].WaitMs)
	}

	// A call that never entered a queue has no journey, and that is an answer.
	empty, err := ledger.QueueEventsByCall(ctx, uuid.New())
	if err != nil {
		t.Fatalf("read an empty journey: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("a call with no queue movements returned %d", len(empty))
	}
}

// W4's read path, against a real server: the filters are SQL, the actor's name
// comes from a LEFT JOIN, and "newest first" has to survive rows that share a
// timestamp — none of which a fake reproduces.
func TestTheAuditTrailCanBeSearched(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	ledger := st.Ledger()

	// One real account and one that will be gone by the time anybody reads.
	actor := uuid.New()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO users (id, username, password_hash, display_name, role)
		 VALUES ($1,'auditadmin','x','Audit Admin','ADMIN')`, actor); err != nil {
		t.Fatalf("seed the actor: %v", err)
	}
	ghost := uuid.New()

	at := func(sec int) time.Time { return time.Date(2026, 8, 24, 9, 0, sec, 0, time.UTC) }
	for _, row := range []struct {
		at     time.Time
		who    *uuid.UUID
		action string
	}{
		{at(10), &actor, "PUT /api/v1/queues/{queueId}"},
		{at(20), &actor, "DELETE /api/v1/extensions/{extensionId}"},
		{at(30), &ghost, "PUT /api/v1/queues/{queueId}"},
		{at(30), nil, "POST /api/v1/calls"},
	} {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO audit_logs (occurred_at, actor_id, action, target_kind, target_id, detail)
			 VALUES ($1,$2,$3,'queue','q-1','{"request":"{}"}')`,
			row.at, row.who, row.action); err != nil {
			t.Fatalf("seed %s: %v", row.action, err)
		}
	}

	t.Run("newest first, and the total counts the filter not the page", func(t *testing.T) {
		rows, total, err := ledger.ListAuditLogs(ctx, AuditFilter{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if total != 4 {
			t.Errorf("total = %d, want 4 — the total is of the filter, not of the page", total)
		}
		if len(rows) != 2 {
			t.Fatalf("returned %d rows for limit 2", len(rows))
		}
		if !rows[0].OccurredAt.Equal(at(30)) || !rows[1].OccurredAt.Equal(at(30)) {
			t.Errorf("the first page is not the newest two: %v / %v",
				rows[0].OccurredAt, rows[1].OccurredAt)
		}
	})

	t.Run("an action prefix selects by method or by resource", func(t *testing.T) {
		for prefix, want := range map[string]int{
			"DELETE ":                1,
			"PUT /api/v1/queues":     2,
			"POST":                   1,
			"PUT /api/v1/extensions": 0,
		} {
			_, total, err := ledger.ListAuditLogs(ctx, AuditFilter{ActionPrefix: prefix, Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if int(total) != want {
				t.Errorf("prefix %q matched %d, want %d", prefix, total, want)
			}
		}
	})

	t.Run("a window is inclusive at the bottom and exclusive at the top", func(t *testing.T) {
		_, total, err := ledger.ListAuditLogs(ctx,
			AuditFilter{From: at(20), To: at(30), Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 {
			t.Errorf("window [20,30) matched %d, want 1 — the two at :30 are above it", total)
		}
	})

	t.Run("a deleted account does not erase what it did", func(t *testing.T) {
		rows, total, err := ledger.ListAuditLogs(ctx, AuditFilter{ActorID: &ghost, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if total != 1 || len(rows) != 1 {
			t.Fatalf("the ghost's row is gone: total=%d rows=%d", total, len(rows))
		}
		if rows[0].ActorUsername != "" {
			t.Errorf("resolved a username for an account that does not exist: %q", rows[0].ActorUsername)
		}
		if rows[0].ActorID == nil || *rows[0].ActorID != ghost {
			t.Error("the row lost the id, which is the only thing left identifying who acted")
		}
	})

	t.Run("the actor's name is resolved for one that still exists", func(t *testing.T) {
		rows, _, err := ledger.ListAuditLogs(ctx, AuditFilter{ActorID: &actor, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 || rows[0].ActorUsername != "auditadmin" {
			t.Errorf("actorUsername not resolved: %+v", rows)
		}
	})
}
