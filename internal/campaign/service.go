// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// DIDFlow is what a campaign needs to know about the number it dials from.
type DIDFlow struct {
	ID        uuid.UUID
	Number    string
	IsEnabled bool
	// AllowOutbound is the DID's "this platform can place calls from this
	// number"; a campaign dials from it.
	AllowOutbound bool
	// Spec is the flow's published revision; nil when the number has no flow
	// or its flow has never been published.
	Spec *flow.Spec
}

// ErrDIDNotFound is returned by Repository.DIDFlow for an unknown DID.
var ErrDIDNotFound = errors.New("campaign: no such DID")

// ErrQueueNotFound is returned by Repository when the follow-up queue does not
// exist.
var ErrQueueNotFound = errors.New("campaign: no such queue")

// NewContact is a contact ready to be stored: its number already canonical.
type NewContact struct {
	ID        uuid.UUID
	Number    string
	NumberRaw string
	TimeZone  string
	Slots     map[string]string
}

// Stored says what became of one NewContact.
type Stored struct {
	ID uuid.UUID
	// IsNew is false when the campaign already had the number; ID is then the
	// existing contact's, which was left untouched.
	IsNew bool
}

// Repository is the persistence the service needs. Every method that changes a
// campaign's status is conditional on the status it expects, so two racing
// callers cannot both win.
type Repository interface {
	// DIDFlow looks a DID up with its published flow.
	DIDFlow(ctx context.Context, didID uuid.UUID) (DIDFlow, error)

	Create(ctx context.Context, c Campaign) (Campaign, error)
	Get(ctx context.Context, id uuid.UUID) (Campaign, error)
	List(ctx context.Context, f ListFilter) ([]Campaign, int64, error)
	// Update reads the campaign under a row lock, hands it to change, and
	// stores what change returns, so a field-lock decision and the write it
	// guards see the same status.
	Update(ctx context.Context, id uuid.UUID, change func(current Campaign) (Campaign, error)) (Campaign, error)
	// Delete removes a DRAFT campaign and its contacts; any other status is a
	// transition error.
	Delete(ctx context.Context, id uuid.UUID) error

	// The four transitions. Each is one conditional UPDATE: it changes the
	// campaign only if it is in a status the move leaves, else returns a
	// transition error naming the status found (or ErrNotFound).
	Start(ctx context.Context, id uuid.UUID) (Campaign, error)
	Pause(ctx context.Context, id uuid.UUID) (Campaign, error)
	Resume(ctx context.Context, id uuid.UUID) (Campaign, error)
	Cancel(ctx context.Context, id uuid.UUID) (Campaign, error)

	// ImportContacts stores a batch in one transaction, first writer winning
	// a number, and refuses a campaign in a terminal status. The result has
	// one entry per input, in order.
	ImportContacts(ctx context.Context, campaignID uuid.UUID, contacts []NewContact) ([]Stored, error)
	ListContacts(ctx context.Context, campaignID uuid.UUID, f ContactFilter) ([]Contact, int64, error)
}

// Controller moves a campaign between statuses. The API asks, the controller
// decides: today it is RepoController, a conditional database update; the
// dialer replaces it so that the actor owning a campaign's in-flight calls is
// the one to stop or start them (design 10 section 4.1). The service never
// touches status any other way.
type Controller interface {
	Start(ctx context.Context, id uuid.UUID) (Campaign, error)
	Pause(ctx context.Context, id uuid.UUID) (Campaign, error)
	Resume(ctx context.Context, id uuid.UUID) (Campaign, error)
	Cancel(ctx context.Context, id uuid.UUID) (Campaign, error)
}

// RepoController is the Controller that applies a transition straight to the
// database. COMPLETED is not here: only the dialer, noticing nothing is left
// to dial and nothing in flight, reaches it.
type RepoController struct{ Repo Repository }

func (c RepoController) Start(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return c.Repo.Start(ctx, id)
}
func (c RepoController) Pause(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return c.Repo.Pause(ctx, id)
}
func (c RepoController) Resume(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return c.Repo.Resume(ctx, id)
}
func (c RepoController) Cancel(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return c.Repo.Cancel(ctx, id)
}

// Env is what the deployment contributes to the rules.
type Env struct {
	// OutboundHours are the outer calling hours (AICC_OUTBOUND_HOURS); zero
	// means config.DefaultOutboundHours.
	OutboundHours dayrange.Range
	// AnswerWindow is how long an originated leg may take to be answered; a
	// campaign's ring timeout must be strictly below it. Zero means
	// config.OutboundAnswerWindow.
	AnswerWindow time.Duration
	// DefaultCountryCode (AICC_DEFAULT_COUNTRY_CODE) completes national
	// numbers of campaigns that name no country code of their own.
	DefaultCountryCode string
}

// Service is the campaign use cases.
type Service struct {
	repo Repository
	ctl  Controller
	env  Env
	now  func() time.Time
}

// NewService builds the service. A nil controller means RepoController.
func NewService(repo Repository, ctl Controller, env Env) *Service {
	if ctl == nil {
		ctl = RepoController{Repo: repo}
	}
	if env.OutboundHours == (dayrange.Range{}) {
		env.OutboundHours, _ = config.ParseOutboundHours(config.DefaultOutboundHours)
	}
	if env.AnswerWindow <= 0 {
		env.AnswerWindow = config.OutboundAnswerWindow
	}
	return &Service{repo: repo, ctl: ctl, env: env, now: time.Now}
}

// Defaults for the fields a create may leave out.
const (
	DefaultMaxConcurrent  = 5
	DefaultRingTimeoutSec = 30
	maxMaxConcurrent      = 100000
	maxNameRunes          = 200
	minRingTimeoutSec     = int(config.MinOutboundRingTimeout / time.Second)
)

// New is what a client supplies to create a campaign. A nil MaxConcurrent or
// RingTimeoutSec means the default (an explicit 0 is a value, and refused); a
// nil RetryPolicy means the default policy.
type New struct {
	Name               string
	DIDID              uuid.UUID
	TimeZone           string
	CallingWindow      CallingWindow
	DefaultCountryCode string
	MaxConcurrent      *int
	RingTimeoutSec     *int
	RetryPolicy        *RetryPolicy
	FollowUpQueueID    *uuid.UUID
}

// Patch is a change to a campaign: the fields present replace the stored ones.
// ClearDefaultCountryCode and ClearFollowUpQueue set those two optional
// fields back to unset.
type Patch struct {
	Name                    *string
	DIDID                   *uuid.UUID
	TimeZone                *string
	CallingWindow           *CallingWindow
	DefaultCountryCode      *string
	ClearDefaultCountryCode bool
	MaxConcurrent           *int
	RingTimeoutSec          *int
	RetryPolicy             *RetryPolicy
	FollowUpQueueID         *uuid.UUID
	ClearFollowUpQueue      bool
}

// Create validates and stores a DRAFT campaign.
func (s *Service) Create(ctx context.Context, in New) (Campaign, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Campaign{}, err
	}
	c := Campaign{
		ID: id, Name: in.Name, DIDID: in.DIDID, Status: StatusDraft,
		TimeZone: in.TimeZone, CallingWindow: in.CallingWindow,
		DefaultCountryCode: in.DefaultCountryCode,
		MaxConcurrent:      DefaultMaxConcurrent, RingTimeoutSec: DefaultRingTimeoutSec,
		FollowUpQueueID: in.FollowUpQueueID,
		RetryPolicy:     DefaultRetryPolicy(),
	}
	if in.RetryPolicy != nil {
		c.RetryPolicy = *in.RetryPolicy
	}
	if in.MaxConcurrent != nil {
		c.MaxConcurrent = *in.MaxConcurrent
	}
	if in.RingTimeoutSec != nil {
		c.RingTimeoutSec = *in.RingTimeoutSec
	}
	all := changes{}
	for _, field := range updatable {
		all[field] = true
	}
	if c, err = s.validate(ctx, c, all); err != nil {
		return Campaign{}, err
	}
	return s.repo.Create(ctx, c)
}

// Get returns one campaign.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return s.repo.Get(ctx, id)
}

// List pages campaigns, newest first.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Campaign, int64, error) {
	if f.Status != "" && !f.Status.IsValid() {
		return nil, 0, invalid("status", "UNKNOWN_STATUS", "%q is not a campaign status", f.Status)
	}
	return s.repo.List(ctx, f)
}

// mutable fields while a campaign runs or is paused; everything else is
// locked, and while it is DRAFT nothing is.
var mutableWhileRunning = map[string]bool{"maxConcurrent": true, "callingWindow": true, "retryPolicy": true}

// updatable lists the fields of a campaign a client may write, in the
// contract's spelling.
var updatable = []string{
	"name", "didId", "timeZone", "callingWindow", "defaultCountryCode",
	"maxConcurrent", "ringTimeoutSec", "retryPolicy", "followUpQueueId",
}

// changes is a set of fields, in the contract's spelling.
type changes map[string]bool

// Update applies a patch under the campaign's field locks (design 10
// section 14): in DRAFT every field may change; in RUNNING and PAUSED only
// maxConcurrent, callingWindow and retryPolicy, and they affect later
// admissions, not calls in flight; in a terminal status none. A field is
// "changed" when its new value differs from the stored one, so a client that
// sends the whole form back is not refused for the fields it did not touch.
func (s *Service) Update(ctx context.Context, id uuid.UUID, p Patch) (Campaign, error) {
	return s.repo.Update(ctx, id, func(cur Campaign) (Campaign, error) {
		next := cur
		ch := changes{}
		if p.Name != nil {
			next.Name = *p.Name
		}
		if p.DIDID != nil {
			next.DIDID = *p.DIDID
		}
		if p.TimeZone != nil {
			next.TimeZone = *p.TimeZone
		}
		if p.CallingWindow != nil {
			next.CallingWindow = *p.CallingWindow
		}
		switch {
		case p.ClearDefaultCountryCode:
			next.DefaultCountryCode = ""
		case p.DefaultCountryCode != nil:
			next.DefaultCountryCode = *p.DefaultCountryCode
		}
		if p.MaxConcurrent != nil {
			next.MaxConcurrent = *p.MaxConcurrent
		}
		if p.RingTimeoutSec != nil {
			next.RingTimeoutSec = *p.RingTimeoutSec
		}
		if p.RetryPolicy != nil {
			next.RetryPolicy = *p.RetryPolicy
		}
		switch {
		case p.ClearFollowUpQueue:
			next.FollowUpQueueID = nil
		case p.FollowUpQueueID != nil:
			next.FollowUpQueueID = p.FollowUpQueueID
		}

		// Only what differs from the stored value is validated: a client that
		// sends the whole form back must not be refused for a field it did
		// not change (a DID disabled since, hours narrowed since). What
		// differs is then compared again after normalising, so a window
		// written in another order but meaning the same is not a change.
		touched := changes{}
		for _, field := range updatable {
			if differs(field, cur, next) {
				touched[field] = true
			}
		}
		var err error
		if next, err = s.validate(ctx, next, touched); err != nil {
			return Campaign{}, err
		}
		for field := range touched {
			if differs(field, cur, next) {
				ch[field] = true
			}
		}

		var lockedFields []string
		for _, field := range updatable {
			if !ch[field] {
				continue
			}
			switch {
			case cur.Status.IsTerminal():
				lockedFields = append(lockedFields, field)
			case cur.Status != StatusDraft && !mutableWhileRunning[field]:
				lockedFields = append(lockedFields, field)
			}
		}
		if len(lockedFields) > 0 {
			return Campaign{}, locked(cur.Status, lockedFields)
		}
		return next, nil
	})
}

func differs(field string, a, b Campaign) bool {
	switch field {
	case "name":
		return a.Name != b.Name
	case "didId":
		return a.DIDID != b.DIDID
	case "timeZone":
		return a.TimeZone != b.TimeZone
	case "callingWindow":
		return fmt.Sprint(a.CallingWindow) != fmt.Sprint(b.CallingWindow)
	case "defaultCountryCode":
		return a.DefaultCountryCode != b.DefaultCountryCode
	case "maxConcurrent":
		return a.MaxConcurrent != b.MaxConcurrent
	case "ringTimeoutSec":
		return a.RingTimeoutSec != b.RingTimeoutSec
	case "retryPolicy":
		return fmt.Sprint(a.RetryPolicy) != fmt.Sprint(b.RetryPolicy)
	case "followUpQueueId":
		if a.FollowUpQueueID == nil || b.FollowUpQueueID == nil {
			return a.FollowUpQueueID != b.FollowUpQueueID
		}
		return *a.FollowUpQueueID != *b.FollowUpQueueID
	}
	return false
}

// Delete removes a DRAFT campaign. A campaign that has started is history and
// is cancelled, not deleted.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}

// Start moves DRAFT to RUNNING. The campaign is checked once more first: its
// DID may have lost its flow or been disabled since it was written, and the
// outer calling hours may have been narrowed.
func (s *Service) Start(ctx context.Context, id uuid.UUID) (Campaign, error) {
	cur, err := s.repo.Get(ctx, id)
	if err != nil {
		return Campaign{}, err
	}
	if cur.Status != StatusDraft {
		return Campaign{}, NewTransitionError(cur.Status, "START")
	}
	if _, err := s.validate(ctx, cur, changes{
		"didId": true, "timeZone": true, "callingWindow": true, "ringTimeoutSec": true,
	}); err != nil {
		return Campaign{}, err
	}
	return s.ctl.Start(ctx, id)
}

// Pause moves RUNNING to PAUSED with reason MANUAL.
func (s *Service) Pause(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return s.ctl.Pause(ctx, id)
}

// Resume moves PAUSED to RUNNING, whatever paused it.
func (s *Service) Resume(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return s.ctl.Resume(ctx, id)
}

// Cancel moves DRAFT, RUNNING or PAUSED to CANCELLED. Calls in flight finish.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) (Campaign, error) {
	return s.ctl.Cancel(ctx, id)
}

// ListContacts pages a campaign's contacts in import order.
func (s *Service) ListContacts(ctx context.Context, id uuid.UUID, f ContactFilter) ([]Contact, int64, error) {
	if f.Status != "" && !f.Status.IsValid() {
		return nil, 0, invalid("status", "UNKNOWN_STATUS", "%q is not a contact status", f.Status)
	}
	if f.LeadIntent != "" && !f.LeadIntent.IsValid() {
		return nil, 0, invalid("leadIntent", "UNKNOWN_INTENT", "%q is not a lead intent", f.LeadIntent)
	}
	if _, err := s.repo.Get(ctx, id); err != nil {
		return nil, 0, err
	}
	return s.repo.ListContacts(ctx, id, f)
}

// validate checks the fields named in touched and returns the campaign with
// them in canonical form. Fields not touched are not looked at: a campaign
// written under yesterday's outer hours must still accept a new maxConcurrent.
func (s *Service) validate(ctx context.Context, c Campaign, touched changes) (Campaign, error) {
	if touched["name"] {
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" {
			return c, invalid("name", "EMPTY", "a campaign needs a name")
		}
		if utf8.RuneCountInString(c.Name) > maxNameRunes {
			return c, invalid("name", "TOO_LONG", "a name is at most %d characters", maxNameRunes)
		}
	}
	if touched["timeZone"] {
		if _, err := LoadZone(c.TimeZone); err != nil {
			return c, invalid("timeZone", "TIME_ZONE_INVALID", "%v", err)
		}
	}
	if touched["callingWindow"] {
		w, err := c.CallingWindow.Normalize(s.env.OutboundHours)
		if err != nil {
			return c, err
		}
		c.CallingWindow = w
	}
	if touched["maxConcurrent"] && (c.MaxConcurrent < 1 || c.MaxConcurrent > maxMaxConcurrent) {
		return c, invalid("maxConcurrent", "OUT_OF_RANGE",
			"maxConcurrent is from 1 to %d, got %d", maxMaxConcurrent, c.MaxConcurrent)
	}
	if touched["ringTimeoutSec"] {
		ring := time.Duration(c.RingTimeoutSec) * time.Second
		switch {
		case c.RingTimeoutSec < minRingTimeoutSec:
			return c, invalid("ringTimeoutSec", "OUT_OF_RANGE",
				"ringTimeoutSec is at least %d, got %d", minRingTimeoutSec, c.RingTimeoutSec)
		case ring >= s.env.AnswerWindow:
			return c, &Error{
				Kind: ErrRingTimeoutTooLong, Field: "ringTimeoutSec", Rule: "NOT_BELOW_ANSWER_WINDOW",
				Message: fmt.Sprintf("ringTimeoutSec %d must be below the %d-second answer window: a call answered after the window would reach a customer who hears silence",
					c.RingTimeoutSec, int(s.env.AnswerWindow/time.Second)),
				Params: map[string]any{"answerWindowSec": int(s.env.AnswerWindow / time.Second)},
			}
		}
	}
	if touched["retryPolicy"] {
		p, err := c.RetryPolicy.Normalize()
		if err != nil {
			return c, err
		}
		c.RetryPolicy = p
	}
	if touched["defaultCountryCode"] && c.DefaultCountryCode != "" {
		if err := config.ValidateDefaultCountryCode(c.DefaultCountryCode); err != nil {
			return c, invalid("defaultCountryCode", "INVALID", "a country calling code is 1 to 3 digits, no +, got %q", c.DefaultCountryCode)
		}
	}
	if touched["didId"] {
		d, err := s.repo.DIDFlow(ctx, c.DIDID)
		if errors.Is(err, ErrDIDNotFound) {
			return c, invalid("didId", "DID_NOT_FOUND", "no such DID")
		}
		if err != nil {
			return c, err
		}
		switch {
		case !d.IsEnabled:
			return c, invalid("didId", "DID_DISABLED", "DID %s is disabled", d.Number)
		case !d.AllowOutbound:
			return c, &Error{
				Kind: ErrDIDNotOutbound, Field: "didId", Rule: "DID_NOT_OUTBOUND",
				Message: fmt.Sprintf("DID %s does not allow outbound calls; a campaign dials from its DID", d.Number),
				Params:  map[string]any{"didNumber": d.Number},
			}
		case d.Spec == nil:
			return c, invalid("didId", "DID_HAS_NO_PUBLISHED_FLOW",
				"DID %s has no published flow; a campaign talks to a flow", d.Number)
		case flow.CanTransfer(d.Spec):
			return c, &Error{
				Kind: ErrFlowHasTransfer, Field: "didId", Rule: "FLOW_CAN_TRANSFER",
				Message: fmt.Sprintf("the flow behind DID %s can transfer the caller to a person; a campaign call has no way to one", d.Number),
				Params:  map[string]any{"didNumber": d.Number},
			}
		}
		c.DIDNumber = d.Number
	}
	return c, nil
}

// CountryCode is the country code that completes this campaign's national
// numbers: its own, else the deployment's.
func (s *Service) countryCode(c Campaign) string {
	if c.DefaultCountryCode != "" {
		return c.DefaultCountryCode
	}
	return s.env.DefaultCountryCode
}
