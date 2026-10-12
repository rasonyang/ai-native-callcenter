// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// MaxImportRows is how many rows one import request may carry. A larger list
// is several requests: a request that size is already a long transaction, and
// the per-row result has to come back in one response.
const MaxImportRows = 5000

// The limits on a contact's slots are the call's own, because slots reach the
// flow as the call's business data (userData): a value the flow would truncate
// (flow.MaxSlotValueLen characters) or the call would drop (32 keys,
// telephony.UserDataMaxKeys; a test in internal/httpapi keeps the two equal,
// because telephony cannot be imported from here) is refused at import, where
// somebody can fix the list, rather than lost on a call.
const (
	MaxSlotValueRunes = flow.MaxSlotValueLen
	MaxSlotsPerRow    = 32
)

// RowStatus is what an import did with one row.
type RowStatus string

const (
	RowImported  RowStatus = "IMPORTED"
	RowDuplicate RowStatus = "DUPLICATE"
	RowRejected  RowStatus = "REJECTED"
)

// RowCode says why a row was rejected. The first two are phonenum's.
type RowCode string

const (
	RowNumberEmpty     RowCode = RowCode(phonenum.NumberEmpty)
	RowNumberInvalid   RowCode = RowCode(phonenum.NumberInvalid)
	RowTimeZoneInvalid RowCode = "TIME_ZONE_INVALID"
	RowSlotsTooMany    RowCode = "SLOTS_TOO_MANY"
	RowSlotUnknown     RowCode = "SLOT_UNKNOWN"
	RowSlotTooLong     RowCode = "SLOT_TOO_LONG"
)

// ImportRow is one line of a contact list as a client sent it.
type ImportRow struct {
	Number   string
	TimeZone string
	Slots    map[string]string
}

// RowResult is what became of one row. Results come back in input order.
type RowResult struct {
	Index  int
	Status RowStatus
	// ContactID is set for IMPORTED and DUPLICATE: the new contact, or the one
	// that already had the number (first wins; its slots are not touched).
	ContactID uuid.UUID
	// Number is the canonical number, set whenever the number was readable.
	Number string
	// Code and Detail say why a REJECTED row was rejected; Detail names the
	// offending slot or time zone.
	Code   RowCode
	Detail string
}

// ImportResult is the outcome of one import.
type ImportResult struct {
	Imported   int
	Duplicates int
	Rejected   int
	Rows       []RowResult
}

// ImportContacts adds rows to a campaign's list. A row that cannot be used is
// rejected on its own; it never fails the others. The good rows go in as one
// transaction, so a database failure leaves the list as it was. Numbers are
// canonicalised with the campaign's country code (else the deployment's), and
// a number the campaign already holds (or that appeared earlier in this batch)
// is a DUPLICATE: the first contact wins and keeps its slots. The do-not-call
// list is not consulted here; the dialer checks it at admission, so a number
// added to the list later still stops.
//
// Allowed while the campaign is DRAFT, RUNNING or PAUSED.
func (s *Service) ImportContacts(ctx context.Context, id uuid.UUID, rows []ImportRow) (ImportResult, error) {
	if len(rows) == 0 {
		return ImportResult{}, invalid("contacts", "EMPTY", "there are no contacts to import")
	}
	if len(rows) > MaxImportRows {
		return ImportResult{}, invalid("contacts", "TOO_MANY_ROWS",
			"an import carries at most %d contacts, got %d; send the list in several requests", MaxImportRows, len(rows))
	}
	c, err := s.repo.Get(ctx, id)
	if err != nil {
		return ImportResult{}, err
	}
	if c.Status.IsTerminal() {
		return ImportResult{}, NewTransitionError(c.Status, "IMPORT_CONTACTS")
	}
	d, err := s.repo.DIDFlow(ctx, c.DIDID)
	if err != nil {
		return ImportResult{}, err
	}
	declared := flow.UserDataNames(d.Spec)
	country := s.countryCode(c)

	res := ImportResult{Rows: make([]RowResult, len(rows))}
	var batch []NewContact
	// at maps a canonical number to its position in batch; pos maps a usable
	// row to the position of the contact that stands for it.
	at := map[string]int{}
	pos := map[int]int{}
	// isFirst marks the rows that claim their number; the others repeat one.
	isFirst := map[int]bool{}

	for i, row := range rows {
		out := RowResult{Index: i}
		number, code, detail := checkRow(row, country, declared)
		out.Number = number
		if code != "" {
			out.Status, out.Code, out.Detail = RowRejected, code, detail
			res.Rows[i] = out
			res.Rejected++
			continue
		}
		res.Rows[i] = out
		if k, seen := at[number]; seen {
			// The same person twice in one list, perhaps in two formats. The
			// first row keeps the contact; this one is told which.
			pos[i] = k
			continue
		}
		cid, err := uuid.NewV7()
		if err != nil {
			return ImportResult{}, err
		}
		at[number] = len(batch)
		pos[i] = len(batch)
		isFirst[i] = true
		batch = append(batch, NewContact{
			ID: cid, Number: number, NumberRaw: row.Number,
			TimeZone: row.TimeZone, Slots: cleanSlots(row.Slots),
		})
	}

	if len(batch) == 0 {
		return res, nil
	}
	stored, err := s.repo.ImportContacts(ctx, id, batch)
	if err != nil {
		return ImportResult{}, err
	}
	for i, k := range pos {
		r := &res.Rows[i]
		r.ContactID = stored[k].ID
		if isFirst[i] && stored[k].IsNew {
			r.Status = RowImported
			res.Imported++
			continue
		}
		// Either the campaign held the number already or an earlier row of
		// this batch claimed it: first wins, and the contact is left as it is.
		r.Status = RowDuplicate
		res.Duplicates++
	}
	return res, nil
}

// checkRow validates one row. It returns the canonical number whenever the
// number was readable, and a code when the row is not usable. Checks run in
// the order a person fixes a list: the number, the time zone, then the slots.
func checkRow(row ImportRow, country string, declared []string) (number string, code RowCode, detail string) {
	number, err := phonenum.Canonical(row.Number, country)
	if err != nil {
		return "", RowCode(phonenum.ReasonOf(err)), ""
	}
	if row.TimeZone != "" {
		if _, err := LoadZone(row.TimeZone); err != nil {
			return number, RowTimeZoneInvalid, row.TimeZone
		}
	}
	slots := cleanSlots(row.Slots)
	if len(slots) > MaxSlotsPerRow {
		return number, RowSlotsTooMany, ""
	}
	names := make([]string, 0, len(slots))
	for name := range slots {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(declared, name) {
			return number, RowSlotUnknown, name
		}
	}
	for _, name := range names {
		if utf8.RuneCountInString(slots[name]) > MaxSlotValueRunes {
			return number, RowSlotTooLong, name
		}
	}
	return number, "", ""
}

// cleanSlots prepares a row's slot values the way the flow's own slot tool
// prepares a value the model records: surrounding space trimmed and every
// control character, line breaks included, turned into a space, so that a
// value rendered into an instruction cannot add lines of its own to it. A slot
// left empty is not provided and is dropped.
func cleanSlots(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for name, value := range in {
		v := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, value))
		if v != "" {
			out[name] = v
		}
	}
	return out
}
