// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The number filter matches either end of a call, because a call the platform
// placed is stored from the DID to the customer while an inbound one is stored
// from the caller to the DID: no single column holds "the other party". The
// older FromNumber filter keeps meaning the caller's number only.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheNumberFilterMatchesEitherEndOfACall(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	started := time.Date(2026, time.March, 10, 9, 0, 0, 0, time.UTC)

	insert := func(minute int, callType, from, to, did string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		at := started.Add(time.Duration(minute) * time.Minute)
		if err := ledger.InsertCDR(ctx, CDR{
			CallID: id, StartedAt: at, EndedAt: at.Add(time.Minute),
			CallType: callType, Status: CDRStatusAnswered,
			FromNumber: from, ToNumber: to, DID: did,
		}); err != nil {
			t.Fatalf("InsertCDR(%s %s): %v", callType, from, err)
		}
		return id
	}
	outbound := insert(0, "OUTBOUND", "100200", "13800000000", "100200")
	inbound := insert(1, "INBOUND", "+861083550341", "95001", "95001")
	internal := insert(2, "INTERNAL", "3001", "3002", "")

	list := func(f CDRFilter) []uuid.UUID {
		t.Helper()
		rows, total, err := ledger.ListCDRs(ctx, f)
		if err != nil {
			t.Fatalf("ListCDRs(%+v): %v", f, err)
		}
		if total != int64(len(rows)) {
			t.Errorf("filter %+v: count %d disagrees with the %d rows listed", f, total, len(rows))
		}
		var ids []uuid.UUID
		for _, r := range rows {
			ids = append(ids, r.CallID)
		}
		return ids
	}

	for _, tc := range []struct {
		name   string
		filter CDRFilter
		want   []uuid.UUID
	}{
		{"the customer's number finds the call the platform placed", CDRFilter{Number: "13800000000"}, []uuid.UUID{outbound}},
		{"a substring of the customer's number does too", CDRFilter{Number: "38000"}, []uuid.UUID{outbound}},
		{"the line the call went out on finds it", CDRFilter{Number: "100200"}, []uuid.UUID{outbound}},
		{"an inbound call is found by its caller's number", CDRFilter{Number: "10835503"}, []uuid.UUID{inbound}},
		{"and by the DID it was dialled on", CDRFilter{Number: "95001"}, []uuid.UUID{inbound}},
		{"an internal call is found by the called extension", CDRFilter{Number: "3002"}, []uuid.UUID{internal}},
		{"a number nobody used finds nothing", CDRFilter{Number: "99999"}, nil},
		{"no number is no constraint", CDRFilter{}, []uuid.UUID{internal, inbound, outbound}},
		{"the number composes with the other filters", CDRFilter{Number: "100", DID: "95001"}, nil},
		// FromNumber is unchanged: the caller's side only.
		{"FromNumber does not find the customer's number", CDRFilter{FromNumber: "13800000000"}, nil},
		{"FromNumber still finds a caller", CDRFilter{FromNumber: "10835503"}, []uuid.UUID{inbound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := list(tc.filter)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d calls %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("call %d = %s, want %s", i, got[i], tc.want[i])
				}
			}
		})
	}
}
