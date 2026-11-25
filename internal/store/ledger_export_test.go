// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// exportCall is one row of the eight calls the call-record export is checked
// against, started in Asia/Shanghai: each filter and each column has a call on
// either side of it.
type exportCall struct {
	no       int
	start    string // local time in Asia/Shanghai
	from, to string
	callType string
	status   string
	billSec  int
}

var exportCalls = []exportCall{
	{no: 1, start: "2026-11-30 23:59:50", from: "02155550101", to: "13800000001", callType: "OUTBOUND", status: CDRStatusAnswered, billSec: 12},
	{no: 2, start: "2026-12-01 00:00:05", from: "02155550101", to: "13800000002", callType: "OUTBOUND", status: CDRStatusAnswered, billSec: 60},
	{no: 3, start: "2026-12-01 09:30:00", from: "02155550102", to: "13800000001", callType: "OUTBOUND", status: CDRStatusNoAnswer},
	{no: 4, start: "2026-12-01 09:31:00", from: "02155550102", to: "13800000003", callType: "OUTBOUND", status: CDRStatusBusy},
	{no: 5, start: "2026-12-01 10:00:00", from: "02155550101", to: "13800000003", callType: "OUTBOUND", status: CDRStatusFailed},
	// Two extensions: answered, and nobody bills it.
	{no: 6, start: "2026-12-01 10:05:00", from: "1001", to: "1002", callType: "INTERNAL", status: CDRStatusAnswered},
	{no: 7, start: "2026-12-31 23:59:59", from: "02155550101", to: "13800000001", callType: "OUTBOUND", status: CDRStatusAnswered, billSec: 3725},
	{no: 8, start: "2027-01-01 00:00:00", from: "02155550101", to: "13800000001", callType: "OUTBOUND", status: CDRStatusAnswered, billSec: 61},
}

// seedExportCalls writes the eight calls and returns their ids by number.
func seedExportCalls(t *testing.T, ledger *LedgerStore) (map[int]uuid.UUID, *time.Location) {
	t.Helper()
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	ids := map[int]uuid.UUID{}
	for _, c := range exportCalls {
		started, err := time.ParseInLocation("2006-01-02 15:04:05", c.start, shanghai)
		if err != nil {
			t.Fatalf("call %d: %v", c.no, err)
		}
		id := uuid.Must(uuid.NewV7())
		cdr := CDR{
			CallID: id, StartedAt: started, EndedAt: started.Add(time.Duration(c.billSec+5) * time.Second),
			CallType: c.callType, Status: c.status, FromNumber: c.from, ToNumber: c.to,
			BillSec: c.billSec, TotalSec: c.billSec + 5,
		}
		if c.billSec > 0 {
			cdr.AnsweredAt = started.Add(5 * time.Second)
		}
		if err := ledger.InsertCDR(context.Background(), cdr); err != nil {
			t.Fatalf("InsertCDR(call %d): %v", c.no, err)
		}
		ids[c.no] = id
	}
	return ids, shanghai
}

// The filters the call records screen and its export add: the called number,
// the billed duration, and a window of local days, composed with each other
// and with the status. The totals are over everything the filter selects, not
// over the page.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheCallRecordFiltersSelectTheCallsTheIssueNames(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()
	ids, shanghai := seedExportCalls(t, ledger)

	december := func(f CDRFilter) CDRFilter {
		f.From = time.Date(2026, time.December, 1, 0, 0, 0, 0, shanghai)
		f.To = time.Date(2027, time.January, 1, 0, 0, 0, 0, shanghai)
		return f
	}
	billSec := func(n int) *int { return &n }

	for _, tc := range []struct {
		name      string
		filter    CDRFilter
		want      []int // call numbers, newest first
		wantTotal int64 // billed seconds over the filter
	}{
		{"December in Shanghai holds 2 to 7, not the second before or after it",
			december(CDRFilter{}), []int{7, 6, 5, 4, 3, 2}, 3785},
		{"the called number finds the calls placed to it, not the ones it placed",
			CDRFilter{ToNumber: "13800000001"}, []int{8, 7, 3, 1}, 3725 + 61 + 12},
		{"billed 60 to 61 seconds, both ends inclusive",
			CDRFilter{BillSecMin: billSec(60), BillSecMax: billSec(61)}, []int{8, 2}, 121},
		{"billed at least a second leaves out the calls nobody billed",
			CDRFilter{BillSecMin: billSec(1)}, []int{8, 7, 2, 1}, 3725 + 61 + 60 + 12},
		{"a maximum alone", CDRFilter{BillSecMax: billSec(0)}, []int{6, 5, 4, 3}, 0},
		{"the window composes with the status",
			december(CDRFilter{Status: CDRStatusAnswered}), []int{7, 6, 2}, 3785},
		{"the called number composes with the status",
			CDRFilter{ToNumber: "13800000003", Status: CDRStatusBusy}, []int{4}, 0},
		{"every filter at once",
			december(CDRFilter{ToNumber: "1380000000", FromNumber: "02155550101",
				Status: CDRStatusAnswered, BillSecMin: billSec(1)}), []int{7, 2}, 3785},
		{"a substring of the called number", CDRFilter{ToNumber: "100"}, []int{6}, 0},
		{"nothing matches", CDRFilter{ToNumber: "99999"}, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := ledger.ListCDRs(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListCDRs: %v", err)
			}
			if got := callNumbers(ids, page.Items); !slices.Equal(got, tc.want) {
				t.Errorf("calls = %v, want %v", got, tc.want)
			}
			if page.Total != int64(len(tc.want)) {
				t.Errorf("total = %d, want %d", page.Total, len(tc.want))
			}
			if page.BillSecTotal != tc.wantTotal {
				t.Errorf("billSecTotal = %d, want %d", page.BillSecTotal, tc.wantTotal)
			}

			// A page of one still carries the totals of the whole filter.
			small := tc.filter
			small.Limit = 1
			first, err := ledger.ListCDRs(ctx, small)
			if err != nil {
				t.Fatalf("ListCDRs(limit 1): %v", err)
			}
			if first.Total != page.Total || first.BillSecTotal != page.BillSecTotal {
				t.Errorf("a page of one has totals %d/%d, the filter %d/%d",
					first.Total, first.BillSecTotal, page.Total, page.BillSecTotal)
			}

			// The export reads exactly what the listing pages through.
			if got := callNumbers(ids, exportAll(t, ledger, tc.filter, 2)); !slices.Equal(got, tc.want) {
				t.Errorf("export = %v, want %v", got, tc.want)
			}
		})
	}
}

// Calls that started in the same microsecond are ordered by their id, so a
// page boundary or an export batch boundary that falls between them neither
// repeats nor skips one, and the export and the pages agree row for row.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheExportReadsTheListingsOrderAcrossBatchesAndTies(t *testing.T) {
	ledger, _, _ := workspaceStore(t)
	ctx := context.Background()

	origin := time.Date(2026, time.December, 1, 2, 0, 0, 0, time.UTC)
	var written []uuid.UUID
	// Three instants, the middle one shared by five calls: with batches of
	// two, a batch boundary falls inside the tie twice.
	for i, at := range []time.Time{
		origin, origin.Add(time.Second), origin.Add(time.Second), origin.Add(time.Second),
		origin.Add(time.Second), origin.Add(time.Second), origin.Add(2 * time.Second),
	} {
		id := uuid.Must(uuid.NewV7())
		if err := ledger.InsertCDR(ctx, CDR{
			CallID: id, StartedAt: at, EndedAt: at.Add(time.Minute), CallType: "INBOUND",
			Status: CDRStatusAnswered, FromNumber: "02155550101", ToNumber: "95001", BillSec: i,
		}); err != nil {
			t.Fatalf("InsertCDR: %v", err)
		}
		written = append(written, id)
	}

	var paged []uuid.UUID
	for offset := 0; ; offset += 2 {
		page, err := ledger.ListCDRs(ctx, CDRFilter{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("ListCDRs(offset %d): %v", offset, err)
		}
		for _, c := range page.Items {
			paged = append(paged, c.CallID)
		}
		if len(page.Items) < 2 {
			break
		}
	}
	// Newest first, and the id breaks the tie, newest first too. The ids are
	// UUIDv7, minted in the order the calls were written, so that is the
	// order they were written in, reversed.
	want := slices.Clone(written)
	slices.Reverse(want)
	if !slices.Equal(paged, want) {
		t.Fatalf("paging lists %v\nwant %v", paged, want)
	}

	for _, batch := range []int32{1, 2, 3, 7, 1000} {
		var exported []uuid.UUID
		for _, c := range exportAll(t, ledger, CDRFilter{}, batch) {
			exported = append(exported, c.CallID)
		}
		if !slices.Equal(exported, paged) {
			t.Errorf("batches of %d export %v\npaging lists %v", batch, exported, paged)
		}
	}
}

// exportAll drains an export in batches of the given size.
func exportAll(t *testing.T, ledger *LedgerStore, filter CDRFilter, batch int32) []CDR {
	t.Helper()
	export := ledger.ExportCDRs(filter)
	export.params.PageLimit = batch
	var out []CDR
	for reads := 0; ; reads++ {
		if reads > 1000 {
			t.Fatal("the export never ended")
		}
		rows, err := export.Next(context.Background())
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if len(rows) == 0 {
			// Read again once done: still nothing.
			if again, err := export.Next(context.Background()); err != nil || len(again) != 0 {
				t.Fatalf("a finished export read %d more rows, err %v", len(again), err)
			}
			return out
		}
		out = append(out, rows...)
	}
}

func callNumbers(ids map[int]uuid.UUID, cdrs []CDR) []int {
	var out []int
	for _, c := range cdrs {
		for no, id := range ids {
			if id == c.CallID {
				out = append(out, no)
			}
		}
	}
	return out
}

// The count and the listing are two statements, so nothing but this keeps
// them selecting the same calls: the listing's WHERE is the count's, word for
// word, plus the export's cursor and nothing else. A filter added to one and
// not the other would make the totals line disagree with the rows under it.
func TestTheCDRCountFiltersAsTheListDoes(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("sql/ledger.sql")
	if err != nil {
		t.Fatal(err)
	}
	where := func(name, end string) string {
		t.Helper()
		re := regexp.MustCompile(`(?s)-- name: ` + name + ` .*?\nWHERE (.*?)` + end)
		m := re.FindSubmatch(raw)
		if m == nil {
			t.Fatalf("no WHERE clause found for %s", name)
		}
		return strings.TrimSpace(string(m[1]))
	}
	list := where("ListCDRs", `\nORDER BY`)
	count := where("CountCDRs", `;`)
	cursor, ok := strings.CutPrefix(list, count)
	if !ok {
		t.Fatalf("ListCDRs and CountCDRs filter differently.\nListCDRs:\n%s\n\nCountCDRs:\n%s", list, count)
	}
	if !strings.Contains(cursor, "before_call_id") || strings.Count(cursor, "\n  AND ") != 1 {
		t.Errorf("ListCDRs adds more than the export's cursor to the count's filter:\n%s", cursor)
	}
}
