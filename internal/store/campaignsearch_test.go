// SPDX-License-Identifier: Apache-2.0

package store

// Searching a campaign's contacts and attempts by phone number
// (listCampaignContacts and listCampaignAttempts with phoneNumber), through the
// campaign service on PostgreSQL: the number is read with the campaign's
// country code, matched exactly on the whole number, combined with the other
// filters, counted in total, and never found in another campaign.
//
// The seed is two campaigns written through the paths production writes them
// by: the import (national numbers completed with the campaign's code, 86),
// the attempt's write-ahead row, the lead the call records and the finish
// routine. The searched campaign holds 3,000 contacts: Chinese mobiles and
// landlines with their area codes, generated from a fixed seed, and a dozen
// probes whose history is chosen. The second campaign holds some of the same
// numbers. Every number is synthetic.

import (
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// searchProbe is a contact of the searched campaign whose history is chosen.
type searchProbe struct {
	typed string // as imported
	e164  string
	// outs are its attempts' outcomes in order; "" is an attempt left ringing.
	outs []campaign.Outcome
	// lead is recorded on its last attempt.
	lead         campaign.LeadIntent
	isTestNumber bool
	// isListed puts the number on the do-not-call list before it is dialled.
	isListed    bool
	wantStatus  campaign.ContactStatus
	wantOutcome campaign.Outcome
	id          uuid.UUID
}

// newSearchProbes returns the probes. mobile, twin and landline end in the
// same eight digits.
func newSearchProbes() map[string]*searchProbe {
	return map[string]*searchProbe{
		"mobile": {typed: "138 2468 1357", e164: "+8613824681357",
			outs: []campaign.Outcome{campaign.OutcomeNoAnswer, campaign.OutcomeAnswered}, lead: campaign.LeadHigh,
			wantStatus: campaign.ContactDone, wantOutcome: campaign.OutcomeAnswered},
		"twin": {typed: "139-2468-1357", e164: "+8613924681357", wantStatus: campaign.ContactActive},
		"landline": {typed: "021-2468 1357", e164: "+862124681357",
			outs:       []campaign.Outcome{campaign.OutcomeNoAnswer, campaign.OutcomeBusy, campaign.OutcomeNoAnswer},
			wantStatus: campaign.ContactExhausted, wantOutcome: campaign.OutcomeNoAnswer},
		"beijing": {typed: "(010) 6555 0125", e164: "+861065550125", isListed: true,
			outs:       []campaign.Outcome{campaign.OutcomeDNCSkipped},
			wantStatus: campaign.ContactDoNotCall, wantOutcome: campaign.OutcomeDNCSkipped},
		"shenzhen": {typed: "0755-2555-0126", e164: "+8675525550126",
			outs:       []campaign.Outcome{campaign.OutcomeAnswered},
			wantStatus: campaign.ContactDone, wantOutcome: campaign.OutcomeAnswered},
		"opt-out": {typed: "150 5555 0127", e164: "+8615055550127",
			outs: []campaign.Outcome{campaign.OutcomeAnswered}, lead: campaign.LeadDoNotCall,
			wantStatus: campaign.ContactDoNotCall, wantOutcome: campaign.OutcomeAnswered},
		"retrying": {typed: "18655550128", e164: "+8618655550128",
			outs:       []campaign.Outcome{campaign.OutcomeNoAnswer},
			wantStatus: campaign.ContactActive, wantOutcome: campaign.OutcomeNoAnswer},
		"ringing": {typed: "177-5555-0129", e164: "+8617755550129",
			outs: []campaign.Outcome{""}, wantStatus: campaign.ContactActive},
		"test number": {typed: "199 5555 0130", e164: "+8619955550130", isTestNumber: true,
			outs: []campaign.Outcome{campaign.OutcomeAnswered}, lead: campaign.LeadLow,
			wantStatus: campaign.ContactDone, wantOutcome: campaign.OutcomeAnswered},
		"invalid": {typed: "131 5555 0131", e164: "+8613155550131",
			outs:       []campaign.Outcome{campaign.OutcomeInvalidNumber},
			wantStatus: campaign.ContactInvalidNumber, wantOutcome: campaign.OutcomeInvalidNumber},
		"foreign": {typed: "+1 (202) 555-0143", e164: "+12025550143",
			outs: []campaign.Outcome{campaign.OutcomeAnswered}, lead: campaign.LeadMedium,
			wantStatus: campaign.ContactDone, wantOutcome: campaign.OutcomeAnswered},
		// shared is in both campaigns: here it waits for a retry, in the other
		// campaign it is a HIGH lead.
		"shared": {typed: "137 5555 0132", e164: "+8613755550132",
			outs:       []campaign.Outcome{campaign.OutcomeNoAnswer},
			wantStatus: campaign.ContactActive, wantOutcome: campaign.OutcomeNoAnswer},
	}
}

const (
	// otherOnly is in the second campaign only; absent is in neither.
	otherOnlyTyped = "136 5555 0133"
	otherOnlyE164  = "+8613655550133"
	absentTyped    = "135 5555 0134"
	absentE164     = "+8613555550134"
	// sharedTail is the eight digits mobile, twin and landline end in.
	sharedTail = "24681357"
)

// searchContact is one contact of the searched campaign as the seed wrote it.
type searchContact struct {
	id       uuid.UUID
	typed    string
	e164     string
	attempts int // in the searched campaign
}

type searchSeed struct {
	st       *Store
	svc      *campaign.Service
	attempts *AttemptStore
	clock    time.Time
	rng      *rand.Rand

	probes          map[string]*searchProbe
	searched, other campaign.Campaign
	contacts        []*searchContact // the searched campaign's, in import order
	// otherShared is the other campaign's contact on the shared number.
	otherShared uuid.UUID
	mobiles     int
	landlines   int
}

var searchMobilePrefixes = []string{
	"130", "131", "132", "133", "135", "136", "137", "138", "139", "150", "151", "152", "157", "158", "159",
	"166", "170", "176", "177", "178", "180", "181", "182", "183", "185", "186", "187", "188", "189", "191", "199",
}

// searchAreaCodes are area codes without their trunk 0, each with its
// subscriber number's length.
var searchAreaCodes = []struct {
	code string
	n    int
}{
	{"10", 8}, {"20", 8}, {"21", 8}, {"22", 8}, {"23", 8}, {"24", 8}, {"25", 8}, {"27", 8}, {"28", 8},
	{"29", 8}, {"311", 7}, {"371", 8}, {"451", 8}, {"510", 7}, {"571", 8}, {"591", 7}, {"755", 8},
	{"757", 8}, {"769", 8}, {"871", 7}, {"898", 7},
}

func (s *searchSeed) digits(n int) string {
	var b strings.Builder
	for range n {
		b.WriteByte(byte('0' + s.rng.IntN(10)))
	}
	return b.String()
}

// mobile is a generated mobile number: its national digits, as a person might
// type them, and its E.164 form.
func (s *searchSeed) mobile() (typed, e164 string) {
	national := searchMobilePrefixes[s.rng.IntN(len(searchMobilePrefixes))] + s.digits(8)
	switch s.rng.IntN(3) {
	case 0:
		typed = national
	case 1:
		typed = national[:3] + " " + national[3:7] + " " + national[7:]
	default:
		typed = national[:3] + "-" + national[3:7] + "-" + national[7:]
	}
	return typed, "+86" + national
}

// landline is a generated landline number, typed with its trunk 0.
func (s *searchSeed) landline() (typed, e164 string) {
	ac := searchAreaCodes[s.rng.IntN(len(searchAreaCodes))]
	sub := string(byte('2'+s.rng.IntN(7))) + s.digits(ac.n-1)
	switch s.rng.IntN(3) {
	case 0:
		typed = "0" + ac.code + sub
	case 1:
		typed = "0" + ac.code + "-" + sub
	default:
		typed = "(0" + ac.code + ") " + sub[:len(sub)-4] + " " + sub[len(sub)-4:]
	}
	return typed, "+86" + ac.code + sub
}

func (s *searchSeed) tick() time.Time {
	s.clock = s.clock.Add(30 * time.Second)
	return s.clock
}

// dial places one call per outcome, in order, and ends each with it; "" leaves
// the call ringing. A lead is recorded on the last call while it is up.
func (s *searchSeed) dial(t *testing.T, contactID uuid.UUID, outs []campaign.Outcome, lead campaign.LeadIntent) {
	t.Helper()
	ctx := context.Background()
	for i, out := range outs {
		at := s.tick()
		a, err := s.attempts.Begin(ctx, contactID, at)
		if err != nil {
			t.Fatalf("begin %s: %v", contactID, err)
		}
		if out == "" {
			continue
		}
		if i == len(outs)-1 && lead != "" {
			if ok, err := s.attempts.SetLead(ctx, a.CallID, lead, leadNotes[lead]); err != nil || !ok {
				t.Fatalf("lead %s: %v %v", contactID, ok, err)
			}
		}
		talk := 0
		if campaign.IsAnsweredOutcome(out) {
			talk = 30 + s.rng.IntN(200)
		}
		if _, err := s.attempts.Finish(ctx, a.CallID, campaign.Ending{
			Outcome: out, HangupCause: outcomeCause[out], TalkSec: talk, EndedAt: at.Add(20 * time.Second),
		}); err != nil {
			t.Fatalf("finish %s %s: %v", contactID, out, err)
		}
	}
}

// create makes a campaign with the country code 86 and imports rows into it
// through the service, as the API does; it returns the import's rows.
func (s *searchSeed) create(t *testing.T, didID, queueID uuid.UUID, name string, typed []string) (campaign.Campaign, []campaign.RowResult) {
	t.Helper()
	ctx := context.Background()
	policy := campaign.DefaultRetryPolicy()
	c, err := s.svc.Create(ctx, campaign.New{
		Name: name, DIDID: didID, TimeZone: "Asia/Shanghai", CallingWindow: attemptWindow,
		DefaultCountryCode: "86", RetryPolicy: &policy, FollowUpQueueID: &queueID,
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]campaign.ImportRow, len(typed))
	for i, n := range typed {
		rows[i] = campaign.ImportRow{Number: n}
	}
	res, err := s.svc.ImportContacts(ctx, c.ID, rows)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != len(typed) {
		t.Fatalf("%s: imported %d of %d: %+v", name, res.Imported, len(typed), res)
	}
	if c, err = s.svc.Start(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	return c, res.Rows
}

func openSearchSeed(t *testing.T) *searchSeed {
	t.Helper()
	ctx := context.Background()
	st, cs := openCampaigns(t)
	st.OutboundHours = dayrange.Range{Start: 8 * 60, End: 21 * 60}
	s := &searchSeed{
		st: st, attempts: st.Attempts(),
		// The deployment's code is 1: an 86 number found from national digits
		// was completed with the campaign's own code.
		svc:    campaign.NewService(cs, nil, campaign.Env{OutboundHours: st.OutboundHours, DefaultCountryCode: "1"}),
		clock:  time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC), // 10:00 in Shanghai, a Monday
		rng:    rand.New(rand.NewPCG(617, 2026)),
		probes: newSearchProbes(),
	}
	didID, _ := seedDID(t, st, "95099", false)
	var queueID uuid.UUID
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO queues (id, name, ext_number) VALUES ($1, 'follow-up', '7001') RETURNING id`,
		uuid.Must(uuid.NewV7())).Scan(&queueID); err != nil {
		t.Fatalf("queue: %v", err)
	}

	// 3,000 numbers: the probes first, then 2,000 mobiles and the rest
	// landlines, none of them a probe's, the other campaign's or the absent one.
	taken := map[string]bool{otherOnlyE164: true, absentE164: true}
	var typed []string
	names := slices.Sorted(maps.Keys(s.probes))
	for _, name := range names {
		p := s.probes[name]
		taken[p.e164] = true
		typed = append(typed, p.typed)
		s.contacts = append(s.contacts, &searchContact{typed: p.typed, e164: p.e164})
	}
	for len(s.contacts) < 3000 {
		gen := s.landline
		if s.mobiles < 2000 {
			gen = s.mobile
		}
		n, e164 := gen()
		if taken[e164] {
			continue
		}
		taken[e164] = true
		if s.mobiles < 2000 {
			s.mobiles++
		} else {
			s.landlines++
		}
		typed = append(typed, n)
		s.contacts = append(s.contacts, &searchContact{typed: n, e164: e164})
	}
	var rows []campaign.RowResult
	s.searched, rows = s.create(t, didID, queueID, "Broadband renewal", typed)
	byNumber := map[string]*searchContact{}
	for i, r := range rows {
		c := s.contacts[i]
		if r.Number != c.e164 {
			t.Fatalf("row %d %q was read as %s, want %s", i, c.typed, r.Number, c.e164)
		}
		c.id = r.ContactID
		byNumber[c.e164] = c
	}
	for _, p := range s.probes {
		p.id = byNumber[p.e164].id
	}

	// Lists the probes need, then their histories.
	for _, p := range s.probes {
		if p.isTestNumber {
			if _, _, err := st.TestNumbers().Add(ctx, p.e164, "probe", nil); err != nil {
				t.Fatal(err)
			}
		}
		if p.isListed {
			if _, _, err := st.DNC().Add(ctx, p.e164, DNCSourceManual, nil, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range names {
		p := s.probes[name]
		s.dial(t, p.id, p.outs, p.lead)
		byNumber[p.e164].attempts = len(p.outs)
	}

	// Histories for some of the generated contacts, so that every status, with
	// and without a lead, is among them too. The rest are never dialled.
	background := slices.Clone(s.contacts[len(s.probes):])
	s.rng.Shuffle(len(background), func(i, j int) { background[i], background[j] = background[j], background[i] })
	take := func(n int) []*searchContact {
		out := background[:n]
		background = background[n:]
		return out
	}
	retries := func(n int) []campaign.Outcome {
		out := make([]campaign.Outcome, n)
		for i := range out {
			out[i] = []campaign.Outcome{campaign.OutcomeNoAnswer, campaign.OutcomeBusy}[s.rng.IntN(2)]
		}
		return out
	}
	leads := []campaign.LeadIntent{campaign.LeadHigh, campaign.LeadMedium, campaign.LeadLow, campaign.LeadNone}
	for i, c := range take(120) { // answered with a lead
		outs := append(retries(s.rng.IntN(2)), campaign.OutcomeAnswered)
		s.dial(t, c.id, outs, leads[i%len(leads)])
		c.attempts = len(outs)
	}
	for _, c := range take(80) { // answered, no lead
		outs := append(retries(s.rng.IntN(2)), campaign.OutcomeAnswered)
		s.dial(t, c.id, outs, "")
		c.attempts = len(outs)
	}
	for _, c := range take(10) { // asked not to be called again
		s.dial(t, c.id, []campaign.Outcome{campaign.OutcomeAnswered}, campaign.LeadDoNotCall)
		c.attempts = 1
	}
	for _, c := range take(50) { // three rings nobody answered
		s.dial(t, c.id, retries(3), "")
		c.attempts = 3
	}
	for _, c := range take(20) {
		s.dial(t, c.id, []campaign.Outcome{campaign.OutcomeInvalidNumber}, "")
		c.attempts = 1
	}
	for _, c := range take(100) { // waiting for a retry
		outs := retries(1 + s.rng.IntN(2))
		s.dial(t, c.id, outs, "")
		c.attempts = len(outs)
	}

	// The other campaign: the shared number, its own, and 200 of the first
	// campaign's generated numbers, 50 of which are HIGH leads there.
	overlap := s.contacts[len(s.probes) : len(s.probes)+200]
	otherTyped := []string{"13755550132", otherOnlyTyped}
	for _, c := range overlap {
		otherTyped = append(otherTyped, c.e164)
	}
	s.other, rows = s.create(t, didID, queueID, "Broadband renewal, second wave", otherTyped)
	s.otherShared = rows[0].ContactID
	s.dial(t, rows[0].ContactID, []campaign.Outcome{campaign.OutcomeAnswered}, campaign.LeadHigh)
	s.dial(t, rows[1].ContactID, []campaign.Outcome{campaign.OutcomeAnswered}, campaign.LeadHigh)
	for _, r := range rows[2:52] {
		s.dial(t, r.ContactID, []campaign.Outcome{campaign.OutcomeNoAnswer, campaign.OutcomeAnswered}, campaign.LeadHigh)
	}
	return s
}

func (s *searchSeed) listContacts(t *testing.T, id uuid.UUID, f campaign.ContactFilter) ([]campaign.Contact, int) {
	t.Helper()
	if f.Limit == 0 {
		f.Limit = 50
	}
	items, total, err := s.svc.ListContacts(context.Background(), id, f)
	if err != nil {
		t.Fatalf("contacts %+v: %v", f, err)
	}
	return items, int(total)
}

func (s *searchSeed) listAttempts(t *testing.T, id uuid.UUID, f campaign.AttemptFilter) ([]campaign.Attempt, int) {
	t.Helper()
	if f.Limit == 0 {
		f.Limit = 50
	}
	items, total, err := s.svc.ListAttempts(context.Background(), id, f)
	if err != nil {
		t.Fatalf("attempts %+v: %v", f, err)
	}
	return items, int(total)
}

// spellings are the ways a person or an integration writes one number.
func spellings(p *searchProbe) []string {
	out := []string{p.e164, p.typed, "00" + p.e164[1:]}
	if strings.HasPrefix(p.e164, "+86") {
		national := p.e164[3:]
		if !strings.HasPrefix(national, "1") || strings.HasPrefix(national, "10") {
			national = "0" + national // a landline is dialled with its trunk 0
		}
		spaced := national[:3] + " " + national[3:7] + " " + national[7:]
		dashed := national[:3] + "-" + national[3:7] + "-" + national[7:]
		out = append(out, national, spaced, dashed, "+86 "+p.e164[3:6]+"-"+p.e164[6:])
	}
	return out
}

func TestACampaignIsSearchedByPhoneNumber(t *testing.T) {
	ctx := context.Background()
	s := openSearchSeed(t)
	searched, other := s.searched.ID, s.other.ID
	probes := s.probes

	t.Run("the seed is the one the issue asks for", func(t *testing.T) {
		if _, total := s.listContacts(t, searched, campaign.ContactFilter{Limit: 1}); total != 3000 {
			t.Errorf("%d contacts, want 3000", total)
		}
		if s.mobiles != 2000 || s.landlines != 3000-2000-len(probes) {
			t.Errorf("%d mobiles and %d landlines generated", s.mobiles, s.landlines)
		}
		for _, st := range []campaign.ContactStatus{
			campaign.ContactActive, campaign.ContactDone, campaign.ContactExhausted,
			campaign.ContactInvalidNumber, campaign.ContactDoNotCall,
		} {
			if _, n := s.listContacts(t, searched, campaign.ContactFilter{Status: st, Limit: 1}); n < 2 {
				t.Errorf("%d contacts are %s", n, st)
			}
		}
		leads := 0
		for _, l := range campaign.AllLeadIntents() {
			_, n := s.listContacts(t, searched, campaign.ContactFilter{LeadIntent: l, Limit: 1})
			if n == 0 {
				t.Errorf("no contact is a %s lead", l)
			}
			leads += n
		}
		finished := 0
		for _, o := range campaign.AllOutcomes() {
			_, n := s.listContacts(t, searched, campaign.ContactFilter{LastOutcome: o, Limit: 1})
			finished += n
		}
		if leads == 0 || leads >= 3000 || finished == 0 || finished >= 3000 || finished <= leads {
			t.Errorf("%d leads and %d contacts with a finished attempt of 3000", leads, finished)
		}
		for _, name := range []string{"twin", "landline"} {
			if !strings.HasSuffix(probes[name].e164, sharedTail) || !strings.HasSuffix(probes["mobile"].e164, sharedTail) {
				t.Errorf("%s does not end in %s", name, sharedTail)
			}
		}
		var direct int
		if err := s.st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM campaign_contacts WHERE number_e164 = $1`, probes["shared"].e164).Scan(&direct); err != nil || direct != 2 {
			t.Errorf("the shared number is held by %d campaigns (%v), want 2", direct, err)
		}
		if err := s.st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM campaign_contacts WHERE number_e164 = ANY($1)`, []string{otherOnlyE164, absentE164}).Scan(&direct); err != nil || direct != 1 {
			t.Errorf("the other campaign's own number and the absent one are held %d times (%v), want 1", direct, err)
		}
	})

	t.Run("every contact is found by the number it was imported as, and only it", func(t *testing.T) {
		for _, c := range s.contacts {
			items, total := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: c.typed})
			if total != 1 || len(items) != 1 || items[0].ID != c.id || items[0].CampaignID != searched || items[0].Number != c.e164 {
				t.Fatalf("%q: total %d, %+v; want the one contact %s", c.typed, total, items, c.id)
			}
		}
	})

	t.Run("every dial of a number is one request", func(t *testing.T) {
		for _, c := range s.contacts {
			items, total := s.listAttempts(t, searched, campaign.AttemptFilter{PhoneNumber: c.e164})
			if total != c.attempts || len(items) != c.attempts {
				t.Fatalf("%s: total %d, %d items; the seed placed %d calls", c.e164, total, len(items), c.attempts)
			}
			for i, a := range items {
				if a.ContactID != c.id || a.CampaignID != searched || a.Number != c.e164 {
					t.Fatalf("%s: attempt %+v is another number's or another campaign's", c.e164, a)
				}
				if i > 0 && a.StartedAt.After(items[i-1].StartedAt) {
					t.Errorf("%s: not newest first", c.e164)
				}
			}
		}
	})

	t.Run("a number is read in every spelling", func(t *testing.T) {
		for name, p := range probes {
			for _, typed := range spellings(p) {
				items, total := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: typed})
				if total != 1 || len(items) != 1 || items[0].ID != p.id {
					t.Errorf("%s as %q: total %d, %+v", name, typed, total, items)
					continue
				}
				if c := items[0]; c.Status != p.wantStatus || c.LastOutcome != p.wantOutcome || c.LeadIntent != p.lead {
					t.Errorf("%s: %s/%s/%s, want %s/%s/%s", name, c.Status, c.LastOutcome, c.LeadIntent,
						p.wantStatus, p.wantOutcome, p.lead)
				}
				attempts, total := s.listAttempts(t, searched, campaign.AttemptFilter{PhoneNumber: typed})
				if total != len(p.outs) || len(attempts) != len(p.outs) {
					t.Errorf("%s as %q: %d attempts (total %d), want %d", name, typed, len(attempts), total, len(p.outs))
					continue
				}
				for i, a := range attempts {
					// Newest first: the last outcome comes first.
					if want := p.outs[len(p.outs)-1-i]; a.Outcome != want || a.ContactID != p.id || a.IsTestNumber != p.isTestNumber {
						t.Errorf("%s attempt %d: %s for %s (test %v), want %s", name, i, a.Outcome, a.ContactID, a.IsTestNumber, want)
					}
				}
			}
		}
	})

	t.Run("part of a number finds nothing, and numbers ending alike find themselves", func(t *testing.T) {
		for _, typed := range []string{
			sharedTail, "2468-1357", "+86" + sharedTail, // the eight digits alone
			"3824681357", "824681357", // a mobile without its first digits
			"2468 1357 0", "1382468135", // a digit too many, or too few
		} {
			if items, total := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: typed}); total != 0 || len(items) != 0 {
				t.Errorf("%q found %d contacts: %+v", typed, total, items)
			}
			if items, total := s.listAttempts(t, searched, campaign.AttemptFilter{PhoneNumber: typed}); total != 0 || len(items) != 0 {
				t.Errorf("%q found %d attempts", typed, total)
			}
		}
		for _, name := range []string{"mobile", "twin", "landline"} {
			p := probes[name]
			items, _ := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: p.typed})
			if len(items) != 1 || items[0].ID != p.id {
				t.Errorf("%s found %+v", name, items)
			}
		}
	})

	t.Run("a number of another campaign or of none finds nothing here", func(t *testing.T) {
		for _, typed := range []string{otherOnlyTyped, otherOnlyE164, absentTyped, absentE164} {
			if items, total := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: typed}); total != 0 || len(items) != 0 {
				t.Errorf("contacts %q: total %d, %+v", typed, total, items)
			}
			if items, total := s.listAttempts(t, searched, campaign.AttemptFilter{PhoneNumber: typed}); total != 0 || len(items) != 0 {
				t.Errorf("attempts %q: total %d", typed, total)
			}
		}
		// Found where it is.
		if _, total := s.listContacts(t, other, campaign.ContactFilter{PhoneNumber: otherOnlyTyped}); total != 1 {
			t.Errorf("the other campaign's own number: total %d there", total)
		}

		// The shared number: this campaign's contact and calls only, though
		// the other campaign's are a HIGH lead and an answered call.
		shared := probes["shared"]
		items, total := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: shared.typed})
		if total != 1 || items[0].ID != shared.id || items[0].CampaignID != searched {
			t.Errorf("shared: total %d, %+v", total, items)
		}
		if _, total := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: shared.typed, LeadIntent: campaign.LeadHigh}); total != 0 {
			t.Errorf("shared is a HIGH lead in the other campaign only, but %d here", total)
		}
		attempts, total := s.listAttempts(t, searched, campaign.AttemptFilter{PhoneNumber: shared.e164})
		if total != 1 || attempts[0].CampaignID != searched || attempts[0].Outcome != campaign.OutcomeNoAnswer {
			t.Errorf("shared's attempts here: total %d, %+v", total, attempts)
		}
		items, total = s.listContacts(t, other, campaign.ContactFilter{PhoneNumber: shared.typed, LeadIntent: campaign.LeadHigh})
		if total != 1 || items[0].ID != s.otherShared {
			t.Errorf("shared in the other campaign: total %d, %+v", total, items)
		}
		if _, total := s.listAttempts(t, other, campaign.AttemptFilter{PhoneNumber: shared.e164, Outcome: campaign.OutcomeAnswered}); total != 1 {
			t.Errorf("shared's answered call in the other campaign: total %d", total)
		}
	})

	t.Run("the number narrows together with the other filters", func(t *testing.T) {
		mobile, landline, test, ringing := probes["mobile"], probes["landline"], probes["test number"], probes["ringing"]
		yes, no := true, false
		for _, tc := range []struct {
			what string
			f    campaign.ContactFilter
			want int
		}{
			{"is this number a HIGH lead", campaign.ContactFilter{PhoneNumber: mobile.typed, LeadIntent: campaign.LeadHigh}, 1},
			{"is it a LOW lead", campaign.ContactFilter{PhoneNumber: mobile.typed, LeadIntent: campaign.LeadLow}, 0},
			{"DONE", campaign.ContactFilter{PhoneNumber: mobile.typed, Status: campaign.ContactDone}, 1},
			{"ACTIVE", campaign.ContactFilter{PhoneNumber: mobile.typed, Status: campaign.ContactActive}, 0},
			{"last ANSWERED", campaign.ContactFilter{PhoneNumber: mobile.typed, LastOutcome: campaign.OutcomeAnswered}, 1},
			{"last NO_ANSWER", campaign.ContactFilter{PhoneNumber: mobile.typed, LastOutcome: campaign.OutcomeNoAnswer}, 0},
			{"all three", campaign.ContactFilter{PhoneNumber: mobile.typed, Status: campaign.ContactDone,
				LeadIntent: campaign.LeadHigh, LastOutcome: campaign.OutcomeAnswered}, 1},
			{"landline EXHAUSTED on NO_ANSWER", campaign.ContactFilter{PhoneNumber: landline.typed,
				Status: campaign.ContactExhausted, LastOutcome: campaign.OutcomeNoAnswer}, 1},
			{"landline as a lead", campaign.ContactFilter{PhoneNumber: landline.typed, LeadIntent: campaign.LeadNone}, 0},
			{"a test number's contact", campaign.ContactFilter{PhoneNumber: test.typed, IsTestNumber: &yes}, 1},
			{"a test number's contact left out", campaign.ContactFilter{PhoneNumber: test.typed, IsTestNumber: &no}, 0},
			{"a contact still ringing has no last outcome", campaign.ContactFilter{PhoneNumber: ringing.typed, IsTestNumber: &no}, 1},
		} {
			items, total := s.listContacts(t, searched, tc.f)
			if total != tc.want || len(items) != tc.want {
				t.Errorf("contacts, %s: total %d, %d items, want %d", tc.what, total, len(items), tc.want)
			}
		}
		for _, tc := range []struct {
			what string
			f    campaign.AttemptFilter
			want int
		}{
			{"answered", campaign.AttemptFilter{PhoneNumber: mobile.typed, Outcome: campaign.OutcomeAnswered}, 1},
			{"no answer", campaign.AttemptFilter{PhoneNumber: mobile.typed, Outcome: campaign.OutcomeNoAnswer}, 1},
			{"busy", campaign.AttemptFilter{PhoneNumber: mobile.typed, Outcome: campaign.OutcomeBusy}, 0},
			{"its own contact", campaign.AttemptFilter{PhoneNumber: landline.typed, ContactID: &landline.id}, 3},
			{"another contact", campaign.AttemptFilter{PhoneNumber: mobile.typed, ContactID: &landline.id}, 0},
			{"landline busy", campaign.AttemptFilter{PhoneNumber: landline.e164, Outcome: campaign.OutcomeBusy}, 1},
			{"a test call", campaign.AttemptFilter{PhoneNumber: test.typed, IsTestNumber: &yes}, 1},
			{"a test call left out", campaign.AttemptFilter{PhoneNumber: test.typed, IsTestNumber: &no}, 0},
			{"the mobile is no test number", campaign.AttemptFilter{PhoneNumber: mobile.typed, IsTestNumber: &yes}, 0},
			{"a call still ringing has no outcome", campaign.AttemptFilter{PhoneNumber: ringing.typed, Outcome: campaign.OutcomeNoAnswer}, 0},
		} {
			items, total := s.listAttempts(t, searched, tc.f)
			if total != tc.want || len(items) != tc.want {
				t.Errorf("attempts, %s: total %d, %d items, want %d", tc.what, total, len(items), tc.want)
			}
		}
	})

	t.Run("total counts the filter, not the page", func(t *testing.T) {
		landline := probes["landline"]
		items, total := s.listAttempts(t, searched, campaign.AttemptFilter{PhoneNumber: landline.typed, Limit: 1, Offset: 1})
		if total != 3 || len(items) != 1 || items[0].Outcome != campaign.OutcomeBusy {
			t.Errorf("the landline's second-newest call: total %d, %+v", total, items)
		}
		items2, total := s.listContacts(t, searched, campaign.ContactFilter{PhoneNumber: landline.typed, Offset: 1})
		if total != 1 || len(items2) != 0 {
			t.Errorf("past the one contact: total %d, %d items", total, len(items2))
		}
		// Unnarrowed, the same filters count far more.
		if _, total := s.listContacts(t, searched, campaign.ContactFilter{LeadIntent: campaign.LeadHigh}); total < 30 {
			t.Errorf("HIGH leads without a number: total %d", total)
		}
		if _, total := s.listAttempts(t, searched, campaign.AttemptFilter{}); total < 500 {
			t.Errorf("attempts without a number: total %d", total)
		}
	})

	t.Run("a value that is not a phone number is refused, not answered empty", func(t *testing.T) {
		for _, typed := range []string{"abc", "138", "138 0013 800O", "+0123456789", "1234567890123456789", "--"} {
			_, _, err := s.svc.ListContacts(ctx, searched, campaign.ContactFilter{PhoneNumber: typed, Limit: 10})
			var ce *campaign.Error
			if !errors.As(err, &ce) || !errors.Is(err, campaign.ErrUnreadableParameter) || ce.Field != "phoneNumber" {
				t.Errorf("contacts %q: %v", typed, err)
			}
			_, _, err = s.svc.ListAttempts(ctx, searched, campaign.AttemptFilter{PhoneNumber: typed, Limit: 10})
			if !errors.Is(err, campaign.ErrUnreadableParameter) {
				t.Errorf("attempts %q: %v", typed, err)
			}
		}
	})
}
