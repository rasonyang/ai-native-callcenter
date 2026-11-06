// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
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
)

// capacityScopes are the campaign driver's scopes plus placing an AI call,
// which K1's slot holders are.
var capacityScopes = append(append([]string{}, campaignScopes...), "calls:create", "calls:create:ai")

// CapacityReport is what a capacity run writes to report.json.
type CapacityReport struct {
	Scenario   string    `json:"scenario"`
	Passed     bool      `json:"passed"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// DurationSec is the whole run, sign-in to report.
	DurationSec float64 `json:"durationSec"`
	DialForm    string  `json:"dialForm"`
	// AppEnv is the application environment the run checked it was given.
	AppEnv map[string]string `json:"appEnv"`
	// Latency is reported and never asserted.
	Latency   map[string]float64 `json:"latencyMs"`
	Campaigns []CampaignInfo     `json:"campaigns"`
	// Contacts is one row per contact: its attempt, its dialog and its CDR.
	Contacts []CapacityRow `json:"contacts"`
	// Direct is one row per call the run placed itself (K1).
	Direct []CapacityRow `json:"direct,omitempty"`
	// Lines is, per DID (K2), the simulator's peak of concurrent dialogs and
	// the application's line gauge as sampled during the run.
	Lines    []LineRow     `json:"lines,omitempty"`
	Provider ProviderCount `json:"provider"`
	// Checks are the scenario's own assertions.
	Checks   CheckList        `json:"checks"`
	AfterRun []AfterRunResult `json:"afterRun"`
	Failures []string         `json:"failures"`
}

// CapacityRow is one call of a capacity run, condensed.
type CapacityRow struct {
	Region      string `json:"region"`
	ID          string `json:"id"`
	Number      string `json:"number"`
	CallID      string `json:"callId"`
	Outcome     string `json:"outcome,omitempty"`
	HangupCause string `json:"hangupCause"`
	ContactEnd  string `json:"contactStatus,omitempty"`
	Invites     int    `json:"invites"`
	FinalStatus int    `json:"finalStatus"`
	EndedBy     string `json:"endedBy"`
	HasBotLeg   bool   `json:"hasBotLeg"`
	// InviteAtMs, AnswerAtMs and EndAtMs are counted from the run's first
	// INVITE; HoldMs is the answer to the end, which for an OVERFLOW leg is
	// how long it was parked. Reported, never asserted.
	InviteAtMs float64  `json:"inviteAtMs"`
	AnswerAtMs *float64 `json:"answerAtMs,omitempty"`
	EndAtMs    *float64 `json:"endAtMs,omitempty"`
	HoldMs     *float64 `json:"answerToEndMs,omitempty"`
	// DownlinkFrames and DownlinkPeakRMS are the audio the callee received.
	DownlinkFrames  int     `json:"downlinkFrames"`
	DownlinkPeakRMS float64 `json:"downlinkPeakRms"`
}

// LineRow is one DID's line use during K2.
type LineRow struct {
	Region string `json:"region"`
	DID    string `json:"did"`
	Limit  int    `json:"limit"`
	// PeakDialogs is the simulator's largest number of dialogs from this
	// caller id that were up at the same time.
	PeakDialogs int `json:"peakDialogs"`
	Dialogs     int `json:"dialogs"`
	// GaugePeak is the largest aicc_did_outbound_in_use the driver read,
	// over GaugeSamples readings; a sample can miss a short peak.
	GaugePeak    float64 `json:"gaugePeak"`
	GaugeSamples int     `json:"gaugeSamples"`
	// AtCapacitySeen says whether the campaign's activity read AT_CAPACITY
	// in any observation. Reported only.
	AtCapacitySeen bool `json:"atCapacitySeen"`
}

// capacityRun is the state a capacity run adds to the campaign driver.
type capacityRun struct {
	sc        CapacityScenario
	directDID api.DID
	direct    []DirectCall
	// gaugePeak and gaugeSamples are the DID line gauge as sampled while
	// the driver waited, by DID.
	gaugePeak    map[string]float64
	gaugeSamples int
	atCapacity   map[string]bool
	// allRinging is when the driver saw every contact's attempt start, the
	// moment it placed the direct calls.
	allRinging time.Time
}

// RunCapacity runs one capacity scenario (issue #247) against an application
// started with the scenario's environment. It writes report.json and
// capacity.md to cfg.OutDir and returns the report, with an error when the
// run was refused, could not proceed, or an assertion failed.
func RunCapacity(ctx context.Context, cfg CampaignConfig, sc CapacityScenario) (*CapacityReport, error) {
	cfg.Cases, cfg.Regions = sc.Cases, sc.Regions
	if cfg.AfterRun == nil {
		cfg.AfterRun = AfterRunChecks
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	pre, err := preflight(ctx, cfg, "e2e-capacity-"+strings.ToLower(sc.ID), capacityScopes)
	if err != nil {
		return nil, err
	}
	env, hold, err := checkAppEnv(pre.composeJSON, sc)
	if err != nil {
		return nil, err
	}
	if err := checkDirectInScenario(pre.scenario, sc); err != nil {
		return nil, err
	}

	rep := &CapacityReport{
		Scenario: sc.ID, StartedAt: time.Now().UTC(), DialForm: string(pre.form), AppEnv: env,
		Latency: map[string]float64{}, Contacts: []CapacityRow{},
	}
	d := &driver{cfg: cfg, c: pre.c, form: pre.form, rep: &CampaignReport{Latency: rep.Latency}, plan: sc.Plan}
	cr := &capacityRun{sc: sc, gaugePeak: map[string]float64{}, atCapacity: map[string]bool{}}
	view, runErr := cr.run(ctx, d)

	var checks Checks
	if runErr == nil {
		switch sc.ID {
		case "K1":
			rep.Contacts, rep.Direct = AssessOverflow(view, hold, &checks)
		case "K2":
			rep.Contacts, rep.Lines = AssessLineLimit(view, sc.LineLimit, cr.gaugePeak, cr.gaugeSamples, cr.atCapacity, &checks)
		}
		assessCapacityRun(view, d.unexpectedBase, &checks)
		for _, chk := range cfg.AfterRun {
			var col Checks
			chk.Run(ctx, view, &col)
			rep.AfterRun = append(rep.AfterRun, AfterRunResult{Name: chk.Name, CheckList: col.list()})
		}
		rep.Provider = view.Provider
		for _, rv := range view.Regions {
			rep.Campaigns = append(rep.Campaigns, CampaignInfo{Region: rv.Region.Code, CampaignID: rv.Campaign.ID.String(), DID: rv.DID.Number, Status: string(rv.Campaign.Status)})
		}
	}
	rep.Checks = checks.list()
	rep.FinishedAt = time.Now().UTC()
	rep.DurationSec = rep.FinishedAt.Sub(rep.StartedAt).Seconds()
	rep.Failures = append([]string{}, rep.Checks.Failures...)
	for _, a := range rep.AfterRun {
		for _, f := range a.Failures {
			rep.Failures = append(rep.Failures, "after-run "+a.Name+": "+f)
		}
	}
	if runErr != nil {
		rep.Failures = append(rep.Failures, runIncomplete+runErr.Error())
	}
	rep.Passed = len(rep.Failures) == 0
	if err := writeJSON(filepath.Join(cfg.OutDir, "report.json"), rep); err != nil && runErr == nil {
		runErr = err
	}
	if err := os.WriteFile(filepath.Join(cfg.OutDir, "capacity.md"), []byte(CapacityTable(rep)), 0o644); err != nil && runErr == nil {
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

// checkAppEnv refuses a run whose application was not started with the
// scenario's environment (e2e.sh capacity recreates it for each scenario and
// renders the configuration it used). It returns the values as configured
// ("" for unset) and the overflow hold in effect.
func checkAppEnv(composeJSON []byte, sc CapacityScenario) (map[string]string, time.Duration, error) {
	out := map[string]string{}
	var problems []string
	keys := make([]string, 0, len(sc.AppEnv))
	for k := range sc.AppEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		got, err := appSetting(composeJSON, k)
		if err != nil {
			return nil, 0, err
		}
		if got != sc.AppEnv[k] {
			problems = append(problems, fmt.Sprintf("%s is %q, want %q", k, got, sc.AppEnv[k]))
		}
		out[k] = got
	}
	if len(problems) > 0 {
		return nil, 0, fmt.Errorf("refusing to run %s: the application was not started with its environment (%s); run it with deploy/e2e/e2e.sh capacity",
			sc.ID, strings.Join(problems, "; "))
	}
	raw := out[EnvOverflowHold]
	if raw == "" {
		raw = defaultOverflowHold
	}
	hold, err := time.ParseDuration(raw)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", EnvOverflowHold, err)
	}
	return out, hold, nil
}

// defaultOverflowHold is the application's AICC_OVERFLOW_HOLD default
// (config.DefaultOverflowHold), written down here so the harness reads the
// deployment rather than the code it tests.
const defaultOverflowHold = "1500ms"

// checkDirectInScenario refuses a run whose direct calls the simulator would
// not answer at once and keep up: each needs one behaviour, a 200 well before
// any contact answers, and a hang-up after the last overflow hold.
func checkDirectInScenario(s *calleesim.Scenario, sc CapacityScenario) error {
	var problems []string
	for _, ds := range sc.Direct {
		list := s.Numbers[ds.DialKey()]
		if len(list) != 1 {
			problems = append(problems, fmt.Sprintf("direct call %s: the scenario has %d behaviours, want 1", ds.Number(), len(list)))
			continue
		}
		b := list[0]
		if b.Final() != 200 || b.FinalAfterMs != k1HolderAnswerMs || b.ByeAfterMs == nil || *b.ByeAfterMs != k1HolderByeMs {
			problems = append(problems, fmt.Sprintf("direct call %s: the scenario answers %d after %d ms and hangs up after %v, want 200 after %d ms and a hang-up after %d ms",
				ds.Number(), b.Final(), b.FinalAfterMs, b.ByeAfterMs, k1HolderAnswerMs, k1HolderByeMs))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the simulator's scenario does not match %s: %s", sc.ID, strings.Join(problems, "; "))
	}
	return nil
}

func (cr *capacityRun) run(ctx context.Context, d *driver) (*RunView, error) {
	pre, err := d.simulatorReport(ctx)
	if err != nil {
		return nil, err
	}
	d.calleeBase, d.unexpectedBase = len(pre.Dialogs), pre.UnexpectedCount
	if d.providerBefore, err = providerStats(ctx, d.c, d.cfg.MockProvider); err != nil {
		return nil, err
	}
	if d.metricsBefore, err = scrapeMetrics(ctx, d.c.client, d.cfg.MetricsURL); err != nil {
		return nil, err
	}

	used := map[string]bool{}
	for _, r := range d.cfg.Regions {
		rr, err := d.provision(ctx, r, used)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Code, err)
		}
		if err := checkLineLimit(rr.did, d.plan.MaxOutboundChannels); err != nil {
			return nil, fmt.Errorf("%s: %w", r.Code, err)
		}
		d.regions = append(d.regions, rr)
	}
	if len(cr.sc.Direct) > 0 {
		number := freeDIDNumber(used)
		did, err := d.c.publishFlowAndDID(ctx, d.cfg.FlowFile, "e2e-capacity-direct-"+number, "e2e capacity direct "+number, number, nil)
		if err != nil {
			return nil, fmt.Errorf("direct calls' DID: %w", err)
		}
		cr.directDID = did
	}

	d.onPoll = func(ctx context.Context) { cr.sample(ctx, d) }
	d.startedAt = time.Now()
	for _, rr := range d.regions {
		if err := d.c.do(ctx, http.MethodPost, "/campaigns/"+rr.campaign.ID.String()+"/start", nil, nil, http.StatusOK); err != nil {
			return nil, fmt.Errorf("%s: start campaign: %w", rr.region.Code, err)
		}
	}
	if len(cr.sc.Direct) > 0 {
		if err := cr.placeDirect(ctx, d); err != nil {
			return nil, err
		}
	}

	if err := d.waitUntil(ctx, "every contact's first attempt to finish", d.firstRoundDone); err != nil {
		return nil, err
	}
	rep := d.rep
	rep.Latency["startToFirstRoundMs"] = float64(time.Since(d.startedAt).Milliseconds())
	if err := d.waitUntil(ctx, "every contact to settle", d.settled); err != nil {
		return nil, err
	}
	rep.Latency["startToSettledMs"] = float64(time.Since(d.startedAt).Milliseconds())
	if err := cr.waitDirectEnded(ctx, d); err != nil {
		return nil, err
	}
	// A stray late attempt, INVITE or CDR would land in this window.
	if err := sleep(ctx, 5*time.Second); err != nil {
		return nil, err
	}
	if err := d.observe(ctx); err != nil {
		return nil, err
	}
	var post calleesim.Report
	if err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &post); err != nil {
		return nil, fmt.Errorf("simulator report: %w", err)
	}
	for _, rr := range d.regions {
		if rr.cdrs, err = d.campaignCDRs(ctx, rr.campaign.ID.String()); err != nil {
			return nil, err
		}
	}
	if err := cr.readDirectCDRs(ctx, d); err != nil {
		return nil, err
	}

	// The dialogs of the run, split into the campaigns' and the direct
	// calls'.
	run := post.Dialogs[min(d.calleeBase, len(post.Dialogs)):]
	directKeys := map[string]int{}
	for i, dc := range cr.direct {
		directKeys[dc.DialKey] = i
	}
	var campaignDialogs []calleesim.DialogRecord
	for i := range run {
		if j, ok := directKeys[run[i].Number]; ok && cr.direct[j].Dialog == nil {
			dlg := run[i]
			cr.direct[j].Dialog = &dlg
			continue
		}
		campaignDialogs = append(campaignDialogs, run[i])
	}

	// The sessions the counters must reach: every answered campaign dialog
	// that did not overflow, and every answered direct call.
	sessions := answeredDialogs(campaignDialogs)
	for _, rr := range d.regions {
		for _, list := range rr.attempts {
			for _, a := range list {
				if deref(a.Outcome) == outcomeOverflow {
					sessions--
				}
			}
		}
	}
	var extraDIDs []string
	for _, dc := range cr.direct {
		if dc.IsAnswered() {
			sessions++
		}
	}
	if cr.directDID.Number != "" {
		extraDIDs = append(extraDIDs, cr.directDID.Number)
	}
	if err := d.settleCounters(ctx, sessions, extraDIDs); err != nil {
		return nil, err
	}

	v := d.view(post)
	v.Dialogs = campaignDialogs
	v.Direct = cr.direct
	return v, nil
}

// checkLineLimit confirms the DID came back with the line limit it was
// created with: the API is where the limit is set (maxOutboundChannels on
// createDID), and the run means nothing if it did not take.
func checkLineLimit(did api.DID, want *int) error {
	switch {
	case want == nil && did.MaxOutboundChannels != nil:
		return fmt.Errorf("DID %s has maxOutboundChannels %d, want none", did.Number, *did.MaxOutboundChannels)
	case want != nil && (did.MaxOutboundChannels == nil || *did.MaxOutboundChannels != *want):
		return fmt.Errorf("DID %s came back with maxOutboundChannels %v, want %d", did.Number, did.MaxOutboundChannels, *want)
	}
	return nil
}

func freeDIDNumber(used map[string]bool) string {
	for {
		n := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
		if !used[n] {
			used[n] = true
			return n
		}
	}
}

// placeDirect waits until every contact's attempt has started, then places
// the direct calls. The dialer admitted every contact while all slots were
// free; the direct calls take their slots when they answer, which the
// simulator does long before any contact answers.
func (cr *capacityRun) placeDirect(ctx context.Context, d *driver) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		started := 0
		for _, rr := range d.regions {
			var l api.CampaignAttemptList
			if err := d.c.do(ctx, http.MethodGet, "/campaigns/"+rr.campaign.ID.String()+"/attempts?limit=200", nil, &l, http.StatusOK); err != nil {
				return err
			}
			started += len(l.Items)
		}
		want := len(d.regions) * len(d.cfg.Cases)
		if started >= want {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d of the %d contacts' attempts started within 30 s, so the direct calls were not placed", started, want)
		}
		if err := sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	cr.allRinging = time.Now()
	d.rep.Latency["startToAllRingingMs"] = float64(cr.allRinging.Sub(d.startedAt).Milliseconds())
	kind := api.CreateCallRequestKind("AI_OUTBOUND")
	for _, ds := range cr.sc.Direct {
		callID := uuid.Must(uuid.NewV7())
		did := cr.directDID.Number
		var created api.CreateCallResponse
		placed := time.Now().UTC()
		if err := d.c.do(ctx, http.MethodPost, "/calls", api.CreateCallRequest{CallID: &callID, Kind: &kind, To: ds.Number(), DID: &did}, &created, http.StatusCreated); err != nil {
			return fmt.Errorf("place the direct call to %s: %w", ds.Number(), err)
		}
		if created.CallID != callID {
			return fmt.Errorf("the direct call to %s was placed as %s, not under the id sent (%s)", ds.Number(), created.CallID, callID)
		}
		cr.direct = append(cr.direct, DirectCall{Number: ds.Number(), DialKey: ds.DialKey(), DID: did, CallID: callID.String(), Placed: placed, CDRs: []api.CDR{}})
	}
	return nil
}

// waitDirectEnded waits until the simulator has seen every direct call's
// dialog end (the callee hangs up 12 s after answering).
func (cr *capacityRun) waitDirectEnded(ctx context.Context, d *driver) error {
	if len(cr.direct) == 0 {
		return nil
	}
	deadline := time.Now().Add(d.cfg.Wait)
	for {
		var r calleesim.Report
		if err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &r); err != nil {
			return fmt.Errorf("simulator report: %w", err)
		}
		ended := 0
		for _, dc := range cr.direct {
			for _, dlg := range r.Dialogs[min(d.calleeBase, len(r.Dialogs)):] {
				if dlg.Number == dc.DialKey && dlg.End != nil {
					ended++
					break
				}
			}
		}
		if ended == len(cr.direct) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for the direct calls to end: %d of %d did within %s", ended, len(cr.direct), d.cfg.Wait)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

// readDirectCDRs reads the ledger rows of the direct calls' DID.
func (cr *capacityRun) readDirectCDRs(ctx context.Context, d *driver) error {
	if len(cr.direct) == 0 {
		return nil
	}
	var l api.CDRList
	if err := d.c.do(ctx, http.MethodGet, "/cdrs?limit=200&did="+url.QueryEscape(cr.directDID.Number), nil, &l, http.StatusOK); err != nil {
		return fmt.Errorf("list the direct calls' cdrs: %w", err)
	}
	if l.Total != len(l.Items) {
		return fmt.Errorf("the direct calls' CDR page did not hold every row (%d of %d)", len(l.Items), l.Total)
	}
	for i := range cr.direct {
		for _, cdr := range l.Items {
			if cdr.CallID.String() == cr.direct[i].CallID {
				cr.direct[i].CDRs = append(cr.direct[i].CDRs, cdr)
			}
		}
	}
	return nil
}

// sample reads the DID line gauge and each campaign's activity while the
// driver waits. It runs on the driver's own loop (driver.onPoll), never
// concurrently with it; a reading that fails is one sample fewer.
func (cr *capacityRun) sample(ctx context.Context, d *driver) {
	for _, rr := range d.regions {
		if rr.campaign.Activity != nil && *rr.campaign.Activity == api.CampaignActivityATCAPACITY {
			cr.atCapacity[rr.did.Number] = true
		}
	}
	m, err := scrapeMetrics(ctx, d.c.client, d.cfg.MetricsURL)
	if err != nil {
		return
	}
	cr.gaugeSamples++
	for _, rr := range d.regions {
		if v, _ := m.Sum(MetricDIDOutboundInUse, map[string]string{"did": rr.did.Number}); v > cr.gaugePeak[rr.did.Number] {
			cr.gaugePeak[rr.did.Number] = v
		}
	}
}
