// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"time"

	"github.com/google/uuid"
)

// Activity is what a RUNNING campaign is doing right now, computed by the
// dialer and never stored (design 10 section 4.1). A closed calling window is
// not a status: the campaign stays RUNNING and says so here.
type Activity string

const (
	// ActivityDialing: calls are in flight.
	ActivityDialing Activity = "DIALING"
	// ActivityWaitingWindow: every pending contact is outside its calling window.
	ActivityWaitingWindow Activity = "WAITING_WINDOW"
	// ActivityWaitingRetry: no pending contact is due yet.
	ActivityWaitingRetry Activity = "WAITING_RETRY"
	// ActivityWaitingFrequency: nothing is in flight and the earliest pending
	// contact is held by the cross-campaign frequency window: another campaign
	// reached its number lately, and this one waits until that touch plus the
	// window (NextDialAt). A number on the list of test numbers is never held.
	ActivityWaitingFrequency Activity = "WAITING_FREQUENCY"
	// ActivityWaitingDailyCap: nothing is in flight and the earliest pending
	// contact is held by the daily dial cap: its number was dialled the
	// deployment's maximum of times today (AICC_OUTBOUND_MAX_DIALS_PER_NUMBER_PER_DAY),
	// and this campaign waits for the start of the contact's calling window on
	// the next local day (NextDialAt). A number on the list of test numbers is
	// never held.
	ActivityWaitingDailyCap Activity = "WAITING_DAILY_CAP"
	// ActivityAtCapacity: due contacts wait on one of the dialer's limits.
	ActivityAtCapacity Activity = "AT_CAPACITY"
)

// IsValid reports whether a is one of the six activities.
func (a Activity) IsValid() bool {
	switch a {
	case ActivityDialing, ActivityWaitingWindow, ActivityWaitingRetry, ActivityWaitingFrequency, ActivityWaitingDailyCap, ActivityAtCapacity:
		return true
	}
	return false
}

// Progress is the dialer's account of one campaign. Empty Activity: the
// campaign is not RUNNING or the dialer has not looked yet.
type Progress struct {
	Activity   Activity
	NextDialAt *time.Time
	InFlight   int
}

// ProgressSource tells what the dialer says about campaigns. The service asks
// it for every campaign it returns.
type ProgressSource interface {
	Progress(id uuid.UUID) (Progress, bool)
}

// Observer is told about changes the dialer must react to. Methods are called
// after the change committed and must not block.
type Observer interface {
	// Updated: a PATCH changed the campaign.
	Updated(before, after Campaign)
	// Imported: contacts were added.
	Imported(id uuid.UUID)
}

// HoldKind is the mark a deferral leaves on the contacts it moves: which limit,
// if any, made their due time later. A contact carries at most one.
type HoldKind string

const (
	// HoldNone: the deferral is a calling window's or a retry's, no mark.
	HoldNone HoldKind = ""
	// HoldFrequency: the cross-campaign frequency window (is_frequency_held).
	HoldFrequency HoldKind = "FREQUENCY"
	// HoldDailyCap: the daily dial cap (is_daily_cap_held).
	HoldDailyCap HoldKind = "DAILY_CAP"
)

// Held is a due contact another campaign reached lately.
type Held struct {
	ContactID uuid.UUID
	// TimeZone is the contact's own zone; empty means the campaign's.
	TimeZone  string
	LastTouch time.Time
}

// Pending is where a campaign's contact list stands.
type Pending struct {
	// ActiveTotal counts ACTIVE contacts, those being dialled included.
	ActiveTotal int
	// DueNow counts those whose next_attempt_at has come.
	DueNow int
	// NextFuture is the earliest next_attempt_at after now; nil if none.
	NextFuture *time.Time
	// AllTotal counts every contact of the campaign.
	AllTotal int
}

// DialingAttempt is an attempt in flight with the ring timeout of its campaign.
type DialingAttempt struct {
	Attempt        Attempt
	RingTimeoutSec int
}

// AttemptFilter narrows and pages a listing of attempts.
type AttemptFilter struct {
	ContactID *uuid.UUID
	Outcome   Outcome
	// IsTestNumber: nil every attempt, else only those whose IsTestNumber is
	// equal.
	IsTestNumber *bool
	Limit        int
	Offset       int
}
