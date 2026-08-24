// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ExtensionKind classifies what registers at an extension.
type ExtensionKind string

// Extension kinds.
const (
	KindAgent ExtensionKind = "AGENT"
	KindQueue ExtensionKind = "QUEUE"
)

// Extension is a SIP endpoint the switch will accept a registration for.
type Extension struct {
	ID          uuid.UUID     `json:"id"`
	Number      string        `json:"number"`
	Kind        ExtensionKind `json:"kind"`
	DisplayName string        `json:"displayName"`
	IsEnabled   bool          `json:"isEnabled"`
	CreatedAt   time.Time     `json:"createdAt"`
	// Password is write-only: it is accepted on create and update and never
	// returned, because the only reader that needs it is the switch. Reading
	// it back is a separate, audited request.
	Password string `json:"password,omitempty"`

	// What this number serves, at most one of them, matching Kind.
	//
	// AgentID is read-only here: the binding is written from the agent's side,
	// where the database can still refuse to delete a phone somebody works at.
	AgentID *uuid.UUID `json:"agentId,omitempty"`
	QueueID *uuid.UUID `json:"queueId,omitempty"`
}

// NewExtension is the shape a create or update body is decoded into: the
// server's defaults, already applied.
//
// A boolean cannot be defaulted after the fact. Every other default in this
// file is applied in validate, which cannot tell a field the operator omitted
// from one they sent as false — both arrive as Go's zero value. So the
// defaults that are booleans are seeded before the decoder runs, and JSON
// leaves an absent field untouched: omitted keeps the default, an explicit
// false still wins. Decoding into a bare struct is how extensions came to be
// created disabled — invisible to the directory view the switch reads, so the
// phone simply never registered and nothing in the API said why.
func NewExtension() Extension {
	return Extension{IsEnabled: true}
}

func (e *Extension) validate(requirePassword bool) error {
	e.Number = trim(e.Number)
	if !digitsOnly(e.Number) {
		return fmt.Errorf("%w: number must be digits", ErrValidation)
	}
	if err := e.validateApartFromNumber(requirePassword); err != nil {
		return err
	}
	e.NameAfterNumber()
	return nil
}

// validateApartFromNumber checks everything that can be checked before a
// number exists. Allocation assigns the number inside its own transaction, so
// it cannot run the whole of validate up front — and running none of it would
// hold the pool lock while discovering that the password was too short.
func (e *Extension) validateApartFromNumber(requirePassword bool) error {
	e.DisplayName = trim(e.DisplayName)

	switch e.Kind {
	case KindAgent, KindQueue:
	case "":
		e.Kind = KindAgent
	default:
		return fmt.Errorf("%w: unknown extension kind %q", ErrValidation, e.Kind)
	}
	if requirePassword && len(e.Password) < 6 {
		return fmt.Errorf("%w: password must be at least 6 characters", ErrValidation)
	}
	// A number serves one thing. The database refuses the contradiction too,
	// but saying which field is wrong is this layer's job.
	if e.Kind == KindQueue {
		if e.QueueID == nil {
			return fmt.Errorf("%w: a QUEUE extension needs the queue it reaches", ErrValidation)
		}
	} else {
		// A target left behind by a changed kind is a claim nothing honours.
		e.QueueID = nil
	}
	return nil
}

// NameAfterNumber gives an unnamed extension the only name it can be given
// before anybody has used it. Called once the number is known, which for an
// allocated extension is not until it is being written.
func (e *Extension) NameAfterNumber() {
	if e.DisplayName == "" {
		e.DisplayName = "Extension " + e.Number
	}
}

// Strategy is how a queue picks among the agents staffing it.
type Strategy string

// Distribution strategies, in our own vocabulary. The switch spells them
// differently; that translation lives at the boundary.
const (
	StrategyLongestIdle   Strategy = "LONGEST_IDLE_AGENT"
	StrategyRoundRobin    Strategy = "ROUND_ROBIN"
	StrategyTopDown       Strategy = "TOP_DOWN"
	StrategyLeastTalkTime Strategy = "AGENT_WITH_LEAST_TALK_TIME"
	StrategyFewestCalls   Strategy = "AGENT_WITH_FEWEST_CALLS"
	StrategyRandom        Strategy = "RANDOM"
)

var strategies = map[Strategy]bool{
	StrategyLongestIdle: true, StrategyRoundRobin: true, StrategyTopDown: true,
	StrategyLeastTalkTime: true, StrategyFewestCalls: true, StrategyRandom: true,
}

// OverflowType is what happens to a caller who leaves a queue unserved.
type OverflowType string

// Overflow behaviours.
const (
	OverflowAnnounceHangup OverflowType = "ANNOUNCE_HANGUP"
	OverflowBotFlow        OverflowType = "BOT_FLOW"
	OverflowForward        OverflowType = "FORWARD"
)

// Overflow describes where an unserved caller goes.
type Overflow struct {
	Type   OverflowType `json:"type"`
	Target string       `json:"target,omitempty"`
	Sound  string       `json:"sound,omitempty"`
}

// TierRules control how a queue widens its search over time.
type TierRules struct {
	IsApplied bool `json:"isApplied"`
	WaitSec   int  `json:"waitSec"`
}

// BusinessHours is one day's opening window.
type BusinessHours struct {
	Weekday int    `json:"weekday"` // 0 is Sunday
	Open    string `json:"open"`    // HH:MM
	Close   string `json:"close"`
}

// Queue is a waiting line served by agents.
type Queue struct {
	ID                       uuid.UUID       `json:"id"`
	Name                     string          `json:"name"`
	ExtNumber                string          `json:"extNumber"`
	DisplayName              string          `json:"displayName"`
	Strategy                 Strategy        `json:"strategy"`
	MohSound                 string          `json:"mohSound"`
	MaxWaitSec               int             `json:"maxWaitSec"`
	MaxWaitNoAgentSec        int             `json:"maxWaitNoAgentSec"`
	AnnounceSound            string          `json:"announceSound,omitempty"`
	AnnounceFrequencySec     int             `json:"announceFrequencySec"`
	TierRules                TierRules       `json:"tierRules"`
	DiscardAbandonedAfterSec int             `json:"discardAbandonedAfterSec"`
	IsAbandonedResumeAllowed bool            `json:"isAbandonedResumeAllowed"`
	RonaDelaySec             int             `json:"ronaDelaySec"`
	SLAThresholdSec          int             `json:"slaThresholdSec"`
	IsRecordingEnabled       bool            `json:"isRecordingEnabled"`
	Hours                    []BusinessHours `json:"hours"`
	Overflow                 Overflow        `json:"overflow"`
	IsEnabled                bool            `json:"isEnabled"`
}

// NewQueue is the shape a create or update body is decoded into. See
// NewExtension for why the boolean defaults are seeded rather than applied in
// validate. isAbandonedResumeAllowed is left false, which is its default.
//
// The three integers are here for exactly the same reason, and it took a
// second incident to see it (C33): a default that is not the zero value cannot
// be applied afterwards either, because validate cannot tell a field the
// operator omitted from one they sent as 0. The INSERT names every column, so
// the column defaults never got a turn, and a queue created through the API
// came out with no RONA wait, an SLA threshold of zero seconds and abandoned
// callers discarded at once.
//
// That half is worse than the boolean half was. A queue created disabled does
// not work, and not working gets reported. A queue created with a zero SLA
// threshold runs perfectly and measures the wrong thing — support-zh sat at
// 0|0|0 in this database, so every SLA figure ever taken from it was against a
// threshold of zero rather than the twenty seconds the schema says.
//
// The same seeding also stops an update from abrading them: a PUT that leaves
// these fields out now restores the default instead of writing zero, which is
// how support-zh is believed to have been flattened in the first place.
func NewQueue() Queue {
	return Queue{
		IsEnabled:                true,
		IsRecordingEnabled:       true,
		DiscardAbandonedAfterSec: 60,
		RonaDelaySec:             10,
		SLAThresholdSec:          20,
	}
}

func (q *Queue) validate() error {
	q.Name = trim(q.Name)
	q.ExtNumber = trim(q.ExtNumber)
	q.DisplayName = trim(q.DisplayName)

	if q.Name == "" {
		return fmt.Errorf("%w: name is required", ErrValidation)
	}
	// The name reaches the switch as part of a queue identifier, so it stays
	// to characters that survive that trip unambiguously.
	for _, r := range q.Name {
		if r == '@' || r == ' ' || r == '\'' {
			return fmt.Errorf("%w: name cannot contain spaces, @ or quotes", ErrValidation)
		}
	}
	if !digitsOnly(q.ExtNumber) {
		return fmt.Errorf("%w: queue extension must be digits", ErrValidation)
	}
	if q.Strategy == "" {
		q.Strategy = StrategyLongestIdle
	}
	if !strategies[q.Strategy] {
		return fmt.Errorf("%w: unknown strategy %q", ErrValidation, q.Strategy)
	}
	switch q.Overflow.Type {
	case "":
		q.Overflow.Type = OverflowAnnounceHangup
	case OverflowAnnounceHangup:
	case OverflowBotFlow, OverflowForward:
		if trim(q.Overflow.Target) == "" {
			return fmt.Errorf("%w: overflow %s needs a target", ErrValidation, q.Overflow.Type)
		}
	default:
		return fmt.Errorf("%w: unknown overflow type %q", ErrValidation, q.Overflow.Type)
	}
	for _, h := range q.Hours {
		if h.Weekday < 0 || h.Weekday > 6 {
			return fmt.Errorf("%w: weekday must be 0 to 6", ErrValidation)
		}
	}
	if q.MohSound == "" {
		q.MohSound = "$${hold_music}"
	}
	if q.DisplayName == "" {
		q.DisplayName = q.Name
	}
	if q.TierRules.WaitSec <= 0 {
		q.TierRules.WaitSec = 300
	}
	if q.Hours == nil {
		q.Hours = []BusinessHours{}
	}
	return nil
}

// QueueAgent is one agent's place on a queue.
type QueueAgent struct {
	QueueID     uuid.UUID `json:"queueId"`
	AgentID     uuid.UUID `json:"agentId"`
	DisplayName string    `json:"displayName"`
	Level       int       `json:"level"`
	Position    int       `json:"position"`
}

// DID is an external number that reaches this call centre.
type DID struct {
	ID     uuid.UUID `json:"id"`
	Number string    `json:"number"`
	// Language is a lowercase BCP 47 subtag, consumed verbatim by the
	// frontend. It sets the greeting, the prompt language and the voice; the
	// provider is a deployment-wide setting and this never selects it.
	Language string `json:"language"`
	// FlowID is the conversation the bot runs. Every number is meant to answer
	// with a bot; the flow catalogue arrives with the AI voice leg, so a
	// number may exist without one in the meantime.
	FlowID *uuid.UUID `json:"flowId,omitempty"`
	// FallbackQueueID is where the caller goes when the bot cannot take the
	// call at all: a provider outage, no capacity, or the gateway down.
	FallbackQueueID    *uuid.UUID `json:"fallbackQueueId,omitempty"`
	IsRecordingEnabled bool       `json:"isRecordingEnabled"`
	Description        string     `json:"description"`
	IsEnabled          bool       `json:"isEnabled"`
}

// NewDID is the shape a create or update body is decoded into. See
// NewExtension for why the boolean defaults are seeded rather than applied in
// validate.
func NewDID() DID {
	return DID{IsEnabled: true, IsRecordingEnabled: true}
}

func (d *DID) validate() error {
	d.Number = trim(d.Number)
	d.Language = trim(d.Language)
	d.Description = trim(d.Description)

	if !digitsOnly(d.Number) {
		return fmt.Errorf("%w: number must be digits", ErrValidation)
	}
	if d.Language == "" {
		d.Language = "en"
	}
	if len(d.Language) > 8 {
		return fmt.Errorf("%w: language must be a short subtag such as en or zh", ErrValidation)
	}
	return nil
}
