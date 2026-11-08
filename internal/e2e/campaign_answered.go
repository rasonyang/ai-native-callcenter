// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// The answered cases of issue #246 (C01, C02, C04): written attempt by
// attempt (Case.Steps), and held to design 10 section 7.2's rule that an
// answered number is never dialled again, and to section 11's rule that a
// HIGH lead files exactly one callback.

// FirstAudioRow is one answered call's time from the callee's 200 OK to the
// first frame above silence it received. Reported, never asserted.
type FirstAudioRow struct {
	Region  string   `json:"region"`
	ID      string   `json:"id"`
	Attempt int      `json:"attempt"`
	CallID  string   `json:"callId"`
	Ms      *float64 `json:"firstAudioAfterAnswerMs"`
}

// stepOutcomes is a stepped case's expected outcomes, in attempt order.
func stepOutcomes(cs Case) string {
	out := make([]string, 0, len(cs.Steps))
	for _, s := range cs.Steps {
		out = append(out, s.Outcome)
	}
	if cs.IsListedMidRun {
		out = append(out, listedSkipOutcome)
	}
	return strings.Join(out, ", then ")
}

// assessSteppedContact checks a contact whose case lists its attempts: each
// attempt against its step, its CDR and its dialog; the contact after the
// first attempt and at the end; the lead and the callbacks it filed; and that
// nothing dialled it again after its answer.
func assessSteppedContact(rv RegionView, cv ContactView, key string, dialogs []calleesim.DialogRecord,
	cdrs map[string][]api.CDR, callbacks []api.Callback, observedAt time.Time) ScenarioResult {
	cs := cv.Case
	res := ScenarioResult{
		Region: rv.Region.Code, ID: cs.ID, Kind: cs.Kind, Number: cv.Number, DialKey: key, Callee: cs.Callee,
		Expected: stepOutcomes(cs), AfterFirst: contactResult(cv.AfterFirst), Contact: contactResult(cv.Contact),
		Attempts: []AttemptResult{}, Invites: len(dialogs),
	}
	var col Checks
	col.That(cv.Contact != nil, "the contact exists in the campaign")
	col.That(cv.AfterFirst != nil, "the contact was read right after its first attempt finished")
	n := len(cs.Steps)
	col.That(len(cv.Attempts) == n, "the contact has %d attempt(s) (%d)", n, len(cv.Attempts))
	col.That(len(dialogs) == n, "the simulator saw %d INVITE(s) for %s (%d)", n, key, len(dialogs))

	answeredAt := -1
	for i, a := range cv.Attempts {
		if i >= n {
			break
		}
		var dlg *calleesim.DialogRecord
		if i < len(dialogs) {
			dlg = &dialogs[i]
		}
		var next *time.Time
		if i+1 < len(cv.Attempts) {
			next = &cv.Attempts[i+1].StartedAt
		}
		ar := assessStep(rv, cs, i, a, key, dlg, cdrs[a.CallID.String()], next)
		res.Attempts = append(res.Attempts, ar)
		if cs.Steps[i].IsAnswered() && answeredAt < 0 {
			answeredAt = i
		}
	}

	// Right after the first attempt.
	if af := cv.AfterFirst; af != nil && n > 0 {
		col.That(af.Attempts == 1 && af.SystemFailures == 0,
			"after the first attempt: attempts = 1 and system_failures = 0 (%d, %d)", af.Attempts, af.SystemFailures)
		if cs.Steps[0].IsAnswered() {
			col.That(af.Status == api.CampaignContactStatus("DONE"), "after the first attempt: the contact is DONE, never to be dialled again (%s)", af.Status)
			col.That(af.NextAttemptAt == nil, "after the first attempt: no next attempt is scheduled")
		} else {
			col.That(af.Status == api.CampaignContactStatus("ACTIVE"), "after the first attempt: the contact is ACTIVE, due for a retry (%s)", af.Status)
			measured, ok := "none", false
			if len(cv.Attempts) > 0 && af.NextAttemptAt != nil && cv.Attempts[0].FinishedAt != nil {
				gap := af.NextAttemptAt.Sub(*cv.Attempts[0].FinishedAt)
				measured = gap.String()
				ok = absDuration(gap-RetryIntervalSec*time.Second) <= retryTolerance
			}
			col.That(ok, "after the first attempt: nextAttemptAt is its end + %d s, the policy's interval (measured %s)"+switchTime, RetryIntervalSec, measured)
		}
	}

	// At the end. Every attempt here reached the customer (a busy line or an
	// answer), so each counts against maxAttempts and none against the system
	// budget; an answer leaves the contact DONE whatever the budget says.
	last := cs.Steps[n-1]
	if ct := cv.Contact; ct != nil {
		col.That(ct.Attempts == n && ct.SystemFailures == 0,
			"at the end: attempts = %d and system_failures = 0 (%d, %d)", n, ct.Attempts, ct.SystemFailures)
		col.That(ct.Status == api.CampaignContactStatus("DONE"), "at the end: the contact is DONE (%s)", ct.Status)
		col.That(ct.NextAttemptAt == nil, "at the end: no next attempt is scheduled")
		col.That(deref(ct.LastOutcome) == last.Outcome, "at the end: the last outcome is %s (%s)", last.Outcome, deref(ct.LastOutcome))
		col.That(deref(ct.LeadIntent) == cs.Lead, "at the end: the contact's lead intent is %q (%q)", cs.Lead, deref(ct.LeadIntent))
	}

	// The retry of a case that was refused before it was answered.
	if n >= 2 && len(cv.Attempts) >= 2 && cv.AfterFirst != nil && cv.AfterFirst.NextAttemptAt != nil {
		late := cv.Attempts[1].StartedAt.Sub(*cv.AfterFirst.NextAttemptAt)
		ms := float64(late.Milliseconds())
		res.Attempts[1].RetryLatenessMs = &ms
		col.That(late >= -inviteSlack, "the retry did not start before nextAttemptAt (it started %s after it)"+switchTime, late)
		if len(dialogs) >= 2 {
			gap := dialogs[1].InviteAt.Sub(dialogs[0].InviteAt)
			gapMs := float64(gap.Milliseconds())
			res.Attempts[1].InviteGapMs = &gapMs
			col.That(gap >= RetryIntervalSec*time.Second-inviteSlack,
				"the two INVITEs are at least the %d s retry interval apart (%s)", RetryIntervalSec, gap.Round(time.Millisecond))
		}
	}

	// Never dialled again after the answer: the answered INVITE is the last
	// one the number received, and a contact answered on its first attempt
	// was watched for at least the retry interval afterwards.
	if answeredAt >= 0 && answeredAt < len(dialogs) {
		after := len(dialogs) - answeredAt - 1
		col.That(after == 0, "no INVITE reached %s after the answered one (%d did)", key, after)
	}
	if answeredAt == 0 && len(cv.Attempts) > 0 {
		from, anchor := watchAnchor(cv.Attempts[0], dialogs)
		watched := observedAt.Sub(from)
		col.That(watched >= RetryIntervalSec*time.Second,
			"the contact was watched for at least the %d s retry interval after its answered call (%s), with no further attempt (%s)",
			RetryIntervalSec, anchor, watched.Round(time.Second))
	}

	// The lead and its callbacks (design 10 section 11).
	var mine []api.Callback
	for _, a := range cv.Attempts {
		for _, cb := range callbacks {
			if cb.CallID != nil && *cb.CallID == a.CallID {
				mine = append(mine, cb)
			}
		}
	}
	res.Callbacks = mine
	col.That(len(mine) == cs.Callbacks, "the contact's calls filed exactly %d callback(s) (%d)", cs.Callbacks, len(mine))
	if answeredAt >= 0 && answeredAt < len(cv.Attempts) {
		a := cv.Attempts[answeredAt]
		col.That(deref(a.LeadIntent) == cs.Lead, "the answered attempt recorded lead intent %q (%q)", cs.Lead, deref(a.LeadIntent))
		if cs.Lead != "" {
			notes := ""
			if a.LeadNotes != nil {
				notes = *a.LeadNotes
			}
			col.That(notes == cs.LeadNotes, "the answered attempt recorded the lead notes %q (%q)", cs.LeadNotes, notes)
		}
		if len(mine) == 1 {
			cb := mine[0]
			col.That(*cb.CallID == a.CallID, "the callback is the answered call's (%s)", cb.CallID)
			col.That(cb.PhoneNumber == cv.Number, "the callback is for the contact's number %s (%s)", cv.Number, cb.PhoneNumber)
			col.That(cb.Message == cs.LeadNotes, "the callback carries the lead notes as its message (%q)", cb.Message)
			col.That(uuidText(cb.QueueID) == uuidText(rv.Campaign.FollowUpQueueID),
				"the callback is filed under the campaign's follow-up queue %q (%q)", uuidText(rv.Campaign.FollowUpQueueID), uuidText(cb.QueueID))
		}
	}

	res.Checks, res.Failures = nonNil(col.checks), nonNil(col.failures)
	for _, a := range res.Attempts {
		res.Failures = append(res.Failures, a.Failures...)
	}
	res.Passed = len(res.Failures) == 0
	return res
}

// assessStep checks one attempt of a stepped case.
func assessStep(rv RegionView, cs Case, i int, a api.CampaignAttempt, key string, dlg *calleesim.DialogRecord, cdrs []api.CDR, next *time.Time) AttemptResult {
	st := cs.Steps[i]
	pre := fmt.Sprintf("attempt %d: ", i+1)
	outcome, cause := deref(a.Outcome), deref(a.HangupCause)
	ar := AttemptResult{
		Index: i + 1, Number: a.Number, CallID: a.CallID.String(), LegID: a.LegID.String(), State: string(a.State),
		Outcome: outcome, HangupCause: cause, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, Dialog: dlg,
		CDRs: cdrs, Source: SourceDialer, IsAnswered: st.IsAnswered(),
	}
	if ar.CDRs == nil {
		ar.CDRs = []api.CDR{}
	}
	if len(cdrs) > 0 {
		ar.Source = SourceCDR
	}
	var col Checks
	col.That(a.State == attemptFinished, pre+"the attempt is FINISHED (%s)", a.State)
	col.That(outcome == st.Outcome, pre+"the outcome is %s (%s)", st.Outcome, outcome)
	if st.Cause != "" {
		col.That(cause == st.Cause, pre+"the hangup cause is %s (%s)", st.Cause, cause)
	} else {
		col.That(cause != "", pre+"the attempt carries a hangup cause (%q)", cause)
	}
	if !st.IsAnswered() {
		col.That(a.LeadIntent == nil, pre+"an unanswered attempt recorded no lead (%q)", deref(a.LeadIntent))
	}

	col.That(len(cdrs) == 1, pre+"exactly one CDR carries the call id (%d)", len(cdrs))
	if len(cdrs) == 1 {
		cdr := cdrs[0]
		col.That(deref(cdr.HangupCause) == cause, pre+"the CDR's hangup cause is the attempt's (%s)", deref(cdr.HangupCause))
		col.That(cdr.ToNumber == key, pre+"the CDR's dialled number is %s (%s)", key, cdr.ToNumber)
		col.That(deref(cdr.DID) == rv.DID.Number, pre+"the CDR names the campaign's DID %s (%s)", rv.DID.Number, deref(cdr.DID))
		col.That(cdr.CallType == api.CallType("OUTBOUND"), pre+"the CDR is typed OUTBOUND (%s)", cdr.CallType)
		bot := hasBotLeg(cdr)
		if st.IsAnswered() {
			col.That(cdr.AnsweredAt != nil, "%sthe CDR records the answer", pre)
			col.That(bot, "%sthe CDR shows the call reached the bot (a BOT leg)", pre)
		} else {
			col.That(cdr.AnsweredAt == nil && cdr.BotSec == 0 && !bot,
				pre+"the CDR records no answer and no bot (answered %v, botSec %d, BOT leg %v)", cdr.AnsweredAt != nil, cdr.BotSec, bot)
		}
	}

	col.That(dlg != nil, "%sthe simulator recorded the attempt's INVITE", pre)
	if dlg != nil {
		setSwitchOffset(&ar, a, dlg)
		assessStepDialog(&col, pre, rv, st, a, dlg, next, &ar)
	}
	ar.Checks, ar.Failures = nonNil(col.checks), nonNil(col.failures)
	ar.Passed = len(ar.Failures) == 0
	return ar
}

func assessStepDialog(col *Checks, pre string, rv RegionView, st Step, a api.CampaignAttempt, d *calleesim.DialogRecord, next *time.Time, ar *AttemptResult) {
	from := sipUser(d.InviteHeaders["from"])
	col.That(from == rv.DID.Number, pre+"the INVITE came from the campaign's caller id %s (%s)", rv.DID.Number, from)
	inWindow, desc := inviteWindow(d, a, next)
	col.That(inWindow, "%s%s", pre, desc)
	if st.Provisional != 0 {
		sent := false
		for _, p := range d.Provisionals {
			if p.Status == st.Provisional && p.HasEarlyMedia == st.EarlyMedia {
				sent = true
			}
		}
		col.That(sent, pre+"the callee sent %d (early media %v)", st.Provisional, st.EarlyMedia)
	}
	final := 0
	if d.Final != nil {
		final = d.Final.Status
	}
	col.That(final == st.SIPStatus, pre+"the callee answered %d (%d)", st.SIPStatus, final)
	if !st.IsAnswered() {
		return
	}
	col.That(d.AckAt != nil, "%sthe 200 OK was acknowledged", pre)
	wantBy := st.EndedBy()
	ended := d.End != nil && d.End.Method == calleesim.EndedWithBye && d.End.By == wantBy
	got := "nothing"
	if d.End != nil {
		got = d.End.By + " " + d.End.Method
	}
	col.That(ended, pre+"the dialog ended with a BYE from the %s (%s)", strings.ToLower(wantBy), got)
	if d.End != nil && d.Final != nil {
		ms := float64(d.End.At.Sub(d.Final.SentAt).Milliseconds())
		ar.AnswerToEndMs = &ms
	}
	var peak float64
	for _, b := range d.Downlink.Energy {
		if b.PeakRMS > peak {
			peak = b.PeakRMS
		}
	}
	col.That(peak > silenceRMS, pre+"the bot was heard: the downlink is above silence (peak rms %.0f, silence %d)", peak, silenceRMS)
	ar.FirstAudioAfterAnswerMs = d.Downlink.FirstAudioAfterAnswerMs
}

// hasBotLeg reports whether a CDR's journey reached the bot.
func hasBotLeg(cdr api.CDR) bool {
	for _, leg := range cdr.Legs {
		if leg.Kind == api.LegKind("BOT") {
			return true
		}
	}
	return false
}

func uuidText[T fmt.Stringer](p *T) string {
	if p == nil {
		return ""
	}
	return (*p).String()
}

// firstAudioRows lists every answered attempt's first-audio time.
func firstAudioRows(results []ScenarioResult) []FirstAudioRow {
	rows := []FirstAudioRow{}
	for _, s := range results {
		for _, a := range s.Attempts {
			if a.IsAnswered {
				rows = append(rows, FirstAudioRow{Region: s.Region, ID: s.ID, Attempt: a.Index, CallID: a.CallID, Ms: a.FirstAudioAfterAnswerMs})
			}
		}
	}
	return rows
}

// FirstAudioTable renders the first-audio times as Markdown.
func FirstAudioTable(rows []FirstAudioRow) string {
	var b strings.Builder
	b.WriteString("\n### First audio (200 OK to the first downlink frame above silence; reported, never asserted)\n\n")
	b.WriteString("| region | case | attempt | call id | first audio |\n|---|---|---|---|---|\n")
	for _, r := range rows {
		ms := "none"
		if r.Ms != nil {
			ms = fmt.Sprintf("%.0f ms", *r.Ms)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n", r.Region, r.ID, r.Attempt, r.CallID, ms)
	}
	return b.String()
}

// assessListedMidRun checks a contact whose number the driver listed on the
// do-not-call list while the contact waited for its retry (C10): attempt 1 is
// dialled and refused as the case's one step says; the listing happens after
// that attempt and before the retry is due; attempt 2 is then skipped
// (DNC_SKIPPED, NUMBER_ON_DNC) with no INVITE and no CDR, and the contact
// ends DNC. The number is dialled exactly once.
//
// The control is the case named by ControlID, which the callee treats the
// same way and nobody lists: the run must have dialled it a second time, after
// C10's retry came due, and read the simulator's record after that moment.
// Without that, a platform that dialled nothing from the retry's due time on
// would pass for one that skipped the number.
//
// Every ordering here is read on one clock, the host's, which the driver, the
// simulator and the dialer share. The retry falls due RetryIntervalSec after
// the callee's final response to attempt 1; the listing is held to that
// response, not to the attempt's finishedAt or the contact's nextAttemptAt,
// which are stamped from the switch's clock and drift from the host's (issue
// #504). Those two are only printed, marked as switch time. A dialog with no
// final response fails each check that needs it; nothing is skipped.
func assessListedMidRun(rv RegionView, cv ContactView, key string, dialogs, control []calleesim.DialogRecord,
	cdrs map[string][]api.CDR, calleeReadAt time.Time) ScenarioResult {
	cs := cv.Case
	res := ScenarioResult{
		Region: rv.Region.Code, ID: cs.ID, Kind: cs.Kind, Number: cv.Number, DialKey: key, Callee: cs.Callee,
		Expected: stepOutcomes(cs), AfterFirst: contactResult(cv.AfterFirst), Contact: contactResult(cv.Contact),
		Attempts: []AttemptResult{}, Invites: len(dialogs), Listing: cv.Listing,
	}
	var col Checks
	col.That(cv.Contact != nil, "the contact exists in the campaign")
	col.That(cv.AfterFirst != nil, "the contact was read right after its first attempt finished")
	col.That(len(cs.Steps) == 1, "the case describes the one dialled attempt (%d steps)", len(cs.Steps))
	col.That(len(cv.Attempts) == 2, "the contact has 2 attempts, the dialled one and the skipped one (%d)", len(cv.Attempts))
	col.That(len(dialogs) == 1, "the simulator saw exactly 1 INVITE for %s, the first (%d)", key, len(dialogs))

	// The attempts.
	for i, a := range cv.Attempts {
		switch {
		case i == 0 && len(cs.Steps) == 1:
			var dlg *calleesim.DialogRecord
			if len(dialogs) > 0 {
				dlg = &dialogs[0]
			}
			var next *time.Time
			if len(cv.Attempts) > 1 {
				next = &cv.Attempts[1].StartedAt
			}
			res.Attempts = append(res.Attempts, assessStep(rv, cs, 0, a, key, dlg, cdrs[a.CallID.String()], next))
		case i == 1:
			res.Attempts = append(res.Attempts, assessSkippedAttempt(a, cdrs[a.CallID.String()], cv.Listing, len(dialogs) > 1))
		}
	}

	// The listing: after attempt 1, confirmed on the list, and before the
	// retry came due; otherwise the case would not test what it claims to.
	// Both moments are the callee's final response (simulator clock) and the
	// retry interval after it.
	var final, due time.Time
	hasFinal := len(dialogs) > 0 && dialogs[0].Final != nil
	if hasFinal {
		final = dialogs[0].Final.SentAt
		due = final.Add(RetryIntervalSec * time.Second)
	}
	col.That(cv.Listing != nil, "the driver listed the number mid-run")
	if l := cv.Listing; l != nil {
		col.That(l.IsOnList, "the do-not-call list read back holds %s", cv.Number)
		if hasFinal {
			margin := due.Sub(l.ListedAt)
			col.That(margin > 0, "the number was listed %s before the retry came due (the callee's final response %s + %d s = %s, listed %s; the contact's nextAttemptAt reads %s [switch time])",
				margin.Round(time.Millisecond), final.Format(time.RFC3339Nano), RetryIntervalSec, due.Format(time.RFC3339Nano),
				l.ListedAt.Format(time.RFC3339Nano), l.NextAttemptAt.Format(time.RFC3339Nano))
			col.That(!l.PostedAt.Before(final), "the number was listed at or after the callee's final response to attempt 1 (%s after the %d, simulator clock)",
				l.PostedAt.Sub(final).Round(time.Millisecond), dialogs[0].Final.Status)
		} else {
			col.That(false, "the number was listed before the retry came due (the simulator recorded no final response to attempt 1, so the due moment is unknown)")
			col.That(false, "the number was listed at or after the callee's final response to attempt 1 (the simulator recorded no final response)")
		}
		if len(dialogs) > 0 {
			col.That(dialogs[0].InviteAt.Before(l.PostedAt), "the only INVITE (%s) came before the listing (%s)",
				dialogs[0].InviteAt.Format(time.RFC3339Nano), l.PostedAt.Format(time.RFC3339Nano))
		}
	}

	// The contact right after attempt 1 and at the end.
	if af := cv.AfterFirst; af != nil {
		col.That(af.Attempts == 1 && af.SystemFailures == 0,
			"after the first attempt: attempts = 1 and system_failures = 0 (%d, %d)", af.Attempts, af.SystemFailures)
		col.That(af.Status == api.CampaignContactStatus("ACTIVE"), "after the first attempt: the contact is ACTIVE, due for a retry (%s)", af.Status)
		measured, ok := "none", false
		if len(cv.Attempts) > 0 && af.NextAttemptAt != nil && cv.Attempts[0].FinishedAt != nil {
			gap := af.NextAttemptAt.Sub(*cv.Attempts[0].FinishedAt)
			measured = gap.String()
			ok = absDuration(gap-RetryIntervalSec*time.Second) <= retryTolerance
		}
		col.That(ok, "after the first attempt: nextAttemptAt is its end + %d s, the policy's interval (measured %s)"+switchTime, RetryIntervalSec, measured)
	}
	if ct := cv.Contact; ct != nil {
		col.That(ct.Status == api.CampaignContactStatus("DNC"), "at the end: the contact is DNC (%s)", ct.Status)
		col.That(ct.Attempts == 1 && ct.SystemFailures == 0,
			"at the end: attempts = 1 and system_failures = 0, the skipped attempt spending neither (%d, %d)", ct.Attempts, ct.SystemFailures)
		col.That(ct.NextAttemptAt == nil, "at the end: no next attempt is scheduled")
		col.That(deref(ct.LastOutcome) == listedSkipOutcome, "at the end: the last outcome is %s (%s)", listedSkipOutcome, deref(ct.LastOutcome))
	}

	// The control: dialled twice, the second time after C10's retry came due,
	// and the simulator's record was read after that.
	ctl := cs.ControlID
	col.That(ctl != "", "the case names a control that is never listed")
	col.That(len(control) == 2, "the control %s was dialled twice in this campaign, so the retries ran (%d INVITEs)", ctl, len(control))
	if cv.Listing != nil {
		switch {
		case !hasFinal:
			col.That(false, "the control's second INVITE came at or after the moment %s's retry was due (the simulator recorded no final response to attempt 1)", cs.ID)
			col.That(false, "the simulator's record was read after the retry came due (the simulator recorded no final response to attempt 1)")
		default:
			if len(control) == 2 {
				late := control[1].InviteAt.Sub(due)
				col.That(late >= -inviteSlack, "the control's second INVITE (%s) came at or after the moment %s's retry was due (%s after)",
					control[1].InviteAt.Format(time.RFC3339Nano), cs.ID, late.Round(time.Millisecond))
			}
			col.That(calleeReadAt.After(due),
				"the simulator's record was read (%s) after the retry came due (%s)", calleeReadAt.Format(time.RFC3339Nano), due.Format(time.RFC3339Nano))
		}
	}

	res.Checks, res.Failures = nonNil(col.checks), nonNil(col.failures)
	for _, a := range res.Attempts {
		res.Failures = append(res.Failures, a.Failures...)
	}
	res.Passed = len(res.Failures) == 0
	return res
}

// assessSkippedAttempt checks the attempt the do-not-call sweep skipped: it
// is finished by the dialer with the listing's outcome and cause, started
// after the listing, and neither reached the simulator nor made a CDR.
func assessSkippedAttempt(a api.CampaignAttempt, cdrs []api.CDR, l *Listing, hasDialog bool) AttemptResult {
	pre := "attempt 2: "
	outcome, cause := deref(a.Outcome), deref(a.HangupCause)
	ar := AttemptResult{
		Index: 2, Number: a.Number, CallID: a.CallID.String(), LegID: a.LegID.String(), State: string(a.State),
		Outcome: outcome, HangupCause: cause, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
		CDRs: cdrs, Source: SourceDialer,
	}
	if ar.CDRs == nil {
		ar.CDRs = []api.CDR{}
	}
	var col Checks
	col.That(a.State == attemptFinished, pre+"the attempt is FINISHED (%s)", a.State)
	col.That(outcome == listedSkipOutcome, pre+"the outcome is %s (%s)", listedSkipOutcome, outcome)
	col.That(cause == listedSkipCause, pre+"the hangup cause is %s (%s)", listedSkipCause, cause)
	col.That(len(cdrs) == 0, pre+"no CDR carries the call id (%d)", len(cdrs))
	col.That(!hasDialog, "%sno INVITE was sent", pre)
	if l != nil {
		col.That(!a.StartedAt.Before(l.PostedAt.Add(-inviteSlack)), pre+"the attempt started (%s) after the number was listed (%s)",
			a.StartedAt.Format(time.RFC3339Nano), l.PostedAt.Format(time.RFC3339Nano))
	}
	ar.Checks, ar.Failures = nonNil(col.checks), nonNil(col.failures)
	ar.Passed = len(ar.Failures) == 0
	return ar
}
