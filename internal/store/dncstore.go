// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// DNCStore is the do-not-call list: numbers this platform never places a call
// to. Every number it holds is canonical E.164.
type DNCStore struct{ q *queries.Queries }

// DNC returns the do-not-call list.
func (s *Store) DNC() *DNCStore { return &DNCStore{q: s.Queries} }

// DNC entry sources, byte-identical in JSON and the database.
const (
	DNCSourceManual       = "MANUAL"
	DNCSourceImport       = "IMPORT"
	DNCSourceConversation = "CONVERSATION"
)

// DNCNumber is one entry on the list.
type DNCNumber struct {
	Number    string     `json:"number"`
	Source    string     `json:"source"`
	CallID    *uuid.UUID `json:"callId,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	CreatedBy *uuid.UUID `json:"createdBy,omitempty"`
}

// DNCFilter narrows a listing. Prefix is a canonical-number prefix ("+8613");
// a complete number is its own prefix, so it finds exactly that entry.
type DNCFilter struct {
	Prefix string
	Limit  int
	Offset int
}

// ErrDNCNotFound means the number is not on the list.
var ErrDNCNotFound = errors.New("that number is not on the do-not-call list")

// ErrDNCInvalid means the number is not canonical E.164.
var ErrDNCInvalid = errors.New("not a canonical E.164 number")

// Add puts a canonical number on the list. isNew is false when it was already
// there; the stored entry (original source, time and author) is returned
// untouched, so adding is idempotent.
func (d *DNCStore) Add(ctx context.Context, number, source string, callID, by *uuid.UUID) (entry DNCNumber, isNew bool, err error) {
	if !phonenum.IsCanonical(number) {
		return DNCNumber{}, false, ErrDNCInvalid
	}
	row, err := d.q.InsertDNCNumber(ctx, queries.InsertDNCNumberParams{
		NumberE164: number, Source: source, CallID: callID, CreatedBy: by,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := d.q.GetDNCNumber(ctx, number)
		if err != nil {
			return DNCNumber{}, false, err
		}
		return dncFromRow(existing), false, nil
	}
	if err != nil {
		return DNCNumber{}, false, err
	}
	return dncFromRow(row), true, nil
}

// Remove takes a canonical number off the list.
func (d *DNCStore) Remove(ctx context.Context, number string) error {
	n, err := d.q.DeleteDNCNumber(ctx, number)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDNCNotFound
	}
	return nil
}

// Contains reports whether a canonical number is on the list. A number that is
// not canonical is never on it.
func (d *DNCStore) Contains(ctx context.Context, number string) (bool, error) {
	if !phonenum.IsCanonical(number) {
		return false, nil
	}
	return d.q.IsDNCNumber(ctx, number)
}

// List pages the list, newest first, with the total for the filter.
func (d *DNCStore) List(ctx context.Context, f DNCFilter) ([]DNCNumber, int64, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	prefix := strings.TrimSpace(f.Prefix)
	rows, err := d.q.ListDNCNumbers(ctx, queries.ListDNCNumbersParams{
		Prefix: prefix, PageLimit: int32(f.Limit), PageOffset: int32(f.Offset),
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := d.q.CountDNCNumbers(ctx, prefix)
	if err != nil {
		return nil, 0, err
	}
	out := make([]DNCNumber, 0, len(rows))
	for _, row := range rows {
		out = append(out, dncFromRow(row))
	}
	return out, total, nil
}

func dncFromRow(row queries.DncNumber) DNCNumber {
	return DNCNumber{
		Number: row.NumberE164, Source: row.Source, CallID: row.CallID,
		CreatedAt: row.CreatedAt.Time, CreatedBy: row.CreatedBy,
	}
}
