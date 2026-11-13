// SPDX-License-Identifier: Apache-2.0

package campaign

import "github.com/google/uuid"

// CallbackStatus is where a callback stands in the agents' work queue. This
// package never moves a callback (agents claim, complete and release them);
// it counts the ones a campaign's HIGH leads filed, in the callbacks table's
// vocabulary.
type CallbackStatus string

const (
	CallbackStatusOpen      CallbackStatus = "OPEN"
	CallbackStatusClaimed   CallbackStatus = "CLAIMED"
	CallbackStatusDone      CallbackStatus = "DONE"
	CallbackStatusDismissed CallbackStatus = "DISMISSED"
)

// AllCallbackStatuses is every callback status, in the order the contract
// declares them.
func AllCallbackStatuses() []CallbackStatus {
	return []CallbackStatus{CallbackStatusOpen, CallbackStatusClaimed, CallbackStatusDone, CallbackStatusDismissed}
}

// OutcomeSummary is what a campaign's calls came to. Nothing in it is kept as
// a running total: the store counts the attempts, contacts and callbacks as
// they stand when it is read, in one snapshot, so a call record revised after
// the call moves its attempt from one count to another exactly once, and every
// count is the total of a listing with the same filter at the same moment.
//
// A value missing from a map is zero.
type OutcomeSummary struct {
	CampaignID uuid.UUID
	// AttemptsByOutcome counts the finished attempts by outcome: ListAttempts
	// with that Outcome. An attempt in flight has no outcome yet.
	AttemptsByOutcome map[Outcome]int
	// ContactsByLastOutcome counts the contacts by the outcome of their latest
	// finished attempt: ListContacts with that LastOutcome. A contact never
	// tried is in none.
	ContactsByLastOutcome map[Outcome]int
	// ContactsByLeadIntent counts the contacts by the lead their calls
	// recorded: ListContacts with that LeadIntent. A contact with no recorded
	// lead is in none.
	ContactsByLeadIntent map[LeadIntent]int
	OptOuts              OptOutCounts
	// CallbacksByStatus counts the callbacks the campaign's calls filed (one
	// per HIGH lead, when its call ended) by their status now.
	CallbacksByStatus map[CallbackStatus]int
}

// OptOutCounts splits a campaign's DNC contacts by how they got there. A
// contact whose number somebody else put on the list while its call was in
// flight is DNC and in neither count.
type OptOutCounts struct {
	// SaidInConversation: the customer asked on the call (IsOptOutSaid).
	SaidInConversation int
	// AlreadyListed: the number was on the list before it was dialled, so the
	// contact was skipped (IsOptOutListed).
	AlreadyListed int
}

// IsOptOutSaid reports whether a contact opted out in conversation: its call
// recorded a DO_NOT_CALL lead, which put the number on the do-not-call list
// when the call ended and made the contact DNC. ListContacts with status DNC
// and lead intent DO_NOT_CALL.
func IsOptOutSaid(status ContactStatus, lead LeadIntent) bool {
	return status == ContactDoNotCall && lead == LeadDoNotCall
}

// IsOptOutListed reports whether a contact was skipped because its number was
// already on the do-not-call list when it came up to be dialled: its latest
// attempt ended DNC_SKIPPED, never rang, and made the contact DNC. ListContacts
// with status DNC and last outcome DNC_SKIPPED.
func IsOptOutListed(status ContactStatus, last Outcome) bool {
	return status == ContactDoNotCall && last == OutcomeDNCSkipped
}

// NewOutcomeSummary is an empty summary of one campaign.
func NewOutcomeSummary(campaignID uuid.UUID) OutcomeSummary {
	return OutcomeSummary{
		CampaignID:            campaignID,
		AttemptsByOutcome:     map[Outcome]int{},
		ContactsByLastOutcome: map[Outcome]int{},
		ContactsByLeadIntent:  map[LeadIntent]int{},
		CallbacksByStatus:     map[CallbackStatus]int{},
	}
}

// AddContacts counts n contacts that share a status, a last outcome and a
// lead ("" for none). Each count reads the same columns the contact listing
// filters on, so the store hands over one row per combination and every
// dimension adds up from them.
func (s *OutcomeSummary) AddContacts(status ContactStatus, last Outcome, lead LeadIntent, n int) {
	if last != "" {
		s.ContactsByLastOutcome[last] += n
	}
	if lead != "" {
		s.ContactsByLeadIntent[lead] += n
	}
	if IsOptOutSaid(status, lead) {
		s.OptOuts.SaidInConversation += n
	}
	if IsOptOutListed(status, last) {
		s.OptOuts.AlreadyListed += n
	}
}
