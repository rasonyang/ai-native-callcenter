// SPDX-License-Identifier: Apache-2.0

package httpapi

// GET /campaigns/{campaignId}/outcome-summary over HTTP, on a real PostgreSQL:
// the wire shape the contract declares (every enum value once, in order, zero
// included), each count against the listing filtered the same way, and who may
// read it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// outcomeRow is one row of a breakdown by outcome.
type outcomeRow struct {
	Outcome string `json:"outcome"`
	Count   int    `json:"count"`
}

// outcomeSummaryView is the response as the contract spells it.
type outcomeSummaryView struct {
	CampaignID            string       `json:"campaignId"`
	AttemptsByOutcome     []outcomeRow `json:"attemptsByOutcome"`
	ContactsByLastOutcome []outcomeRow `json:"contactsByLastOutcome"`
	ContactsByLeadIntent  []struct {
		LeadIntent string `json:"leadIntent"`
		Count      int    `json:"count"`
	} `json:"contactsByLeadIntent"`
	OptOuts struct {
		SaidInConversation *int `json:"saidInConversation"`
		AlreadyListed      *int `json:"alreadyListed"`
	} `json:"optOuts"`
	CallbacksByStatus []struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	} `json:"callbacksByStatus"`
}

func (f *fixture) outcomeSummary(cookie, campaignID string) (outcomeSummaryView, reply) {
	f.t.Helper()
	r := f.do(call{method: http.MethodGet, path: "/campaigns/" + campaignID + "/outcome-summary", cookie: cookie})
	var v outcomeSummaryView
	if r.status == http.StatusOK {
		if err := json.Unmarshal(r.body, &v); err != nil {
			f.t.Fatalf("decode summary: %v: %s", err, r.body)
		}
	}
	return v, r
}

// listTotal is the total of a listing.
func (f *fixture) listTotal(cookie, path string) int {
	f.t.Helper()
	r := f.do(call{method: http.MethodGet, path: path, cookie: cookie})
	if r.status != http.StatusOK {
		f.t.Fatalf("%s: %d %s", path, r.status, r.body)
	}
	var page struct {
		Total *int `json:"total"`
	}
	if err := json.Unmarshal(r.body, &page); err != nil || page.Total == nil {
		f.t.Fatalf("%s: no total in %s (%v)", path, r.body, err)
	}
	return *page.Total
}

func TestACampaignsOutcomeSummaryOverHTTP(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	ctx := context.Background()
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil)
	cid := uuid.MustParse(c.ID)

	// A campaign with nothing in it already answers every value, at zero.
	empty, r := f.outcomeSummary(admin, c.ID)
	if r.status != http.StatusOK {
		t.Fatalf("summary of an empty campaign: %d %s", r.status, r.body)
	}
	checkSummaryShape(t, empty, c.ID)
	for _, row := range empty.AttemptsByOutcome {
		if row.Count != 0 {
			t.Errorf("an empty campaign has %d attempts %s", row.Count, row.Outcome)
		}
	}

	// Seven contacts in the fictional 555-01XX block.
	rows := make([]map[string]any, 7)
	for i := range rows {
		rows[i] = map[string]any{"number": fmt.Sprintf("+1201555%04d", 100+i)}
	}
	if r := f.post(admin, "/campaigns/"+c.ID+"/contacts", map[string]any{"contacts": rows}); r.status != http.StatusOK {
		t.Fatalf("import: %d %s", r.status, r.body)
	}
	if r := f.post(admin, "/campaigns/"+c.ID+"/start", nil); r.status != http.StatusOK {
		t.Fatalf("start: %d %s", r.status, r.body)
	}
	contacts, _, err := f.st.Campaigns().ListContacts(ctx, cid, campaign.ContactFilter{Limit: 10})
	if err != nil || len(contacts) != 7 {
		t.Fatalf("contacts: %d %v", len(contacts), err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	next := func() time.Time { at = at.Add(time.Minute); return at }
	finish := func(ct campaign.Contact, out campaign.Outcome, cause string) {
		t.Helper()
		when := next()
		a, err := f.st.Attempts().Begin(ctx, ct.ID, when)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.Attempts().Finish(ctx, a.CallID, campaign.Ending{Outcome: out, HangupCause: cause, EndedAt: when.Add(30 * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	// 0: rang out once, then a HIGH lead (its callback is filed).
	finish(contacts[0], campaign.OutcomeNoAnswer, "NO_ANSWER")
	high := answeredCampaignCall(t, f.st, contacts[0].ID, campaign.LeadHigh, "Send the renewal offer.", next())
	// 1: asked on the call not to be called again.
	answeredCampaignCall(t, f.st, contacts[1].ID, campaign.LeadDoNotCall, "Stop calling.", next())
	// 2: already on the list when it came up.
	if _, _, err := f.st.DNC().Add(ctx, contacts[2].Number, store.DNCSourceManual, nil, nil); err != nil {
		t.Fatal(err)
	}
	finish(contacts[2], campaign.OutcomeDNCSkipped, "NUMBER_ON_DNC")
	// 3: busy, waiting for a retry.
	finish(contacts[3], campaign.OutcomeBusy, "USER_BUSY")
	// 4: ringing now.
	if _, err := f.st.Attempts().Begin(ctx, contacts[4].ID, next()); err != nil {
		t.Fatal(err)
	}
	// 5: never tried. 6: a MEDIUM lead.
	answeredCampaignCall(t, f.st, contacts[6].ID, campaign.LeadMedium, "Later in the year.", next())

	got, r := f.outcomeSummary(admin, c.ID)
	if r.status != http.StatusOK {
		t.Fatalf("summary: %d %s", r.status, r.body)
	}
	checkSummaryShape(t, got, c.ID)
	wantAttempts := map[string]int{"NO_ANSWER": 1, "ANSWERED": 3, "DNC_SKIPPED": 1, "BUSY": 1}
	wantLast := map[string]int{"ANSWERED": 3, "DNC_SKIPPED": 1, "BUSY": 1}
	wantLead := map[string]int{"HIGH": 1, "DO_NOT_CALL": 1, "MEDIUM": 1}
	base := "/campaigns/" + c.ID
	for _, row := range got.AttemptsByOutcome {
		listed := f.listTotal(admin, base+"/attempts?limit=1&outcome="+row.Outcome)
		if row.Count != wantAttempts[row.Outcome] || row.Count != listed {
			t.Errorf("attempts %s: %d, want %d, listed %d", row.Outcome, row.Count, wantAttempts[row.Outcome], listed)
		}
	}
	for _, row := range got.ContactsByLastOutcome {
		listed := f.listTotal(admin, base+"/contacts?limit=1&lastOutcome="+row.Outcome)
		if row.Count != wantLast[row.Outcome] || row.Count != listed {
			t.Errorf("contacts last %s: %d, want %d, listed %d", row.Outcome, row.Count, wantLast[row.Outcome], listed)
		}
	}
	for _, row := range got.ContactsByLeadIntent {
		listed := f.listTotal(admin, base+"/contacts?limit=1&leadIntent="+row.LeadIntent)
		if row.Count != wantLead[row.LeadIntent] || row.Count != listed {
			t.Errorf("contacts lead %s: %d, want %d, listed %d", row.LeadIntent, row.Count, wantLead[row.LeadIntent], listed)
		}
	}
	said := f.listTotal(admin, base+"/contacts?limit=1&status=DNC&leadIntent=DO_NOT_CALL")
	listed := f.listTotal(admin, base+"/contacts?limit=1&status=DNC&lastOutcome=DNC_SKIPPED")
	if *got.OptOuts.SaidInConversation != 1 || *got.OptOuts.AlreadyListed != 1 || said != 1 || listed != 1 {
		t.Errorf("opt-outs = %d said, %d listed; the listings say %d and %d",
			*got.OptOuts.SaidInConversation, *got.OptOuts.AlreadyListed, said, listed)
	}
	for _, row := range got.CallbacksByStatus {
		listed := f.listTotal(admin, "/callbacks?callId="+high.CallID.String()+"&status="+row.Status)
		want := 0
		if row.Status == "OPEN" {
			want = 1
		}
		if row.Count != want || row.Count != listed {
			t.Errorf("callbacks %s: %d, want %d, listed %d", row.Status, row.Count, want, listed)
		}
	}

	// The contact listing's new filter is validated like the others.
	r = f.do(call{method: http.MethodGet, path: base + "/contacts?lastOutcome=MAYBE", cookie: admin})
	wantReply(t, "unknown last outcome", r, http.StatusUnprocessableEntity, "VALIDATION_FAILED")
	if _, r := f.outcomeSummary(admin, uuid.NewString()); r.status != http.StatusNotFound || r.code != "NOT_FOUND" {
		t.Errorf("unknown campaign: %d %s", r.status, r.body)
	}
	r = f.do(call{method: http.MethodGet, path: "/campaigns/not-a-uuid/outcome-summary", cookie: admin})
	if r.status != http.StatusBadRequest {
		t.Errorf("a malformed id: %d %s", r.status, r.body)
	}
}

// checkSummaryShape holds a response to the contract: every outcome, intent
// and callback status once, in the enum's order, and both opt-out counts.
func checkSummaryShape(t *testing.T, v outcomeSummaryView, campaignID string) {
	t.Helper()
	if v.CampaignID != campaignID {
		t.Errorf("campaignId = %q, want %q", v.CampaignID, campaignID)
	}
	outcomes := campaign.AllOutcomes()
	for name, list := range map[string][]string{
		"attemptsByOutcome":     outcomesOf(v.AttemptsByOutcome),
		"contactsByLastOutcome": outcomesOf(v.ContactsByLastOutcome),
	} {
		if len(list) != len(outcomes) {
			t.Errorf("%s has %d rows, want %d", name, len(list), len(outcomes))
			continue
		}
		for i, o := range outcomes {
			if list[i] != string(o) {
				t.Errorf("%s[%d] = %s, want %s", name, i, list[i], o)
			}
		}
	}
	intents := campaign.AllLeadIntents()
	if len(v.ContactsByLeadIntent) != len(intents) {
		t.Errorf("contactsByLeadIntent has %d rows", len(v.ContactsByLeadIntent))
	} else {
		for i, l := range intents {
			if v.ContactsByLeadIntent[i].LeadIntent != string(l) {
				t.Errorf("contactsByLeadIntent[%d] = %s, want %s", i, v.ContactsByLeadIntent[i].LeadIntent, l)
			}
		}
	}
	statuses := []string{"OPEN", "CLAIMED", "DONE", "DISMISSED"}
	if len(v.CallbacksByStatus) != len(statuses) {
		t.Errorf("callbacksByStatus has %d rows", len(v.CallbacksByStatus))
	} else {
		for i, s := range statuses {
			if v.CallbacksByStatus[i].Status != s {
				t.Errorf("callbacksByStatus[%d] = %s, want %s", i, v.CallbacksByStatus[i].Status, s)
			}
		}
	}
	if v.OptOuts.SaidInConversation == nil || v.OptOuts.AlreadyListed == nil {
		t.Errorf("optOuts is missing a count: %+v", v.OptOuts)
	}
}

func outcomesOf(rows []outcomeRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Outcome
	}
	return out
}

// The summary is a campaigns:read operation like any other read of a
// campaign: a person's session and a key reach it alike.
func TestTheOutcomeSummaryNeedsCampaignsRead(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	supervisor := f.seedSupervisor("sup", 1100)
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil)
	path := "/campaigns/" + c.ID + "/outcome-summary"

	_, readKey := f.issueKey(admin, "campaigns:read")
	_, writeKey := f.issueKey(admin, "campaigns:write")
	_, otherKey := f.issueKey(admin, "history:read:all", "calls:read:own")

	for who, bearer := range map[string]string{"campaigns:read key": readKey} {
		if r := f.do(call{method: http.MethodGet, path: path, bearer: bearer}); r.status != http.StatusOK {
			t.Errorf("%s: %d %s", who, r.status, r.body)
		}
	}
	for who, cookie := range map[string]string{"supervisor": supervisor, "admin": admin} {
		if r := f.do(call{method: http.MethodGet, path: path, cookie: cookie}); r.status != http.StatusOK {
			t.Errorf("%s: %d %s", who, r.status, r.body)
		}
	}
	for who, bearer := range map[string]string{"campaigns:write key": writeKey, "unrelated key": otherKey} {
		wantReply(t, who, f.do(call{method: http.MethodGet, path: path, bearer: bearer}), http.StatusForbidden, "INSUFFICIENT_SCOPE")
	}
	wantReply(t, "agent", f.do(call{method: http.MethodGet, path: path, cookie: f.agent.cookie}), http.StatusForbidden, "INSUFFICIENT_SCOPE")
	if r := f.do(call{method: http.MethodGet, path: path}); r.status != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", r.status)
	}
}
