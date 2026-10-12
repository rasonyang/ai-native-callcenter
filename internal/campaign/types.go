// SPDX-License-Identifier: Apache-2.0

// Package campaign is the AI outbound campaign model: what a campaign is, the
// rules for what may be said about one, its state machine and the import of
// its contacts (docs/design/10-campaigns.md).
//
// This package knows nothing about HTTP and nothing about SQL. It owns the
// meaning (validation, field locks, which transitions exist, how a contact list
// row becomes a contact) and reaches persistence through Repository, the
// dialer through Controller. The dialer itself is a later slice: until it
// exists the controller is a plain database update (RepoController); when it
// does, the API still asks and the dialer actor decides.
package campaign

import (
	"time"

	"github.com/google/uuid"
)

// Status is where a campaign is in its life. The values are byte-identical in
// JSON and the database.
type Status string

const (
	StatusDraft     Status = "DRAFT"
	StatusRunning   Status = "RUNNING"
	StatusPaused    Status = "PAUSED"
	StatusCompleted Status = "COMPLETED"
	StatusCancelled Status = "CANCELLED"
)

// IsTerminal reports whether nothing can happen to a campaign in this status
// any more: its contacts and leads stay readable and that is all.
func (s Status) IsTerminal() bool { return s == StatusCompleted || s == StatusCancelled }

// IsValid reports whether s is one of the five statuses.
func (s Status) IsValid() bool {
	switch s {
	case StatusDraft, StatusRunning, StatusPaused, StatusCompleted, StatusCancelled:
		return true
	}
	return false
}

// PauseReason says why a campaign is PAUSED.
type PauseReason string

const (
	PauseManual          PauseReason = "MANUAL"
	PauseTrunkFailure    PauseReason = "TRUNK_FAILURE"
	PauseProviderFailure PauseReason = "PROVIDER_FAILURE"
)

// ContactStatus is where a contact is in a campaign.
type ContactStatus string

const (
	ContactActive        ContactStatus = "ACTIVE"
	ContactDone          ContactStatus = "DONE"
	ContactExhausted     ContactStatus = "EXHAUSTED"
	ContactInvalidNumber ContactStatus = "INVALID_NUMBER"
	ContactDoNotCall     ContactStatus = "DNC"
)

// IsValid reports whether s is one of the five contact statuses.
func (s ContactStatus) IsValid() bool {
	switch s {
	case ContactActive, ContactDone, ContactExhausted, ContactInvalidNumber, ContactDoNotCall:
		return true
	}
	return false
}

// Outcome is how an attempt ended (design 10 section 7.2).
type Outcome string

const (
	OutcomeAnswered        Outcome = "ANSWERED"
	OutcomeBotFailed       Outcome = "BOT_FAILED"
	OutcomeOverflow        Outcome = "OVERFLOW"
	OutcomeNoAnswer        Outcome = "NO_ANSWER"
	OutcomeBusy            Outcome = "BUSY"
	OutcomeRejected        Outcome = "REJECTED"
	OutcomeUnknown         Outcome = "UNKNOWN"
	OutcomeTransient       Outcome = "TRANSIENT"
	OutcomeOriginateFailed Outcome = "ORIGINATE_FAILED"
	OutcomeInvalidNumber   Outcome = "INVALID_NUMBER"
	OutcomeDNCSkipped      Outcome = "DNC_SKIPPED"
)

// LeadIntent is what a call concluded about the customer (design 10
// section 11).
type LeadIntent string

const (
	LeadHigh      LeadIntent = "HIGH"
	LeadMedium    LeadIntent = "MEDIUM"
	LeadLow       LeadIntent = "LOW"
	LeadNone      LeadIntent = "NONE"
	LeadDoNotCall LeadIntent = "DO_NOT_CALL"
)

// IsValid reports whether i is one of the five intents.
func (i LeadIntent) IsValid() bool {
	switch i {
	case LeadHigh, LeadMedium, LeadLow, LeadNone, LeadDoNotCall:
		return true
	}
	return false
}

// ContactCounts is how many contacts a campaign has in each status.
type ContactCounts struct {
	Total         int `json:"total"`
	Active        int `json:"active"`
	Done          int `json:"done"`
	Exhausted     int `json:"exhausted"`
	InvalidNumber int `json:"invalidNumber"`
	DNC           int `json:"dnc"`
}

// Campaign is one AI outbound campaign.
type Campaign struct {
	ID   uuid.UUID
	Name string
	// DIDID is the outbound DID: it names the flow and the caller id.
	DIDID uuid.UUID
	// DIDNumber is that DID's number, for display. Read-only.
	DIDNumber   string
	Status      Status
	PauseReason PauseReason
	// TimeZone is the IANA name that contacts without their own fall back to.
	TimeZone      string
	CallingWindow CallingWindow
	// DefaultCountryCode completes national numbers on import; empty falls
	// back to the deployment's AICC_DEFAULT_COUNTRY_CODE.
	DefaultCountryCode string
	MaxConcurrent      int
	RingTimeoutSec     int
	RetryPolicy        RetryPolicy
	FollowUpQueueID    *uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
	Contacts           ContactCounts
}

// Contact is one number a campaign dials.
type Contact struct {
	ID         uuid.UUID
	CampaignID uuid.UUID
	// Number is canonical E.164; NumberRaw is what was imported.
	Number    string
	NumberRaw string
	// TimeZone overrides the campaign's when set.
	TimeZone       string
	Slots          map[string]string
	Status         ContactStatus
	Attempts       int
	SystemFailures int
	NextAttemptAt  *time.Time
	LastOutcome    Outcome
	LeadIntent     LeadIntent
	LeadNotes      string
	CreatedAt      time.Time
}

// ListFilter narrows and pages a listing of campaigns.
type ListFilter struct {
	Status Status
	Limit  int
	Offset int
}

// ContactFilter narrows and pages a listing of contacts.
type ContactFilter struct {
	Status     ContactStatus
	LeadIntent LeadIntent
	Limit      int
	Offset     int
}
