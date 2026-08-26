// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"encoding/json"
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
	CallEnded   CallState = "ENDED"
	// No ENDING. A call ends when its last party releases, which is one
	// event, so there was never a moment to be in it — the state was declared,
	// never assigned and never read (C4). A name in an enum that nothing can
	// produce is a promise to every client that it might.
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
// 2026-08-24). This table implements it, and after C56 the two say the same
// thing. What differs is shape, not behaviour: the diagram's IDLE is birth and
// death, and a party is a channel, so it is born DIALING or RINGING and ends
// in the terminal RELEASED. Two states were considered for the diagram and
// withdrawn — QUEUED, because a caller waiting in a queue is a channel with
// music playing and where the call is belongs to the call (call.Queue,
// queue_events, queue_wait_sec) rather than to the leg; and a separate
// abandon trigger, because abandoning is a hangup and the cause already says
// which kind (cdrs.missed_reason reads it).
// PartyState is on the wire, so do not add a state here without the contract.
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
	// ExtensionNumber is the extension this leg is at, when it is at a phone
	// this platform manages. Set from what the leg itself says — our own
	// originate stamps aicc_extension, and the directory stamps it on a
	// phone's own INVITE — so it is a fact about the phone and holds whether
	// or not anybody is signed in there.
	//
	// AgentID answers a different question: *whose* phone it is, which only
	// presence can say. The two came apart the day a system could place a call
	// for an agent who never signed into this application, and conflating them
	// cost that call its talk time and its leg record: the ledger asks "did an
	// agent's phone place this?" and was reading "is the originator a known
	// agent?" instead.
	ExtensionNumber string

	CreatedAt    time.Time
	AnsweredAt   time.Time
	ReleasedAt   time.Time
	ReleaseCause string
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

// HangupCause is the reason to end this leg with, in the switch's vocabulary.
//
// One rule, three answers, and the switch acts on the difference — this is not
// decoration (owner directive 2026-08-25):
//
//   - The conversation is up: an ordinary goodbye, NORMAL_CLEARING.
//   - Not up yet and this leg started the call: the caller changed their mind,
//     ORIGINATOR_CANCEL.
//   - Not up yet and this leg was the one being called: they were rung and
//     said no, CALL_REJECTED.
//
// What it costs to get wrong, measured against this switch: mod_callcenter
// counts a leg that ends in any cause it does not recognise as one that failed
// to answer, so an agent declining a ringing call under NORMAL_CLEARING — what
// this sent before — had no_answer_count incremented, and with max_no_answer
// at 2 was benched after two declines as though they had been ignoring the
// phone. Under CALL_REJECTED the count stays at 0 and only reject_delay_time
// applies; under ORIGINATOR_CANCEL neither does.
//
// TALKING rather than answeredAt is the test for "up", and the difference is
// the auto-answer phone: it picks up in front of nobody, and a leg that
// answered is not a conversation. A party reaches TALKING on the bridge.
func (p *Party) HangupCause() string {
	switch {
	case p.State == PartyTalking || p.State == PartyHeld:
		return "NORMAL_CLEARING"
	case p.Role == RoleOriginator:
		return "ORIGINATOR_CANCEL"
	default:
		return "CALL_REJECTED"
	}
}

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

// The bounds on a call's business data, as the contract states them
// (`UserData` in docs/openapi.json: maxProperties 32, values maxLength 1024).
//
// They live here rather than in the HTTP layer because they are a fact about
// what a call may carry, not about how one request happened to arrive. Every
// other way in — a channel variable on an inbound call, a REFER's context, a
// tool the bot calls — reaches this method and nothing else, and a limit only
// the HTTP handler knew would be a limit those paths silently walked past.
const (
	UserDataMaxKeys       = 32
	UserDataMaxValueBytes = 1024
)

// UserDataChange reports what a merge actually did, in sorted order.
//
// Changed and Deleted name real movement: a key set to the value it already
// holds is in neither, and so is a delete of a key that was not there. What is
// announced to a screen has to be something that moved, or a transfer that
// carries identical data across would report a change nobody made.
//
// Dropped names what the bounds refused. It is separate from the other two
// because it is the caller's to act on: a request can answer 400, a phone call
// cannot, so the paths that cannot refuse log it instead.
type UserDataChange struct {
	Changed []string
	Deleted []string
	Dropped []string
}

// IsEmpty reports that the merge moved nothing at all.
func (u UserDataChange) IsEmpty() bool {
	return len(u.Changed) == 0 && len(u.Deleted) == 0
}

// MergeUserData applies an RFC 7386 style merge patch — null removes a key,
// any other value replaces it — within the bounds above.
//
// The order is deletions, then replacements, then additions, and it is the
// order that makes the result predictable rather than an accident of map
// iteration:
//
//   - Deletions are always free. Removing a key cannot breach a bound, so a
//     patch that deletes two and adds two nets zero and always fits.
//   - A key the call already carries is never evicted to make room for a new
//     one. Business data a call has been carrying since it started is not
//     something a later patch gets to displace.
//   - Additions go in **sorted** order until the key bound is reached, and the
//     rest are dropped. Which ones survive has to be the same on every run:
//     unsorted, the same patch against the same call would keep a different
//     pair each time, and nobody could tell from the outside why.
//
// A value over the byte bound is dropped whole, never truncated — half a
// customer's order number on a screen, with no sign the other half was ever
// sent, is worse than a gap. A replacement that is dropped leaves the old
// value standing.
func (c *Call) MergeUserData(patch map[string]any) UserDataChange {
	if c.UserData == nil {
		c.UserData = map[string]any{}
	}
	plan := c.planUserDataMerge(patch)
	for _, k := range plan.Deleted {
		delete(c.UserData, k)
	}
	for _, k := range plan.Changed {
		c.UserData[k] = patch[k]
	}
	return plan
}

// planUserDataMerge works out what a patch would do without doing it.
//
// Split out so that a caller who must refuse rather than partly apply — a
// request with somebody waiting on the answer — can ask first and leave the
// call untouched. Deciding and doing in one pass would leave that caller with
// only two options, both wrong: apply and then report the loss, or unpick a
// map it has already changed.
func (c *Call) planUserDataMerge(patch map[string]any) UserDataChange {
	var plan UserDataChange
	room := UserDataMaxKeys - len(c.UserData)

	var additions []string
	for k, v := range patch {
		if v == nil {
			if _, ok := c.UserData[k]; ok {
				plan.Deleted = append(plan.Deleted, k)
				room++
			}
			continue
		}
		if size, ok := userDataValueSize(v); !ok || size > UserDataMaxValueBytes {
			plan.Dropped = append(plan.Dropped, k)
			continue
		}
		old, exists := c.UserData[k]
		if !exists {
			additions = append(additions, k)
			continue
		}
		// A replacement keeps its place in the map, so it needs no room; it
		// only needs to be saying something new.
		if !sameUserDataValue(old, v) {
			plan.Changed = append(plan.Changed, k)
		}
	}

	slices.Sort(additions)
	for _, k := range additions {
		if room <= 0 {
			plan.Dropped = append(plan.Dropped, k)
			continue
		}
		plan.Changed = append(plan.Changed, k)
		room--
	}

	slices.Sort(plan.Changed)
	slices.Sort(plan.Deleted)
	slices.Sort(plan.Dropped)
	return plan
}

// userDataValueSize measures a value the way the bound is written: in bytes,
// because a Chinese character is three where an English one is one, and the
// bound is about what is stored and shipped.
//
// Values are strings by contract, which is the only case that costs nothing to
// measure. Anything else is sized by what it would become on the wire and in
// the ledger's jsonb — and something that cannot be encoded at all cannot
// reach either, so it is refused rather than stored.
func userDataValueSize(v any) (int, bool) {
	if s, ok := v.(string); ok {
		return len(s), true
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	return len(encoded), true
}

// sameUserDataValue answers whether a replacement is really a replacement.
// Strings are the contract's values and compare directly; anything else is
// read as different, which errs towards announcing a change that did not
// happen rather than swallowing one that did.
func sameUserDataValue(old, new any) bool {
	oldStr, oldOK := old.(string)
	newStr, newOK := new.(string)
	return oldOK && newOK && oldStr == newStr
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
	// ExtensionNumber is the phone this leg is at, when it is one of ours; see
	// Party.ExtensionNumber for why it is not the same question as AgentID.
	ExtensionNumber string     `json:"extensionNumber,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	AnsweredAt      *time.Time `json:"answeredAt,omitempty"`
	ReleasedAt      *time.Time `json:"releasedAt,omitempty"`
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
			PartyID:         p.PartyID,
			ChannelID:       p.ChannelID,
			Role:            p.Role,
			State:           p.State,
			Number:          p.Number,
			OtherNumber:     p.OtherNumber,
			AgentID:         p.AgentID,
			ExtensionNumber: p.ExtensionNumber,
			CreatedAt:       p.CreatedAt,
			ReleaseCause:    p.ReleaseCause,
			IsBotLeg:        p.IsBotLeg,
			IsMuted:         p.IsMuted,
			Bridges:         slices.Clone(p.Bridges),
			BilledSec:       p.BilledSec,
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
