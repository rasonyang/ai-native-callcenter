// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// MaxTestNumbers is how many numbers the list of test numbers may hold. Every
// entry gives up the cross-campaign frequency protection for its number, a
// protection that exists for customers, so the list stays small.
const MaxTestNumbers = 100

// MaxTestNumberNote is the longest note, in characters.
const MaxTestNumberNote = 200

// testNumberLockKey serialises the adds to the list. It shares the advisory
// lock keyspace with advisoryLockKey and extensionPoolLockKey and must differ
// from both.
const testNumberLockKey int64 = 0x414943430003 // "AICC" + 3

// TestNumberStore is the list of test numbers: numbers the cross-campaign
// frequency window does not hold back. Every number it holds is canonical E.164.
type TestNumberStore struct {
	pool    *pgxpool.Pool
	q       *queries.Queries
	onAdded func()
}

// TestNumbers returns the list of test numbers.
func (s *Store) TestNumbers() *TestNumberStore {
	return &TestNumberStore{pool: s.Pool, q: s.Queries, onAdded: s.OnTestNumberAdded}
}

// TestNumber is one entry on the list.
type TestNumber struct {
	Number    string     `json:"number"`
	Note      string     `json:"note,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	CreatedBy *uuid.UUID `json:"createdBy,omitempty"`
}

// TestNumberFilter narrows a listing. Prefix is a canonical-number prefix.
type TestNumberFilter struct {
	Prefix string
	Limit  int
	Offset int
}

// ErrTestNumberNotFound means the number is not on the list.
var ErrTestNumberNotFound = errors.New("that number is not on the list of test numbers")

// ErrTestNumberInvalid means the number is not canonical E.164.
var ErrTestNumberInvalid = errors.New("not a canonical E.164 number")

// ErrTestNumberListFull means the list already holds MaxTestNumbers numbers and
// the one being added is not among them.
var ErrTestNumberListFull = errors.New("the list of test numbers is full")

// Add puts a canonical number on the list. isNew is false when it was already
// there; the stored entry (original note, time and author) is returned
// untouched, so adding is idempotent, a full list included. A new number is
// refused with ErrTestNumberListFull when MaxTestNumbers are listed; the adds
// are serialised, so two of them racing at the last place cannot both land.
func (t *TestNumberStore) Add(ctx context.Context, number, note string, by *uuid.UUID) (entry TestNumber, isNew bool, err error) {
	if !phonenum.IsCanonical(number) {
		return TestNumber{}, false, ErrTestNumberInvalid
	}
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return TestNumber{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := t.q.WithTx(tx)
	// The lock is taken before the statement that counts, so that statement's
	// snapshot already holds whatever the previous adder committed.
	if err := q.LockTestNumbers(ctx, testNumberLockKey); err != nil {
		return TestNumber{}, false, err
	}
	var notePtr *string
	if n := strings.TrimSpace(note); n != "" {
		notePtr = &n
	}
	row, err := q.InsertTestNumber(ctx, queries.InsertTestNumberParams{
		NumberE164: number, Note: notePtr, CreatedBy: by, MaxEntries: MaxTestNumbers,
	})
	switch {
	case err == nil:
		if err := tx.Commit(ctx); err != nil {
			return TestNumber{}, false, err
		}
		if t.onAdded != nil {
			t.onAdded()
		}
		return testNumberFromRow(row), true, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Already listed, or the list is full.
		existing, err := q.GetTestNumber(ctx, number)
		if errors.Is(err, pgx.ErrNoRows) {
			return TestNumber{}, false, ErrTestNumberListFull
		}
		if err != nil {
			return TestNumber{}, false, err
		}
		return testNumberFromRow(existing), false, nil
	default:
		return TestNumber{}, false, err
	}
}

// Remove takes a canonical number off the list.
func (t *TestNumberStore) Remove(ctx context.Context, number string) error {
	n, err := t.q.DeleteTestNumber(ctx, number)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTestNumberNotFound
	}
	return nil
}

// List pages the list, newest first, with the total for the filter.
func (t *TestNumberStore) List(ctx context.Context, f TestNumberFilter) ([]TestNumber, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	prefix := strings.TrimSpace(f.Prefix)
	rows, err := t.q.ListTestNumbers(ctx, queries.ListTestNumbersParams{
		Prefix: prefix, PageLimit: int32(f.Limit), PageOffset: int32(f.Offset),
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.CountTestNumbers(ctx, prefix)
	if err != nil {
		return nil, 0, err
	}
	out := make([]TestNumber, 0, len(rows))
	for _, row := range rows {
		out = append(out, testNumberFromRow(row))
	}
	return out, total, nil
}

func testNumberFromRow(row queries.TestNumber) TestNumber {
	out := TestNumber{Number: row.NumberE164, CreatedAt: row.CreatedAt.Time, CreatedBy: row.CreatedBy}
	if row.Note != nil {
		out.Note = *row.Note
	}
	return out
}
