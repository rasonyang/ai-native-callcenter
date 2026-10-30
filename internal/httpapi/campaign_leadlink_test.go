// SPDX-License-Identifier: Apache-2.0

package httpapi

// A HIGH lead links to its callback by call id (design 10 section 14, #188):
// the contact names its latest call, and listCallbacks filters by call. No
// phone number is compared anywhere, so one number with callbacks from
// several calls resolves each lead to its own.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type leadLinkContact struct {
	ID            string  `json:"id"`
	Number        string  `json:"number"`
	LeadIntent    *string `json:"leadIntent"`
	LastCallID    *string `json:"lastCallId"`
	LastAttemptID *string `json:"lastAttemptId"`
}

type leadLinkCallback struct {
	ID          string  `json:"id"`
	CallID      *string `json:"callId"`
	PhoneNumber string  `json:"phoneNumber"`
	Message     string  `json:"message"`
}

// answeredCampaignCall runs one attempt of contact to its end: the call
// records lead (none when empty) and its CDR lands answered.
func answeredCampaignCall(t *testing.T, st *store.Store, contactID uuid.UUID, lead campaign.LeadIntent, notes string, at time.Time) campaign.Attempt {
	t.Helper()
	ctx := context.Background()
	a, err := st.Attempts().Begin(ctx, contactID, at)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if lead != "" {
		if ok, err := st.Attempts().SetLead(ctx, a.CallID, lead, notes); err != nil || !ok {
			t.Fatalf("set lead: %v %v", ok, err)
		}
	}
	if err := st.Ledger().InsertCDR(ctx, store.CDR{
		CallID: a.CallID, StartedAt: at, AnsweredAt: at.Add(5 * time.Second), EndedAt: at.Add(time.Minute),
		CallType: "OUTBOUND", FromNumber: "95099", ToNumber: a.Number, DID: "95099",
		Status: store.CDRStatusAnswered, HangupCause: "NORMAL_CLEARING", BotSec: 55,
		Legs: []store.Leg{{Kind: "TRUNK", Label: "customer"}},
	}); err != nil {
		t.Fatalf("cdr: %v", err)
	}
	return a
}

func TestAHighLeadLinksToItsCallbackByCallID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.seedAdmin()
	supervisor := f.seedSupervisor("sup", 1100)
	didID, _ := campaignDID(t, f.st, "95099", false)
	_, linkKey := f.issueKey(admin, "campaigns:read", "calls:read:own")

	// Two campaigns share +8613800138000; the first also has a contact that
	// is never dialled and one whose call records no lead.
	first := f.createCampaign(admin, didID, nil)
	second := f.createCampaign(admin, didID, func(b map[string]any) { b["name"] = "Second wave" })
	const shared = "+8613800138000"
	for id, numbers := range map[string][]string{
		first.ID:  {shared, "+86 138 0013 8001", "008613800138002"},
		second.ID: {"13800138000"},
	} {
		rows := make([]map[string]any, 0, len(numbers))
		for _, n := range numbers {
			rows = append(rows, map[string]any{"number": n})
		}
		if r := f.post(admin, "/campaigns/"+id+"/contacts", map[string]any{"contacts": rows}); r.status != 200 {
			t.Fatalf("import: %d %s", r.status, r.body)
		}
		if r := f.post(admin, "/campaigns/"+id+"/start", nil); r.status != 200 {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
	}

	contacts := func(t *testing.T, campaignID, cookie, bearer string) map[string]leadLinkContact {
		t.Helper()
		r := f.do(call{method: http.MethodGet, path: "/campaigns/" + campaignID + "/contacts", cookie: cookie, bearer: bearer})
		if r.status != 200 {
			t.Fatalf("list contacts: %d %s", r.status, r.body)
		}
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(r.body, &page); err != nil {
			t.Fatal(err)
		}
		out := map[string]leadLinkContact{}
		for _, raw := range page.Items {
			// The contract declares both required and nullable: present as
			// null, never left out.
			var keys map[string]json.RawMessage
			_ = json.Unmarshal(raw, &keys)
			for _, k := range []string{"lastCallId", "lastAttemptId"} {
				if _, ok := keys[k]; !ok {
					t.Errorf("a contact came back without %s: %s", k, raw)
				}
			}
			var c leadLinkContact
			_ = json.Unmarshal(raw, &c)
			out[c.Number] = c
		}
		return out
	}

	before := contacts(t, first.ID, admin, "")
	for n, c := range before {
		if c.LastCallID != nil || c.LastAttemptID != nil {
			t.Errorf("%s names a call before its first attempt: %+v", n, c)
		}
	}

	// An older, unrelated callback for the shared number, from an inbound call.
	inboundCall := uuid.New()
	if _, err := f.st.Ledger().InsertCallback(ctx, &inboundCall, nil, "13800138000", "inbound: billing question"); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).UTC()
	callA := answeredCampaignCall(t, f.st, uuid.MustParse(before[shared].ID), campaign.LeadHigh, "first wave: wants the offer", at)
	callNoLead := answeredCampaignCall(t, f.st, uuid.MustParse(before["+8613800138001"].ID), "", "", at)
	secondContacts := contacts(t, second.ID, admin, "")
	callB := answeredCampaignCall(t, f.st, uuid.MustParse(secondContacts[shared].ID), campaign.LeadHigh, "second wave: call after 6", at.Add(10*time.Minute))

	callbacks := func(t *testing.T, query, cookie, bearer string) []leadLinkCallback {
		t.Helper()
		r := f.do(call{method: http.MethodGet, path: "/callbacks" + query, cookie: cookie, bearer: bearer})
		if r.status != 200 {
			t.Fatalf("callbacks%s: %d %s", query, r.status, r.body)
		}
		var page struct {
			Items []leadLinkCallback `json:"items"`
		}
		if err := json.Unmarshal(r.body, &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	if all := callbacks(t, "", admin, ""); len(all) != 3 {
		t.Fatalf("the queue holds %d callbacks, want 3 (one inbound, two HIGH leads)", len(all))
	}

	// Each credential that runs a campaign follows the link: the
	// administrator, a supervisor's session, and a key scoped to exactly
	// campaigns:read plus calls:read:own.
	creds := []struct {
		who            string
		cookie, bearer string
	}{{"admin", admin, ""}, {"supervisor", supervisor, ""}, {"scoped key", "", linkKey}}
	for _, cr := range creds {
		t.Run(cr.who, func(t *testing.T) {
			for _, w := range []struct {
				campaignID string
				call       campaign.Attempt
				message    string
			}{
				{first.ID, callA, "first wave: wants the offer"},
				{second.ID, callB, "second wave: call after 6"},
			} {
				lead := contacts(t, w.campaignID, cr.cookie, cr.bearer)[shared]
				if lead.LeadIntent == nil || *lead.LeadIntent != "HIGH" ||
					lead.LastCallID == nil || *lead.LastCallID != w.call.CallID.String() ||
					lead.LastAttemptID == nil || *lead.LastAttemptID != w.call.ID.String() {
					t.Fatalf("lead = %+v, want HIGH on call %s", lead, w.call.CallID)
				}
				got := callbacks(t, "?callId="+*lead.LastCallID, cr.cookie, cr.bearer)
				if len(got) != 1 || got[0].CallID == nil || *got[0].CallID != *lead.LastCallID || got[0].Message != w.message {
					t.Errorf("callbacks of %s = %+v, want exactly its own", *lead.LastCallID, got)
				}
			}
			// A call that filed nothing, and a call nobody placed: empty.
			if got := callbacks(t, "?callId="+callNoLead.CallID.String(), cr.cookie, cr.bearer); len(got) != 0 {
				t.Errorf("a call with no lead resolves to %+v", got)
			}
			if got := callbacks(t, "?callId="+uuid.NewString(), cr.cookie, cr.bearer); len(got) != 0 {
				t.Errorf("an unknown call resolves to %+v", got)
			}
		})
	}

	noLead := contacts(t, first.ID, admin, "")["+8613800138001"]
	if noLead.LeadIntent != nil || noLead.LastCallID == nil || *noLead.LastCallID != callNoLead.CallID.String() {
		t.Errorf("a call that recorded no lead: %+v", noLead)
	}
	if never := contacts(t, first.ID, admin, "")["+8613800138002"]; never.LastCallID != nil {
		t.Errorf("a contact never dialled names %v", *never.LastCallID)
	}
	if got := callbacks(t, "?callId="+inboundCall.String()+"&status=OPEN", admin, ""); len(got) != 1 || got[0].Message != "inbound: billing question" {
		t.Errorf("the inbound callback by its call = %+v", got)
	}
	r := f.do(call{method: http.MethodGet, path: "/callbacks?callId=not-a-uuid", cookie: admin})
	if r.status != http.StatusBadRequest {
		t.Errorf("a malformed callId: %d %s", r.status, r.body)
	}
}
