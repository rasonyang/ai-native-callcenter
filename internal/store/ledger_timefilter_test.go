// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The optional time window on the CDR and audit-log listings: a missing bound
// is no constraint, `from` is inclusive and `to` is exclusive. The bounds are
// written as coalesce(arg, ±infinity) so they stay index conditions under a
// generic plan; this pins that the rewrite kept the meaning, with rows exactly
// ON each bound so an off-by-one on either side shows.
//
// Needs a real server (AICC_TEST_DATABASE_URL); each test gets its own
// scratch database, so the only rows are the ones seeded here.
func TestTheTimeWindowOfTheListingsIsFromInclusiveToExclusive(t *testing.T) {
	ledger, _, pool := workspaceStore(t)
	ctx := context.Background()

	// Whole-second offsets from a fixed origin keep the bounds exact.
	origin := time.Date(2026, time.March, 10, 9, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return origin.Add(time.Duration(minutes) * time.Minute) }

	// Offsets in minutes: the window below is [10, 30). 10 and 30 sit on the
	// bounds; 9/11 and 29/31 are one minute either side.
	offsets := []int{-600, 0, 9, 10, 11, 20, 29, 30, 31, 90, 600}
	wantNames := func(lo, hi int, loSet, hiSet bool) []string {
		var out []string
		for _, m := range offsets {
			if (loSet && m < lo) || (hiSet && m >= hi) {
				continue
			}
			out = append(out, fmt.Sprintf("m%+d", m))
		}
		slices.Reverse(out) // newest first
		return out
	}

	byCall := map[uuid.UUID]string{}
	for _, m := range offsets {
		id := uuid.Must(uuid.NewV7())
		byCall[id] = fmt.Sprintf("m%+d", m)
		if err := ledger.InsertCDR(ctx, CDR{
			CallID: id, StartedAt: at(m), EndedAt: at(m).Add(3 * time.Minute),
			CallType: "INBOUND", Status: CDRStatusAnswered,
			FromNumber: "+1415555010" + fmt.Sprint(len(byCall)%10), DID: "95001",
		}); err != nil {
			t.Fatalf("InsertCDR(%+d min): %v", m, err)
		}
	}
	// A row the status filter would drop, to be sure the window composes with
	// the other optional filters rather than replacing them.
	missed := uuid.Must(uuid.NewV7())
	byCall[missed] = "missed@20"
	if err := ledger.InsertCDR(ctx, CDR{
		CallID: missed, StartedAt: at(20), EndedAt: at(21),
		CallType: "INBOUND", Status: CDRStatusNoAnswer, DID: "95001",
	}); err != nil {
		t.Fatalf("InsertCDR(missed): %v", err)
	}

	actor := uuid.Must(uuid.NewV7())
	other := uuid.Must(uuid.NewV7())
	byAudit := map[int64]string{}
	for _, m := range offsets {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO audit_logs (occurred_at, actor_id, action, target_kind, target_id)
			VALUES ($1, $2, 'queue.update', 'QUEUE', 'q1') RETURNING id`, at(m), actor).Scan(&id); err != nil {
			t.Fatalf("seed audit row %+d: %v", m, err)
		}
		byAudit[id] = fmt.Sprintf("m%+d", m)
	}
	// Another actor and another action inside the window.
	for _, row := range []struct {
		actor  uuid.UUID
		action string
	}{{other, "queue.update"}, {actor, "agent.update"}} {
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO audit_logs (occurred_at, actor_id, action) VALUES ($1, $2, $3) RETURNING id`,
			at(20), row.actor, row.action).Scan(&id); err != nil {
			t.Fatalf("seed extra audit row: %v", err)
		}
		byAudit[id] = "extra@20"
	}

	from, to := at(10), at(30)
	windows := []struct {
		name         string
		from, to     time.Time
		lo, hi       int
		loSet, hiSet bool
	}{
		{"no bounds", time.Time{}, time.Time{}, 0, 0, false, false},
		{"lower bound only", from, time.Time{}, 10, 0, true, false},
		{"upper bound only", time.Time{}, to, 0, 30, false, true},
		{"bounded window", from, to, 10, 30, true, true},
		{"empty window", at(12), at(12), 12, 12, true, true},
	}

	for _, w := range windows {
		t.Run("cdrs/"+w.name, func(t *testing.T) {
			want := wantNames(w.lo, w.hi, w.loSet, w.hiSet)
			// The extra NO_ANSWER row at minute 20 is in any window that holds 20.
			inWindow := func(m int) bool { return !(w.loSet && m < w.lo) && !(w.hiSet && m >= w.hi) }

			rows, total, err := ledger.ListCDRs(ctx, CDRFilter{From: w.from, To: w.to, DID: "95001", Limit: 200})
			if err != nil {
				t.Fatalf("ListCDRs: %v", err)
			}
			wantTotal := len(want)
			if inWindow(20) {
				wantTotal++
			}
			if int(total) != wantTotal || len(rows) != wantTotal {
				t.Fatalf("total=%d rows=%d, want %d", total, len(rows), wantTotal)
			}
			var got []string
			for _, r := range rows {
				if name := byCall[r.CallID]; name != "missed@20" {
					got = append(got, name)
				}
			}
			if !slices.Equal(got, want) {
				t.Errorf("rows (newest first) = %v\nwant %v", got, want)
			}

			// The window composes with the other filters.
			_, total, err = ledger.ListCDRs(ctx, CDRFilter{From: w.from, To: w.to, Status: CDRStatusNoAnswer})
			if err != nil {
				t.Fatalf("ListCDRs(status): %v", err)
			}
			if wantMissed := map[bool]int64{true: 1, false: 0}[inWindow(20)]; total != wantMissed {
				t.Errorf("NO_ANSWER total = %d, want %d", total, wantMissed)
			}
		})

		t.Run("audit/"+w.name, func(t *testing.T) {
			want := wantNames(w.lo, w.hi, w.loSet, w.hiSet)
			rows, total, err := ledger.ListAuditLogs(ctx, AuditFilter{
				ActorID: &actor, ActionPrefix: "queue.", From: w.from, To: w.to, Limit: 200,
			})
			if err != nil {
				t.Fatalf("ListAuditLogs: %v", err)
			}
			if int(total) != len(want) || len(rows) != len(want) {
				t.Fatalf("total=%d rows=%d, want %d", total, len(rows), len(want))
			}
			var got []string
			for _, r := range rows {
				got = append(got, byAudit[r.ID])
			}
			if !slices.Equal(got, want) {
				t.Errorf("rows (newest first) = %v\nwant %v", got, want)
			}

			// Without the actor and action filters the window alone decides.
			_, total, err = ledger.ListAuditLogs(ctx, AuditFilter{From: w.from, To: w.to, Limit: 200})
			if err != nil {
				t.Fatalf("ListAuditLogs(window only): %v", err)
			}
			wantAll := len(want)
			if !(w.loSet && 20 < w.lo) && !(w.hiSet && 20 >= w.hi) {
				wantAll += 2
			}
			if int(total) != wantAll {
				t.Errorf("window-only total = %d, want %d", total, wantAll)
			}
		})
	}
}

// BUSY is a status of its own on the listing: the filter separates it from
// NO_ANSWER, and the reports, which count only ANSWERED, keep every row.
func TestABusyCallListsApartFromANoAnswerCall(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	start := time.Date(2026, time.March, 10, 9, 0, 0, 0, time.UTC)
	for i, st := range []string{CDRStatusBusy, CDRStatusBusy, CDRStatusNoAnswer, CDRStatusAnswered} {
		cause := map[string]string{CDRStatusBusy: "USER_BUSY", CDRStatusNoAnswer: "NO_ANSWER", CDRStatusAnswered: "NORMAL_CLEARING"}[st]
		if err := ledger.InsertCDR(ctx, CDR{
			CallID: uuid.Must(uuid.NewV7()), StartedAt: start.Add(time.Duration(i) * time.Minute),
			EndedAt:  start.Add(time.Duration(i)*time.Minute + 20*time.Second),
			CallType: "OUTBOUND", Status: st, HangupCause: cause, FromNumber: "95099", ToNumber: "1868888666" + fmt.Sprint(i),
		}); err != nil {
			t.Fatalf("InsertCDR(%s): %v", st, err)
		}
	}
	for status, want := range map[string]int{CDRStatusBusy: 2, CDRStatusNoAnswer: 1, CDRStatusAnswered: 1, "": 4} {
		rows, total, err := ledger.ListCDRs(ctx, CDRFilter{Status: status, Limit: 50})
		if err != nil {
			t.Fatalf("ListCDRs(%q): %v", status, err)
		}
		if int(total) != want || len(rows) != want {
			t.Errorf("status %q: total %d, rows %d, want %d", status, total, len(rows), want)
		}
		for _, r := range rows {
			if status != "" && r.Status != status {
				t.Errorf("status %q listing returned a %s row", status, r.Status)
			}
		}
	}
}
