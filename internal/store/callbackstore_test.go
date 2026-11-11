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

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// A dial made from a callback is noted on it at once and learns its outcome
// when the call's CDR lands — without the callback closing, because ringing
// somebody is not the same as keeping the promise. Needs a real server: the
// outcome is written in the CDR's own transaction. Set AICC_TEST_DATABASE_URL
// to run it (see migrate_test.go); without it, it skips.
func TestACallbackRemembersHowItsLastAttemptWent(t *testing.T) {
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

	var announced []Callback
	st.OnCallbackSettled = func(cb Callback) { announced = append(announced, cb) }
	ledger := st.Ledger()

	holder, stranger := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	cb, err := ledger.InsertCallback(ctx, nil, nil, "18600000000", "call me back")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Nobody has claimed it: a dial cannot be recorded against it.
	if _, err := ledger.MarkCallbackAttempt(ctx, cb.ID, uuid.Must(uuid.NewV7()), holder); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("attempt on an unclaimed callback: got %v, want no rows", err)
	}
	if _, err := ledger.ClaimCallback(ctx, cb.ID, holder); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Somebody else's claim is not this caller's to dial from either.
	if _, err := ledger.MarkCallbackAttempt(ctx, cb.ID, uuid.Must(uuid.NewV7()), stranger); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("attempt by a non-holder: got %v, want no rows", err)
	}

	callID := uuid.Must(uuid.NewV7())
	marked, err := ledger.MarkCallbackAttempt(ctx, cb.ID, callID, holder)
	if err != nil {
		t.Fatalf("mark attempt: %v", err)
	}
	if marked.Status != "CLAIMED" || marked.LastAttemptCallID == nil || *marked.LastAttemptCallID != callID ||
		marked.LastAttemptAt == nil || marked.LastAttemptStatus != "" {
		t.Fatalf("after marking: %+v", marked)
	}

	ended := time.Now().UTC().Truncate(time.Second)
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: callID, StartedAt: ended.Add(-30 * time.Second), EndedAt: ended,
		CallType: "OUTBOUND", Language: "zh", FromNumber: "1001", ToNumber: "18600000000",
		Status: CDRStatusNoAnswer, HangupCause: "NO_USER_RESPONSE",
	}); err != nil {
		t.Fatalf("insert cdr: %v", err)
	}

	rows, _, err := ledger.ListCallbacks(ctx, "", nil, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var after Callback
	for _, row := range rows {
		if row.ID == cb.ID {
			after = row
		}
	}
	if after.Status != "CLAIMED" {
		t.Fatalf("a call that went unanswered must not close the callback: %+v", after)
	}
	if after.LastAttemptStatus != CDRStatusNoAnswer || after.LastAttemptAt == nil || !after.LastAttemptAt.Equal(ended) {
		t.Fatalf("outcome not copied back: %+v", after)
	}
	if len(announced) != 1 || announced[0].ID != cb.ID || announced[0].LastAttemptStatus != CDRStatusNoAnswer {
		t.Fatalf("the settled callback was not announced once: %+v", announced)
	}
}

// openCallbackStore is a migrated scratch database with the store on it.
func openCallbackStore(t *testing.T) *Store {
	t.Helper()
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
	return st
}

// claimedForTest is a callback claimed by a fresh holder.
func claimedForTest(t *testing.T, ledger *LedgerStore) (Callback, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	holder := uuid.Must(uuid.NewV7())
	cb, err := ledger.InsertCallback(ctx, nil, nil, "18600000000", "call me back")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if cb, err = ledger.ClaimCallback(ctx, cb.ID, holder); err != nil {
		t.Fatalf("claim: %v", err)
	}
	return cb, holder
}

// The attempt is written once the dial has placed the call (#478), and a call
// can end before that: an originate the switch fails at once has its
// placement-failure CDR written from another goroutine while the request is
// still on its way to the mark. The CDR then finds no callback to settle, so
// the mark takes the outcome from it, exactly as the settle would have
// written it, instead of leaving the attempt without one for good.
func TestAMarkWrittenAfterItsCDRCarriesTheOutcome(t *testing.T) {
	ctx := context.Background()
	st := openCallbackStore(t)
	var announced []Callback
	st.OnCallbackSettled = func(cb Callback) { announced = append(announced, cb) }
	ledger := st.Ledger()
	cb, holder := claimedForTest(t, ledger)

	callID := uuid.Must(uuid.NewV7())
	ended := time.Now().UTC().Truncate(time.Millisecond)
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: callID, StartedAt: ended.Add(-2 * time.Second), EndedAt: ended,
		CallType: "OUTBOUND", FromNumber: "1008", ToNumber: "18600000000",
		Status: CDRStatusFailed, HangupCause: "USER_NOT_REGISTERED",
		Tech: map[string]any{"isPlacementFailure": true},
	}); err != nil {
		t.Fatalf("insert cdr: %v", err)
	}
	if len(announced) != 0 {
		t.Fatalf("a CDR settled a callback nobody had marked: %+v", announced)
	}

	marked, err := ledger.MarkCallbackAttempt(ctx, cb.ID, callID, holder)
	if err != nil {
		t.Fatalf("mark attempt: %v", err)
	}
	for name, got := range map[string]Callback{"returned": marked, "listed": listedCallback(t, ledger, cb.ID)} {
		if got.LastAttemptCallID == nil || *got.LastAttemptCallID != callID ||
			got.LastAttemptStatus != CDRStatusFailed || got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(ended) {
			t.Errorf("%s: %+v, want %s FAILED at %v", name, got, callID, ended)
		}
		if got.Status != "CLAIMED" || got.HandledBy == nil || *got.HandledBy != holder {
			t.Errorf("%s: %+v, want still CLAIMED by %s", name, got, holder)
		}
	}
}

// The CDR and the mark can be written at the same moment, and under READ
// COMMITTED one interleaving loses the outcome unless they are serialised:
// the CDR's transaction has written its row and looked for a callback marked
// with the call (none yet), and has not committed when the mark is written.
// A mark that read the CDR there would not see it, and the settle that has
// run would not see the mark. This holds the CDR's transaction open at that
// point, exactly as InsertCDR leaves it after its settle, and the mark waits
// for it to commit and then reads its outcome.
func TestAMarkRacingItsCDRStillTakesTheOutcome(t *testing.T) {
	ctx := context.Background()
	st := openCallbackStore(t)
	ledger := st.Ledger()
	cb, holder := claimedForTest(t, ledger)
	callID := uuid.Must(uuid.NewV7())
	ended := time.Now().UTC().Truncate(time.Millisecond)

	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := queries.New(tx)
	if _, err := q.InsertCDR(ctx, queries.InsertCDRParams{
		CallID: callID, StartedAt: stamp(ended.Add(-30 * time.Second)), EndedAt: stamp(ended),
		CallType: "OUTBOUND", FromNumber: "1008", ToNumber: "18600000000",
		AgentIds: []uuid.UUID{}, Status: CDRStatusNoAnswer, HangupCause: "NO_ANSWER",
		UserData: []byte("{}"), Tech: []byte("{}"), Legs: []byte("[]"), UnbackedClaims: []string{},
	}); err != nil {
		t.Fatalf("insert cdr: %v", err)
	}
	if err := lockCallAttempt(ctx, q, callID); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if rows, err := q.SettleCallbackAttempt(ctx, queries.SettleCallbackAttemptParams{CallID: callID}); err != nil || len(rows) != 0 {
		t.Fatalf("settle before the mark: %d rows, %v", len(rows), err)
	}

	type result struct {
		cb  Callback
		err error
	}
	done := make(chan result, 1)
	go func() {
		marked, err := ledger.MarkCallbackAttempt(ctx, cb.ID, callID, holder)
		done <- result{marked, err}
	}()

	// Commit once the mark is waiting for this transaction; a mark that does
	// not wait (the defect) finishes instead, and the assertions below fail.
	var early *result
	deadline := time.Now().Add(10 * time.Second)
	for early == nil && !isWaitingForCallAttemptLock(t, st) {
		if time.Now().After(deadline) {
			t.Fatal("the mark neither finished nor waited for the CDR's transaction")
		}
		select {
		case r := <-done:
			early = &r
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the CDR: %v", err)
	}
	r := early
	if r == nil {
		select {
		case got := <-done:
			r = &got
		case <-time.After(10 * time.Second):
			t.Fatal("the mark never finished")
		}
	}
	if r.err != nil {
		t.Fatalf("mark attempt: %v", r.err)
	}
	for name, got := range map[string]Callback{"returned": r.cb, "listed": listedCallback(t, ledger, cb.ID)} {
		if got.LastAttemptCallID == nil || *got.LastAttemptCallID != callID ||
			got.LastAttemptStatus != CDRStatusNoAnswer || got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(ended) {
			t.Errorf("%s: %+v, want %s NO_ANSWER at %v", name, got, callID, ended)
		}
	}
}

// isWaitingForCallAttemptLock reports whether a session of this test's
// database is waiting for a call attempt's lock.
func isWaitingForCallAttemptLock(t *testing.T, st *Store) bool {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted AND objsubid = 2 AND classid = $1::int::oid
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		callAttemptLockClass).Scan(&n); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return n > 0
}

func listedCallback(t *testing.T, ledger *LedgerStore, id uuid.UUID) Callback {
	t.Helper()
	rows, _, err := ledger.ListCallbacks(context.Background(), "", nil, 200, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("callback %s not listed", id)
	return Callback{}
}

// The total of a callback page is the count of the rows matching its filters,
// whatever the page holds (#469).
func TestCallbackCountAgreesWithTheListUnderEveryFilter(t *testing.T) {
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

	holder := uuid.Must(uuid.NewV7())
	callID := uuid.Must(uuid.NewV7())
	plan := map[string]int{"OPEN": 7, "CLAIMED": 4, "DONE": 3, "DISMISSED": 2}
	for _, status := range []string{"OPEN", "CLAIMED", "DONE", "DISMISSED"} {
		for i := 0; i < plan[status]; i++ {
			var cid *uuid.UUID
			if status == "DONE" && i == 0 {
				cid = &callID
			}
			cb, err := ledger.InsertCallback(ctx, cid, nil, "18600000000", "m")
			if err != nil {
				t.Fatal(err)
			}
			switch status {
			case "CLAIMED":
				_, err = ledger.ClaimCallback(ctx, cb.ID, holder)
			case "DONE", "DISMISSED":
				_, err = ledger.HandleCallback(ctx, cb.ID, status, holder)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	cases := []struct {
		name   string
		status string
		callID *uuid.UUID
		want   int64
	}{
		{"none", "", nil, 16},
		{"OPEN", "OPEN", nil, 7},
		{"CLAIMED", "CLAIMED", nil, 4},
		{"DONE", "DONE", nil, 3},
		{"DISMISSED", "DISMISSED", nil, 2},
		{"a call", "", &callID, 1},
		{"a call and a status that excludes it", "OPEN", &callID, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, total, err := ledger.ListCallbacks(ctx, tc.status, tc.callID, 200, 0)
			if err != nil {
				t.Fatal(err)
			}
			if total != tc.want || int64(len(rows)) != tc.want {
				t.Errorf("total %d, %d rows, want %d", total, len(rows), tc.want)
			}
			page, total2, err := ledger.ListCallbacks(ctx, tc.status, tc.callID, 3, 0)
			if err != nil || total2 != tc.want || len(page) > 3 {
				t.Errorf("a page of 3: total %d, %d rows, err %v", total2, len(page), err)
			}
			none, total3, err := ledger.ListCallbacks(ctx, tc.status, tc.callID, 0, 0)
			if err != nil || total3 != tc.want || int64(len(none)) != min(tc.want, 50) {
				t.Errorf("limit 0 is the default page: total %d, %d rows, err %v", total3, len(none), err)
			}
		})
	}
}
