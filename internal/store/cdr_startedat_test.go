// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Both paths can write one call's row (a bot transfer the caller hung up on, a
// restart that orphaned the bot). The row that ended later wins as a whole, so
// the call's start follows the row that reaches furthest: a bot row that began
// at the call's creation (#181) is replaced by the human path's row with the
// same start, and a bot row that ended earlier, started when its own session
// did, never pulls the start of the stored row later.
func TestTheLaterEndingRowKeepsTheCallsEarlierStart(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()

	created := time.Now().Add(-10 * time.Minute).Truncate(time.Millisecond)
	callID := uuid.Must(uuid.NewV7())
	row := func(startedAt, answeredAt time.Time, endedAfter time.Duration, ringSec, totalSec int) CDR {
		return CDR{
			CallID: callID, StartedAt: startedAt, AnsweredAt: answeredAt,
			EndedAt:  created.Add(endedAfter),
			CallType: "INBOUND", Status: CDRStatusAnswered,
			RingSec: ringSec, TotalSec: totalSec,
		}
	}
	read := func() CDR {
		t.Helper()
		got, err := ledger.GetCDR(ctx, callID)
		if err != nil {
			t.Fatalf("GetCDR() error = %v", err)
		}
		return got
	}

	// The human path wrote the whole call: started at creation, 90 s long.
	if err := ledger.InsertCDR(ctx, row(created, created.Add(5*time.Second), 90*time.Second, 5, 90)); err != nil {
		t.Fatal(err)
	}
	// A bot row for the same call ending earlier, begun at the bot's own
	// session start, must not replace it.
	if err := ledger.InsertCDR(ctx, row(created.Add(5*time.Second), created.Add(5*time.Second), 60*time.Second, 0, 55)); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.StartedAt.Equal(created) || got.RingSec != 5 || got.TotalSec != 90 {
		t.Errorf("an earlier-ending row replaced the stored one: startedAt %v ringSec %d totalSec %d",
			got.StartedAt, got.RingSec, got.TotalSec)
	}

	// A bot provisional row and the human path's row, both from creation: the
	// later ending replaces and the start stays the creation time.
	callID = uuid.Must(uuid.NewV7())
	if err := ledger.InsertCDR(ctx, row(created, created.Add(3*time.Second), 60*time.Second, 3, 60)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.InsertCDR(ctx, row(created, created.Add(3*time.Second), 120*time.Second, 3, 120)); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.StartedAt.Equal(created) || got.TotalSec != 120 {
		t.Errorf("startedAt %v totalSec %d, want %v and 120", got.StartedAt, got.TotalSec, created)
	}
}
