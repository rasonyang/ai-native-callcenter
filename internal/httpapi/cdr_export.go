// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/csv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// The call-record export (exportCDRs): what its file holds and how each cell
// is written. The operation itself is ExportCDRs in ledger_handlers.go.

// cdrExportMaxMonths is the widest window one export covers, in calendar
// months of the deployment's time zone.
const cdrExportMaxMonths = 2

// cdrExportHeaders are the header row, in column order, per language. The two
// start columns take the zone's name (%s).
var cdrExportHeaders = map[string][]string{
	"en": {
		"Call ID", "Start date (%s)", "Start time (%s)", "Call type",
		"Caller number", "Called number", "Status", "Hangup cause",
		"Ring duration (s)", "Talk duration (s)", "Billed duration (s)", "Total duration (s)",
		"Campaign ID", "Lead intent",
	},
	"zh": {
		"通话 ID", "开始日期（%s）", "开始时间（%s）", "通话类型",
		"主叫号码", "被叫号码", "状态", "挂断原因",
		"振铃时长（秒）", "通话时长（秒）", "计费时长（秒）", "总时长（秒）",
		"活动 ID", "意向",
	},
}

// cdrExportLanguage is the header row's language: the language parameter, else
// what Accept-Language prefers of en and zh, else en.
func cdrExportLanguage(params api.ExportCDRsParams) string {
	if params.Language != nil {
		if _, ok := cdrExportHeaders[string(*params.Language)]; ok {
			return string(*params.Language)
		}
	}
	if params.AcceptLanguage != nil {
		if lang := preferredLanguage(*params.AcceptLanguage); lang != "" {
			return lang
		}
	}
	return "en"
}

// preferredLanguage reads an Accept-Language header for the languages the
// export has a header row in: the tag with the highest weight whose primary
// subtag is one of them, the earlier one on a tie, "" when none is acceptable.
// A weight of 0 means "not this one" and is never picked.
func preferredLanguage(header string) string {
	best, bestWeight := "", 0.0
	for part := range strings.SplitSeq(header, ",") {
		tag, attrs, _ := strings.Cut(part, ";")
		weight := 1.0
		for attr := range strings.SplitSeq(attrs, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(attr), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			if w, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				weight = w
			}
		}
		primary, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
		if _, ok := cdrExportHeaders[primary]; ok && weight > bestWeight {
			best, bestWeight = primary, weight
		}
	}
	return best
}

// cdrExportFileName names the file for its window: calls-<first day>-<last
// day>.csv in the deployment's zone. The window's end is exclusive, so a
// window of whole days names the last day it holds, not the midnight after.
func cdrExportFileName(from, to time.Time, zone *time.Location) string {
	last := to.Add(-time.Nanosecond)
	if last.Before(from) {
		last = from
	}
	return "calls-" + from.In(zone).Format("20060102") + "-" + last.In(zone).Format("20060102") + ".csv"
}

// cdrExportFile is the CSV being written: UTF-8 with a byte order mark, CRLF
// line ends, every cell quoted when it needs it (encoding/csv).
type cdrExportFile struct {
	w           http.ResponseWriter
	csv         *csv.Writer
	language    string
	zone        *time.Location
	countryCode string
	row         []string
}

func newCDRExportFile(w http.ResponseWriter, language string, zone *time.Location, countryCode string) *cdrExportFile {
	out := csv.NewWriter(w)
	out.UseCRLF = true
	return &cdrExportFile{w: w, csv: out, language: language, zone: zone, countryCode: countryCode,
		row: make([]string, len(cdrExportHeaders[language]))}
}

// begin sends the status line, the byte order mark and the header row.
func (f *cdrExportFile) begin(name string) {
	h := f.w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// Call records: not for a shared cache.
	h.Set("Cache-Control", "no-store")
	f.w.WriteHeader(http.StatusOK)
	// A spreadsheet that finds no BOM guesses a code page and garbles the
	// Chinese header row.
	_, _ = io.WriteString(f.w, "\uFEFF")
	header := make([]string, len(f.row))
	for i, name := range cdrExportHeaders[f.language] {
		header[i] = strings.ReplaceAll(name, "%s", f.zone.String())
	}
	_ = f.csv.Write(header)
}

// write adds a batch of calls and sends it on, so the client receives the
// file as it is read. The error is the client's: it is no longer there.
func (f *cdrExportFile) write(batch []store.CDR) error {
	for i := range batch {
		f.fill(&batch[i])
		if err := f.csv.Write(f.row); err != nil {
			return err
		}
	}
	return f.flush()
}

// end sends whatever the last batch left in the buffer.
func (f *cdrExportFile) end() error {
	return f.flush()
}

func (f *cdrExportFile) flush() error {
	f.csv.Flush()
	if err := f.csv.Error(); err != nil {
		return err
	}
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// fill writes one call into the row, in the columns of cdrExportHeaders.
// Enum values are the contract's own; an absent field is an empty cell.
func (f *cdrExportFile) fill(cdr *store.CDR) {
	started := cdr.StartedAt.In(f.zone)
	campaignID := ""
	if cdr.CampaignID != nil {
		campaignID = cdr.CampaignID.String()
	}
	f.row[0] = cdr.CallID.String()
	f.row[1] = started.Format("2006-01-02")
	f.row[2] = started.Format("15:04:05")
	f.row[3] = guardFormula(cdr.CallType)
	f.row[4] = numberCell(phonenum.National(cdr.FromNumber, f.countryCode))
	f.row[5] = numberCell(phonenum.National(cdr.ToNumber, f.countryCode))
	f.row[6] = guardFormula(cdr.Status)
	f.row[7] = guardFormula(cdr.HangupCause)
	f.row[8] = strconv.Itoa(cdr.RingSec)
	f.row[9] = strconv.Itoa(cdr.TalkSec)
	f.row[10] = strconv.Itoa(cdr.BillSec)
	f.row[11] = strconv.Itoa(cdr.TotalSec)
	f.row[12] = campaignID
	f.row[13] = guardFormula(string(cdr.LeadIntent))
}

// numberCell writes a telephone number so that a spreadsheet keeps it as
// text: a leading tab, as the browser's contact export writes one (asText in
// web/src/lib/csv.ts), so a leading 0 stays and an 18-digit number is not
// turned into a rounded figure. Only a value that is a number gets the tab; a
// caller id is whatever the far end sent, so anything else is free text and
// guarded as such.
func numberCell(v string) string {
	if v == "" {
		return ""
	}
	digits := strings.TrimPrefix(v, "+")
	if digits == "" {
		return guardFormula(v)
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return guardFormula(v)
		}
	}
	return "\t" + v
}

// guardFormula keeps a spreadsheet from running a cell: a value that starts
// like a formula (=, + , - or @, also after leading whitespace) or with a tab
// or a carriage return, which a spreadsheet may skip, gets a leading
// apostrophe, the OWASP convention for CSV injection. The same rule as
// guardFormula in web/src/lib/csv.ts.
func guardFormula(v string) string {
	if v == "" {
		return v
	}
	if v[0] == '\t' || v[0] == '\r' {
		return "'" + v
	}
	trimmed := strings.TrimLeftFunc(v, unicode.IsSpace)
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + v
	}
	return v
}
