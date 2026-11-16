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
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// The test-number case of issue #578, as data. Two campaigns, A then B, are
// started in a row on two numbers that A reaches: N1 (not listed) and N2 (on
// the list of test numbers before A starts). The cross-campaign frequency
// window must hold N1 for B and not N2; listing N1 then lifts the hold. The
// case needs the application started with a window (e2e.sh frequency does
// that; the environment's default is off).
//
// Both numbers are in the US range, suffixes 81 and 82, and answer with 200
// and hang up after 3 s. Each has two identical behaviours in the scenario
// (A's call and B's), in every dial form.

// FrequencyScenarioID names the case in reports.
const FrequencyScenarioID = "F1"

// The application setting the case changes.
const EnvCampaignFrequencyWindow = "AICC_CAMPAIGN_FREQUENCY_WINDOW"

var frequencyStep = Step{Provisional: 180, SIPStatus: 200, FinalAfterMs: 300, ByeAfterMs: 3000}

// FrequencyUnlisted and FrequencyListed are the case's two contacts.
var (
	FrequencyUnlisted = Case{ID: "F1-N1", Kind: KindScenario, Suffix: "81", Callee: "200 after 300 ms, hangs up 3 s later; not on the test list", Steps: []Step{frequencyStep, frequencyStep}}
	FrequencyListed   = Case{ID: "F1-N2", Kind: KindScenario, Suffix: "82", Callee: "200 after 300 ms, hangs up 3 s later; on the test list", Steps: []Step{frequencyStep, frequencyStep}}
	FrequencyCases    = []Case{FrequencyUnlisted, FrequencyListed}
	// FrequencyRegion is the one country the case uses.
	FrequencyRegion = Regions[0]
)

var frequencyScopes = append(append([]string{}, campaignScopes...), "testnumbers:read", "testnumbers:write")

// frequencyDialBound is how long a contact that must be dialled may take to
// get its attempt: the dialer's tick plus the originate.
const frequencyDialBound = 60 * time.Second

// frequencySlack bounds how far nextDialAt and frequencyHoldEndsAt may lie
// from the touch plus the window: both are computed from the same stored
// time, so they differ by rounding only.
const frequencySlack = 2 * time.Second

// FrequencyReport is what a run writes to report.json.
type FrequencyReport struct {
	Scenario    string    `json:"scenario"`
	Passed      bool      `json:"passed"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	DurationSec float64   `json:"durationSec"`
	DialForm    string    `json:"dialForm"`
	WindowSec   float64   `json:"windowSec"`
	// Timeline is what was observed, in order.
	Timeline []string `json:"timeline"`
	// Attempts are the attempts of both campaigns, by campaign.
	Attempts map[string][]api.CampaignAttempt `json:"attempts"`
	Checks   CheckList                        `json:"checks"`
	Failures []string                         `json:"failures"`
}

// HoldCheck says whether a time the API reports (nextDialAt or a contact's
// frequencyHoldEndsAt) is the touch plus the window: the touch is the start
// of the other campaign's attempt, and slack covers rounding.
func HoldCheck(got *time.Time, touch time.Time, window, slack time.Duration) error {
	if got == nil {
		return fmt.Errorf("absent, want %s (touch %s + window %s)", touch.Add(window).Format(time.RFC3339), touch.Format(time.RFC3339), window)
	}
	want := touch.Add(window)
	if d := got.Sub(want); d < -slack || d > slack {
		return fmt.Errorf("%s, want %s (touch %s + window %s), off by %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano), touch.Format(time.RFC3339Nano), window, d)
	}
	return nil
}

type frequencyRun struct {
	cfg  CampaignConfig
	c    *httpc
	rep  *FrequencyReport
	chk  *collector
	n1   string
	n2   string
	k1   string // dial keys, the simulator's
	k2   string
	did  api.DID
	form string
}

func (f *frequencyRun) note(format string, args ...any) {
	f.rep.Timeline = append(f.rep.Timeline, fmt.Sprintf("%s  %s", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...)))
}

// RunFrequency runs the case against an application started with a nonzero
// AICC_CAMPAIGN_FREQUENCY_WINDOW. It writes report.json to cfg.OutDir and
// returns the report, with an error when the run was refused, could not
// proceed, or an assertion failed.
func RunFrequency(ctx context.Context, cfg CampaignConfig) (*FrequencyReport, error) {
	cfg.Cases, cfg.Regions = FrequencyCases, []Region{FrequencyRegion}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	pre, err := preflight(ctx, cfg, "e2e-frequency", frequencyScopes)
	if err != nil {
		return nil, err
	}
	raw, err := appSetting(pre.composeJSON, EnvCampaignFrequencyWindow)
	if err != nil {
		return nil, err
	}
	window, err := time.ParseDuration(raw)
	if err != nil || window < time.Hour {
		return nil, fmt.Errorf("%s is %q in the compose configuration, want a window of an hour or more; run make e2e-frequency", EnvCampaignFrequencyWindow, raw)
	}
	r := FrequencyRegion
	f := &frequencyRun{
		cfg: cfg, c: pre.c, form: string(pre.form),
		n1: r.Number(FrequencyUnlisted), n2: r.Number(FrequencyListed),
		k1: r.DialKey(FrequencyUnlisted, pre.form), k2: r.DialKey(FrequencyListed, pre.form),
	}
	f.rep = &FrequencyReport{Scenario: FrequencyScenarioID, StartedAt: time.Now().UTC(), DialForm: f.form, WindowSec: window.Seconds(), Attempts: map[string][]api.CampaignAttempt{}}
	f.chk = &collector{}
	runErr := f.run(ctx, window)

	f.rep.FinishedAt = time.Now().UTC()
	f.rep.DurationSec = f.rep.FinishedAt.Sub(f.rep.StartedAt).Seconds()
	f.rep.Checks = CheckList{Checks: f.chk.checks, Failures: f.chk.failures}
	f.rep.Failures = append([]string{}, f.chk.failures...)
	if runErr != nil {
		f.rep.Failures = append(f.rep.Failures, runIncomplete+runErr.Error())
	}
	f.rep.Passed = len(f.rep.Failures) == 0
	if err := writeJSON(filepath.Join(cfg.OutDir, "report.json"), f.rep); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil {
		return f.rep, runErr
	}
	if !f.rep.Passed {
		return f.rep, fmt.Errorf("%d assertion(s) failed: %s", len(f.rep.Failures), strings.Join(f.rep.Failures, "; "))
	}
	return f.rep, nil
}

func (f *frequencyRun) run(ctx context.Context, window time.Duration) error {
	// A repeat run in the same database finds both numbers touched less than
	// a window ago by the earlier run, and A's N1 would be held.
	for _, n := range []string{f.n1, f.n2} {
		if err := f.c.do(ctx, http.MethodDelete, "/test-numbers/"+url.PathEscape(n), nil, nil, http.StatusNoContent, http.StatusNotFound); err != nil {
			return fmt.Errorf("clear %s from the test list: %w", n, err)
		}
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, n := range []string{f.n1, f.n2} {
			_ = f.c.do(cctx, http.MethodDelete, "/test-numbers/"+url.PathEscape(n), nil, nil, http.StatusNoContent, http.StatusNotFound)
		}
	}()

	number := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
	did, err := f.c.publishFlowAndDID(ctx, f.cfg.FlowFile, "e2e-frequency-"+number, "e2e frequency "+number, number, nil)
	if err != nil {
		return err
	}
	f.did = did

	var listed api.TestNumber
	if err := f.c.do(ctx, http.MethodPost, "/test-numbers", api.TestNumberWrite{Number: f.n2, Note: ptr("e2e frequency case")}, &listed, http.StatusOK, http.StatusCreated); err != nil {
		return fmt.Errorf("list %s: %w", f.n2, err)
	}
	f.chk.that(listed.Number == f.n2, "the list holds %s as %s", f.n2, listed.Number)
	f.note("listed N2 %s before A starts", f.n2)

	// Campaign A: both numbers, listed N2 and unlisted N1.
	a, err := f.createCampaign(ctx, "A")
	if err != nil {
		return err
	}
	if err := f.start(ctx, a, "A"); err != nil {
		return err
	}
	if err := f.waitCampaign(ctx, a, "A", func(c api.Campaign) bool { return c.Status == api.CampaignStatusCOMPLETED }, "COMPLETED"); err != nil {
		return err
	}
	attA, err := f.attempts(ctx, a)
	if err != nil {
		return err
	}
	f.rep.Attempts["A"] = attA
	byNum := map[string]api.CampaignAttempt{}
	for _, at := range attA {
		byNum[at.Number] = at
		f.note("A attempt %s: started %s, outcome %s, touched %v, isTestNumber %v", at.Number, at.StartedAt.UTC().Format(time.RFC3339), outcomeOf(at), at.IsTouched, at.IsTestNumber)
	}
	f.chk.that(len(attA) == 2, "campaign A made 2 attempts (%d)", len(attA))
	a1, ok1 := byNum[f.n1]
	a2, ok2 := byNum[f.n2]
	if !ok1 || !ok2 {
		return fmt.Errorf("campaign A has no attempt for %s and %s (it has %d attempts); a repeat run needs a fresh environment: make e2e-up", f.n1, f.n2, len(attA))
	}
	f.chk.that(a1.IsTouched && a2.IsTouched, "both of A's attempts reached the callee (touched: N1 %v, N2 %v)", a1.IsTouched, a2.IsTouched)
	f.chk.that(!a1.IsTestNumber, "A's attempt to the unlisted N1 has isTestNumber=false")
	f.chk.that(a2.IsTestNumber, "A's attempt to the listed N2 has isTestNumber=true")

	// Campaign B: the same numbers; N2 dials, N1 waits for the window.
	bStart := time.Now()
	b, err := f.createCampaign(ctx, "B")
	if err != nil {
		return err
	}
	defer f.cancel(context.WithoutCancel(ctx), b)
	if err := f.start(ctx, b, "B"); err != nil {
		return err
	}
	b2, err := f.waitAttempt(ctx, b, f.n2, frequencyDialBound)
	if err != nil {
		return fmt.Errorf("B did not dial the listed N2: %w", err)
	}
	f.note("B dialled N2 %s after %s", f.n2, b2.StartedAt.Sub(bStart).Round(time.Millisecond))
	f.chk.that(b2.StartedAt.Sub(bStart) < frequencyDialBound, "B dialled the listed N2 within %s of starting (%s)", frequencyDialBound, b2.StartedAt.Sub(bStart).Round(time.Millisecond))
	f.chk.that(b2.IsTestNumber, "B's attempt to N2 has isTestNumber=true")
	f.chk.that(b2.IsTouched, "B's attempt to N2 reached the callee")

	// With N2 done, nothing is in flight and N1 is the only pending contact.
	var held api.Campaign
	if err := f.waitCampaign(ctx, b, "B", func(c api.Campaign) bool {
		held = c
		return c.Activity != nil && *c.Activity == api.CampaignActivityWAITINGFREQUENCY
	}, "activity WAITING_FREQUENCY"); err != nil {
		return err
	}
	f.note("B activity %s, nextDialAt %s", *held.Activity, timeStr(held.NextDialAt))
	f.chk.that(held.Status == api.CampaignStatusRUNNING, "B is RUNNING")
	if err := HoldCheck(held.NextDialAt, a1.StartedAt, window, frequencySlack); err != nil {
		f.chk.that(false, "B's nextDialAt is A's N1 start + window: %v", err)
	} else {
		f.chk.that(true, "B's nextDialAt is A's N1 start + window (%s)", held.NextDialAt.UTC().Format(time.RFC3339))
	}
	contacts, err := f.contacts(ctx, b)
	if err != nil {
		return err
	}
	c1, c2 := contacts[f.n1], contacts[f.n2]
	f.chk.that(c1.ID != c2.ID && c1.ID.String() != "00000000-0000-0000-0000-000000000000", "B holds contacts for N1 and N2")
	if err := HoldCheck(c1.FrequencyHoldEndsAt, a1.StartedAt, window, frequencySlack); err != nil {
		f.chk.that(false, "B's N1 contact frequencyHoldEndsAt is A's N1 start + window: %v", err)
	} else {
		f.chk.that(true, "B's N1 contact frequencyHoldEndsAt is A's N1 start + window (%s)", c1.FrequencyHoldEndsAt.UTC().Format(time.RFC3339))
	}
	f.chk.that(c2.FrequencyHoldEndsAt == nil, "B's N2 contact has no frequencyHoldEndsAt")
	f.note("B N1 contact: status %s, frequencyHoldEndsAt %s", c1.Status, timeStr(c1.FrequencyHoldEndsAt))

	// B must not have dialled N1 so far: the simulator saw two INVITEs for
	// N2 (A's and B's) and one for N1 (A's).
	rep, err := f.simulator(ctx)
	if err != nil {
		return err
	}
	f.chk.that(countDialogs(rep, f.k1) == 1, "the callee saw 1 INVITE for N1 (%d): only A's", countDialogs(rep, f.k1))
	f.chk.that(countDialogs(rep, f.k2) == 2, "the callee saw 2 INVITEs for N2 (%d): A's and B's", countDialogs(rep, f.k2))
	f.chk.that(rep.UnexpectedCount == 0, "no unexpected dialog at the callee (%d)", rep.UnexpectedCount)
	if atts, err := f.attempts(ctx, b); err == nil {
		f.chk.that(len(atts) == 1, "B has made 1 attempt so far, to N2 (%d)", len(atts))
	}

	// Listing N1 re-arms its held contact at once.
	listedAt := time.Now()
	if err := f.c.do(ctx, http.MethodPost, "/test-numbers", api.TestNumberWrite{Number: f.n1, Note: ptr("e2e frequency case")}, nil, http.StatusOK, http.StatusCreated); err != nil {
		return fmt.Errorf("list %s: %w", f.n1, err)
	}
	f.note("listed N1 %s while B waits", f.n1)
	b1, err := f.waitAttempt(ctx, b, f.n1, frequencyDialBound)
	if err != nil {
		return fmt.Errorf("B did not dial N1 after it was listed: %w", err)
	}
	f.note("B dialled N1 %s after listing", b1.StartedAt.Sub(listedAt).Round(time.Millisecond))
	f.chk.that(b1.StartedAt.Sub(listedAt) < frequencyDialBound, "B dialled N1 within %s of listing it (%s)", frequencyDialBound, b1.StartedAt.Sub(listedAt).Round(time.Millisecond))
	f.chk.that(b1.IsTestNumber, "B's attempt to N1 after listing has isTestNumber=true")
	f.chk.that(b1.IsTouched, "B's attempt to N1 reached the callee")
	if err := f.waitCampaign(ctx, b, "B", func(c api.Campaign) bool { return c.Status == api.CampaignStatusCOMPLETED }, "COMPLETED"); err != nil {
		return err
	}
	attB, err := f.attempts(ctx, b)
	if err != nil {
		return err
	}
	f.rep.Attempts["B"] = attB
	f.chk.that(len(attB) == 2, "B made 2 attempts in all (%d)", len(attB))
	return nil
}

func (f *frequencyRun) createCampaign(ctx context.Context, name string) (api.Campaign, error) {
	r := FrequencyRegion
	maxConcurrent, ring, attempts, system := 2, RingTimeoutSec, 1, MaxSystemRetries
	cc := r.CountryCode
	var camp api.Campaign
	if err := f.c.do(ctx, http.MethodPost, "/campaigns", api.CampaignWrite{
		Name:               "e2e frequency " + name + " " + f.did.Number,
		DIDID:              f.did.ID,
		TimeZone:           r.TimeZone,
		CallingWindow:      api.CampaignCallingWindow{Days: allDays, Ranges: []api.CampaignTimeRange{{Start: "00:00", End: "24:00"}}},
		MaxConcurrent:      &maxConcurrent,
		RingTimeoutSec:     &ring,
		DefaultCountryCode: &cc,
		RetryPolicy:        &api.CampaignRetryPolicyWrite{MaxAttempts: &attempts, MaxSystemRetries: &system},
	}, &camp, http.StatusCreated); err != nil {
		return camp, fmt.Errorf("create campaign %s: %w", name, err)
	}
	rows := []api.CampaignContactRow{{Number: f.n1}, {Number: f.n2}}
	var imp api.CampaignImportResult
	if err := f.c.do(ctx, http.MethodPost, "/campaigns/"+camp.ID.String()+"/contacts", api.CampaignContactsImport{Contacts: rows}, &imp, http.StatusOK); err != nil {
		return camp, fmt.Errorf("import contacts into %s: %w", name, err)
	}
	if imp.Imported != len(rows) || imp.Rejected != 0 || imp.Duplicates != 0 {
		return camp, fmt.Errorf("import into %s: %d imported, %d duplicates, %d rejected of %d rows", name, imp.Imported, imp.Duplicates, imp.Rejected, len(rows))
	}
	return camp, nil
}

func (f *frequencyRun) start(ctx context.Context, c api.Campaign, name string) error {
	if err := f.c.do(ctx, http.MethodPost, "/campaigns/"+c.ID.String()+"/start", nil, nil, http.StatusOK); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	f.note("started %s (%s)", name, c.ID)
	return nil
}

func (f *frequencyRun) cancel(ctx context.Context, c api.Campaign) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_ = f.c.do(cctx, http.MethodPost, "/campaigns/"+c.ID.String()+"/cancel", nil, nil, http.StatusOK, http.StatusConflict)
}

func (f *frequencyRun) waitCampaign(ctx context.Context, c api.Campaign, name string, done func(api.Campaign) bool, what string) error {
	deadline := time.Now().Add(f.cfg.Wait)
	var last api.Campaign
	for {
		if err := f.c.do(ctx, http.MethodGet, "/campaigns/"+c.ID.String(), nil, &last, http.StatusOK); err != nil {
			return fmt.Errorf("read campaign %s: %w", name, err)
		}
		if done(last) {
			return nil
		}
		if time.Now().After(deadline) {
			act := "none"
			if last.Activity != nil {
				act = string(*last.Activity)
			}
			return fmt.Errorf("campaign %s did not reach %s within %s (status %s, activity %s)", name, what, f.cfg.Wait, last.Status, act)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (f *frequencyRun) attempts(ctx context.Context, c api.Campaign) ([]api.CampaignAttempt, error) {
	var l api.CampaignAttemptList
	if err := f.c.do(ctx, http.MethodGet, "/campaigns/"+c.ID.String()+"/attempts?limit=100", nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	return l.Items, nil
}

func (f *frequencyRun) contacts(ctx context.Context, c api.Campaign) (map[string]api.CampaignContact, error) {
	var l api.CampaignContactList
	if err := f.c.do(ctx, http.MethodGet, "/campaigns/"+c.ID.String()+"/contacts?limit=100", nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	out := map[string]api.CampaignContact{}
	for _, ct := range l.Items {
		out[ct.Number] = ct
	}
	return out, nil
}

// waitAttempt waits for a finished-or-dialling attempt to number and returns
// it with its final state, once it has finished.
func (f *frequencyRun) waitAttempt(ctx context.Context, c api.Campaign, number string, bound time.Duration) (api.CampaignAttempt, error) {
	deadline := time.Now().Add(bound)
	for {
		atts, err := f.attempts(ctx, c)
		if err != nil {
			return api.CampaignAttempt{}, err
		}
		for _, at := range atts {
			if at.Number == number && at.FinishedAt != nil {
				return at, nil
			}
		}
		if time.Now().After(deadline) {
			return api.CampaignAttempt{}, fmt.Errorf("no finished attempt for %s within %s (%d attempts)", number, bound, len(atts))
		}
		if err := sleep(ctx, time.Second); err != nil {
			return api.CampaignAttempt{}, err
		}
	}
}

func (f *frequencyRun) simulator(ctx context.Context) (calleesim.Report, error) {
	var r calleesim.Report
	if err := f.c.getJSON(ctx, f.cfg.CalleeSim+"/report", &r); err != nil {
		return r, fmt.Errorf("simulator report: %w", err)
	}
	return r, nil
}

func countDialogs(r calleesim.Report, key string) int {
	n := 0
	for _, d := range r.Dialogs {
		if d.Number == key {
			n++
		}
	}
	return n
}

func outcomeOf(a api.CampaignAttempt) string {
	if a.Outcome == nil {
		return "-"
	}
	return string(*a.Outcome)
}

func timeStr(t *time.Time) string {
	if t == nil {
		return "absent"
	}
	return t.UTC().Format(time.RFC3339)
}
