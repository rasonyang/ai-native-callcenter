// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// The mock provider's scenario, the flow and the cases are three files that
// must agree; the driver refuses a run when they do not.
func TestProviderScenarioFileCoversEveryScriptedCase(t *testing.T) {
	sc, err := mockprovider.LoadScenario("../../deploy/e2e/provider-scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	flow, err := os.ReadFile("../../deploy/e2e/flow.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckProviderScenarioCoversCases(sc, flow, AllCases); err != nil {
		t.Fatal(err)
	}
	scripted := 0
	for _, cs := range AllCases {
		if cs.Script != "" {
			scripted++
		}
	}
	if scripted != 1 {
		t.Fatalf("%d scripted cases, want 1 (C01)", scripted)
	}
}

func TestProviderScenarioRefusals(t *testing.T) {
	flow := []byte(`{"initialNode":"a","nodes":{"a":{"instruction":{"en":"x E2E script: {slots.userData.e2eScript}.","zh":"y E2E script: {slots.userData.e2eScript}."}}}}`)
	good := `{"scripts":[
		{"name":"lead","instructionsContain":"E2E script: C01_LEAD_HIGH.","steps":[
			{"op":"CALL_TOOL","toolName":"record_lead","args":{"intent":"HIGH","notes":"e2e C01: the customer asked to be called back"}},
			{"op":"AWAIT_TOOL_RESULT"},{"op":"END_SESSION"}]},
		{"name":"default","steps":[{"op":"SPEAK","durationSec":1}]}]}`
	for name, tc := range map[string]struct {
		scenario, flow string
		want           string // "" passes
	}{
		"good":              {good, string(flow), ""},
		"no marker in flow": {good, `{"initialNode":"a","nodes":{"a":{"instruction":{"en":"x","zh":"y"}}}}`, "entry instruction"},
		"no default": {`{"scripts":[{"name":"lead","instructionsContain":"E2E script: C01_LEAD_HIGH.","steps":[
			{"op":"CALL_TOOL","toolName":"record_lead","args":{"intent":"HIGH","notes":"e2e C01: the customer asked to be called back"}},
			{"op":"AWAIT_TOOL_RESULT"},{"op":"END_SESSION"}]}]}`, string(flow), "default script"},
		"default calls a tool": {`{"scripts":[
			{"name":"lead","instructionsContain":"E2E script: C01_LEAD_HIGH.","steps":[
				{"op":"CALL_TOOL","toolName":"record_lead","args":{"intent":"HIGH","notes":"e2e C01: the customer asked to be called back"}},
				{"op":"AWAIT_TOOL_RESULT"},{"op":"END_SESSION"}]},
			{"name":"default","steps":[{"op":"SPEAK","durationSec":1},{"op":"CALL_TOOL","toolName":"hangup"}]}]}`, string(flow), "neither call a tool"},
		"wrong notes": {strings.Replace(good, "asked to be called back", "said no", 1), string(flow), "does not record_lead"},
		"never ends":  {strings.Replace(good, `{"op":"AWAIT_TOOL_RESULT"},{"op":"END_SESSION"}`, `{"op":"AWAIT_TOOL_RESULT"}`, 1), string(flow), "must end the call"},
	} {
		t.Run(name, func(t *testing.T) {
			sc, err := mockprovider.ParseScenario([]byte(tc.scenario))
			if err != nil {
				t.Fatal(err)
			}
			err = CheckProviderScenarioCoversCases(sc, []byte(tc.flow), AllCases)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// A case whose attempts behave differently needs a simulator that has never
// answered its number; one that only saw other numbers is fine.
func TestCheckSimulatorUnused(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		c04 := Regions[0].DialKey(AnsweredCases[2], form)
		c03 := Regions[0].DialKey(DefaultCases[0], form)
		if err := checkSimulatorUnused(calleesim.Report{Dialogs: []calleesim.DialogRecord{{Number: c03}}}, Regions, AllCases, form); err != nil {
			t.Fatalf("refused a simulator that never saw a stepped number: %v", err)
		}
		err := checkSimulatorUnused(calleesim.Report{Dialogs: []calleesim.DialogRecord{{Number: c04}}}, Regions, AllCases, form)
		if err == nil || !strings.Contains(err.Error(), "US C04") {
			t.Fatalf("got %v, want a refusal naming US C04", err)
		}
	})
}

func TestCheckScenarioRefusesAMismatchedStep(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		s := loadScenarioFor(t, form)
		key := Regions[1].DialKey(AnsweredCases[2], form)
		s.Numbers[key] = s.Numbers[key][:1] // C04 loses its answered attempt
		err := CheckScenarioCoversCases(s, Regions, AllCases, form)
		if err == nil || !strings.Contains(err.Error(), "CN C04") {
			t.Fatalf("got %v, want a refusal naming CN C04", err)
		}
	})
}

func TestParseMetrics(t *testing.T) {
	text := `# HELP aicc_bot_slots x
# TYPE aicc_bot_slots gauge
aicc_bot_slots{otel_scope_name="aicc",otel_scope_schema_url="",otel_scope_version="",state="IN_USE"} 0
aicc_bot_slots{otel_scope_name="aicc",otel_scope_schema_url="",otel_scope_version="",state="RESERVED"} 2
aicc_did_outbound_in_use{did="95000001",otel_scope_name="aicc"} 1
aicc_provider_sessions_started_total{otel_scope_name="aicc",provider="openai"} 7
aicc_campaign_attempts_total{outcome="BUSY",note="a \"quoted\", value"} 3
go_goroutines 19
aicc_bare 4.5 1700000000000
`
	m, err := ParseMetrics(text)
	if err != nil {
		t.Fatal(err)
	}
	if v, n := m.Sum(MetricBotSlots, map[string]string{"state": "RESERVED"}); v != 2 || n != 1 {
		t.Fatalf("RESERVED = %v over %d series", v, n)
	}
	if v, n := m.Sum(MetricBotSlots, nil); v != 2 || n != 2 {
		t.Fatalf("bot slots sum %v over %d series", v, n)
	}
	if v, n := m.Sum(MetricProviderSessions, nil); v != 7 || n != 1 {
		t.Fatalf("sessions %v over %d", v, n)
	}
	got := m.Find(MetricCampaignAttempts, map[string]string{"outcome": "BUSY"})
	if len(got) != 1 || got[0].Labels["note"] != `a "quoted", value` || got[0].Value != 3 {
		t.Fatalf("quoted label: %+v", got)
	}
	if _, ok := got[0].Labels["otel_scope_name"]; ok {
		t.Fatal("the exporter's scope labels are kept")
	}
	if _, ok := m["go_goroutines"]; ok {
		t.Fatal("a family that is not the application's is kept")
	}
	if v, _ := m.Sum("aicc_bare", nil); v != 4.5 {
		t.Fatalf("bare series %v", v)
	}
	if _, n := m.Sum("aicc_absent", nil); n != 0 {
		t.Fatal("an absent family has series")
	}
	if _, err := ParseMetrics(`aicc_x{a="unterminated} 1`); err == nil {
		t.Fatal("an unterminated label value parsed")
	}
}

// fakeRun builds a run's view by hand: one region with C03 (busy twice) and
// C04 (busy, then answered), and the counters given.
func fakeRun(t *testing.T, form phonenum.DialForm) *RunView {
	t.Helper()
	r := Regions[0]
	base := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	dlg := func(key string, at time.Duration, provisional, final int) calleesim.DialogRecord {
		d := calleesim.DialogRecord{Number: key, InviteAt: base.Add(at), Final: &calleesim.FinalRecord{Status: final, SentAt: base.Add(at + time.Second)}}
		if provisional != 0 {
			d.Provisionals = []calleesim.ProvisionalRecord{{Status: provisional}}
		}
		return d
	}
	c03, c04 := DefaultCases[0], AnsweredCases[2]
	k03, k04 := r.DialKey(c03, form), r.DialKey(c04, form)
	busy, answered := api.CampaignOutcome("BUSY"), api.CampaignOutcome("ANSWERED")
	attempt := func(o *api.CampaignOutcome) api.CampaignAttempt {
		end := base.Add(time.Minute)
		return api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), State: attemptFinished, Outcome: o, FinishedAt: &end}
	}
	a03 := []api.CampaignAttempt{attempt(&busy), attempt(&busy)}
	a04 := []api.CampaignAttempt{attempt(&busy), attempt(&answered)}
	cdr := func(a api.CampaignAttempt, bot bool) api.CDR {
		c := api.CDR{CallID: a.CallID}
		if bot {
			c.Legs = []api.Leg{{Kind: "BOT"}}
		}
		return c
	}
	v := &RunView{
		Form:  form,
		Cases: []Case{c03, c04},
		Dialogs: []calleesim.DialogRecord{
			dlg(k03, 0, 180, 486), dlg(k04, 0, 180, 486), dlg(k03, 61*time.Second, 180, 486), dlg(k04, 61*time.Second, 180, 200),
		},
		Regions: []RegionView{{
			Region: r, DID: api.DID{Number: "95000001"},
			Contacts: []ContactView{{Case: c03, Number: r.Number(c03), Attempts: a03}, {Case: c04, Number: r.Number(c04), Attempts: a04}},
			CDRs:     []api.CDR{cdr(a03[0], false), cdr(a03[1], false), cdr(a04[0], false), cdr(a04[1], true)},
		}},
		Provider: ProviderCount{Before: mockprovider.Stats{Total: 5}, After: mockprovider.Stats{Total: 6}},
	}
	v.MetricsBefore, _ = ParseMetrics("aicc_provider_sessions_started_total{provider=\"openai\"} 5\naicc_campaign_attempts_total{outcome=\"BUSY\"} 10\n")
	v.MetricsAfter, _ = ParseMetrics(`aicc_provider_sessions_started_total{provider="openai"} 6
aicc_campaign_attempts_total{outcome="BUSY"} 13
aicc_campaign_attempts_total{outcome="ANSWERED"} 1
aicc_bot_slots{state="IN_USE"} 0
aicc_bot_slots{state="RESERVED"} 0
aicc_did_outbound_in_use{did="95000001"} 0
`)
	return v
}

func run(check func(context.Context, *RunView, *Checks), v *RunView) *Checks {
	var c Checks
	check(context.Background(), v, &c)
	return &c
}

func TestInvariantsHoldOnAConsistentRun(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		for name, check := range map[string]func(context.Context, *RunView, *Checks){
			"sessions": CheckProviderSessions, "no-invite-after-answer": CheckNoInviteAfterAnswer,
			"capacity": CheckCapacityReleased, "finished-once": CheckAttemptsFinishedOnce,
		} {
			if c := run(check, fakeRun(t, form)); c.Failed() || len(c.checks) == 0 {
				t.Errorf("%s: %d checks, failures %v", name, len(c.checks), c.failures)
			}
		}
	})
}

func TestProviderSessionsInvariantFails(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		v := fakeRun(t, form)
		v.Provider.After.Total = 7 // one session more than answered calls: one opened on a ring
		if c := run(CheckProviderSessions, v); !c.Failed() {
			t.Error("an extra mock session passed")
		}
		v = fakeRun(t, form)
		v.MetricsAfter[MetricProviderSessions][0].Value = 5 // the application opened none
		if c := run(CheckProviderSessions, v); !c.Failed() {
			t.Error("a missing application session passed")
		}
		v = fakeRun(t, form)
		v.Regions[0].CDRs[0].Legs = []api.Leg{{Kind: "BOT"}} // a busy call that reached the bot
		if c := run(CheckProviderSessions, v); !c.Failed() {
			t.Error("a bot leg on a call that only rang passed")
		}
		empty := &RunView{Cases: AllCases, Regions: fakeRun(t, form).Regions}
		if c := run(CheckProviderSessions, empty); !c.Failed() {
			t.Error("a run with no dialogs passed")
		}
	})
}

func TestNoInviteAfterAnswerFails(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		v := fakeRun(t, form)
		k04 := v.Dialogs[3].Number
		redial := v.Dialogs[3]
		redial.InviteAt = redial.InviteAt.Add(time.Minute)
		redial.Final = &calleesim.FinalRecord{Status: 486}
		v.Dialogs = append(v.Dialogs, redial)
		if c := run(CheckNoInviteAfterAnswer, v); !c.Failed() {
			t.Errorf("an INVITE to %s after its answer passed", k04)
		}
		v = fakeRun(t, form)
		v.Dialogs = v.Dialogs[:3] // C04 never answered
		if c := run(CheckNoInviteAfterAnswer, v); !c.Failed() {
			t.Error("a run that answered fewer numbers than its cases passed")
		}
	})
}

func TestCapacityInvariantFails(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		v := fakeRun(t, form)
		v.MetricsAfter[MetricBotSlots][0].Value = 1
		if c := run(CheckCapacityReleased, v); !c.Failed() {
			t.Error("a bot slot left in use passed")
		}
		v = fakeRun(t, form)
		delete(v.MetricsAfter, MetricDIDOutboundInUse)
		if c := run(CheckCapacityReleased, v); !c.Failed() {
			t.Error("a DID with no line series passed")
		}
	})
}

func TestFinishedOnceInvariantFails(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		v := fakeRun(t, form)
		v.MetricsAfter[MetricCampaignAttempts][1].Value = 14 // BUSY counted once more than the attempts
		if c := run(CheckAttemptsFinishedOnce, v); !c.Failed() {
			t.Error("a second finish passed")
		}
		v = fakeRun(t, form)
		v.Regions[0].Contacts[1].Attempts[1].State = "DIALING"
		if c := run(CheckAttemptsFinishedOnce, v); !c.Failed() {
			t.Error("an unfinished attempt passed")
		}
	})
}

// The clocks (issue #504). The driver, the callee simulator and the dialer read
// the host clock; an attempt's finishedAt and a contact's nextAttemptAt are
// stamped from the switch's, which drifts. A check that orders one against
// the other passes or fails with the drift. These tests build a run whose
// switch-stamped fields are moved by `sw` and leave everything else alone.

// failureKey is a failure without the timestamps and durations it prints.
func failureKey(f string) string { return strings.SplitN(f, " (", 2)[0] }

func failureKeys(fs []string) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, failureKey(f))
	}
	return out
}

// c10Run2 is C10 US as the second `make e2e-campaign` run on one environment
// recorded it (2026-11-07), the switch's clock 1.135 s ahead of the host's
// and moved a further sw. isEarly lists the number 50 ms before the callee's
// 486 instead of 268 ms after.
func c10Run2(sw time.Duration, isEarly bool) ScenarioResult {
	r := Regions[0]
	cs := caseByID(AllCases, "C10")
	base := time.Date(2026, 11, 7, 1, 28, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	sat := func(ms int) *time.Time { t := at(ms).Add(sw); return &t } // switch-stamped
	did := "95000001"
	rv := RegionView{Region: r, DID: api.DID{Number: did}}
	busy, skipped := api.CampaignOutcome("BUSY"), api.CampaignOutcome(listedSkipOutcome)
	busyCause, dncCause := "USER_BUSY", listedSkipCause
	skippedAt := at(19500)

	a1 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
		Outcome: &busy, HangupCause: &busyCause, StartedAt: at(17850), FinishedAt: sat(20050)}
	a2 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
		Outcome: &skipped, HangupCause: &dncCause, StartedAt: at(19400), FinishedAt: &skippedAt} // the dialer finishes a skip
	campaignID := uuid.Must(uuid.NewV7())
	key := r.DialKey(cs, phonenum.FormE164)
	cdrEnd := sat(20050)
	cdrs := map[string][]api.CDR{a1.CallID.String(): {{CallID: a1.CallID, HangupCause: &busyCause, ToNumber: key, DID: &did,
		CallType: "OUTBOUND", CampaignID: &campaignID, EndedAt: *cdrEnd}}}

	posted := at(19183)
	if isEarly {
		posted = at(18865) // 50 ms before the 486
	}
	next := sat(80050)
	lastOutcome := skipped
	af := api.CampaignContact{Number: r.Number(cs), Status: "ACTIVE", Attempts: 1, NextAttemptAt: next, LastOutcome: &busy}
	final := api.CampaignContact{Number: r.Number(cs), Status: "DNC", Attempts: 1, LastOutcome: &lastOutcome}
	cv := ContactView{Case: cs, Number: r.Number(cs), Attempts: []api.CampaignAttempt{a1, a2}, AfterFirst: &af, Contact: &final,
		Listing: &Listing{PostedAt: posted, ListedAt: posted.Add(7 * time.Millisecond), ConfirmedAt: posted.Add(17 * time.Millisecond),
			IsOnList: true, NextAttemptAt: *next}}
	dlg := calleesim.DialogRecord{Number: key, InviteAt: at(17900), InviteHeaders: map[string]string{"from": "<sip:" + did + "@10.0.0.11>"},
		Provisionals: []calleesim.ProvisionalRecord{{Status: 180, SentAt: at(18115)}}, Final: &calleesim.FinalRecord{Status: 486, SentAt: at(18915)}}
	// The control, C03, is busy the same way and dialled again a minute later.
	ctl1 := calleesim.DialogRecord{InviteAt: at(17900), Final: &calleesim.FinalRecord{Status: 486, SentAt: at(18900)}}
	ctl2 := calleesim.DialogRecord{InviteAt: at(79000), Final: &calleesim.FinalRecord{Status: 486, SentAt: at(79900)}}
	return assessListedMidRun(rv, cv, key, []calleesim.DialogRecord{dlg}, []calleesim.DialogRecord{ctl1, ctl2}, cdrs, at(90000))
}

func TestListedMidRunIsHeldToTheCalleesClock(t *testing.T) {
	res := c10Run2(0, false)
	if !res.Passed || len(res.Failures) != 0 {
		t.Fatalf("run 2's data failed: %v", res.Failures)
	}
	if got := res.Attempts[0].SwitchOffsetMs; got == nil || *got != 1135 {
		t.Fatalf("switch offset %v, want 1135 ms (finishedAt 20.050 minus the 486 at 18.915)", got)
	}
	if res.Attempts[1].SwitchOffsetMs != nil {
		t.Fatal("the skipped attempt, which has no dialog, has a switch offset")
	}
	early := c10Run2(0, true)
	if early.Passed || len(early.Failures) != 1 || !strings.Contains(early.Failures[0], "listed at or after the callee's final response") {
		t.Fatalf("a listing 50 ms before the 486: passed %v, failures %v", early.Passed, early.Failures)
	}
}

// The switch's clock, ahead or behind by 10 s, changes nothing: same verdict,
// same checks, same failing check.
func TestListedMidRunDoesNotDependOnTheSwitchClock(t *testing.T) {
	for _, isEarly := range []bool{false, true} {
		want := c10Run2(0, isEarly)
		for _, sw := range []time.Duration{10 * time.Second, -10 * time.Second} {
			got := c10Run2(sw, isEarly)
			if got.Passed != want.Passed || len(got.Checks) != len(want.Checks) || len(got.Failures) != len(want.Failures) ||
				strings.Join(failureKeys(got.Failures), "\n") != strings.Join(failureKeys(want.Failures), "\n") {
				t.Errorf("early=%v switch %+v: passed %v, %d checks, failures %q; unshifted: passed %v, %d checks, failures %q",
					isEarly, sw, got.Passed, len(got.Checks), got.Failures, want.Passed, len(want.Checks), want.Failures)
			}
		}
	}
}

// Without the callee's final response there is no clock to anchor on: the
// checks that need it fail, they are not skipped.
func TestListedMidRunWithoutAFinalResponseFails(t *testing.T) {
	r := Regions[0]
	cs := caseByID(AllCases, "C10")
	now := time.Date(2026, 11, 7, 1, 28, 0, 0, time.UTC)
	cv := ContactView{Case: cs, Number: r.Number(cs), Listing: &Listing{PostedAt: now, ListedAt: now, ConfirmedAt: now, IsOnList: true}}
	res := assessListedMidRun(RegionView{Region: r}, cv, "k", []calleesim.DialogRecord{{InviteAt: now}}, nil, nil, now)
	for _, want := range []string{"listed at or after the callee's final response", "listed before the retry came due", "control's second INVITE", "read after the retry came due"} {
		found := false
		for _, f := range res.Failures {
			found = found || strings.Contains(f, want)
		}
		if !found {
			t.Errorf("no failure names %q: %q", want, res.Failures)
		}
	}
}

// A check that reads the switch's clock says so, and the others do not move
// with it: on a shifted switch clock every failure carries the marker, and
// the checks made are the same.
func requireOnlySwitchTimeMoves(t *testing.T, run func(sw time.Duration) ScenarioResult) {
	t.Helper()
	want := run(0)
	if !want.Passed {
		t.Fatalf("the unshifted run failed: %v", want.Failures)
	}
	for _, sw := range []time.Duration{10 * time.Second, -10 * time.Second} {
		got := run(sw)
		if len(got.Checks) != len(want.Checks) {
			t.Errorf("switch %+v: %d checks, want %d", sw, len(got.Checks), len(want.Checks))
		}
		for _, f := range got.Failures {
			if !strings.HasSuffix(f, switchTime) {
				t.Errorf("switch %+v: a check that is not marked as switch time failed: %s", sw, f)
			}
		}
	}
}

func TestSteppedContactReadsTheSwitchClockOnlyWhereMarked(t *testing.T) {
	requireOnlySwitchTimeMoves(t, func(sw time.Duration) ScenarioResult {
		r := Regions[0]
		cs := caseByID(AllCases, "C04")
		base := time.Date(2026, 11, 7, 1, 30, 0, 0, time.UTC)
		at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
		sat := func(ms int) *time.Time { t := at(ms).Add(sw); return &t }
		did := "95000001"
		rv := RegionView{Region: r, DID: api.DID{Number: did}}
		busy, answered := api.CampaignOutcome("BUSY"), api.CampaignOutcome("ANSWERED")
		busyCause, normal := "USER_BUSY", "NORMAL_CLEARING"
		a1 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
			Outcome: &busy, HangupCause: &busyCause, StartedAt: at(100), FinishedAt: sat(2285)}
		a2 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
			Outcome: &answered, HangupCause: &normal, StartedAt: at(62300), FinishedAt: sat(66800)}
		key := r.DialKey(cs, phonenum.FormE164)
		answeredAt := sat(62600)
		cdrs := map[string][]api.CDR{
			a1.CallID.String(): {{CallID: a1.CallID, HangupCause: &busyCause, ToNumber: key, DID: &did, CallType: "OUTBOUND", EndedAt: *sat(2285)}},
			a2.CallID.String(): {{CallID: a2.CallID, HangupCause: &normal, ToNumber: key, DID: &did, CallType: "OUTBOUND", EndedAt: *sat(66800),
				AnsweredAt: answeredAt, Legs: []api.Leg{{Kind: "BOT"}}}},
		}
		af := api.CampaignContact{Number: r.Number(cs), Status: "ACTIVE", Attempts: 1, NextAttemptAt: sat(62285), LastOutcome: &busy}
		final := api.CampaignContact{Number: r.Number(cs), Status: "DONE", Attempts: 2, LastOutcome: &answered}
		cv := ContactView{Case: cs, Number: r.Number(cs), Attempts: []api.CampaignAttempt{a1, a2}, AfterFirst: &af, Contact: &final}
		from := map[string]string{"from": "<sip:" + did + "@10.0.0.11>"}
		d1 := calleesim.DialogRecord{Number: key, InviteAt: at(150), InviteHeaders: from,
			Provisionals: []calleesim.ProvisionalRecord{{Status: 180}}, Final: &calleesim.FinalRecord{Status: 486, SentAt: at(1150)}}
		ackAt := at(62650)
		d2 := calleesim.DialogRecord{Number: key, InviteAt: at(62350), InviteHeaders: from,
			Provisionals: []calleesim.ProvisionalRecord{{Status: 180}}, Final: &calleesim.FinalRecord{Status: 200, SentAt: at(62600)},
			AckAt: &ackAt, End: &calleesim.EndRecord{Method: calleesim.EndedWithBye, By: calleesim.EndedByCallee, At: at(65650)},
			Downlink: calleesim.Downlink{Energy: []calleesim.EnergyBucket{{PeakRMS: 3000}}}}
		return assessSteppedContact(rv, cv, key, []calleesim.DialogRecord{d1, d2}, cdrs, nil, at(130000))
	})
}

func TestRetriedContactReadsTheSwitchClockOnlyWhereMarked(t *testing.T) {
	requireOnlySwitchTimeMoves(t, func(sw time.Duration) ScenarioResult {
		r := Regions[0]
		cs := caseByID(AllCases, "C03")
		base := time.Date(2026, 11, 7, 1, 31, 0, 0, time.UTC)
		at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
		sat := func(ms int) *time.Time { t := at(ms).Add(sw); return &t }
		did := "95000001"
		rv := RegionView{Region: r, DID: api.DID{Number: did}}
		busy := api.CampaignOutcome("BUSY")
		cause := "USER_BUSY"
		a1 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
			Outcome: &busy, HangupCause: &cause, StartedAt: at(100), FinishedAt: sat(2285)}
		a2 := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
			Outcome: &busy, HangupCause: &cause, StartedAt: at(62300), FinishedAt: sat(63500)}
		key := r.DialKey(cs, phonenum.FormE164)
		cdr := func(a api.CampaignAttempt, end int) []api.CDR {
			return []api.CDR{{CallID: a.CallID, HangupCause: &cause, ToNumber: key, DID: &did, CallType: "OUTBOUND", EndedAt: *sat(end)}}
		}
		cdrs := map[string][]api.CDR{a1.CallID.String(): cdr(a1, 2285), a2.CallID.String(): cdr(a2, 63500)}
		af := api.CampaignContact{Number: r.Number(cs), Status: "ACTIVE", Attempts: 1, NextAttemptAt: sat(62285), LastOutcome: &busy}
		final := api.CampaignContact{Number: r.Number(cs), Status: "EXHAUSTED", Attempts: 2, LastOutcome: &busy}
		cv := ContactView{Case: cs, Number: r.Number(cs), Attempts: []api.CampaignAttempt{a1, a2}, AfterFirst: &af, Contact: &final}
		from := map[string]string{"from": "<sip:" + did + "@10.0.0.11>"}
		dlg := func(inv, fin int) calleesim.DialogRecord {
			return calleesim.DialogRecord{Number: key, InviteAt: at(inv), InviteHeaders: from, Provisionals: []calleesim.ProvisionalRecord{{Status: 180}},
				Final: &calleesim.FinalRecord{Status: 486, SentAt: at(fin)}}
		}
		var findings []string
		return assessContact(rv, cv, key, []calleesim.DialogRecord{dlg(150, 1150), dlg(62350, 63350)}, cdrs, at(130000), &findings)
	})
}
