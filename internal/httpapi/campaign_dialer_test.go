// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// progressFake is what the dialer says about campaigns.
type progressFake map[uuid.UUID]campaign.Progress

func (p progressFake) Progress(id uuid.UUID) (campaign.Progress, bool) {
	v, ok := p[id]
	return v, ok
}

type attemptView struct {
	ID          string  `json:"id"`
	ContactID   string  `json:"contactId"`
	Number      string  `json:"number"`
	CallID      string  `json:"callId"`
	LegID       string  `json:"legId"`
	State       string  `json:"state"`
	Outcome     *string `json:"outcome"`
	HangupCause *string `json:"hangupCause"`
	TalkSec     *int    `json:"talkSec"`
	IsTouched   bool    `json:"isTouched"`
	StartedAt   string  `json:"startedAt"`
	FinishedAt  *string `json:"finishedAt"`
}

type attemptList struct {
	Items []attemptView `json:"items"`
	Total int           `json:"total"`
}

func TestARunningCampaignShowsWhatTheDialerSaysAboutIt(t *testing.T) {
	progress := progressFake{}
	outer, _ := dayrange.Parse("08:00-21:00")
	f := newFixtureWithStore(t, func(st *store.Store, d *Deps) {
		d.Campaigns = campaign.NewService(st.Campaigns(), nil,
			campaign.Env{OutboundHours: outer, DefaultCountryCode: "86"}).
			WithPlacer(canPlace(true)).WithDialer(progress, nil)
	})
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil)

	type view struct {
		Status     string  `json:"status"`
		Activity   *string `json:"activity"`
		NextDialAt *string `json:"nextDialAt"`
		InFlight   *int    `json:"inFlight"`
	}
	read := func() view {
		r := f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID, cookie: admin})
		var v view
		if err := json.Unmarshal(r.body, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	id := uuid.MustParse(c.ID)

	// A draft has none, even if the dialer had something to say.
	progress[id] = campaign.Progress{Activity: campaign.ActivityDialing, InFlight: 3}
	if v := read(); v.Activity != nil || v.InFlight != nil || v.NextDialAt != nil {
		t.Errorf("a DRAFT shows %+v", v)
	}

	if r := f.post(admin, "/campaigns/"+c.ID+"/start", nil); r.status != http.StatusOK {
		t.Fatalf("start: %d %s", r.status, r.body)
	}
	next := time.Date(2026, 6, 2, 1, 0, 0, 0, time.UTC)
	progress[id] = campaign.Progress{Activity: campaign.ActivityWaitingWindow, NextDialAt: &next, InFlight: 0}
	v := read()
	if v.Status != "RUNNING" || v.Activity == nil || *v.Activity != "WAITING_WINDOW" ||
		v.NextDialAt == nil || *v.NextDialAt != "2026-06-02T01:00:00Z" || v.InFlight == nil || *v.InFlight != 0 {
		t.Errorf("running campaign = %+v, want WAITING_WINDOW until 01:00Z with nothing in flight", v)
	}

	// The list carries it too, and a paused campaign loses it.
	r := f.do(call{method: http.MethodGet, path: "/campaigns", cookie: admin})
	var list struct{ Items []view }
	_ = json.Unmarshal(r.body, &list)
	if len(list.Items) != 1 || list.Items[0].Activity == nil {
		t.Errorf("list = %+v", list)
	}
	f.post(admin, "/campaigns/"+c.ID+"/pause", nil)
	if v := read(); v.Status != "PAUSED" || v.Activity != nil {
		t.Errorf("a paused campaign shows %+v", v)
	}
}

func TestACampaignsAttemptsAreListedNewestFirstAndFiltered(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	didID, _ := campaignDID(t, f.st, "95099", false)
	c := f.createCampaign(admin, didID, nil)
	cid := uuid.MustParse(c.ID)
	ctx := context.Background()

	rows := []campaign.NewContact{}
	for _, n := range []string{"+8613800000001", "+8613800000002", "+8613800000003"} {
		rows = append(rows, campaign.NewContact{ID: uuid.Must(uuid.NewV7()), Number: n, NumberRaw: n})
	}
	if _, err := f.st.Campaigns().ImportContacts(ctx, cid, rows); err != nil {
		t.Fatal(err)
	}
	if r := f.post(admin, "/campaigns/"+c.ID+"/start", nil); r.status != http.StatusOK {
		t.Fatalf("start: %d %s", r.status, r.body)
	}

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var attempts []campaign.Attempt
	for i, kind := range []string{"NO_ANSWER", "USER_BUSY", ""} {
		a, err := f.st.Attempts().Begin(ctx, rows[i].ID, base.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, a)
		if kind == "" {
			continue // the third is still in flight
		}
		status := store.CDRStatusNoAnswer
		if kind == "USER_BUSY" {
			status = store.CDRStatusBusy
		}
		start := base.Add(time.Duration(i) * time.Minute)
		if err := f.st.Ledger().InsertCDR(ctx, store.CDR{
			CallID: a.CallID, StartedAt: start, EndedAt: start.Add(30 * time.Second), CallType: "OUTBOUND",
			FromNumber: "95099", ToNumber: a.Number, DID: "95099", Status: status, HangupCause: kind,
			Legs: []store.Leg{{Kind: "TRUNK", Label: "customer"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	get := func(query string) (attemptList, reply) {
		r := f.do(call{method: http.MethodGet, path: "/campaigns/" + c.ID + "/attempts" + query, cookie: admin})
		var l attemptList
		_ = json.Unmarshal(r.body, &l)
		return l, r
	}
	all, r := get("")
	if r.status != http.StatusOK || all.Total != 3 || len(all.Items) != 3 {
		t.Fatalf("attempts: %d %s", r.status, r.body)
	}
	if all.Items[0].CallID != attempts[2].CallID.String() || all.Items[2].CallID != attempts[0].CallID.String() {
		t.Error("attempts are not newest first")
	}
	if a := all.Items[0]; a.State != "DIALING" || a.Outcome != nil || a.FinishedAt != nil || a.IsTouched ||
		a.LegID != attempts[2].LegID.String() || a.ContactID != rows[2].ID.String() {
		t.Errorf("in-flight attempt = %+v", a)
	}
	if a := all.Items[2]; a.State != "FINISHED" || a.Outcome == nil || *a.Outcome != "NO_ANSWER" ||
		!a.IsTouched || a.FinishedAt == nil || a.HangupCause == nil || *a.HangupCause != "NO_ANSWER" || a.TalkSec == nil {
		t.Errorf("finished attempt = %+v", a)
	}

	if l, _ := get("?outcome=BUSY"); l.Total != 1 || l.Items[0].Number != "+8613800000002" {
		t.Errorf("outcome filter = %+v", l)
	}
	if l, _ := get("?contactId=" + rows[0].ID.String()); l.Total != 1 || l.Items[0].Number != "+8613800000001" {
		t.Errorf("contact filter = %+v", l)
	}
	if l, _ := get("?limit=1&offset=1"); len(l.Items) != 1 || l.Total != 3 || l.Items[0].Number != "+8613800000002" {
		t.Errorf("paging = %+v", l)
	}
	_, r = get("?outcome=NOPE")
	wantReply(t, "unknown outcome", r, http.StatusUnprocessableEntity, "VALIDATION_FAILED")
	r = f.do(call{method: http.MethodGet, path: "/campaigns/" + uuid.NewString() + "/attempts", cookie: admin})
	wantReply(t, "unknown campaign", r, http.StatusNotFound, "NOT_FOUND")
}
