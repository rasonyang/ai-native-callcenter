// SPDX-License-Identifier: Apache-2.0

package httpapi

// Campaigns at the HTTP boundary, on a real PostgreSQL and the real routing
// tree: the contract's codes, the state machine under concurrent requests, the
// scopes, and the import of a messy customer list.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// The slots the flow reads are the ones declaredSlots names.
const campaignFlowJSON = `{
  "id": %q, "specVersion": "v2", "initialNode": "ask",
  "global": {"persona": "A polite caller."},
  "nodes": {"ask": {
    "instruction": "Hello {slots.userData.name}, about your {slots.userData.plan} plan in {slots.userData.city}.",
    "tools": %s}}
}`

func campaignFlowSpec(slug string, transfer bool) []byte {
	tools := `["hangup"]`
	if transfer {
		tools = `["hangup", "transfer_to_agent"]`
	}
	return []byte(fmt.Sprintf(campaignFlowJSON, slug, tools))
}

// campaignDID creates a published flow and a DID answered by it.
func campaignDID(t *testing.T, st *store.Store, number string, transfer bool) (didID, flowID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	flowID, err := st.Flows().Create(ctx, "flow-"+number, "Flow "+number, campaignFlowSpec("flow-"+number, transfer))
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	if err := st.Flows().Publish(ctx, flowID, "test"); err != nil {
		t.Fatalf("publish flow: %v", err)
	}
	did, err := st.Catalog().CreateDID(ctx, catalog.DID{
		ID: uuid.Must(uuid.NewV7()), Number: number, Language: "en", FlowID: &flowID,
		IsEnabled: true, AllowOutbound: true,
	})
	if err != nil {
		t.Fatalf("create did: %v", err)
	}
	return did.ID, flowID
}

func (f *fixture) seedSupervisor(username string, poolLow int) string {
	f.t.Helper()
	hash, err := auth.HashPassword(seedPassword)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.st.Accounts().Provision(context.Background(), store.NewAccount{
		Username: username, PasswordHash: hash, DisplayName: username,
		Role: string(auth.RoleSupervisor), CallcenterName: "sup-" + username,
		SIPPassword: "sip-" + username, ExtensionLow: poolLow, ExtensionHigh: poolLow + 9,
	}); err != nil {
		f.t.Fatalf("provision supervisor: %v", err)
	}
	return f.signIn(username)
}

// campaignView is the part of a campaign these tests read back.
type campaignView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	DIDID         string  `json:"didId"`
	DIDNumber     string  `json:"didNumber"`
	Status        string  `json:"status"`
	PauseReason   *string `json:"pauseReason"`
	TimeZone      string  `json:"timeZone"`
	MaxConcurrent int     `json:"maxConcurrent"`
	RingTimeout   int     `json:"ringTimeoutSec"`
	CallingWindow struct {
		Days   []string `json:"days"`
		Ranges []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"ranges"`
	} `json:"callingWindow"`
	RetryPolicy struct {
		MaxAttempts      int            `json:"maxAttempts"`
		MaxSystemRetries int            `json:"maxSystemRetries"`
		RetryOutcomes    []string       `json:"retryOutcomes"`
		IntervalSec      map[string]int `json:"intervalSec"`
		TransientBackoff struct {
			BaseSec int `json:"baseSec"`
			Factor  int `json:"factor"`
			CapSec  int `json:"capSec"`
		} `json:"transientBackoff"`
	} `json:"retryPolicy"`
	StartedAt  *string `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`
	Contacts   struct {
		Total, Active, Done, Exhausted, InvalidNumber, DNC int
	} `json:"contacts"`
}

func decodeCampaign(t *testing.T, r reply) campaignView {
	t.Helper()
	var v campaignView
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode campaign: %v: %s", err, r.body)
	}
	return v
}

func weekdayWindow() map[string]any {
	return map[string]any{
		"days":   []string{"MON", "TUE", "WED", "THU", "FRI"},
		"ranges": []map[string]string{{"start": "09:00", "end": "12:00"}, {"start": "14:00", "end": "18:00"}},
	}
}

func campaignBody(didID uuid.UUID, mutate func(map[string]any)) map[string]any {
	b := map[string]any{
		"name": "Autumn follow-up", "didId": didID.String(), "timeZone": "Asia/Shanghai",
		"callingWindow": weekdayWindow(),
	}
	if mutate != nil {
		mutate(b)
	}
	return b
}

func (f *fixture) createCampaign(cookie string, didID uuid.UUID, mutate func(map[string]any)) campaignView {
	f.t.Helper()
	r := f.do(call{method: http.MethodPost, path: "/campaigns", cookie: cookie, body: campaignBody(didID, mutate)})
	if r.status != http.StatusCreated {
		f.t.Fatalf("create campaign: %d %s", r.status, r.body)
	}
	return decodeCampaign(f.t, r)
}

func (f *fixture) post(cookie, path string, body any) reply {
	f.t.Helper()
	return f.do(call{method: http.MethodPost, path: path, cookie: cookie, body: body})
}

type errView struct {
	Error struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	} `json:"error"`
}

func errOf(t *testing.T, r reply) errView {
	t.Helper()
	var e errView
	_ = json.Unmarshal(r.body, &e)
	return e
}

func wantReply(t *testing.T, what string, r reply, status int, code string) {
	t.Helper()
	if r.status != status || r.code != code {
		t.Errorf("%s: %d %q, want %d %q; body %.300s", what, r.status, r.code, status, code, r.body)
	}
}

func TestACampaignRunsItsLifecycleThroughTheAPI(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)

	c := f.createCampaign(admin, didID, nil)
	if c.Status != "DRAFT" || c.DIDNumber != "95099" || c.PauseReason != nil || c.StartedAt != nil ||
		c.MaxConcurrent != 5 || c.RingTimeout != 30 {
		t.Errorf("created = %+v", c)
	}
	rp := c.RetryPolicy
	if rp.MaxAttempts != 3 || rp.MaxSystemRetries != 5 || len(rp.RetryOutcomes) != 6 ||
		rp.IntervalSec["NO_ANSWER"] != 7200 || rp.IntervalSec["BUSY"] != 1800 ||
		rp.IntervalSec["REJECTED"] != 86400 || rp.IntervalSec["UNKNOWN"] != 86400 ||
		rp.TransientBackoff.BaseSec != 300 || rp.TransientBackoff.Factor != 3 || rp.TransientBackoff.CapSec != 14400 {
		t.Errorf("default retry policy = %+v", rp)
	}

	got := f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin})
	if got.status != http.StatusOK || decodeCampaign(t, got).Name != "Autumn follow-up" {
		t.Errorf("get: %d %s", got.status, got.body)
	}

	// Listing, filtered by status.
	second := f.createCampaign(admin, didID, func(b map[string]any) { b["name"] = "Second" })
	f.post(admin, "/campaigns/"+second.ID+"/start", nil)
	var list struct {
		Items []campaignView `json:"items"`
		Total int            `json:"total"`
	}
	r := f.do(call{method: http.MethodGet, path: "/campaigns", cookie: admin})
	_ = json.Unmarshal(r.body, &list)
	if list.Total != 2 || len(list.Items) != 2 || list.Items[0].Name != "Second" {
		t.Errorf("list = %d items, total %d, first %q — newest first", len(list.Items), list.Total, list.Items[0].Name)
	}
	r = f.do(call{method: http.MethodGet, path: "/campaigns?status=RUNNING", cookie: admin})
	list.Items = nil
	_ = json.Unmarshal(r.body, &list)
	if list.Total != 1 || list.Items[0].ID != second.ID {
		t.Errorf("status filter: %+v", list)
	}
	r = f.do(call{method: http.MethodGet, path: "/campaigns?status=BOGUS", cookie: admin})
	wantReply(t, "unknown status filter", r, http.StatusUnprocessableEntity, "VALIDATION_FAILED")
	r = f.do(call{method: http.MethodGet, path: "/campaigns?limit=1&offset=1", cookie: admin})
	list.Items = nil
	_ = json.Unmarshal(r.body, &list)
	if len(list.Items) != 1 || list.Total != 2 || list.Items[0].ID != c.ID {
		t.Errorf("paging: %d items total %d", len(list.Items), list.Total)
	}

	// The full path of one campaign.
	for _, step := range []struct {
		action, status, reason string
	}{
		{"start", "RUNNING", ""}, {"pause", "PAUSED", "MANUAL"}, {"resume", "RUNNING", ""}, {"cancel", "CANCELLED", ""},
	} {
		r := f.post(admin, "/campaigns/"+c.ID+"/"+step.action, nil)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", step.action, r.status, r.body)
		}
		v := decodeCampaign(t, r)
		reason := ""
		if v.PauseReason != nil {
			reason = *v.PauseReason
		}
		if v.Status != step.status || reason != step.reason {
			t.Errorf("after %s: %s/%q, want %s/%q", step.action, v.Status, reason, step.status, step.reason)
		}
		if step.action == "start" && v.StartedAt == nil {
			t.Error("a started campaign has no startedAt")
		}
		if step.action == "cancel" && v.FinishedAt == nil {
			t.Error("a cancelled campaign has no finishedAt")
		}
	}
}

func TestCampaignWritesAreValidatedWithTheContractsCodes(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	transferDID, _ := campaignDID(t, f.st, "95001", true)

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		status int
		code   string
		field  string
	}{
		{"a flow that can transfer", func(b map[string]any) { b["didId"] = transferDID.String() },
			422, "CAMPAIGN_FLOW_HAS_TRANSFER", "didId"},
		{"ring timeout at the answer window", func(b map[string]any) { b["ringTimeoutSec"] = 60 },
			422, "CAMPAIGN_RING_TIMEOUT_TOO_LONG", "ringTimeoutSec"},
		{"ring timeout over it", func(b map[string]any) { b["ringTimeoutSec"] = 300 },
			422, "CAMPAIGN_RING_TIMEOUT_TOO_LONG", "ringTimeoutSec"},
		{"ring timeout under five", func(b map[string]any) { b["ringTimeoutSec"] = 4 },
			422, "VALIDATION_FAILED", "ringTimeoutSec"},
		{"a window outside the permitted hours", func(b map[string]any) {
			b["callingWindow"] = map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "07:00", "end": "12:00"}}}
		}, 422, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT", "callingWindow.ranges[0]"},
		{"a window across midnight under daytime hours", func(b map[string]any) {
			b["callingWindow"] = map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "22:00", "end": "02:00"}}}
		}, 422, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT", "callingWindow.ranges[0]"},
		{"a window ending after the hours", func(b map[string]any) {
			b["callingWindow"] = map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "20:00", "end": "21:01"}}}
		}, 422, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT", "callingWindow.ranges[0]"},
		{"overlapping ranges", func(b map[string]any) {
			b["callingWindow"] = map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{
				{"start": "09:00", "end": "12:00"}, {"start": "11:00", "end": "13:00"}}}
		}, 422, "VALIDATION_FAILED", "callingWindow.ranges"},
		{"no days", func(b map[string]any) {
			b["callingWindow"] = map[string]any{"days": []string{}, "ranges": []map[string]string{{"start": "09:00", "end": "12:00"}}}
		}, 422, "VALIDATION_FAILED", "callingWindow.days"},
		{"a time zone that does not exist", func(b map[string]any) { b["timeZone"] = "Mars/Olympus" },
			422, "VALIDATION_FAILED", "timeZone"},
		{"max concurrent zero", func(b map[string]any) { b["maxConcurrent"] = 0 }, 422, "VALIDATION_FAILED", "maxConcurrent"},
		{"an unknown DID", func(b map[string]any) { b["didId"] = uuid.NewString() }, 422, "VALIDATION_FAILED", "didId"},
		{"an unknown follow-up queue", func(b map[string]any) { b["followUpQueueId"] = uuid.NewString() },
			422, "VALIDATION_FAILED", "followUpQueueId"},
		{"an empty name", func(b map[string]any) { b["name"] = " " }, 422, "VALIDATION_FAILED", "name"},
		{"an answered outcome cannot be retried", func(b map[string]any) {
			b["retryPolicy"] = map[string]any{"retryOutcomes": []string{"ANSWERED"}}
		}, 422, "VALIDATION_FAILED", "retryPolicy.retryOutcomes"},
		{"zero attempts", func(b map[string]any) { b["retryPolicy"] = map[string]any{"maxAttempts": 0} },
			422, "VALIDATION_FAILED", "retryPolicy.maxAttempts"},
		{"a field the contract does not have", func(b map[string]any) { b["status"] = "RUNNING" }, 400, "VALIDATION_FAILED", ""},
		{"a string where a number goes", func(b map[string]any) { b["maxConcurrent"] = "five" }, 400, "VALIDATION_FAILED", ""},
		{"no calling window", func(b map[string]any) { delete(b, "callingWindow") }, 422, "VALIDATION_FAILED", "callingWindow.days"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.post(admin, "/campaigns", campaignBody(didID, tc.mutate))
			wantReply(t, tc.name, r, tc.status, tc.code)
			if tc.field != "" {
				if got := errOf(t, r).Error.Params["field"]; got != tc.field {
					t.Errorf("params.field = %v, want %s", got, tc.field)
				}
			}
		})
	}
	r := f.do(call{method: http.MethodGet, path: "/campaigns", cookie: admin})
	if !strings.Contains(string(r.body), `"total":0`) {
		t.Errorf("a refused create stored a campaign: %s", r.body)
	}

	// Accepted: the edges of the same rules, and a daylight-saving zone.
	for name, mutate := range map[string]func(map[string]any){
		"ring timeout 59": func(b map[string]any) { b["ringTimeoutSec"] = 59 },
		"ring timeout 5":  func(b map[string]any) { b["ringTimeoutSec"] = 5 },
		"a DST time zone": func(b map[string]any) { b["timeZone"] = "America/New_York" },
		"the whole hours": func(b map[string]any) {
			b["callingWindow"].(map[string]any)["ranges"] = []map[string]string{{"start": "08:00", "end": "21:00"}}
		},
		"a country code":    func(b map[string]any) { b["defaultCountryCode"] = "1" },
		"no retries at all": func(b map[string]any) { b["retryPolicy"] = map[string]any{"retryOutcomes": []string{}} },
	} {
		r := f.post(admin, "/campaigns", campaignBody(didID, mutate))
		if r.status != http.StatusCreated {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
}

// A deployment that calls in the evening and at night can have a window across
// midnight, which belongs to the day it starts on.
func TestAWindowAcrossMidnightIsAllowedWhereTheOuterHoursAllowIt(t *testing.T) {
	night, _ := dayrange.Parse("20:00-04:00")
	f := newFixtureWithStore(t, func(st *store.Store, d *Deps) {
		d.Campaigns = campaign.NewService(st.Campaigns(), nil, campaign.Env{OutboundHours: night})
	})
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	across := map[string]any{"days": []string{"FRI", "SAT"}, "ranges": []map[string]string{{"start": "22:00", "end": "02:00"}}}

	c := f.createCampaign(admin, didID, func(b map[string]any) { b["callingWindow"] = across })
	if len(c.CallingWindow.Ranges) != 1 || c.CallingWindow.Ranges[0].End != "02:00" {
		t.Errorf("window = %+v", c.CallingWindow)
	}

	patch := func(w map[string]any) reply {
		return f.do(call{method: http.MethodPatch, path: "/campaigns/" + c.ID, cookie: admin, body: map[string]any{"callingWindow": w}})
	}
	// Outside on PATCH is refused with the same code as on create.
	wantReply(t, "daytime", patch(map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "09:00", "end": "12:00"}}}),
		422, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT")
	wantReply(t, "starts before the hours", patch(map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "19:00", "end": "23:00"}}}),
		422, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT")
	wantReply(t, "ends after the hours", patch(map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "23:00", "end": "05:00"}}}),
		422, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT")
	wantReply(t, "inside", patch(map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "01:00", "end": "03:00"}}}),
		200, "")

	// Narrowing a running campaign's window is allowed and held to the hours.
	f.post(admin, "/campaigns/"+c.ID+"/start", nil)
	wantReply(t, "narrowing while running", patch(map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "01:30", "end": "02:30"}}}),
		200, "")
	wantReply(t, "widening past the hours while running", patch(map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "01:30", "end": "09:00"}}}),
		422, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT")
}

func TestPatchingThroughTheAPI(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	otherDID, _ := campaignDID(t, f.st, "95100", false)
	transferDID, _ := campaignDID(t, f.st, "95001", true)
	c := f.createCampaign(admin, didID, func(b map[string]any) { b["defaultCountryCode"] = "86" })
	url := "/campaigns/" + c.ID
	patch := func(body map[string]any) reply {
		return f.do(call{method: http.MethodPatch, path: url, cookie: admin, body: body})
	}

	// DRAFT: everything.
	r := patch(map[string]any{"name": "Renamed", "didId": otherDID.String(), "timeZone": "America/New_York",
		"maxConcurrent": 9, "ringTimeoutSec": 20, "retryPolicy": map[string]any{"maxAttempts": 5}})
	if r.status != http.StatusOK {
		t.Fatalf("patch DRAFT: %d %s", r.status, r.body)
	}
	v := decodeCampaign(t, r)
	if v.Name != "Renamed" || v.DIDNumber != "95100" || v.TimeZone != "America/New_York" || v.MaxConcurrent != 9 ||
		v.RingTimeout != 20 || v.RetryPolicy.MaxAttempts != 5 {
		t.Errorf("patched = %+v", v)
	}
	// The retry policy was replaced as a whole: what the patch left out is the default.
	if v.RetryPolicy.MaxSystemRetries != 5 || v.RetryPolicy.IntervalSec["BUSY"] != 1800 {
		t.Errorf("retry policy not whole: %+v", v.RetryPolicy)
	}

	// Validation applies on PATCH as on create.
	wantReply(t, "to a transferring flow", patch(map[string]any{"didId": transferDID.String()}), 422, "CAMPAIGN_FLOW_HAS_TRANSFER")
	wantReply(t, "ring timeout 60", patch(map[string]any{"ringTimeoutSec": 60}), 422, "CAMPAIGN_RING_TIMEOUT_TOO_LONG")
	wantReply(t, "null name", patch(map[string]any{"name": nil}), 422, "VALIDATION_FAILED")
	wantReply(t, "unknown field", patch(map[string]any{"status": "RUNNING"}), 400, "VALIDATION_FAILED")
	wantReply(t, "nothing named", patch(map[string]any{}), 200, "")

	// null clears the two optional fields, absent leaves them.
	queueID := uuid.Must(uuid.NewV7())
	if _, err := f.st.Pool.Exec(context.Background(),
		`INSERT INTO queues (id, name, ext_number) VALUES ($1, 'follow-up', '7001')`, queueID); err != nil {
		t.Fatal(err)
	}
	r = patch(map[string]any{"followUpQueueId": queueID.String()})
	if !strings.Contains(string(r.body), queueID.String()) {
		t.Errorf("follow-up queue not set: %s", r.body)
	}
	r = patch(map[string]any{"name": "Again"})
	if !strings.Contains(string(r.body), queueID.String()) || !strings.Contains(string(r.body), `"defaultCountryCode":"86"`) {
		t.Errorf("an absent field was cleared: %s", r.body)
	}
	r = patch(map[string]any{"followUpQueueId": nil, "defaultCountryCode": nil})
	if strings.Contains(string(r.body), "followUpQueueId") || strings.Contains(string(r.body), "defaultCountryCode") {
		t.Errorf("null did not clear: %s", r.body)
	}

	// RUNNING: three fields open, the rest locked, and the lock names them.
	f.post(admin, url+"/start", nil)
	r = patch(map[string]any{"maxConcurrent": 3, "retryPolicy": map[string]any{"maxAttempts": 2},
		"callingWindow": map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": "10:00", "end": "11:00"}}}})
	if r.status != http.StatusOK || decodeCampaign(t, r).MaxConcurrent != 3 {
		t.Errorf("patch the open fields of a RUNNING campaign: %d %s", r.status, r.body)
	}
	r = patch(map[string]any{"name": "Locked", "timeZone": "UTC", "maxConcurrent": 7})
	wantReply(t, "locked fields", r, 409, "CAMPAIGN_FIELD_LOCKED")
	fields, _ := errOf(t, r).Error.Params["fields"].([]any)
	var names []string
	for _, x := range fields {
		names = append(names, fmt.Sprint(x))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"name", "timeZone"}) {
		t.Errorf("params.fields = %v, want [name timeZone]", names)
	}
	// Nothing in the refused patch was applied, not even the open field.
	got := decodeCampaign(t, f.do(call{method: http.MethodGet, path: url, cookie: admin}))
	if got.MaxConcurrent != 3 || got.Name != "Again" {
		t.Errorf("a refused patch changed the campaign: %+v", got)
	}
	// Sending a locked field with the value it has is not a change.
	wantReply(t, "same value", patch(map[string]any{"name": "Again", "timeZone": "America/New_York", "maxConcurrent": 4}), 200, "")

	// Terminal: nothing, and an empty patch is still fine.
	f.post(admin, url+"/cancel", nil)
	wantReply(t, "cancelled", patch(map[string]any{"maxConcurrent": 8}), 409, "CAMPAIGN_FIELD_LOCKED")
	wantReply(t, "cancelled, nothing named", patch(map[string]any{}), 200, "")
	wantReply(t, "unknown campaign", f.do(call{method: http.MethodPatch, path: "/campaigns/" + uuid.NewString(), cookie: admin,
		body: map[string]any{"name": "x"}}), 404, "NOT_FOUND")
}

// Every action from every status, through the API.
func TestEveryCampaignActionFromEveryStatusThroughTheAPI(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)

	// reach puts a fresh campaign in a status.
	reach := func(status string) campaignView {
		c := f.createCampaign(admin, didID, nil)
		id := c.ID
		move := func(action string) {
			if r := f.post(admin, "/campaigns/"+id+"/"+action, nil); r.status != http.StatusOK {
				t.Fatalf("reaching %s: %s: %d %s", status, action, r.status, r.body)
			}
		}
		switch status {
		case "RUNNING":
			move("start")
		case "PAUSED":
			move("start")
			move("pause")
		case "CANCELLED":
			move("cancel")
		case "COMPLETED":
			// Only the dialer completes a campaign.
			if _, err := f.st.Pool.Exec(context.Background(),
				`UPDATE campaigns SET status = 'COMPLETED', finished_at = now() WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		return decodeCampaign(t, f.do(call{method: http.MethodGet, path: "/campaigns/" + id, cookie: admin}))
	}

	type action struct {
		name string
		do   func(id string) reply
		// allowed: status -> expected HTTP status; others are refused with
		// refusedStatus/refusedCode.
		allowed       map[string]int
		refusedStatus int
		refusedCode   string
	}
	all := []string{"DRAFT", "RUNNING", "PAUSED", "COMPLETED", "CANCELLED"}
	nonTerminal := map[string]int{"DRAFT": 200, "RUNNING": 200, "PAUSED": 200}
	actions := []action{
		{"start", func(id string) reply { return f.post(admin, "/campaigns/"+id+"/start", nil) },
			map[string]int{"DRAFT": 200}, 409, "CAMPAIGN_INVALID_TRANSITION"},
		{"pause", func(id string) reply { return f.post(admin, "/campaigns/"+id+"/pause", nil) },
			map[string]int{"RUNNING": 200}, 409, "CAMPAIGN_INVALID_TRANSITION"},
		{"resume", func(id string) reply { return f.post(admin, "/campaigns/"+id+"/resume", nil) },
			map[string]int{"PAUSED": 200}, 409, "CAMPAIGN_INVALID_TRANSITION"},
		{"cancel", func(id string) reply { return f.post(admin, "/campaigns/"+id+"/cancel", nil) },
			nonTerminal, 409, "CAMPAIGN_INVALID_TRANSITION"},
		{"delete", func(id string) reply {
			return f.do(call{method: http.MethodDelete, path: "/campaigns/" + id, cookie: admin})
		},
			map[string]int{"DRAFT": 204}, 409, "CAMPAIGN_INVALID_TRANSITION"},
		{"import", func(id string) reply {
			return f.post(admin, "/campaigns/"+id+"/contacts", map[string]any{"contacts": []map[string]any{{"number": "+8613800138000"}}})
		}, nonTerminal, 409, "CAMPAIGN_INVALID_TRANSITION"},
		{"rename", func(id string) reply {
			return f.do(call{method: http.MethodPatch, path: "/campaigns/" + id, cookie: admin, body: map[string]any{"name": "Renamed"}})
		}, map[string]int{"DRAFT": 200}, 409, "CAMPAIGN_FIELD_LOCKED"},
		{"change maxConcurrent", func(id string) reply {
			return f.do(call{method: http.MethodPatch, path: "/campaigns/" + id, cookie: admin, body: map[string]any{"maxConcurrent": 11}})
		}, nonTerminal, 409, "CAMPAIGN_FIELD_LOCKED"},
	}

	for _, a := range actions {
		for _, status := range all {
			t.Run(a.name+" from "+status, func(t *testing.T) {
				c := reach(status)
				if c.Status != status {
					t.Fatalf("setup left the campaign %s, want %s", c.Status, status)
				}
				r := a.do(c.ID)
				if want, ok := a.allowed[status]; ok {
					wantReply(t, a.name+" from "+status, r, want, "")
					return
				}
				wantReply(t, a.name+" from "+status, r, a.refusedStatus, a.refusedCode)
				// A refusal changes nothing.
				after := decodeCampaign(t, f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin}))
				if after.Status != status || after.Name != c.Name || after.MaxConcurrent != c.MaxConcurrent {
					t.Errorf("a refused %s changed the campaign: %+v -> %+v", a.name, c, after)
				}
			})
		}
	}

	// A deleted draft is gone; an unknown id is 404 for every operation.
	c := f.createCampaign(admin, didID, nil)
	f.do(call{method: http.MethodDelete, path: "/campaigns/" + c.ID, cookie: admin})
	wantReply(t, "get a deleted campaign", f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin}), 404, "NOT_FOUND")
	ghost := uuid.NewString()
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/campaigns/" + ghost}, {http.MethodDelete, "/campaigns/" + ghost},
		{http.MethodPost, "/campaigns/" + ghost + "/start"}, {http.MethodPost, "/campaigns/" + ghost + "/pause"},
		{http.MethodPost, "/campaigns/" + ghost + "/resume"}, {http.MethodPost, "/campaigns/" + ghost + "/cancel"},
		{http.MethodGet, "/campaigns/" + ghost + "/contacts"},
	} {
		wantReply(t, p.method+" "+p.path, f.do(call{method: p.method, path: p.path, cookie: admin}), 404, "NOT_FOUND")
	}
	wantReply(t, "import into an unknown campaign", f.post(admin, "/campaigns/"+ghost+"/contacts",
		map[string]any{"contacts": []map[string]any{{"number": "+8613800138000"}}}), 404, "NOT_FOUND")
	wantReply(t, "not a uuid", f.do(call{method: http.MethodGet, path: "/campaigns/not-a-uuid", cookie: admin}), 400, "VALIDATION_FAILED")

	// A breaker's pause is resumed by an operator like any other.
	b := reach("RUNNING")
	if _, err := f.st.Pool.Exec(context.Background(),
		`UPDATE campaigns SET status = 'PAUSED', pause_reason = 'TRUNK_FAILURE' WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	r := f.do(call{method: http.MethodGet, path: "/campaigns/" + b.ID, cookie: admin})
	if v := decodeCampaign(t, r); v.PauseReason == nil || *v.PauseReason != "TRUNK_FAILURE" {
		t.Errorf("pause reason = %v", v.PauseReason)
	}
	r = f.post(admin, "/campaigns/"+b.ID+"/resume", nil)
	if v := decodeCampaign(t, r); r.status != 200 || v.Status != "RUNNING" || v.PauseReason != nil {
		t.Errorf("resume after a breaker: %d %+v", r.status, v)
	}

	// The refusal says which status it found.
	cc := reach("CANCELLED")
	r = f.post(admin, "/campaigns/"+cc.ID+"/start", nil)
	if got := errOf(t, r).Error.Params["status"]; got != "CANCELLED" {
		t.Errorf("params.status = %v", got)
	}
}

// Of N simultaneous requests for one transition exactly one wins, and the
// losers are told the status they found.
func TestRacingRequestsHaveOneWinner(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)

	race := func(n int, do func(i int) reply) (wins int, others []reply) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		gate := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				r := do(i)
				mu.Lock()
				defer mu.Unlock()
				if r.status == http.StatusOK {
					wins++
				} else {
					others = append(others, r)
				}
			}()
		}
		close(gate)
		wg.Wait()
		return
	}

	for round := range 3 {
		c := f.createCampaign(admin, didID, nil)
		wins, losers := race(16, func(int) reply { return f.post(admin, "/campaigns/"+c.ID+"/start", nil) })
		if wins != 1 {
			t.Errorf("round %d: %d of 16 concurrent starts won, want exactly 1", round, wins)
		}
		for _, r := range losers {
			if r.status != 409 || r.code != "CAMPAIGN_INVALID_TRANSITION" {
				t.Errorf("a losing start: %d %s", r.status, r.body)
			}
		}
		wins, _ = race(16, func(int) reply { return f.post(admin, "/campaigns/"+c.ID+"/cancel", nil) })
		if wins != 1 {
			t.Errorf("round %d: %d of 16 concurrent cancels won, want exactly 1", round, wins)
		}
		wins, _ = race(8, func(int) reply { return f.post(admin, "/campaigns/"+c.ID+"/start", nil) })
		if wins != 0 {
			t.Errorf("round %d: a cancelled campaign was started %d times", round, wins)
		}
	}

	// Start against cancel: cancel always lands, start only if it came first.
	for round := range 10 {
		c := f.createCampaign(admin, didID, nil)
		var startR, cancelR reply
		var wg sync.WaitGroup
		gate := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-gate; startR = f.post(admin, "/campaigns/"+c.ID+"/start", nil) }()
		go func() { defer wg.Done(); <-gate; cancelR = f.post(admin, "/campaigns/"+c.ID+"/cancel", nil) }()
		close(gate)
		wg.Wait()
		final := decodeCampaign(t, f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin}))
		if cancelR.status != 200 || final.Status != "CANCELLED" {
			t.Errorf("round %d: cancel %d, final %s", round, cancelR.status, final.Status)
		}
		if (startR.status == 200) != (final.StartedAt != nil) {
			t.Errorf("round %d: start answered %d but startedAt is %v", round, startR.status, final.StartedAt)
		}
	}

	// A rename racing a start: the answer to the rename says whether it
	// happened, and the stored name agrees.
	for round := range 10 {
		c := f.createCampaign(admin, didID, nil)
		name := fmt.Sprintf("renamed %d", round)
		var patchR reply
		var wg sync.WaitGroup
		gate := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			patchR = f.do(call{method: http.MethodPatch, path: "/campaigns/" + c.ID, cookie: admin, body: map[string]any{"name": name}})
		}()
		go func() { defer wg.Done(); <-gate; f.post(admin, "/campaigns/"+c.ID+"/start", nil) }()
		close(gate)
		wg.Wait()
		final := decodeCampaign(t, f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin}))
		switch patchR.status {
		case 200:
			if final.Name != name {
				t.Errorf("round %d: rename answered 200 but the name is %q", round, final.Name)
			}
		case 409:
			if patchR.code != "CAMPAIGN_FIELD_LOCKED" || final.Name == name {
				t.Errorf("round %d: rename refused (%s) but the name is %q", round, patchR.code, final.Name)
			}
		default:
			t.Errorf("round %d: rename answered %d %s", round, patchR.status, patchR.body)
		}
	}
}

func TestCampaignContactsAreListedByStatusAndLead(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, func(b map[string]any) { b["defaultCountryCode"] = "86" })

	rows := make([]map[string]any, 0, 30)
	for i := range 30 {
		rows = append(rows, map[string]any{"number": fmt.Sprintf("1380013%04d", i), "slots": map[string]string{"name": fmt.Sprintf("c%02d", i)}})
	}
	r := f.post(admin, "/campaigns/"+c.ID+"/contacts", map[string]any{"contacts": rows})
	if r.status != 200 {
		t.Fatalf("import: %d %s", r.status, r.body)
	}
	// What the dialer and the lead recorder will write, set directly.
	if _, err := f.st.Pool.Exec(context.Background(), `
		UPDATE campaign_contacts SET status = 'DONE', next_attempt_at = NULL, attempts = 1, last_outcome = 'ANSWERED',
		       lead_intent = CASE WHEN number_e164 < '+8613800130005' THEN 'HIGH' ELSE 'LOW' END, lead_notes = 'asked for a quote'
		WHERE campaign_id = $1 AND number_e164 < '+8613800130010'`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(context.Background(), `
		UPDATE campaign_contacts SET status = 'EXHAUSTED', next_attempt_at = NULL, attempts = 3, last_outcome = 'NO_ANSWER'
		WHERE campaign_id = $1 AND number_e164 >= '+8613800130025'`, c.ID); err != nil {
		t.Fatal(err)
	}

	type page struct {
		Items []struct {
			ID, Number, Status string
			LeadIntent         *string
			LeadNotes          *string
			LastOutcome        *string
			NextAttemptAt      *string
			Attempts           int
			Slots              map[string]string
		} `json:"items"`
		Total int `json:"total"`
	}
	get := func(q string) page {
		t.Helper()
		r := f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID + "/contacts" + q, cookie: admin})
		if r.status != 200 {
			t.Fatalf("list %s: %d %s", q, r.status, r.body)
		}
		var p page
		_ = json.Unmarshal(r.body, &p)
		return p
	}
	if p := get(""); p.Total != 30 || len(p.Items) != 30 || p.Items[0].Number != "+8613800130000" || p.Items[29].Number != "+8613800130029" {
		t.Errorf("all: total %d, %d items, first %s", p.Total, len(p.Items), p.Items[0].Number)
	}
	if p := get("?status=DONE"); p.Total != 10 || len(p.Items) != 10 {
		t.Errorf("DONE: %d/%d", p.Total, len(p.Items))
	}
	if p := get("?status=ACTIVE"); p.Total != 15 {
		t.Errorf("ACTIVE: %d", p.Total)
	} else if p.Items[0].NextAttemptAt != nil {
		t.Errorf("a contact of a DRAFT campaign is due: %v", *p.Items[0].NextAttemptAt)
	}
	if p := get("?status=EXHAUSTED"); p.Total != 5 {
		t.Errorf("EXHAUSTED: %d", p.Total)
	}
	if p := get("?leadIntent=HIGH"); p.Total != 5 || *p.Items[0].LeadIntent != "HIGH" || *p.Items[0].LeadNotes != "asked for a quote" {
		t.Errorf("HIGH: %d", p.Total)
	}
	if p := get("?leadIntent=LOW&status=DONE"); p.Total != 5 {
		t.Errorf("LOW and DONE: %d", p.Total)
	}
	if p := get("?leadIntent=MEDIUM"); p.Total != 0 || len(p.Items) != 0 {
		t.Errorf("MEDIUM: %d", p.Total)
	}
	if p := get("?limit=7&offset=7"); len(p.Items) != 7 || p.Total != 30 || p.Items[0].Number != "+8613800130007" {
		t.Errorf("page: %d items, first %s", len(p.Items), p.Items[0].Number)
	}
	for _, q := range []string{"?status=GONE", "?leadIntent=WARM"} {
		r := f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID + "/contacts" + q, cookie: admin})
		wantReply(t, q, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED")
	}
	// The campaign's counters agree.
	v := decodeCampaign(t, f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin}))
	if v.Contacts.Total != 30 || v.Contacts.Active != 15 || v.Contacts.Done != 10 || v.Contacts.Exhausted != 5 ||
		v.Contacts.InvalidNumber != 0 || v.Contacts.DNC != 0 {
		t.Errorf("counters = %+v", v.Contacts)
	}

	// Starting makes the ACTIVE ones due; imports into a running campaign are due at once.
	f.post(admin, "/campaigns/"+c.ID+"/start", nil)
	for _, p := range []page{get("?status=ACTIVE")} {
		for _, it := range p.Items {
			if it.NextAttemptAt == nil {
				t.Errorf("%s is ACTIVE in a RUNNING campaign with no due time", it.Number)
			}
		}
	}
	f.post(admin, "/campaigns/"+c.ID+"/contacts", map[string]any{"contacts": []map[string]any{{"number": "13900000001"}}})
	if p := get("?status=ACTIVE"); p.Total != 16 || p.Items[15].NextAttemptAt == nil {
		t.Errorf("a contact imported into a RUNNING campaign is not due: %+v", p.Items[len(p.Items)-1])
	}
}

// 3000 rows of the kind a real list has, every row checked against the
// expected answer built in by the generator.
func TestAMessyListOfThreeThousandIsImportedRowByRow(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, func(b map[string]any) { b["defaultCountryCode"] = "86" })
	url := "/campaigns/" + c.ID + "/contacts"

	list := genContacts(89, 3000)
	// The generator must have produced a list worth testing.
	kinds := map[string]int{}
	for _, r := range list.Rows {
		kinds[strings.SplitN(r.Kind, ",", 2)[0]]++
	}
	t.Logf("%d rows: %d to import, %d duplicates, %d rejected; kinds %v",
		len(list.Rows), list.Imported, list.Duplicates, list.Rejected, kinds)
	if list.Imported < 1500 || list.Duplicates < 300 || list.Rejected < 150 {
		t.Fatalf("the generated list is too tame: %d/%d/%d", list.Imported, list.Duplicates, list.Rejected)
	}

	type rowResult struct {
		Index     int    `json:"index"`
		Status    string `json:"status"`
		ContactID string `json:"contactId"`
		Number    string `json:"number"`
		Code      string `json:"code"`
		Detail    string `json:"detail"`
	}
	type result struct {
		Imported, Duplicates, Rejected int
		Rows                           []rowResult `json:"rows"`
	}
	importAll := func(l genList) result {
		t.Helper()
		r := f.post(admin, url, l.body())
		if r.status != http.StatusOK {
			t.Fatalf("import: %d %.400s", r.status, r.body)
		}
		var out result
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	res := importAll(list)
	if len(res.Rows) != len(list.Rows) {
		t.Fatalf("%d results for %d rows", len(res.Rows), len(list.Rows))
	}
	if res.Imported != list.Imported || res.Duplicates != list.Duplicates || res.Rejected != list.Rejected {
		t.Errorf("counts = %d/%d/%d, want %d/%d/%d", res.Imported, res.Duplicates, res.Rejected,
			list.Imported, list.Duplicates, list.Rejected)
	}
	idOf := map[int]string{} // the contact each first row created
	bad := 0
	for i, got := range res.Rows {
		row := list.Rows[i]
		fail := func(format string, args ...any) {
			bad++
			if bad <= 15 {
				t.Errorf("row %d (%s, sent %v): %s", i, row.Kind, row.JSON["number"], fmt.Sprintf(format, args...))
			}
		}
		if got.Index != i {
			fail("index = %d", got.Index)
		}
		if got.Status != list.WantStatus[i] {
			fail("status = %s, want %s", got.Status, list.WantStatus[i])
			continue
		}
		if got.Number != row.WantNumber {
			fail("number = %q, want %q", got.Number, row.WantNumber)
		}
		switch got.Status {
		case "REJECTED":
			if got.Code != row.WantCode {
				fail("code = %q, want %q", got.Code, row.WantCode)
			}
			if got.ContactID != "" {
				fail("a rejected row has a contact id")
			}
		case "IMPORTED":
			if got.ContactID == "" || got.Code != "" {
				fail("imported row: %+v", got)
			}
			idOf[i] = got.ContactID
		case "DUPLICATE":
			if got.ContactID != idOf[list.WantFirst[i]] || got.ContactID == "" {
				fail("duplicate points at %s, want row %d's contact %s", got.ContactID, list.WantFirst[i], idOf[list.WantFirst[i]])
			}
		}
	}

	// What is stored is exactly the first claimant of every number, whole.
	type stored struct {
		ID, Number, NumberRaw string
		TimeZone              *string
		Slots                 map[string]string
		Status                string
	}
	want := map[string]int{} // canonical number -> claimant row
	for i, s := range list.WantStatus {
		if s == "IMPORTED" {
			want[list.Rows[i].WantNumber] = i
		}
	}
	got := map[string]stored{}
	for offset := 0; ; offset += 1000 {
		r := f.do(call{method: http.MethodGet, path: fmt.Sprintf("%s?limit=1000&offset=%d", url, offset), cookie: admin})
		var p struct {
			Items []stored `json:"items"`
			Total int      `json:"total"`
		}
		_ = json.Unmarshal(r.body, &p)
		if p.Total != list.Imported {
			t.Fatalf("total contacts = %d, want %d", p.Total, list.Imported)
		}
		for _, it := range p.Items {
			got[it.Number] = it
		}
		if len(p.Items) == 0 || offset+len(p.Items) >= p.Total {
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d distinct stored numbers, want %d", len(got), len(want))
	}
	mismatch := 0
	for number, i := range want {
		row, s := list.Rows[i], got[number]
		wantZone := row.WantZone
		gotZone := ""
		if s.TimeZone != nil {
			gotZone = *s.TimeZone
		}
		if s.ID != idOf[i] || s.NumberRaw != row.Raw || gotZone != wantZone || s.Status != "ACTIVE" ||
			!mapsEqual(s.Slots, row.WantSlots) {
			mismatch++
			if mismatch <= 10 {
				t.Errorf("%s from row %d (%s): stored %+v, want raw %q zone %q slots %v id %s",
					number, i, row.Kind, s, row.Raw, wantZone, row.WantSlots, idOf[i])
			}
		}
	}
	v := decodeCampaign(t, f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin}))
	if v.Contacts.Total != list.Imported || v.Contacts.Active != list.Imported {
		t.Errorf("counters = %+v, want %d active", v.Contacts, list.Imported)
	}

	// Importing the same list again changes nothing: every usable row is a
	// duplicate of the contact it already had, every bad row is still bad.
	again := importAll(list)
	if again.Imported != 0 || again.Duplicates != list.Imported+list.Duplicates || again.Rejected != list.Rejected {
		t.Errorf("re-import counts = %d/%d/%d", again.Imported, again.Duplicates, again.Rejected)
	}
	for i, r := range again.Rows {
		switch list.WantStatus[i] {
		case "REJECTED":
			if r.Status != "REJECTED" || r.Code != list.Rows[i].WantCode {
				t.Fatalf("row %d: re-import status %s code %s", i, r.Status, r.Code)
			}
		default:
			first := i
			if list.WantStatus[i] == "DUPLICATE" {
				first = list.WantFirst[i]
			}
			if r.Status != "DUPLICATE" || r.ContactID != idOf[first] {
				t.Fatalf("row %d: re-import status %s contact %s, want DUPLICATE of %s", i, r.Status, r.ContactID, idOf[first])
			}
		}
	}

	// ... and with different slots it still overwrites nothing: every number
	// that was claimed keeps what its first row gave it. (A number whose first
	// row was rejected for its slots was never claimed, and now has a usable
	// row: that one is new, and has the new slots.)
	changed := genContacts(89, 3000)
	for i := range changed.Rows {
		if _, ok := changed.Rows[i].JSON["slots"]; ok {
			changed.Rows[i].JSON["slots"] = map[string]string{"name": "OVERWRITTEN"}
		}
	}
	importAll(changed)
	after := map[string]map[string]string{}
	for offset := 0; ; offset += 1000 {
		r := f.do(call{method: http.MethodGet, path: fmt.Sprintf("%s?limit=1000&offset=%d", url, offset), cookie: admin})
		var p struct {
			Items []stored `json:"items"`
			Total int      `json:"total"`
		}
		_ = json.Unmarshal(r.body, &p)
		for _, it := range p.Items {
			after[it.Number] = it.Slots
		}
		if len(p.Items) == 0 || offset+len(p.Items) >= p.Total {
			break
		}
	}
	for number, i := range want {
		if !mapsEqual(after[number], list.Rows[i].WantSlots) {
			t.Errorf("%s: slots %v after a repeat import with other slots, want the first import's %v", number, after[number], list.Rows[i].WantSlots)
			break
		}
	}
	for number, slots := range after {
		if _, claimed := want[number]; !claimed && slots["name"] != "OVERWRITTEN" {
			t.Errorf("%s is a new contact of the second import and should carry its slots, has %v", number, slots)
			break
		}
	}

	// The same list in another campaign: the same people are new there.
	other := f.createCampaign(admin, didID, func(b map[string]any) { b["name"] = "Other"; b["defaultCountryCode"] = "86" })
	r := f.post(admin, "/campaigns/"+other.ID+"/contacts", list.body())
	var second result
	_ = json.Unmarshal(r.body, &second)
	if second.Imported != list.Imported || second.Duplicates != list.Duplicates {
		t.Errorf("second campaign counts = %d/%d, want %d/%d", second.Imported, second.Duplicates, list.Imported, list.Duplicates)
	}
	var shared int
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM (SELECT number_e164 FROM campaign_contacts GROUP BY number_e164 HAVING count(DISTINCT campaign_id) = 2) s`).Scan(&shared); err != nil || shared != list.Imported {
		t.Errorf("%d numbers in both campaigns, want %d (%v)", shared, list.Imported, err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestImportLimitsAndDefaults(t *testing.T) {
	f := newFixture(t) // the fixture's campaign service completes national numbers with 86
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil) // no country code of its own
	url := "/campaigns/" + c.ID + "/contacts"

	// The deployment's country code applies when the campaign has none.
	r := f.post(admin, url, map[string]any{"contacts": []map[string]any{{"number": "138 0013 8000"}, {"number": "+8613800138000"}}})
	var res struct {
		Imported, Duplicates int
		Rows                 []struct{ Number string }
	}
	_ = json.Unmarshal(r.body, &res)
	if res.Imported != 1 || res.Duplicates != 1 || res.Rows[0].Number != "+8613800138000" {
		t.Errorf("deployment country code: %s", r.body)
	}

	// 5000 rows is the limit; 5001 is refused whole, with a code that says so.
	rows := func(n int) map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"number": fmt.Sprintf("+86139%08d", i)}
		}
		return map[string]any{"contacts": out}
	}
	r = f.post(admin, url, rows(5001))
	wantReply(t, "5001 rows", r, 422, "VALIDATION_FAILED")
	if e := errOf(t, r).Error.Params; e["rule"] != "TOO_MANY_ROWS" || e["field"] != "contacts" {
		t.Errorf("params = %v", e)
	}
	r = f.post(admin, url, rows(5000))
	_ = json.Unmarshal(r.body, &res)
	if r.status != 200 || res.Imported != 5000 {
		t.Errorf("5000 rows: %d imported %d", r.status, res.Imported)
	}
	if v := decodeCampaign(t, f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin})); v.Contacts.Total != 5001 {
		t.Errorf("total = %d, want 5001", v.Contacts.Total)
	}

	wantReply(t, "no rows", f.post(admin, url, map[string]any{"contacts": []map[string]any{}}), 422, "VALIDATION_FAILED")
	wantReply(t, "no contacts property", f.post(admin, url, map[string]any{}), 422, "VALIDATION_FAILED")
	wantReply(t, "a row property the contract does not have", f.post(admin, url,
		map[string]any{"contacts": []map[string]any{{"number": "+8613800138001", "name": "x"}}}), 400, "VALIDATION_FAILED")
	wantReply(t, "a number that is not text", f.post(admin, url,
		map[string]any{"contacts": []map[string]any{{"number": 13800138001}}}), 400, "VALIDATION_FAILED")

	// A body beyond the cap is refused before it is read.
	huge := map[string]any{"contacts": []map[string]any{{"number": "+8613800138002",
		"slots": map[string]string{"name": strings.Repeat("x", 17<<20)}}}}
	wantReply(t, "a 17 MiB body", f.post(admin, url, huge), http.StatusRequestEntityTooLarge, "VALIDATION_FAILED")
}

func TestOnlyScopedCredentialsReachCampaigns(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	supervisor := f.seedSupervisor("sup", 1100)
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil)

	_, readKey := f.issueKey(admin, "campaigns:read")
	_, writeKey := f.issueKey(admin, "campaigns:write")
	_, otherKey := f.issueKey(admin, "config:write", "dnc:write", "contacts:write")

	type op struct {
		name, method, path string
		body               any
		write              bool
	}
	ops := []op{
		{"list", http.MethodGet, "/campaigns", nil, false},
		{"get", http.MethodGet, "/campaigns/" + c.ID, nil, false},
		{"list contacts", http.MethodGet, "/campaigns/" + c.ID + "/contacts", nil, false},
		{"create", http.MethodPost, "/campaigns", campaignBody(didID, func(b map[string]any) { b["name"] = "By key" }), true},
		{"patch", http.MethodPatch, "/campaigns/" + c.ID, map[string]any{"maxConcurrent": 6}, true},
		{"start", http.MethodPost, "/campaigns/" + uuid.NewString() + "/start", nil, true},
		{"pause", http.MethodPost, "/campaigns/" + uuid.NewString() + "/pause", nil, true},
		{"resume", http.MethodPost, "/campaigns/" + uuid.NewString() + "/resume", nil, true},
		{"cancel", http.MethodPost, "/campaigns/" + uuid.NewString() + "/cancel", nil, true},
		{"import", http.MethodPost, "/campaigns/" + c.ID + "/contacts", map[string]any{"contacts": []map[string]any{{"number": "+8613800138000"}}}, true},
		{"delete", http.MethodDelete, "/campaigns/" + uuid.NewString(), nil, true},
	}

	for _, o := range ops {
		t.Run(o.name, func(t *testing.T) {
			// An agent's session reaches none of it.
			r := f.do(call{method: o.method, path: o.path, body: o.body, cookie: f.agent.cookie})
			wantReply(t, "agent", r, http.StatusForbidden, "INSUFFICIENT_SCOPE")
			// Nor does a key with other scopes, or nothing.
			r = f.do(call{method: o.method, path: o.path, body: o.body, bearer: otherKey})
			wantReply(t, "unrelated key", r, http.StatusForbidden, "INSUFFICIENT_SCOPE")
			r = f.do(call{method: o.method, path: o.path, body: o.body})
			if r.status != http.StatusUnauthorized {
				t.Errorf("anonymous: %d", r.status)
			}

			// A read-only key reads and cannot write; a write-only key writes
			// and cannot read.
			r = f.do(call{method: o.method, path: o.path, body: o.body, bearer: readKey})
			if o.write {
				wantReply(t, "read-only key", r, http.StatusForbidden, "INSUFFICIENT_SCOPE")
			} else if r.status != http.StatusOK {
				t.Errorf("read-only key on a read: %d %s", r.status, r.body)
			}
			r = f.do(call{method: o.method, path: o.path, body: o.body, bearer: writeKey})
			if !o.write {
				wantReply(t, "write-only key", r, http.StatusForbidden, "INSUFFICIENT_SCOPE")
			} else if r.status == http.StatusForbidden || r.status == http.StatusUnauthorized {
				t.Errorf("write-only key on a write: %d %s", r.status, r.body)
			}

			// A supervisor's and an administrator's sessions reach everything:
			// whatever they get, it is not a refusal of the credential.
			for who, cookie := range map[string]string{"supervisor": supervisor, "admin": admin} {
				r = f.do(call{method: o.method, path: o.path, body: o.body, cookie: cookie})
				if r.status == http.StatusForbidden || r.status == http.StatusUnauthorized {
					t.Errorf("%s: %d %s", who, r.status, r.body)
				}
			}
		})
	}

	// The scope is what a session's role grants: supervisors and administrators.
	for _, role := range []auth.Role{auth.RoleSupervisor, auth.RoleAdmin} {
		got := grantedScopes(role)
		if !slices.Contains(got, "campaigns:read") || !slices.Contains(got, "campaigns:write") {
			t.Errorf("%s is not granted the campaign scopes: %v", role, got)
		}
	}
	if got := grantedScopes(auth.RoleAgent); slices.Contains(got, "campaigns:read") || slices.Contains(got, "campaigns:write") {
		t.Errorf("an agent is granted campaign scopes: %v", got)
	}
}

// Publishing a flow revision that can transfer is refused with the campaign's
// code while a campaign that has not finished dials with the flow.
func TestPublishingATransferUnderALiveCampaignIsRefusedOverHTTP(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, flowID := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil)
	ctx := context.Background()

	if err := f.st.Flows().UpdateDraft(ctx, flowID, "Flow", campaignFlowSpec("flow-95099", true)); err != nil {
		t.Fatal(err)
	}
	publish := func() reply {
		return f.do(call{method: http.MethodPost, path: "/flows/" + flowID.String() + "/publish", cookie: admin, body: map[string]any{}})
	}
	for _, step := range []string{"", "start", "pause"} {
		if step != "" {
			f.post(admin, "/campaigns/"+c.ID+"/"+step, nil)
		}
		r := publish()
		wantReply(t, "publish under a live campaign ("+step+")", r, http.StatusConflict, "CAMPAIGN_FLOW_HAS_TRANSFER")
	}
	if spec, err := f.st.Flows().PublishedSpec(ctx, flowID); err != nil || flow.CanTransfer(spec) {
		t.Fatalf("a refused publish went live: %v %v", spec, err)
	}

	// The campaign's own start also re-checks, for a flow that changed some
	// other way: nothing to do here, the guard above is what keeps it true.
	f.post(admin, "/campaigns/"+c.ID+"/cancel", nil)
	wantReply(t, "publish after cancel", publish(), http.StatusOK, "")
	if spec, err := f.st.Flows().PublishedSpec(ctx, flowID); err != nil || !flow.CanTransfer(spec) {
		t.Errorf("the allowed publish did not go live: %v %v", spec, err)
	}
	// And a new campaign on that DID is now refused at creation.
	r := f.post(admin, "/campaigns", campaignBody(didID, nil))
	wantReply(t, "create on a transferring flow", r, http.StatusUnprocessableEntity, "CAMPAIGN_FLOW_HAS_TRANSFER")
}

// The same refusal on the number's side maps to the same code.
func TestACampaignRefusalOnADIDMapsToTheContractsCode(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		err    error
		status int
		code   ErrorCode
	}{
		{&campaign.Error{Kind: campaign.ErrFlowHasTransfer, Rule: "DID_REBIND_BLOCKED", Message: "x"}, 409, CodeCampaignFlowHasTransfer},
		{&campaign.Error{Kind: campaign.ErrFlowHasTransfer, Field: "didId", Message: "x"}, 422, CodeCampaignFlowHasTransfer},
	} {
		w := httptest.NewRecorder()
		s.writeCatalogError(w, httptest.NewRequest(http.MethodPut, "/api/v1/dids/x", nil), tc.err)
		var e errView
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != tc.status || ErrorCode(e.Error.Code) != tc.code {
			t.Errorf("%v: %d %s, want %d %s", tc.err, w.Code, e.Error.Code, tc.status, tc.code)
		}
	}
}

// The limits on a contact's slots are the call's own. campaign cannot import
// telephony, so this is where they are held equal.
func TestContactSlotLimitsAreTheCallsOwn(t *testing.T) {
	if campaign.MaxSlotsPerRow != telephony.UserDataMaxKeys {
		t.Errorf("a contact may carry %d slots but a call carries %d keys", campaign.MaxSlotsPerRow, telephony.UserDataMaxKeys)
	}
	if campaign.MaxSlotValueRunes != flow.MaxSlotValueLen {
		t.Errorf("a slot value may be %d characters but the flow keeps %d", campaign.MaxSlotValueRunes, flow.MaxSlotValueLen)
	}
	// 256 of the widest characters still fit the call's byte cap.
	if 4*campaign.MaxSlotValueRunes > telephony.UserDataMaxValueBytes {
		t.Errorf("a value of %d four-byte characters is %d bytes, over the call's %d",
			campaign.MaxSlotValueRunes, 4*campaign.MaxSlotValueRunes, telephony.UserDataMaxValueBytes)
	}
}

// campaignDIDWithoutOutbound is a DID with a published flow that is not
// allowed to place calls.
func campaignDIDWithoutOutbound(t *testing.T, st *store.Store, number string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	didID, _ := campaignDID(t, st, number, false)
	dids, err := st.Catalog().ListDIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dids {
		if d.ID == didID {
			d.AllowInbound, d.AllowOutbound = true, false
			if _, err := st.Catalog().UpdateDID(ctx, d); err != nil {
				t.Fatalf("clear allowOutbound: %v", err)
			}
			return didID
		}
	}
	t.Fatal("DID vanished")
	return uuid.Nil
}

// A campaign dials from its DID, so the DID must allow outbound calls: at
// create and on PATCH of didId (start re-validates the same way; see the
// campaign package's TestStartChecksTheCampaignAgain).
func TestACampaignNeedsADIDThatAllowsOutbound(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	noOutbound := campaignDIDWithoutOutbound(t, f.st, "95201")

	r := f.post(admin, "/campaigns", campaignBody(noOutbound, nil))
	wantReply(t, "create on a DID that does not allow outbound", r, http.StatusUnprocessableEntity, "CAMPAIGN_DID_NOT_OUTBOUND")
	if got := errOf(t, r).Error.Params["field"]; got != "didId" {
		t.Errorf("params.field = %v, want didId", got)
	}

	c := f.createCampaign(admin, didID, nil)
	r = f.do(call{method: http.MethodPatch, path: "/campaigns/" + c.ID, cookie: admin, body: map[string]any{"didId": noOutbound.String()}})
	wantReply(t, "PATCH didId to one that does not allow outbound", r, http.StatusUnprocessableEntity, "CAMPAIGN_DID_NOT_OUTBOUND")
}

// Clearing allowOutbound on a DID a campaign that has not finished dials from
// is refused with 409, and allowed once the campaign is cancelled.
func TestClearingAllowOutboundUnderALiveCampaignIsRefusedOverHTTP(t *testing.T) {
	f := newFixtureWithStore(t, func(st *store.Store, d *Deps) {
		d.Catalog = catalog.NewService(st.Catalog(), nil, st.Catalog())
	})
	admin := f.seedAdmin()
	didID, flowID := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil)

	put := func(allowOutbound bool) reply {
		return f.do(call{method: http.MethodPut, path: "/dids/" + didID.String(), cookie: admin, body: map[string]any{
			"number": "95099", "language": "en", "flowId": flowID.String(), "isEnabled": true,
			"allowInbound": true, "allowOutbound": allowOutbound,
		}})
	}
	for _, step := range []string{"", "start", "pause"} {
		if step != "" {
			f.post(admin, "/campaigns/"+c.ID+"/"+step, nil)
		}
		wantReply(t, "clear allowOutbound under a campaign ("+step+")", put(false), http.StatusConflict, "CAMPAIGN_DID_NOT_OUTBOUND")
	}
	wantReply(t, "keep allowOutbound", put(true), http.StatusOK, "")

	f.post(admin, "/campaigns/"+c.ID+"/cancel", nil)
	wantReply(t, "clear allowOutbound after cancel", put(false), http.StatusOK, "")
}
