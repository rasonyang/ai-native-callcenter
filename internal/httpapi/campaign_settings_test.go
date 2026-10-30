// SPDX-License-Identifier: Apache-2.0

package httpapi

// What the campaign form learns from the server instead of mirroring it: the
// settings operation, the DID's isCampaignReady, and the announcement of a
// created campaign. Each is checked against the behaviour it describes.

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

func settingsOf(t *testing.T, f *fixture, cookie string) api.CampaignSettings {
	t.Helper()
	r := f.do(call{method: http.MethodGet, path: "/campaigns/settings", cookie: cookie})
	if r.status != http.StatusOK {
		t.Fatalf("settings: %d %s", r.status, r.body)
	}
	var s api.CampaignSettings
	if err := json.Unmarshal(r.body, &s); err != nil {
		t.Fatalf("decode settings: %v: %s", err, r.body)
	}
	return s
}

func campaignFixtureWith(t *testing.T, env campaign.Env) *fixture {
	return newFixtureWithStore(t, func(st *store.Store, d *Deps) {
		d.Campaigns = campaign.NewService(st.Campaigns(), nil, env)
	})
}

func windowBody(start, end string) func(map[string]any) {
	return func(b map[string]any) {
		b["callingWindow"] = map[string]any{"days": []string{"MON"}, "ranges": []map[string]string{{"start": start, "end": end}}}
	}
}

// The settings are the deployment's own values, they move with the
// configuration, and the limits they state are the edges the server enforces.
func TestCampaignSettingsAreTheValuesTheServerEnforces(t *testing.T) {
	narrow, _ := dayrange.Parse("10:00-12:00")
	night, _ := dayrange.Parse("20:00-04:00")
	for _, tc := range []struct {
		name         string
		env          campaign.Env
		start, end   string
		inside, edge []string // a window range inside the hours; one just past an end
	}{
		{"the default hours", campaign.Env{}, "08:00", "21:00",
			[]string{"08:00", "21:00"}, []string{"07:59", "21:01"}},
		{"a narrow range", campaign.Env{OutboundHours: narrow}, "10:00", "12:00",
			[]string{"10:00", "12:00"}, []string{"09:59", "12:01"}},
		{"a range across midnight", campaign.Env{OutboundHours: night}, "20:00", "04:00",
			[]string{"22:00", "02:00"}, []string{"19:59", "04:01"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := campaignFixtureWith(t, tc.env)
			admin := f.seedAdmin()
			s := settingsOf(t, f, admin)
			if s.OutboundHours.Start != tc.start || s.OutboundHours.End != tc.end {
				t.Errorf("outboundHours = %+v, want %s-%s", s.OutboundHours, tc.start, tc.end)
			}
			didID, _ := campaignDID(t, f.st, "95099", false)

			// The hours as a window are accepted, and a minute past either end is not.
			ok := f.post(admin, "/campaigns", campaignBody(didID, windowBody(tc.start, tc.end)))
			wantReply(t, "the outer hours themselves", ok, http.StatusCreated, "")
			wantReply(t, "inside", f.post(admin, "/campaigns", campaignBody(didID, windowBody(tc.inside[0], tc.inside[1]))),
				http.StatusCreated, "")
			wantReply(t, "past the start", f.post(admin, "/campaigns", campaignBody(didID, windowBody(tc.edge[0], tc.inside[1]))),
				http.StatusUnprocessableEntity, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT")
			wantReply(t, "past the end", f.post(admin, "/campaigns", campaignBody(didID, windowBody(tc.inside[0], tc.edge[1]))),
				http.StatusUnprocessableEntity, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT")

			// The numeric limits are the validators' edges.
			within := func(b map[string]any) { windowBody(tc.inside[0], tc.inside[1])(b) }
			create := func(ring, conc int) reply {
				return f.post(admin, "/campaigns", campaignBody(didID, func(b map[string]any) {
					within(b)
					b["ringTimeoutSec"], b["maxConcurrent"] = ring, conc
				}))
			}
			wantReply(t, "the largest ring timeout", create(s.Limits.RingTimeoutSec.Max, s.Limits.MaxConcurrent.Max), http.StatusCreated, "")
			wantReply(t, "the smallest ring timeout", create(s.Limits.RingTimeoutSec.Min, s.Limits.MaxConcurrent.Min), http.StatusCreated, "")
			wantReply(t, "one over", create(s.Limits.RingTimeoutSec.Max+1, 1), http.StatusUnprocessableEntity, "CAMPAIGN_RING_TIMEOUT_TOO_LONG")
			wantReply(t, "one under", create(s.Limits.RingTimeoutSec.Min-1, 1), http.StatusUnprocessableEntity, "VALIDATION_FAILED")
			wantReply(t, "too many concurrent", create(30, s.Limits.MaxConcurrent.Max+1), http.StatusUnprocessableEntity, "VALIDATION_FAILED")
		})
	}
}

// The route is not shadowed by /campaigns/{campaignId}, and the operation is
// reachable by a key holding campaigns:read and by nothing narrower.
func TestCampaignSettingsRoutingAndScope(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	if s := settingsOf(t, f, admin); s.Defaults.MaxConcurrent != 5 {
		t.Errorf("defaults = %+v", s.Defaults)
	}
	_, reader := f.issueKey(admin, "campaigns:read")
	wantReply(t, "campaigns:read", f.do(call{method: http.MethodGet, path: "/campaigns/settings", bearer: reader}), http.StatusOK, "")
	_, writer := f.issueKey(admin, "campaigns:write")
	r := f.do(call{method: http.MethodGet, path: "/campaigns/settings", bearer: writer})
	if r.status != http.StatusForbidden {
		t.Errorf("campaigns:write alone read the settings: %d %s", r.status, r.body)
	}
	wantReply(t, "anonymous", f.do(call{method: http.MethodGet, path: "/campaigns/settings"}), http.StatusUnauthorized, "SESSION_EXPIRED")
	// A campaign id is still a campaign id.
	r = f.do(call{method: http.MethodGet, path: "/campaigns/" + uuid.NewString(), cookie: admin})
	wantReply(t, "unknown campaign", r, http.StatusNotFound, "NOT_FOUND")
}

// What a create without the optional fields stores is what the settings say.
func TestTheSettingsDefaultsAreWhatACreateStores(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	s := settingsOf(t, f, admin)

	r := f.post(admin, "/campaigns", map[string]any{
		"name": "Minimal", "didId": didID.String(), "timeZone": "Asia/Shanghai", "callingWindow": weekdayWindow(),
	})
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	var c api.Campaign
	if err := json.Unmarshal(r.body, &c); err != nil {
		t.Fatal(err)
	}
	if c.MaxConcurrent != s.Defaults.MaxConcurrent || c.RingTimeoutSec != s.Defaults.RingTimeoutSec {
		t.Errorf("stored %d/%d, settings say %d/%d", c.MaxConcurrent, c.RingTimeoutSec,
			s.Defaults.MaxConcurrent, s.Defaults.RingTimeoutSec)
	}
	if !reflect.DeepEqual(c.RetryPolicy, s.Defaults.RetryPolicy) {
		t.Errorf("stored policy %+v, settings say %+v", c.RetryPolicy, s.Defaults.RetryPolicy)
	}
	// And the stored row agrees, not only the response.
	got := f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID.String(), cookie: admin})
	var again api.Campaign
	_ = json.Unmarshal(got.body, &again)
	if !reflect.DeepEqual(again.RetryPolicy, s.Defaults.RetryPolicy) || again.MaxConcurrent != s.Defaults.MaxConcurrent {
		t.Errorf("re-read campaign differs from the defaults: %+v", again)
	}
}

type didView struct {
	ID              string `json:"id"`
	Number          string `json:"number"`
	IsCampaignReady *bool  `json:"isCampaignReady"`
}

func listedDID(t *testing.T, f *fixture, cookie string, id uuid.UUID) didView {
	t.Helper()
	r := f.do(call{method: http.MethodGet, path: "/dids", cookie: cookie})
	if r.status != http.StatusOK {
		t.Fatalf("list dids: %d %s", r.status, r.body)
	}
	var out struct {
		Items []didView `json:"items"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode dids: %v: %s", err, r.body)
	}
	for _, d := range out.Items {
		if d.ID == id.String() {
			if d.IsCampaignReady == nil {
				t.Fatalf("DID %s has no isCampaignReady: %s", d.Number, r.body)
			}
			return d
		}
	}
	t.Fatalf("DID %s is not listed", id)
	return didView{}
}

// isCampaignReady agrees with createCampaign for every kind of number, in the
// list and in the write responses, and follows a republish.
func TestIsCampaignReadyAgreesWithCreatingACampaign(t *testing.T) {
	f := newFixtureWithStore(t, func(st *store.Store, d *Deps) {
		d.Catalog = catalog.NewService(st.Catalog(), nil, st.Catalog())
	})
	admin := f.seedAdmin()
	ctx := context.Background()

	flowOf := func(slug string, transfer, publish bool) uuid.UUID {
		id, err := f.st.Flows().Create(ctx, slug, slug, campaignFlowSpec(slug, transfer))
		if err != nil {
			t.Fatal(err)
		}
		if publish {
			if err := f.st.Flows().Publish(ctx, id, "t"); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	didOf := func(number string, flowID *uuid.UUID, enabled, outbound bool) uuid.UUID {
		d, err := f.st.Catalog().CreateDID(ctx, catalog.DID{
			ID: uuid.Must(uuid.NewV7()), Number: number, Language: "en", FlowID: flowID,
			IsEnabled: enabled, AllowOutbound: outbound, AllowInbound: !outbound && flowID != nil,
		})
		if err != nil {
			t.Fatalf("did %s: %v", number, err)
		}
		return d.ID
	}
	pub, draftOnly, withTransfer := flowOf("pub", false, true), flowOf("draft", false, false), flowOf("xfer", true, true)
	flipper := flowOf("flip", false, true)

	cases := []struct {
		name    string
		id      uuid.UUID
		ready   bool
		refusal string
	}{
		{"published, cannot transfer", didOf("95101", &pub, true, true), true, ""},
		{"no flow", didOf("95102", nil, true, true), false, "VALIDATION_FAILED"},
		{"an unpublished draft only", didOf("95103", &draftOnly, true, true), false, "VALIDATION_FAILED"},
		{"published with transfer_to_agent", didOf("95104", &withTransfer, true, true), false, "CAMPAIGN_FLOW_HAS_TRANSFER"},
		{"disabled", didOf("95105", &pub, false, true), false, "VALIDATION_FAILED"},
		{"outbound not allowed", didOf("95106", &pub, true, false), false, "CAMPAIGN_DID_NOT_OUTBOUND"},
	}
	flipDID := didOf("95107", &flipper, true, true)
	cases = append(cases, struct {
		name    string
		id      uuid.UUID
		ready   bool
		refusal string
	}{"a flow republished", flipDID, true, ""})

	attempt := func(t *testing.T, id uuid.UUID, wantReady bool, refusal string) {
		t.Helper()
		r := f.post(admin, "/campaigns", campaignBody(id, nil))
		if wantReady {
			wantReply(t, "create", r, http.StatusCreated, "")
			// A campaign that has not finished blocks republishing a transferring flow.
			c := decodeCampaign(t, r)
			wantReply(t, "cleanup", f.do(call{method: http.MethodDelete, path: "/campaigns/" + c.ID, cookie: admin}),
				http.StatusNoContent, "")
			return
		}
		wantReply(t, "create", r, http.StatusUnprocessableEntity, refusal)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := *listedDID(t, f, admin, tc.id).IsCampaignReady; got != tc.ready {
				t.Errorf("isCampaignReady = %v, want %v", got, tc.ready)
			}
			attempt(t, tc.id, tc.ready, tc.refusal)
		})
	}

	// Republish the flow with a transfer, then without: the flag follows the
	// published revision, never the draft.
	spec := func(transfer bool) []byte { return campaignFlowSpec("flip", transfer) }
	if err := f.st.Flows().UpdateDraft(ctx, flipper, "flip", spec(true)); err != nil {
		t.Fatal(err)
	}
	if !*listedDID(t, f, admin, flipDID).IsCampaignReady {
		t.Error("an unpublished draft with a transfer changed the flag")
	}
	for _, transfer := range []bool{true, false} {
		if err := f.st.Flows().UpdateDraft(ctx, flipper, "flip", spec(transfer)); err != nil {
			t.Fatal(err)
		}
		if err := f.st.Flows().Publish(ctx, flipper, "t"); err != nil {
			t.Fatal(err)
		}
		want := !transfer
		if got := *listedDID(t, f, admin, flipDID).IsCampaignReady; got != want {
			t.Errorf("after publishing transfer=%v: isCampaignReady = %v, want %v", transfer, got, want)
		}
		refusal := "CAMPAIGN_FLOW_HAS_TRANSFER"
		attempt(t, flipDID, want, refusal)
	}

	// The write responses carry it too.
	r := f.do(call{method: http.MethodPut, path: "/dids/" + flipDID.String(), cookie: admin, body: map[string]any{
		"number": "95107", "flowId": flipper.String(), "allowInbound": false, "allowOutbound": true, "isEnabled": true,
	}})
	var put didView
	_ = json.Unmarshal(r.body, &put)
	if r.status != http.StatusOK || put.IsCampaignReady == nil || !*put.IsCampaignReady {
		t.Errorf("PUT /dids: %d %s", r.status, r.body)
	}
	r = f.do(call{method: http.MethodPost, path: "/dids", cookie: admin, body: map[string]any{
		"number": "95108", "allowInbound": false, "allowOutbound": true, "isCampaignReady": true,
	}})
	var created didView
	_ = json.Unmarshal(r.body, &created)
	if r.status != http.StatusCreated || created.IsCampaignReady == nil || *created.IsCampaignReady {
		t.Errorf("POST /dids with a client-supplied flag: %d %s", r.status, r.body)
	}
}

// A create through the API reaches the reporter exactly once; a refused one
// never does. The campaign's own screens learn of the new DRAFT without a
// refetch.
func TestCreatingACampaignIsAnnouncedOnce(t *testing.T) {
	var (
		mu    sync.Mutex
		tasks []func()
		seen  []campaign.Snapshot
	)
	sched := schedulerFunc(func(_ time.Duration, f func()) func() bool {
		mu.Lock()
		tasks = append(tasks, f)
		mu.Unlock()
		return func() bool { return true }
	})
	f := newFixtureWithStore(t, func(st *store.Store, d *Deps) {
		svc := campaign.NewService(st.Campaigns(), nil, campaign.Env{})
		rep := campaign.NewReporter(svc.Get, func(s campaign.Snapshot) { mu.Lock(); seen = append(seen, s); mu.Unlock() }, sched, nil)
		t.Cleanup(rep.Close)
		d.Campaigns = svc.WithCreatedHook(rep.Touch)
	})
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	runWindows := func() {
		mu.Lock()
		due := tasks
		tasks = nil
		mu.Unlock()
		for _, f := range due {
			f()
		}
	}

	wantReply(t, "refused", f.post(admin, "/campaigns", campaignBody(didID, windowBody("05:00", "06:00"))),
		http.StatusUnprocessableEntity, "CAMPAIGN_WINDOW_OUTSIDE_LIMIT")
	runWindows()
	if len(seen) != 0 {
		t.Fatalf("a refused create announced %+v", seen)
	}

	c := f.createCampaign(admin, didID, nil)
	runWindows()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].CampaignID.String() != c.ID || seen[0].Status != campaign.StatusDraft ||
		seen[0].Contacts != (campaign.ContactCounts{}) {
		t.Fatalf("announcements = %+v, want one DRAFT with zero contacts", seen)
	}
}

type schedulerFunc func(d time.Duration, f func()) func() bool

func (s schedulerFunc) AfterFunc(d time.Duration, f func()) func() bool { return s(d, f) }
