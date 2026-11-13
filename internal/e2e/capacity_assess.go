// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// More of the application's counters, read by the capacity scenarios (names
// from internal/obs/callmetrics.go, as the Prometheus exporter writes them).
const (
	// MetricBotOverflow counts answered outbound calls hung up because no
	// bot slot freed within the hold.
	MetricBotOverflow = "aicc_bot_overflow_total"
	// MetricCampaignOverflow is the same per campaign.
	MetricCampaignOverflow = "aicc_campaign_overflow_total"
	// MetricOverflowHoldCount and MetricOverflowHoldSum are the overflow hold
	// histogram (aicc_bot_overflow_hold_ms, unit ms) by outcome.
	MetricOverflowHoldCount = "aicc_bot_overflow_hold_ms_milliseconds_count"
	MetricOverflowHoldSum   = "aicc_bot_overflow_hold_ms_milliseconds_sum"
	// MetricDialerBlocked counts the times the dialer had a contact to dial
	// and did not, by reason.
	MetricDialerBlocked = "aicc_dialer_admission_blocked_total"
)

// PeakConcurrent is the largest number of dialogs that were up at the same
// moment, each from its INVITE to its end (dialogEnd), read on the
// simulator's monotonic timeline: a step of the wall clock between one
// dialog's end and the next one's INVITE would otherwise show them up
// together. At one instant an end is counted before a start, since a line
// handed back is free for the next call. A dialog that never ended counts as
// up to the end of the record, and unended says how many did.
func PeakConcurrent(dialogs []calleesim.DialogRecord) (peak, unended int) {
	type edge struct {
		ms    float64
		delta int
	}
	edges := make([]edge, 0, 2*len(dialogs))
	for _, d := range dialogs {
		edges = append(edges, edge{d.InviteAfterStartMs, +1})
		if end, ok := dialogEnd(d); ok {
			edges = append(edges, edge{end.ms, -1})
		} else {
			unended++
		}
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].ms != edges[j].ms {
			return edges[i].ms < edges[j].ms
		}
		return edges[i].delta < edges[j].delta
	})
	up := 0
	for _, e := range edges {
		up += e.delta
		peak = max(peak, up)
	}
	return peak, unended
}

// downlinkPeak is the loudest 100 ms window the callee received, and how
// many frames it received in all.
func downlinkPeak(d calleesim.DialogRecord) (float64, int) {
	var peak float64
	for _, b := range d.Downlink.Energy {
		peak = max(peak, b.PeakRMS)
	}
	return peak, d.Downlink.Frames
}

func ptr[T any](v T) *T { return &v }

// row condenses one call for the report. Its times from the run's first
// INVITE are wall-clock readings, reported only; its hold is read on the
// simulator's monotonic clock (K3 asserts it), with the wall clock's reading
// beside it.
func row(region, id, number string, d *calleesim.DialogRecord, cdrs []api.CDR, t0 simMoment) CapacityRow {
	r := CapacityRow{Region: region, ID: id, Number: number}
	if len(cdrs) == 1 {
		r.CallID = cdrs[0].CallID.String()
		r.HangupCause = deref(cdrs[0].HangupCause)
		r.HasBotLeg = hasBotLeg(cdrs[0]) || cdrs[0].BotSec > 0
	}
	if d == nil {
		return r
	}
	r.InviteAtMs = inviteMoment(*d).wallSinceMs(t0)
	if fin, ok := finalMoment(*d); ok {
		r.FinalStatus = d.Final.Status
		if d.Final.Status == 200 {
			r.AnswerAtMs = ptr(fin.wallSinceMs(t0))
		}
	}
	if end, ok := dialogEnd(*d); ok {
		r.EndAtMs = ptr(end.wallSinceMs(t0))
	}
	if ms, wallMs, ok := holdMsOf(*d); ok {
		r.HoldMs, r.HoldWallMs = ptr(ms), ptr(wallMs)
	}
	if d.End != nil {
		r.EndedBy = d.End.By + " " + d.End.Method
	}
	r.DownlinkPeakRMS, r.DownlinkFrames = downlinkPeak(*d)
	return r
}

// firstInvite is the earliest INVITE of the run on the simulator's monotonic
// timeline, the zero of the report's times.
func firstInvite(v *RunView) simMoment {
	var t0 simMoment
	seen := false
	consider := func(d calleesim.DialogRecord) {
		if m := inviteMoment(d); !seen || m.before(t0) {
			t0, seen = m, true
		}
	}
	for _, d := range v.Dialogs {
		consider(d)
	}
	for _, dc := range v.Direct {
		if dc.Dialog != nil {
			consider(*dc.Dialog)
		}
	}
	return t0
}

// atMs prints a moment of the run on both clocks, from the run's first INVITE.
func atMs(m, t0 simMoment) string {
	return fmt.Sprintf("%.0f ms (wall clock %.0f ms)", m.sinceMs(t0), m.wallSinceMs(t0))
}

func metricDelta(v *RunView, name string, match map[string]string) (float64, int) {
	before, _ := v.MetricsBefore.Sum(name, match)
	after, n := v.MetricsAfter.Sum(name, match)
	return after - before, n
}

// AssessOverflow makes K1's assertions: one contact reached the bot and two
// were hung up as OVERFLOW, silent, and none of them was dialled again. It
// first holds the run to what makes that split the only possible one: every
// contact was ringing before any answered, and the direct calls had taken
// their slots before the first contact answered and still held them when the
// last overflow ended. Hold durations and the spread of the answers are
// reported, never asserted.
func AssessOverflow(v *RunView, hold time.Duration, c *Checks) (contacts, direct []CapacityRow) {
	contacts, direct = []CapacityRow{}, []CapacityRow{}
	c.That(len(v.Regions) == 1, "K1 runs one campaign (%d)", len(v.Regions))
	if len(v.Regions) != 1 {
		return contacts, direct
	}
	rv := v.Regions[0]
	t0 := firstInvite(v)

	type contactCall struct {
		cv      ContactView
		dialogs []calleesim.DialogRecord
		cdrs    []api.CDR
		outcome string
	}
	cdrsByCall := map[string][]api.CDR{}
	for _, cdr := range rv.CDRs {
		cdrsByCall[cdr.CallID.String()] = append(cdrsByCall[cdr.CallID.String()], cdr)
	}
	var calls []contactCall
	counts := map[string]int{}
	for _, cv := range rv.Contacts {
		cc := contactCall{cv: cv, dialogs: dialogsFor(v.Dialogs, rv.Region.DialKey(cv.Case, v.Form))}
		if len(cv.Attempts) > 0 {
			cc.cdrs = cdrsByCall[cv.Attempts[0].CallID.String()]
			cc.outcome = deref(cv.Attempts[0].Outcome)
		}
		counts[cc.outcome]++
		calls = append(calls, cc)
	}

	// The split.
	c.That(len(calls) == 3 && counts["ANSWERED"] == 1 && counts[outcomeOverflow] == 2,
		"of the 3 contacts that answered together, 1 reached the bot and 2 were OVERFLOW (%d contacts: %v)", len(calls), counts)

	// What makes the split the only possible one. The order is read on the
	// simulator's monotonic timeline; the times printed run from the first
	// INVITE, with the wall clock's reading beside each.
	var lastInvite, firstAnswer, lastOverflowEnd simMoment
	hasInvite, answered, overflowEnded := false, 0, false
	for _, cc := range calls {
		for _, d := range cc.dialogs {
			if inv := inviteMoment(d); !hasInvite || lastInvite.before(inv) {
				lastInvite, hasInvite = inv, true
			}
			if fin, ok := finalMoment(d); ok && d.Final.Status == 200 {
				if answered == 0 || fin.before(firstAnswer) {
					firstAnswer = fin
				}
				answered++
			}
			if end, ok := dialogEnd(d); ok && cc.outcome == outcomeOverflow && (!overflowEnded || lastOverflowEnd.before(end)) {
				lastOverflowEnd, overflowEnded = end, true
			}
		}
	}
	c.That(answered == 3 && lastInvite.before(firstAnswer),
		"precondition: all 3 contacts were ringing at once, every INVITE before the first answer (%d answered; last INVITE at %s, first answer at %s)",
		answered, atMs(lastInvite, t0), atMs(firstAnswer, t0))
	var lastHolderAnswer, firstHolderEnd simMoment
	holders, holderEnded := 0, false
	for _, dc := range v.Direct {
		if !dc.IsAnswered() {
			continue
		}
		if fin, _ := finalMoment(*dc.Dialog); holders == 0 || lastHolderAnswer.before(fin) {
			lastHolderAnswer = fin
		}
		holders++
		if end, ok := dialogEnd(*dc.Dialog); ok && (!holderEnded || end.before(firstHolderEnd)) {
			firstHolderEnd, holderEnded = end, true
		}
	}
	c.That(len(v.Direct) == 2 && holders == 2 && answered > 0 && lastHolderAnswer.before(firstAnswer),
		"precondition: both direct calls were answered, taking their slots, before the first contact answered (%d of %d answered; last at %s, first contact answer at %s)",
		holders, len(v.Direct), atMs(lastHolderAnswer, t0), atMs(firstAnswer, t0))
	c.That(overflowEnded && holderEnded && lastOverflowEnd.before(firstHolderEnd),
		"precondition: both direct calls still held their slots when the last OVERFLOW leg was hung up (first direct call ended at %s, last overflow at %s)",
		atMs(firstHolderEnd, t0), atMs(lastOverflowEnd, t0))

	// Each contact.
	var answers []float64
	for _, cc := range calls {
		pre := fmt.Sprintf("%s (%s): ", cc.cv.Case.ID, cc.cv.Number)
		var dlg *calleesim.DialogRecord
		if len(cc.dialogs) > 0 {
			dlg = &cc.dialogs[0]
		}
		r := row(rv.Region.Code, cc.cv.Case.ID, cc.cv.Number, dlg, cc.cdrs, t0)
		r.Outcome, r.Invites = cc.outcome, len(cc.dialogs)
		if cc.cv.Contact != nil {
			r.ContactEnd = string(cc.cv.Contact.Status)
		}
		contacts = append(contacts, r)

		c.That(len(cc.cv.Attempts) == 1 && cc.cv.Attempts[0].State == attemptFinished,
			pre+"exactly one attempt, FINISHED (%d)", len(cc.cv.Attempts))
		c.That(len(cc.dialogs) == 1, pre+"exactly one INVITE over the run and the 5 s after it: an answered number, OVERFLOW included, is never dialled again (%d)", len(cc.dialogs))
		c.That(cc.cv.Contact != nil && cc.cv.Contact.Status == api.CampaignContactStatus("DONE") && cc.cv.Contact.Attempts == 1,
			pre+"the contact is DONE after its one attempt (%s)", r.ContactEnd)
		c.That(len(cc.cdrs) == 1, pre+"exactly one CDR (%d)", len(cc.cdrs))
		if dlg == nil || len(cc.cdrs) != 1 {
			continue
		}
		cdr := cc.cdrs[0]
		c.That(dlg.Final != nil && dlg.Final.Status == 200 && dlg.AckAt != nil, "%sthe callee answered 200 and the switch acknowledged it", pre)
		c.That(cdr.AnsweredAt != nil, "%sthe CDR records the answer", pre)
		if r.AnswerAtMs != nil {
			answers = append(answers, *r.AnswerAtMs)
		}
		switch cc.outcome {
		case outcomeOverflow:
			c.That(r.HangupCause == outcomeOverflow, pre+"the CDR's hangup cause is OVERFLOW (%s)", r.HangupCause)
			c.That(!r.HasBotLeg, "%sthe CDR shows no bot leg and no bot seconds", pre)
			c.That(dlg.End != nil && dlg.End.By == calleesim.EndedByCaller && dlg.End.Method == calleesim.EndedWithBye,
				pre+"the platform hung the leg up with a BYE (%s)", r.EndedBy)
			c.That(r.DownlinkPeakRMS <= silenceRMS,
				pre+"the leg heard nothing: no downlink window above silence (peak rms %.0f, silence %d, %d frames)", r.DownlinkPeakRMS, silenceRMS, r.DownlinkFrames)
		case "ANSWERED":
			c.That(r.HangupCause != outcomeOverflow && r.HasBotLeg, pre+"the CDR shows the call reached the bot (cause %s, bot leg %v)", r.HangupCause, r.HasBotLeg)
			c.That(dlg.End != nil && dlg.End.By == calleesim.EndedByCallee && dlg.End.Method == calleesim.EndedWithBye,
				pre+"the callee ended the call with a BYE after 6 s (%s)", r.EndedBy)
			c.That(r.DownlinkPeakRMS > silenceRMS,
				pre+"the bot was heard, so the simulator measures this path: the downlink is above silence (peak rms %.0f, %d frames)", r.DownlinkPeakRMS, r.DownlinkFrames)
		}
	}
	if len(answers) > 1 {
		sort.Float64s(answers)
		c.That(true, "reported: the contacts' answers spread over %.0f ms; each OVERFLOW leg's hold is in the table (configured hold %s)", answers[len(answers)-1]-answers[0], hold)
	}

	// The direct calls.
	for i, dc := range v.Direct {
		pre := fmt.Sprintf("direct call %d (%s): ", i+1, dc.Number)
		r := row(rv.Region.Code, fmt.Sprintf("holder-%d", i+1), dc.Number, dc.Dialog, dc.CDRs, t0)
		if dc.Dialog != nil {
			r.Invites = 1
		}
		direct = append(direct, r)
		c.That(dc.Dialog != nil && dc.IsAnswered(), "%sthe simulator answered it", pre)
		c.That(len(dc.CDRs) == 1 && r.HasBotLeg && r.HangupCause != outcomeOverflow,
			pre+"exactly one CDR, and it reached the bot (%d CDRs, cause %s)", len(dc.CDRs), r.HangupCause)
		c.That(dc.Dialog != nil && dc.Dialog.End != nil && dc.Dialog.End.By == calleesim.EndedByCallee,
			pre+"the callee hung up, 12 s after answering (%s)", r.EndedBy)
	}

	// The application's own count.
	overflow := counts[outcomeOverflow]
	n, series := metricDelta(v, MetricBotOverflow, nil)
	c.That(series > 0 && n == float64(overflow) && overflow > 0, "%s moved by the %d OVERFLOW call(s) (+%.0f)", MetricBotOverflow, overflow, n)
	n, series = metricDelta(v, MetricCampaignOverflow, map[string]string{"campaign": rv.Campaign.ID.String()})
	c.That(series == 1 && n == float64(overflow), "%s{campaign} moved by %d (+%.0f)", MetricCampaignOverflow, overflow, n)
	held, series := metricDelta(v, MetricOverflowHoldCount, map[string]string{"outcome": outcomeOverflow})
	sum, _ := metricDelta(v, MetricOverflowHoldSum, map[string]string{"outcome": outcomeOverflow})
	mean := 0.0
	if held > 0 {
		mean = sum / held
	}
	c.That(series == 1 && held == float64(overflow),
		"the application recorded %d overflow hold(s) (+%.0f; reported: mean hold %.0f ms as it measured it, configured %s)", overflow, held, mean, hold)
	return contacts, direct
}

// AssessLineLimit makes K2's assertions, per campaign and DID: the simulator
// never had more dialogs up at once from the DID than its limit, and had
// exactly that many at the peak, so the limit is what held the campaign back;
// every contact was dialled once and rang out; the line gauge never read
// above the limit. The dialer reporting DID_LINES as its reason for holding a
// contact back is asserted too.
func AssessLineLimit(v *RunView, limit int, gaugePeak map[string]float64, gaugeSamples int, atCapacity map[string]bool, c *Checks) (contacts []CapacityRow, lines []LineRow) {
	contacts, lines = []CapacityRow{}, []LineRow{}
	c.That(len(v.Regions) > 0, "K2 runs a campaign per region (%d)", len(v.Regions))
	t0 := firstInvite(v)
	for _, rv := range v.Regions {
		code := rv.Region.Code
		cdrsByCall := map[string][]api.CDR{}
		for _, cdr := range rv.CDRs {
			cdrsByCall[cdr.CallID.String()] = append(cdrsByCall[cdr.CallID.String()], cdr)
		}
		attempted := 0
		for _, cv := range rv.Contacts {
			pre := fmt.Sprintf("%s %s (%s): ", code, cv.Case.ID, cv.Number)
			dialogs := dialogsFor(v.Dialogs, rv.Region.DialKey(cv.Case, v.Form))
			var dlg *calleesim.DialogRecord
			var cdrs []api.CDR
			outcome := ""
			if len(dialogs) > 0 {
				dlg = &dialogs[0]
			}
			if len(cv.Attempts) > 0 {
				attempted++
				cdrs = cdrsByCall[cv.Attempts[0].CallID.String()]
				outcome = deref(cv.Attempts[0].Outcome)
			}
			r := row(code, cv.Case.ID, cv.Number, dlg, cdrs, t0)
			r.Outcome, r.Invites = outcome, len(dialogs)
			if cv.Contact != nil {
				r.ContactEnd = string(cv.Contact.Status)
			}
			contacts = append(contacts, r)
			c.That(len(cv.Attempts) == 1 && cv.Attempts[0].State == attemptFinished && outcome == "NO_ANSWER",
				pre+"exactly one attempt, FINISHED as NO_ANSWER (%d, %s)", len(cv.Attempts), outcome)
			c.That(len(dialogs) == 1, pre+"exactly one INVITE (%d)", len(dialogs))
			c.That(cv.Contact != nil && cv.Contact.Status == api.CampaignContactStatus("EXHAUSTED"),
				pre+"the contact is EXHAUSTED after its one allowed attempt (%s)", r.ContactEnd)
			c.That(len(cdrs) == 1 && r.HangupCause == "NO_ANSWER" && !r.HasBotLeg,
				pre+"exactly one CDR, NO_ANSWER, no bot (%d CDRs, cause %s)", len(cdrs), r.HangupCause)
			if dlg != nil {
				c.That(dlg.End != nil && dlg.End.Method == calleesim.EndedWithCancel && dlg.End.By == calleesim.EndedByCaller,
					pre+"the platform cancelled it at the ring timeout (%s)", r.EndedBy)
			}
		}
		c.That(attempted == len(rv.Contacts) && attempted == 6, "%s: all %d contacts were attempted (%d)", code, len(rv.Contacts), attempted)

		var fromDID []calleesim.DialogRecord
		for _, d := range v.Dialogs {
			if sipUser(d.InviteHeaders["from"]) == rv.DID.Number {
				fromDID = append(fromDID, d)
			}
		}
		peak, unended := PeakConcurrent(fromDID)
		lr := LineRow{
			Region: code, DID: rv.DID.Number, Limit: limit, PeakDialogs: peak, Dialogs: len(fromDID),
			GaugePeak: gaugePeak[rv.DID.Number], GaugeSamples: gaugeSamples, AtCapacitySeen: atCapacity[rv.DID.Number],
		}
		lines = append(lines, lr)
		c.That(len(fromDID) == len(rv.Contacts) && len(fromDID) > 0 && unended == 0,
			"%s: the simulator saw %d dialogs from DID %s, all ended (%d, %d unended)", code, len(rv.Contacts), rv.DID.Number, len(fromDID), unended)
		c.That(peak <= limit, "%s: never more than %d dialogs from DID %s were up at once (peak %d)", code, limit, rv.DID.Number, peak)
		c.That(peak == limit, "%s: the peak reached the limit, so the limit is what held the campaign back (peak %d, limit %d)", code, peak, limit)
		c.That(gaugeSamples > 0 && lr.GaugePeak <= float64(limit),
			"%s: %s{did=%q} never read above %d in %d samples (peak read %.0f)", code, MetricDIDOutboundInUse, rv.DID.Number, limit, gaugeSamples, lr.GaugePeak)
	}
	n, series := metricDelta(v, MetricDialerBlocked, map[string]string{"reason": "DID_LINES"})
	c.That(series == 1 && n > 0, "the dialer held contacts back for DID_LINES (%s{reason=\"DID_LINES\"} +%.0f)", MetricDialerBlocked, n)
	return contacts, lines
}

// assessCapacityRun makes the run-wide checks of a capacity run that the
// campaign scenarios make in assess: the simulator saw nothing its scenario
// does not know; every dialog is one contact's, from its campaign's caller
// id, or one direct call's, from the direct calls' DID; no mock provider
// script failed, went unclaimed or called a tool (no capacity case is
// scripted).
func assessCapacityRun(v *RunView, unexpectedBase int, c *Checks) {
	c.That(v.Callee.UnexpectedCount == unexpectedBase,
		"the simulator saw no dialog its scenario does not know during the run (unexpected %d -> %d)", unexpectedBase, v.Callee.UnexpectedCount)
	owner := map[string]string{}
	want := 0
	for _, rv := range v.Regions {
		for _, cv := range rv.Contacts {
			owner[rv.Region.DialKey(cv.Case, v.Form)] = rv.DID.Number
			want++
		}
	}
	unowned, wrong := 0, 0
	for _, d := range v.Dialogs {
		did, ok := owner[d.Number]
		switch {
		case !ok:
			unowned++
		case sipUser(d.InviteHeaders["from"]) != did:
			wrong++
		}
	}
	c.That(len(v.Dialogs) == want && want > 0 && unowned == 0 && wrong == 0,
		"every one of the run's %d campaign dialogs is one contact's, from its campaign's caller id (%d dialogs, %d unowned, %d from another caller id)",
		want, len(v.Dialogs), unowned, wrong)
	wrongDirect := 0
	for _, dc := range v.Direct {
		if dc.Dialog == nil || sipUser(dc.Dialog.InviteHeaders["from"]) != dc.DID {
			wrongDirect++
		}
	}
	c.That(wrongDirect == 0, "every direct call reached the simulator once, from the direct calls' DID (%d of %d did not)", wrongDirect, len(v.Direct))
	b, a := v.Provider.Before, v.Provider.After
	c.That(a.ScriptsFailed == b.ScriptsFailed && a.Unmatched == b.Unmatched && a.ToolCalls == b.ToolCalls,
		"the mock provider played only its default script: no script failed (%d -> %d), none went unclaimed (%d -> %d), no tool was called (%d -> %d)",
		b.ScriptsFailed, a.ScriptsFailed, b.Unmatched, a.Unmatched, b.ToolCalls, a.ToolCalls)
}

// capacityTimesNote heads the tables of the capacity runs: which clock each
// time column is read on.
const capacityTimesNote = "Times in ms. Answered at and ended at run from the run's first INVITE on the simulator's wall clock; reported, never asserted. " +
	"Answer to end is read on the simulator's monotonic clock (K3 asserts it for its refused calls); answer to end (wall) is the same interval on the wall clock, " +
	"and differs from it when that clock stepped."

// CapacityTable renders a capacity run as Markdown.
func CapacityTable(rep *CapacityReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n## %s (%.0f s; passed %v)\n\n", rep.Scenario, rep.DurationSec, rep.Passed)
	keys := make([]string, 0, len(rep.AppEnv))
	for k := range rep.AppEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := rep.AppEnv[k]
		if v == "" {
			v = "(default)"
		}
		fmt.Fprintf(&b, "- %s = %s\n", k, v)
	}
	b.WriteString("\n" + capacityTimesNote + "\n\n")
	b.WriteString("| region | call | number | outcome | cause | contact | INVITEs | final | answered at | ended at | answer to end | answer to end (wall) | ended by | bot leg | downlink frames | peak rms |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	opt := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf("%.0f", *p)
	}
	for _, list := range [][]CapacityRow{rep.Contacts, rep.Direct} {
		for _, r := range list {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d | %d | %s | %s | %s | %s | %s | %v | %d | %.0f |\n",
				r.Region, r.ID, r.Number, r.Outcome, r.HangupCause, r.ContactEnd, r.Invites, r.FinalStatus,
				opt(r.AnswerAtMs), opt(r.EndAtMs), opt(r.HoldMs), opt(r.HoldWallMs), r.EndedBy, r.HasBotLeg, r.DownlinkFrames, r.DownlinkPeakRMS)
		}
	}
	if len(rep.Lines) > 0 {
		b.WriteString("\n| region | DID | limit | peak dialogs up at once | dialogs | line gauge peak (samples) | AT_CAPACITY seen |\n|---|---|---|---|---|---|---|\n")
		for _, l := range rep.Lines {
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %d | %.0f (%d) | %v |\n", l.Region, l.DID, l.Limit, l.PeakDialogs, l.Dialogs, l.GaugePeak, l.GaugeSamples, l.AtCapacitySeen)
		}
	}
	return b.String()
}
