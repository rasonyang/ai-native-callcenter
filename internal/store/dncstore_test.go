// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

func openDNC(t *testing.T) (*Store, *DNCStore) {
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
	return st, st.DNC()
}

// The list's behaviour: idempotent add that leaves the first entry alone,
// exact membership, removal, and newest-first paging by prefix.
func TestTheDNCListAddsOnceAndForgetsOnRequest(t *testing.T) {
	ctx := context.Background()
	_, list := openDNC(t)
	author := uuid.New()
	call := uuid.New()

	first, isNew, err := list.Add(ctx, "+8613800138000", DNCSourceManual, nil, &author)
	if err != nil || !isNew {
		t.Fatalf("add: %v isNew=%v", err, isNew)
	}
	again, isNew, err := list.Add(ctx, "+8613800138000", DNCSourceImport, nil, nil)
	if err != nil || isNew {
		t.Fatalf("add again: %v isNew=%v, want a quiet repeat", err, isNew)
	}
	if again.Source != DNCSourceManual || again.CreatedBy == nil || *again.CreatedBy != author || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("the repeat rewrote the entry: %+v vs %+v", again, first)
	}

	conv, _, err := list.Add(ctx, "+442071838750", DNCSourceConversation, &call, nil)
	if err != nil {
		t.Fatal(err)
	}
	if conv.CallID == nil || *conv.CallID != call || conv.CreatedBy != nil {
		t.Errorf("conversation entry = %+v, want the call and no author", conv)
	}

	if _, _, err := list.Add(ctx, "13800138000", DNCSourceManual, nil, nil); !errors.Is(err, ErrDNCInvalid) {
		t.Errorf("a non-canonical number: %v, want ErrDNCInvalid", err)
	}

	for number, want := range map[string]bool{
		"+8613800138000": true, "+8613800138001": false, "13800138000": false, "": false,
	} {
		got, err := list.Contains(ctx, number)
		if err != nil || got != want {
			t.Errorf("Contains(%q) = %v, %v; want %v", number, got, err, want)
		}
	}

	items, total, err := list.List(ctx, DNCFilter{})
	if err != nil || total != 2 || len(items) != 2 || items[0].Number != "+442071838750" {
		t.Errorf("list = %+v total=%d err=%v, want both, newest first", items, total, err)
	}
	items, total, _ = list.List(ctx, DNCFilter{Prefix: "+86"})
	if total != 1 || len(items) != 1 || items[0].Number != "+8613800138000" {
		t.Errorf("prefix +86 = %+v total=%d", items, total)
	}

	if err := list.Remove(ctx, "+8613800138000"); err != nil {
		t.Fatal(err)
	}
	if err := list.Remove(ctx, "+8613800138000"); !errors.Is(err, ErrDNCNotFound) {
		t.Errorf("removing twice: %v, want ErrDNCNotFound", err)
	}
	if got, _ := list.Contains(ctx, "+8613800138000"); got {
		t.Error("a removed number is still listed")
	}
}

// The database refuses what the code would not write, so a second writer
// (a migration, a script, the bot) cannot put a malformed row in either.
func TestTheDNCTableRefusesWhatIsNotCanonical(t *testing.T) {
	ctx := context.Background()
	st, _ := openDNC(t)

	for name, stmt := range map[string]string{
		"national form":       `INSERT INTO dnc_numbers (number_e164, source) VALUES ('13800138000', 'MANUAL')`,
		"no plus":             `INSERT INTO dnc_numbers (number_e164, source) VALUES ('8613800138000', 'MANUAL')`,
		"leading zero":        `INSERT INTO dnc_numbers (number_e164, source) VALUES ('+0613800138000', 'MANUAL')`,
		"too short":           `INSERT INTO dnc_numbers (number_e164, source) VALUES ('+1234567', 'MANUAL')`,
		"letters":             `INSERT INTO dnc_numbers (number_e164, source) VALUES ('+86138001380ab', 'MANUAL')`,
		"unknown source":      `INSERT INTO dnc_numbers (number_e164, source) VALUES ('+8613800138000', 'WEB')`,
		"call id on a MANUAL": `INSERT INTO dnc_numbers (number_e164, source, call_id) VALUES ('+8613800138000', 'MANUAL', gen_random_uuid())`,
	} {
		if _, err := st.Pool.Exec(ctx, stmt); err == nil {
			t.Errorf("%s: the table accepted it", name)
		}
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO dnc_numbers (number_e164, source) VALUES ('+12025550123', 'IMPORT')`); err != nil {
		t.Errorf("a canonical row was refused: %v", err)
	}
}

// The dialer skips the contacts waiting on a number the moment it is listed;
// a number that was already there is not news.
func TestAddingANumberToTheDNCListTellsTheDialerOnce(t *testing.T) {
	ctx := context.Background()
	st, _ := openDNC(t)
	var told int
	st.OnDNCAdded = func() { told++ }
	list := st.DNC()
	for range 3 {
		if _, _, err := list.Add(ctx, "+8613800138000", DNCSourceManual, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if told != 1 {
		t.Errorf("told %d times, want once", told)
	}
	if _, _, err := list.Add(ctx, "+8613800138001", DNCSourceManual, nil, nil); err != nil || told != 2 {
		t.Errorf("a new number: told %d, %v", told, err)
	}
}

// aicc doctor asks how many entries lie outside the built-in country code.
// Country codes are prefix-free, so "+1" matches exactly the numbers of +1 and
// nothing of +1x-style codes elsewhere (+1 is not a prefix of +86, +44 or +91).
func TestTheDNCListCountsEntriesOutsideACountryCode(t *testing.T) {
	ctx := context.Background()
	st, list := openDNC(t)
	if n, err := st.DNCCountOutsideCountryCode(ctx, "1"); err != nil || n != 0 {
		t.Fatalf("empty list: %d, %v; want 0", n, err)
	}
	for _, n := range []string{"+12125550101", "+14165550101", "+8613800138000", "+442071838750", "+919876543210"} {
		if _, _, err := list.Add(ctx, n, DNCSourceManual, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.DNCCountOutsideCountryCode(ctx, "1"); err != nil || n != 3 {
		t.Errorf("outside +1: %d, %v; want 3", n, err)
	}
	if n, err := st.DNCCountOutsideCountryCode(ctx, "86"); err != nil || n != 4 {
		t.Errorf("outside +86: %d, %v; want 4", n, err)
	}
}
