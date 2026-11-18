// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

func TestCarrierTimeoutScenarioCoversTheCase(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		sc := loadScenarioFor(t, form)
		if err := CheckScenarioCoversCases(sc, []Region{CarrierTimeoutRegion}, CarrierTimeoutCases, form); err != nil {
			t.Error(err)
		}
		key := CarrierTimeoutRegion.DialKey(CarrierTimeoutCase, form)
		if err := checkCarrierScenarioTiming(sc, key); err != nil {
			t.Error(err)
		}
	})
}

func TestCheckCarrierScenarioTimingRefusesAnotherScript(t *testing.T) {
	const key = "+12025550191"
	for name, b := range map[string]calleesim.Behaviour{
		"the 180 comes at 100 ms": {Provisionals: []calleesim.Provisional{{Status: 180, AtMs: 100}}, FinalStatus: 408, FinalAfterMs: 25000},
		"the 408 comes at 300 ms": {Provisionals: []calleesim.Provisional{{Status: 180, AtMs: 2000}}, FinalStatus: 408, FinalAfterMs: 300},
		"the final is 480":        {Provisionals: []calleesim.Provisional{{Status: 180, AtMs: 2000}}, FinalStatus: 480, FinalAfterMs: 25000},
	} {
		sc := &calleesim.Scenario{Numbers: map[string][]calleesim.Behaviour{key: {b}}}
		if err := checkCarrierScenarioTiming(sc, key); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := calleesim.Behaviour{Provisionals: []calleesim.Provisional{{Status: 180, AtMs: 2000}}, FinalStatus: 408, FinalAfterMs: 25000}
	if err := checkCarrierScenarioTiming(&calleesim.Scenario{Numbers: map[string][]calleesim.Behaviour{key: {good}}}, key); err != nil {
		t.Error(err)
	}
}

// carrierFixture is a run that passes every check; each case below breaks one
// thing.
func carrierFixture() carrierTimeoutObservation {
	callID, did := uuid.New(), "95123456"
	finished := time.Date(2026, 11, 16, 10, 0, 25, 0, time.UTC)
	next := finished.Add(carrierTimeoutDefaultIntervalSec * time.Second)
	no, active := api.CampaignOutcome("NO_ANSWER"), api.CampaignContactStatusACTIVE
	cause := CarrierTimeoutCause
	att := api.CampaignAttempt{
		CallID: callID, State: attemptFinished, Outcome: &no, HangupCause: &cause,
		StartedAt: finished.Add(-25 * time.Second), FinishedAt: &finished,
	}
	ct := api.CampaignContact{Attempts: 1, Status: active, LastOutcome: &no, NextAttemptAt: &next}
	waiting := api.CampaignActivityWAITINGRETRY
	camp := api.Campaign{RingTimeoutSec: CarrierTimeoutRingSec, RetryPolicy: api.CampaignRetryPolicy{
		MaxAttempts: CarrierTimeoutMaxAttempts, IntervalSec: api.CampaignIntervalSec{"NO_ANSWER": carrierTimeoutDefaultIntervalSec},
	}}
	now := camp
	now.Status, now.Activity = api.CampaignStatusRUNNING, &waiting
	tech := map[string]interface{}{"sipStatus": float64(408), "ringStartedAt": "2026-11-16T10:00:02.020Z"}
	key := "+12025550191"
	cdr := api.CDR{
		CallID: callID, HangupCause: &cause, ToNumber: key, DID: &did, RingSec: 23, Status: api.CDRStatusNOANSWER, Tech: &tech,
	}
	invite := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	dlg := calleesim.DialogRecord{
		Number: key, InviteAt: invite,
		Provisionals: []calleesim.ProvisionalRecord{{Status: 180, SentAt: invite.Add(2 * time.Second), AfterInviteMs: 2001}},
		Final:        &calleesim.FinalRecord{Status: 408, SentAt: invite.Add(25 * time.Second), AfterInviteMs: 25001},
	}
	return carrierTimeoutObservation{
		Key: key, DIDNumber: did, Campaign: camp, CampaignNow: now,
		Attempts: []api.CampaignAttempt{att}, AfterFirst: &ct, Contact: &ct,
		CampaignCDRs: []api.CDR{cdr}, Dialogs: []calleesim.DialogRecord{dlg},
		CancelledStatus: string(api.CampaignStatusCANCELLED), CancelledAttempts: 1, HasCancelledReadBk: true,
	}
}

func TestAssessCarrierTimeout(t *testing.T) {
	chk, m := assessCarrierTimeout(carrierFixture())
	if len(chk.Failures) != 0 {
		t.Fatalf("the passing fixture failed: %v", chk.Failures)
	}
	if len(chk.Checks) < 30 || m.RingSec != 23 || m.SimRingMs != 23000 || m.IntervalSec != carrierTimeoutDefaultIntervalSec {
		t.Errorf("checks %d, measure %+v", len(chk.Checks), m)
	}
	if m.SIPStatus == nil || *m.SIPStatus != 408 || m.RingStartedToFinishedSecs == nil {
		t.Errorf("measure %+v", m)
	}

	for _, tc := range []struct {
		name string
		edit func(o *carrierTimeoutObservation)
		want string
	}{
		{"TRANSIENT outcome", func(o *carrierTimeoutObservation) {
			tr := api.CampaignOutcome("TRANSIENT")
			o.Attempts[0].Outcome = &tr
		}, "outcome is NO_ANSWER"},
		{"a system failure was spent", func(o *carrierTimeoutObservation) { o.AfterFirst.SystemFailures, o.AfterFirst.Attempts = 1, 0 }, "attempts = 1 and systemFailures = 0"},
		{"the contact is exhausted", func(o *carrierTimeoutObservation) {
			o.Contact = &api.CampaignContact{Attempts: 1, Status: api.CampaignContactStatusEXHAUSTED}
		}, "the contact is ACTIVE"},
		{"the interval is 60 s", func(o *carrierTimeoutObservation) {
			n := o.Attempts[0].FinishedAt.Add(time.Minute)
			o.AfterFirst.NextAttemptAt = &n
		}, "NO_ANSWER interval" + ""},
		{"the interval is the harness's", func(o *carrierTimeoutObservation) { o.Campaign.RetryPolicy.IntervalSec["NO_ANSWER"] = 60 }, "product default"},
		{"ringSec is 0", func(o *carrierTimeoutObservation) { o.CampaignCDRs[0].RingSec = 0 }, "ringSec"},
		{"ringSec is 30", func(o *carrierTimeoutObservation) { o.CampaignCDRs[0].RingSec = 30 }, "ringSec"},
		{"no sipStatus", func(o *carrierTimeoutObservation) { delete(*o.CampaignCDRs[0].Tech, "sipStatus") }, "tech.sipStatus"},
		{"sipStatus 487", func(o *carrierTimeoutObservation) { (*o.CampaignCDRs[0].Tech)["sipStatus"] = float64(487) }, "tech.sipStatus"},
		{"no ringStartedAt", func(o *carrierTimeoutObservation) { delete(*o.CampaignCDRs[0].Tech, "ringStartedAt") }, "tech.ringStartedAt is present"},
		{"ringStartedAt without fraction", func(o *carrierTimeoutObservation) {
			(*o.CampaignCDRs[0].Tech)["ringStartedAt"] = "2026-11-16T10:00:02Z"
		}, "fractional seconds"},
		{"a CANCEL was seen", func(o *carrierTimeoutObservation) {
			o.Dialogs[0].End = &calleesim.EndRecord{Method: calleesim.EndedWithCancel, By: calleesim.EndedByCaller}
		}, "never ended"},
		{"two INVITEs", func(o *carrierTimeoutObservation) { o.Dialogs = append(o.Dialogs, o.Dialogs[0]) }, "exactly 1 INVITE"},
		{"a second attempt", func(o *carrierTimeoutObservation) { o.Attempts = append(o.Attempts, o.Attempts[0]) }, "1 attempt"},
		{"the simulator played another ring", func(o *carrierTimeoutObservation) { o.Dialogs[0].Final.AfterInviteMs = 5000 }, "scenario's 23 s"},
		{"the campaign is still running", func(o *carrierTimeoutObservation) { o.CancelledStatus = "RUNNING" }, "cancelled"},
		{"no CDR", func(o *carrierTimeoutObservation) { o.CampaignCDRs = nil }, "exactly one CDR"},
	} {
		o := carrierFixture()
		tc.edit(&o)
		chk, _ := assessCarrierTimeout(o)
		found := false
		for _, f := range chk.Failures {
			found = found || strings.Contains(f, tc.want)
		}
		if !found {
			t.Errorf("%s: no failure names %q: %q", tc.name, tc.want, chk.Failures)
		}
	}
}

// The interval is read on the switch's clock, so the check says so.
func TestCarrierTimeoutIntervalIsMarkedSwitchTime(t *testing.T) {
	o := carrierFixture()
	n := o.Attempts[0].FinishedAt.Add(time.Minute)
	o.AfterFirst.NextAttemptAt = &n
	chk, _ := assessCarrierTimeout(o)
	for _, f := range chk.Failures {
		if strings.Contains(f, "NO_ANSWER interval (measured") && !strings.HasSuffix(f, switchTime) {
			t.Errorf("the interval check is not marked as switch time: %s", f)
		}
	}
	if len(chk.Failures) == 0 {
		t.Error("a 60 s interval passed")
	}
}

func TestExpectedOutcome(t *testing.T) {
	dlg := func(final int, prov ...int) *calleesim.DialogRecord {
		d := &calleesim.DialogRecord{Final: &calleesim.FinalRecord{Status: final}}
		for _, p := range prov {
			d.Provisionals = append(d.Provisionals, calleesim.ProvisionalRecord{Status: p})
		}
		return d
	}
	cancelled := func(d *calleesim.DialogRecord) *calleesim.DialogRecord {
		d.End = &calleesim.EndRecord{Method: calleesim.EndedWithCancel, By: calleesim.EndedByCaller}
		return d
	}
	for _, tc := range []struct {
		name  string
		cause string
		d     *calleesim.DialogRecord
		want  string
		ok    bool
	}{
		{"408 after 180", causeRingTimer, dlg(408, 180), "NO_ANSWER", true},
		{"408 after 183", causeRingTimer, dlg(408, 183), "NO_ANSWER", true},
		{"408 with no provisional", causeRingTimer, dlg(408), "TRANSIENT", true},
		{"a timer expiry that is not a 408", causeRingTimer, dlg(504, 180), "TRANSIENT", true},
		{"no dialog to judge by", causeRingTimer, nil, "", false},
		{"the far end's own 487 after 180", causeFarEndTerminated, dlg(487, 180), "NO_ANSWER", true},
		{"the far end's own 487 after 183", causeFarEndTerminated, dlg(487, 183), "NO_ANSWER", true},
		{"the far end's own 487 with no provisional", causeFarEndTerminated, dlg(487), "TRANSIENT", true},
		{"a 487 that answered our CANCEL", causeFarEndTerminated, cancelled(dlg(487, 180)), "TRANSIENT", true},
		{"an ORIGINATOR_CANCEL that is not a 487", causeFarEndTerminated, dlg(480, 180), "TRANSIENT", true},
		{"no dialog to judge the 487 by", causeFarEndTerminated, nil, "", false},
		{"busy", "USER_BUSY", dlg(486, 180), "BUSY", true},
		{"a cause the design does not list", "BEARER_CAPABILITY_NOT_AUTHORIZED", dlg(488), "", false},
	} {
		got, rule, ok := expectedOutcome(tc.cause, tc.d)
		if got != tc.want || ok != tc.ok || (ok && rule == "") {
			t.Errorf("%s: got %q (%q, %v), want %q (%v)", tc.name, got, rule, ok, tc.want, tc.ok)
		}
	}
}
