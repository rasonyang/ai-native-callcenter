// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// campaignScopes are what the campaign driver's API key may do: provision a
// flow and DIDs, keep the do-not-call list, run campaigns, read the ledger and
// read the callback queue (a HIGH lead files a callback). Nothing else.
var campaignScopes = []string{
	"config:read", "config:write", "campaigns:read", "campaigns:write", "dnc:read", "dnc:write", "history:read:all",
	"calls:read:own",
}

// CampaignConfig is everything the campaign driver needs to know about its
// environment.
type CampaignConfig struct {
	// API is the application's REST base; ReadyURL its readiness endpoint.
	API, ReadyURL string
	// CalleeSim is the simulator's report base; MockProvider the mock
	// provider's HTTP base, for its session counters.
	CalleeSim, MockProvider string
	// MetricsURL is the application's metrics exposition (the ops listener's
	// GET /metrics), for the run-wide invariants.
	MetricsURL string
	// ESLAddr and ESLPassword reach the switch, for the gateway check.
	ESLAddr, ESLPassword string
	// ComposeConfig is the resolved compose configuration (JSON): the guard
	// reads it, and so does the driver, for the deployment's dial form.
	ComposeConfig string
	// ScenarioFile is the simulator's scenario; every case must be in it.
	ScenarioFile string
	// ProviderScenarioFile is the mock provider's scenario: every scripted
	// case must get its script from it, and every other call the default.
	ProviderScenarioFile string

	Username, Password string
	FlowFile           string

	// OutDir receives report.json, probes.md and bundles/.
	OutDir string
	// Wait bounds each phase: the first attempt of every contact, and every
	// contact settling.
	Wait time.Duration

	// Cases and Regions default to AllCases and Regions.
	Cases   []Case
	Regions []Region
	// AfterRun are checked once the run is over; nil means AfterRunChecks.
	AfterRun []AfterRunCheck
}

// RunCampaigns runs the scenarios and probes: one campaign per region, each
// holding every case, both started together. It writes the report to OutDir
// and returns it, with an error when the run was refused, could not proceed,
// or an assertion failed.
func RunCampaigns(ctx context.Context, cfg CampaignConfig) (*CampaignReport, error) {
	if len(cfg.Cases) == 0 {
		cfg.Cases = AllCases
	}
	if len(cfg.Regions) == 0 {
		cfg.Regions = Regions
	}
	if cfg.AfterRun == nil {
		cfg.AfterRun = AfterRunChecks
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}

	pre, err := preflight(ctx, cfg, "e2e-campaign", campaignScopes)
	if err != nil {
		return nil, err
	}
	c, form := pre.c, pre.form

	rep := &CampaignReport{StartedAt: time.Now().UTC(), DialForm: string(form), Latency: map[string]float64{}}
	d := &driver{cfg: cfg, c: c, form: form, rep: rep, plan: defaultPlan}
	runErr := d.run(ctx)

	rep.FinishedAt = time.Now().UTC()
	rep.DurationSec = rep.FinishedAt.Sub(rep.StartedAt).Seconds()
	rep.Failures = collectFailures(rep)
	if runErr != nil {
		rep.Failures = append(rep.Failures, runIncomplete+runErr.Error())
	}
	rep.Passed = len(rep.Failures) == 0
	if err := writeBundles(cfg.OutDir, rep); err != nil && runErr == nil {
		runErr = err
	}
	if err := writeJSON(filepath.Join(cfg.OutDir, "report.json"), rep); err != nil && runErr == nil {
		runErr = err
	}
	if err := os.WriteFile(filepath.Join(cfg.OutDir, "probes.md"), []byte(ProbeTable(rep.Probes)+FirstAudioTable(rep.FirstAudio)+SwitchClockTable(rep.Scenarios)), 0o644); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil {
		return rep, runErr
	}
	if !rep.Passed {
		return rep, fmt.Errorf("%d assertion(s) failed: %s", len(rep.Failures), strings.Join(rep.Failures, "; "))
	}
	return rep, nil
}

// prepared is what preflight leaves a run: the signed-in client, the
// deployment's dial form, the resolved compose configuration and the
// simulator's scenario.
type prepared struct {
	c           *httpc
	form        phonenum.DialForm
	composeJSON []byte
	scenario    *calleesim.Scenario
}

// preflight refuses a run before anything is sent: the configuration must be
// the isolated environment, the simulator's and the mock provider's scenarios
// must play what cfg's cases expect, and the switch must hold only the
// harness's gateways. Then it waits for the application and signs in with a
// key holding scopes.
func preflight(ctx context.Context, cfg CampaignConfig, keyName string, scopes []string) (*prepared, error) {
	composeJSON, err := os.ReadFile(cfg.ComposeConfig)
	if err != nil {
		return nil, fmt.Errorf("read compose config: %w", err)
	}
	if err := CheckComposeConfig(composeJSON); err != nil {
		return nil, err
	}
	rawForm, err := appSetting(composeJSON, "AICC_OUTBOUND_DIAL_FORM")
	if err != nil {
		return nil, err
	}
	form, err := phonenum.ParseDialForm(rawForm)
	if err != nil {
		return nil, err
	}
	scenario, err := calleesim.LoadScenario(cfg.ScenarioFile)
	if err != nil {
		return nil, err
	}
	if err := CheckScenarioCoversCases(scenario, cfg.Regions, cfg.Cases, form); err != nil {
		return nil, err
	}
	providerScenario, err := mockprovider.LoadScenario(cfg.ProviderScenarioFile)
	if err != nil {
		return nil, err
	}
	flowSpec, err := os.ReadFile(cfg.FlowFile)
	if err != nil {
		return nil, err
	}
	if err := CheckProviderScenarioCoversCases(providerScenario, flowSpec, cfg.Cases); err != nil {
		return nil, err
	}
	if err := checkSwitch(ctx, cfg.ESLAddr, cfg.ESLPassword); err != nil {
		return nil, err
	}

	c := &httpc{base: cfg.API, client: newClient()}
	if err := waitReady(ctx, c.client, cfg.ReadyURL, cfg.Wait); err != nil {
		return nil, err
	}
	if err := c.signInWithKey(ctx, cfg.Username, cfg.Password, keyName, scopes); err != nil {
		return nil, err
	}
	return &prepared{c: c, form: form, composeJSON: composeJSON, scenario: scenario}, nil
}

// runIncomplete prefixes the failure of a run that stopped before its
// assertions.
const runIncomplete = "the run did not complete: "

// appSetting is one variable of the aicc service's environment in a resolved
// compose configuration; "" when it is not set.
func appSetting(composeJSON []byte, key string) (string, error) {
	var cfg composeConfig
	if err := json.Unmarshal(composeJSON, &cfg); err != nil {
		return "", fmt.Errorf("compose config is not JSON: %w", err)
	}
	app, ok := cfg.Services["aicc"]
	if !ok {
		return "", fmt.Errorf("no aicc service in the compose configuration")
	}
	if v := app.Environment[key]; v != nil {
		return *v, nil
	}
	return "", nil
}

// CheckScenarioCoversCases refuses a run whose simulator would not play what
// the cases expect: every dialled case's key, in the deployment's dial form,
// must be in the scenario with the case's provisional and final response, and
// a do-not-call case must be absent, so dialling it would also show as an
// unexpected dialog. The keys of all regions must be distinct.
func CheckScenarioCoversCases(s *calleesim.Scenario, regions []Region, cases []Case, form phonenum.DialForm) error {
	var problems []string
	seen := map[string]string{}
	for _, r := range regions {
		for _, cs := range cases {
			key := r.DialKey(cs, form)
			label := r.Code + " " + cs.ID
			if prev, dup := seen[key]; dup {
				problems = append(problems, fmt.Sprintf("%s and %s dial the same string %s", prev, label, key))
			}
			seen[key] = label
			list := s.Numbers[key]
			if len(cs.Steps) > 0 {
				problems = append(problems, stepProblems(label, key, cs.Steps, list)...)
				continue
			}
			if cs.IsOnDNC {
				if len(list) > 0 {
					problems = append(problems, fmt.Sprintf("%s (%s) is on the do-not-call list and must not be in the scenario", label, key))
				}
				continue
			}
			if len(list) != 1 {
				problems = append(problems, fmt.Sprintf("%s: the scenario has %d behaviours for %s, want 1", label, len(list), key))
				continue
			}
			b := list[0]
			wantFinal := cs.SIPStatus
			if wantFinal == 0 {
				wantFinal = neverAnsweredStatus
				if b.FinalAfterMs < neverAnsweredAfterMs {
					problems = append(problems, fmt.Sprintf("%s: a never-answered callee must wait %d ms, has %d", label, neverAnsweredAfterMs, b.FinalAfterMs))
				}
			}
			if b.Final() != wantFinal {
				problems = append(problems, fmt.Sprintf("%s: the scenario answers %d, want %d", label, b.Final(), wantFinal))
			}
			if len(b.Provisionals) != 1 || b.Provisionals[0].Status != cs.Provisional || b.Provisionals[0].EarlyMedia != cs.EarlyMedia {
				problems = append(problems, fmt.Sprintf("%s: the scenario's provisionals are %+v, want one %d (early media %v)", label, b.Provisionals, cs.Provisional, cs.EarlyMedia))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the simulator's scenario does not match the campaign cases (AICC_OUTBOUND_DIAL_FORM=%s; its keys must be the numbers in that form, deploy/e2e/README.md \"Dial forms\"): %s", form, strings.Join(problems, "; "))
	}
	return nil
}

// CheckProviderScenarioCoversCases refuses a run whose mock provider would
// not play what the cases expect. The flow's entry instruction must render
// the contact's script slot as ScriptMarker in both languages; a contact with
// no script must get the default script, which speaks and neither calls a
// tool nor ends the call (the callee hangs up, and the smoke hears speech);
// a scripted contact must get its own script, which records the case's lead
// and, when the callee never hangs up, ends the call itself.
func CheckProviderScenarioCoversCases(sc *mockprovider.Scenario, flowSpec []byte, cases []Case) error {
	var problems []string
	var flow struct {
		InitialNode string `json:"initialNode"`
		Nodes       map[string]struct {
			Instruction map[string]string `json:"instruction"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(flowSpec, &flow); err != nil {
		return fmt.Errorf("flow file: %w", err)
	}
	slotRef := fmt.Sprintf(ScriptMarker, "{slots.userData."+ScriptSlot+"}")
	for _, lang := range []string{"en", "zh"} {
		if !strings.Contains(flow.Nodes[flow.InitialNode].Instruction[lang], slotRef) {
			problems = append(problems, fmt.Sprintf("the flow's entry instruction (%s) does not contain %q", lang, slotRef))
		}
	}

	def := sc.Pick(fmt.Sprintf(ScriptMarker, ""))
	switch {
	case def == nil || def.InstructionsContain != "":
		problems = append(problems, "an unscripted call is not given the default script (empty instructionsContain)")
	default:
		if def.Steps[0].Op != mockprovider.OpSpeak {
			problems = append(problems, fmt.Sprintf("the default script %q does not speak first", def.Name))
		}
		for _, st := range def.Steps {
			if st.Op == mockprovider.OpCallTool || st.Op == mockprovider.OpEndSession {
				problems = append(problems, fmt.Sprintf("the default script %q must neither call a tool nor end the call (%s)", def.Name, st.Op))
			}
		}
	}

	for _, cs := range cases {
		if cs.Script == "" {
			if cs.Lead != "" || cs.Callbacks != 0 {
				problems = append(problems, fmt.Sprintf("%s expects a lead but names no script to record it", cs.ID))
			}
			continue
		}
		marker := fmt.Sprintf(ScriptMarker, cs.Script)
		s := sc.Pick(marker)
		if s == nil || s.InstructionsContain != marker {
			problems = append(problems, fmt.Sprintf("%s: no script is selected by exactly %q", cs.ID, marker))
			continue
		}
		problems = append(problems, scriptProblems(cs, s)...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("the mock provider's scenario does not match the campaign cases: %s", strings.Join(problems, "; "))
	}
	return nil
}

// scriptProblems checks that a scripted case's script records its lead and,
// when no attempt's callee hangs up, ends the call.
func scriptProblems(cs Case, s *mockprovider.Script) []string {
	var problems []string
	lead := -1
	for i, st := range s.Steps {
		if st.Op != mockprovider.OpCallTool || st.ToolName != "record_lead" {
			continue
		}
		var args struct {
			Intent string `json:"intent"`
			Notes  string `json:"notes"`
		}
		_ = json.Unmarshal(st.Args, &args)
		if args.Intent == cs.Lead && args.Notes == cs.LeadNotes {
			lead = i
		}
	}
	if cs.Lead != "" && (lead < 0 || lead+1 >= len(s.Steps) || s.Steps[lead+1].Op != mockprovider.OpAwaitToolResult) {
		problems = append(problems, fmt.Sprintf("%s: script %q does not record_lead %s with the case's notes and await the result", cs.ID, s.Name, cs.Lead))
	}
	byCallee := false
	for _, st := range cs.Steps {
		if st.IsAnswered() && st.ByeAfterMs > 0 {
			byCallee = true
		}
	}
	if !byCallee && s.Steps[len(s.Steps)-1].Op != mockprovider.OpEndSession {
		problems = append(problems, fmt.Sprintf("%s: the callee never hangs up, so script %q must end the call (END_SESSION)", cs.ID, s.Name))
	}
	return problems
}

// stepProblems compares a case written attempt by attempt with the
// simulator's behaviours for its number: one behaviour per attempt, each with
// the step's provisional, final response, answer time and hang-up.
func stepProblems(label, key string, steps []Step, list []calleesim.Behaviour) []string {
	if len(list) != len(steps) {
		return []string{fmt.Sprintf("%s: the scenario has %d behaviours for %s, want %d", label, len(list), key, len(steps))}
	}
	var problems []string
	for i, st := range steps {
		b := list[i]
		at := fmt.Sprintf("%s attempt %d", label, i+1)
		if b.Final() != st.SIPStatus {
			problems = append(problems, fmt.Sprintf("%s: the scenario answers %d, want %d", at, b.Final(), st.SIPStatus))
		}
		if st.FinalAfterMs > 0 && b.FinalAfterMs != st.FinalAfterMs {
			problems = append(problems, fmt.Sprintf("%s: the scenario answers after %d ms, want %d", at, b.FinalAfterMs, st.FinalAfterMs))
		}
		switch {
		case st.Provisional == 0 && len(b.Provisionals) != 0:
			problems = append(problems, fmt.Sprintf("%s: the scenario sends provisionals %+v, want none", at, b.Provisionals))
		case st.Provisional != 0 && (len(b.Provisionals) != 1 || b.Provisionals[0].Status != st.Provisional || b.Provisionals[0].EarlyMedia != st.EarlyMedia):
			problems = append(problems, fmt.Sprintf("%s: the scenario's provisionals are %+v, want one %d (early media %v)", at, b.Provisionals, st.Provisional, st.EarlyMedia))
		}
		bye := 0
		if b.ByeAfterMs != nil {
			// A hang-up at 0 ms would read as "never" below.
			bye = max(*b.ByeAfterMs, 1)
		}
		if bye != st.ByeAfterMs {
			problems = append(problems, fmt.Sprintf("%s: the callee hangs up %d ms after answering, want %d (0: never)", at, bye, st.ByeAfterMs))
		}
	}
	return problems
}

// campaignPlan is what the driver provisions for each region: the campaign's
// ring timeout and attempt budget, and the DID's outbound line limit.
type campaignPlan struct {
	RingTimeoutSec int
	MaxAttempts    int
	// MaxOutboundChannels is the DID's maxOutboundChannels; nil means no
	// limit.
	MaxOutboundChannels *int
}

// defaultPlan is the policy of the campaign scenarios and probes.
var defaultPlan = campaignPlan{RingTimeoutSec: RingTimeoutSec, MaxAttempts: MaxAttempts}

// driver is one run.
type driver struct {
	cfg  CampaignConfig
	c    *httpc
	form phonenum.DialForm
	rep  *CampaignReport
	plan campaignPlan

	regions []*regionRun
	dnc     []string
	// calleeBase is how many dialogs the simulator held before the run.
	calleeBase      int
	unexpectedBase  int
	providerBefore  mockprovider.Stats
	providerAfter   mockprovider.Stats
	metricsBefore   Metrics
	metricsAfter    Metrics
	callbacks       []api.Callback
	startedAt       time.Time
	lastObservation time.Time
	// calleeReadAt is when the simulator's final record was read.
	calleeReadAt time.Time
	// onPoll, when set, runs after every successful observation while the
	// driver waits (the capacity runs sample the lines in use with it).
	onPoll func(ctx context.Context)
	// fallbackQueue, when set, is the fallback queue of each region's DID
	// (K3 proves a campaign call never goes to it).
	fallbackQueue *uuid.UUID
}

// regionRun is one campaign of the run as the driver tracks it.
type regionRun struct {
	region   Region
	did      api.DID
	campaign api.Campaign
	// byNumber maps a contact's canonical number to its case.
	byNumber map[string]Case
	// afterFirst is each contact as first seen with exactly one attempt, that
	// one finished: the contact right after its first attempt.
	afterFirst map[string]api.CampaignContact
	contacts   map[string]api.CampaignContact
	attempts   map[string][]api.CampaignAttempt
	cdrs       []api.CDR
	// listings is each mid-run listing the driver made, by canonical number.
	listings map[string]*Listing
}

func (d *driver) run(ctx context.Context) error {
	pre, err := d.simulatorReport(ctx)
	if err != nil {
		return err
	}
	d.calleeBase, d.unexpectedBase = len(pre.Dialogs), pre.UnexpectedCount
	if err := checkSimulatorUnused(pre, d.cfg.Regions, d.cfg.Cases, d.form); err != nil {
		return err
	}
	if d.providerBefore, err = providerStats(ctx, d.c, d.cfg.MockProvider); err != nil {
		return err
	}
	if d.metricsBefore, err = scrapeMetrics(ctx, d.c.client, d.cfg.MetricsURL); err != nil {
		return err
	}

	defer d.removeDNC(context.WithoutCancel(ctx))
	usedDIDs := map[string]bool{}
	for _, r := range d.cfg.Regions {
		rr, err := d.provision(ctx, r, usedDIDs)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Code, err)
		}
		d.regions = append(d.regions, rr)
	}

	// Both campaigns start together: the scenarios run concurrently, never
	// one after another (a retry is at least a minute away).
	d.startedAt = time.Now()
	for _, rr := range d.regions {
		if err := d.c.do(ctx, http.MethodPost, "/campaigns/"+rr.campaign.ID.String()+"/start", nil, nil, http.StatusOK); err != nil {
			return fmt.Errorf("%s: start campaign: %w", rr.region.Code, err)
		}
	}

	if err := d.waitUntil(ctx, "every contact's first attempt to finish", d.firstRoundDone); err != nil {
		return err
	}
	d.rep.Latency["startToFirstRoundMs"] = float64(time.Since(d.startedAt).Milliseconds())
	if err := d.waitUntil(ctx, "every contact to settle", d.settled); err != nil {
		return err
	}
	d.rep.Latency["startToSettledMs"] = float64(time.Since(d.startedAt).Milliseconds())

	// A stray late attempt, INVITE or CDR would land in this window.
	if err := sleep(ctx, 5*time.Second); err != nil {
		return err
	}
	if err := d.observe(ctx); err != nil {
		return err
	}
	var post calleesim.Report
	if err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &post); err != nil {
		return fmt.Errorf("simulator report: %w", err)
	}
	d.calleeReadAt = time.Now().UTC()
	for _, rr := range d.regions {
		if rr.cdrs, err = d.campaignCDRs(ctx, rr.campaign.ID.String()); err != nil {
			return err
		}
	}
	// Every answered dialog reaches a bot here: no case overflows, and the
	// run places no direct call.
	if err := d.settleCounters(ctx, answeredDialogs(post.Dialogs[min(d.calleeBase, len(post.Dialogs)):]), nil); err != nil {
		return err
	}
	// After the attempts were seen finished: a HIGH lead's callback is
	// written in the same transaction as its attempt's finish.
	var callbacks api.CallbackList
	if err := d.c.do(ctx, http.MethodGet, "/callbacks?limit=100000", nil, &callbacks, http.StatusOK); err != nil {
		return fmt.Errorf("list callbacks: %w", err)
	}
	d.callbacks = callbacks.Items

	view := d.view(post)
	assess(d.rep, view, d.cfg.Cases, d.form, d.unexpectedBase)
	for _, chk := range d.cfg.AfterRun {
		var col Checks
		chk.Run(ctx, view, &col)
		d.rep.AfterRun = append(d.rep.AfterRun, AfterRunResult{Name: chk.Name, CheckList: col.list()})
	}
	return nil
}

func (d *driver) provision(ctx context.Context, r Region, usedDIDs map[string]bool) (*regionRun, error) {
	var did api.DID
	for try := 0; ; try++ {
		number := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
		if usedDIDs[number] {
			continue
		}
		var err error
		slug := "e2e-campaign-" + strings.ToLower(r.Code) + "-" + number
		did, err = d.c.publishFlowAndDIDWith(ctx, d.cfg.FlowFile, slug, "e2e campaign "+r.Code+" "+number,
			api.DIDWrite{Number: number, MaxOutboundChannels: d.plan.MaxOutboundChannels, FallbackQueueID: d.fallbackQueue})
		if err == nil {
			usedDIDs[number] = true
			break
		}
		if try == 2 {
			return nil, err
		}
	}

	rr := &regionRun{
		region: r, did: did, byNumber: map[string]Case{},
		afterFirst: map[string]api.CampaignContact{}, listings: map[string]*Listing{},
	}
	for _, cs := range d.cfg.Cases {
		rr.byNumber[r.Number(cs)] = cs
		if !cs.IsOnDNC {
			continue
		}
		n := r.Number(cs)
		// 200 when an earlier run that was cut short left it listed.
		if err := d.c.do(ctx, http.MethodPost, "/dnc-numbers", api.DNCNumberWrite{Number: n}, nil, http.StatusOK, http.StatusCreated); err != nil {
			return nil, fmt.Errorf("add %s to the do-not-call list: %w", n, err)
		}
		d.dnc = append(d.dnc, n)
	}

	maxConcurrent := len(d.cfg.Cases)
	ring := d.plan.RingTimeoutSec
	attempts, system := d.plan.MaxAttempts, MaxSystemRetries
	base, factor, capSec := RetryIntervalSec, 1, RetryIntervalSec
	intervals := api.CampaignIntervalSec{
		"NO_ANSWER": RetryIntervalSec, "BUSY": RetryIntervalSec, "REJECTED": RetryIntervalSec, "UNKNOWN": RetryIntervalSec,
	}
	outcomes := []api.CampaignRetryOutcome{
		api.CampaignRetryOutcomeNOANSWER, api.CampaignRetryOutcomeBUSY, api.CampaignRetryOutcomeREJECTED,
		api.CampaignRetryOutcomeUNKNOWN, api.CampaignRetryOutcomeTRANSIENT, api.CampaignRetryOutcomeORIGINATEFAILED,
	}
	cc := r.CountryCode
	if err := d.c.do(ctx, http.MethodPost, "/campaigns", api.CampaignWrite{
		Name:               "e2e " + r.Code + " " + did.Number,
		DIDID:              did.ID,
		TimeZone:           r.TimeZone,
		CallingWindow:      api.CampaignCallingWindow{Days: allDays, Ranges: []api.CampaignTimeRange{{Start: "00:00", End: "24:00"}}},
		MaxConcurrent:      &maxConcurrent,
		RingTimeoutSec:     &ring,
		DefaultCountryCode: &cc,
		RetryPolicy: &api.CampaignRetryPolicyWrite{
			MaxAttempts: &attempts, MaxSystemRetries: &system, RetryOutcomes: &outcomes, IntervalSec: &intervals,
			TransientBackoff: &api.CampaignBackoffWrite{BaseSec: &base, Factor: &factor, CapSec: &capSec},
		},
	}, &rr.campaign, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}

	rows := make([]api.CampaignContactRow, 0, len(d.cfg.Cases))
	for _, cs := range d.cfg.Cases {
		row := api.CampaignContactRow{Number: r.Number(cs)}
		if cs.Script != "" {
			// Business data the flow's instruction renders, which is how the
			// mock provider tells this contact's calls apart (ScriptMarker).
			row.Slots = &map[string]string{ScriptSlot: cs.Script}
		}
		rows = append(rows, row)
	}
	var imp api.CampaignImportResult
	if err := d.c.do(ctx, http.MethodPost, "/campaigns/"+rr.campaign.ID.String()+"/contacts", api.CampaignContactsImport{Contacts: rows}, &imp, http.StatusOK); err != nil {
		return nil, fmt.Errorf("import contacts: %w", err)
	}
	if imp.Imported != len(rows) || len(imp.Rows) != len(rows) || imp.Rejected != 0 || imp.Duplicates != 0 {
		return nil, fmt.Errorf("import: %d imported, %d duplicates, %d rejected of %d rows", imp.Imported, imp.Duplicates, imp.Rejected, len(rows))
	}
	return rr, nil
}

var allDays = []api.Weekday{api.WeekdayMON, api.WeekdayTUE, api.WeekdayWED, api.WeekdayTHU, api.WeekdayFRI, api.WeekdaySAT, api.WeekdaySUN}

// errListing marks a failure to list a number mid-run: unlike a failed
// observation it is not retried, since a listing that half happened would
// leave the case void.
var errListing = errors.New("list a number mid-run")

// listMidRun puts the number of every case with IsListedMidRun on the
// do-not-call list, region by region, as soon as the contact has been seen
// right after its first attempt with a retry due: the number is then listed
// while its contact waits. The list is read back to confirm it. A region
// whose contact is not there yet is left for the next observation, and none
// of this holds the other cases back.
func (d *driver) listMidRun(ctx context.Context) error {
	for _, rr := range d.regions {
		for _, cs := range d.cfg.Cases {
			if !cs.IsListedMidRun {
				continue
			}
			n := rr.region.Number(cs)
			af, seen := rr.afterFirst[n]
			if rr.listings[n] != nil || !seen || af.NextAttemptAt == nil {
				continue
			}
			l := &Listing{NextAttemptAt: *af.NextAttemptAt}
			d.dnc = append(d.dnc, n)
			l.PostedAt = time.Now().UTC()
			if err := d.c.do(ctx, http.MethodPost, "/dnc-numbers", api.DNCNumberWrite{Number: n}, nil, http.StatusOK, http.StatusCreated); err != nil {
				return fmt.Errorf("%w: %s %s: add %s to the do-not-call list: %v", errListing, rr.region.Code, cs.ID, n, err)
			}
			l.ListedAt = time.Now().UTC()
			var list api.DNCNumberList
			if err := d.c.do(ctx, http.MethodGet, "/dnc-numbers?limit=10&prefix="+url.QueryEscape(n), nil, &list, http.StatusOK); err != nil {
				return fmt.Errorf("%w: %s %s: read the do-not-call list back: %v", errListing, rr.region.Code, cs.ID, err)
			}
			for _, item := range list.Items {
				if item.Number == n {
					l.IsOnList = true
				}
			}
			l.ConfirmedAt = time.Now().UTC()
			rr.listings[n] = l
		}
	}
	return nil
}

func (d *driver) removeDNC(ctx context.Context) {
	for _, n := range d.dnc {
		_ = d.c.do(ctx, http.MethodDelete, "/dnc-numbers/"+url.PathEscape(n), nil, nil, http.StatusNoContent, http.StatusNotFound)
	}
}

// counterSettle bounds how long the driver waits for the counters to catch up
// with what the API and the simulator already show; counterConfirm is how
// long it then waits before the reading it keeps, so a late extra increment
// (a second finish, a second session) lands in it.
const (
	counterSettle  = 30 * time.Second
	counterConfirm = 3 * time.Second
)

// settleCounters reads the application's metrics and the mock provider's
// counters for the after-run checks. They move on hand-offs the API does not
// wait for: an attempt is FINISHED in the API when its transaction commits,
// and the dialer counts it afterwards; a bot slot and a DID line are given
// back when the switch reports the hangup; the mock counts a session closed
// when its socket does. So the driver waits, up to counterSettle, until every
// counter the invariants read has caught up (bot slots and the lines of the
// run's DIDs and of extraDIDs at zero, the finished-attempt count at what the
// run shows, both session counts at sessions, no session live), then waits
// counterConfirm more and keeps that reading, whether or not it caught up: the
// checks judge it, and the wait is reported. A series that does not exist
// reads as zero here; the checks decide whether it must exist.
func (d *driver) settleCounters(ctx context.Context, sessions int, extraDIDs []string) error {
	attempts := 0
	for _, rr := range d.regions {
		for _, list := range rr.attempts {
			attempts += len(list)
		}
	}
	caughtUp := func(m Metrics, s mockprovider.Stats) bool {
		for _, state := range []string{"IN_USE", "RESERVED"} {
			if v, n := m.Sum(MetricBotSlots, map[string]string{"state": state}); n > 1 || v != 0 {
				return false
			}
		}
		dids := append([]string{}, extraDIDs...)
		for _, rr := range d.regions {
			dids = append(dids, rr.did.Number)
		}
		for _, did := range dids {
			if v, n := m.Sum(MetricDIDOutboundInUse, map[string]string{"did": did}); n != 1 || v != 0 {
				return false
			}
		}
		before, _ := d.metricsBefore.Sum(MetricCampaignAttempts, nil)
		after, _ := m.Sum(MetricCampaignAttempts, nil)
		sb, _ := d.metricsBefore.Sum(MetricProviderSessions, nil)
		sa, _ := m.Sum(MetricProviderSessions, nil)
		return after-before == float64(attempts) && sa-sb == float64(sessions) &&
			s.Total-d.providerBefore.Total == int64(sessions) && s.Live == 0
	}
	start := time.Now()
	deadline := start.Add(counterSettle)
	for {
		m, err := scrapeMetrics(ctx, d.c.client, d.cfg.MetricsURL)
		if err != nil {
			return err
		}
		s, err := providerStats(ctx, d.c, d.cfg.MockProvider)
		if err != nil {
			return err
		}
		if caughtUp(m, s) || time.Now().After(deadline) {
			break
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	d.rep.Latency["countersSettledMs"] = float64(time.Since(start).Milliseconds())
	if err := sleep(ctx, counterConfirm); err != nil {
		return err
	}
	var err error
	if d.metricsAfter, err = scrapeMetrics(ctx, d.c.client, d.cfg.MetricsURL); err != nil {
		return err
	}
	if d.providerAfter, err = providerStats(ctx, d.c, d.cfg.MockProvider); err != nil {
		return err
	}
	return nil
}

// simulatorReport is the simulator's record, waited for up to the run's bound:
// e2e.sh restarts the simulator before a campaign run (checkSimulatorUnused).
func (d *driver) simulatorReport(ctx context.Context) (calleesim.Report, error) {
	var r calleesim.Report
	deadline := time.Now().Add(d.cfg.Wait)
	for {
		err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &r)
		if err == nil {
			return r, nil
		}
		if time.Now().After(deadline) {
			return r, fmt.Errorf("simulator report: %w", err)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return r, err
		}
	}
}

// checkSimulatorUnused refuses a run in which the simulator would play the
// wrong behaviour: it answers the n-th INVITE for a number with the n-th
// behaviour of the scenario, counting every INVITE since it started, so a
// case whose attempts behave differently (C04: busy, then answered) needs a
// simulator that has never seen its number. e2e.sh restarts it first.
func checkSimulatorUnused(pre calleesim.Report, regions []Region, cases []Case, form phonenum.DialForm) error {
	seen := map[string]int{}
	for _, dlg := range pre.Dialogs {
		seen[dlg.Number]++
	}
	var used []string
	for _, r := range regions {
		for _, cs := range cases {
			if len(cs.Steps) < 2 && !cs.IsListedMidRun {
				continue
			}
			if key := r.DialKey(cs, form); seen[key] > 0 {
				used = append(used, fmt.Sprintf("%s %s (%s, %d INVITEs)", r.Code, cs.ID, key, seen[key]))
			}
		}
	}
	if len(used) > 0 {
		return fmt.Errorf("the simulator has already answered numbers whose attempts behave differently, so it would play the wrong attempt: %s; restart it (deploy/e2e/e2e.sh campaign does)",
			strings.Join(used, ", "))
	}
	return nil
}

func providerStats(ctx context.Context, c *httpc, base string) (mockprovider.Stats, error) {
	var s mockprovider.Stats
	if err := c.getJSON(ctx, base+mockprovider.StatsPath, &s); err != nil {
		return s, fmt.Errorf("mock provider stats: %w", err)
	}
	return s, nil
}

// observe reads both campaigns, their contacts and their attempts once, and
// remembers each contact as it stood right after its first attempt.
func (d *driver) observe(ctx context.Context) error {
	for _, rr := range d.regions {
		id := rr.campaign.ID.String()
		var camp api.Campaign
		if err := d.c.do(ctx, http.MethodGet, "/campaigns/"+id, nil, &camp, http.StatusOK); err != nil {
			return err
		}
		// Attempts before contacts: an attempt's finish and its contact's
		// counters commit in one transaction, so a contact read after its
		// attempt was seen FINISHED includes that finish. Read the other way
		// round, a finish landing between the two reads would pair a finished
		// attempt with the contact as it was before.
		var attempts api.CampaignAttemptList
		if err := d.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/attempts?limit=200", nil, &attempts, http.StatusOK); err != nil {
			return err
		}
		var contacts api.CampaignContactList
		if err := d.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/contacts?limit=200", nil, &contacts, http.StatusOK); err != nil {
			return err
		}
		if contacts.Total != len(contacts.Items) || attempts.Total != len(attempts.Items) {
			return fmt.Errorf("%s: a page did not hold every row (contacts %d of %d, attempts %d of %d)",
				rr.region.Code, len(contacts.Items), contacts.Total, len(attempts.Items), attempts.Total)
		}
		rr.campaign = camp
		rr.contacts = map[string]api.CampaignContact{}
		rr.attempts = map[string][]api.CampaignAttempt{}
		for _, ct := range contacts.Items {
			rr.contacts[ct.Number] = ct
		}
		for _, a := range attempts.Items {
			rr.attempts[a.Number] = append(rr.attempts[a.Number], a)
		}
		for n, list := range rr.attempts {
			sort.SliceStable(list, func(i, j int) bool { return list[i].StartedAt.Before(list[j].StartedAt) })
			if _, seen := rr.afterFirst[n]; !seen && len(list) == 1 && list[0].State == attemptFinished {
				if ct, ok := rr.contacts[n]; ok {
					rr.afterFirst[n] = ct
				}
			}
		}
	}
	d.lastObservation = time.Now().UTC()
	return nil
}

const attemptFinished = api.CampaignAttemptState("FINISHED")

func (d *driver) firstRoundDone() bool {
	for _, rr := range d.regions {
		for n := range rr.byNumber {
			if _, ok := rr.afterFirst[n]; !ok {
				return false
			}
		}
	}
	return true
}

func (d *driver) settled() bool {
	for _, rr := range d.regions {
		for n := range rr.byNumber {
			ct, ok := rr.contacts[n]
			if !ok || ct.Status == api.CampaignContactStatus("ACTIVE") {
				return false
			}
			for _, a := range rr.attempts[n] {
				if a.State != attemptFinished {
					return false
				}
			}
		}
	}
	return true
}

func (d *driver) waitUntil(ctx context.Context, what string, done func() bool) error {
	deadline := time.Now().Add(d.cfg.Wait)
	for {
		err := d.observe(ctx)
		if err == nil {
			err = d.listMidRun(ctx)
		}
		if err == nil && d.onPoll != nil {
			d.onPoll(ctx)
		}
		if err == nil && done() {
			return nil
		}
		if err != nil && errors.Is(err, errListing) {
			return err
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("waiting for %s: %w", what, err)
			}
			return fmt.Errorf("waiting for %s: not within %s", what, d.cfg.Wait)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

func (d *driver) campaignCDRs(ctx context.Context, campaignID string) ([]api.CDR, error) {
	var l api.CDRList
	if err := d.c.do(ctx, http.MethodGet, "/cdrs?limit=200&campaignId="+campaignID, nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list cdrs: %w", err)
	}
	if l.Total != len(l.Items) {
		return nil, fmt.Errorf("the CDR page did not hold every row (%d of %d)", len(l.Items), l.Total)
	}
	return l.Items, nil
}

// view is what the run left, for the assessment and the after-run checks.
func (d *driver) view(post calleesim.Report) *RunView {
	v := &RunView{
		c: d.c, ObservedAt: d.lastObservation, Cases: d.cfg.Cases, Form: d.form,
		Provider:      ProviderCount{Before: d.providerBefore, After: d.providerAfter},
		MetricsBefore: d.metricsBefore, MetricsAfter: d.metricsAfter, Callbacks: d.callbacks,
	}
	if d.calleeBase <= len(post.Dialogs) {
		v.Dialogs = post.Dialogs[d.calleeBase:]
	}
	v.Callee = post
	v.CalleeReadAt = d.calleeReadAt
	for _, rr := range d.regions {
		rv := RegionView{Region: rr.region, DID: rr.did, Campaign: rr.campaign, CDRs: rr.cdrs}
		for _, cs := range d.cfg.Cases {
			n := rr.region.Number(cs)
			cv := ContactView{Case: cs, Number: n, Attempts: rr.attempts[n]}
			if ct, ok := rr.contacts[n]; ok {
				cv.Contact = &ct
			}
			if ct, ok := rr.afterFirst[n]; ok {
				cv.AfterFirst = &ct
			}
			cv.Listing = rr.listings[n]
			rv.Contacts = append(rv.Contacts, cv)
		}
		v.Regions = append(v.Regions, rv)
	}
	return v
}
