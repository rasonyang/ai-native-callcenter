// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// cdrExportCall is one of the eight calls the call records screen and its
// export are checked against, started in Asia/Shanghai.
type cdrExportCall struct {
	no       int
	start    string
	from, to string
	callType string
	status   string
	billSec  int
}

var cdrExportCalls = []cdrExportCall{
	{1, "2026-11-30 23:59:50", "02155550101", "13800000001", "OUTBOUND", store.CDRStatusAnswered, 12},
	{2, "2026-12-01 00:00:05", "02155550101", "13800000002", "OUTBOUND", store.CDRStatusAnswered, 60},
	{3, "2026-12-01 09:30:00", "02155550102", "13800000001", "OUTBOUND", store.CDRStatusNoAnswer, 0},
	{4, "2026-12-01 09:31:00", "02155550102", "13800000003", "OUTBOUND", store.CDRStatusBusy, 0},
	{5, "2026-12-01 10:00:00", "02155550101", "13800000003", "OUTBOUND", store.CDRStatusFailed, 0},
	{6, "2026-12-01 10:05:00", "1001", "1002", "INTERNAL", store.CDRStatusAnswered, 0},
	{7, "2026-12-31 23:59:59", "02155550101", "13800000001", "OUTBOUND", store.CDRStatusAnswered, 3725},
	{8, "2027-01-01 00:00:00", "02155550101", "13800000001", "OUTBOUND", store.CDRStatusAnswered, 61},
}

// exportFixture is a server whose deployment is in Asia/Shanghai with +86 as
// its country code, holding the eight calls, and a key that reads every call.
type exportFixture struct {
	*fixture
	zone   *time.Location
	ids    map[string]int // call id -> call number
	key    string
	admin  string
	superv string
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithConfig(t, func(c *config.Config) {
		c.TimeZone = zone
		c.DefaultCountryCode = "86"
	})
	e := &exportFixture{fixture: f, zone: zone, ids: map[string]int{}}
	e.admin = f.seedAdmin()
	e.superv = f.seedSupervisor("sofia", 1020)
	_, e.key = f.issueKey(e.admin, "history:read:all")
	for _, c := range cdrExportCalls {
		started, err := time.ParseInLocation("2006-01-02 15:04:05", c.start, zone)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.Must(uuid.NewV7())
		cdr := store.CDR{
			CallID: id, StartedAt: started, EndedAt: started.Add(time.Duration(c.billSec+7) * time.Second),
			CallType: c.callType, Status: c.status, FromNumber: c.from, ToNumber: c.to,
			RingSec: 7, BillSec: c.billSec, TalkSec: c.billSec, TotalSec: c.billSec + 7,
			HangupCause: "NORMAL_CLEARING",
		}
		if c.billSec > 0 {
			cdr.AnsweredAt = started.Add(7 * time.Second)
		}
		if err := f.st.Ledger().InsertCDR(context.Background(), cdr); err != nil {
			t.Fatalf("InsertCDR(call %d): %v", c.no, err)
		}
		e.ids[id.String()] = c.no
	}
	return e
}

// day is midnight of a calendar day in the deployment's zone, as the screen
// sends it.
func (e *exportFixture) day(y int, m time.Month, d int) string {
	return time.Date(y, m, d, 0, 0, 0, 0, e.zone).Format(time.RFC3339)
}

// download asks for the export with the given query and request headers.
func (e *exportFixture) download(t *testing.T, query url.Values, cookie, bearer string, header map[string]string) reply {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.server.URL+"/api/v1/cdrs/export?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "aicc_session", Value: cookie})
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read the file: %v", err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	return reply{status: res.StatusCode, code: env.Error.Code, body: raw, header: res.Header}
}

// records parses a downloaded file: the BOM is required and stripped.
func records(t *testing.T, r reply) [][]string {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200; body %.300s", r.status, r.code, r.body)
	}
	body, ok := bytes.CutPrefix(r.body, []byte("\xEF\xBB\xBF"))
	if !ok {
		t.Fatalf("the file does not start with a UTF-8 byte order mark: % x", r.body[:min(len(r.body), 8)])
	}
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatalf("parse the file: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the file has no header row")
	}
	return rows
}

// listPage is the part of a CDRList these tests read.
type listPage struct {
	Items []struct {
		CallID string `json:"callId"`
	} `json:"items"`
	Total        int    `json:"total"`
	BillSecTotal int    `json:"billSecTotal"`
	TimeZone     string `json:"timeZone"`
}

func (e *exportFixture) list(t *testing.T, query url.Values) listPage {
	t.Helper()
	r := e.in(t).do(call{method: http.MethodGet, path: "/cdrs?" + query.Encode(), bearer: e.key})
	if r.status != http.StatusOK {
		t.Fatalf("GET /cdrs?%s = %d (%s); body %.300s", query.Encode(), r.status, r.code, r.body)
	}
	var page listPage
	if err := json.Unmarshal(r.body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return page
}

// pageThrough collects the call numbers listCDRs pages through, two at a time.
func (e *exportFixture) pageThrough(t *testing.T, query url.Values) []int {
	t.Helper()
	var out []int
	for offset := 0; ; offset += 2 {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("limit", "2")
		q.Set("offset", strconv.Itoa(offset))
		page := e.list(t, q)
		for _, it := range page.Items {
			out = append(out, e.ids[it.CallID])
		}
		if len(page.Items) < 2 {
			return out
		}
	}
}

// The list answers the totals of the whole filter on every page, and the zone
// a client must turn its days into windows in.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheCallRecordListCarriesTheFilteredTotalsAndTheZone(t *testing.T) {
	e := newExportFixture(t)

	december := url.Values{"from": {e.day(2026, time.December, 1)}, "to": {e.day(2027, time.January, 1)}, "limit": {"2"}}
	page := e.list(t, december)
	if len(page.Items) != 2 || page.Total != 6 || page.BillSecTotal != 3785 {
		t.Errorf("December, a page of 2: %d items, total %d, billSecTotal %d; want 2, 6, 3785",
			len(page.Items), page.Total, page.BillSecTotal)
	}
	if page.TimeZone != "Asia/Shanghai" {
		t.Errorf("timeZone = %q, want Asia/Shanghai", page.TimeZone)
	}

	for _, tc := range []struct {
		name  string
		query url.Values
		want  []int
	}{
		{"one local day", url.Values{"from": {e.day(2026, time.December, 1)}, "to": {e.day(2026, time.December, 2)}},
			[]int{6, 5, 4, 3, 2}},
		{"the called number", url.Values{"toNumber": {"13800000001"}}, []int{8, 7, 3, 1}},
		{"a billed range", url.Values{"billSecMin": {"60"}, "billSecMax": {"61"}}, []int{8, 2}},
		{"billed at all", url.Values{"billSecMin": {"1"}}, []int{8, 7, 2, 1}},
		{"with the status", url.Values{"toNumber": {"13800000001"}, "status": {"ANSWERED"}}, []int{8, 7, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.pageThrough(t, tc.query); !slices.Equal(got, tc.want) {
				t.Errorf("calls = %v, want %v", got, tc.want)
			}
		})
	}

	// An agent's own listing is a CDRList too, and says the same zone.
	r := e.do(call{method: http.MethodGet, path: "/cdrs/mine", cookie: e.agent.cookie})
	var mine listPage
	if err := json.Unmarshal(r.body, &mine); err != nil || r.status != http.StatusOK || mine.TimeZone != "Asia/Shanghai" {
		t.Errorf("GET /cdrs/mine = %d, timeZone %q, err %v; body %.200s", r.status, mine.TimeZone, err, r.body)
	}
}

// The file holds exactly the calls listCDRs pages through for the same
// filter, in the same order, dated in the deployment's zone.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheCallRecordExportHoldsWhatTheListPagesThrough(t *testing.T) {
	e := newExportFixture(t)

	window := func(extra url.Values, from, to string) url.Values {
		q := url.Values{"from": {from}, "to": {to}}
		for k, v := range extra {
			q[k] = v
		}
		return q
	}
	all := func(extra url.Values) url.Values {
		return window(extra, e.day(2026, time.November, 30), e.day(2027, time.January, 2))
	}
	for _, tc := range []struct {
		name  string
		query url.Values
		want  []int
	}{
		{"December", window(nil, e.day(2026, time.December, 1), e.day(2027, time.January, 1)), []int{7, 6, 5, 4, 3, 2}},
		{"one local day", window(nil, e.day(2026, time.December, 1), e.day(2026, time.December, 2)), []int{6, 5, 4, 3, 2}},
		{"every call", all(nil), []int{8, 7, 6, 5, 4, 3, 2, 1}},
		{"the called number", all(url.Values{"toNumber": {"13800000001"}}), []int{8, 7, 3, 1}},
		{"a billed range", all(url.Values{"billSecMin": {"60"}, "billSecMax": {"61"}}), []int{8, 2}},
		{"billed at all", all(url.Values{"billSecMin": {"1"}}), []int{8, 7, 2, 1}},
		{"the status", all(url.Values{"status": {"ANSWERED"}, "fromNumber": {"0215555010"}}), []int{8, 7, 2, 1}},
		{"nothing", all(url.Values{"toNumber": {"99999"}}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paged := e.pageThrough(t, tc.query)
			if !slices.Equal(paged, tc.want) {
				t.Fatalf("listCDRs pages through %v, want %v", paged, tc.want)
			}
			rows := records(t, e.download(t, tc.query, "", e.key, nil))
			var exported []int
			for _, row := range rows[1:] {
				exported = append(exported, e.ids[row[0]])
			}
			if !slices.Equal(exported, paged) {
				t.Errorf("the file holds %v, the list pages through %v", exported, paged)
			}
		})
	}

	// One file, read cell by cell.
	r := e.download(t, all(nil), "", e.key, nil)
	if ct := r.header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := r.header.Get("Content-Disposition"); cd != `attachment; filename="calls-20261130-20270101.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !bytes.Contains(r.body, []byte("\r\n")) {
		t.Error("the file's lines do not end in CRLF")
	}
	rows := records(t, r)
	wantHeader := []string{"Call ID", "Start date (Asia/Shanghai)", "Start time (Asia/Shanghai)", "Call type",
		"Caller number", "Called number", "Status", "Hangup cause", "Ring duration (s)", "Talk duration (s)",
		"Billed duration (s)", "Total duration (s)", "Campaign ID", "Lead intent"}
	if !slices.Equal(rows[0], wantHeader) {
		t.Errorf("header = %q\nwant %q", rows[0], wantHeader)
	}
	byNo := map[int][]string{}
	for _, row := range rows[1:] {
		byNo[e.ids[row[0]]] = row
	}
	// Dated in Shanghai whatever zone the client or the server is in: call 1
	// is the last of November there and call 2 the first of December.
	for no, want := range map[int][2]string{
		1: {"2026-11-30", "23:59:50"}, 2: {"2026-12-01", "00:00:05"},
		7: {"2026-12-31", "23:59:59"}, 8: {"2027-01-01", "00:00:00"},
	} {
		if got := [2]string{byNo[no][1], byNo[no][2]}; got != want {
			t.Errorf("call %d is dated %v, want %v", no, got, want)
		}
	}
	// Numbers are text: a leading 0 survives, and the tab that makes a
	// spreadsheet keep it is the first character.
	if got := byNo[1][4:6]; !slices.Equal(got, []string{"\t02155550101", "\t13800000001"}) {
		t.Errorf("call 1 numbers = %q", got)
	}
	if got := byNo[6][3:7]; !slices.Equal(got, []string{"INTERNAL", "\t1001", "\t1002", "ANSWERED"}) {
		t.Errorf("call 6 = %q", got)
	}
	if got := byNo[7][7:14]; !slices.Equal(got, []string{"NORMAL_CLEARING", "7", "3725", "3725", "3732", "", ""}) {
		t.Errorf("call 7 cause and durations = %q", got)
	}
	if got := byNo[3][10]; got != "0" {
		t.Errorf("call 3 billSec = %q, want 0", got)
	}
}

// Who may download it: what reads every call record reads the file, whether a
// person or a key, and it is the same file; an agent and a key without the
// scope are refused.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheCallRecordExportNeedsTheScopeThatReadsEveryCall(t *testing.T) {
	e := newExportFixture(t)
	query := url.Values{"from": {e.day(2026, time.December, 1)}, "to": {e.day(2027, time.January, 1)}}

	byKey := e.download(t, query, "", e.key, nil)
	bySupervisor := e.download(t, query, e.superv, "", nil)
	byAdmin := e.download(t, query, e.admin, "", nil)
	if byKey.status != http.StatusOK || bySupervisor.status != http.StatusOK || byAdmin.status != http.StatusOK {
		t.Fatalf("key %d, supervisor %d, admin %d; want 200 for each", byKey.status, bySupervisor.status, byAdmin.status)
	}
	if !bytes.Equal(byKey.body, bySupervisor.body) || !bytes.Equal(byKey.body, byAdmin.body) {
		t.Error("a key with history:read:all downloads a different file from a person's session")
	}

	if r := e.download(t, query, e.agent.cookie, "", nil); r.status != http.StatusForbidden {
		t.Errorf("an agent's login: status %d (%s), want 403", r.status, r.code)
	}
	_, own := e.issueKey(e.admin, "history:read:own")
	if r := e.download(t, query, "", own, nil); r.status != http.StatusForbidden {
		t.Errorf("a key without history:read:all: status %d (%s), want 403", r.status, r.code)
	}
	if r := e.download(t, query, "", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("no credential: status %d (%s), want 401", r.status, r.code)
	}
}

// The window is required and two calendar months at most, in the deployment's
// zone, and a refusal is the error envelope, never a file.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheCallRecordExportWindowIsBoundedToTwoMonths(t *testing.T) {
	e := newExportFixture(t)
	from := e.day(2026, time.December, 1)

	for _, tc := range []struct {
		name      string
		query     url.Values
		status    int
		wantField string
		wantRule  string
	}{
		{"two months exactly", url.Values{"from": {from}, "to": {e.day(2027, time.February, 1)}}, http.StatusOK, "", ""},
		{"one day more", url.Values{"from": {from}, "to": {e.day(2027, time.February, 2)}}, http.StatusBadRequest, "to", "EXPORT_WINDOW_TOO_LONG"},
		{"a second more", url.Values{"from": {from}, "to": {time.Date(2027, time.February, 1, 0, 0, 1, 0, e.zone).Format(time.RFC3339)}},
			http.StatusBadRequest, "to", "EXPORT_WINDOW_TOO_LONG"},
		// The same two months written in UTC: counted in UTC they would be a
		// day more than two months, so this passes only because the months
		// are counted in the deployment's zone.
		{"the same window written in UTC", url.Values{"from": {"2026-11-30T16:00:00Z"}, "to": {"2027-01-31T16:00:00Z"}}, http.StatusOK, "", ""},
		{"no from", url.Values{"to": {e.day(2026, time.December, 2)}}, http.StatusBadRequest, "from", ""},
		{"no to", url.Values{"from": {from}}, http.StatusBadRequest, "to", ""},
		{"neither", url.Values{}, http.StatusBadRequest, "", ""},
		{"an unparseable bound", url.Values{"from": {"yesterday"}, "to": {from}}, http.StatusBadRequest, "from", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := e.download(t, tc.query, "", e.key, nil)
			if r.status != tc.status {
				t.Fatalf("status = %d (%s), want %d; body %.300s", r.status, r.code, tc.status, r.body)
			}
			if tc.status == http.StatusOK {
				records(t, r)
				return
			}
			if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("a refusal is %q, want the JSON envelope", ct)
			}
			var env struct {
				Error struct {
					Code   string         `json:"code"`
					Params map[string]any `json:"params"`
				} `json:"error"`
			}
			if err := json.Unmarshal(r.body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != string(CodeValidationFailed) {
				t.Errorf("code = %q, want VALIDATION_FAILED", env.Error.Code)
			}
			if tc.wantField != "" && env.Error.Params["field"] != tc.wantField {
				t.Errorf("params.field = %v, want %s", env.Error.Params["field"], tc.wantField)
			}
			if tc.wantRule != "" {
				if env.Error.Params["rule"] != tc.wantRule || env.Error.Params["maxMonths"] != float64(2) {
					t.Errorf("params = %v, want rule %s and maxMonths 2", env.Error.Params, tc.wantRule)
				}
			}
		})
	}
}

// The header row is in the request's language; nothing else is translated.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheCallRecordExportHeaderFollowsTheRequestLanguage(t *testing.T) {
	e := newExportFixture(t)
	query := url.Values{"from": {e.day(2026, time.December, 1)}, "to": {e.day(2026, time.December, 2)}}
	zhHeader := []string{"通话 ID", "开始日期（Asia/Shanghai）", "开始时间（Asia/Shanghai）", "通话类型",
		"主叫号码", "被叫号码", "状态", "挂断原因", "振铃时长（秒）", "通话时长（秒）", "计费时长（秒）", "总时长（秒）",
		"活动 ID", "意向"}

	for _, tc := range []struct {
		name     string
		language string
		accept   string
		wantZh   bool
	}{
		{"English by default", "", "", false},
		{"the language parameter", "zh", "", true},
		{"Accept-Language", "", "zh-CN,zh;q=0.9,en;q=0.8", true},
		{"the parameter wins over the header", "en", "zh-CN", false},
		{"the header's weights", "", "en;q=0.4, zh-Hans;q=0.6", true},
		{"a language with no header row of its own", "", "fr-FR, de;q=0.9", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range query {
				q[k] = v
			}
			if tc.language != "" {
				q.Set("language", tc.language)
			}
			header := map[string]string{}
			if tc.accept != "" {
				header["Accept-Language"] = tc.accept
			}
			rows := records(t, e.download(t, q, "", e.key, header))
			if isZh := slices.Equal(rows[0], zhHeader); isZh != tc.wantZh {
				t.Errorf("header = %q, want Chinese %v", rows[0], tc.wantZh)
			}
			// Enum values stay the contract's whatever the header's language.
			if len(rows) < 2 || rows[1][3] != "INTERNAL" || rows[1][6] != "ANSWERED" {
				t.Errorf("first row = %q, want INTERNAL and ANSWERED as written", rows[1])
			}
		})
	}
}

// Cells a spreadsheet would misread: numbers in E.164 of the deployment's code
// come out national, a long number and a leading zero stay text, a field with
// a comma, a quote or a line break is quoted, and a caller id that is a
// formula is not run.
//
// Needs a real server (AICC_TEST_DATABASE_URL).
func TestTheCallRecordExportWritesCellsASpreadsheetKeeps(t *testing.T) {
	e := newExportFixture(t)
	at := time.Date(2026, time.October, 15, 10, 0, 0, 0, e.zone)
	campaignless := uuid.Must(uuid.NewV7())
	for _, cdr := range []store.CDR{
		{CallID: campaignless, StartedAt: at, EndedAt: at.Add(time.Minute), CallType: "INBOUND",
			Status: store.CDRStatusAnswered, FromNumber: "+862155550101", ToNumber: "123456789012345678",
			HangupCause: "A,B \"C\"\nD"},
		{CallID: uuid.Must(uuid.NewV7()), StartedAt: at.Add(time.Second), EndedAt: at.Add(time.Minute),
			CallType: "INBOUND", Status: store.CDRStatusNoAnswer, FromNumber: "=HYPERLINK(\"x\")",
			ToNumber: "+8613800000001", HangupCause: "-1"},
	} {
		if err := e.st.Ledger().InsertCDR(context.Background(), cdr); err != nil {
			t.Fatal(err)
		}
	}
	query := url.Values{"from": {e.day(2026, time.October, 15)}, "to": {e.day(2026, time.October, 16)}}
	r := e.download(t, query, "", e.key, nil)
	// Quoted, the quote doubled; the line break inside is CRLF like every
	// other in the file, and reads back as one.
	if !bytes.Contains(r.body, []byte(`"A,B ""C""`+"\r\nD\"")) {
		t.Errorf("a field with a comma, a quote and a line break is not quoted: %q", r.body)
	}
	rows := records(t, r)
	if len(rows) != 3 {
		t.Fatalf("%d rows, want a header and 2 calls", len(rows))
	}
	formula, plain := rows[1], rows[2]
	if got := plain[4:8]; !slices.Equal(got, []string{"\t02155550101", "\t123456789012345678", "ANSWERED", "A,B \"C\"\nD"}) {
		t.Errorf("cells = %q", got)
	}
	if got := formula[4:8]; !slices.Equal(got, []string{`'=HYPERLINK("x")`, "\t13800000001", "NO_ANSWER", "'-1"}) {
		t.Errorf("cells = %q", got)
	}
}

// Accept-Language is read for the two languages there is a header row in.
func TestPreferredLanguage(t *testing.T) {
	t.Parallel()
	for header, want := range map[string]string{
		"":                                  "",
		"zh":                                "zh",
		"zh-CN,zh;q=0.9,en;q=0.8":           "zh",
		"en-US,en;q=0.9,zh-CN;q=0.8":        "en",
		"fr, zh;q=0.5":                      "zh",
		"fr, de":                            "",
		"zh;q=0, en;q=0.1":                  "en",
		"zh;q=0":                            "",
		"EN-gb":                             "en",
		"en;q=0.5, zh;q=0.5":                "en",
		"*":                                 "",
		" zh-Hant-TW ; q=0.7 , en ; q=0.6 ": "zh",
	} {
		if got := preferredLanguage(header); got != want {
			t.Errorf("preferredLanguage(%q) = %q, want %q", header, got, want)
		}
	}
}

// The file is named by the days it covers, in the deployment's zone.
func TestCDRExportFileName(t *testing.T) {
	t.Parallel()
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, shanghai) }
	for _, tc := range []struct {
		from, to time.Time
		want     string
	}{
		{day(time.December, 1), day(time.December, 2), "calls-20261201-20261201.csv"},
		{day(time.December, 1), day(time.December, 1).AddDate(0, 1, 0), "calls-20261201-20261231.csv"},
		{day(time.December, 1), day(time.December, 1).Add(10 * time.Hour), "calls-20261201-20261201.csv"},
		{day(time.December, 1), day(time.December, 1), "calls-20261201-20261201.csv"},
		{day(time.December, 2), day(time.December, 1), "calls-20261202-20261202.csv"},
	} {
		if got := cdrExportFileName(tc.from.UTC(), tc.to.UTC(), shanghai); got != tc.want {
			t.Errorf("cdrExportFileName(%s, %s) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestNumberCellAndGuardFormula(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                   "",
		"02155550101":        "\t02155550101",
		"+8613800000001":     "\t+8613800000001",
		"123456789012345678": "\t123456789012345678",
		"1001":               "\t1001",
		"+":                  "'+",
		"anonymous":          "anonymous",
		"=1+1":               "'=1+1",
		"@SUM(A1)":           "'@SUM(A1)",
		"-2":                 "'-2",
		" =cmd":              "' =cmd",
		"*86":                "*86",
	} {
		if got := numberCell(in); got != want {
			t.Errorf("numberCell(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"NORMAL_CLEARING": "NORMAL_CLEARING",
		"\tx":             "'\tx",
		"\rx":             "'\rx",
		"+x":              "'+x",
		"a=b":             "a=b",
	} {
		if got := guardFormula(in); got != want {
			t.Errorf("guardFormula(%q) = %q, want %q", in, got, want)
		}
	}
}
