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

	holder, stranger := uuid.New(), uuid.New()
	cb, err := ledger.InsertCallback(ctx, nil, nil, "18600000000", "call me back")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Nobody has claimed it: a dial cannot be recorded against it.
	if _, err := ledger.MarkCallbackAttempt(ctx, cb.ID, uuid.New(), holder); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("attempt on an unclaimed callback: got %v, want no rows", err)
	}
	if _, err := ledger.ClaimCallback(ctx, cb.ID, holder); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Somebody else's claim is not this caller's to dial from either.
	if _, err := ledger.MarkCallbackAttempt(ctx, cb.ID, uuid.New(), stranger); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("attempt by a non-holder: got %v, want no rows", err)
	}

	callID := uuid.New()
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

	rows, err := ledger.ListCallbacks(ctx, "", 10, 0)
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
