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
	KindBot   ExtensionKind = "BOT"
	KindPlain ExtensionKind = "PLAIN"
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
	// returned, because the only reader that needs it is the switch.
	Password string `json:"password,omitempty"`
}

func (e *Extension) validate(requirePassword bool) error {
	e.Number = trim(e.Number)
	e.DisplayName = trim(e.DisplayName)

	if !digitsOnly(e.Number) {
		return fmt.Errorf("%w: number must be digits", ErrValidation)
	}
	switch e.Kind {
	case KindAgent, KindBot, KindPlain:
	case "":
		e.Kind = KindAgent
	default:
		return fmt.Errorf("%w: unknown extension kind %q", ErrValidation, e.Kind)
	}
	if requirePassword && len(e.Password) < 6 {
		return fmt.Errorf("%w: password must be at least 6 characters", ErrValidation)
	}
	if e.DisplayName == "" {
		e.DisplayName = "Extension " + e.Number
	}
	return nil
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
	// frontend and used to pick a voice provider.
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
