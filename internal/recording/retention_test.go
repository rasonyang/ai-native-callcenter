// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

type fakeLedger struct {
	due    []Expired
	marked []uuid.UUID
	markFn func(uuid.UUID) error
}

func (f *fakeLedger) ExpiredRecordings(context.Context, int, int) ([]Expired, error) {
	return f.due, nil
}

func (f *fakeLedger) MarkRecordingDeleted(_ context.Context, id uuid.UUID) error {
	if f.markFn != nil {
		if err := f.markFn(id); err != nil {
			return err
		}
	}
	f.marked = append(f.marked, id)
	return nil
}

type fakeStorage struct {
	deleted  []string
	deleteFn func(string) error
}

func (fakeStorage) Backend() string { return "FS" }
func (fakeStorage) Bucket() string  { return "" }
func (fakeStorage) Ingest(context.Context, string) (int64, error) {
	return 0, nil
}

func (fakeStorage) Open(context.Context, string) (io.ReadSeekCloser, int64, error) {
	return nil, 0, nil
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	if f.deleteFn != nil {
		if err := f.deleteFn(key); err != nil {
			return err
		}
	}
	f.deleted = append(f.deleted, key)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func due(n int) ([]Expired, []uuid.UUID) {
	var out []Expired
	var ids []uuid.UUID
	for i := range n {
		id := uuid.New()
		ids = append(ids, id)
		out = append(out, Expired{ID: id, Key: string(rune('a'+i)) + ".wav"})
	}
	return out, ids
}

// Zero keeps everything. A deployment upgrading into this feature must not
// start deleting audio because nobody chose a number.
func TestRetentionOfZeroDeletesNothing(t *testing.T) {
	rows, _ := due(3)
	ledger := &fakeLedger{due: rows}
	storage := &fakeStorage{}
	sweeper := NewSweeper(ledger, storage, 0, quiet())

	if sweeper.IsEnabled() {
		t.Error("a retention of zero reports itself enabled")
	}
	// Run returns immediately rather than ticking; Sweep is what would delete.
	sweeper.Run(context.Background())
	if len(storage.deleted) != 0 || len(ledger.marked) != 0 {
		t.Errorf("deleted %v and marked %v with retention off",
			storage.deleted, ledger.marked)
	}
}

// The object goes first, and the row is stamped only once it has.
//
// The other order loses track of bytes: a row marked deleted whose object
// survived is an orphan nothing will look for again, paid for for ever.
func TestTheAudioIsDeletedBeforeTheRowSaysSo(t *testing.T) {
	rows, ids := due(1)
	ledger := &fakeLedger{due: rows}
	storage := &fakeStorage{deleteFn: func(string) error {
		return errors.New("the object store is unreachable")
	}}

	n, err := NewSweeper(ledger, storage, 30, quiet()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("reported %d deletions after the object delete failed", n)
	}
	if len(ledger.marked) != 0 {
		t.Errorf("recording %s was marked deleted while its audio is still there — "+
			"nothing will ever look for those bytes again", ids[0])
	}
}

// An object that is already gone is not a failure: it is the state the sweep
// was trying to reach, and the row still needs its mark. This is what repairs
// a sweep that died between the delete and the stamp.
func TestAnAlreadyMissingObjectStillMarksTheRow(t *testing.T) {
	rows, ids := due(1)
	ledger := &fakeLedger{due: rows}
	storage := &fakeStorage{deleteFn: func(string) error { return fs.ErrNotExist }}

	n, err := NewSweeper(ledger, storage, 30, quiet()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || len(ledger.marked) != 1 || ledger.marked[0] != ids[0] {
		t.Errorf("deleted=%d marked=%v — a recording whose audio was already gone "+
			"stays due for ever, and every sweep retries it", n, ledger.marked)
	}
}

// One unreachable object must not stop the rest. A sweep that gives up on the
// first failure never gets past it, and the queue behind it grows for ever.
func TestOneFailureDoesNotStopTheSweep(t *testing.T) {
	rows, _ := due(3)
	ledger := &fakeLedger{due: rows}
	storage := &fakeStorage{deleteFn: func(key string) error {
		if key == "b.wav" {
			return errors.New("that one is unreachable")
		}
		return nil
	}}

	n, err := NewSweeper(ledger, storage, 30, quiet()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d of 3, want the two that could be deleted", n)
	}
	if len(ledger.marked) != 2 {
		t.Errorf("marked %d rows, want 2 — the failed one must stay due", len(ledger.marked))
	}
}
