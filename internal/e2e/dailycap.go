// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// The daily dial cap case of issue #577, as data. One campaign, one contact,
// a callee that rings and never answers, so every attempt ends NO_ANSWER when
// our own ring timeout cancels it. With the cap at 3 the contact is dialled
// exactly three times; the fourth dial of the local day is never sent, the
// contact waits for the start of its calling window on the next local day,
// and the campaign's activity is WAITING_DAILY_CAP with nextDialAt at that
// time. The case needs the application started with the cap (e2e.sh dailycap
// does that; the environment's default is 0, off).
//
// The number is in the US range, suffix 92. The simulator plays four
// behaviours, one more than the cap lets through, so a fourth INVITE would be
// answered the same way and show in its record.

// DailyCapScenarioID names the case in reports.
const DailyCapScenarioID = "D1"

// The application setting the case changes, and the cap it expects.
const (
	EnvOutboundMaxDialsPerNumberPerDay = "AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY"
	DailyCapMax                        = 3
	// DailyCapMaxAttempts is the campaign's maxAttempts: well above the cap,
	// so the budget is never what stops the dialling.
	DailyCapMaxAttempts = 6
	// DailyCapRetryIntervalSec is the shortest interval the contract allows
	// (CampaignIntervalSec: 60 to 2592000).
	DailyCapRetryIntervalSec = 60
	// DailyCapRingTimeoutSec is the shortest ring timeout the contract allows.
	DailyCapRingTimeoutSec = 5
)

var dailyCapStep = Step{Provisional: 180, SIPStatus: 480, FinalAfterMs: 120000, Outcome: "NO_ANSWER"}

var (
	// DailyCapCase is the one contact.
	DailyCapCase = Case{
		ID: "D1-N1", Kind: KindScenario, Suffix: "92",
		Callee: "180, then rings until the caller gives up (480 at 120 s, never reached)",
		Steps:  []Step{dailyCapStep, dailyCapStep, dailyCapStep, dailyCapStep},
	}
	DailyCapCases = []Case{DailyCapCase}
	// DailyCapRegion is the one country the case uses; the campaign's zone is
	// chosen at run time (ChooseDailyCapZone).
	DailyCapRegion = Regions[0]
)

// dailyCapZones are the campaign's candidate zones. The run takes the one
// whose local time is farthest from a midnight, so the contact's local day
// does not turn over during the run.
var dailyCapZones = []string{
	"America/New_York", "America/Los_Angeles", "Europe/London", "Asia/Kolkata", "Asia/Shanghai", "Pacific/Auckland",
}

const (
	// dailyCapReachBound is how long the campaign may take to be capped: three
	// attempts, two intervals, and the dialer's passes.
	dailyCapReachBound = 6 * time.Minute
	// dailyCapQuiet is the quiet period after the activity turned
	// WAITING_DAILY_CAP: one retry interval plus a dialer pass margin. In it a
	// fourth dial would have gone out if the cap were not holding.
	dailyCapQuiet = (DailyCapRetryIntervalSec + 30) * time.Second
	// dailyCapSlack bounds how far nextAttemptAt and nextDialAt may lie from
	// the next local midnight.
	dailyCapSlack = 2 * time.Second
	// dailyCapMinFromMidnight is how far from a local midnight the chosen zone
	// must be, to cover the run (about six minutes) with room to spare.
	dailyCapMinFromMidnight = 30 * time.Minute
)

var dailyCapScopes = campaignScopes

// NextLocalMidnight is the start of the day after t's, in loc.
func NextLocalMidnight(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc)
}

// ChooseDailyCapZone returns the candidate whose local time at now is
// farthest from a local midnight (either side), and that distance.
func ChooseDailyCapZone(now time.Time, zones []string) (string, time.Duration, error) {
	best, bestDist := "", time.Duration(-1)
	for _, z := range zones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			return "", 0, fmt.Errorf("zone %s: %w", z, err)
		}
		l := now.In(loc)
		since := l.Sub(time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc))
		until := NextLocalMidnight(now, loc).Sub(now)
		d := min(since, until)
		if d > bestDist {
			best, bestDist = z, d
		}
	}
	if best == "" {
		return "", 0, fmt.Errorf("no zone to choose from")
	}
	return best, bestDist, nil
}

// DailyCapReport is what a run writes to report.json.
type DailyCapReport struct {
	Scenario    string    `json:"scenario"`
	Passed      bool      `json:"passed"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	DurationSec float64   `json:"durationSec"`
	DialForm    string    `json:"dialForm"`
	Number      string    `json:"number"`
	DialKey     string    `json:"dialKey"`
	Cap         int       `json:"cap"`
	// TimeZone is the campaign's zone, and ZoneDistanceFromMidnightSec how far
	// its local time was from a midnight when the run chose it.
	TimeZone                    string   `json:"timeZone"`
	ZoneDistanceFromMidnightSec float64  `json:"zoneDistanceFromMidnightSec"`
	RingTimeoutSec              int      `json:"ringTimeoutSec"`
	RetryIntervalSec            int      `json:"retryIntervalSec"`
	QuietSec                    float64  `json:"quietSec"`
	ExpectedReleaseAt           string   `json:"expectedReleaseAt,omitempty"`
	CappedObservedAt            string   `json:"cappedObservedAt,omitempty"`
	Timeline                    []string `json:"timeline"`

	Campaign      api.Campaign             `json:"campaign"`
	CampaignAtCap api.Campaign             `json:"campaignAtCap"`
	CampaignAfter api.Campaign             `json:"campaignAfterQuiet"`
	Attempts      []api.CampaignAttempt    `json:"attempts"`
	ContactAtCap  *api.CampaignContact     `json:"contactAtCap,omitempty"`
	ContactAfter  *api.CampaignContact     `json:"contactAfterQuiet,omitempty"`
	CDRs          []api.CDR                `json:"cdrs"`
	Dialogs       []calleesim.DialogRecord `json:"dialogs"`
	Checks        CheckList                `json:"checks"`
	Failures      []string                 `json:"failures"`
}

// dailyCapObservation is everything the assertions read.
type dailyCapObservation struct {
	Cap      int
	Zone     *time.Location
	Campaign api.Campaign // as the API created it
	// AtCap is the campaign and contact when the activity first read
	// WAITING_DAILY_CAP; After both once the quiet period had passed.
	CampaignAtCap, CampaignAfter api.Campaign
	ContactAtCap, ContactAfter   *api.CampaignContact
	Attempts                     []api.CampaignAttempt
	CDRs                         []api.CDR
	// Dialogs are the simulator's records for the number, in arrival order;
	// UnexpectedDialogs how many dialogs it saw for numbers it does not know.
	Dialogs           []calleesim.DialogRecord
	UnexpectedDialogs int
	// CappedObservedAt is when the run first read WAITING_DAILY_CAP.
	CappedObservedAt time.Time
}

// assessDailyCap makes every assertion of the case.
func assessDailyCap(o dailyCapObservation) (CheckList, time.Time) {
	var col collector
	var release time.Time

	camp := o.Campaign
	col.that(camp.RetryPolicy.MaxAttempts > o.Cap+1, "maxAttempts (%d) is above the cap (%d) and its next one, so the budget is not what stops the dialling", camp.RetryPolicy.MaxAttempts, o.Cap)
	col.that(camp.RetryPolicy.IntervalSec["NO_ANSWER"] == DailyCapRetryIntervalSec, "the NO_ANSWER interval is %d s (%d)", DailyCapRetryIntervalSec, camp.RetryPolicy.IntervalSec["NO_ANSWER"])
	col.that(camp.RingTimeoutSec == DailyCapRingTimeoutSec, "the ring timeout is %d s (%d)", DailyCapRingTimeoutSec, camp.RingTimeoutSec)

	// The callee: exactly the capped number of INVITEs, none after the cap
	// showed.
	col.that(len(o.Dialogs) == o.Cap, "the callee saw exactly %d INVITEs for the number (%d)", o.Cap, len(o.Dialogs))
	late := 0
	for _, d := range o.Dialogs {
		if !o.CappedObservedAt.IsZero() && d.InviteAt.After(o.CappedObservedAt) {
			late++
		}
	}
	col.that(late == 0, "no INVITE reached the callee after the activity turned WAITING_DAILY_CAP, in a quiet period of %s (%d)", dailyCapQuiet, late)
	col.that(o.UnexpectedDialogs == 0, "no unexpected dialog at the callee (%d)", o.UnexpectedDialogs)

	// The attempts: three, each NO_ANSWER.
	col.that(len(o.Attempts) == o.Cap, "the contact made %d attempts (%d)", o.Cap, len(o.Attempts))
	for i, a := range o.Attempts {
		col.that(a.State == attemptFinished && deref(a.Outcome) == "NO_ANSWER",
			"attempt %d is FINISHED as NO_ANSWER (%s, %s)", i+1, a.State, deref(a.Outcome))
	}

	// The contact: waiting, no budget spent beyond the three, unchanged over
	// the quiet period.
	for _, c := range []struct {
		when string
		ct   *api.CampaignContact
	}{{"at the cap", o.ContactAtCap}, {"after the quiet period", o.ContactAfter}} {
		if c.ct == nil {
			col.that(false, "%s: the contact was read", c.when)
			continue
		}
		col.that(c.ct.Status == api.CampaignContactStatusACTIVE, "%s: the contact is ACTIVE (%s)", c.when, c.ct.Status)
		col.that(c.ct.Attempts == o.Cap && c.ct.SystemFailures == 0,
			"%s: attempts = %d and systemFailures = 0 (%d, %d)", c.when, o.Cap, c.ct.Attempts, c.ct.SystemFailures)
	}
	if o.ContactAtCap != nil && o.ContactAfter != nil {
		col.that(o.ContactAtCap.Attempts == o.ContactAfter.Attempts && o.ContactAtCap.SystemFailures == o.ContactAfter.SystemFailures,
			"the contact's counters did not move over the quiet period (attempts %d -> %d, systemFailures %d -> %d)",
			o.ContactAtCap.Attempts, o.ContactAfter.Attempts, o.ContactAtCap.SystemFailures, o.ContactAfter.SystemFailures)
	}

	// The release: the start of the next local day's window, which is the
	// next local midnight for a window open all day. The day is the one the
	// third attempt started in.
	if len(o.Attempts) > 0 && o.Zone != nil {
		release = NextLocalMidnight(o.Attempts[len(o.Attempts)-1].StartedAt, o.Zone)
		for _, c := range []struct {
			when string
			ct   *api.CampaignContact
		}{{"at the cap", o.ContactAtCap}, {"after the quiet period", o.ContactAfter}} {
			if c.ct != nil {
				col.that(timeNear(c.ct.NextAttemptAt, release, dailyCapSlack),
					"%s: the contact's nextAttemptAt is the next local midnight %s (%s)", c.when, release.UTC().Format(time.RFC3339), timeStr(c.ct.NextAttemptAt))
			}
		}
	}

	// The campaign.
	for _, c := range []struct {
		when string
		cp   api.Campaign
	}{{"at the cap", o.CampaignAtCap}, {"after the quiet period", o.CampaignAfter}} {
		act := "none"
		if c.cp.Activity != nil {
			act = string(*c.cp.Activity)
		}
		col.that(c.cp.Status == api.CampaignStatusRUNNING && act == string(api.CampaignActivityWAITINGDAILYCAP),
			"%s: the campaign is RUNNING with activity WAITING_DAILY_CAP (%s, %s)", c.when, c.cp.Status, act)
		if !release.IsZero() {
			col.that(timeNear(c.cp.NextDialAt, release, dailyCapSlack),
				"%s: the campaign's nextDialAt is the release time %s (%s)", c.when, release.UTC().Format(time.RFC3339), timeStr(c.cp.NextDialAt))
		}
	}

	// The CDRs: one per call, with the campaign's id.
	byCall := map[string]api.CDR{}
	for _, c := range o.CDRs {
		byCall[c.CallID.String()] = c
	}
	col.that(len(o.CDRs) == o.Cap, "the campaign has %d CDRs (%d)", o.Cap, len(o.CDRs))
	for i, a := range o.Attempts {
		c, ok := byCall[a.CallID.String()]
		col.that(ok && c.CampaignID != nil && c.CampaignID.String() == o.Campaign.ID.String(),
			"attempt %d's call %s has a CDR carrying the campaign id", i+1, a.CallID)
	}
	return CheckList{Checks: col.checks, Failures: col.failures}, release
}

func timeNear(got *time.Time, want time.Time, slack time.Duration) bool {
	if got == nil {
		return false
	}
	d := got.Sub(want)
	return d >= -slack && d <= slack
}

type dailyCapRun struct {
	cfg CampaignConfig
	c   *httpc
	rep *DailyCapReport
	key string
}

func (r *dailyCapRun) note(format string, args ...any) {
	r.rep.Timeline = append(r.rep.Timeline, fmt.Sprintf("%s  %s", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...)))
}

// RunDailyCap runs the case against an application started with
// AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY=3. It writes report.json to
// cfg.OutDir and returns the report, with an error when the run was refused,
// could not proceed, or an assertion failed.
func RunDailyCap(ctx context.Context, cfg CampaignConfig) (*DailyCapReport, error) {
	cfg.Cases, cfg.Regions = DailyCapCases, []Region{DailyCapRegion}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	pre, err := preflight(ctx, cfg, "e2e-dailycap", dailyCapScopes)
	if err != nil {
		return nil, err
	}
	raw, err := appSetting(pre.composeJSON, EnvOutboundMaxDialsPerNumberPerDay)
	if err != nil {
		return nil, err
	}
	if n, err := strconv.Atoi(raw); err != nil || n != DailyCapMax {
		return nil, fmt.Errorf("%s is %q in the compose configuration, want %d; run make e2e-dailycap", EnvOutboundMaxDialsPerNumberPerDay, raw, DailyCapMax)
	}
	r := DailyCapRegion
	key := r.DialKey(DailyCapCase, pre.form)
	zone, dist, err := ChooseDailyCapZone(time.Now(), dailyCapZones)
	if err != nil {
		return nil, err
	}
	if dist < dailyCapMinFromMidnight {
		return nil, fmt.Errorf("no candidate zone is more than %s from a local midnight (best %s, %s)", dailyCapMinFromMidnight, zone, dist)
	}
	run := &dailyCapRun{cfg: cfg, c: pre.c, key: key}
	run.rep = &DailyCapReport{
		Scenario: DailyCapScenarioID, StartedAt: time.Now().UTC(), DialForm: string(pre.form),
		Number: r.Number(DailyCapCase), DialKey: key, Cap: DailyCapMax,
		TimeZone: zone, ZoneDistanceFromMidnightSec: dist.Seconds(),
		RingTimeoutSec: DailyCapRingTimeoutSec, RetryIntervalSec: DailyCapRetryIntervalSec, QuietSec: dailyCapQuiet.Seconds(),
		Attempts: []api.CampaignAttempt{}, CDRs: []api.CDR{}, Dialogs: []calleesim.DialogRecord{},
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, err
	}
	var chk CheckList
	obs, runErr := run.run(ctx, zone, loc)
	if runErr == nil {
		var release time.Time
		chk, release = assessDailyCap(obs)
		if !release.IsZero() {
			run.rep.ExpectedReleaseAt = release.UTC().Format(time.RFC3339)
		}
	}

	rep := run.rep
	rep.FinishedAt = time.Now().UTC()
	rep.DurationSec = rep.FinishedAt.Sub(rep.StartedAt).Seconds()
	rep.Checks = chk
	rep.Failures = append([]string{}, chk.Failures...)
	if runErr != nil {
		rep.Failures = append(rep.Failures, runIncomplete+runErr.Error())
	}
	rep.Passed = len(rep.Failures) == 0
	if err := writeJSON(filepath.Join(cfg.OutDir, "report.json"), rep); err != nil && runErr == nil {
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

func (r *dailyCapRun) run(ctx context.Context, zone string, loc *time.Location) (dailyCapObservation, error) {
	obs := dailyCapObservation{Cap: DailyCapMax, Zone: loc}
	reg := DailyCapRegion

	before, err := r.simulator(ctx)
	if err != nil {
		return obs, err
	}
	if n := countDialogs(before, r.key); n > 0 {
		return obs, fmt.Errorf("the simulator has already seen %d INVITE(s) for %s; restart it (deploy/e2e/e2e.sh dailycap does)", n, r.key)
	}
	unexpectedBase := before.UnexpectedCount

	number := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
	did, err := r.c.publishFlowAndDID(ctx, r.cfg.FlowFile, "e2e-dailycap-"+number, "e2e daily cap "+number, number, nil)
	if err != nil {
		return obs, err
	}

	maxConcurrent, ring, attempts, system := 1, DailyCapRingTimeoutSec, DailyCapMaxAttempts, MaxSystemRetries
	base, factor, capSec := DailyCapRetryIntervalSec, 1, DailyCapRetryIntervalSec
	intervals := api.CampaignIntervalSec{"NO_ANSWER": DailyCapRetryIntervalSec}
	outcomes := []api.CampaignRetryOutcome{api.CampaignRetryOutcomeNOANSWER}
	cc := reg.CountryCode
	var camp api.Campaign
	if err := r.c.do(ctx, http.MethodPost, "/campaigns", api.CampaignWrite{
		Name:               "e2e daily cap " + did.Number,
		DIDID:              did.ID,
		TimeZone:           zone,
		CallingWindow:      api.CampaignCallingWindow{Days: allDays, Ranges: []api.CampaignTimeRange{{Start: "00:00", End: "24:00"}}},
		MaxConcurrent:      &maxConcurrent,
		RingTimeoutSec:     &ring,
		DefaultCountryCode: &cc,
		RetryPolicy: &api.CampaignRetryPolicyWrite{
			MaxAttempts: &attempts, MaxSystemRetries: &system, RetryOutcomes: &outcomes, IntervalSec: &intervals,
			TransientBackoff: &api.CampaignBackoffWrite{BaseSec: &base, Factor: &factor, CapSec: &capSec},
		},
	}, &camp, http.StatusCreated); err != nil {
		return obs, fmt.Errorf("create campaign: %w", err)
	}
	obs.Campaign = camp
	r.rep.Campaign = camp
	id := camp.ID.String()
	defer r.cancel(context.WithoutCancel(ctx), id)

	rows := []api.CampaignContactRow{{Number: reg.Number(DailyCapCase)}}
	var imp api.CampaignImportResult
	if err := r.c.do(ctx, http.MethodPost, "/campaigns/"+id+"/contacts", api.CampaignContactsImport{Contacts: rows}, &imp, http.StatusOK); err != nil {
		return obs, fmt.Errorf("import the contact: %w", err)
	}
	if imp.Imported != 1 || imp.Rejected != 0 || imp.Duplicates != 0 {
		return obs, fmt.Errorf("import: %d imported, %d duplicates, %d rejected of 1 row", imp.Imported, imp.Duplicates, imp.Rejected)
	}
	if err := r.c.do(ctx, http.MethodPost, "/campaigns/"+id+"/start", nil, nil, http.StatusOK); err != nil {
		return obs, fmt.Errorf("start the campaign: %w", err)
	}
	r.note("started campaign %s (zone %s, ring %d s, NO_ANSWER interval %d s, maxAttempts %d, cap %d) for %s, dialled as %s",
		id, zone, camp.RingTimeoutSec, DailyCapRetryIntervalSec, camp.RetryPolicy.MaxAttempts, DailyCapMax, reg.Number(DailyCapCase), r.key)

	// Wait for the cap to show.
	deadline := time.Now().Add(dailyCapReachBound)
	for {
		var cur api.Campaign
		if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id, nil, &cur, http.StatusOK); err != nil {
			return obs, fmt.Errorf("read the campaign: %w", err)
		}
		if cur.Activity != nil && *cur.Activity == api.CampaignActivityWAITINGDAILYCAP {
			obs.CampaignAtCap, obs.CappedObservedAt = cur, time.Now()
			break
		}
		if time.Now().After(deadline) {
			act := "none"
			if cur.Activity != nil {
				act = string(*cur.Activity)
			}
			return obs, fmt.Errorf("the campaign did not reach WAITING_DAILY_CAP within %s (status %s, activity %s)", dailyCapReachBound, cur.Status, act)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return obs, err
		}
	}
	ct, err := r.contact(ctx, id)
	if err != nil {
		return obs, err
	}
	obs.ContactAtCap = &ct
	r.rep.CappedObservedAt = obs.CappedObservedAt.UTC().Format(time.RFC3339Nano)
	r.rep.CampaignAtCap, r.rep.ContactAtCap = obs.CampaignAtCap, obs.ContactAtCap
	r.note("activity first read WAITING_DAILY_CAP, nextDialAt %s; contact %s, attempts %d, systemFailures %d, nextAttemptAt %s",
		timeStr(obs.CampaignAtCap.NextDialAt), ct.Status, ct.Attempts, ct.SystemFailures, timeStr(ct.NextAttemptAt))

	// The quiet period: a retry interval and a dialer pass in which a fourth
	// dial would go out if the cap did not hold it.
	r.note("quiet period of %s", dailyCapQuiet)
	if err := sleep(ctx, dailyCapQuiet); err != nil {
		return obs, err
	}
	if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id, nil, &obs.CampaignAfter, http.StatusOK); err != nil {
		return obs, fmt.Errorf("read the campaign: %w", err)
	}
	ct2, err := r.contact(ctx, id)
	if err != nil {
		return obs, err
	}
	obs.ContactAfter = &ct2
	if obs.Attempts, err = r.attempts(ctx, id); err != nil {
		return obs, err
	}
	// CDRs of the attempts are written as each call ends; the third's may be
	// a moment behind its attempt, but the quiet period has passed.
	if obs.CDRs, err = r.cdrs(ctx, id); err != nil {
		return obs, err
	}
	post, err := r.simulator(ctx)
	if err != nil {
		return obs, err
	}
	for _, d := range post.Dialogs {
		if d.Number == r.key {
			obs.Dialogs = append(obs.Dialogs, d)
		}
	}
	obs.UnexpectedDialogs = post.UnexpectedCount - unexpectedBase
	r.rep.CampaignAfter, r.rep.ContactAfter = obs.CampaignAfter, obs.ContactAfter
	r.rep.Attempts, r.rep.CDRs, r.rep.Dialogs = obs.Attempts, obs.CDRs, obs.Dialogs
	r.note("after the quiet period: %d INVITEs, %d attempts, contact attempts %d, activity %s, nextDialAt %s",
		len(obs.Dialogs), len(obs.Attempts), ct2.Attempts, actStr(obs.CampaignAfter.Activity), timeStr(obs.CampaignAfter.NextDialAt))
	return obs, nil
}

func actStr(a *api.CampaignActivity) string {
	if a == nil {
		return "none"
	}
	return string(*a)
}

func (r *dailyCapRun) cancel(ctx context.Context, id string) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := r.c.do(cctx, http.MethodPost, "/campaigns/"+id+"/cancel", nil, nil, http.StatusOK, http.StatusConflict); err != nil {
		r.note("cancel campaign %s: %v", id, err)
		return
	}
	r.note("cancelled campaign %s", id)
}

func (r *dailyCapRun) attempts(ctx context.Context, id string) ([]api.CampaignAttempt, error) {
	var l api.CampaignAttemptList
	if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/attempts?limit=100", nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	sort.SliceStable(l.Items, func(i, j int) bool { return l.Items[i].StartedAt.Before(l.Items[j].StartedAt) })
	return l.Items, nil
}

func (r *dailyCapRun) contact(ctx context.Context, id string) (api.CampaignContact, error) {
	var l api.CampaignContactList
	if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/contacts?limit=100", nil, &l, http.StatusOK); err != nil {
		return api.CampaignContact{}, fmt.Errorf("list contacts: %w", err)
	}
	if len(l.Items) != 1 {
		return api.CampaignContact{}, fmt.Errorf("the campaign has %d contacts, want 1", len(l.Items))
	}
	return l.Items[0], nil
}

func (r *dailyCapRun) cdrs(ctx context.Context, id string) ([]api.CDR, error) {
	var l api.CDRList
	if err := r.c.do(ctx, http.MethodGet, "/cdrs?limit=200&campaignId="+id, nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list cdrs: %w", err)
	}
	return l.Items, nil
}

func (r *dailyCapRun) simulator(ctx context.Context) (calleesim.Report, error) {
	var rep calleesim.Report
	if err := r.c.getJSON(ctx, r.cfg.CalleeSim+"/report", &rep); err != nil {
		return rep, fmt.Errorf("simulator report: %w", err)
	}
	return rep, nil
}
