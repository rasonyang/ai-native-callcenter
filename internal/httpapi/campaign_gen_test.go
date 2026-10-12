// SPDX-License-Identifier: Apache-2.0

package httpapi

// A synthetic customer list for the import tests, and the expected answer for
// every row of it.
//
// The expectations are built in, not computed from the code under test: a
// row is made from a person whose canonical number is chosen first, and then
// written in a format that should canonicalise back to it; a defective row is
// made by choosing the defect. The oracle at the end only has to replay the
// first-writer-wins rule over those choices. Everything is generated from a
// seed, so a failure names the row and reruns identically. No data is real:
// the numbers are random digits and the names are made up.

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// The slots the test flow reads (see campaignFlowJSON).
var declaredSlots = []string{"name", "plan", "city"}

// A format writes a person's national digits and canonical number as a
// client might have typed them.
type numberFormat struct {
	name  string
	write func(nat, canon string, r *rand.Rand) string
}

func group(s string, sizes ...int) []string {
	var out []string
	for _, n := range sizes {
		if len(s) < n {
			break
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

var mobileFormats = []numberFormat{
	{"e164", func(_, canon string, _ *rand.Rand) string { return canon }},
	{"00 prefix", func(nat, _ string, _ *rand.Rand) string { return "0086" + nat }},
	{"trunk zero", func(nat, _ string, _ *rand.Rand) string { return "0" + nat }},
	{"bare national", func(nat, _ string, _ *rand.Rand) string { return nat }},
	{"spaced", func(nat, _ string, _ *rand.Rand) string { return strings.Join(group(nat, 3, 4, 4), " ") }},
	{"dashed", func(nat, _ string, _ *rand.Rand) string { return strings.Join(group(nat, 3, 4, 4), "-") }},
	{"dotted", func(nat, _ string, _ *rand.Rand) string { return strings.Join(group(nat, 3, 4, 4), ".") }},
	{"e164 spaced", func(nat, _ string, _ *rand.Rand) string { return "+86 " + strings.Join(group(nat, 3, 4, 4), " ") }},
	{"e164 dashed", func(nat, _ string, _ *rand.Rand) string { return "+86-" + strings.Join(group(nat, 3, 4, 4), "-") }},
	{"padded", func(nat, _ string, _ *rand.Rand) string { return "  " + nat + "  " }},
}

var landlineFormats = []numberFormat{
	{"e164", func(_, canon string, _ *rand.Rand) string { return canon }},
	{"trunk zero", func(nat, _ string, _ *rand.Rand) string { return "0" + nat }},
	{"bracketed area", func(nat, _ string, _ *rand.Rand) string { return "(0" + nat[:2] + ") " + nat[2:6] + "-" + nat[6:] }},
	{"e164 bracketed", func(nat, _ string, _ *rand.Rand) string { return "+86 (" + nat[:2] + ") " + nat[2:6] + " " + nat[6:] }},
	{"dashed", func(nat, _ string, _ *rand.Rand) string { return "0" + nat[:2] + "-" + nat[2:] }},
	{"00 prefix", func(nat, _ string, _ *rand.Rand) string { return "0086" + nat }},
}

// person is somebody on the list: one canonical number.
type person struct {
	nat, canon string
	isLandline bool
}

func newPerson(r *rand.Rand, seen map[string]bool) person {
	for {
		var p person
		if r.IntN(100) < 78 {
			p.nat = fmt.Sprintf("1%d%09d", 3+r.IntN(7), r.IntN(1_000_000_000))
		} else {
			area := []string{"10", "21", "20", "28"}[r.IntN(4)]
			p.nat = fmt.Sprintf("%s%08d", area, 1_000_000+r.IntN(8_999_999))
			p.isLandline = true
		}
		p.canon = "+86" + p.nat
		if !seen[p.canon] {
			seen[p.canon] = true
			return p
		}
	}
}

func (p person) write(r *rand.Rand) (string, string) {
	formats := mobileFormats
	if p.isLandline {
		formats = landlineFormats
	}
	f := formats[r.IntN(len(formats))]
	return f.write(p.nat, p.canon, r), f.name
}

// Numbers that can never be a contact, each with the code they must be
// rejected with.
var badNumbers = []struct {
	number, code string
}{
	{"", "NUMBER_EMPTY"},
	{"   ", "NUMBER_EMPTY"},
	{" - ( ) . ", "NUMBER_EMPTY"},
	{"12345", "NUMBER_INVALID"},
	{"+86138", "NUMBER_INVALID"},
	{"1", "NUMBER_INVALID"},
	{"+8613800138000123456", "NUMBER_INVALID"},
	{"008613800138000123456", "NUMBER_INVALID"},
	{"1380013abcd", "NUMBER_INVALID"},
	{"call me later", "NUMBER_INVALID"},
	{"+86 138 0013 ABCD", "NUMBER_INVALID"},
	{"13800138000 ext 5", "NUMBER_INVALID"},
	{"+0123456789", "NUMBER_INVALID"},
	{"+", "NUMBER_INVALID"},
	{"00", "NUMBER_INVALID"},
	{"📞13800138000", "NUMBER_INVALID"},
	{"１３８００１３８０００", "NUMBER_INVALID"},
	{"13800138000;13800138001", "NUMBER_INVALID"},
}

var zones = []string{"Asia/Shanghai", "America/New_York", "Europe/London", "Australia/Lord_Howe", "UTC"}
var badZones = []string{"Mars/Phobos", "Local", " Asia/Shanghai", "Asia/Shanghai ", "GMT+8", "Asia\\Shanghai"}

// value is a slot value as sent and as it must come back.
type value struct{ sent, want string }

var plainValues = []value{
	{"王小明", "王小明"},
	{"Alice O'Brien", "Alice O'Brien"},
	{"Priority 99", "Priority 99"},
	{"𠮷野家 𪚥", "𠮷野家 𪚥"},           // rare characters outside the BMP
	{"😀👍🏽 thanks", "😀👍🏽 thanks"}, // emoji, one with a skin-tone modifier
	{"👨‍👩‍👧‍👦", "👨‍👩‍👧‍👦"},       // a ZWJ sequence: seven runes
	{"Müller & Söhne", "Müller & Söhne"},
	{"Zoë \"Z\" <z@example.com>", "Zoë \"Z\" <z@example.com>"},
	{"  王小明  ", "王小明"},
	{"line one\nline two", "line one line two"},
	{"tab\tseparated", "tab separated"},
	{"crlf\r\nbreak", "crlf  break"},
	{"北京", "北京"},
	{"Shanghai", "Shanghai"},
}

func capValue(r *rand.Rand, runes int) value {
	units := []string{"字", "a", "😀", "é", "𠮷"}
	u := units[r.IntN(len(units))]
	s := strings.Repeat(u, runes)
	return value{s, s}
}

type genRow struct {
	// JSON is the row as sent.
	JSON map[string]any
	// Index is its place in the request.
	Index int
	// Kind describes how it was made, for failure messages.
	Kind string

	// The oracle.
	WantCode   string // "" when the row is usable
	WantNumber string // canonical, "" when unreadable
	WantSlots  map[string]string
	WantZone   string
	Raw        string
}

type genList struct {
	Rows []genRow
	// WantStatus, WantFirst: the replay of first-writer-wins over Rows.
	WantStatus []string
	WantFirst  []int // for DUPLICATE: the index of the row that holds the contact
	Imported   int
	Duplicates int
	Rejected   int
}

// genContacts makes n rows from a seed.
func genContacts(seed uint64, n int) genList {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	seen := map[string]bool{}
	var people []person
	list := genList{}

	add := func(row genRow) {
		row.Index = len(list.Rows)
		list.Rows = append(list.Rows, row)
	}

	pickSlots := func() (sent, want map[string]string) {
		sent, want = map[string]string{}, map[string]string{}
		for _, name := range declaredSlots {
			switch r.IntN(10) {
			case 0, 1, 2: // missing optional slot
				continue
			case 3: // present but empty: the same as missing
				sent[name] = []string{"", "   ", "\n"}[r.IntN(3)]
				continue
			}
			v := plainValues[r.IntN(len(plainValues))]
			sent[name], want[name] = v.sent, v.want
		}
		return sent, want
	}

	for len(list.Rows) < n {
		roll := r.IntN(100)
		switch {
		case roll < 62 || len(people) == 0: // a new person
			p := newPerson(r, seen)
			people = append(people, p)
			number, fname := p.write(r)
			sentSlots, wantSlots := pickSlots()
			row := genRow{
				Kind: "new person, " + fname, WantNumber: p.canon, WantSlots: wantSlots, Raw: number,
				JSON: map[string]any{"number": number, "slots": sentSlots},
			}
			if r.IntN(100) < 25 {
				z := zones[r.IntN(len(zones))]
				row.JSON["timeZone"], row.WantZone = z, z
			}
			if len(sentSlots) == 0 && r.IntN(2) == 0 {
				delete(row.JSON, "slots") // no slots at all
			}
			add(row)

		case roll < 80: // somebody already on the list, written differently
			p := people[r.IntN(len(people))]
			number, fname := p.write(r)
			sentSlots, wantSlots := pickSlots()
			add(genRow{
				Kind: "repeat, " + fname, WantNumber: p.canon, WantSlots: wantSlots, Raw: number,
				JSON: map[string]any{"number": number, "slots": sentSlots},
			})

		case roll < 88: // a number that cannot be one
			bad := badNumbers[r.IntN(len(badNumbers))]
			row := genRow{
				Kind: fmt.Sprintf("bad number %q", bad.number), WantCode: bad.code, Raw: bad.number,
				JSON: map[string]any{"number": bad.number},
			}
			// Sometimes with other defects too: the number is judged first.
			if r.IntN(3) == 0 {
				row.JSON["timeZone"] = badZones[r.IntN(len(badZones))]
				row.JSON["slots"] = map[string]string{"undeclared": "x"}
			}
			add(row)

		case roll < 92: // a bad time zone on a good number
			p := newPerson(r, seen)
			people = append(people, p)
			number, _ := p.write(r)
			z := badZones[r.IntN(len(badZones))]
			row := genRow{
				Kind: "bad zone " + z, WantCode: "TIME_ZONE_INVALID", WantNumber: p.canon, Raw: number,
				JSON: map[string]any{"number": number, "timeZone": z},
			}
			if r.IntN(2) == 0 {
				row.JSON["slots"] = map[string]string{"undeclared": "x"} // zone is judged before slots
			}
			add(row)

		case roll < 94: // 33 slots: too many for a call to carry
			p := newPerson(r, seen)
			people = append(people, p)
			number, _ := p.write(r)
			slots := map[string]string{}
			for i := 0; i < 33; i++ {
				slots[fmt.Sprintf("extra%02d", i)] = "v" // also undeclared: the count is judged first
			}
			add(genRow{
				Kind: "33 slots", WantCode: "SLOTS_TOO_MANY", WantNumber: p.canon, Raw: number,
				JSON: map[string]any{"number": number, "slots": slots},
			})

		case roll < 97: // a slot the flow does not read
			p := newPerson(r, seen)
			people = append(people, p)
			number, _ := p.write(r)
			slots := map[string]string{"name": "ok", "favourite_colour": "blue"}
			if r.IntN(2) == 0 {
				slots["plan"] = capValue(r, 257).sent // too long as well: unknown is judged first
			}
			add(genRow{
				Kind: "unknown slot", WantCode: "SLOT_UNKNOWN", WantNumber: p.canon, Raw: number,
				JSON: map[string]any{"number": number, "slots": slots},
			})

		case roll < 99: // a value over the cap
			p := newPerson(r, seen)
			people = append(people, p)
			number, _ := p.write(r)
			over := capValue(r, 257+r.IntN(50))
			add(genRow{
				Kind: "value over the cap", WantCode: "SLOT_TOO_LONG", WantNumber: p.canon, Raw: number,
				JSON: map[string]any{"number": number, "slots": map[string]string{"name": "fine", "city": over.sent}},
			})

		default: // a value exactly at the cap
			p := newPerson(r, seen)
			people = append(people, p)
			number, _ := p.write(r)
			at := capValue(r, 256)
			add(genRow{
				Kind: "value at the cap", WantNumber: p.canon, Raw: number,
				WantSlots: map[string]string{"city": at.want},
				JSON:      map[string]any{"number": number, "slots": map[string]string{"city": at.sent}},
			})
		}
	}

	// Replay: a usable row claims its number if nobody has; later usable rows
	// with the number are duplicates of the claimant.
	claimed := map[string]int{}
	list.WantStatus = make([]string, len(list.Rows))
	list.WantFirst = make([]int, len(list.Rows))
	for i, row := range list.Rows {
		switch {
		case row.WantCode != "":
			list.WantStatus[i] = "REJECTED"
			list.Rejected++
		case claimedBy(claimed, row.WantNumber) >= 0:
			list.WantStatus[i] = "DUPLICATE"
			list.WantFirst[i] = claimed[row.WantNumber]
			list.Duplicates++
		default:
			claimed[row.WantNumber] = i
			list.WantStatus[i] = "IMPORTED"
			list.Imported++
		}
	}
	return list
}

func claimedBy(m map[string]int, number string) int {
	if i, ok := m[number]; ok {
		return i
	}
	return -1
}

// body is the request for the rows.
func (l genList) body() map[string]any {
	rows := make([]map[string]any, len(l.Rows))
	for i, r := range l.Rows {
		rows[i] = r.JSON
	}
	return map[string]any{"contacts": rows}
}
