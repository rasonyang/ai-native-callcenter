// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// botLegFailed is the userData.botReason the CDR of a call whose bot leg was
// refused carries (issue #459).
const (
	userDataBotReason = "botReason"
	botLegFailed      = "BOT_LEG_FAILED"
	// causeTemporaryFailure is the switch's cause for the bot's 503.
	causeTemporaryFailure = "NORMAL_TEMPORARY_FAILURE"
)

// botReasonOf reads userData.botReason off a CDR; present says the key exists.
func botReasonOf(cdr api.CDR) (reason string, present bool) {
	if cdr.UserData == nil {
		return "", false
	}
	v, ok := (*cdr.UserData)[userDataBotReason]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

func refusalRow(rc RefusalCall, dc DirectCall, region string, t0 time.Time, f RefusalFacts) RefusalRow {
	r := row(region, rc.ID, dc.Number, dc.Dialog, dc.CDRs, t0)
	if dc.Dialog != nil {
		r.Invites = 1
	}
	out := RefusalRow{CapacityRow: r, Role: string(rc.Role), ChannelID: f.Channels[dc.CallID]}
	if len(dc.CDRs) == 1 {
		out.BotReason, _ = botReasonOf(dc.CDRs[0])
		out.QueueID = uuidText(dc.CDRs[0].QueueID)
	}
	return out
}

// answeredWithAck asserts the callee answered 200 and the switch acknowledged.
func answeredWithAck(c *Checks, pre string, dlg *calleesim.DialogRecord) bool {
	ok := dlg != nil && dlg.Final != nil && dlg.Final.Status == 200 && dlg.AckAt != nil
	c.That(ok, "%sthe callee answered 200 and the switch acknowledged it", pre)
	return ok
}

// assessRefused makes the assertions every refused call shares: the callee
// answered, and the CDR is one answered row with no BOT leg and the reason.
func assessRefused(c *Checks, pre string, dlg *calleesim.DialogRecord, cdrs []api.CDR) *api.CDR {
	answeredWithAck(c, pre, dlg)
	c.That(len(cdrs) == 1, pre+"exactly one CDR (%d)", len(cdrs))
	if len(cdrs) != 1 {
		return nil
	}
	cdr := &cdrs[0]
	c.That(cdr.AnsweredAt != nil, "%sthe CDR records the answer", pre)
	c.That(!hasBotLeg(*cdr) && cdr.BotSec == 0, "%sthe CDR shows no BOT leg and no bot seconds (%d legs, %d bot s)", pre, len(cdr.Legs), cdr.BotSec)
	reason, present := botReasonOf(*cdr)
	c.That(present && reason == botLegFailed, "%sthe CDR's userData.botReason is %s (%q, present %v)", pre, botLegFailed, reason, present)
	return cdr
}

// endedBy reports whether the dialog ended by who with a BYE.
func endedByBye(dlg *calleesim.DialogRecord, who string) bool {
	return dlg != nil && dlg.End != nil && dlg.End.By == who && dlg.End.Method == calleesim.EndedWithBye
}

func holdMs(r RefusalRow) float64 {
	if r.HoldMs == nil {
		return -1
	}
	return *r.HoldMs
}

// AssessRefusal makes K3's assertions. It first holds the run to its order:
// the holder was answered and on the bot before the first refused call was
// placed, and was still up when the last refused call ended. It then judges
// R1, R2, R3 and N1 and the holder's own CDR. Latency is reported only. The
// run-wide invariants (AfterRunChecks) are made by the caller, and what the
// switch's log must show goes out as probes.
func AssessRefusal(v *RunView, sc RefusalScenario, f RefusalFacts, c *Checks) ([]RefusalRow, []SwitchProbe) {
	rows, probes := []RefusalRow{}, []SwitchProbe{}
	c.That(len(v.Direct) == len(sc.Direct) && len(v.Regions) == 1 && len(v.Regions[0].Contacts) == 1,
		"K3 placed %d direct calls and one campaign of one contact (%d direct, %d campaigns)", len(sc.Direct), len(v.Direct), len(v.Regions))
	if len(v.Direct) != len(sc.Direct) || len(v.Regions) != 1 || len(v.Regions[0].Contacts) != 1 {
		return rows, probes
	}
	t0 := firstInvite(v)
	rv := v.Regions[0]
	code := rv.Region.Code
	direct := map[string]DirectCall{}
	spec := map[string]RefusalCall{}
	for i, rc := range sc.Direct {
		direct[rc.ID], spec[rc.ID] = v.Direct[i], rc
	}
	holder, r1, r2, n1 := direct["HOLDER"], direct["R1"], direct["R2"], direct["N1"]
	cv := rv.Contacts[0]

	// R3's pieces.
	dialogs := dialogsFor(v.Dialogs, rv.Region.DialKey(cv.Case, v.Form))
	var r3dlg *calleesim.DialogRecord
	if len(dialogs) > 0 {
		r3dlg = &dialogs[0]
	}
	var r3cdrs []api.CDR
	var r3 api.CampaignAttempt
	if len(cv.Attempts) > 0 {
		r3 = cv.Attempts[0]
		for _, cdr := range rv.CDRs {
			if cdr.CallID == r3.CallID {
				r3cdrs = append(r3cdrs, cdr)
			}
		}
	}

	// The order the run was held to.
	c.That(f.HolderOnBotAt != nil && !f.HolderOnBotAt.IsZero(), "precondition: the driver saw the holder answered and on the bot (a bot slot in use, a session open on the mock) before it placed the first refused call")
	c.That(holder.Dialog != nil && holder.IsAnswered() && r1.Dialog != nil && r2.Dialog != nil && r3dlg != nil,
		"precondition: the holder, R1, R2 and R3 all reached the simulator (holder answered %v)", holder.IsAnswered())
	if holder.Dialog != nil && holder.IsAnswered() && r1.Dialog != nil && r2.Dialog != nil && r3dlg != nil {
		answer := holder.Dialog.Final.SentAt
		first := minTime(r1.Dialog.InviteAt, r2.Dialog.InviteAt, r3dlg.InviteAt)
		c.That(answer.Before(first),
			"precondition: the holder was answered before the first refused INVITE (answered at %.0f ms, first refused INVITE at %.0f ms)", msSince(t0, answer), msSince(t0, first))
		if f.HolderOnBotAt != nil {
			placed := minTime(r1.Placed, r2.Placed)
			c.That(f.HolderOnBotAt.Before(placed),
				"precondition: the holder was on the bot before the first refused call was placed (on the bot at %s, first placed at %s)", f.HolderOnBotAt.Format(time.RFC3339Nano), placed.Format(time.RFC3339Nano))
		}
		var lastEnd time.Time
		for _, d := range []*calleesim.DialogRecord{r1.Dialog, r2.Dialog, r3dlg} {
			if end, ok := dialogEnd(*d); ok && end.After(lastEnd) {
				lastEnd = end
			}
		}
		holderEnd, ended := dialogEnd(*holder.Dialog)
		c.That(!lastEnd.IsZero() && ended && holderEnd.After(lastEnd),
			"precondition: the holder was still up when the last refused call ended (holder ended at %.0f ms, last refused call at %.0f ms)", msSince(t0, holderEnd), msSince(t0, lastEnd))
	}
	c.That(n1.Dialog != nil && holder.Dialog != nil && n1.Dialog.InviteAt.After(endOr(holder.Dialog)),
		"precondition: the normal call was placed after the holder ended")

	// The holder.
	{
		rc := spec["HOLDER"]
		pre := "HOLDER (" + holder.Number + "): "
		rows = append(rows, refusalRow(rc, holder, code, t0, f))
		answeredWithAck(c, pre, holder.Dialog)
		c.That(len(holder.CDRs) == 1 && hasBotLeg(holder.CDRs[0]), pre+"exactly one CDR, with a BOT leg (%d CDRs)", len(holder.CDRs))
		if len(holder.CDRs) == 1 {
			_, present := botReasonOf(holder.CDRs[0])
			c.That(!present, "%sthe CDR has no userData.botReason: the bot answered it", pre)
		}
		c.That(endedByBye(holder.Dialog, calleesim.EndedByCallee), "%sthe callee hung up at its scripted time, not the platform", pre)
	}

	// R1: a single AI call, a DID with a fallback queue.
	{
		rc := spec["R1"]
		pre := "R1 (" + r1.Number + "): "
		r := refusalRow(rc, r1, code, t0, f)
		rows = append(rows, r)
		assessRefused(c, pre, r1.Dialog, r1.CDRs)
		c.That(endedByBye(r1.Dialog, calleesim.EndedByCallee),
			pre+"the callee hung up: the platform did not drop the customer (%s)", r.EndedBy)
		want := float64(k3QueuedByeMs)
		c.That(r.HoldMs != nil && holdMs(r) >= want-k3QueuedByeSlackMs && holdMs(r) <= want+k3QueuedByeSlackMs,
			pre+"the callee's BYE came at its scripted %d ms after the answer, +/- %d ms (%.0f ms)", k3QueuedByeMs, k3QueuedByeSlackMs, holdMs(r))
		c.That(f.Channels[r1.CallID] != "", "%sthe customer leg's channel id was read from the live call, to find its lines in the switch log", pre)
		probes = append(probes, SwitchProbe{Case: "R1", CallID: r1.CallID, LegID: f.Channels[r1.CallID], Kind: ProbeQueueTransfer, QueueExt: f.QueueExt})
	}

	// R2: a single AI call, no fallback queue, language zh.
	{
		rc := spec["R2"]
		pre := "R2 (" + r2.Number + "): "
		r := refusalRow(rc, r2, code, t0, f)
		rows = append(rows, r)
		cdr := assessRefused(c, pre, r2.Dialog, r2.CDRs)
		c.That(endedByBye(r2.Dialog, calleesim.EndedByCaller),
			pre+"the platform hung the customer up with a BYE, ahead of the callee's scripted %d ms (%s)", k3StaysUpByeMs, r.EndedBy)
		c.That(r.HoldMs != nil && holdMs(r) <= k3PromptByeMaxMs,
			pre+"the platform's BYE came within %d ms of the answer, the prompt included (%.0f ms)", k3PromptByeMaxMs, holdMs(r))
		if cdr != nil {
			c.That(deref(cdr.HangupCause) == causeTemporaryFailure, pre+"the CDR's hangup cause is the switch's %s (%s)", causeTemporaryFailure, deref(cdr.HangupCause))
			c.That(cdr.QueueID == nil, pre+"the call never reached a queue (queueId %s)", uuidText(cdr.QueueID))
		}
		c.That(f.Channels[r2.CallID] != "", "%sthe customer leg's channel id was read from the live call, to find its lines in the switch log", pre)
		probes = append(probes, SwitchProbe{Case: "R2", CallID: r2.CallID, LegID: f.Channels[r2.CallID], Kind: ProbePromptHangup, Lang: rc.promptLang(), QueueExt: f.QueueExt})
	}

	// R3: a campaign call, a DID with a fallback queue it must not use.
	{
		pre := fmt.Sprintf("R3 (%s): ", cv.Number)
		r := row(code, "R3", cv.Number, r3dlg, r3cdrs, t0)
		r.Invites = len(dialogs)
		r.Outcome = deref(r3.Outcome)
		if cv.Contact != nil {
			r.ContactEnd = string(cv.Contact.Status)
		}
		legID := ""
		if len(cv.Attempts) > 0 {
			legID = r3.LegID.String()
		}
		rows = append(rows, RefusalRow{CapacityRow: r, Role: "CAMPAIGN", ChannelID: legID})
		c.That(len(cv.Attempts) == 1 && r3.State == attemptFinished && deref(r3.Outcome) == outcomeBotFailed,
			pre+"exactly one attempt, FINISHED as %s (%d attempts, outcome %s)", outcomeBotFailed, len(cv.Attempts), r.Outcome)
		c.That(len(cv.Attempts) == 1 && deref(r3.HangupCause) == causeTemporaryFailure,
			pre+"the attempt's hangup cause is %s (%s)", causeTemporaryFailure, deref(r3.HangupCause))
		c.That(cv.Contact != nil && cv.Contact.Status == api.CampaignContactStatus("DONE") && cv.Contact.Attempts == 1,
			pre+"the contact is DONE after its one attempt, not redialled (%s)", r.ContactEnd)
		c.That(len(dialogs) == 1, pre+"exactly one INVITE over the run and the 5 s after it (%d)", len(dialogs))
		cdr := assessRefused(c, pre, r3dlg, r3cdrs)
		c.That(endedByBye(r3dlg, calleesim.EndedByCaller),
			pre+"the platform hung the customer up with a BYE, ahead of the callee's scripted %d ms (%s)", k3StaysUpByeMs, r.EndedBy)
		c.That(r.HoldMs != nil && holdMs(RefusalRow{CapacityRow: r}) <= k3PlatformByeMaxMs,
			pre+"the platform's BYE came within %d ms of the answer (%.0f ms)", k3PlatformByeMaxMs, holdMs(RefusalRow{CapacityRow: r}))
		if cdr != nil {
			c.That(deref(cdr.HangupCause) == causeTemporaryFailure, pre+"the CDR's hangup cause is the switch's %s (%s)", causeTemporaryFailure, deref(cdr.HangupCause))
			c.That(cdr.QueueID == nil, pre+"the call never reached the DID's fallback queue (queueId %s)", uuidText(cdr.QueueID))
		}
		c.That(legID != "", "%sthe attempt names its leg id, to find its lines in the switch log", pre)
		probes = append(probes, SwitchProbe{Case: "R3", CallID: r3.CallID.String(), LegID: legID, Kind: ProbeHangupOnly, QueueExt: f.QueueExt})
	}

	// N1: the pair is free, a normal call reaches the bot.
	{
		rc := spec["N1"]
		pre := "N1 (" + n1.Number + "): "
		r := refusalRow(rc, n1, code, t0, f)
		rows = append(rows, r)
		answeredWithAck(c, pre, n1.Dialog)
		c.That(len(n1.CDRs) == 1 && n1.CDRs[0].AnsweredAt != nil && hasBotLeg(n1.CDRs[0]),
			pre+"exactly one CDR, answered, with a BOT leg (%d CDRs, bot leg %v)", len(n1.CDRs), r.HasBotLeg)
		if len(n1.CDRs) == 1 {
			reason, present := botReasonOf(n1.CDRs[0])
			c.That(!present, "%sthe CDR has no userData.botReason (%q, present %v)", pre, reason, present)
		}
		c.That(r.DownlinkPeakRMS > silenceRMS,
			pre+"the bot was heard: the downlink is above silence (peak rms %.0f, %d frames, silence %d)", r.DownlinkPeakRMS, r.DownlinkFrames, silenceRMS)
		c.That(endedByBye(n1.Dialog, calleesim.EndedByCallee), pre+"the callee ended the call after its scripted %d ms (%s)", k3NormalByeMs, r.EndedBy)
	}
	return rows, probes
}

func (rc RefusalCall) promptLang() string {
	if rc.Language == "zh" {
		return "zh"
	}
	return "en"
}

func minTime(first time.Time, rest ...time.Time) time.Time {
	for _, t := range rest {
		if t.Before(first) {
			first = t
		}
	}
	return first
}

// endOr is when the dialog ended, or the zero time (before everything) when
// it has not.
func endOr(d *calleesim.DialogRecord) time.Time {
	end, _ := dialogEnd(*d)
	return end
}

// Probe kinds: what the switch's log must show for a refused call's leg.
const (
	// ProbeQueueTransfer: the inline continuation transferred the leg to the
	// fallback queue's extension, and played no prompt.
	ProbeQueueTransfer = "QUEUE_TRANSFER"
	// ProbePromptHangup: it played the language's bot-unavailable file, from
	// a real directory, and hung up.
	ProbePromptHangup = "PROMPT_HANGUP"
	// ProbeHangupOnly: it hung up with no prompt and no transfer to the
	// queue extension.
	ProbeHangupOnly = "HANGUP_ONLY"
)

// SwitchProbe is what the switch's log must show for one call.
type SwitchProbe struct {
	Case   string `json:"case"`
	CallID string `json:"callId"`
	// LegID is the customer leg's channel id, the prefix of the leg's lines
	// in the switch's log file.
	LegID string `json:"legId"`
	Kind  string `json:"kind"`
	// QueueExt is the fallback queue's extension number.
	QueueExt string `json:"queueExt,omitempty"`
	// Lang is the language of the prompt: zh, or en for anything else.
	Lang string `json:"lang,omitempty"`
}

// SwitchLogResult is the switch log's judgement.
type SwitchLogResult struct {
	Checks CheckList `json:"checks"`
	// Evidence is, per case, the log lines that satisfied (or the leg's lines
	// that did not satisfy) its probe.
	Evidence map[string][]string `json:"evidence"`
}

// promptPath finds the path of a bot-unavailable file in a line of the log,
// for the language; "" when the line has none.
func promptPath(line, lang string) string {
	re := regexp.MustCompile(`([^\s()\[\]'"<>,;]*/aicc/` + regexp.QuoteMeta(lang) + `/bot-unavailable\.wav)`)
	if m := re.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// transferTo reports whether a log line shows a transfer to the queue's
// extension in the aicc context.
func transferTo(line, ext string) bool {
	q := regexp.QuoteMeta(ext)
	re := regexp.MustCompile(`(?i)transfer\(\s*` + q + `\s+XML\s+aicc\s*\)|transfer[^\n]*\b` + q + `@aicc|\b` + q + `@aicc[^\n]*transfer`)
	return re.MatchString(line)
}

// EvaluateSwitchLog judges the switch's log file against the probes. Every
// leg must have lines in it (a count, so an empty log fails), and then:
//
// Only executed playbacks (lines with "playback(") are read for prompts.
//
//	QUEUE_TRANSFER  a line shows the transfer to the queue's extension, and no
//	                line plays a bot-unavailable file;
//	PROMPT_HANGUP   a line names the language's bot-unavailable file with an
//	                absolute path, no "${" left in it, and no line transfers
//	                to the queue; the other language's file is not played;
//	HANGUP_ONLY     no line transfers to the queue's extension and none plays
//	                a bot-unavailable file.
func EvaluateSwitchLog(probes []SwitchProbe, log string) SwitchLogResult {
	var c Checks
	res := SwitchLogResult{Evidence: map[string][]string{}}
	lines := strings.Split(log, "\n")
	c.That(len(probes) > 0 && strings.TrimSpace(log) != "", "the switch log has content to read (%d bytes) and %d call(s) to find in it", len(log), len(probes))
	for _, p := range probes {
		pre := p.Case + ": "
		var leg []string
		if p.LegID != "" {
			for _, l := range lines {
				if strings.Contains(l, p.LegID) {
					leg = append(leg, l)
				}
			}
		}
		c.That(p.LegID != "" && len(leg) > 0, "%sthe switch log has lines for the customer leg %q (%d)", pre, p.LegID, len(leg))
		if len(leg) == 0 {
			continue
		}
		var transfers, prompts, otherPrompts, unexpanded []string
		other := "zh"
		if p.Lang == "zh" {
			other = "en"
		}
		for _, l := range leg {
			if p.QueueExt != "" && transferTo(l, p.QueueExt) {
				transfers = append(transfers, l)
			}
			// Only an executed playback counts: the "Transfer ... to inline[...]"
			// line carries the whole action list, unexpanded, before any of it
			// runs.
			if !strings.Contains(l, "playback(") {
				continue
			}
			if strings.Contains(l, "bot-unavailable") {
				prompts = append(prompts, l)
			}
			if p.Lang != "" {
				if path := promptPath(l, p.Lang); path != "" && (!strings.HasPrefix(path, "/") || strings.Contains(path, "$")) {
					unexpanded = append(unexpanded, l)
				}
				if promptPath(l, other) != "" {
					otherPrompts = append(otherPrompts, l)
				}
			}
		}
		switch p.Kind {
		case ProbeQueueTransfer:
			c.That(len(transfers) > 0, "%sthe switch transferred the customer leg to the queue extension %s (%d line(s))", pre, p.QueueExt, len(transfers))
			c.That(len(prompts) == 0, "%sno bot-unavailable prompt was played to the queued customer (%d line(s))", pre, len(prompts))
			res.Evidence[p.Case] = firstN(transfers, 3)
		case ProbePromptHangup:
			var good []string
			for _, l := range prompts {
				if path := promptPath(l, p.Lang); strings.HasPrefix(path, "/") && !strings.Contains(path, "$") {
					good = append(good, l)
				}
			}
			c.That(len(good) > 0, "%sthe switch played .../aicc/%s/bot-unavailable.wav from an expanded, absolute path (%d line(s))", pre, p.Lang, len(good))
			c.That(len(unexpanded) == 0, "%sno prompt path was left unexpanded or relative (%d line(s))", pre, len(unexpanded))
			c.That(len(otherPrompts) == 0, "%sthe other language's file was not played (%d line(s))", pre, len(otherPrompts))
			c.That(len(transfers) == 0, "%sthe customer was not transferred to a queue (%d line(s))", pre, len(transfers))
			res.Evidence[p.Case] = firstN(prompts, 4)
		case ProbeHangupOnly:
			c.That(len(transfers) == 0, "%sthe switch did not transfer the customer to the fallback queue extension %s (%d line(s))", pre, p.QueueExt, len(transfers))
			c.That(len(prompts) == 0, "%sno bot-unavailable prompt was played (%d line(s))", pre, len(prompts))
			var exec []string
			for _, l := range leg {
				if strings.Contains(l, "EXECUTE [") {
					exec = append(exec, l)
				}
			}
			res.Evidence[p.Case] = firstN(exec[max(len(exec)-3, 0):], 3)
		default:
			c.That(false, "%sunknown probe kind %q", pre, p.Kind)
		}
	}
	res.Checks = c.list()
	return res
}

func firstN(lines []string, n int) []string {
	if len(lines) > n {
		lines = lines[:n]
	}
	return append([]string{}, lines...)
}

// ApplySwitchLog is the second half of a K3 run. e2e.sh copies the switch's
// log out of its container after the run, and the test binary calls this with
// the run's output directory: it judges the log against the probes in
// report.json, writes the result into the report (and capacity.md) and
// returns an error when a check failed.
func ApplySwitchLog(outDir, logPath string) (*RefusalReport, error) {
	data, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	if err != nil {
		return nil, err
	}
	var rep RefusalReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("report.json: %w", err)
	}
	if len(rep.SwitchProbes) == 0 {
		return &rep, fmt.Errorf("report.json has no switch probes: the run stopped before its assertions")
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		return &rep, fmt.Errorf("switch log: %w", err)
	}
	res := EvaluateSwitchLog(rep.SwitchProbes, string(log))
	rep.SwitchLog = &res
	// Replace the earlier judgement of a repeated call, if any.
	kept := rep.Failures[:0]
	for _, f := range rep.Failures {
		if !strings.HasPrefix(f, "switch log: ") {
			kept = append(kept, f)
		}
	}
	rep.Failures = kept
	for _, f := range res.Checks.Failures {
		rep.Failures = append(rep.Failures, "switch log: "+f)
	}
	rep.Passed = len(rep.Failures) == 0
	if err := writeRefusalReport(outDir, &rep); err != nil {
		return &rep, err
	}
	if !rep.Passed {
		return &rep, fmt.Errorf("%d assertion(s) failed: %s", len(rep.Failures), strings.Join(rep.Failures, "; "))
	}
	return &rep, nil
}

// RefusalTable renders a K3 run as Markdown.
func RefusalTable(rep *RefusalReport) string {
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
	fmt.Fprintf(&b, "- the switch's RTP range = %s\n", rep.SwitchRTPRange)
	if rep.Facts.QueueExt != "" {
		fmt.Fprintf(&b, "- the fallback queue = extension %s\n", rep.Facts.QueueExt)
	}
	b.WriteString("\nTimes from the run's first INVITE, in ms; reported, never asserted.\n\n")
	b.WriteString("| call | role | number | outcome | cause | botReason | queue | contact | INVITEs | answered at | ended at | answer to end | ended by | bot leg | peak rms | leg |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	opt := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf("%.0f", *p)
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, r := range rep.Calls {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %d | %s | %s | %s | %s | %v | %.0f | %s |\n",
			r.ID, r.Role, r.Number, dash(r.Outcome), dash(r.HangupCause), dash(r.BotReason), dash(r.QueueID), dash(r.ContactEnd), r.Invites,
			opt(r.AnswerAtMs), opt(r.EndAtMs), opt(r.HoldMs), dash(r.EndedBy), r.HasBotLeg, r.DownlinkPeakRMS, dash(r.ChannelID))
	}
	if rep.SwitchLog != nil {
		b.WriteString("\nThe switch's log:\n\n")
		cases := make([]string, 0, len(rep.SwitchLog.Evidence))
		for k := range rep.SwitchLog.Evidence {
			cases = append(cases, k)
		}
		sort.Strings(cases)
		for _, k := range cases {
			for _, l := range rep.SwitchLog.Evidence[k] {
				fmt.Fprintf(&b, "- %s: `%s`\n", k, strings.TrimSpace(l))
			}
		}
		fmt.Fprintf(&b, "\n%d checks, %d failed.\n", len(rep.SwitchLog.Checks.Checks), len(rep.SwitchLog.Checks.Failures))
	} else {
		b.WriteString("\nThe switch's log has not been read yet (e2e.sh reads it after the run).\n")
	}
	return b.String()
}
