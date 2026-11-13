// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// refusalScopes are the capacity driver's scopes plus reading the live calls,
// where a call's channel id (the leg id in the switch's log) is found.
var refusalScopes = append(append([]string{}, capacityScopes...), "calls:read:all")

// RefusalReport is what a K3 run writes to report.json.
type RefusalReport struct {
	Scenario    string    `json:"scenario"`
	Passed      bool      `json:"passed"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	DurationSec float64   `json:"durationSec"`
	DialForm    string    `json:"dialForm"`
	// AppEnv is the application environment the run checked it was given.
	AppEnv map[string]string `json:"appEnv"`
	// SwitchRTPRange is the range the switch is configured with, checked
	// apart from the bot's single pair.
	SwitchRTPRange string `json:"switchRtpRange"`
	// Latency is reported and never asserted.
	Latency   map[string]float64 `json:"latencyMs"`
	Campaigns []CampaignInfo     `json:"campaigns"`
	Facts     RefusalFacts       `json:"facts"`
	// Calls is one row per call, in the order they were placed (the campaign's
	// contact last).
	Calls    []RefusalRow     `json:"calls"`
	Provider ProviderCount    `json:"provider"`
	Checks   CheckList        `json:"checks"`
	AfterRun []AfterRunResult `json:"afterRun"`
	// SwitchProbes are what the switch's log must show for each refused call;
	// e2e.sh reads the log after the run and has the test binary judge it
	// (SwitchLog), because the log is in the switch's container.
	SwitchProbes []SwitchProbe    `json:"switchProbes"`
	SwitchLog    *SwitchLogResult `json:"switchLog,omitempty"`
	Failures     []string         `json:"failures"`
}

// RefusalFacts are what the run observed that the assessment needs.
type RefusalFacts struct {
	// QueueID and QueueExt are the fallback queue the run created.
	QueueID  string `json:"queueId"`
	QueueExt string `json:"queueExt"`
	// Channels maps a call id to the switch channel id of its customer leg,
	// read from the live call while it was up.
	Channels map[string]string `json:"channels"`
	// HolderOnBotAt is when the driver saw the holder answered, on the bot
	// (a slot in use, a session open on the mock), before it placed the
	// first refused call, on its wall clock. HolderOnBotAfterRunStartMs is the
	// same moment on the driver's monotonic timeline, the value the order
	// against the calls' placing (DirectCall.PlacedAfterRunStartMs) is read
	// from.
	HolderOnBotAt              *time.Time `json:"holderOnBotAt,omitempty"`
	HolderOnBotAfterRunStartMs *float64   `json:"holderOnBotAfterRunStartMs,omitempty"`
}

// RefusalRow is one call of a K3 run, condensed.
type RefusalRow struct {
	CapacityRow
	Role string `json:"role"`
	// BotReason is the CDR's userData.botReason ("" when absent).
	BotReason string `json:"botReason,omitempty"`
	QueueID   string `json:"queueId,omitempty"`
	ChannelID string `json:"channelId,omitempty"`
}

// refusalCall is a direct call as the run tracks it.
type refusalCall struct {
	spec   RefusalCall
	did    api.DID
	direct DirectCall
}

type refusalRun struct {
	sc      RefusalScenario
	calls   []*refusalCall
	facts   RefusalFacts
	queue   api.Queue
	campDID api.DID
	// holderAt is when the driver saw the holder on the bot, with its
	// monotonic reading (RefusalFacts.HolderOnBotAfterRunStartMs).
	holderAt time.Time
}

// RunRefusal runs K3 (issue #459) against an application started with the
// scenario's environment. It writes report.json and capacity.md to cfg.OutDir
// and returns the report, with an error when the run was refused, could not
// proceed, or an assertion failed. The switch's log is judged afterwards
// (ApplySwitchLog).
func RunRefusal(ctx context.Context, cfg CampaignConfig, sc RefusalScenario) (*RefusalReport, error) {
	cfg.Cases, cfg.Regions = sc.CampaignCases(), []Region{sc.Region}
	if cfg.AfterRun == nil {
		cfg.AfterRun = AfterRunChecks
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	pre, err := preflight(ctx, cfg, "e2e-capacity-"+strings.ToLower(sc.ID), refusalScopes)
	if err != nil {
		return nil, err
	}
	env, _, err := checkAppEnv(pre.composeJSON, CapacityScenario{ID: sc.ID, AppEnv: sc.AppEnv})
	if err != nil {
		return nil, err
	}
	low, _ := strconv.Atoi(env[EnvBotRTPPortLow])
	high, _ := strconv.Atoi(env[EnvBotRTPPortHigh])
	swRange, err := checkRTPRanges(pre.composeJSON, low, high)
	if err != nil {
		return nil, err
	}
	if err := checkRefusalInScenario(pre.scenario, sc); err != nil {
		return nil, err
	}

	started := time.Now()
	rep := &RefusalReport{
		Scenario: sc.ID, StartedAt: started.UTC(), DialForm: string(pre.form), AppEnv: env, SwitchRTPRange: swRange,
		Latency: map[string]float64{}, Calls: []RefusalRow{}, SwitchProbes: []SwitchProbe{},
	}
	d := &driver{cfg: cfg, c: pre.c, form: pre.form, rep: &CampaignReport{Latency: rep.Latency}, plan: sc.Plan, runStart: started}
	rr := &refusalRun{sc: sc, facts: RefusalFacts{Channels: map[string]string{}}}
	view, runErr := rr.run(ctx, d)

	var checks Checks
	if runErr == nil {
		var rows []RefusalRow
		rows, rep.SwitchProbes = AssessRefusal(view, sc, rr.facts, &checks)
		rep.Calls = rows
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
	rep.Facts = rr.facts
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
	if err := writeRefusalReport(cfg.OutDir, rep); err != nil && runErr == nil {
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

func writeRefusalReport(dir string, rep *RefusalReport) error {
	if err := writeJSON(filepath.Join(dir, "report.json"), rep); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "capacity.md"), []byte(RefusalTable(rep)), 0o644)
}

// serviceSetting is one variable of a service's environment in a resolved
// compose configuration; "" when it is not set.
func serviceSetting(composeJSON []byte, service, key string) (string, error) {
	var cfg composeConfig
	if err := json.Unmarshal(composeJSON, &cfg); err != nil {
		return "", fmt.Errorf("compose config is not JSON: %w", err)
	}
	svc, ok := cfg.Services[service]
	if !ok {
		return "", fmt.Errorf("no %s service in the compose configuration", service)
	}
	if v := svc.Environment[key]; v != nil {
		return *v, nil
	}
	return "", nil
}

// switchImageRTPRange is the range the switch image ships (freeswitch/conf),
// which holds when the overlay does not set one.
const switchImageRTPRange = "20000-25999"

// checkRTPRanges refuses a run whose bot range (low-high) shares a port with
// the switch's, which the compose configuration sets through FS_RTP_START_PORT
// and FS_RTP_END_PORT. A shared port would make the refusal K3 provokes
// come from the switch's media instead of the bot's. It returns the switch's
// range as configured.
func checkRTPRanges(composeJSON []byte, low, high int) (string, error) {
	if low <= 0 || high < low {
		return "", fmt.Errorf("the bot's RTP range %d-%d is not a range", low, high)
	}
	start, err := serviceSetting(composeJSON, "freeswitch", "FS_RTP_START_PORT")
	if err != nil {
		return "", err
	}
	end, err := serviceSetting(composeJSON, "freeswitch", "FS_RTP_END_PORT")
	if err != nil {
		return "", err
	}
	swLow, swHigh := 20000, 25999
	if start != "" || end != "" {
		if swLow, err = strconv.Atoi(start); err != nil {
			return "", fmt.Errorf("FS_RTP_START_PORT is %q, not a port", start)
		}
		if swHigh, err = strconv.Atoi(end); err != nil {
			return "", fmt.Errorf("FS_RTP_END_PORT is %q, not a port", end)
		}
	}
	if swHigh < swLow {
		return "", fmt.Errorf("the switch's RTP range %d-%d is not a range", swLow, swHigh)
	}
	if low <= swHigh && swLow <= high {
		return "", fmt.Errorf("refusing to run: the bot's RTP range %d-%d overlaps the switch's %d-%d, so a refusal could come from either", low, high, swLow, swHigh)
	}
	return fmt.Sprintf("%d-%d", swLow, swHigh), nil
}

// checkRefusalInScenario refuses a run whose simulator would not play what
// the direct calls expect: one behaviour per number, with the step's
// provisional, answer time and hang-up. The campaign contact is checked by
// the shared preflight.
func checkRefusalInScenario(s *calleesim.Scenario, sc RefusalScenario) error {
	var problems []string
	for _, rc := range sc.Direct {
		key := rc.DialKey(sc.Region)
		problems = append(problems, stepProblems(rc.ID, key, []Step{rc.Step}, s.Numbers[key])...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("the simulator's scenario does not match %s: %s", sc.ID, strings.Join(problems, "; "))
	}
	return nil
}

// run carries out K3 in order: queue and DIDs, the holder (waited for until it
// is on the bot), the refused calls, the end of the holder, the normal call.
func (rr *refusalRun) run(ctx context.Context, d *driver) (*RunView, error) {
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

	if err := rr.provision(ctx, d); err != nil {
		return nil, err
	}
	d.onPoll = func(ctx context.Context) { rr.captureChannels(ctx, d) }

	// 1. The holder takes the pair.
	holder := rr.calls[0]
	if err := rr.place(ctx, d, holder); err != nil {
		return nil, err
	}
	if err := rr.waitHolderOnBot(ctx, d, holder); err != nil {
		return nil, err
	}

	// 2. The refused calls, placed together: the pair stays held.
	d.startedAt = time.Now()
	for _, rc := range rr.calls[1:] {
		if rc.spec.Role != RoleRefused {
			continue
		}
		if err := rr.place(ctx, d, rc); err != nil {
			return nil, err
		}
	}
	if err := d.c.do(ctx, http.MethodPost, "/campaigns/"+d.regions[0].campaign.ID.String()+"/start", nil, nil, http.StatusOK); err != nil {
		return nil, fmt.Errorf("start campaign: %w", err)
	}
	if err := rr.waitChannels(ctx, d); err != nil {
		return nil, err
	}
	if err := d.waitUntil(ctx, "R3's contact to settle", d.settled); err != nil {
		return nil, err
	}
	d.rep.Latency["startToCampaignSettledMs"] = float64(time.Since(d.startedAt).Milliseconds())
	var refusedKeys []string
	for _, rc := range rr.calls {
		if rc.spec.Role == RoleRefused {
			refusedKeys = append(refusedKeys, rc.direct.DialKey)
		}
	}
	if err := rr.waitDialogsEnded(ctx, d, refusedKeys); err != nil {
		return nil, err
	}

	// 3. The holder ends, the pair is free again, and a normal call follows.
	if err := rr.waitDialogsEnded(ctx, d, []string{holder.direct.DialKey}); err != nil {
		return nil, err
	}
	if err := rr.waitSlotsFree(ctx, d); err != nil {
		return nil, err
	}
	normal := rr.calls[len(rr.calls)-1]
	if err := rr.place(ctx, d, normal); err != nil {
		return nil, err
	}
	if err := rr.waitDialogsEnded(ctx, d, []string{normal.direct.DialKey}); err != nil {
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
	rr0 := d.regions[0]
	if rr0.cdrs, err = d.campaignCDRs(ctx, rr0.campaign.ID.String()); err != nil {
		return nil, err
	}
	if err := rr.readCDRs(ctx, d); err != nil {
		return nil, err
	}

	// The dialogs of the run, split into the campaign's and the direct calls'.
	run := post.Dialogs[min(d.calleeBase, len(post.Dialogs)):]
	byKey := map[string]*refusalCall{}
	for _, rc := range rr.calls {
		byKey[rc.direct.DialKey] = rc
	}
	var campaignDialogs []calleesim.DialogRecord
	for i := range run {
		if rc, ok := byKey[run[i].Number]; ok && rc.direct.Dialog == nil {
			dlg := run[i]
			rc.direct.Dialog = &dlg
			continue
		}
		campaignDialogs = append(campaignDialogs, run[i])
	}

	// The sessions the counters must reach: the answered direct calls that
	// meet a bot (the holder and the normal call). The refused calls and the
	// BOT_FAILED attempt open none.
	sessions := 0
	var extraDIDs []string
	var directs []DirectCall
	for _, rc := range rr.calls {
		directs = append(directs, rc.direct)
		extraDIDs = append(extraDIDs, rc.did.Number)
		if rc.direct.IsAnswered() && !rc.direct.IsExpectedNoBot {
			sessions++
		}
	}
	if err := d.settleCounters(ctx, sessions, extraDIDs); err != nil {
		return nil, err
	}
	v := d.view(post)
	v.Dialogs = campaignDialogs
	v.Direct = directs
	rr.facts.HolderOnBotAt = ptr(rr.holderAt.UTC())
	rr.facts.HolderOnBotAfterRunStartMs = ptr(d.sinceRunStartMs(rr.holderAt))
	return v, nil
}

// provision creates the fallback queue, the DIDs of the direct calls and the
// campaign with its DID (which has the queue too).
func (rr *refusalRun) provision(ctx context.Context, d *driver) error {
	yes := true
	// The tail of a UUIDv7 is its random part; the head is the millisecond.
	id := strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
	name := "e2e-k3-" + id[len(id)-12:]
	if err := d.c.do(ctx, http.MethodPost, "/queues", api.QueueWrite{Name: name, IsEnabled: &yes}, &rr.queue, http.StatusCreated); err != nil {
		return fmt.Errorf("create the fallback queue: %w", err)
	}
	if !rr.queue.IsEnabled || rr.queue.ExtNumber == "" {
		return fmt.Errorf("the fallback queue came back disabled or without an extension (enabled %v, extension %q)", rr.queue.IsEnabled, rr.queue.ExtNumber)
	}
	rr.facts.QueueID, rr.facts.QueueExt = rr.queue.ID.String(), rr.queue.ExtNumber

	used := map[string]bool{}
	for _, spec := range rr.sc.Direct {
		number := freeDIDNumber(used)
		w := api.DIDWrite{Number: number}
		if spec.Language != "" {
			lang := spec.Language
			w.Language = &lang
		}
		if spec.HasFallbackQueue {
			q := rr.queue.ID
			w.FallbackQueueID = &q
		}
		did, err := d.c.publishFlowAndDIDWith(ctx, d.cfg.FlowFile, "e2e-refusal-"+strings.ToLower(spec.ID)+"-"+number, "e2e refusal "+spec.ID+" "+number, w)
		if err != nil {
			return fmt.Errorf("%s: DID: %w", spec.ID, err)
		}
		if err := checkDIDShape(did, spec.Language, spec.HasFallbackQueue, rr.queue.ID); err != nil {
			return fmt.Errorf("%s: %w", spec.ID, err)
		}
		rr.calls = append(rr.calls, &refusalCall{spec: spec, did: did, direct: DirectCall{
			Number: spec.Number(rr.sc.Region), DialKey: spec.DialKey(rr.sc.Region), DID: did.Number, CDRs: []api.CDR{},
			IsExpectedNoBot: spec.Role == RoleRefused,
		}})
	}

	// R3's campaign, on a DID that has the queue.
	d.fallbackQueue = &rr.queue.ID
	rg, err := d.provision(ctx, rr.sc.Region, used)
	if err != nil {
		return fmt.Errorf("%s: %w", rr.sc.Region.Code, err)
	}
	if err := checkDIDShape(rg.did, "", true, rr.queue.ID); err != nil {
		return fmt.Errorf("R3: %w", err)
	}
	d.regions = append(d.regions, rg)
	rr.campDID = rg.did
	return nil
}

// checkDIDShape confirms the DID came back as it was written: the run means
// nothing if the language or the fallback queue did not take.
func checkDIDShape(did api.DID, language string, hasQueue bool, queueID uuid.UUID) error {
	want := language
	if want == "" {
		want = "en"
	}
	if did.Language != want {
		return fmt.Errorf("DID %s came back with language %q, want %q", did.Number, did.Language, want)
	}
	switch {
	case hasQueue && (did.FallbackQueueID == nil || *did.FallbackQueueID != queueID):
		return fmt.Errorf("DID %s came back without the fallback queue %s", did.Number, queueID)
	case !hasQueue && did.FallbackQueueID != nil:
		return fmt.Errorf("DID %s came back with a fallback queue (%s), want none", did.Number, *did.FallbackQueueID)
	}
	return nil
}

// place sends POST /calls for a direct call.
func (rr *refusalRun) place(ctx context.Context, d *driver, rc *refusalCall) error {
	kind := api.CreateCallRequestKind("AI_OUTBOUND")
	callID := uuid.Must(uuid.NewV7())
	did := rc.did.Number
	var created api.CreateCallResponse
	placed := time.Now()
	rc.direct.Placed, rc.direct.PlacedAfterRunStartMs = placed.UTC(), d.sinceRunStartMs(placed)
	if err := d.c.do(ctx, http.MethodPost, "/calls", api.CreateCallRequest{CallID: &callID, Kind: &kind, To: rc.direct.Number, DID: &did}, &created, http.StatusCreated); err != nil {
		return fmt.Errorf("place %s (%s): %w", rc.spec.ID, rc.direct.Number, err)
	}
	if created.CallID != callID {
		return fmt.Errorf("%s was placed as %s, not under the id sent (%s)", rc.spec.ID, created.CallID, callID)
	}
	rc.direct.CallID = callID.String()
	return nil
}

// dialogOf is the simulator's dialog for a number during the run, nil when it
// has none yet.
func dialogOf(r calleesim.Report, base int, key string) *calleesim.DialogRecord {
	for _, dlg := range r.Dialogs[min(base, len(r.Dialogs)):] {
		if dlg.Number == key {
			x := dlg
			return &x
		}
	}
	return nil
}

// waitHolderOnBot waits until the holder is answered, up, and on the bot: the
// simulator shows its 200, the application has a bot slot in use and the mock
// has a session open. Anything else and the run stops, since a split it did
// not set up would be reported as a result.
func (rr *refusalRun) waitHolderOnBot(ctx context.Context, d *driver, holder *refusalCall) error {
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		var r calleesim.Report
		if err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &r); err != nil {
			return fmt.Errorf("simulator report: %w", err)
		}
		dlg := dialogOf(r, d.calleeBase, holder.direct.DialKey)
		m, merr := scrapeMetrics(ctx, d.c.client, d.cfg.MetricsURL)
		s, serr := providerStats(ctx, d.c, d.cfg.MockProvider)
		if merr == nil && serr == nil {
			inUse, _ := m.Sum(MetricBotSlots, map[string]string{"state": "IN_USE"})
			answered := dlg != nil && dlg.Final != nil && dlg.Final.Status == 200 && dlg.End == nil
			last = fmt.Sprintf("answered and up %v, bot slots in use %.0f, mock sessions live %d", answered, inUse, s.Live)
			if answered && inUse == 1 && s.Live == 1 {
				rr.holderAt = time.Now() // keeps its monotonic reading
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the holder (%s) did not reach the bot within 30 s (%s), so the refused calls would not meet a held pair", holder.direct.Number, last)
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// captureChannels reads the live calls and keeps the channel id of each direct
// call's customer leg, the id the switch's log prefixes the call's lines with.
// A failed read is one sample fewer.
func (rr *refusalRun) captureChannels(ctx context.Context, d *driver) {
	var l api.CallList
	if err := d.c.do(ctx, http.MethodGet, "/calls", nil, &l, http.StatusOK); err != nil {
		return
	}
	for _, rc := range rr.calls {
		if rc.direct.CallID == "" || rr.facts.Channels[rc.direct.CallID] != "" {
			continue
		}
		for _, snap := range l.Items {
			if snap.CallID.String() != rc.direct.CallID {
				continue
			}
			parties := append([]api.PartySnapshot{}, snap.Parties...)
			sort.SliceStable(parties, func(i, j int) bool { return parties[i].CreatedAt.Before(parties[j].CreatedAt) })
			for _, p := range parties {
				if (p.IsBotLeg == nil || !*p.IsBotLeg) && p.ChannelID != "" {
					rr.facts.Channels[rc.direct.CallID] = p.ChannelID
					break
				}
			}
		}
	}
}

// waitChannels polls the live calls until every refused call's channel id is
// known, for up to 20 s. A call that ended first leaves its id unknown, which
// the switch-log check then reports.
func (rr *refusalRun) waitChannels(ctx context.Context, d *driver) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		rr.captureChannels(ctx, d)
		missing := 0
		for _, rc := range rr.calls {
			if rc.spec.Role == RoleRefused && rr.facts.Channels[rc.direct.CallID] == "" {
				missing++
			}
		}
		if missing == 0 || time.Now().After(deadline) {
			return nil
		}
		if err := sleep(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

// waitDialogsEnded waits until the simulator shows each number's dialog ended.
func (rr *refusalRun) waitDialogsEnded(ctx context.Context, d *driver, keys []string) error {
	deadline := time.Now().Add(d.cfg.Wait)
	for {
		var r calleesim.Report
		if err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &r); err != nil {
			return fmt.Errorf("simulator report: %w", err)
		}
		ended := 0
		for _, k := range keys {
			if dlg := dialogOf(r, d.calleeBase, k); dlg != nil {
				if _, ok := dialogEnd(*dlg); ok {
					ended++
				}
			}
		}
		if ended == len(keys) {
			return nil
		}
		rr.captureChannels(ctx, d)
		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for %v to end: %d of %d did within %s", keys, ended, len(keys), d.cfg.Wait)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

// waitSlotsFree waits until no bot slot is in use or reserved, so the normal
// call finds the pair free.
func (rr *refusalRun) waitSlotsFree(ctx context.Context, d *driver) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		m, err := scrapeMetrics(ctx, d.c.client, d.cfg.MetricsURL)
		if err != nil {
			return err
		}
		inUse, _ := m.Sum(MetricBotSlots, map[string]string{"state": "IN_USE"})
		reserved, _ := m.Sum(MetricBotSlots, map[string]string{"state": "RESERVED"})
		if inUse == 0 && reserved == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the holder ended but bot slots stayed taken (in use %.0f, reserved %.0f) for 20 s", inUse, reserved)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

// readCDRs reads each direct call's ledger row from its DID, waiting up to 30 s
// for a row that is still being written.
func (rr *refusalRun) readCDRs(ctx context.Context, d *driver) error {
	for _, rc := range rr.calls {
		deadline := time.Now().Add(30 * time.Second)
		for {
			var l api.CDRList
			if err := d.c.do(ctx, http.MethodGet, "/cdrs?limit=200&did="+url.QueryEscape(rc.did.Number), nil, &l, http.StatusOK); err != nil {
				return fmt.Errorf("list %s's cdrs: %w", rc.spec.ID, err)
			}
			rc.direct.CDRs = nil
			for _, cdr := range l.Items {
				if cdr.CallID.String() == rc.direct.CallID {
					rc.direct.CDRs = append(rc.direct.CDRs, cdr)
				}
			}
			if len(rc.direct.CDRs) > 0 || time.Now().After(deadline) {
				break
			}
			if err := sleep(ctx, 500*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return nil
}
