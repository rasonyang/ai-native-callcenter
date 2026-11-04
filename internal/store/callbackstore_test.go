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
