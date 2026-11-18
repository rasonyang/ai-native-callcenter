// SPDX-License-Identifier: Apache-2.0

package httpapi

// listCampaignContacts and listCampaignAttempts narrowed by phoneNumber, over
// HTTP on a real PostgreSQL: the spellings a query string carries (an encoded
// "+", an unencoded one, spaces and dashes), the 400 for a value that is not a
// phone number, and a campaigns:read key answered exactly as a session is. The
// matching rules themselves are pinned in the store's
// TestACampaignIsSearchedByPhoneNumber.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

type searchPage struct {
	Items []struct {
		ID          string  `json:"id"`
		CampaignID  string  `json:"campaignId"`
		ContactID   string  `json:"contactId"`
		Number      string  `json:"number"`
		LeadIntent  *string `json:"leadIntent"`
		Outcome     *string `json:"outcome"`
		LastOutcome *string `json:"lastOutcome"`
	} `json:"items"`
	Total *int `json:"total"`
}

func TestACampaignIsSearchedByPhoneNumberOverHTTP(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.seedAdmin()
	supervisor := f.seedSupervisor("sup", 1100)
	_, readKey := f.issueKey(admin, "campaigns:read")
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, func(b map[string]any) { b["defaultCountryCode"] = "86" })
	other := f.createCampaign(admin, didID, func(b map[string]any) {
		b["name"] = "Second wave"
		b["defaultCountryCode"] = "86"
	})

	// Forty mobiles and a Shanghai landline ending in the same eight digits as
	// one of them; the second campaign holds one of the mobiles too.
	rows := []map[string]any{{"number": "021-2468 0007"}}
	for i := range 40 {
		rows = append(rows, map[string]any{"number": fmt.Sprintf("138 2468 %04d", i)})
	}
	for id, list := range map[string][]map[string]any{c.ID: rows, other.ID: {{"number": "13824680007"}}} {
		if r := f.post(admin, "/campaigns/"+id+"/contacts", map[string]any{"contacts": list}); r.status != http.StatusOK {
			t.Fatalf("import: %d %s", r.status, r.body)
		}
		if r := f.post(admin, "/campaigns/"+id+"/start", nil); r.status != http.StatusOK {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
	}
	const number = "+8613824680007"
	var contactID, otherContactID uuid.UUID
	for id, into := range map[string]*uuid.UUID{c.ID: &contactID, other.ID: &otherContactID} {
		if err := f.st.Pool.QueryRow(ctx, `SELECT id FROM campaign_contacts WHERE campaign_id = $1 AND number_e164 = $2`,
			id, number).Scan(into); err != nil {
			t.Fatal(err)
		}
	}
	// Two calls to the number here (no answer, then a HIGH lead) and one in
	// the second campaign.
	attempts := f.st.Attempts()
	at := time.Date(2026, 6, 1, 2, 0, 0, 0, time.UTC)
	for i, step := range []struct {
		contact uuid.UUID
		out     campaign.Outcome
		lead    campaign.LeadIntent
	}{
		{contactID, campaign.OutcomeNoAnswer, ""},
		{contactID, campaign.OutcomeAnswered, campaign.LeadHigh},
		{otherContactID, campaign.OutcomeAnswered, campaign.LeadLow},
	} {
		started := at.Add(time.Duration(i) * time.Minute)
		a, err := attempts.Begin(ctx, step.contact, started)
		if err != nil {
			t.Fatal(err)
		}
		if step.lead != "" {
			if ok, err := attempts.SetLead(ctx, a.CallID, step.lead, "Asked for the offer by email."); err != nil || !ok {
				t.Fatalf("lead: %v %v", ok, err)
			}
		}
		if _, err := attempts.Finish(ctx, a.CallID, campaign.Ending{
			Outcome: step.out, HangupCause: "NORMAL_CLEARING", EndedAt: started.Add(30 * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// get asks with every credential that may read campaigns and checks that
	// each is answered the same, byte for byte.
	get := func(path string) reply {
		t.Helper()
		bySession := f.do(call{method: http.MethodGet, path: path, cookie: admin})
		for who, r := range map[string]reply{
			"a supervisor's session": f.do(call{method: http.MethodGet, path: path, cookie: supervisor}),
			"a campaigns:read key":   f.do(call{method: http.MethodGet, path: path, bearer: readKey}),
		} {
			if r.status != bySession.status || !bytes.Equal(r.body, bySession.body) {
				t.Errorf("%s: %s answered %d %s, a session %d %s", path, who, r.status, r.body, bySession.status, bySession.body)
			}
		}
		return bySession
	}
	page := func(path string) searchPage {
		t.Helper()
		r := get(path)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
		var p searchPage
		if err := json.Unmarshal(r.body, &p); err != nil || p.Total == nil {
			t.Fatalf("%s: %s (%v)", path, r.body, err)
		}
		return p
	}
	contacts := "/campaigns/" + c.ID + "/contacts"
	calls := "/campaigns/" + c.ID + "/attempts"
	q := func(v string) string { return "?phoneNumber=" + url.QueryEscape(v) }

	// The spellings a query string carries. An unencoded "+" arrives as a
	// space and still reads as "+".
	for _, raw := range []string{
		q("13824680007"), q("138 2468 0007"), q("138-2468-0007"), q("+86 138 2468 0007"), q(number),
		"?phoneNumber=%2B8613824680007", "?phoneNumber=+8613824680007", "?phoneNumber=008613824680007",
	} {
		p := page(contacts + raw)
		if *p.Total != 1 || len(p.Items) != 1 || p.Items[0].Number != number || p.Items[0].CampaignID != c.ID ||
			p.Items[0].LeadIntent == nil || *p.Items[0].LeadIntent != "HIGH" {
			t.Errorf("contacts %s: %+v", raw, p)
		}
		p = page(calls + raw)
		if *p.Total != 2 || len(p.Items) != 2 || *p.Items[0].Outcome != "ANSWERED" || *p.Items[1].Outcome != "NO_ANSWER" {
			t.Errorf("attempts %s: %+v", raw, p)
		}
		for _, it := range p.Items {
			if it.CampaignID != c.ID || it.ContactID != contactID.String() || it.Number != number {
				t.Errorf("attempts %s: %+v is not this campaign's call to the number", raw, it)
			}
		}
	}

	// With the other filters, and total for each.
	for _, tc := range []struct {
		path string
		want int
	}{
		{contacts + q("13824680007") + "&leadIntent=HIGH", 1},
		{contacts + q("13824680007") + "&leadIntent=LOW", 0}, // LOW in the second campaign only
		{contacts + q("13824680007") + "&status=DONE&lastOutcome=ANSWERED", 1},
		{contacts + q("13824680007") + "&status=ACTIVE", 0},
		{contacts + q("13824680007") + "&isTestNumber=false", 1},
		{contacts + q("021 2468 0007"), 1}, // the landline that ends the same
		{contacts + q("24680007"), 0},      // the shared eight digits alone
		{contacts + q("13824680041"), 0},   // in no campaign
		{contacts, 41},
		{calls + q("13824680007") + "&outcome=ANSWERED", 1},
		{calls + q("13824680007") + "&outcome=BUSY", 0},
		{calls + q("13824680007") + "&contactId=" + contactID.String(), 2},
		{calls + q("13824680007") + "&contactId=" + otherContactID.String(), 0},
		{calls + q("13824680007") + "&limit=1&offset=1", 2},
		{calls + q("021 2468 0007"), 0},
		{"/campaigns/" + other.ID + "/contacts" + q("13824680007") + "&leadIntent=LOW", 1},
	} {
		if p := page(tc.path); *p.Total != tc.want {
			t.Errorf("%s: total %d, want %d", tc.path, *p.Total, tc.want)
		}
	}
	if p := page(contacts + q("13824680041")); p.Items == nil || len(p.Items) != 0 {
		t.Errorf("no match is an empty array: %+v", p.Items)
	}

	// A value that is not a phone number is a 400 naming the parameter, on
	// both listings and for every credential.
	for _, tc := range []struct{ raw, rule string }{
		{q("abc"), "NUMBER_INVALID"},
		{q("138"), "NUMBER_INVALID"},
		{q("1382468000７"), "NUMBER_INVALID"},
		{q("13824680007; DROP"), "NUMBER_INVALID"},
		{q("+0123456789"), "NUMBER_INVALID"},
		{q("---"), "NUMBER_EMPTY"},
		{"?phoneNumber=+", "NUMBER_INVALID"}, // an unencoded "+" alone
	} {
		for _, path := range []string{contacts + tc.raw, calls + tc.raw, contacts + tc.raw + "&leadIntent=HIGH"} {
			r := get(path)
			wantReply(t, path, r, http.StatusBadRequest, "VALIDATION_FAILED")
			if e := errOf(t, r); e.Error.Params["field"] != "phoneNumber" || e.Error.Params["rule"] != tc.rule {
				t.Errorf("%s: params %v, want field phoneNumber and rule %s", path, e.Error.Params, tc.rule)
			}
		}
	}

	// Empty is no narrowing; an unknown campaign is not found before the
	// number is read.
	if p := page(contacts + "?phoneNumber="); *p.Total != 41 {
		t.Errorf("an empty phoneNumber narrowed the list to %d", *p.Total)
	}
	wantReply(t, "unknown campaign", get("/campaigns/"+uuid.NewString()+"/contacts"+q("abc")), http.StatusNotFound, "NOT_FOUND")
}
