// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// The simulator's scenario must play what K3 expects, and K3's numbers must be
// the harness's own: inside the US range, apart from every other case's.
func TestScenarioFileCoversK3(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		s := loadScenarioFor(t, form)
		if err := CheckScenarioCoversCases(s, []Region{K3.Region}, K3.CampaignCases(), form); err != nil {
			t.Errorf("R3: %v", err)
		}
		if err := checkRefusalInScenario(s, K3); err != nil {
			t.Error(err)
		}
		seen := map[string]string{}
		for _, r := range Regions {
			for _, cs := range AllCases {
				seen[r.Number(cs)] = r.Code + " " + cs.ID
			}
		}
		for _, id := range []string{"K1", "K2"} {
			sc := CapacityScenarios[id]
			for _, r := range sc.Regions {
				for _, cs := range sc.Cases {
					seen[r.Number(cs)] = id + " " + cs.ID
				}
			}
			for _, ds := range sc.Direct {
				seen[ds.Number()] = id + " direct " + ds.Suffix
			}
		}
		check := func(label, number string) {
			if !phonenum.IsCanonical(number) || !strings.HasPrefix(number, K3.Region.Prefix) || len(number) != len(K3.Region.Prefix)+2 {
				t.Errorf("%s: %q is not a number of %sxx", label, number, K3.Region.Prefix)
			}
			if prev, dup := seen[number]; dup {
				t.Errorf("%s and %s share %s", label, prev, number)
			}
			seen[number] = label
		}
		for _, rc := range K3.Direct {
			check("K3 "+rc.ID, rc.Number(K3.Region))
		}
		check("K3 "+K3.Campaign.ID, K3.Region.Number(K3.Campaign))
	})
}

// K3's timing: the holder answers before anything else and outlasts every
// refused call with room to spare; the platform's hang-up is told apart from
// the callee's scripted one; the bot's range is one pair clear of the
// switch's.
func TestK3HoldsThePairPastTheRefusedCalls(t *testing.T) {
	if (k3RTPLow%2 != 0) || k3RTPHigh != k3RTPLow+1 {
		t.Errorf("the bot's range %d-%d is not exactly one pair (an even port and the next)", k3RTPLow, k3RTPHigh)
	}
	if K3.AppEnv[EnvBotRTPPortLow] != "26000" || K3.AppEnv[EnvBotRTPPortHigh] != "26001" {
		t.Errorf("K3's environment %v", K3.AppEnv)
	}
	roles := map[RefusalRole]int{}
	for _, rc := range K3.Direct {
		roles[rc.Role]++
	}
	if roles[RoleHolder] != 1 || roles[RoleRefused] != 2 || roles[RoleNormal] != 1 || K3.Direct[0].Role != RoleHolder || K3.Direct[len(K3.Direct)-1].Role != RoleNormal {
		t.Fatalf("K3's calls %v, want a holder first, two refused calls, a normal call last", roles)
	}
	// The refused calls are placed after the holder is on the bot (a few
	// seconds at most) and the longest of them, R1, lasts its scripted time.
	const placedByMs = 10_000
	if k3HolderByeMs < placedByMs+k3QueuedByeMs+10_000 {
		t.Errorf("the holder hangs up %d ms after answering; R1 may still be up at %d ms", k3HolderByeMs, placedByMs+k3QueuedByeMs)
	}
	if k3PromptByeMaxMs+2000 > k3StaysUpByeMs {
		t.Errorf("the callee's scripted %d ms is too close to the platform's bound %d ms to tell who hung up", k3StaysUpByeMs, k3PromptByeMaxMs)
	}
	if k3AnswerMs >= (k3RingTimeoutSec-1)*1000 {
		t.Errorf("the callees answer at %d ms, too close to the %d s ring timeout", k3AnswerMs, k3RingTimeoutSec)
	}
	if !K3.Campaign.IsAnswered() || K3.Plan.MaxAttempts != 1 {
		t.Errorf("R3 must be answered once and not redialled: %+v %+v", K3.Campaign, K3.Plan)
	}
}

func TestCheckRTPRanges(t *testing.T) {
	compose := func(env map[string]string) []byte {
		cfg := map[string]any{"services": map[string]any{"freeswitch": map[string]any{"environment": env}}}
		data, _ := json.Marshal(cfg)
		return data
	}
	if got, err := checkRTPRanges(compose(map[string]string{"FS_RTP_START_PORT": "20000", "FS_RTP_END_PORT": "20099"}), k3RTPLow, k3RTPHigh); err != nil || got != "20000-20099" {
		t.Fatalf("%q, %v", got, err)
	}
	if got, err := checkRTPRanges(compose(nil), k3RTPLow, k3RTPHigh); err != nil || got != switchImageRTPRange {
		t.Fatalf("the image's own range: %q, %v", got, err)
	}
	for name, env := range map[string]map[string]string{
		"overlap at the bot's low port": {"FS_RTP_START_PORT": "20000", "FS_RTP_END_PORT": "26000"},
		"switch range around the pair":  {"FS_RTP_START_PORT": "16384", "FS_RTP_END_PORT": "32768"},
		"switch range inside the bot's": {"FS_RTP_START_PORT": "26001", "FS_RTP_END_PORT": "26001"},
		"a port that is not a number":   {"FS_RTP_START_PORT": "x", "FS_RTP_END_PORT": "26001"},
		"an end before the start":       {"FS_RTP_START_PORT": "30000", "FS_RTP_END_PORT": "20000"},
	} {
		if _, err := checkRTPRanges(compose(env), k3RTPLow, k3RTPHigh); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
	if _, err := checkRTPRanges(compose(nil), 0, 0); err == nil {
		t.Error("an empty bot range passed")
	}
}

func TestCheckAppEnvForK3(t *testing.T) {
	compose := func(env map[string]string) []byte {
		cfg := map[string]any{"services": map[string]any{"aicc": map[string]any{"environment": env}}}
		data, _ := json.Marshal(cfg)
		return data
	}
	k3 := CapacityScenario{ID: K3.ID, AppEnv: K3.AppEnv}
	env := map[string]string{EnvBotRTPPortLow: "26000", EnvBotRTPPortHigh: "26001"}
	if got, _, err := checkAppEnv(compose(env), k3); err != nil || got[EnvBotRTPPortHigh] != "26001" {
		t.Fatalf("%v %v", got, err)
	}
	if _, _, err := checkAppEnv(compose(map[string]string{}), k3); err == nil || !strings.Contains(err.Error(), EnvBotRTPPortLow) {
		t.Fatalf("K3 on the default application start: %v", err)
	}
	if _, _, err := checkAppEnv(compose(map[string]string{EnvBotRTPPortLow: "26000", EnvBotRTPPortHigh: "26003"}), k3); err == nil {
		t.Fatal("a two-pair range passed")
	}
	// The other capacity scenarios must run on the default range.
	for _, sc := range []CapacityScenario{K1, K2} {
		bad := map[string]string{EnvBotMaxCalls: sc.AppEnv[EnvBotMaxCalls], EnvBotInboundReserve: sc.AppEnv[EnvBotInboundReserve], EnvOverflowHold: sc.AppEnv[EnvOverflowHold], EnvBotRTPPortLow: "26000", EnvBotRTPPortHigh: "26001"}
		if _, _, err := checkAppEnv(compose(bad), sc); err == nil {
			t.Errorf("%s ran on K3's application start", sc.ID)
		}
	}
}

// fakeK3 is a K3 run that went as designed: the holder (invite at 0) answered
// at 300 ms and stayed 28 s; R1 was queued and its callee hung up 8 s after
// answering; R2 and R3 were hung up by the platform a second or so after
// answering; N1 came after the holder and reached the bot.
func fakeK3(t *testing.T, form phonenum.DialForm) (*RunView, RefusalFacts) {
	t.Helper()
	r := K3.Region
	base := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	dialog := func(key, did string, invite, answer, end int, by string, peak float64) calleesim.DialogRecord {
		ack := at(answer + 20)
		d := calleesim.DialogRecord{
			Number: key, InviteAt: at(invite), InviteHeaders: map[string]string{"from": "<sip:" + did + "@10.0.0.11>"},
			Provisionals: []calleesim.ProvisionalRecord{{Status: 180, SentAt: at(invite + 100)}},
			Final:        &calleesim.FinalRecord{Status: 200, SentAt: at(answer)}, AckAt: &ack,
			End: &calleesim.EndRecord{Method: calleesim.EndedWithBye, By: by, At: at(end)},
		}
		if peak > 0 {
			d.Downlink = calleesim.Downlink{Frames: 200, Energy: []calleesim.EnergyBucket{{StartMs: 900, Frames: 5, PeakRMS: peak}}}
		}
		return d
	}
	str := func(s string) *string { return &s }
	reason := &map[string]interface{}{userDataBotReason: botLegFailed}
	dids := map[string]string{"HOLDER": "95000001", "R1": "95000002", "R2": "95000003", "N1": "95000004"}
	const campaignDID = "95000005"
	queueID := uuid.Must(uuid.NewV7())
	campaignID := uuid.Must(uuid.NewV7())

	type timing struct{ invite, answer, end int }
	times := map[string]timing{
		"HOLDER": {0, 300, 28300}, "R1": {1600, 1900, 9900}, "R2": {1650, 1950, 3200}, "N1": {30000, 30300, 36300},
	}
	v := &RunView{Form: form, Cases: K3.CampaignCases()}
	f := RefusalFacts{QueueID: queueID.String(), QueueExt: "7001", Channels: map[string]string{}}
	placed := map[string]time.Time{"HOLDER": at(-500), "R1": at(1500), "R2": at(1500), "N1": at(29000)}
	for _, rc := range K3.Direct {
		tm := times[rc.ID]
		by, peak := calleesim.EndedByCallee, 0.0
		if rc.ID == "R2" {
			by = calleesim.EndedByCaller
		}
		if rc.ID == "HOLDER" || rc.ID == "N1" {
			peak = 5000
		}
		dlg := dialog(rc.DialKey(r), dids[rc.ID], tm.invite, tm.answer, tm.end, by, peak)
		id := uuid.Must(uuid.NewV7())
		cdr := api.CDR{CallID: id, HangupCause: str("NORMAL_CLEARING"), AnsweredAt: ptr(at(tm.answer))}
		switch rc.ID {
		case "HOLDER", "N1":
			cdr.Legs = []api.Leg{{Kind: "BOT"}}
		case "R1":
			cdr.UserData = reason
			cdr.QueueID = &queueID
		case "R2":
			cdr.UserData = reason
			cdr.HangupCause = str(causeTemporaryFailure)
		}
		v.Direct = append(v.Direct, DirectCall{Number: rc.Number(r), DialKey: rc.DialKey(r), DID: dids[rc.ID], CallID: id.String(),
			Placed: placed[rc.ID], Dialog: &dlg, CDRs: []api.CDR{cdr}, IsExpectedNoBot: rc.Role == RoleRefused})
		f.Channels[id.String()] = "leg-" + rc.ID
	}
	// R3
	cs := K3.Campaign
	key := r.DialKey(cs, form)
	v.Dialogs = []calleesim.DialogRecord{dialog(key, campaignDID, 2200, 2500, 3600, calleesim.EndedByCaller, 0)}
	botFailed := api.CampaignOutcome(outcomeBotFailed)
	finished := at(3700)
	a := api.CampaignAttempt{ID: uuid.Must(uuid.NewV7()), CallID: uuid.Must(uuid.NewV7()), LegID: uuid.Must(uuid.NewV7()), Number: r.Number(cs), State: attemptFinished,
		Outcome: &botFailed, HangupCause: str(causeTemporaryFailure), FinishedAt: &finished, StartedAt: at(2000)}
	ct := api.CampaignContact{Number: r.Number(cs), Status: api.CampaignContactStatus("DONE"), Attempts: 1}
	rv := RegionView{Region: r, DID: api.DID{Number: campaignDID}, Campaign: api.Campaign{ID: campaignID}}
	rv.Contacts = []ContactView{{Case: cs, Number: r.Number(cs), Attempts: []api.CampaignAttempt{a}, Contact: &ct}}
	rv.CDRs = []api.CDR{{CallID: a.CallID, HangupCause: str(causeTemporaryFailure), AnsweredAt: ptr(at(2500)), UserData: reason, CampaignID: &campaignID}}
	v.Regions = []RegionView{rv}
	f.HolderOnBotAt = ptr(at(1000))

	v.Provider = ProviderCount{Before: mockprovider.Stats{Total: 10}, After: mockprovider.Stats{Total: 12}}
	v.MetricsAfter, _ = ParseMetrics(`aicc_provider_sessions_started_total{provider="openai"} 2
aicc_campaign_attempts_total{outcome="BOT_FAILED"} 1
aicc_bot_slots{state="IN_USE"} 0
aicc_bot_slots{state="RESERVED"} 0
aicc_did_outbound_in_use{did="95000001"} 0
aicc_did_outbound_in_use{did="95000002"} 0
aicc_did_outbound_in_use{did="95000003"} 0
aicc_did_outbound_in_use{did="95000004"} 0
aicc_did_outbound_in_use{did="95000005"} 0
`)
	// The probes find the campaign's leg through its attempt, the direct
	// calls' through the channels read while they were up.
	onBothClocksAll(v, &f)
	return v, f
}

func refusalAssess(v *RunView, f RefusalFacts) (*Checks, []RefusalRow, []SwitchProbe) {
	var c Checks
	rows, probes := AssessRefusal(v, K3, f, &c)
	return &c, rows, probes
}

func TestAssessRefusalPassesTheDesignedRun(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		v, f := fakeK3(t, form)
		c, rows, probes := refusalAssess(v, f)
		if c.Failed() || len(c.checks) < 40 {
			t.Fatalf("%d checks, failures %v", len(c.checks), c.failures)
		}
		if len(rows) != 5 || len(probes) != 3 {
			t.Fatalf("rows %d, probes %d", len(rows), len(probes))
		}
		byID := map[string]RefusalRow{}
		for _, r := range rows {
			byID[r.ID] = r
		}
		if r := byID["R1"]; r.HoldMs == nil || *r.HoldMs != 8000 || r.BotReason != botLegFailed || r.QueueID == "" {
			t.Errorf("R1 %+v", r)
		}
		if r := byID["R3"]; r.Outcome != outcomeBotFailed || r.ContactEnd != "DONE" || r.Invites != 1 {
			t.Errorf("R3 %+v", r)
		}
		for _, p := range probes {
			if p.LegID == "" || p.Kind == "" {
				t.Errorf("probe %+v", p)
			}
		}
		if probes[1].Kind != ProbePromptHangup || probes[1].Lang != "zh" || probes[0].QueueExt != "7001" || probes[2].QueueExt != "7001" {
			t.Errorf("probes %+v", probes)
		}
	})
}

func TestAssessRefusalFails(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		for name, spoil := range map[string]func(v *RunView, f *RefusalFacts){
			"a run without its direct calls":   func(v *RunView, _ *RefusalFacts) { v.Direct = nil },
			"the holder never seen on the bot": func(_ *RunView, f *RefusalFacts) { f.HolderOnBotAt = nil },
			"the holder answered after the first refused INVITE": func(v *RunView, _ *RefusalFacts) {
				v.Direct[0].Dialog.Final.SentAt = v.Direct[1].Dialog.InviteAt.Add(time.Second)
			},
			"the holder was on the bot after R1 was placed": func(_ *RunView, f *RefusalFacts) { f.HolderOnBotAt = ptr(f.HolderOnBotAt.Add(time.Minute)) },
			"the holder ended before R1 did": func(v *RunView, _ *RefusalFacts) {
				v.Direct[0].Dialog.End.At = v.Direct[1].Dialog.End.At.Add(-time.Second)
			},
			"the normal call came before the holder ended": func(v *RunView, _ *RefusalFacts) {
				v.Direct[3].Dialog.InviteAt = v.Direct[0].Dialog.End.At.Add(-time.Second)
			},
			"R1 was hung up by the platform": func(v *RunView, _ *RefusalFacts) { v.Direct[1].Dialog.End.By = calleesim.EndedByCaller },
			"R1's callee hung up late": func(v *RunView, _ *RefusalFacts) {
				v.Direct[1].Dialog.End.At = v.Direct[1].Dialog.End.At.Add(3 * time.Second)
			},
			"R1 has a bot leg":    func(v *RunView, _ *RefusalFacts) { v.Direct[1].CDRs[0].Legs = []api.Leg{{Kind: "BOT"}} },
			"R1 has no botReason": func(v *RunView, _ *RefusalFacts) { v.Direct[1].CDRs[0].UserData = nil },
			"R1's botReason is something else": func(v *RunView, _ *RefusalFacts) {
				v.Direct[1].CDRs[0].UserData = &map[string]interface{}{userDataBotReason: "OTHER"}
			},
			"R1's leg id was not read": func(v *RunView, f *RefusalFacts) { delete(f.Channels, v.Direct[1].CallID) },
			"R2 was left up until its callee hung up": func(v *RunView, _ *RefusalFacts) {
				v.Direct[2].Dialog.End.By = calleesim.EndedByCallee
				v.Direct[2].Dialog.End.At = v.Direct[2].Dialog.Final.SentAt.Add(20 * time.Second)
			},
			"R2's hang-up was slow": func(v *RunView, _ *RefusalFacts) {
				v.Direct[2].Dialog.End.At = v.Direct[2].Dialog.Final.SentAt.Add((k3PromptByeMaxMs + 1000) * time.Millisecond)
			},
			"R2's cause is something else": func(v *RunView, _ *RefusalFacts) {
				c := "NORMAL_CLEARING"
				v.Direct[2].CDRs[0].HangupCause = &c
			},
			"R2 reached a queue": func(v *RunView, _ *RefusalFacts) {
				id := uuid.Must(uuid.NewV7())
				v.Direct[2].CDRs[0].QueueID = &id
			},
			"R3 finished ANSWERED": func(v *RunView, _ *RefusalFacts) {
				o := api.CampaignOutcome("ANSWERED")
				v.Regions[0].Contacts[0].Attempts[0].Outcome = &o
			},
			"R3 was dialled again": func(v *RunView, _ *RefusalFacts) {
				v.Dialogs = append(v.Dialogs, v.Dialogs[0])
			},
			"R3's contact is not DONE": func(v *RunView, _ *RefusalFacts) {
				v.Regions[0].Contacts[0].Contact.Status = api.CampaignContactStatus("ACTIVE")
			},
			"R3 reached the queue": func(v *RunView, _ *RefusalFacts) {
				id := uuid.Must(uuid.NewV7())
				v.Regions[0].CDRs[0].QueueID = &id
			},
			"R3 has a bot leg":             func(v *RunView, _ *RefusalFacts) { v.Regions[0].CDRs[0].Legs = []api.Leg{{Kind: "BOT"}} },
			"R3 was hung up by the callee": func(v *RunView, _ *RefusalFacts) { v.Dialogs[0].End.By = calleesim.EndedByCallee },
			"N1 had no bot leg":            func(v *RunView, _ *RefusalFacts) { v.Direct[3].CDRs[0].Legs = nil },
			"N1 carries a botReason": func(v *RunView, _ *RefusalFacts) {
				v.Direct[3].CDRs[0].UserData = &map[string]interface{}{userDataBotReason: botLegFailed}
			},
			"N1 was silent":             func(v *RunView, _ *RefusalFacts) { v.Direct[3].Dialog.Downlink = calleesim.Downlink{} },
			"the holder has no bot leg": func(v *RunView, _ *RefusalFacts) { v.Direct[0].CDRs[0].Legs = nil },
		} {
			t.Run(name, func(t *testing.T) {
				v, f := fakeK3(t, form)
				spoil(v, &f)
				onBothClocksAll(v, &f) // the spoils move the wall-clock stamps
				if c, _, _ := refusalAssess(v, f); !c.Failed() {
					t.Fatalf("passed (%d checks)", len(c.checks))
				}
			})
		}
	})
}

// The invariants of #246, adapted for K3: a BOT_FAILED attempt and a refused
// direct call are answered and open no session; the holder and N1 open one.
func TestInvariantsAdaptToRefusedBotLegs(t *testing.T) {
	forEachForm(t, func(t *testing.T, form phonenum.DialForm) {
		for name, check := range map[string]func(context.Context, *RunView, *Checks){
			"sessions": CheckProviderSessions, "no-invite-after-answer": CheckNoInviteAfterAnswer,
			"capacity": CheckCapacityReleased, "finished-once": CheckAttemptsFinishedOnce,
		} {
			v, _ := fakeK3(t, form)
			if c := run(check, v); c.Failed() || len(c.checks) == 0 {
				t.Errorf("%s: %d checks, failures %v", name, len(c.checks), c.failures)
			}
		}
		for name, spoil := range map[string]func(v *RunView){
			"a session for the BOT_FAILED attempt": func(v *RunView) { v.Provider.After.Total = 13 },
			"a session missing for the holder":     func(v *RunView) { v.Provider.After.Total = 11 },
			"the BOT_FAILED attempt has a bot leg": func(v *RunView) { v.Regions[0].CDRs[0].Legs = []api.Leg{{Kind: "BOT"}} },
			"a refused direct call has a bot leg":  func(v *RunView) { v.Direct[1].CDRs[0].Legs = []api.Leg{{Kind: "BOT"}} },
			"the application counted three sessions": func(v *RunView) {
				v.MetricsAfter, _ = ParseMetrics("aicc_provider_sessions_started_total{provider=\"openai\"} 3\n")
			},
		} {
			v, _ := fakeK3(t, form)
			spoil(v)
			if c := run(CheckProviderSessions, v); !c.Failed() {
				t.Errorf("%s passed", name)
			}
		}
	})
}

const k3Log = `
2026-11-04 08:00:01 [INFO] other-leg-0000 unrelated line
leg-R1 2026-11-04 08:00:02 [DEBUG] mod_dptools_interface.c:100 EXECUTE [depth=0] sofia/external/+12025550161@10.0.0.13 bridge({absolute_codec_string=PCMU}sofia/gateway/aicc_bot/x)
leg-R1 2026-11-04 08:00:02 [NOTICE] Originate Failed. Cause: NORMAL_TEMPORARY_FAILURE
leg-R1 2026-11-04 08:00:02 [DEBUG] EXECUTE [depth=0] sofia/external/+12025550161@10.0.0.13 transfer(7001 XML aicc)
leg-R2 2026-11-04 08:00:02 [NOTICE] Originate Failed. Cause: NORMAL_TEMPORARY_FAILURE
leg-R2 2026-11-04 08:00:02 [DEBUG] EXECUTE [depth=0] sofia/external/+12025550162@10.0.0.13 playback(/usr/local/freeswitch/sounds/aicc/zh/bot-unavailable.wav)
leg-R2 2026-11-04 08:00:02 [DEBUG] EXECUTE [depth=0] sofia/external/+12025550162@10.0.0.13 hangup(NORMAL_TEMPORARY_FAILURE)
leg-R3 2026-11-04 08:00:03 [NOTICE] Originate Failed. Cause: NORMAL_TEMPORARY_FAILURE
leg-R3 2026-11-04 08:00:03 [DEBUG] EXECUTE [depth=0] sofia/external/+12025550163@10.0.0.13 hangup(NORMAL_TEMPORARY_FAILURE)
`

func k3Probes() []SwitchProbe {
	return []SwitchProbe{
		{Case: "R1", LegID: "leg-R1", Kind: ProbeQueueTransfer, QueueExt: "7001"},
		{Case: "R2", LegID: "leg-R2", Kind: ProbePromptHangup, Lang: "zh", QueueExt: "7001"},
		{Case: "R3", LegID: "leg-R3", Kind: ProbeHangupOnly, QueueExt: "7001"},
	}
}

func TestEvaluateSwitchLog(t *testing.T) {
	res := EvaluateSwitchLog(k3Probes(), k3Log)
	if len(res.Checks.Failures) != 0 || len(res.Checks.Checks) < 10 {
		t.Fatalf("%d checks, failures %v", len(res.Checks.Checks), res.Checks.Failures)
	}
	if len(res.Evidence["R1"]) != 1 || !strings.Contains(res.Evidence["R2"][0], "/aicc/zh/bot-unavailable.wav") || len(res.Evidence["R3"]) == 0 {
		t.Errorf("evidence %v", res.Evidence)
	}
	for name, log := range map[string]string{
		"an empty log":                     "",
		"R1 never transferred":             strings.Replace(k3Log, "transfer(7001 XML aicc)", "hangup(NORMAL_TEMPORARY_FAILURE)", 1),
		"R1 was played the prompt":         k3Log + "leg-R1 playback(/x/aicc/en/bot-unavailable.wav)\n",
		"R2's path is not expanded":        strings.Replace(k3Log, "/usr/local/freeswitch/sounds", "${sounds_dir}", 1),
		"R2's path is relative":            strings.Replace(k3Log, "playback(/usr/local/freeswitch/sounds/", "playback(sounds/", 1),
		"R2 was played the English prompt": strings.Replace(k3Log, "/aicc/zh/", "/aicc/en/", 1),
		"R2 was queued":                    k3Log + "leg-R2 transfer(7001 XML aicc)\n",
		"R3 was queued":                    k3Log + "leg-R3 transfer(7001 XML aicc)\n",
		"R3 was played a prompt":           k3Log + "leg-R3 playback(/x/aicc/zh/bot-unavailable.wav)\n",
		"no line for R3's leg":             strings.ReplaceAll(k3Log, "leg-R3", "leg-RX"),
		"R2 was played nothing at all":     strings.Replace(k3Log, "playback(/usr/local/freeswitch/sounds/aicc/zh/bot-unavailable.wav)", "playback(none)", 1),
	} {
		if res := EvaluateSwitchLog(k3Probes(), log); len(res.Checks.Failures) == 0 {
			t.Errorf("%s: passed", name)
		}
	}
	noLeg := k3Probes()
	noLeg[0].LegID = ""
	if res := EvaluateSwitchLog(noLeg, k3Log); len(res.Checks.Failures) == 0 {
		t.Error("a probe with no leg id passed")
	}
}

func TestApplySwitchLogRewritesTheReport(t *testing.T) {
	dir := t.TempDir()
	rep := &RefusalReport{Scenario: "K3", Passed: true, AppEnv: map[string]string{}, Calls: []RefusalRow{}, SwitchProbes: k3Probes(), Failures: []string{}}
	if err := writeRefusalReport(dir, rep); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "freeswitch-file.log")
	if err := os.WriteFile(logPath, []byte(k3Log), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ApplySwitchLog(dir, logPath)
	if err != nil || !got.Passed || got.SwitchLog == nil {
		t.Fatalf("%v %+v", err, got)
	}
	if md, _ := os.ReadFile(filepath.Join(dir, "capacity.md")); !strings.Contains(string(md), "bot-unavailable.wav") {
		t.Errorf("capacity.md lacks the switch log's lines:\n%s", md)
	}
	// A second pass over a bad log replaces the verdict, it does not stack.
	if err := os.WriteFile(logPath, []byte(strings.ReplaceAll(k3Log, "leg-R1", "leg-RX")), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = ApplySwitchLog(dir, logPath)
	if err == nil || got.Passed {
		t.Fatalf("a log without R1 passed: %v", err)
	}
	n := len(got.Failures)
	got, _ = ApplySwitchLog(dir, logPath)
	if len(got.Failures) != n {
		t.Errorf("failures stacked: %d then %d", n, len(got.Failures))
	}
	// A report from a run that stopped early has nothing to judge.
	empty := t.TempDir()
	if err := writeRefusalReport(empty, &RefusalReport{Scenario: "K3", AppEnv: map[string]string{}, Calls: []RefusalRow{}, SwitchProbes: []SwitchProbe{}, Failures: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySwitchLog(empty, logPath); err == nil {
		t.Error("a report without probes passed")
	}
}
