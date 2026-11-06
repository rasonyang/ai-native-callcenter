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
