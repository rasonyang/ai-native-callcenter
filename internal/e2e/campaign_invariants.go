// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// The run-wide invariants of issue #246. Each holds for any campaign run,
// whatever its cases, and each equality is paired with a count, so a run
// that observed nothing fails instead of passing.

// pairedAttempt is one attempt with the simulator's dialog for it (nil when
// none reached the simulator) and the campaign's CDRs for its call id.
type pairedAttempt struct {
	region  RegionView
	contact ContactView
	index   int
	attempt api.CampaignAttempt
	dialog  *calleesim.DialogRecord
	cdrs    []api.CDR
}

func (p pairedAttempt) label() string {
	return fmt.Sprintf("%s %s attempt %d (%s)", p.region.Region.Code, p.contact.Case.ID, p.index+1, p.attempt.CallID)
}

// pairAttempts pairs every attempt of the run with its dialog, in order per
// contact: a contact's n-th INVITE is its n-th attempt (the assessment
// checks each INVITE falls inside its attempt).
func pairAttempts(v *RunView) []pairedAttempt {
	var out []pairedAttempt
	for _, rv := range v.Regions {
		cdrs := map[string][]api.CDR{}
		for _, cdr := range rv.CDRs {
			cdrs[cdr.CallID.String()] = append(cdrs[cdr.CallID.String()], cdr)
		}
		for _, cv := range rv.Contacts {
			dialogs := dialogsFor(v.Dialogs, rv.Region.DialKey(cv.Case, v.Form))
			for i, a := range cv.Attempts {
				p := pairedAttempt{region: rv, contact: cv, index: i, attempt: a, cdrs: cdrs[a.CallID.String()]}
				if i < len(dialogs) {
					p.dialog = &dialogs[i]
				}
				out = append(out, p)
			}
		}
	}
	return out
}

// expectedAnswered is how many calls the run's cases have the callee answer.
func expectedAnswered(v *RunView) int {
	n := 0
	for _, cs := range v.Cases {
		n += cs.AnsweredSteps()
	}
	return n * len(v.Regions)
}

// CheckProviderSessions: the provider sessions started equal the answered
// dialogs that reached a bot, and none was started for a leg that only rang
// (180 or 183).
//
// Read three ways. The mock provider counts the sessions it accepted, and
// the application counts the sessions it opened
// (aicc_provider_sessions_started_total, which it moves before it dials, so
// it counts one that fails while being set up as well); both must have moved
// by exactly the number of dialogs the simulator answered that reached a
// bot. Neither knows which call a session was for, so per call the ledger
// says it: every answered call's CDR shows a BOT leg, and no CDR of a call
// that only rang does.
//
// Issue #247 adapts it, explicitly, for runs whose calls do not all reach a
// bot or are not all a campaign's:
//   - an attempt whose outcome is OVERFLOW was answered and met no bot, so it
//     opens no session and its CDR must show no BOT leg;
//   - a direct call (RunView.Direct) that was answered opens one session;
//   - a run whose cases have no call that only rings (K1) cannot show one,
//     so that check is recorded as not applicable instead of failing on an
//     empty count;
//   - a run whose calls open no session at all (K2, in an application start
//     of its own) may have no series of the application's counter yet: a
//     missing series reads as zero, and only then.
//
// Issue #459 (K3) adapts it once more: an attempt that finished BOT_FAILED
// (the bot refused the INVITE) was answered and met no bot either, so it is
// counted with OVERFLOW; and a direct call marked IsExpectedNoBot is answered
// and opens no session, and its CDR must show no BOT leg.
//
// The campaign scenarios have no OVERFLOW, no direct call and both kinds of
// call, so for them it is the check it was.
func CheckProviderSessions(_ context.Context, v *RunView, c *Checks) {
	answered := answeredDialogs(v.Dialogs)
	want := expectedAnswered(v)
	c.That(answered == want && len(v.Dialogs) > 0,
		"the simulator answered %d of the run's %d dialogs, the number the cases answer (%d)", answered, len(v.Dialogs), want)

	overflow := 0
	for _, p := range pairAttempts(v) {
		if metNoBot(deref(p.attempt.Outcome)) {
			overflow++
		}
	}
	direct, directNoBot, directNoBotWithBot := 0, 0, 0
	for _, d := range v.Direct {
		switch {
		case d.IsAnswered() && d.IsExpectedNoBot:
			directNoBot++
			if len(d.CDRs) == 1 && (hasBotLeg(d.CDRs[0]) || d.CDRs[0].BotSec > 0) {
				directNoBotWithBot++
			}
		case d.IsAnswered():
			direct++
		}
	}
	sessions := answered - overflow + direct

	mock := v.Provider.Sessions()
	c.That(mock == int64(sessions),
		"the mock provider accepted one session per answered dialog that reached a bot: %d session(s), %d answered campaign dialog(s) - %d OVERFLOW + %d answered direct call(s) = %d",
		mock, answered, overflow, direct, sessions)
	if directNoBot > 0 {
		c.That(directNoBotWithBot == 0, "no answered direct call that was refused by the bot has a BOT leg or bot seconds in its CDR (%d of %d do)", directNoBotWithBot, directNoBot)
	}
	c.That(v.Provider.After.Live == 0, "no provider session is still open on the mock (%d)", v.Provider.After.Live)

	before, _ := v.MetricsBefore.Sum(MetricProviderSessions, nil)
	after, series := v.MetricsAfter.Sum(MetricProviderSessions, nil)
	app := after - before
	c.That((series > 0 || sessions == 0) && app == float64(sessions),
		"the application opened one provider session per answered dialog that reached a bot: %s moved %.0f -> %.0f (+%.0f, %d series), want +%d",
		MetricProviderSessions, before, after, app, series, sessions)

	ringing, ringingWithBot, reached, reachedWithBot, overflowWithBot, overflowSeen := 0, 0, 0, 0, 0, 0
	var stray []string
	for _, p := range pairAttempts(v) {
		if p.dialog == nil || len(p.cdrs) != 1 {
			continue
		}
		isAnswered := p.dialog.Final != nil && p.dialog.Final.Status == 200
		bot := hasBotLeg(p.cdrs[0]) || p.cdrs[0].BotSec > 0
		switch {
		case isAnswered && metNoBot(deref(p.attempt.Outcome)):
			overflowSeen++
			if bot {
				overflowWithBot++
				stray = append(stray, p.label())
			}
		case isAnswered:
			reached++
			if bot {
				reachedWithBot++
			}
		case len(p.dialog.Provisionals) > 0:
			ringing++
			if bot {
				ringingWithBot++
				stray = append(stray, p.label())
			}
		}
	}
	// want == overflow: a run whose every answered call met no bot (K3's
	// campaign has R3 only) reaches none, and says so.
	c.That(reachedWithBot == reached && reached == answered-overflow && (reached > 0 || want == overflow),
		"every answered call that was not OVERFLOW shows in its CDR that it reached the bot (%d of %d; %d answered, %d OVERFLOW)",
		reachedWithBot, reached, answered, overflow)
	if overflow > 0 {
		c.That(overflowSeen == overflow && overflowWithBot == 0,
			"no OVERFLOW call reached the bot (%d of %d OVERFLOW call(s) have a BOT leg or bot seconds: %v)", overflowWithBot, overflowSeen, stray)
	}
	if wantRinging(v) == 0 {
		c.That(ringing == 0, "not applicable: the run's cases have no call that only rings, and none did (%d)", ringing)
		return
	}
	c.That(ringing > 0 && ringingWithBot == 0,
		"no call that only rang (180 or 183, %d of them) reached the bot (%d did: %v)", ringing, ringingWithBot, stray)
}

// outcomeOverflow is the outcome of an answered call that met no bot because
// no slot freed; outcomeBotFailed, of one whose bot refused it (issue #459).
const (
	outcomeOverflow  = "OVERFLOW"
	outcomeBotFailed = "BOT_FAILED"
)

// metNoBot reports whether an attempt outcome is an answered call that never
// reached the bot.
func metNoBot(outcome string) bool { return outcome == outcomeOverflow || outcome == outcomeBotFailed }

// wantRinging is how many of the run's calls only ring, per the cases: every
// unanswered, dialled case, once per region.
func wantRinging(v *RunView) int {
	n := 0
	for _, cs := range v.Cases {
		if !cs.IsOnDNC && len(cs.Steps) == 0 && cs.Provisional != 0 {
			n++
		}
		for _, st := range cs.Steps {
			if !st.IsAnswered() && st.Provisional != 0 {
				n++
			}
		}
	}
	return n * len(v.Regions)
}

// CheckNoInviteAfterAnswer: every answered number receives exactly one
// answered INVITE across the run, and none after it. An answered number is
// never dialled again (design 10 section 7.2), so its answered INVITE is its
// last; a number that was busy first (C04) has its earlier, refused INVITE
// before the answer, which the rule allows.
func CheckNoInviteAfterAnswer(_ context.Context, v *RunView, c *Checks) {
	byNumber := map[string][]calleesim.DialogRecord{}
	for _, d := range v.Dialogs {
		byNumber[d.Number] = append(byNumber[d.Number], d)
	}
	wantNumbers := 0
	for _, cs := range v.Cases {
		if cs.IsAnswered() {
			wantNumbers++
		}
	}
	wantNumbers *= len(v.Regions)

	var numbers []string
	for n, list := range byNumber {
		for _, d := range list {
			if d.Final != nil && d.Final.Status == 200 {
				numbers = append(numbers, n)
				break
			}
		}
	}
	sort.Strings(numbers)
	// A run whose cases answer nothing (issue #247's K2) must answer no
	// number; its cardinality is that it dialled at all.
	c.That(len(numbers) == wantNumbers && (wantNumbers > 0 || len(v.Dialogs) > 0),
		"%d number(s) were answered during the run, as the cases say (%d; %d dialogs)", wantNumbers, len(numbers), len(v.Dialogs))
	for _, n := range numbers {
		list := byNumber[n]
		answered, after := 0, 0
		var answeredAt time.Time
		for _, d := range list {
			if d.Final != nil && d.Final.Status == 200 {
				answered++
				if answeredAt.IsZero() {
					answeredAt = d.InviteAt
				}
			}
		}
		for _, d := range list {
			if d.InviteAt.After(answeredAt) {
				after++
			}
		}
		c.That(answered == 1, "%s: exactly one INVITE was answered (%d of its %d)", n, answered, len(list))
		c.That(after == 0, "%s: no INVITE reached it after the answer (%d did)", n, after)
	}
}

// CheckCapacityReleased: the bot slots in use and the DIDs' outbound lines in
// use are back to zero once the run has settled (aicc_bot_slots by state,
// aicc_did_outbound_in_use by did). Each DID series must exist: the run's
// DIDs placed calls, which take lines. The bot slot series must exist when
// the run's calls took a slot; a run in an application start of its own whose
// calls never answered (issue #247's K2) may have none, which reads as zero.
// The DID of the run's direct calls (K1) is held to the same.
func CheckCapacityReleased(_ context.Context, v *RunView, c *Checks) {
	tookSlot := answeredDialogs(v.Dialogs) > 0
	for _, d := range v.Direct {
		tookSlot = tookSlot || d.IsAnswered()
	}
	for _, state := range []string{"IN_USE", "RESERVED"} {
		val, n := v.MetricsAfter.Sum(MetricBotSlots, map[string]string{"state": state})
		c.That((n == 1 || (n == 0 && !tookSlot)) && val == 0, "%s{state=%q} is back to 0 (%d series, value %.0f)", MetricBotSlots, state, n, val)
	}
	for _, rv := range v.Regions {
		val, n := v.MetricsAfter.Sum(MetricDIDOutboundInUse, map[string]string{"did": rv.DID.Number})
		c.That(n == 1 && val == 0, "%s: %s{did=%q} is back to 0 (%d series, value %.0f)", rv.Region.Code, MetricDIDOutboundInUse, rv.DID.Number, n, val)
	}
	seen := map[string]bool{}
	for _, d := range v.Direct {
		if seen[d.DID] {
			continue
		}
		seen[d.DID] = true
		val, n := v.MetricsAfter.Sum(MetricDIDOutboundInUse, map[string]string{"did": d.DID})
		c.That(n == 1 && val == 0, "direct calls: %s{did=%q} is back to 0 (%d series, value %.0f)", MetricDIDOutboundInUse, d.DID, n, val)
	}
}

// CheckAttemptsFinishedOnce: every attempt is finished exactly once. The API
// shows each attempt once and FINISHED, with its outcome and end; the
// application counts a finish when it first commits, and never a revision of
// one (aicc_campaign_attempts_total by outcome), so a second finish of an
// attempt would show as one more than the attempts, per outcome.
func CheckAttemptsFinishedOnce(_ context.Context, v *RunView, c *Checks) {
	seen := map[string]int{}
	byOutcome := map[string]float64{}
	total, unfinished, contacts, withAttempts := 0, 0, 0, 0
	var notFinished []string
	for _, rv := range v.Regions {
		for _, cv := range rv.Contacts {
			contacts++
			if len(cv.Attempts) > 0 {
				withAttempts++
			}
			for _, a := range cv.Attempts {
				total++
				seen[a.ID.String()]++
				if a.State != attemptFinished || a.FinishedAt == nil || a.Outcome == nil {
					unfinished++
					notFinished = append(notFinished, fmt.Sprintf("%s %s %s", rv.Region.Code, cv.Case.ID, a.State))
					continue
				}
				byOutcome[string(*a.Outcome)]++
			}
		}
	}
	dup := 0
	for _, n := range seen {
		if n > 1 {
			dup++
		}
	}
	c.That(total > 0 && withAttempts == contacts, "every one of the run's %d contacts has an attempt (%d do; %d attempts)", contacts, withAttempts, total)
	c.That(dup == 0, "every attempt appears once (%d appear more than once)", dup)
	c.That(unfinished == 0, "every attempt is FINISHED with an outcome and an end (%d are not: %v)", unfinished, notFinished)

	before := v.MetricsBefore.ByLabel(MetricCampaignAttempts, "outcome")
	after := v.MetricsAfter.ByLabel(MetricCampaignAttempts, "outcome")
	outcomes := map[string]bool{}
	for o := range byOutcome {
		outcomes[o] = true
	}
	for o := range after {
		outcomes[o] = true
	}
	var names []string
	for o := range outcomes {
		names = append(names, o)
	}
	sort.Strings(names)
	var sum float64
	for _, o := range names {
		delta := after[o] - before[o]
		sum += delta
		c.That(delta == byOutcome[o], "%s{outcome=%q} moved by the %.0f attempt(s) with that outcome (+%.0f)", MetricCampaignAttempts, o, byOutcome[o], delta)
	}
	c.That(total > 0 && sum == float64(total), "%s moved by the run's %d attempts, once each (+%.0f)", MetricCampaignAttempts, total, sum)
}

// CheckOneCDRPerChannel: every attempt that created a channel has exactly one
// CDR, and that CDR carries the campaign's id. An attempt creates a channel
// unless it was never placed (DNC_SKIPPED) or failed before the switch made
// one (ORIGINATE_FAILED). The CDRs are read by the campaign's DID, not by
// campaign, so a row that lost its campaign id is still found; each is then
// read once more by call id.
func CheckOneCDRPerChannel(ctx context.Context, v *RunView, c *Checks) {
	byDID := map[string]map[string][]api.CDR{}
	for _, rv := range v.Regions {
		var l api.CDRList
		path := "/cdrs?limit=1000&did=" + url.QueryEscape(rv.DID.Number)
		if err := v.Do(ctx, http.MethodGet, path, nil, &l, http.StatusOK); err != nil {
			c.That(false, "%s: list the CDRs of DID %s: %v", rv.Region.Code, rv.DID.Number, err)
			return
		}
		c.That(l.Total == len(l.Items), "%s: one page holds every CDR of DID %s (%d of %d)", rv.Region.Code, rv.DID.Number, len(l.Items), l.Total)
		m := map[string][]api.CDR{}
		for _, cdr := range l.Items {
			m[cdr.CallID.String()] = append(m[cdr.CallID.String()], cdr)
		}
		byDID[rv.Region.Code] = m
	}

	channels, withDialog, good := 0, 0, 0
	var bad []string
	ownCalls := map[string]map[string]bool{}
	for _, p := range pairAttempts(v) {
		code := p.region.Region.Code
		if ownCalls[code] == nil {
			ownCalls[code] = map[string]bool{}
		}
		id := p.attempt.CallID.String()
		ownCalls[code][id] = true
		outcome := deref(p.attempt.Outcome)
		if outcome == "DNC_SKIPPED" || outcome == "ORIGINATE_FAILED" {
			if n := len(byDID[code][id]); n != 0 {
				bad = append(bad, fmt.Sprintf("%s: %s, never placed, has %d CDR(s)", p.label(), outcome, n))
			}
			continue
		}
		channels++
		if p.dialog != nil {
			withDialog++
		}
		list := byDID[code][id]
		if len(list) != 1 {
			bad = append(bad, fmt.Sprintf("%s: %d CDRs", p.label(), len(list)))
			continue
		}
		want := p.region.Campaign.ID.String()
		if got := uuidText(list[0].CampaignID); got != want {
			bad = append(bad, fmt.Sprintf("%s: the CDR's campaignId is %q, want %s", p.label(), got, want))
			continue
		}
		var one api.CDRDetail
		if err := v.Do(ctx, http.MethodGet, "/cdrs/"+id, nil, &one, http.StatusOK); err != nil {
			bad = append(bad, fmt.Sprintf("%s: read the CDR by call id: %v", p.label(), err))
			continue
		}
		if one.CDR.CallID != p.attempt.CallID {
			bad = append(bad, fmt.Sprintf("%s: the CDR read by call id is call %s", p.label(), one.CDR.CallID))
			continue
		}
		if got := uuidText(one.CDR.CampaignID); got != want {
			bad = append(bad, fmt.Sprintf("%s: the CDR read by call id has campaignId %q, want %s", p.label(), got, want))
			continue
		}
		good++
	}
	c.That(channels > 0 && channels == len(v.Dialogs) && withDialog == channels,
		"the run's %d attempts that created a channel are the simulator's %d dialogs (%d paired)", channels, len(v.Dialogs), withDialog)
	c.That(good == channels && len(bad) == 0,
		"each of the %d attempts that created a channel has exactly one CDR, carrying its campaign's id (%d do; %s)", channels, good, strings.Join(bad, "; "))
	for _, rv := range v.Regions {
		code := rv.Region.Code
		var strays []string
		for id := range byDID[code] {
			if !ownCalls[code][id] {
				strays = append(strays, id)
			}
		}
		sort.Strings(strays)
		c.That(len(byDID[code]) > 0 && len(strays) == 0,
			"%s: every CDR of DID %s belongs to an attempt of the campaign (%d of %d do not: %v)", code, rv.DID.Number, len(strays), len(byDID[code]), strays)
	}
}
