// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// CallState is the lifecycle of the call aggregate.
type CallState string

// Call states.
const (
	CallCreated CallState = "CREATED"
	CallRunning CallState = "RUNNING"
	CallEnding  CallState = "ENDING"
	CallEnded   CallState = "ENDED"
)

// PartyState is the lifecycle of one call leg.
type PartyState string

// Party states.
const (
	PartyDialing  PartyState = "DIALING"
	PartyRinging  PartyState = "RINGING"
	PartyTalking  PartyState = "TALKING"
	PartyHeld     PartyState = "HELD"
	PartyReleased PartyState = "RELEASED"
)

// PartyRole distinguishes the leg that started the call from the ones it
// reached. The first party of a call is always the originator.
type PartyRole string

// Party roles.
const (
	RoleOriginator PartyRole = "ORIGINATOR"
	RoleTarget     PartyRole = "TARGET"
)

// PartyTrigger is what moves a party between states.
type PartyTrigger string

// Party triggers.
const (
	TriggerAnswer   PartyTrigger = "ANSWER"
	TriggerHold     PartyTrigger = "HOLD"
	TriggerRetrieve PartyTrigger = "RETRIEVE"
	TriggerRelease  PartyTrigger = "RELEASE"
)

// partyTransitions is the complete party state machine. Anything absent is
// rejected: a transition table beats scattered conditionals when the switch
// delivers events out of order.
//
// Deliberately absent: DIALING→RINGING and RINGING→DIALING (a leg does not
// change role mid-life), and every edge out of RELEASED (terminal).
var partyTransitions = map[PartyState]map[PartyTrigger]PartyState{
	PartyDialing: {
		TriggerAnswer:  PartyTalking,
		TriggerRelease: PartyReleased,
	},
	PartyRinging: {
		TriggerAnswer:  PartyTalking,
		TriggerRelease: PartyReleased,
	},
	PartyTalking: {
		TriggerHold:    PartyHeld,
		TriggerRelease: PartyReleased,
		// A duplicate answer is a no-op rather than an error: FreeSWITCH can
		// report both CHANNEL_ANSWER and a bridge for the same leg.
		TriggerAnswer: PartyTalking,
	},
	PartyHeld: {
		TriggerRetrieve: PartyTalking,
		TriggerRelease:  PartyReleased,
	},
	PartyReleased: {},
}

// ErrIllegalTransition reports a state change the machine forbids.
type ErrIllegalTransition struct {
	From    PartyState
	Trigger PartyTrigger
}

func (e ErrIllegalTransition) Error() string {
	return fmt.Sprintf("illegal party transition: %s on %s", e.Trigger, e.From)
}

// Party is one leg of a call. PartyID is the FreeSWITCH channel UUID, assigned
// by us before the channel exists wherever we originate it.
type Party struct {
	PartyID   uuid.UUID
	ChannelID string
	Role      PartyRole
	State     PartyState

	// Number is this leg's own address: the caller's number for an inbound
	// originator, the dialed extension for a target.
	Number      string
	OtherNumber string
	AgentID     *uuid.UUID

	CreatedAt    time.Time
	AnsweredAt   time.Time
	ReleasedAt   time.Time
	ReleaseCause string
	// TransferredAway marks a leg that ended because the call moved on.
	TransferredAway bool
}

// apply moves the party, reporting an error for forbidden transitions.
func (p *Party) apply(trigger PartyTrigger, at time.Time) error {
	next, ok := partyTransitions[p.State][trigger]
	if !ok {
		return ErrIllegalTransition{From: p.State, Trigger: trigger}
	}
	if next == PartyTalking && p.AnsweredAt.IsZero() {
		p.AnsweredAt = at
	}
	if next == PartyReleased {
		p.ReleasedAt = at
	}
	p.State = next
	return nil
}

// IsActive reports whether this leg still exists on the switch.
func (p *Party) IsActive() bool { return p.State != PartyReleased }

// Call is the aggregate: one conversation, one or more parties, one identity
// that survives every transfer.
type Call struct {
	CallID   uuid.UUID
	CallType events.CallType
	State    CallState

	// Language and flow are the routing decisions taken at call setup.
	Language string
	QueueID  *uuid.UUID

	Parties []*Party

	// UserData is business context (ticketId, collected slots). It survives
	// transfers and lands in the CDR. Switch facts never live here.
	UserData map[string]any

	CreatedAt time.Time
	EndedAt   time.Time
}

// NewCall creates a call in CREATED with its immutable type stamped.
func NewCall(callID uuid.UUID, callType events.CallType, at time.Time) *Call {
	return &Call{
		CallID:    callID,
		CallType:  callType,
		State:     CallCreated,
		UserData:  map[string]any{},
		CreatedAt: at,
	}
}

// AddParty appends a leg. The first party is the originator and starts in
// DIALING; every later party starts in RINGING.
func (c *Call) AddParty(channelID string, number string, at time.Time) *Party {
	role, state := RoleTarget, PartyRinging
	if len(c.Parties) == 0 {
		role, state = RoleOriginator, PartyDialing
	}
	p := &Party{
		PartyID:   uuid.Must(uuid.NewV7()),
		ChannelID: channelID,
		Role:      role,
		State:     state,
		Number:    number,
		CreatedAt: at,
	}
	c.Parties = append(c.Parties, p)
	if c.State == CallCreated {
		c.State = CallRunning
	}
	return p
}

// PartyByChannel finds a leg by its FreeSWITCH channel UUID.
func (c *Call) PartyByChannel(channelID string) *Party {
	for _, p := range c.Parties {
		if p.ChannelID == channelID {
			return p
		}
	}
	return nil
}

// Originator returns the leg that started the call.
func (c *Call) Originator() *Party {
	for _, p := range c.Parties {
		if p.Role == RoleOriginator {
			return p
		}
	}
	return nil
}

// ActiveParties returns the legs still up on the switch.
func (c *Call) ActiveParties() []*Party {
	var out []*Party
	for _, p := range c.Parties {
		if p.IsActive() {
			out = append(out, p)
		}
	}
	return out
}

// AnsweredAt reports when the call was first answered by anybody, including
// the bot; the human/AI split comes from per-party timestamps.
func (c *Call) AnsweredAt() time.Time {
	var first time.Time
	for _, p := range c.Parties {
		if p.AnsweredAt.IsZero() {
			continue
		}
		if first.IsZero() || p.AnsweredAt.Before(first) {
			first = p.AnsweredAt
		}
	}
	return first
}

// MergeUserData applies an RFC 7386 style merge patch: null removes a key,
// any other value replaces it.
func (c *Call) MergeUserData(patch map[string]any) {
	if c.UserData == nil {
		c.UserData = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			delete(c.UserData, k)
			continue
		}
		c.UserData[k] = v
	}
}

// Finish moves the call towards its terminal state once no leg remains.
func (c *Call) Finish(at time.Time) bool {
	if len(c.ActiveParties()) > 0 || c.State == CallEnded {
		return false
	}
	c.State = CallEnded
	c.EndedAt = at
	return true
}

// Snapshot is an immutable view of a call for API responses and recovery.
type Snapshot struct {
	CallID    uuid.UUID       `json:"callId"`
	CallType  events.CallType `json:"callType"`
	State     CallState       `json:"state"`
	Language  string          `json:"language,omitempty"`
	QueueID   *uuid.UUID      `json:"queueId,omitempty"`
	Parties   []PartySnapshot `json:"parties"`
	UserData  map[string]any  `json:"userData,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
	EndedAt   *time.Time      `json:"endedAt,omitempty"`
}

// PartySnapshot is an immutable view of one leg.
type PartySnapshot struct {
	PartyID     uuid.UUID  `json:"partyId"`
	ChannelID   string     `json:"channelId"`
	Role        PartyRole  `json:"role"`
	State       PartyState `json:"state"`
	Number      string     `json:"number,omitempty"`
	OtherNumber string     `json:"otherNumber,omitempty"`
	AgentID     *uuid.UUID `json:"agentId,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	AnsweredAt  *time.Time `json:"answeredAt,omitempty"`
	ReleasedAt  *time.Time `json:"releasedAt,omitempty"`
}

// Snapshot copies the call into a value safe to hand outside the actor.
func (c *Call) Snapshot() Snapshot {
	s := Snapshot{
		CallID:    c.CallID,
		CallType:  c.CallType,
		State:     c.State,
		Language:  c.Language,
		QueueID:   c.QueueID,
		UserData:  maps.Clone(c.UserData),
		CreatedAt: c.CreatedAt,
		Parties:   make([]PartySnapshot, 0, len(c.Parties)),
	}
	if !c.EndedAt.IsZero() {
		ended := c.EndedAt
		s.EndedAt = &ended
	}
	for _, p := range c.Parties {
		ps := PartySnapshot{
			PartyID:     p.PartyID,
			ChannelID:   p.ChannelID,
			Role:        p.Role,
			State:       p.State,
			Number:      p.Number,
			OtherNumber: p.OtherNumber,
			AgentID:     p.AgentID,
			CreatedAt:   p.CreatedAt,
		}
		if !p.AnsweredAt.IsZero() {
			answered := p.AnsweredAt
			ps.AnsweredAt = &answered
		}
		if !p.ReleasedAt.IsZero() {
			released := p.ReleasedAt
			ps.ReleasedAt = &released
		}
		s.Parties = append(s.Parties, ps)
	}
	return s
}
