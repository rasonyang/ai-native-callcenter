// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// The simulator's scenario must play what K1 and K2 expect, and their
// numbers must be the harness's own: inside the region's range (the
// fictional US range or the Chinese range reserved for this harness) and
// apart from every campaign case's.
func TestScenarioFileCoversTheCapacityScenarios(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		s := loadScenarioFor(t, form)
		seen := map[string]string{}
		for _, r := range Regions {
			for _, cs := range AllCases {
				seen[r.DialKey(cs, form)] = r.Code + " " + cs.ID
			}
		}
		numbers := 0
		for _, id := range []string{"K1", "K2"} {
			sc := CapacityScenarios[id]
			if err := CheckScenarioCoversCases(s, sc.Regions, sc.Cases, form); err != nil {
				t.Errorf("%s: %v", id, err)
			}
			if err := checkDirectInScenario(s, sc); err != nil {
				t.Errorf("%s: %v", id, err)
			}
			check := func(label, number, prefix string) {
				numbers++
				if !phonenum.IsCanonical(number) || !strings.HasPrefix(number, prefix) || len(number) != len(prefix)+2 {
					t.Errorf("%s: %q is not a number of %sxx", label, number, prefix)
				}
				if prev, dup := seen[number]; dup {
					t.Errorf("%s and %s share %s", label, prev, number)
				}
				seen[number] = label
			}
			for _, r := range sc.Regions {
				for _, cs := range sc.Cases {
					check(id+" "+r.Code+" "+cs.ID, r.Number(cs), r.Prefix)
				}
			}
			for _, ds := range sc.Direct {
				check(id+" direct "+ds.Suffix, ds.Number(), ds.Region.Prefix)
			}
		}
		if numbers != 3+2+12 {
			t.Fatalf("%d capacity numbers, want 17 (K1: 3 contacts and 2 slot holders; K2: 6 contacts in each of 2 regions)", numbers)
		}
	})
}

// K1's arithmetic: three contacts can only ring together with three free
// slots (the dialer never over-dials a campaign without samples), the two
// slot holders leave exactly one, they answer long before the contacts and
// are still up after the last overflow hold, and the contacts answer inside
// the ring timeout (which the switch checks in whole seconds).
func TestK1LeavesExactlyOneSlotForThreeAnswers(t *testing.T) {
	hold, err := time.ParseDuration(k1OverflowHoldStr)
	if err != nil {
		t.Fatal(err)
	}
	if len(K1.Cases) != k1BotSlots || len(K1.Direct) != k1BotSlots-1 || len(K1.Regions) != 1 {
		t.Fatalf("K1 has %d contacts, %d slot holders and %d regions with %d slots; want %d, %d and 1",
			len(K1.Cases), len(K1.Direct), len(K1.Regions), k1BotSlots, k1BotSlots, k1BotSlots-1)
	}
	if K1.AppEnv[EnvBotMaxCalls] != "3" || K1.AppEnv[EnvBotInboundReserve] != "0" {
		t.Fatalf("K1's environment %v", K1.AppEnv)
	}
	if k1HolderAnswerMs+2000 > k1AnswerAfterMs {
		t.Errorf("the slot holders answer at %d ms, too close to the contacts' %d ms", k1HolderAnswerMs, k1AnswerAfterMs)
	}
	if k1HolderAnswerMs+k1HolderByeMs < k1AnswerAfterMs+int(hold.Milliseconds())+3000 {
		t.Errorf("the slot holders hang up at %d ms, too close to the last overflow (%d ms + hold %s)", k1HolderAnswerMs+k1HolderByeMs, k1AnswerAfterMs, hold)
	}
	if k1ByeAfterMs < int(hold.Milliseconds())+2000 {
		t.Errorf("a contact hangs up %d ms after answering, before the platform's overflow hang-up could be told apart", k1ByeAfterMs)
	}
	if k1AnswerAfterMs > (k1RingTimeoutSec-1)*1000-1000 {
		t.Errorf("the contacts answer at %d ms, too close to the %d s ring timeout", k1AnswerAfterMs, k1RingTimeoutSec)
	}
	if K2.LineLimit != *K2.Plan.MaxOutboundChannels || len(K2.Cases) <= 2*K2.LineLimit {
		t.Fatalf("K2 asserts %d lines, provisions %d, with %d contacts", K2.LineLimit, *K2.Plan.MaxOutboundChannels, len(K2.Cases))
	}
}

func TestPeakConcurrent(t *testing.T) {
	base := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	cancelled := func(from, to int) calleesim.DialogRecord {
		return calleesim.DialogRecord{InviteAt: at(from), End: &calleesim.EndRecord{Method: calleesim.EndedWithCancel, At: at(to)}}
	}
	refused := func(from, to int) calleesim.DialogRecord {
		return calleesim.DialogRecord{InviteAt: at(from), Final: &calleesim.FinalRecord{Status: 486, SentAt: at(to)}}
	}
	open := calleesim.DialogRecord{InviteAt: at(0), Final: &calleesim.FinalRecord{Status: 200, SentAt: at(10)}}
	for name, tc := range map[string]struct {
		dialogs       []calleesim.DialogRecord
		peak, unended int
	}{
		"none":                       {nil, 0, 0},
		"one":                        {[]calleesim.DialogRecord{cancelled(0, 8000)}, 1, 0},
		"a line handed straight on":  {[]calleesim.DialogRecord{cancelled(0, 8000), cancelled(8000, 16000)}, 1, 0},
		"two waves of two":           {[]calleesim.DialogRecord{cancelled(0, 8000), cancelled(5, 8005), cancelled(8001, 16000), cancelled(8006, 16010)}, 2, 0},
		"three at once":              {[]calleesim.DialogRecord{cancelled(0, 8000), cancelled(5, 8005), cancelled(10, 8010)}, 3, 0},
		"a refusal ends at its 4xx":  {[]calleesim.DialogRecord{refused(0, 300), cancelled(300, 900), cancelled(301, 900)}, 2, 0},
		"an unended dialog stays up": {[]calleesim.DialogRecord{open, cancelled(9000, 9500)}, 2, 1},
	} {
		t.Run(name, func(t *testing.T) {
			peak, unended := PeakConcurrent(tc.dialogs)
			if peak != tc.peak || unended != tc.unended {
				t.Fatalf("peak %d, unended %d; want %d and %d", peak, unended, tc.peak, tc.unended)
			}
		})
	}
}

// fakeK1 is a K1 run that went as designed: two slot holders answered at
// 300 ms and stayed 12 s, the three contacts answered at 5 s, one reached the
// bot and two were hung up 1.5 s later without a sound.
func fakeK1(t *testing.T, form phonenum.DialForm) *RunView {
	t.Helper()
	r := K1.Regions[0]
	base := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	from := func(did string) map[string]string { return map[string]string{"from": "<sip:" + did + "@10.0.0.11>"} }
	answeredDialog := func(key, did string, invite, answer, end int, by string, peak float64) calleesim.DialogRecord {
		ack := at(answer + 20)
		d := calleesim.DialogRecord{
			Number: key, InviteAt: at(invite), InviteHeaders: from(did),
			Provisionals: []calleesim.ProvisionalRecord{{Status: 180, SentAt: at(invite + 100)}},
			Final:        &calleesim.FinalRecord{Status: 200, SentAt: at(answer)}, AckAt: &ack,
			End: &calleesim.EndRecord{Method: calleesim.EndedWithBye, By: by, At: at(end)},
		}
		if peak > 0 {
			d.Downlink = calleesim.Downlink{Frames: 200, Energy: []calleesim.EnergyBucket{{StartMs: answer - invite + 400, Frames: 5, PeakRMS: peak}}}
		}
		return d
	}
	const campaignDID, directDID = "95000001", "95000002"
	answered, overflow := api.CampaignOutcome("ANSWERED"), api.CampaignOutcome(outcomeOverflow)
	done := api.CampaignContactStatus("DONE")
	campaignID := uuid.Must(uuid.NewV7())
	v := &RunView{Form: form, Cases: K1.Cases}
	rv := RegionView{Region: r, DID: api.DID{Number: campaignDID}, Campaign: api.Campaign{ID: campaignID}}
	cause := func(s string) *string { return &s }
	for i, cs := range K1.Cases {
		key := r.DialKey(cs, form)
		outcome, peak, end, by, hc, legs := &overflow, 0.0, 6530, calleesim.EndedByCaller, "OVERFLOW", []api.Leg(nil)
		if i == 0 {
			outcome, peak, end, by, hc, legs = &answered, 5000, 11000, calleesim.EndedByCallee, "NORMAL_CLEARING", []api.Leg{{Kind: "BOT"}}
		}
		v.Dialogs = append(v.Dialogs, answeredDialog(key, campaignDID, i, 5000+i, end, by, peak))
		finished := at(end)
		a := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished, Outcome: outcome, FinishedAt: &finished, StartedAt: at(0)}
		ct := api.CampaignContact{Number: r.Number(cs), Status: done, Attempts: 1}
		rv.Contacts = append(rv.Contacts, ContactView{Case: cs, Number: r.Number(cs), Attempts: []api.CampaignAttempt{a}, Contact: &ct})
		answeredAt := at(5000 + i)
		rv.CDRs = append(rv.CDRs, api.CDR{CallID: a.CallID, HangupCause: cause(hc), AnsweredAt: &answeredAt, Legs: legs, CampaignID: &campaignID})
	}
	v.Regions = []RegionView{rv}
	for i, ds := range K1.Direct {
		dlg := answeredDialog(ds.DialKey(), directDID, 100+i, 300+i, 12300+i, calleesim.EndedByCallee, 5000)
		id := uuid.Must(uuid.NewV7())
		v.Direct = append(v.Direct, DirectCall{Number: ds.Number(), DialKey: ds.DialKey(), DID: directDID, CallID: id.String(), Dialog: &dlg,
			CDRs: []api.CDR{{CallID: id, HangupCause: cause("NORMAL_CLEARING"), Legs: []api.Leg{{Kind: "BOT"}}}}})
	}
	v.Provider = ProviderCount{Before: mockprovider.Stats{Total: 10}, After: mockprovider.Stats{Total: 13}}
	v.MetricsBefore = Metrics{}
	v.MetricsAfter, _ = ParseMetrics(`aicc_provider_sessions_started_total{provider="openai"} 3
aicc_campaign_attempts_total{outcome="ANSWERED"} 1
aicc_campaign_attempts_total{outcome="OVERFLOW"} 2
aicc_bot_slots{state="IN_USE"} 0
aicc_bot_slots{state="RESERVED"} 0
aicc_did_outbound_in_use{did="95000001"} 0
aicc_did_outbound_in_use{did="95000002"} 0
aicc_bot_overflow_total 2
aicc_campaign_overflow_total{campaign="` + campaignID.String() + `"} 2
aicc_bot_overflow_hold_ms_milliseconds_count{outcome="OVERFLOW"} 2
aicc_bot_overflow_hold_ms_milliseconds_sum{outcome="OVERFLOW"} 3002
`)
	return v
}

func overflowChecks(v *RunView) *Checks {
	var c Checks
	AssessOverflow(v, 1500*time.Millisecond, &c)
	return &c
}

func TestAssessOverflowPassesTheDesignedRun(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		v := fakeK1(t, form)
		var c Checks
		contacts, direct := AssessOverflow(v, 1500*time.Millisecond, &c)
		if c.Failed() || len(c.checks) < 20 {
			t.Fatalf("%d checks, failures %v", len(c.checks), c.failures)
		}
		if len(contacts) != 3 || len(direct) != 2 || contacts[1].HoldMs == nil || *contacts[1].HoldMs != 1529 {
			t.Fatalf("rows %+v / %+v", contacts, direct)
		}
	})
}

func TestAssessOverflowFails(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		for name, spoil := range map[string]func(v *RunView){
			"no contact at all": func(v *RunView) { v.Regions[0].Contacts = nil },
			"every contact reached the bot": func(v *RunView) {
				ok := api.CampaignOutcome("ANSWERED")
				for i := range v.Regions[0].Contacts {
					v.Regions[0].Contacts[i].Attempts[0].Outcome = &ok
				}
			},
			"an OVERFLOW leg heard the bot": func(v *RunView) {
				v.Dialogs[1].Downlink = calleesim.Downlink{Frames: 10, Energy: []calleesim.EnergyBucket{{StartMs: 5100, Frames: 5, PeakRMS: 900}}}
			},
			"an OVERFLOW number was dialled again": func(v *RunView) {
				again := v.Dialogs[2]
				again.InviteAt = again.InviteAt.Add(time.Minute)
				v.Dialogs = append(v.Dialogs, again)
			},
			"an OVERFLOW leg was hung up by the callee": func(v *RunView) {
				v.Dialogs[1].End.By = calleesim.EndedByCallee
			},
			"an OVERFLOW CDR has a bot leg": func(v *RunView) {
				v.Regions[0].CDRs[1].Legs = []api.Leg{{Kind: "BOT"}}
			},
			"a slot holder answered after the contacts": func(v *RunView) {
				v.Direct[1].Dialog.Final.SentAt = v.Dialogs[0].Final.SentAt.Add(time.Second)
			},
			"a slot holder hung up before the overflow ended": func(v *RunView) {
				v.Direct[0].Dialog.End.At = v.Dialogs[1].Final.SentAt.Add(500 * time.Millisecond)
			},
			"only one slot holder": func(v *RunView) { v.Direct = v.Direct[:1] },
			"a contact was answered before another rang": func(v *RunView) {
				v.Dialogs[2].InviteAt = v.Dialogs[0].Final.SentAt.Add(time.Millisecond)
			},
			"the application counted no overflow": func(v *RunView) { delete(v.MetricsAfter, MetricBotOverflow) },
			"the ANSWERED leg was silent, so silence proves nothing": func(v *RunView) {
				v.Dialogs[0].Downlink = calleesim.Downlink{}
			},
		} {
			t.Run(name, func(t *testing.T) {
				v := fakeK1(t, form)
				spoil(v)
				if c := overflowChecks(v); !c.Failed() {
					t.Fatalf("passed (%d checks)", len(c.checks))
				}
			})
		}
	})
}

// The invariants of #246, adapted for K1: an OVERFLOW leg is answered and
// opens no session, a direct call opens one, and the run has no call that
// only rings.
func TestInvariantsAdaptToOverflowAndDirectCalls(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		for name, check := range map[string]func(context.Context, *RunView, *Checks){
			"sessions": CheckProviderSessions, "no-invite-after-answer": CheckNoInviteAfterAnswer,
			"capacity": CheckCapacityReleased, "finished-once": CheckAttemptsFinishedOnce,
		} {
			if c := run(check, fakeK1(t, form)); c.Failed() || len(c.checks) == 0 {
				t.Errorf("%s: %d checks, failures %v", name, len(c.checks), c.failures)
			}
		}
		v := fakeK1(t, form)
		v.Provider.After.Total = 15 // a session for each OVERFLOW leg
		if c := run(CheckProviderSessions, v); !c.Failed() {
			t.Error("a session per OVERFLOW leg passed")
		}
		v = fakeK1(t, form)
		v.Regions[0].CDRs[2].Legs = []api.Leg{{Kind: "BOT"}}
		if c := run(CheckProviderSessions, v); !c.Failed() {
			t.Error("an OVERFLOW call with a bot leg passed")
		}
		v = fakeK1(t, form)
		v.Direct = nil // the holders' sessions are then unexplained
		if c := run(CheckProviderSessions, v); !c.Failed() {
			t.Error("sessions without the direct calls that opened them passed")
		}
		v = fakeK1(t, form)
		v.Dialogs[2].Final = &calleesim.FinalRecord{Status: 487, SentAt: v.Dialogs[2].Final.SentAt}
		c := run(CheckProviderSessions, v)
		notApplicable := false
		for _, f := range c.failures {
			notApplicable = notApplicable || strings.HasPrefix(f, "not applicable")
		}
		if !notApplicable {
			t.Errorf("a contact that only rang, in a run whose cases have none, passed the not-applicable check: %v", c.failures)
		}
		v = fakeK1(t, form)
		v.MetricsAfter[MetricDIDOutboundInUse] = v.MetricsAfter[MetricDIDOutboundInUse][:1] // the direct DID's series is gone
		if c := run(CheckCapacityReleased, v); !c.Failed() {
			t.Error("a direct DID without a line series passed")
		}
	})
}

// fakeK2 is one region of K2: six contacts in three waves of two, each
// cancelled at the ring timeout.
func fakeK2(t *testing.T, form phonenum.DialForm) *RunView {
	t.Helper()
	r := K2.Regions[0]
	base := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	const did = "95000003"
	noAnswer, exhausted := api.CampaignOutcome("NO_ANSWER"), api.CampaignContactStatus("EXHAUSTED")
	campaignID := uuid.Must(uuid.NewV7())
	v := &RunView{Form: form, Cases: K2.Cases}
	rv := RegionView{Region: r, DID: api.DID{Number: did}, Campaign: api.Campaign{ID: campaignID}}
	cause := "NO_ANSWER"
	for i, cs := range K2.Cases {
		start := (i / 2) * 7500
		v.Dialogs = append(v.Dialogs, calleesim.DialogRecord{
			Number: r.DialKey(cs, form), InviteAt: at(start + i%2), InviteHeaders: map[string]string{"from": "<sip:" + did + "@x>"},
			Provisionals: []calleesim.ProvisionalRecord{{Status: 180}}, Final: &calleesim.FinalRecord{Status: 487, SentAt: at(start + 7470)},
			End: &calleesim.EndRecord{Method: calleesim.EndedWithCancel, By: calleesim.EndedByCaller, At: at(start + 7468)},
		})
		end := at(start + 7470)
		a := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), State: attemptFinished, Outcome: &noAnswer, FinishedAt: &end, StartedAt: at(start)}
		ct := api.CampaignContact{Number: r.Number(cs), Status: exhausted, Attempts: 1}
		rv.Contacts = append(rv.Contacts, ContactView{Case: cs, Number: r.Number(cs), Attempts: []api.CampaignAttempt{a}, Contact: &ct})
		rv.CDRs = append(rv.CDRs, api.CDR{CallID: a.CallID, HangupCause: &cause, CampaignID: &campaignID})
	}
	v.Regions = []RegionView{rv}
	v.MetricsBefore = Metrics{}
	v.MetricsAfter, _ = ParseMetrics(`aicc_campaign_attempts_total{outcome="NO_ANSWER"} 6
aicc_did_outbound_in_use{did="95000003"} 0
aicc_dialer_admission_blocked_total{reason="DID_LINES"} 4
`)
	return v
}

func lineChecks(v *RunView, gauge float64) *Checks {
	var c Checks
	AssessLineLimit(v, 2, map[string]float64{"95000003": gauge}, 40, map[string]bool{}, &c)
	return &c
}

func TestAssessLineLimit(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		if c := lineChecks(fakeK2(t, form), 2); c.Failed() || len(c.checks) < 30 {
			t.Fatalf("%d checks, failures %v", len(c.checks), c.failures)
		}
		for name, spoil := range map[string]func(v *RunView){
			"three lines at once": func(v *RunView) { v.Dialogs[2].InviteAt = v.Dialogs[0].InviteAt.Add(time.Millisecond) },
			"never reached the limit": func(v *RunView) {
				for i := range v.Dialogs {
					v.Dialogs[i].InviteAt = v.Dialogs[0].InviteAt.Add(time.Duration(i) * 8 * time.Second)
					v.Dialogs[i].End.At = v.Dialogs[i].InviteAt.Add(7 * time.Second)
				}
			},
			"a contact was never attempted": func(v *RunView) {
				v.Regions[0].Contacts[5].Attempts = nil
				v.Dialogs = v.Dialogs[:5]
			},
			"nothing was dialled":                      func(v *RunView) { v.Dialogs = nil },
			"the dialer never named the line limit":    func(v *RunView) { delete(v.MetricsAfter, MetricDialerBlocked) },
			"a dialog that never ended holds its line": func(v *RunView) { v.Dialogs[5].End, v.Dialogs[5].Final = nil, nil },
		} {
			t.Run(name, func(t *testing.T) {
				v := fakeK2(t, form)
				spoil(v)
				if c := lineChecks(v, 2); !c.Failed() {
					t.Fatalf("passed (%d checks)", len(c.checks))
				}
			})
		}
		if c := lineChecks(fakeK2(t, form), 3); !c.Failed() {
			t.Error("a line gauge read above the limit passed")
		}
	})
}

// The invariants of #246, adapted for K2: nothing is answered, no session
// is opened, and the application, started for the run, may not have the
// session or slot series at all.
func TestInvariantsOnARunThatOnlyRings(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		for name, check := range map[string]func(context.Context, *RunView, *Checks){
			"sessions": CheckProviderSessions, "no-invite-after-answer": CheckNoInviteAfterAnswer,
			"capacity": CheckCapacityReleased, "finished-once": CheckAttemptsFinishedOnce,
		} {
			if c := run(check, fakeK2(t, form)); c.Failed() || len(c.checks) == 0 {
				t.Errorf("%s: %d checks, failures %v", name, len(c.checks), c.failures)
			}
		}
		v := fakeK2(t, form)
		v.Provider.After.Total = 1
		if c := run(CheckProviderSessions, v); !c.Failed() {
			t.Error("a session on a run that only rang passed")
		}
		v = fakeK2(t, form)
		v.Dialogs[0].Final = &calleesim.FinalRecord{Status: 200}
		if c := run(CheckNoInviteAfterAnswer, v); !c.Failed() {
			t.Error("an answered number on a run whose cases answer none passed")
		}
		v = fakeK2(t, form)
		v.Dialogs = nil
		if c := run(CheckNoInviteAfterAnswer, v); !c.Failed() {
			t.Error("a run that dialled nothing passed")
		}
		v = fakeK2(t, form)
		v.MetricsAfter, _ = ParseMetrics("aicc_bot_slots{state=\"IN_USE\"} 1\naicc_did_outbound_in_use{did=\"95000003\"} 0\n")
		if c := run(CheckCapacityReleased, v); !c.Failed() {
			t.Error("a slot left in use passed")
		}
	})
}

func TestCheckAppEnv(t *testing.T) {
	compose := func(env map[string]string) []byte {
		cfg := map[string]any{"services": map[string]any{"aicc": map[string]any{"environment": env}}}
		data, _ := json.Marshal(cfg)
		return data
	}
	env, hold, err := checkAppEnv(compose(map[string]string{EnvBotMaxCalls: "3", EnvBotInboundReserve: "0", EnvOverflowHold: "1500ms"}), K1)
	if err != nil || hold != 1500*time.Millisecond || env[EnvBotMaxCalls] != "3" {
		t.Fatalf("K1: %v, hold %s, env %v", err, hold, env)
	}
	if _, _, err := checkAppEnv(compose(map[string]string{EnvBotMaxCalls: "3", EnvBotInboundReserve: "0", EnvOverflowHold: "1500ms"}), K2); err == nil ||
		!strings.Contains(err.Error(), EnvBotMaxCalls) {
		t.Fatalf("K2 on K1's application start: %v", err)
	}
	if _, _, err := checkAppEnv(compose(map[string]string{EnvBotMaxCalls: ""}), K1); err == nil {
		t.Fatal("K1 on the default application start passed")
	}
	_, hold, err = checkAppEnv(compose(map[string]string{EnvBotMaxCalls: "", EnvBotInboundReserve: "", EnvOverflowHold: ""}), K2)
	if err != nil || hold != 1500*time.Millisecond {
		t.Fatalf("K2: %v, hold %s", err, hold)
	}
}
