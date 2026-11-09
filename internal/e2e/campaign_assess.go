// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// retryTolerance is how far a contact's nextAttemptAt may sit from the end
// of the attempt plus the interval. The dialer adds the interval exactly; the
// slack covers the two timestamps being written apart, not the dialer's speed.
const retryTolerance = 2 * time.Second

// inviteSlack is how far before its attempt's start the simulator's INVITE
// may sit and still be that attempt's. The simulator and the dialer read the
// same host clock. The window's upper end, the next attempt's start, takes no
// slack, since a retry is at least a minute away: see inviteWindow.
const inviteSlack = time.Second

// switchTime marks a check that reads the switch's clock: an attempt's
// finishedAt and the contact's nextAttemptAt are stamped from the switch's
// event timestamps. That clock follows the kernel's monotonic clock and does
// not step, while the host's wall clock does (on Colima the VM's is stepped
// every 10 s), so the two part ways (issues #504, #506). Every other
// check anchors on the simulator, whose wall clock the driver and the dialer
// share. No tolerance is added for the offset: it is not bounded, so a fixed
// one would only make the failure come later. C07 reads the simulator's
// monotonic clock instead.
const switchTime = " [switch time]"

// simDialogEnd is the simulator's end of a dialog on the host clock: the end of
// the call when it ended, else the final response. It reports false when the
// simulator recorded neither.
func simDialogEnd(d *calleesim.DialogRecord) (time.Time, bool) {
	switch {
	case d == nil:
		return time.Time{}, false
	case d.End != nil:
		return d.End.At, true
	case d.Final != nil:
		return d.Final.SentAt, true
	}
	return time.Time{}, false
}

// setSwitchOffset records the attempt's measured switch-clock offset: its
// finishedAt (switch time) minus the end of its dialog (simulator time).
func setSwitchOffset(ar *AttemptResult, a api.CampaignAttempt, d *calleesim.DialogRecord) {
	if end, ok := simDialogEnd(d); ok && a.FinishedAt != nil {
		ms := float64(a.FinishedAt.Sub(end).Milliseconds())
		ar.SwitchOffsetMs = &ms
	}
}

// inviteWindow reports whether a dialog's INVITE is the attempt's. The lower
// bound is the attempt's start (dialer and simulator share a clock). The
// upper bound is the next attempt's start, which the dialer stamps too: the
// dialog must have ended by then. The attempt's own finishedAt is switch time
// and is not used. The last attempt has only the lower bound.
func inviteWindow(d *calleesim.DialogRecord, a api.CampaignAttempt, next *time.Time) (bool, string) {
	ok := !d.InviteAt.Before(a.StartedAt.Add(-inviteSlack))
	desc := fmt.Sprintf("the INVITE (%s) falls inside the attempt (started %s", d.InviteAt.Format(time.RFC3339Nano), a.StartedAt.Format(time.RFC3339Nano))
	if next == nil {
		return ok, desc + ", the last attempt)"
	}
	end, has := simDialogEnd(d)
	if !has {
		end = d.InviteAt
	}
	ok = ok && !end.After(*next)
	return ok, desc + fmt.Sprintf("; its dialog ended %s, the next attempt started %s)", end.Format(time.RFC3339Nano), next.Format(time.RFC3339Nano))
}

// Where an attempt's ending came from.
const (
	// SourceCDR: the CDR finished the attempt (the channel existed).
	SourceCDR = "CDR"
	// SourceJob: the originate's job result did (a CDR marked
	// tech.isPlacementFailure, design 10 section 5.5).
	SourceJob = "JOB"
	// SourceDialer: the dialer did, with no call placed (no CDR).
	SourceDialer = "DIALER"
)

// CampaignReport is what a run writes to report.json.
type CampaignReport struct {
	Passed     bool      `json:"passed"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// DurationSec is the whole run, sign-in to report.
	DurationSec float64 `json:"durationSec"`
	// DialForm is the deployment's AICC_OUTBOUND_DIAL_FORM.
	DialForm string `json:"dialForm"`
	// Latency is reported and never asserted.
	Latency   map[string]float64 `json:"latencyMs"`
	Campaigns []CampaignInfo     `json:"campaigns"`
	Scenarios []ScenarioResult   `json:"scenarios"`
	// Probes is the measured table, one row per region and case: SIP final
	// response, switch hangup cause, outcome, retry interval and budget.
	Probes []ProbeRow `json:"probes"`
	// FirstAudio is every answered call's time to the first word heard.
	FirstAudio []FirstAudioRow `json:"firstAudio"`
	Provider   ProviderCount   `json:"provider"`
	// Run holds the assertions about the run as a whole: the simulator's
	// verdict, the provider, and which campaign each dialog belongs to.
	Run CheckList `json:"run"`
	// CrossRegion asserts that the two countries come out the same.
	CrossRegion CheckList        `json:"crossRegion"`
	AfterRun    []AfterRunResult `json:"afterRun"`
	// Findings are observations for a person to judge; they fail nothing.
	Findings []string `json:"findings"`
	Failures []string `json:"failures"`
}

// CheckList is the assertions made, and those that failed.
type CheckList struct {
	Checks   []string `json:"checks"`
	Failures []string `json:"failures"`
}

// AfterRunResult is one AfterRunCheck's assertions.
type AfterRunResult struct {
	Name string `json:"name"`
	CheckList
}

// CampaignInfo identifies one campaign of the run.
type CampaignInfo struct {
	Region     string `json:"region"`
	CampaignID string `json:"campaignId"`
	DID        string `json:"did"`
	Status     string `json:"status"`
}

// ScenarioResult is one case in one region.
type ScenarioResult struct {
	Region string `json:"region"`
	ID     string `json:"id"`
	Kind   Kind   `json:"kind"`
	Number string `json:"number"`
	// DialKey is what the switch was handed: the Request-URI user part.
	DialKey string `json:"dialKey"`
	Callee  string `json:"callee"`
	// Expected is the outcome held to: the scenario's, or for a probe the
	// one its first attempt measured.
	Expected   string          `json:"expected"`
	Passed     bool            `json:"passed"`
	Checks     []string        `json:"checks"`
	Failures   []string        `json:"failures"`
	AfterFirst *ContactResult  `json:"contactAfterFirstAttempt,omitempty"`
	Contact    *ContactResult  `json:"contact,omitempty"`
	Attempts   []AttemptResult `json:"attempts"`
	// Invites is the simulator's INVITE count for the dial key in this run.
	Invites int `json:"invites"`
	// Callbacks are the callbacks the contact's calls filed (answered cases).
	Callbacks []api.Callback `json:"callbacks,omitempty"`
	// Listing is the driver's mid-run listing of the number (C10).
	Listing *Listing `json:"listedMidRun,omitempty"`
}

// ContactResult is a contact as read from the API.
type ContactResult struct {
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	SystemFailures int        `json:"systemFailures"`
	LastOutcome    string     `json:"lastOutcome,omitempty"`
	NextAttemptAt  *time.Time `json:"nextAttemptAt,omitempty"`
}

// AttemptResult is one attempt: what the API says of it, its CDR, the
// simulator's record of its dialog, and whether its own assertions held.
type AttemptResult struct {
	// Index counts from 1.
	Index       int       `json:"index"`
	Number      string    `json:"number"`
	CallID      string    `json:"callId"`
	LegID       string    `json:"legId"`
	State       string    `json:"state"`
	Outcome     string    `json:"outcome"`
	HangupCause string    `json:"hangupCause"`
	StartedAt   time.Time `json:"startedAt"`
	// FinishedAt is switch time.
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Source     string     `json:"source"`
	CDRs       []api.CDR  `json:"cdrs"`
	// Dialog is the simulator's record of this attempt's INVITE.
	Dialog   *calleesim.DialogRecord `json:"dialog,omitempty"`
	Passed   bool                    `json:"passed"`
	Checks   []string                `json:"checks"`
	Failures []string                `json:"failures"`
	// Bundle is the failure bundle's directory, relative to the run's.
	Bundle string `json:"bundle,omitempty"`
	// CancelAfterInviteMs is a ringing case's time from the INVITE to the
	// caller's CANCEL on the simulator's monotonic clock: the value C07's bound
	// reads. CancelAfterInviteWallMs is the same interval as two wall-clock
	// stamps and CancelAfterStartMs runs from the attempt's start to the CANCEL
	// on the wall clock. A gap between the monotonic and the wall value is a
	// step of the host's wall clock.
	CancelAfterInviteMs     *float64 `json:"cancelAfterInviteMs,omitempty"`
	CancelAfterInviteWallMs *float64 `json:"cancelAfterInviteWallMs,omitempty"`
	CancelAfterStartMs      *float64 `json:"cancelAfterStartMs,omitempty"`
	// RetryLatenessMs is how long after the previous nextAttemptAt this
	// attempt started. Reported, never asserted beyond "not early".
	RetryLatenessMs *float64 `json:"retryLatenessMs,omitempty"`
	// SwitchOffsetMs is the measured offset of the switch's clock from the
	// host's: the attempt's finishedAt (switch time) minus the end of its
	// dialog as the simulator saw it (host time). It includes the signalling
	// latency between the two events, added when the callee ended the dialog
	// and subtracted when the switch did (its BYE or CANCEL); it is set for an
	// attempt that has a dialog and a finish.
	SwitchOffsetMs *float64 `json:"switchOffsetMs,omitempty"`
	// InviteGapMs is the time from the previous attempt's INVITE to this
	// one's. Reported, never asserted beyond "not before the interval".
	InviteGapMs *float64 `json:"inviteGapMs,omitempty"`
	// IsAnswered marks an attempt the callee answered. FirstAudioAfterAnswerMs
	// runs from its 200 OK to the first downlink frame above silence, and
	// AnswerToEndMs to the BYE. Reported, never asserted.
	IsAnswered              bool     `json:"isAnswered,omitempty"`
	FirstAudioAfterAnswerMs *float64 `json:"firstAudioAfterAnswerMs,omitempty"`
	AnswerToEndMs           *float64 `json:"answerToEndMs,omitempty"`
}

// ProbeRow is one line of the measured table.
type ProbeRow struct {
	Region string `json:"region"`
	ID     string `json:"id"`
	Callee string `json:"callee"`
	// SIPFinal is the simulator's final response to the first INVITE: its
	// status, "487 after CANCEL" for a ring the caller gave up on, or
	// "none" when nothing was dialled.
	SIPFinal    string `json:"sipFinal"`
	HangupCause string `json:"hangupCause"`
	Outcome     string `json:"outcome"`
	Source      string `json:"source"`
	// Budget is the counter the first attempt spent, read off the contact.
	Budget Budget `json:"budget"`
	// RetryIntervalSec is nextAttemptAt minus the first attempt's end; nil
	// when the contact is not dialled again.
	RetryIntervalSec  *float64 `json:"retryIntervalSec"`
	ContactAfterFirst string   `json:"contactAfterFirstAttempt"`
	ContactFinal      string   `json:"contactFinal"`
	Attempts          int      `json:"attempts"`
	SystemFailures    int      `json:"systemFailures"`
	Invites           int      `json:"invites"`
	// Design is section 7.2's intent for this case: the scenario's expected
	// outcome, or this harness's reading of the SIP response (sipIntent).
	Design string `json:"design"`
}

// assess makes every assertion about a finished run and fills the report.
func assess(rep *CampaignReport, v *RunView, cases []Case, form phonenum.DialForm, unexpectedBase int) {
	rep.Provider = v.Provider

	for _, rv := range v.Regions {
		rep.Campaigns = append(rep.Campaigns, CampaignInfo{
			Region: rv.Region.Code, CampaignID: rv.Campaign.ID.String(), DID: rv.DID.Number, Status: string(rv.Campaign.Status),
		})
		cdrs := map[string][]api.CDR{}
		for _, cdr := range rv.CDRs {
			cdrs[cdr.CallID.String()] = append(cdrs[cdr.CallID.String()], cdr)
		}
		for _, cv := range rv.Contacts {
			key := rv.Region.DialKey(cv.Case, form)
			if cv.Case.IsListedMidRun {
				control := rv.Region.DialKey(caseByID(cases, cv.Case.ControlID), form)
				rep.Scenarios = append(rep.Scenarios, assessListedMidRun(rv, cv, key, dialogsFor(v.Dialogs, key),
					dialogsFor(v.Dialogs, control), cdrs, v.CalleeReadAt))
				continue
			}
			if len(cv.Case.Steps) > 0 {
				rep.Scenarios = append(rep.Scenarios, assessSteppedContact(rv, cv, key, dialogsFor(v.Dialogs, key), cdrs, v.Callbacks, v.ObservedAt))
				continue
			}
			rep.Scenarios = append(rep.Scenarios, assessContact(rv, cv, key, dialogsFor(v.Dialogs, key), cdrs, v.ObservedAt, &rep.Findings))
		}
	}

	// C07 and C08 name it: a ringing leg, with or without early media, must
	// not open a provider session (design 10 section 2, rule 1). The mock
	// provider cannot tell one call from another, so this is read off the
	// counts: it opened exactly as many sessions as the simulator answered
	// calls, which leaves none for a leg that only rang. The run-wide
	// invariant (AfterRunChecks) holds the application's own counter to it
	// as well.
	answered := answeredDialogs(v.Dialogs)
	noRingingSession := v.Provider.Sessions() == int64(answered) && v.Provider.After.Live == 0
	for i := range rep.Scenarios {
		sc := &rep.Scenarios[i]
		if cs := caseByID(cases, sc.ID); len(cs.Steps) > 0 || cs.SIPStatus != 0 || cs.IsOnDNC {
			continue
		}
		msg := fmt.Sprintf("no provider session was opened while it rang: the mock opened %d session(s) during the run, one per answered call (%d), and has %d live",
			v.Provider.Sessions(), answered, v.Provider.After.Live)
		sc.Checks = append(sc.Checks, msg)
		if !noRingingSession {
			sc.Failures = append(sc.Failures, msg)
			sc.Passed = false
		}
	}

	var run Checks
	run.That(v.Callee.UnexpectedCount == unexpectedBase,
		"the simulator saw no dialog its scenario does not know during the run (unexpected %d -> %d)", unexpectedBase, v.Callee.UnexpectedCount)
	assessAttribution(&run, v, form, rep.Scenarios)
	assessProviderScripts(&run, v, cases)
	rep.Run = run.list()

	rep.FirstAudio = firstAudioRows(rep.Scenarios)
	rep.Probes = probeRows(rep.Scenarios, cases)
	rep.CrossRegion = crossRegion(rep.Probes)
	if rep.Findings == nil {
		rep.Findings = []string{}
	}
}

// answeredDialogs is how many of the dialogs the simulator answered with 200.
func answeredDialogs(dialogs []calleesim.DialogRecord) int {
	n := 0
	for _, d := range dialogs {
		if d.Final != nil && d.Final.Status == 200 {
			n++
		}
	}
	return n
}

// assessProviderScripts checks what the mock provider's scripts did during
// the run: every scripted contact's call requested its tool once and got the
// result back, and no script failed or went unclaimed.
func assessProviderScripts(run *Checks, v *RunView, cases []Case) {
	scripted := 0
	for _, cs := range cases {
		if cs.Script != "" {
			scripted += cs.AnsweredSteps()
		}
	}
	scripted *= len(v.Regions)
	b, a := v.Provider.Before, v.Provider.After
	run.That(scripted > 0 && a.ToolCalls-b.ToolCalls == int64(scripted),
		"the mock provider requested %d tool call(s), one per scripted answered call (%d)", scripted, a.ToolCalls-b.ToolCalls)
	run.That(a.ToolResults-b.ToolResults == int64(scripted),
		"the application answered all %d tool call(s) (%d results)", scripted, a.ToolResults-b.ToolResults)
	run.That(a.ScriptsFailed == b.ScriptsFailed, "no mock provider script failed (%d -> %d)", b.ScriptsFailed, a.ScriptsFailed)
	run.That(a.Unmatched == b.Unmatched, "every provider session was claimed by a script (unclaimed %d -> %d)", b.Unmatched, a.Unmatched)
}

func caseByID(cases []Case, id string) Case {
	for _, cs := range cases {
		if cs.ID == id {
			return cs
		}
	}
	return Case{}
}

// dialogsFor are the run's dialogs whose Request-URI user is key, in order.
func dialogsFor(all []calleesim.DialogRecord, key string) []calleesim.DialogRecord {
	var out []calleesim.DialogRecord
	for _, d := range all {
		if d.Number == key {
			out = append(out, d)
		}
	}
	return out
}

// assessAttribution checks that every dialog of the run belongs to exactly
// one contact, came from that contact's campaign (its DID is the caller id in
// From), and that no campaign's INVITE carries the other country's number.
func assessAttribution(run *Checks, v *RunView, form phonenum.DialForm, results []ScenarioResult) {
	type owner struct {
		region RegionView
		cs     Case
	}
	owners := map[string]owner{}
	for _, rv := range v.Regions {
		for _, cv := range rv.Contacts {
			owners[rv.Region.DialKey(cv.Case, form)] = owner{rv, cv.Case}
		}
	}
	wantPerRegion := map[string]int{}
	for _, r := range results {
		wantPerRegion[r.Region] += r.Invites
	}

	unowned, wrongCaller := 0, 0
	perRegion := map[string]int{}
	foreign := map[string]int{}
	for _, d := range v.Dialogs {
		o, ok := owners[d.Number]
		if !ok {
			unowned++
			continue
		}
		perRegion[o.region.Region.Code]++
		from := sipUser(d.InviteHeaders["from"])
		if from != o.region.DID.Number {
			wrongCaller++
		}
		// The caller id names the campaign; the number must be that
		// campaign's country.
		for _, rv := range v.Regions {
			if from == rv.DID.Number && !strings.HasPrefix(d.Number, dialPrefix(rv.Region, form)) {
				foreign[rv.Region.Code]++
			}
		}
	}
	total := 0
	for _, n := range wantPerRegion {
		total += n
	}
	run.That(len(v.Dialogs) == total && total > 0, "the simulator saw exactly the run's %d INVITEs (%d)", total, len(v.Dialogs))
	run.That(unowned == 0, "every dialog of the run belongs to one contact of one campaign (%d do not)", unowned)
	run.That(wrongCaller == 0, "every dialog came from its own campaign's caller id (%d did not)", wrongCaller)
	for _, rv := range v.Regions {
		code := rv.Region.Code
		run.That(perRegion[code] == wantPerRegion[code] && perRegion[code] > 0,
			"%s: the simulator saw %d INVITEs for the campaign's numbers (%d)", code, wantPerRegion[code], perRegion[code])
		run.That(foreign[code] == 0, "%s: no INVITE from campaign %s (caller id %s) carries a number outside %s (%d did)",
			code, rv.Campaign.ID, rv.DID.Number, rv.Region.Prefix+"xx", foreign[code])
	}
}

// dialPrefix is a region's number prefix in the dial form.
func dialPrefix(r Region, form phonenum.DialForm) string {
	full, _ := phonenum.DialString(r.Prefix+"00", form, r.CountryCode)
	return full[:len(full)-2]
}

// sipUser is the user part of the SIP URI in a From, To or Contact value.
func sipUser(header string) string {
	_, rest, ok := strings.Cut(header, "sip:")
	if !ok {
		return ""
	}
	user, _, ok := strings.Cut(rest, "@")
	if !ok {
		return ""
	}
	return user
}

func contactResult(ct *api.CampaignContact) *ContactResult {
	if ct == nil {
		return nil
	}
	out := &ContactResult{Status: string(ct.Status), Attempts: ct.Attempts, SystemFailures: ct.SystemFailures, NextAttemptAt: ct.NextAttemptAt}
	if ct.LastOutcome != nil {
		out.LastOutcome = string(*ct.LastOutcome)
	}
	return out
}

func deref[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// assessContact checks one contact: its attempts, their CDRs and dialogs,
// its counters right after the first attempt and at the end, and the retry.
func assessContact(rv RegionView, cv ContactView, key string, dialogs []calleesim.DialogRecord,
	cdrs map[string][]api.CDR, observedAt time.Time, findings *[]string) ScenarioResult {
	cs := cv.Case
	res := ScenarioResult{
		Region: rv.Region.Code, ID: cs.ID, Kind: cs.Kind, Number: cv.Number, DialKey: key, Callee: cs.Callee,
		AfterFirst: contactResult(cv.AfterFirst), Contact: contactResult(cv.Contact), Attempts: []AttemptResult{},
		Invites: len(dialogs),
	}
	var col Checks
	col.That(cv.Contact != nil, "the contact exists in the campaign")
	col.That(cv.AfterFirst != nil, "the contact was read right after its first attempt finished")

	expected := cs.Outcome
	if cs.Kind == KindProbe && len(cv.Attempts) > 0 {
		expected = deref(cv.Attempts[0].Outcome)
	}
	res.Expected = expected
	design, known := designOutcomes[expected]
	col.That(known, "%q is an unanswered outcome of design 10 section 7.2", expected)
	retried := known && design.TerminalStatus == ""
	wantAttempts := 1
	if retried {
		wantAttempts = MaxAttempts
	}
	wantInvites := wantAttempts
	if cs.IsOnDNC {
		wantInvites = 0
	}
	col.That(len(cv.Attempts) == wantAttempts, "the contact has %d attempt(s) (%d)", wantAttempts, len(cv.Attempts))
	col.That(len(dialogs) == wantInvites, "the simulator saw %d INVITE(s) for %s (%d)", wantInvites, key, len(dialogs))

	firstCause := ""
	for i, a := range cv.Attempts {
		var dlg *calleesim.DialogRecord
		if i < len(dialogs) {
			dlg = &dialogs[i]
		}
		var next *time.Time
		if i+1 < len(cv.Attempts) {
			next = &cv.Attempts[i+1].StartedAt
		}
		res.Attempts = append(res.Attempts, assessAttempt(rv, cs, i, a, key, expected, dlg, cdrs[a.CallID.String()], next, &firstCause, findings))
	}

	spend := func(n int) (int, int) {
		switch design.Budget {
		case BudgetAttempts:
			return n, 0
		case BudgetSystem:
			return 0, n
		}
		return 0, 0
	}
	if af := cv.AfterFirst; af != nil && known {
		wa, ws := spend(1)
		col.That(af.Attempts == wa && af.SystemFailures == ws,
			"after the first attempt: attempts = %d and system_failures = %d (%d, %d)", wa, ws, af.Attempts, af.SystemFailures)
		if retried {
			col.That(af.Status == api.CampaignContactStatus("ACTIVE"), "after the first attempt: the contact is ACTIVE, due for a retry (%s)", af.Status)
			first := cv.Attempts[0]
			measured := "none"
			ok := false
			if af.NextAttemptAt != nil && first.FinishedAt != nil {
				gap := af.NextAttemptAt.Sub(*first.FinishedAt)
				measured = gap.String()
				ok = absDuration(gap-RetryIntervalSec*time.Second) <= retryTolerance
			}
			col.That(ok, "after the first attempt: nextAttemptAt is its end + %d s, the policy's interval (measured %s)"+switchTime, RetryIntervalSec, measured)
		} else {
			col.That(string(af.Status) == design.TerminalStatus, "after the first attempt: the contact is %s (%s)", design.TerminalStatus, af.Status)
			col.That(af.NextAttemptAt == nil, "after the first attempt: no next attempt is scheduled")
		}
	}
	if ct := cv.Contact; ct != nil && known {
		wa, ws := spend(wantAttempts)
		col.That(ct.Attempts == wa && ct.SystemFailures == ws,
			"at the end: attempts = %d and system_failures = %d (%d, %d)", wa, ws, ct.Attempts, ct.SystemFailures)
		want := design.TerminalStatus
		if retried {
			want = "EXHAUSTED" // both budgets are MaxAttempts = MaxSystemRetries = 2
		}
		col.That(string(ct.Status) == want, "at the end: the contact is %s (%s)", want, ct.Status)
		col.That(ct.NextAttemptAt == nil, "at the end: no next attempt is scheduled")
		col.That(deref(ct.LastOutcome) == expected, "at the end: the last outcome is %s (%s)", expected, deref(ct.LastOutcome))
	}
	if retried && len(cv.Attempts) >= 2 && cv.AfterFirst != nil && cv.AfterFirst.NextAttemptAt != nil {
		late := cv.Attempts[1].StartedAt.Sub(*cv.AfterFirst.NextAttemptAt)
		ms := float64(late.Milliseconds())
		res.Attempts[1].RetryLatenessMs = &ms
		col.That(late >= -inviteSlack, "the retry did not start before nextAttemptAt (it started %s after it)"+switchTime, late)
	}
	if !retried && len(cv.Attempts) >= 1 {
		from, anchor := watchAnchor(cv.Attempts[0], dialogs)
		watched := observedAt.Sub(from)
		col.That(watched >= RetryIntervalSec*time.Second,
			"the contact was watched for at least the %d s retry interval after its attempt (%s), with no further attempt (%s)", RetryIntervalSec, anchor, watched.Round(time.Second))
	}

	res.Checks, res.Failures = nonNil(col.checks), nonNil(col.failures)
	for _, a := range res.Attempts {
		res.Failures = append(res.Failures, a.Failures...)
	}
	res.Passed = len(res.Failures) == 0
	return res
}

// assessAttempt checks one attempt against its CDR and its dialog.
func assessAttempt(rv RegionView, cs Case, i int, a api.CampaignAttempt, key, expected string,
	dlg *calleesim.DialogRecord, cdrs []api.CDR, next *time.Time, firstCause *string, findings *[]string) AttemptResult {
	pre := fmt.Sprintf("attempt %d: ", i+1)
	outcome, cause := deref(a.Outcome), deref(a.HangupCause)
	ar := AttemptResult{
		Index: i + 1, Number: a.Number, CallID: a.CallID.String(), LegID: a.LegID.String(), State: string(a.State),
		Outcome: outcome, HangupCause: cause, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Dialog: dlg,
		CDRs: cdrs, Source: SourceDialer,
	}
	if ar.CDRs == nil {
		ar.CDRs = []api.CDR{}
	}
	if len(cdrs) > 0 {
		ar.Source = SourceCDR
		if cdrs[0].Tech != nil {
			if v, ok := (*cdrs[0].Tech)["isPlacementFailure"].(bool); ok && v {
				ar.Source = SourceJob
			}
		}
	}
	var col Checks
	col.That(a.State == attemptFinished, pre+"the attempt is FINISHED (%s)", a.State)
	col.That(outcome == expected, pre+"the outcome is %s (%s)", expected, outcome)
	switch {
	case cs.Kind == KindScenario:
		col.That(cause == cs.Cause, pre+"the hangup cause is %s (%s)", cs.Cause, cause)
	case i == 0:
		*firstCause = cause
		col.That(cause != "", pre+"the attempt carries a hangup cause (%q)", cause)
		if want, listed := designCauseOutcomes[cause]; listed {
			if ar.Source == SourceJob && designOutcomes[want].Budget == BudgetSystem {
				want = "ORIGINATE_FAILED" // a failure before any channel, section 5.5
			}
			col.That(outcome == want, pre+"design 10 section 7.2 maps %s to %s (%s)", cause, want, outcome)
		} else {
			*findings = append(*findings, fmt.Sprintf("%s %s: hangup cause %q is not in design 10 section 7.2; it came out %s", rv.Region.Code, cs.ID, cause, outcome))
		}
	default:
		col.That(cause == *firstCause, pre+"the hangup cause is the first attempt's %s (%s)", *firstCause, cause)
	}

	if cs.IsOnDNC {
		col.That(dlg == nil, "%sno INVITE was sent", pre)
	} else {
		col.That(len(cdrs) == 1, pre+"exactly one CDR carries the call id (%d)", len(cdrs))
		if len(cdrs) == 1 {
			cdr := cdrs[0]
			col.That(deref(cdr.HangupCause) == cause, pre+"the CDR's hangup cause is the attempt's (%s)", deref(cdr.HangupCause))
			col.That(cdr.ToNumber == key, pre+"the CDR's dialled number is %s (%s)", key, cdr.ToNumber)
			col.That(deref(cdr.DID) == rv.DID.Number, pre+"the CDR names the campaign's DID %s (%s)", rv.DID.Number, deref(cdr.DID))
			col.That(cdr.AnsweredAt == nil && cdr.BotSec == 0, pre+"the CDR records no answer and no bot time (answered %v, botSec %d)", cdr.AnsweredAt != nil, cdr.BotSec)
		}
		col.That(dlg != nil, "%sthe simulator recorded the attempt's INVITE", pre)
	}
	if dlg != nil {
		setSwitchOffset(&ar, a, dlg)
		assessDialog(&col, pre, rv, cs, a, dlg, next, &ar)
	}
	ar.Checks, ar.Failures = nonNil(col.checks), nonNil(col.failures)
	ar.Passed = len(ar.Failures) == 0
	return ar
}

func assessDialog(col *Checks, pre string, rv RegionView, cs Case, a api.CampaignAttempt, d *calleesim.DialogRecord, next *time.Time, ar *AttemptResult) {
	from := sipUser(d.InviteHeaders["from"])
	col.That(from == rv.DID.Number, pre+"the INVITE came from the campaign's caller id %s (%s)", rv.DID.Number, from)
	inWindow, desc := inviteWindow(d, a, next)
	col.That(inWindow, "%s%s", pre, desc)

	sent := false
	for _, p := range d.Provisionals {
		if p.Status == cs.Provisional && p.HasEarlyMedia == cs.EarlyMedia {
			sent = true
		}
	}
	col.That(sent, pre+"the callee sent %d (early media %v)", cs.Provisional, cs.EarlyMedia)
	final := 0
	if d.Final != nil {
		final = d.Final.Status
	}
	if cs.SIPStatus != 0 {
		col.That(final == cs.SIPStatus, pre+"the callee answered %d (%d)", cs.SIPStatus, final)
		return
	}
	// A ringing callee: never answered, and the caller gave up with a CANCEL.
	col.That(final == 487, pre+"the callee never answered; its INVITE was terminated with 487 (%d)", final)
	cancelled := d.End != nil && d.End.Method == calleesim.EndedWithCancel && d.End.By == calleesim.EndedByCaller
	col.That(cancelled, "%sthe caller ended the dialog with a CANCEL", pre)
	if !cancelled {
		return
	}
	// The ring is read on the simulator's monotonic clock: the host's wall
	// clock steps (on Colima by as much as a few seconds), so two wall-clock stamps can misstate
	// a 5 s ring by that much. The switch checks the ring timeout in whole
	// seconds from the start of the originate, which the INVITE trails, so a
	// true ring is 4 to 5 s from the originate and the lower bound allows
	// RingInviteLagAllowance below that. The wall-clock readings are reported
	// beside it; the gap between them is the step.
	ring := time.Duration(d.End.AfterInviteMs * float64(time.Millisecond))
	ms := d.End.AfterInviteMs
	ar.CancelAfterInviteMs = &ms
	wallMs := float64(d.End.At.Sub(d.InviteAt).Milliseconds())
	ar.CancelAfterInviteWallMs = &wallMs
	startMs := float64(d.End.At.Sub(a.StartedAt).Milliseconds())
	ar.CancelAfterStartMs = &startMs
	if cs.HasRingTimeoutBound {
		lo := RingTimeoutSec*time.Second - RingTimeoutTolerance - RingInviteLagAllowance
		hi := RingTimeoutSec*time.Second + RingTimeoutTolerance
		// A dialog with no measured value (zero) fails: a ring is never 0 ms.
		col.That(ring >= lo && ring <= hi,
			pre+"the CANCEL came at the %d s ring timeout, %s to %s after the INVITE (%.0f ms on the simulator's monotonic clock; wall clock: %.0f ms after the INVITE, %.0f ms after the attempt started)",
			RingTimeoutSec, lo, hi, ms, wallMs, startMs)
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// probeRows reads the measured table off the results: every case, scenarios
// included, since the table is what the switch and the platform did.
func probeRows(results []ScenarioResult, cases []Case) []ProbeRow {
	byID := map[string]Case{}
	for _, cs := range cases {
		byID[cs.ID] = cs
	}
	rows := make([]ProbeRow, 0, len(results))
	for _, s := range results {
		cs := byID[s.ID]
		row := ProbeRow{Region: s.Region, ID: s.ID, Callee: s.Callee, Invites: s.Invites, SIPFinal: "none"}
		switch {
		case len(cs.Steps) > 0:
			row.Design = stepOutcomes(cs)
		case cs.Kind == KindScenario:
			row.Design = cs.Outcome
		default:
			row.Design = sipIntent[cs.SIPStatus]
		}
		if len(s.Attempts) > 0 {
			a := s.Attempts[0]
			row.HangupCause, row.Outcome, row.Source = a.HangupCause, a.Outcome, a.Source
			if d := a.Dialog; d != nil && d.Final != nil {
				row.SIPFinal = fmt.Sprint(d.Final.Status)
				if d.End != nil && d.End.Method == calleesim.EndedWithCancel {
					row.SIPFinal += " after CANCEL"
				}
			}
			if af := s.AfterFirst; af != nil {
				row.ContactAfterFirst = af.Status
				switch {
				case af.Attempts == 1 && af.SystemFailures == 0:
					row.Budget = BudgetAttempts
				case af.Attempts == 0 && af.SystemFailures == 1:
					row.Budget = BudgetSystem
				case af.Attempts == 0 && af.SystemFailures == 0:
					row.Budget = BudgetNone
				default:
					row.Budget = Budget(fmt.Sprintf("attempts %d, system %d", af.Attempts, af.SystemFailures))
				}
				if af.NextAttemptAt != nil && a.FinishedAt != nil {
					sec := af.NextAttemptAt.Sub(*a.FinishedAt).Seconds()
					row.RetryIntervalSec = &sec
				}
			}
		}
		if s.Contact != nil {
			row.ContactFinal = s.Contact.Status
			row.Attempts, row.SystemFailures = s.Contact.Attempts, s.Contact.SystemFailures
		}
		rows = append(rows, row)
	}
	return rows
}

// crossRegion asserts that the countries come out the same on every count:
// SIP response, hangup cause, outcome, source, budget, retry interval and the
// contact's fate. A difference is a finding, so it fails the run.
func crossRegion(rows []ProbeRow) CheckList {
	var col Checks
	byID := map[string][]ProbeRow{}
	var order []string
	for _, p := range rows {
		if _, seen := byID[p.ID]; !seen {
			order = append(order, p.ID)
		}
		byID[p.ID] = append(byID[p.ID], p)
	}
	for _, id := range order {
		list := byID[id]
		col.That(len(list) == 2, "%s ran in both regions (%d)", id, len(list))
		for _, p := range list[1:] {
			q := list[0]
			col.That(q.signature() == p.signature() && sameInterval(q.RetryIntervalSec, p.RetryIntervalSec),
				"%s: %s and %s agree (%s | %s)"+switchTime, id, q.Region, p.Region, q.signature(), p.signature())
		}
	}
	return col.list()
}

func (p ProbeRow) signature() string {
	return strings.Join([]string{p.SIPFinal, p.HangupCause, p.Outcome, p.Source, string(p.Budget), p.ContactAfterFirst, p.ContactFinal,
		fmt.Sprintf("attempts=%d system=%d invites=%d retry=%s", p.Attempts, p.SystemFailures, p.Invites, intervalText(p.RetryIntervalSec))}, " / ")
}

func sameInterval(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return absDuration(time.Duration((*a-*b)*float64(time.Second))) <= retryTolerance
}

func intervalText(sec *float64) string {
	if sec == nil {
		return "none"
	}
	return fmt.Sprintf("%.0fs", *sec)
}

// watchAnchor is where the "watched for the retry interval" check starts
// counting for an attempt: the end of its dialog as the simulator saw it, or
// the attempt's own start (the dialer's clock) when the simulator recorded no
// end for it (the case has no dialog). Never the attempt's finishedAt, which
// is switch time.
func watchAnchor(a api.CampaignAttempt, dialogs []calleesim.DialogRecord) (time.Time, string) {
	if len(dialogs) > 0 {
		if end, ok := simDialogEnd(&dialogs[0]); ok {
			return end, "from the end of its dialog on the simulator's clock"
		}
	}
	return a.StartedAt, "from the attempt's start, the simulator having recorded no end of a dialog"
}

// SwitchClockTable renders the switch's measured clock offset as Markdown:
// per region, the smallest and largest SwitchOffsetMs over the attempts that
// have one, and how many that is.
func SwitchClockTable(results []ScenarioResult) string {
	type span struct {
		lo, hi float64
		n      int
	}
	by := map[string]*span{}
	var order []string
	for _, s := range results {
		for _, a := range s.Attempts {
			if a.SwitchOffsetMs == nil {
				continue
			}
			sp := by[s.Region]
			if sp == nil {
				sp = &span{lo: *a.SwitchOffsetMs, hi: *a.SwitchOffsetMs}
				by[s.Region] = sp
				order = append(order, s.Region)
			}
			sp.lo, sp.hi = min(sp.lo, *a.SwitchOffsetMs), max(sp.hi, *a.SwitchOffsetMs)
			sp.n++
		}
	}
	var b strings.Builder
	b.WriteString("\n### Switch clock (an attempt's finishedAt minus the end of its dialog on the simulator; includes signalling latency)\n\n")
	b.WriteString("Checks marked [switch time] read the switch's clock; every other check reads the simulator's and the driver's, which share the host's wall clock (C07 reads the simulator's monotonic clock).\n\n")
	b.WriteString("| region | min offset | max offset | attempts |\n|---|---|---|---|\n")
	for _, region := range order {
		sp := by[region]
		fmt.Fprintf(&b, "| %s | %.0f ms | %.0f ms | %d |\n", region, sp.lo, sp.hi, sp.n)
	}
	if len(order) == 0 {
		b.WriteString("| none | | | 0 |\n")
	}
	return b.String()
}

// ProbeTable renders the measured table as Markdown, one block per region.
func ProbeTable(rows []ProbeRow) string {
	var b strings.Builder
	region := ""
	for _, p := range rows {
		if p.Region != region {
			region = p.Region
			fmt.Fprintf(&b, "\n### %s\n\n", region)
			b.WriteString("| case | callee | SIP final | hangup_cause | outcome | source | budget | retry interval [switch time] | after 1st | final | attempts | system failures | INVITEs | section 7.2 |\n")
			b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %d | %d | %d | %s |\n",
			p.ID, p.Callee, p.SIPFinal, p.HangupCause, p.Outcome, p.Source, p.Budget, intervalText(p.RetryIntervalSec),
			p.ContactAfterFirst, p.ContactFinal, p.Attempts, p.SystemFailures, p.Invites, p.Design)
	}
	return b.String()
}

// collectFailures lists every failed assertion of the run, prefixed.
func collectFailures(rep *CampaignReport) []string {
	out := []string{}
	for _, f := range rep.Run.Failures {
		out = append(out, "run: "+f)
	}
	for _, s := range rep.Scenarios {
		for _, f := range s.Failures {
			out = append(out, fmt.Sprintf("%s %s: %s", s.Region, s.ID, f))
		}
	}
	for _, f := range rep.CrossRegion.Failures {
		out = append(out, "cross-region: "+f)
	}
	for _, a := range rep.AfterRun {
		for _, f := range a.Failures {
			out = append(out, a.Name+": "+f)
		}
	}
	return out
}

// writeBundles writes a directory per attempt of every failed scenario
// (bundles/<region>-<case>-<n>/): the attempt with its CDRs and the
// simulator's dialog, the scenario's failures, and ids.txt, the identifiers
// e2e.sh slices the switch and application logs by (call id, leg id, SIP
// Call-ID). A failed scenario with no attempt gets one bundle for the contact.
func writeBundles(outDir string, rep *CampaignReport) error {
	for i := range rep.Scenarios {
		sc := &rep.Scenarios[i]
		if sc.Passed {
			continue
		}
		write := func(name string, ids []string, attempt any) (string, error) {
			dir := filepath.Join("bundles", name)
			if err := os.MkdirAll(filepath.Join(outDir, dir), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(outDir, dir, "ids.txt"), []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(outDir, dir, "failures.txt"), []byte(strings.Join(sc.Failures, "\n")+"\n"), 0o644); err != nil {
				return "", err
			}
			return dir, writeJSON(filepath.Join(outDir, dir, "attempt.json"), attempt)
		}
		if len(sc.Attempts) == 0 {
			if _, err := write(fmt.Sprintf("%s-%s-contact", sc.Region, sc.ID), []string{sc.Number, sc.DialKey}, sc); err != nil {
				return err
			}
			continue
		}
		for j := range sc.Attempts {
			a := &sc.Attempts[j]
			ids := []string{a.CallID, a.LegID}
			if a.Dialog != nil {
				ids = append(ids, a.Dialog.CallID)
			}
			dir, err := write(fmt.Sprintf("%s-%s-%d", sc.Region, sc.ID, a.Index), ids, a)
			if err != nil {
				return err
			}
			a.Bundle = dir
		}
	}
	return nil
}
