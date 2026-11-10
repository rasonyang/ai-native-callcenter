// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/google/uuid"
)

// The spool sweep asks which calls already have a row; a row retention has
// marked deleted still counts, because the call was booked.
func TestBookedRecordingCallIDsSeesEveryRowIncludingDeletedOnes(t *testing.T) {
	st, ctx := liveStore(t)
	l := st.Ledger()
	live, deleted, unbooked := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if _, err := l.InsertRecording(ctx, Recording{CallID: live, Backend: RecordingBackendFS, ObjectKey: "a.wav"}); err != nil {
		t.Fatal(err)
	}
	gone, err := l.InsertRecording(ctx, Recording{CallID: deleted, Backend: RecordingBackendFS, ObjectKey: "b.wav"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.MarkRecordingDeleted(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	// A second row for one call must not list the call twice.
	if _, err := l.InsertRecording(ctx, Recording{CallID: live, Backend: RecordingBackendFS, ObjectKey: "c.wav"}); err != nil {
		t.Fatal(err)
	}

	got, err := l.BookedRecordingCallIDs(ctx, []uuid.UUID{live, deleted, unbooked})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]int{}
	for _, id := range got {
		seen[id]++
	}
	if len(got) != 2 || seen[live] != 1 || seen[deleted] != 1 {
		t.Errorf("booked = %v, want the live and the deleted call once each", got)
	}
	if none, err := l.BookedRecordingCallIDs(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("no ids returned %v, %v", none, err)
	}
}
