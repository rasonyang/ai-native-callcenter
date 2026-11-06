// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// The campaign scenarios and probes of issues #245 and #246, as data. A Case is one
// contact; a Region is one campaign, and every region runs the whole table at
// once. The numbers exist only inside deploy/e2e: the environment's network
// is internal and its one gateway is the callee simulator.

// The policy every e2e campaign carries: the validated minimums (ring timeout
// config.MinOutboundRingTimeout, intervals campaign.minIntervalSec), so a
// retry is observable within a minute. Both budgets are 2, so the second
// attempt of a retried outcome spends its budget and the contact ends
// EXHAUSTED, whichever budget the outcome draws on.
const (
	RingTimeoutSec   = 5
	RetryIntervalSec = 60
	MaxAttempts      = 2
	MaxSystemRetries = 2
	// RingTimeoutTolerance is the functional bound of C07 (from the issue):
	// the simulator sees the CANCEL at the ring timeout, give or take this.
	RingTimeoutTolerance = time.Second
)

// Region is one campaign: a country and the numbers it dials. Numbers are
// never mixed: every contact of a region is Prefix plus a Case's Suffix.
type Region struct {
	// Code is "US" or "CN".
	Code string
	// Prefix is the canonical number up to a Case's two-digit Suffix.
	Prefix string
	// CountryCode is the campaign's defaultCountryCode (digits, no plus).
	CountryCode string
	// TimeZone is the campaign's zone; the window is open all day anyway.
	TimeZone string
}

// Regions are the two campaigns. The US range is the fictional 202 555 01xx;
// the Chinese one, 139 8888 66xx, is reserved for this harness. Neither may
// be replaced by a number a real trunk rings.
var Regions = []Region{
	{Code: "US", Prefix: "+120255501", CountryCode: "1", TimeZone: "America/New_York"},
	{Code: "CN", Prefix: "+86139888866", CountryCode: "86", TimeZone: "Asia/Shanghai"},
}

// Number is the contact's canonical E.164 number.
func (r Region) Number(c Case) string { return r.Prefix + c.Suffix }

// DialKey is the string the switch is handed for the contact, which is the
// Request-URI user part the simulator receives and its scenario is keyed on:
// the canonical number in the deployment's AICC_OUTBOUND_DIAL_FORM, computed
// the way the dialer computes it (phonenum.DialString with the campaign's
// country code).
func (r Region) DialKey(c Case, form phonenum.DialForm) string {
	out, _ := r.DialNumber(r.Number(c), form)
	return out
}

// DialNumber writes one canonical number of the region in the form, the way
// the dialer does; the second result is false when the form could not be
// applied, in which case the number is returned as it was.
func (r Region) DialNumber(number string, form phonenum.DialForm) (string, bool) {
	return phonenum.DialString(number, form, r.CountryCode)
}

// Kind says whether a Case asserts a fixed expectation or records what it
// measured.
type Kind string

const (
	KindScenario Kind = "SCENARIO"
	KindProbe    Kind = "PROBE"
)

// Case is one contact.
type Case struct {
	ID     string
	Kind   Kind
	Suffix string
	// Callee describes what the simulator does, for the report.
	Callee string
	// SIPStatus is the final response the simulator sends; zero for a callee
	// that rings until it is cancelled.
	SIPStatus int
	// Provisional is the 1xx the callee sends: 180, or 183 with EarlyMedia.
	Provisional int
	EarlyMedia  bool
	// IsOnDNC puts the number on the do-not-call list before the campaign
	// starts.
	IsOnDNC bool
	// IsListedMidRun makes the driver put the number on the do-not-call list
	// while the contact waits for its retry (C10): after Steps[0], the one
	// attempt that is dialled, a second attempt is skipped as DNC_SKIPPED
	// (cause NUMBER_ON_DNC) and the contact ends DNC. The case dials once, so
	// it needs the one callee behaviour Steps[0] describes, in every dial form.
	IsListedMidRun bool
	// ControlID, for a case listed mid-run, names the case that behaves the
	// same on the callee's side and is never listed (C03). The run must have
	// dialled it a second time, so a retry that is never made cannot pass for
	// a skipped one.
	ControlID string
	// Outcome and Cause are a scenario's expectation; a probe has neither and
	// is held to design 10 section 7.2 instead (designCauseOutcomes).
	Outcome string
	Cause   string
	// HasRingTimeoutBound asserts the CANCEL at the ring timeout within
	// RingTimeoutTolerance (C07). Other ringing cases report the time only.
	HasRingTimeoutBound bool

	// Steps, when set, lists the case's attempts one by one: what the callee
	// does on each and what it must come to. Every case that reaches an
	// answer is written this way (issue #246), and the single-attempt fields
	// above stay empty for it.
	Steps []Step
	// Script names the mock provider's script for the contact's calls. The
	// driver imports the contact with it in the slot ScriptSlot, the flow's
	// instruction renders it (ScriptMarker), and the mock picks its script by
	// that text. Empty: the mock's default script.
	Script string
	// Lead and LeadNotes are what the contact's answered call must record
	// (record_lead); an empty Lead means no lead at all.
	Lead, LeadNotes string
	// Callbacks is how many callbacks the contact's calls must file.
	Callbacks int
}

// Step is one attempt of a case written attempt by attempt.
type Step struct {
	// Provisional is the 1xx the callee sends first (180, or 183 with
	// EarlyMedia); zero for none.
	Provisional int
	EarlyMedia  bool
	// SIPStatus is the final response: 200, or the error the callee refuses
	// with.
	SIPStatus int
	// FinalAfterMs, when set, is when the final response goes out, counted
	// from the INVITE.
	FinalAfterMs int
	// ByeAfterMs, when set, is when the callee hangs up, counted from its 200
	// OK; zero means the callee never does and the caller (the bot) ends the
	// call.
	ByeAfterMs int
	// Outcome is the attempt's expected outcome, and Cause its expected
	// hangup cause, asserted when set (an answered call's is recorded only).
	Outcome, Cause string
}

// IsAnswered reports whether the callee answers this attempt.
func (s Step) IsAnswered() bool { return s.SIPStatus == 200 }

// EndedBy is who ends an answered attempt's dialog, as the simulator records
// it: the callee when it hangs up by itself, otherwise the caller.
func (s Step) EndedBy() string {
	if s.ByeAfterMs > 0 {
		return calleesim.EndedByCallee
	}
	return calleesim.EndedByCaller
}

// IsAnswered reports whether any attempt of the case reaches an answer.
func (c Case) IsAnswered() bool {
	for _, s := range c.Steps {
		if s.IsAnswered() {
			return true
		}
	}
	return false
}

// AnsweredSteps is how many of the case's attempts the callee answers.
func (c Case) AnsweredSteps() int {
	n := 0
	for _, s := range c.Steps {
		if s.IsAnswered() {
			n++
		}
	}
	return n
}

// ScriptSlot is the contact slot that carries a case's Script, and
// ScriptMarker the line the flow's instruction renders from it (its verb takes
// the script, or in the flow file the slot reference). The mock provider
// selects a script by a substring of the standing instructions, and the
// instruction is where a contact's own data reaches them, so a scripted
// contact is told apart from the other contacts of its campaign by this one
// line. The full stop keeps one script's marker from being a prefix of
// another's.
const (
	ScriptSlot   = "e2eScript"
	ScriptMarker = "E2E script: %s."
)

// neverAnsweredAfterMs is how long a ringing callee waits before it gives up
// by itself, with neverAnsweredStatus: far beyond the ring timeout, so only
// the caller's CANCEL ends the dialog in time, and not a 200, so a missing
// CANCEL can never become an answered call.
const (
	neverAnsweredAfterMs = 120_000
	neverAnsweredStatus  = 480
)

// Where a number listed mid-run ends (C10): the second attempt is skipped.
const (
	listedSkipOutcome = "DNC_SKIPPED"
	listedSkipCause   = "NUMBER_ON_DNC"
)

// DefaultCases are C03, C05, C06, C07, C08, C09, C10 and P1 to P5 (issues
// #245 and #496).
var DefaultCases = []Case{
	{ID: "C03", Kind: KindScenario, Suffix: "03", Callee: "180, then 486 Busy Here", Provisional: 180, SIPStatus: 486,
		Outcome: "BUSY", Cause: "USER_BUSY"},
	{ID: "C05", Kind: KindScenario, Suffix: "05", Callee: "180, then 603 Decline", Provisional: 180, SIPStatus: 603,
		Outcome: "REJECTED", Cause: "CALL_REJECTED"},
	{ID: "C06", Kind: KindScenario, Suffix: "06", Callee: "180, then 404 Not Found", Provisional: 180, SIPStatus: 404,
		Outcome: "INVALID_NUMBER", Cause: "UNALLOCATED_NUMBER"},
	{ID: "C07", Kind: KindScenario, Suffix: "07", Callee: "180, never answered", Provisional: 180,
		Outcome: "NO_ANSWER", Cause: "NO_ANSWER", HasRingTimeoutBound: true},
	{ID: "C08", Kind: KindScenario, Suffix: "08", Callee: "183 with early media, never answered", Provisional: 183, EarlyMedia: true,
		Outcome: "NO_ANSWER", Cause: "NO_ANSWER"},
	{ID: "C09", Kind: KindScenario, Suffix: "09", Callee: "on the do-not-call list (never dialled)", IsOnDNC: true,
		Outcome: "DNC_SKIPPED", Cause: "NUMBER_ON_DNC"},
	{ID: "C10", Kind: KindScenario, Suffix: "10",
		Callee:         "180, then 486 Busy Here; the driver lists the number while the retry waits (never dialled again)",
		Steps:          []Step{{Provisional: 180, SIPStatus: 486, Outcome: "BUSY", Cause: "USER_BUSY"}},
		IsListedMidRun: true, ControlID: "C03"},
	{ID: "P1", Kind: KindProbe, Suffix: "11", Callee: "180, then 480 Temporarily Unavailable", Provisional: 180, SIPStatus: 480},
	{ID: "P2", Kind: KindProbe, Suffix: "12", Callee: "180, then 408 Request Timeout", Provisional: 180, SIPStatus: 408},
	{ID: "P3", Kind: KindProbe, Suffix: "13", Callee: "180, then 503 Service Unavailable", Provisional: 180, SIPStatus: 503},
	{ID: "P4", Kind: KindProbe, Suffix: "14", Callee: "180, then 484 Address Incomplete", Provisional: 180, SIPStatus: 484},
	{ID: "P5", Kind: KindProbe, Suffix: "15", Callee: "180, then 410 Gone", Provisional: 180, SIPStatus: 410},
}

// AnsweredCases are C01, C02 and C04 (issue #246): the paths on which a
// campaign reaches a person. They run in the same campaigns as DefaultCases,
// so the run-wide invariants (AfterRunChecks) see answered and unanswered
// calls side by side.
var AnsweredCases = []Case{
	{ID: "C01", Kind: KindScenario, Suffix: "01",
		Callee: "180, then 200 after 2 s, a tone on the uplink; the bot records a HIGH lead and ends the call",
		Steps:  []Step{{Provisional: 180, SIPStatus: 200, FinalAfterMs: 2000, Outcome: "ANSWERED"}},
		Script: "C01_LEAD_HIGH", Lead: "HIGH", LeadNotes: "e2e C01: the customer asked to be called back", Callbacks: 1},
	{ID: "C02", Kind: KindScenario, Suffix: "02",
		Callee: "200, then the customer hangs up at 1 s",
		Steps:  []Step{{SIPStatus: 200, ByeAfterMs: 1000, Outcome: "ANSWERED"}}},
	{ID: "C04", Kind: KindScenario, Suffix: "04",
		Callee: "486 Busy Here on attempt 1; 200 on attempt 2, the customer hanging up at 3 s",
		Steps: []Step{
			{Provisional: 180, SIPStatus: 486, Outcome: "BUSY", Cause: "USER_BUSY"},
			{Provisional: 180, SIPStatus: 200, ByeAfterMs: 3000, Outcome: "ANSWERED"},
		}},
}

// AllCases is what a campaign run holds unless told otherwise: issue #245's
// unanswered cases and probes, then issue #246's answered cases.
var AllCases = append(append([]Case{}, DefaultCases...), AnsweredCases...)

// Budget is what an outcome spends, as the contact's counters show it.
type Budget string

const (
	// BudgetAttempts counts against maxAttempts (contact.attempts).
	BudgetAttempts Budget = "ATTEMPTS"
	// BudgetSystem counts against maxSystemRetries (contact.systemFailures).
	BudgetSystem Budget = "SYSTEM"
	// BudgetNone counts against neither.
	BudgetNone Budget = "NONE"
)

// designOutcome is one row of design 10 section 7.2, written down from the
// design and not imported from internal/campaign, so the run checks the
// platform against the design rather than against itself.
type designOutcome struct {
	Budget Budget
	// TerminalStatus is the contact status of an outcome that is never
	// dialled again; empty for one the e2e policy retries.
	TerminalStatus string
}

// designOutcomes are the unanswered outcomes of section 7.2. The e2e policy
// retries every retryable one.
var designOutcomes = map[string]designOutcome{
	"NO_ANSWER":        {Budget: BudgetAttempts},
	"BUSY":             {Budget: BudgetAttempts},
	"REJECTED":         {Budget: BudgetAttempts},
	"UNKNOWN":          {Budget: BudgetAttempts},
	"TRANSIENT":        {Budget: BudgetSystem},
	"ORIGINATE_FAILED": {Budget: BudgetSystem},
	"INVALID_NUMBER":   {Budget: BudgetNone, TerminalStatus: "INVALID_NUMBER"},
	"DNC_SKIPPED":      {Budget: BudgetNone, TerminalStatus: "DNC"},
}

// designCauseOutcomes is the "typical causes" column of section 7.2 for an
// unanswered call, plus our own ring timeout, which the switch reports as
// NO_ANSWER. A probe whose cause is listed must come out with the listed
// outcome; a cause the design does not list is reported as a finding.
var designCauseOutcomes = map[string]string{
	"NO_ANSWER":                 "NO_ANSWER",
	"NO_USER_RESPONSE":          "NO_ANSWER",
	"USER_BUSY":                 "BUSY",
	"CALL_REJECTED":             "REJECTED",
	"NORMAL_TEMPORARY_FAILURE":  "TRANSIENT",
	"NETWORK_OUT_OF_ORDER":      "TRANSIENT",
	"RECOVERY_ON_TIMER_EXPIRE":  "TRANSIENT",
	"SWITCH_CONGESTION":         "TRANSIENT",
	"NORMAL_CIRCUIT_CONGESTION": "TRANSIENT",
	"UNALLOCATED_NUMBER":        "INVALID_NUMBER",
	"INVALID_NUMBER_FORMAT":     "INVALID_NUMBER",
	"NUMBER_CHANGED":            "INVALID_NUMBER",
	"NUMBER_ON_DNC":             "DNC_SKIPPED",
}

// sipIntent is how this harness reads section 7.2 for each SIP final
// response a probe sends: which outcome the design intends for that kind of
// failure. It is reported next to the measurement and never asserted; a
// mismatch is for a person to judge.
var sipIntent = map[int]string{
	404: "INVALID_NUMBER (the number does not exist)",
	408: "TRANSIENT (no final answer from downstream: a network-side timeout)",
	410: "INVALID_NUMBER (the number is gone)",
	480: "NO_ANSWER (the callee's side reports the user not reachable now)",
	484: "INVALID_NUMBER (the number is malformed)",
	486: "BUSY",
	503: "TRANSIENT (the network is unavailable)",
	603: "REJECTED",
}
