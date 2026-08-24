// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
	"maps"
	"slices"
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
// delivers events out of order. It is the only thing that may move a party —
// assigning a state anywhere else skips this table and is a defect.
//
// The specification is the diagram in docs/design/01-telephony.md §2 (owner,
// 2026-08-24). Where this table still differs from it — no IDLE and no
// QUEUED — the differences are named there and tracked as C56; do not close
// the gap piecemeal here, PartyState is on the wire.
//
// Deliberately absent: DIALING→RINGING and RINGING→DIALING. A leg does not
// change role mid-life — DIALING is the party that placed the call, RINGING
// is a party being offered one, and an edge between them would mean a leg
// became somebody else. The caller's ringback is the *other* party's RINGING.
// The first draft of the specification carried DIALING→RINGING; the owner
// ruled it forbidden (2026-08-24), so this table stands and the diagram was
// amended. Every edge out of RELEASED is absent too (terminal).
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

// OpenBridge records that this leg has become bridged to another, if it is not
// already recorded as bridged. Idempotent: the switch can report the same
// bridge from both legs, and a leg retrieved from hold re-bridges to a stretch
// that was never closed.
func (p *Party) OpenBridge(otherChannelID string, at time.Time) {
	if n := len(p.Bridges); n > 0 && p.Bridges[n-1].EndedAt.IsZero() {
		return
	}
	p.Bridges = append(p.Bridges, BridgeSpan{OtherChannelID: otherChannelID, StartedAt: at})
}

// CloseBridge ends the open stretch, if there is one.
func (p *Party) CloseBridge(at time.Time) {
	if n := len(p.Bridges); n > 0 && p.Bridges[n-1].EndedAt.IsZero() {
		p.Bridges[n-1].EndedAt = at
	}
}

// BridgedSec is the total time these legs spent in two-way media, counting any
// stretch they shared only once. Overlap is real: during a consultation the
// caller is bridged to two agents at once, and an extension calling another
// extension puts the same conversation on two legs that both belong to agents.
//
// A stretch still open when the call ended is closed at endedAt.
func BridgedSec(parties []*PartySnapshot, endedAt time.Time) int {
	type span struct{ from, to time.Time }
	var spans []span
	for _, p := range parties {
		for _, b := range p.Bridges {
			to := b.EndedAt
			if to.IsZero() {
				to = endedAt
			}
			if to.After(b.StartedAt) {
				spans = append(spans, span{b.StartedAt, to})
			}
		}
	}
	if len(spans) == 0 {
		return 0
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from.Compare(b.from) })

	var total time.Duration
	cur := spans[0]
	for _, s := range spans[1:] {
		if s.from.After(cur.to) {
			total += cur.to.Sub(cur.from)
			cur = s
			continue
		}
		if s.to.After(cur.to) {
			cur.to = s.to
		}
	}
	total += cur.to.Sub(cur.from)
	return int(total.Seconds())
}

// FirstBridgeAt is when these legs first carried a conversation, or the zero
// time if none ever did.
func FirstBridgeAt(parties []*PartySnapshot) time.Time {
	var first time.Time
	for _, p := range parties {
		for _, b := range p.Bridges {
			if first.IsZero() || b.StartedAt.Before(first) {
				first = b.StartedAt
			}
		}
	}
	return first
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
// BridgeSpan is one stretch of two-way media between this leg and another.
// EndedAt is zero while the bridge is still up.
//
// Answering is not the same as being heard: an auto-answer phone picks up in
// front of nobody, and a leg whose codec cannot meet the caller's returns a
// clean 200 with no media at all. The bridge is what says a conversation
// happened.
type BridgeSpan struct {
	OtherChannelID string    `json:"otherChannelId,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	EndedAt        time.Time `json:"endedAt,omitzero"`
}

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
	// IsBotLeg marks the leg the switch dialed towards the AI gateway. A call
	// that has one and was never handed to a person belongs to the bot's
	// ledger, not this path's.
	IsBotLeg bool
	// Bridges are the stretches during which this leg had two-way media with
	// another. A leg can have several: the caller is bridged to the bot, then
	// to the agent who takes the call over, then to whoever that agent
	// transfers it to. Whose leg a stretch sits on is what tells a
	// conversation with a person from one with the bot — an agent's leg is
	// never bridged to the bot's.
	Bridges []BridgeSpan
	// BilledSec is the switch's own count of this leg's answered seconds, kept
	// beside ours so the two can be compared rather than merely trusted.
	BilledSec int
	// IsMuted tracks the switch-side mute on this leg. The switch reports no
	// event for it and no channel variable survives a re-read, so this is the
	// only record that the agent's microphone is off — which is precisely why
	// it must live on the party rather than in a browser's memory: a reload,
	// a second tab or a supervisor's view would each answer differently.
	IsMuted bool
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
	// IsMintedID marks an identity minted by the dialplan before any leg
	// existed. When two provisional calls turn out to be one conversation,
	// the minted identity is the one every other record refers to.
	IsMintedID bool

	// Language and flow are the routing decisions taken at call setup.
	Language string
	QueueID  *uuid.UUID

	Parties []*Party

	// UserData is business context (ticketId, collected slots). It survives
	// transfers and lands in the CDR. Switch facts never live here.
	UserData map[string]any

	// Queue is what happened between joining a queue and reaching a person,
	// recorded as the callcenter events arrive.
	Queue QueueFacts
	// Bot is the AI leg's share of the story, captured from the caller's
	// channel when that leg hangs up.
	Bot BotShare

	CreatedAt time.Time
	EndedAt   time.Time
}

// QueueFacts is a call's passage through a queue.
type QueueFacts struct {
	Name         string     `json:"name,omitempty"`
	JoinedAt     time.Time  `json:"joinedAt,omitzero"`
	BridgedAt    time.Time  `json:"bridgedAt,omitzero"`
	LeftAt       time.Time  `json:"leftAt,omitzero"`
	Cause        string     `json:"cause,omitempty"`
	CancelReason string     `json:"cancelReason,omitempty"`
	AgentID      *uuid.UUID `json:"agentId,omitempty"`
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
// AgentIDs is every agent with a party on this call, which is the audience for
// anything that happens on it.
//
// Every agent, not the agent whose leg the event is about. A call is one
// conversation and both sides of it are on one screen: the moment that matters
// most is the *caller* releasing, and telling only the caller's own agent —
// of which there is none — leaves the agent's softphone showing a call that
// ended. It is also a superset of whoever is bridged at this instant, so a
// consult does not make the other agent's panel flicker.
//
// Released parties still count. An agent who was on the call is entitled to
// see it end.
func (c *Call) AgentIDs() []uuid.UUID {
	var out []uuid.UUID
	for _, p := range c.Parties {
		if p.AgentID != nil && !slices.Contains(out, *p.AgentID) {
			out = append(out, *p.AgentID)
		}
	}
	return out
}

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
	Queue     QueueFacts      `json:"queue,omitzero"`
	Bot       BotShare        `json:"-"`
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
	// ReleaseCause is the switch's word for why the leg ended.
	ReleaseCause string `json:"releaseCause,omitempty"`
	IsBotLeg     bool   `json:"isBotLeg,omitempty"`
	IsMuted      bool   `json:"isMuted,omitempty"`
	// Bridges is this leg's two-way-media history; see BridgeSpan.
	Bridges []BridgeSpan `json:"bridges,omitempty"`
	// BilledSec is the switch's own count of this leg's answered seconds.
	BilledSec int `json:"billedSec,omitempty"`
}

// IsActive reports whether this leg is still on the call.
func (p PartySnapshot) IsActive() bool {
	return p.State != PartyReleased
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
		Queue:     c.Queue,
		Bot:       c.Bot,
		CreatedAt: c.CreatedAt,
		Parties:   make([]PartySnapshot, 0, len(c.Parties)),
	}
	if !c.EndedAt.IsZero() {
		ended := c.EndedAt
		s.EndedAt = &ended
	}
	for _, p := range c.Parties {
		ps := PartySnapshot{
			PartyID:      p.PartyID,
			ChannelID:    p.ChannelID,
			Role:         p.Role,
			State:        p.State,
			Number:       p.Number,
			OtherNumber:  p.OtherNumber,
			AgentID:      p.AgentID,
			CreatedAt:    p.CreatedAt,
			ReleaseCause: p.ReleaseCause,
			IsBotLeg:     p.IsBotLeg,
			IsMuted:      p.IsMuted,
			Bridges:      slices.Clone(p.Bridges),
			BilledSec:    p.BilledSec,
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
