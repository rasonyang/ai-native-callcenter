// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// The carrier's 408 case of issue #576, as data. A carrier that ends an
// unanswered call itself, with a SIP 408 some seconds after it sent 180, used
// to leave the attempt TRANSIENT (a system outcome that spends
// maxSystemRetries and retries after the transient backoff, and that the trunk
// breaker counts as a trunk fault). It is now NO_ANSWER: the phone rang, so the
// attempt spends one of maxAttempts and waits the NO_ANSWER interval.
//
// The case needs a campaign of its own, which the shared campaign tables
// cannot give it: the callee takes 25 s to send its 408, so the ring timeout
// must be above that (the harness's 5 s would end the call with our own CANCEL
// first), and the interval is the product's default (2 h), not the harness's
// 60 s, which is what makes "retried after the NO_ANSWER interval" a statement
// about the product's policy and not about the harness's. The campaign is
// cancelled at the end, so the retry never fires.
//
// The number is in the US range, suffix 91, and plays one behaviour: 180 at
// 2000 ms, then a final 408 at 25000 ms. The scenario holds it in every dial
// form.

// CarrierTimeoutScenarioID names the case in reports.
const CarrierTimeoutScenarioID = "T1"

const (
	// CarrierTimeoutRingSec is the campaign's ring timeout: above the callee's
	// 408 (25 s), so our own CANCEL never comes first, and below the
	// originator's 60 s answer window (the API refuses more than 59).
	CarrierTimeoutRingSec = 45
	// CarrierTimeoutMaxAttempts is the campaign's maxAttempts: above 1, so the
	// contact is retried and not exhausted by its one attempt.
	CarrierTimeoutMaxAttempts = 3
	// carrierTimeoutDefaultIntervalSec is the product's default NO_ANSWER
	// interval (CampaignRetryPolicy.intervalSec in the contract: 7200). The run
	// reads it back from the campaign the API created and fails when it is
	// anything else, so the case notices a changed default.
	carrierTimeoutDefaultIntervalSec = 7200
	// carrierTimeoutRingMs is the callee's 180 to its 408 in the scenario:
	// 25000 - 2000 ms.
	carrierTimeoutRingMs = 23000.0
	// carrierRingTolerance is how far the CDR's ringSec may sit from the
	// simulator's own reading of the same ring. ringSec is truncated to whole
	// seconds from stamps the switch took, so it may read up to a second under
	// the simulator's value, and the two processes see the same message a few
	// milliseconds apart.
	carrierRingTolerance = 1000.0
	// carrierScenarioTolerance bounds how far the simulator's own reading of
	// the ring may sit from the scenario's 23 s: a simulator that played
	// another script would make the comparison with ringSec meaningless.
	carrierScenarioTolerance = 1000.0
)

// CarrierTimeoutCause is the hangup cause the switch gives a far-end 408.
const CarrierTimeoutCause = causeRingTimer

var (
	// CarrierTimeoutCase is the one contact.
	CarrierTimeoutCase = Case{
		ID: "T1-N1", Kind: KindScenario, Suffix: "91",
		Callee: "180 at 2 s, then 408 at 25 s (no BYE, no answer)",
		Steps:  []Step{{Provisional: 180, SIPStatus: 408, FinalAfterMs: 25000, Outcome: "NO_ANSWER", Cause: CarrierTimeoutCause}},
	}
	CarrierTimeoutCases = []Case{CarrierTimeoutCase}
	// CarrierTimeoutRegion is the one country the case uses.
	CarrierTimeoutRegion = Regions[0]
)

var carrierTimeoutScopes = campaignScopes

// carrierTimeoutProbeBound is how long the CDR may take to appear after the
// attempt finished, and carrierTimeoutSettle how long the run waits after that
// for a late revision of the attempt, the CDR or the contact (a fuller CDR may
// replace the first), and for a stray second INVITE.
const (
	carrierTimeoutCDRBound = 30 * time.Second
	carrierTimeoutSettle   = 5 * time.Second
)

// CarrierTimeoutMeasure is what the run measured, printed beside the checks.
type CarrierTimeoutMeasure struct {
	// RingSec is the CDR's ringSec; SimRingMs the simulator's reading of the
	// same ring on its monotonic clock (the final response's afterInviteMs
	// minus the first provisional's). The wall-clock reading of the same two
	// stamps is SimRingWallMs, printed to show a step of the host's clock.
	RingSec       int     `json:"ringSec"`
	SimRingMs     float64 `json:"simRingMs"`
	SimRingWallMs float64 `json:"simRingWallMs"`
	// IntervalSec is nextAttemptAt minus the attempt's finishedAt, both switch
	// time.
	IntervalSec float64 `json:"intervalSec"`
	SIPStatus   *int    `json:"sipStatus,omitempty"`
	// RingStartedAt is the CDR's tech.ringStartedAt as sent, and
	// RingStartedToFinishedSec its distance to the attempt's finishedAt, both
	// switch time. Reported, not asserted.
	RingStartedAt             string   `json:"ringStartedAt,omitempty"`
	RingStartedToFinishedSecs *float64 `json:"ringStartedToFinishedSec,omitempty"`
}

// CarrierTimeoutReport is what a run writes to report.json.
type CarrierTimeoutReport struct {
	Scenario    string    `json:"scenario"`
	Passed      bool      `json:"passed"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	DurationSec float64   `json:"durationSec"`
	DialForm    string    `json:"dialForm"`
	Number      string    `json:"number"`
	DialKey     string    `json:"dialKey"`
	Timeline    []string  `json:"timeline"`

	Campaign api.Campaign             `json:"campaign"`
	Attempts []api.CampaignAttempt    `json:"attempts"`
	Contact  *api.CampaignContact     `json:"contact,omitempty"`
	CDRs     []api.CDR                `json:"cdrs"`
	Dialogs  []calleesim.DialogRecord `json:"dialogs"`
	Measure  CarrierTimeoutMeasure    `json:"measure"`
	Checks   CheckList                `json:"checks"`
	Failures []string                 `json:"failures"`
}

// carrierTimeoutObservation is everything the assertions read.
type carrierTimeoutObservation struct {
	// Key is the string the switch was handed, DIDNumber the campaign's
	// caller id.
	Key, DIDNumber string
	// Campaign is as the API created it (the resolved policy); CampaignNow as
	// read after the attempt, before it was cancelled.
	Campaign, CampaignNow api.Campaign
	// Attempts are the contact's attempts at the end; AfterFirst is the
	// contact as read right after attempt 1 was seen FINISHED, Contact as read
	// at the end.
	Attempts           []api.CampaignAttempt
	AfterFirst         *api.CampaignContact
	Contact            *api.CampaignContact
	CampaignCDRs       []api.CDR
	Dialogs            []calleesim.DialogRecord
	UnexpectedDialogs  int
	CancelledStatus    string
	CancelledAttempts  int
	HasCancelledReadBk bool
}

// assessCarrierTimeout makes every assertion of the case. The clocks: the
// ring is read on the simulator's monotonic clock; the retry interval and
// the ring's stamps are switch time, both ends from the switch.
func assessCarrierTimeout(o carrierTimeoutObservation) (CheckList, CarrierTimeoutMeasure) {
	var col collector
	var m CarrierTimeoutMeasure

	// The campaign: its own plan, as the API resolved it.
	camp := o.Campaign
	col.that(camp.RingTimeoutSec == CarrierTimeoutRingSec && camp.RingTimeoutSec > 25,
		"the campaign rings %d s, above the callee's 25 s 408 (%d)", CarrierTimeoutRingSec, camp.RingTimeoutSec)
	col.that(camp.RetryPolicy.IntervalSec["NO_ANSWER"] == carrierTimeoutDefaultIntervalSec,
		"the campaign's NO_ANSWER interval is the product default %d s (%d)", carrierTimeoutDefaultIntervalSec, camp.RetryPolicy.IntervalSec["NO_ANSWER"])
	col.that(camp.RetryPolicy.MaxAttempts >= 2, "maxAttempts allows a retry (%d)", camp.RetryPolicy.MaxAttempts)

	// The attempt.
	col.that(len(o.Attempts) == 1, "the contact has made 1 attempt, and no retry (%d)", len(o.Attempts))
	var att api.CampaignAttempt
	if len(o.Attempts) > 0 {
		att = o.Attempts[0]
	}
	col.that(att.State == attemptFinished, "the attempt is FINISHED (%s)", att.State)
	col.that(deref(att.Outcome) == "NO_ANSWER", "the attempt's outcome is NO_ANSWER, not TRANSIENT (%s)", deref(att.Outcome))
	col.that(deref(att.HangupCause) == CarrierTimeoutCause, "the attempt's hangup cause is %s (%s)", CarrierTimeoutCause, deref(att.HangupCause))
	col.that(att.FinishedAt != nil, "the attempt has a finishedAt")

	// The contact: one attempt spent, no system failure, waiting for the
	// retry, right after the attempt and at the end.
	for _, c := range []struct {
		when string
		ct   *api.CampaignContact
	}{{"after the attempt", o.AfterFirst}, {"at the end", o.Contact}} {
		if c.ct == nil {
			col.that(false, "%s: the contact was read", c.when)
			continue
		}
		col.that(c.ct.Attempts == 1 && c.ct.SystemFailures == 0,
			"%s: attempts = 1 and systemFailures = 0 (%d, %d)", c.when, c.ct.Attempts, c.ct.SystemFailures)
		col.that(c.ct.Status == api.CampaignContactStatusACTIVE,
			"%s: the contact is ACTIVE, waiting for its retry, not EXHAUSTED or DONE (%s)", c.when, c.ct.Status)
		col.that(c.ct.NextAttemptAt != nil, "%s: nextAttemptAt is set", c.when)
		col.that(deref(c.ct.LastOutcome) == "NO_ANSWER", "%s: the last outcome is NO_ANSWER (%s)", c.when, deref(c.ct.LastOutcome))
	}
	// nextAttemptAt and finishedAt are both stamped from the switch's event
	// timestamps, so their difference reads one clock. The dialer adds the
	// interval exactly; retryTolerance (2 s) covers the two stamps being
	// written apart, as in every campaign check of the same kind.
	if o.AfterFirst != nil && o.AfterFirst.NextAttemptAt != nil && att.FinishedAt != nil {
		gap := o.AfterFirst.NextAttemptAt.Sub(*att.FinishedAt)
		m.IntervalSec = gap.Seconds()
		want := carrierTimeoutDefaultIntervalSec * time.Second
		col.that(absDuration(gap-want) <= retryTolerance,
			"nextAttemptAt is the attempt's finishedAt + %d s, the NO_ANSWER interval (measured %s, tolerance %s)"+switchTime, carrierTimeoutDefaultIntervalSec, gap, retryTolerance)
	} else {
		col.that(false, "nextAttemptAt is the attempt's finishedAt + %d s, the NO_ANSWER interval (nextAttemptAt or finishedAt is missing)"+switchTime, carrierTimeoutDefaultIntervalSec)
	}
	col.that(o.CampaignNow.Status == api.CampaignStatusRUNNING, "the campaign is still RUNNING (%s)", o.CampaignNow.Status)
	col.that(o.CampaignNow.Activity != nil && *o.CampaignNow.Activity == api.CampaignActivityWAITINGRETRY,
		"the campaign is WAITING_RETRY (%s)", func() string {
			if o.CampaignNow.Activity == nil {
				return "no activity"
			}
			return string(*o.CampaignNow.Activity)
		}())

	// The simulator: one INVITE, 180 then 408, nothing from the caller.
	col.that(len(o.Dialogs) == 1, "the callee saw exactly 1 INVITE for %s during the run (%d)", o.Key, len(o.Dialogs))
	col.that(o.UnexpectedDialogs == 0, "no unexpected dialog at the callee (%d)", o.UnexpectedDialogs)
	var simRingOK bool
	if len(o.Dialogs) == 1 {
		d := o.Dialogs[0]
		first := firstRinging(d)
		col.that(first != nil, "the callee recorded a 180 or 183 (%d provisionals)", len(d.Provisionals))
		col.that(d.Final != nil && d.Final.Status == 408, "the callee recorded the final 408 (%s)", finalText(d))
		// A CANCEL and a BYE from our side are both ended "by the caller".
		col.that(d.End == nil || d.End.By != calleesim.EndedByCaller,
			"the caller never ended the dialog: no CANCEL, no BYE (%s)", endText(d))
		col.that(d.Final != nil && d.Final.AfterInviteMs < CarrierTimeoutRingSec*1000,
			"the 408 came before the ring timeout, so no CANCEL could have preceded it (at %.0f ms, timeout %d s)", finalAfterInvite(d), CarrierTimeoutRingSec)
		if first != nil && d.Final != nil {
			m.SimRingMs = d.Final.AfterInviteMs - first.AfterInviteMs
			m.SimRingWallMs = float64(d.Final.SentAt.Sub(first.SentAt).Milliseconds())
			simRingOK = math.Abs(m.SimRingMs-carrierTimeoutRingMs) <= carrierScenarioTolerance
			col.that(simRingOK, "the simulator's own ring, 180 to 408, is the scenario's 23 s (%.0f ms on its monotonic clock; %.0f ms as wall-clock stamps)", m.SimRingMs, m.SimRingWallMs)
		}
	}

	// The CDR.
	var mine []api.CDR
	for _, c := range o.CampaignCDRs {
		if c.CallID == att.CallID {
			mine = append(mine, c)
		}
	}
	col.that(len(o.CampaignCDRs) == 1 && len(mine) == 1, "the campaign has exactly one CDR, the attempt's (%d of %d)", len(mine), len(o.CampaignCDRs))
	if len(mine) == 1 {
		cdr := mine[0]
		col.that(deref(cdr.HangupCause) == CarrierTimeoutCause, "the CDR's hangup cause is %s (%s)", CarrierTimeoutCause, deref(cdr.HangupCause))
		col.that(cdr.AnsweredAt == nil && cdr.BillSec == 0 && cdr.BotSec == 0 && cdr.Status != api.CDRStatusANSWERED,
			"the CDR records no answer (status %s, billSec %d, botSec %d)", cdr.Status, cdr.BillSec, cdr.BotSec)
		col.that(cdr.ToNumber == o.Key, "the CDR's dialled number is %s (%s)", o.Key, cdr.ToNumber)
		col.that(deref(cdr.DID) == o.DIDNumber, "the CDR names the campaign's DID %s (%s)", o.DIDNumber, deref(cdr.DID))

		status, hasStatus := techNumber(cdr.Tech, "sipStatus")
		if hasStatus {
			s := int(status)
			m.SIPStatus = &s
		}
		col.that(hasStatus && status == 408, "tech.sipStatus is 408 (%s)", techText(cdr.Tech, "sipStatus"))
		raw, isString := techString(cdr.Tech, "ringStartedAt")
		col.that(isString, "tech.ringStartedAt is present as a string (%s)", techText(cdr.Tech, "ringStartedAt"))
		if isString {
			m.RingStartedAt = raw
			at, err := time.Parse(time.RFC3339Nano, raw)
			col.that(err == nil && strings.HasSuffix(raw, "Z") && strings.Contains(raw, "."),
				"tech.ringStartedAt is an RFC 3339 UTC timestamp with fractional seconds (%q)", raw)
			if err == nil && att.FinishedAt != nil {
				s := att.FinishedAt.Sub(at).Seconds()
				m.RingStartedToFinishedSecs = &s
			}
		}

		// The ring, against the simulator's own reading of it. Two processes'
		// wall clocks are never compared (#565): the simulator's value is on
		// its monotonic clock, the CDR's is the switch's own difference.
		m.RingSec = cdr.RingSec
		diff := math.Abs(float64(cdr.RingSec)*1000 - m.SimRingMs)
		col.that(simRingOK && cdr.RingSec > 0 && diff <= carrierRingTolerance,
			"the CDR's ringSec %d is within %.0f s of the simulator's 180 to 408, %.3f s on its monotonic clock (differs by %.3f s)",
			cdr.RingSec, carrierRingTolerance/1000, m.SimRingMs/1000, diff/1000)
	}

	// The campaign was cancelled, so the 2 h retry cannot fire.
	if o.HasCancelledReadBk {
		col.that(o.CancelledStatus == string(api.CampaignStatusCANCELLED), "the campaign was cancelled at the end, so the retry cannot fire (%s)", o.CancelledStatus)
		col.that(o.CancelledAttempts == 1, "the cancelled campaign still has 1 attempt (%d)", o.CancelledAttempts)
	}
	return CheckList{Checks: nonNil(col.checks), Failures: nonNil(col.failures)}, m
}

// firstRinging is the dialog's first 180 or 183.
func firstRinging(d calleesim.DialogRecord) *calleesim.ProvisionalRecord {
	for i := range d.Provisionals {
		if s := d.Provisionals[i].Status; s == 180 || s == 183 {
			return &d.Provisionals[i]
		}
	}
	return nil
}

func finalText(d calleesim.DialogRecord) string {
	if d.Final == nil {
		return "no final response"
	}
	return fmt.Sprint(d.Final.Status)
}

func finalAfterInvite(d calleesim.DialogRecord) float64 {
	if d.Final == nil {
		return 0
	}
	return d.Final.AfterInviteMs
}

func endText(d calleesim.DialogRecord) string {
	if d.End == nil {
		return "no end recorded"
	}
	return d.End.Method + " by " + d.End.By
}

// techNumber reads a numeric key of a CDR's tech, however the JSON decoder
// held it.
func techNumber(tech *map[string]interface{}, key string) (float64, bool) {
	if tech == nil {
		return 0, false
	}
	switch v := (*tech)[key].(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case int:
		return float64(v), true
	}
	return 0, false
}

func techString(tech *map[string]interface{}, key string) (string, bool) {
	if tech == nil {
		return "", false
	}
	s, ok := (*tech)[key].(string)
	return s, ok
}

// techText prints a tech key for a check's text, whatever it holds.
func techText(tech *map[string]interface{}, key string) string {
	if tech == nil {
		return "no tech"
	}
	v, ok := (*tech)[key]
	if !ok {
		return "absent"
	}
	return fmt.Sprintf("%v", v)
}

type carrierTimeoutRun struct {
	cfg CampaignConfig
	c   *httpc
	rep *CarrierTimeoutReport
	key string
}

func (r *carrierTimeoutRun) note(format string, args ...any) {
	r.rep.Timeline = append(r.rep.Timeline, fmt.Sprintf("%s  %s", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...)))
}

// RunCarrierTimeout runs the case against an application as `e2e.sh up`
// started it. It writes report.json to cfg.OutDir and returns the report, with
// an error when the run was refused, could not proceed, or an assertion failed.
func RunCarrierTimeout(ctx context.Context, cfg CampaignConfig) (*CarrierTimeoutReport, error) {
	cfg.Cases, cfg.Regions = CarrierTimeoutCases, []Region{CarrierTimeoutRegion}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}
	pre, err := preflight(ctx, cfg, "e2e-carrier-timeout", carrierTimeoutScopes)
	if err != nil {
		return nil, err
	}
	r := CarrierTimeoutRegion
	key := r.DialKey(CarrierTimeoutCase, pre.form)
	// preflight checks the case's responses, not its timing: the 180 must come
	// at 2 s and the 408 at 25 s, or the 23 s ring the CDR is held to is not
	// what the simulator plays.
	if err := checkCarrierScenarioTiming(pre.scenario, key); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	run := &carrierTimeoutRun{cfg: cfg, c: pre.c, key: key}
	run.rep = &CarrierTimeoutReport{
		Scenario: CarrierTimeoutScenarioID, StartedAt: time.Now().UTC(), DialForm: string(pre.form),
		Number: r.Number(CarrierTimeoutCase), DialKey: key,
		Attempts: []api.CampaignAttempt{}, CDRs: []api.CDR{}, Dialogs: []calleesim.DialogRecord{},
	}
	var chk CheckList
	obs, runErr := run.run(ctx)
	if runErr == nil {
		var m CarrierTimeoutMeasure
		chk, m = assessCarrierTimeout(obs)
		run.rep.Measure = m
		run.note("ringSec %d; simulator's ring (monotonic) %.0f ms, (wall) %.0f ms; nextAttemptAt - finishedAt %.1f s [switch time]; ringStartedAt %q",
			m.RingSec, m.SimRingMs, m.SimRingWallMs, m.IntervalSec, m.RingStartedAt)
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

// checkCarrierScenarioTiming refuses a simulator scenario whose case does not
// ring at 2000 ms and answer 408 at 25000 ms.
func checkCarrierScenarioTiming(s *calleesim.Scenario, key string) error {
	list := s.Numbers[key]
	if len(list) != 1 {
		return fmt.Errorf("the scenario has %d behaviours, want 1", len(list))
	}
	b := list[0]
	if b.Final() != 408 || b.FinalAfterMs != 25000 || len(b.Provisionals) != 1 || b.Provisionals[0].Status != 180 || b.Provisionals[0].AtMs != 2000 {
		return fmt.Errorf("the scenario plays %+v, want a 180 at 2000 ms and a final 408 at 25000 ms", b)
	}
	return nil
}

func (r *carrierTimeoutRun) run(ctx context.Context) (carrierTimeoutObservation, error) {
	obs := carrierTimeoutObservation{Key: r.key}
	reg := CarrierTimeoutRegion

	// The simulator answers a number's n-th INVITE with the n-th behaviour:
	// one that has seen the number would play nothing for this one.
	before, err := r.simulator(ctx)
	if err != nil {
		return obs, err
	}
	if n := countDialogs(before, r.key); n > 0 {
		return obs, fmt.Errorf("the simulator has already seen %d INVITE(s) for %s; restart it (deploy/e2e/e2e.sh carrier-timeout does)", n, r.key)
	}
	base, unexpectedBase := len(before.Dialogs), before.UnexpectedCount

	number := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
	did, err := r.c.publishFlowAndDID(ctx, r.cfg.FlowFile, "e2e-carrier-timeout-"+number, "e2e carrier timeout "+number, number, nil)
	if err != nil {
		return obs, err
	}
	obs.DIDNumber = did.Number

	// The campaign's own plan: ring 45 s, maxAttempts 3, and no interval at
	// all, so the NO_ANSWER interval is the product's default. The API returns
	// the resolved policy, which the assessment reads back.
	maxConcurrent, ring, attempts := 1, CarrierTimeoutRingSec, CarrierTimeoutMaxAttempts
	cc := reg.CountryCode
	var camp api.Campaign
	if err := r.c.do(ctx, http.MethodPost, "/campaigns", api.CampaignWrite{
		Name:               "e2e carrier timeout " + did.Number,
		DIDID:              did.ID,
		TimeZone:           reg.TimeZone,
		CallingWindow:      api.CampaignCallingWindow{Days: allDays, Ranges: []api.CampaignTimeRange{{Start: "00:00", End: "24:00"}}},
		MaxConcurrent:      &maxConcurrent,
		RingTimeoutSec:     &ring,
		DefaultCountryCode: &cc,
		RetryPolicy:        &api.CampaignRetryPolicyWrite{MaxAttempts: &attempts},
	}, &camp, http.StatusCreated); err != nil {
		return obs, fmt.Errorf("create campaign: %w", err)
	}
	obs.Campaign = camp
	id := camp.ID.String()
	// Whatever happens next, the 2 h retry must not be left to fire.
	cancelled := false
	defer func() {
		if !cancelled {
			r.cancel(context.WithoutCancel(ctx), id)
		}
	}()
	rows := []api.CampaignContactRow{{Number: reg.Number(CarrierTimeoutCase)}}
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
	r.note("started campaign %s (ring %d s, maxAttempts %d, NO_ANSWER interval %d s) for %s, dialled as %s",
		id, camp.RingTimeoutSec, camp.RetryPolicy.MaxAttempts, camp.RetryPolicy.IntervalSec["NO_ANSWER"], reg.Number(CarrierTimeoutCase), r.key)

	// Wait for attempt 1 to finish. Attempts are read before the contact: an
	// attempt's finish and its contact's counters commit together, so a contact
	// read after the attempt was seen FINISHED includes the finish.
	deadline := time.Now().Add(r.cfg.Wait)
	for {
		atts, err := r.attempts(ctx, id)
		if err != nil {
			return obs, err
		}
		if len(atts) > 0 && atts[0].State == attemptFinished {
			ct, err := r.contact(ctx, id)
			if err != nil {
				return obs, err
			}
			obs.AfterFirst = &ct
			r.note("attempt 1 finished: outcome %s, cause %s; contact %s, attempts %d, systemFailures %d, nextAttemptAt %s",
				outcomeOf(atts[0]), deref(atts[0].HangupCause), ct.Status, ct.Attempts, ct.SystemFailures, timeStr(ct.NextAttemptAt))
			break
		}
		if time.Now().After(deadline) {
			return obs, fmt.Errorf("attempt 1 did not finish within %s (%d attempts)", r.cfg.Wait, len(atts))
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return obs, err
		}
	}

	// The CDR, then a pause for anything that would revise the attempt, the
	// contact or the CDR, or for a stray second INVITE.
	cdrDeadline := time.Now().Add(carrierTimeoutCDRBound)
	for {
		cdrs, err := r.cdrs(ctx, id)
		if err != nil {
			return obs, err
		}
		if len(cdrs) > 0 {
			break
		}
		if time.Now().After(cdrDeadline) {
			return obs, fmt.Errorf("no CDR for the campaign within %s of the attempt finishing", carrierTimeoutCDRBound)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return obs, err
		}
	}
	if err := sleep(ctx, carrierTimeoutSettle); err != nil {
		return obs, err
	}
	if obs.Attempts, err = r.attempts(ctx, id); err != nil {
		return obs, err
	}
	ct, err := r.contact(ctx, id)
	if err != nil {
		return obs, err
	}
	obs.Contact = &ct
	if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id, nil, &obs.CampaignNow, http.StatusOK); err != nil {
		return obs, fmt.Errorf("read the campaign: %w", err)
	}
	if obs.CampaignCDRs, err = r.cdrs(ctx, id); err != nil {
		return obs, err
	}
	post, err := r.simulator(ctx)
	if err != nil {
		return obs, err
	}
	if base <= len(post.Dialogs) {
		for _, d := range post.Dialogs[base:] {
			if d.Number == r.key {
				obs.Dialogs = append(obs.Dialogs, d)
			}
		}
	}
	obs.UnexpectedDialogs = post.UnexpectedCount - unexpectedBase

	// Stop the campaign, so the retry two hours away cannot fire.
	r.cancel(ctx, id)
	cancelled = true
	var after api.Campaign
	if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id, nil, &after, http.StatusOK); err != nil {
		return obs, fmt.Errorf("read the campaign after cancelling it: %w", err)
	}
	obs.CancelledStatus, obs.HasCancelledReadBk = string(after.Status), true
	if list, err := r.attempts(ctx, id); err == nil {
		obs.CancelledAttempts = len(list)
	}
	r.rep.Campaign, r.rep.Attempts, r.rep.Contact, r.rep.CDRs, r.rep.Dialogs = obs.Campaign, obs.Attempts, obs.Contact, obs.CampaignCDRs, obs.Dialogs
	return obs, nil
}

func (r *carrierTimeoutRun) cancel(ctx context.Context, id string) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := r.c.do(cctx, http.MethodPost, "/campaigns/"+id+"/cancel", nil, nil, http.StatusOK, http.StatusConflict); err != nil {
		r.note("cancel campaign %s: %v", id, err)
		return
	}
	r.note("cancelled campaign %s", id)
}

func (r *carrierTimeoutRun) attempts(ctx context.Context, id string) ([]api.CampaignAttempt, error) {
	var l api.CampaignAttemptList
	if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/attempts?limit=100", nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	sort.SliceStable(l.Items, func(i, j int) bool { return l.Items[i].StartedAt.Before(l.Items[j].StartedAt) })
	return l.Items, nil
}

func (r *carrierTimeoutRun) contact(ctx context.Context, id string) (api.CampaignContact, error) {
	var l api.CampaignContactList
	if err := r.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/contacts?limit=100", nil, &l, http.StatusOK); err != nil {
		return api.CampaignContact{}, fmt.Errorf("list contacts: %w", err)
	}
	if len(l.Items) != 1 {
		return api.CampaignContact{}, fmt.Errorf("the campaign has %d contacts, want 1", len(l.Items))
	}
	return l.Items[0], nil
}

func (r *carrierTimeoutRun) cdrs(ctx context.Context, id string) ([]api.CDR, error) {
	var l api.CDRList
	if err := r.c.do(ctx, http.MethodGet, "/cdrs?limit=200&campaignId="+id, nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list cdrs: %w", err)
	}
	return l.Items, nil
}

func (r *carrierTimeoutRun) simulator(ctx context.Context) (calleesim.Report, error) {
	var rep calleesim.Report
	deadline := time.Now().Add(r.cfg.Wait)
	for {
		err := r.c.getJSON(ctx, r.cfg.CalleeSim+"/report", &rep)
		if err == nil {
			return rep, nil
		}
		if time.Now().After(deadline) {
			return rep, fmt.Errorf("simulator report: %w", err)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return rep, err
		}
	}
}
