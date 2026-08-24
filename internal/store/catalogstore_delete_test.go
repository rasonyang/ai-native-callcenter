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
