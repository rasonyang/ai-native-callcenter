// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// AttemptState is where an attempt is. DIALING becomes FINISHED once; a later,
// fuller CDR may change a FINISHED attempt's outcome, never its state.
type AttemptState string

const (
	AttemptDialing  AttemptState = "DIALING"
	AttemptFinished AttemptState = "FINISHED"
)

// ErrNumberInFlight: another attempt, in this campaign or any other, is
// dialling the same number. It is the partial unique index
// uq_campaign_attempts_dialing_number_e164 speaking, so it holds however many
// admitters race.
var ErrNumberInFlight = errors.New("campaign: the number is already being dialled")

// ErrContactNotDialable: the contact is gone or no longer ACTIVE.
var ErrContactNotDialable = errors.New("campaign: the contact cannot be dialled")

// ErrNoAttempt: no attempt carries this call id.
var ErrNoAttempt = errors.New("campaign: no attempt for that call")

// Attempt is one call placed (or about to be placed) to a contact.
type Attempt struct {
	ID         uuid.UUID
	CampaignID uuid.UUID
	ContactID  uuid.UUID
	Number     string
	// CallID is minted before the originate and becomes the CDR's call id.
	CallID uuid.UUID
	// LegID is the customer leg's origination uuid, minted with CallID.
	LegID          uuid.UUID
	OriginateJobID string
	State          AttemptState
	Outcome        Outcome
	HangupCause    string
	TalkSec        int
	IsTouched      bool
	// IsTestNumber: the number was on the list of test numbers when the
	// attempt was placed. History; a later delisting does not change it.
	IsTestNumber bool
	// LeadIntent is the effective lead: the one the flow declared, or what a
	// verdict left of it.
	LeadIntent LeadIntent
	LeadNotes  string
	// DeclaredLeadIntent, LeadVerdict and LeadVerdictReason are set only on an
	// attempt whose lead a text model was owed to judge (design 10 section
	// 11.1): the flow's intent, kept apart, the verdict and its reason.
	DeclaredLeadIntent LeadIntent
	LeadVerdict        LeadVerdict
	LeadVerdictReason  string
	StartedAt          time.Time
	FinishedAt         *time.Time
}

// Ending is how an attempt ended, as the finish routine takes it.
type Ending struct {
	Outcome     Outcome
	HangupCause string
	TalkSec     int
	// EndedAt is when the call ended; the retry is measured from it.
	EndedAt time.Time
}

// Finished tells the dialer that an attempt's outcome was written, after the
// transaction that wrote it committed.
type Finished struct {
	CampaignID uuid.UUID
	ContactID  uuid.UUID
	AttemptID  uuid.UUID
	CallID     uuid.UUID
	Outcome    Outcome
	// IsRevision: the attempt was already FINISHED and a fuller CDR replaced
	// its outcome.
	IsRevision bool
	// ContactStatus and NextAttemptAt are the contact as the routine left it.
	ContactStatus ContactStatus
	NextAttemptAt *time.Time
	// SettleReason is the rule that decided ContactStatus, for the log; empty
	// when the routine left the contact alone (a revision of an older attempt).
	SettleReason SettleReason
	// IsLeadJudgementOwed: this finish made the attempt's lead one a text model
	// is to judge (its verdict is PENDING), so the judge is to be woken.
	IsLeadJudgementOwed bool
}
