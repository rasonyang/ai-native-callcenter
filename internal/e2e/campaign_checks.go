// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// AfterRunChecks run after every campaign run, in order, over everything the
// run left (RunView). Their assertions go into the run's report under
// afterRun, and a failed one fails the run.
//
// This is the plug-in point for checks that hold for any run rather than for
// one scenario: the run-wide invariants of issue #246, one AfterRunCheck each
// (campaign_invariants.go).
var AfterRunChecks = []AfterRunCheck{
	{Name: "provider-sessions", Run: CheckProviderSessions},
	{Name: "no-invite-after-answer", Run: CheckNoInviteAfterAnswer},
	{Name: "capacity-released", Run: CheckCapacityReleased},
	{Name: "attempts-finished-once", Run: CheckAttemptsFinishedOnce},
	{Name: "one-cdr-per-channel", Run: CheckOneCDRPerChannel},
}

// AfterRunCheck is one check made on a finished run.
type AfterRunCheck struct {
	Name string
	Run  func(ctx context.Context, run *RunView, checks *Checks)
}

// Checks collects assertions: every one made, and the ones that failed. Pair
// every equality with a cardinality, so that an empty result fails.
type Checks struct {
	checks, failures []string
}

// That records one assertion.
func (c *Checks) That(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.checks = append(c.checks, msg)
	if !ok {
		c.failures = append(c.failures, msg)
	}
}

// Failed reports whether any assertion failed.
func (c *Checks) Failed() bool { return len(c.failures) > 0 }

func (c *Checks) list() CheckList {
	return CheckList{Checks: nonNil(c.checks), Failures: nonNil(c.failures)}
}

// RunView is what a finished run left.
type RunView struct {
	c *httpc
	// ObservedAt is when the campaigns were last read.
	ObservedAt time.Time
	Regions    []RegionView
	// Dialogs are the simulator's dialogs that began during the run, in
	// order, except those of Direct; Callee is its whole record.
	Dialogs []calleesim.DialogRecord
	Callee  calleesim.Report
	// Direct are the calls the run placed itself through POST /calls,
	// outside any campaign (issue #247's K1 slot holders); empty for the
	// campaign scenarios. Each holds a bot slot and opens a provider session
	// like a campaign call, and the invariants count them where they apply.
	Direct []DirectCall
	// Provider is the mock provider's session counters around the run.
	Provider ProviderCount
	// MetricsBefore and MetricsAfter are the application's counters read
	// before the campaigns started and after the run had settled.
	MetricsBefore Metrics
	MetricsAfter  Metrics
	// Callbacks is the whole callback queue, read after the run settled.
	Callbacks []api.Callback
	// CalleeReadAt is when the simulator's record (Callee) was read.
	CalleeReadAt time.Time
	// Cases are the run's cases, the same in every region.
	Cases []Case
	// Form is the deployment's dial form: a contact's dialogs are those whose
	// Request-URI user is its number in this form.
	Form phonenum.DialForm
}

// Do calls the public API with the run's scoped key.
func (v *RunView) Do(ctx context.Context, method, path string, in, out any, okStatus ...int) error {
	return v.c.do(ctx, method, path, in, out, okStatus...)
}

// RegionView is one campaign at the end of the run.
type RegionView struct {
	Region   Region
	DID      api.DID
	Campaign api.Campaign
	Contacts []ContactView
	// CDRs are the ledger rows of the campaign.
	CDRs []api.CDR
}

// ContactView is one contact at the end of the run.
type ContactView struct {
	Case   Case
	Number string
	// Contact is the contact at the end; AfterFirst as it stood right after
	// its first attempt finished. Either is nil when it was never seen.
	Contact    *api.CampaignContact
	AfterFirst *api.CampaignContact
	// Listing is how the driver listed the number mid-run (a case with
	// IsListedMidRun); nil for any other case.
	Listing *Listing
	// Attempts are oldest first.
	Attempts []api.CampaignAttempt
}

// Listing is the driver's listing of a contact's number on the do-not-call
// list while the contact waited for its retry.
type Listing struct {
	// PostedAt is just before the request, ListedAt when the API answered it
	// and ConfirmedAt when the list read back held the number.
	PostedAt    time.Time `json:"postedAt"`
	ListedAt    time.Time `json:"listedAt"`
	ConfirmedAt time.Time `json:"confirmedAt"`
	// IsOnList is whether the read-back list held the number.
	IsOnList bool `json:"isOnList"`
	// NextAttemptAt is the contact's own, as read before the listing: the
	// moment the retry came due.
	NextAttemptAt time.Time `json:"nextAttemptAt"`
}

// ProviderCount is the mock provider's counters, read before the campaigns
// started and after the run settled.
type ProviderCount struct {
	Before mockprovider.Stats `json:"before"`
	After  mockprovider.Stats `json:"after"`
}

// DirectCall is one call the run placed itself, outside any campaign.
type DirectCall struct {
	Number string `json:"number"`
	// DialKey is the Request-URI user part the simulator saw.
	DialKey string    `json:"dialKey"`
	DID     string    `json:"did"`
	CallID  string    `json:"callId"`
	Placed  time.Time `json:"placedAt"`
	// IsExpectedNoBot says the call is answered and is meant to meet no bot
	// (K3's refused calls): it opens no provider session and its CDR has no
	// BOT leg.
	IsExpectedNoBot bool `json:"isExpectedNoBot,omitempty"`
	// Dialog is the simulator's record of its INVITE; CDRs the ledger's rows
	// for its call id.
	Dialog *calleesim.DialogRecord `json:"dialog,omitempty"`
	CDRs   []api.CDR               `json:"cdrs"`
}

// IsAnswered reports whether the callee answered the call.
func (d DirectCall) IsAnswered() bool {
	return d.Dialog != nil && d.Dialog.Final != nil && d.Dialog.Final.Status == 200
}

// Sessions is how many sessions the mock opened during the run.
func (p ProviderCount) Sessions() int64 { return p.After.Total - p.Before.Total }
