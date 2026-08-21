// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// AgentLookup is the agent service as call control needs it: who is at which
// phone, and the two facts about an agent that only a call can report — that
// they are on one, and that their part of one has ended.
type AgentLookup interface {
	AgentAtExtension(extensionNumber string) (uuid.UUID, bool)
	AgentByCallcenterName(name string) (uuid.UUID, bool)
	SetOnCall(ctx context.Context, agentID uuid.UUID, onCall bool)
	// BeginAfterCallWork starts an agent's wrap-up for the call whose agent
	// leg just ended. Fire and forget: a call is over whether or not presence
	// could be recorded, so this reports nothing back to the switch path.
	BeginAfterCallWork(ctx context.Context, agentID, callID uuid.UUID)
}

// Errors returned by call operations.
var (
	ErrNotCallParty = errors.New("not a party to this call")
	ErrNoAgentLeg   = errors.New("no agent leg on this call")
	ErrInvalidDTMF  = errors.New("not a DTMF sequence")
	// ErrNotForCallType reports an operation this kind of call does not offer.
	// One extension calling another is two people on a line, not a call being
	// handled: there is no third party to pass it to and no queue to put it
	// back into, so transferring, holding and retrieving it mean nothing
	// (owner's ruling, 2026-08-20).
	ErrNotForCallType = errors.New("not available on this kind of call")
)

// Coordinator turns switch events into calls and carries out call control.
//
// Calls are not created by this application: FreeSWITCH answers the phone and
// tells us afterwards. So a channel we have never seen is adopted rather than
// rejected, which is also what makes the application safe to restart while
// calls are up.
type Coordinator struct {
	registry  *Registry
	adapter   *Adapter
	agents    AgentLookup
	pub       Publisher
	cdr       *CDRAssembler
	taps      Tapper
	audiences Audiences
	// queues resolves the switch's queue names; nil leaves the waiting line
	// empty, which is honest — a queue we cannot name is one no screen can
	// render.
	queues QueueCatalog
	// waiting is who is queued right now, which no other part of the system
	// knows: the switch reports a count, and a call in a queue looks like any
	// other call from the registry's side.
	waiting *WaitingLine
}

// Audiences records who may see a call's live transcript. Nil leaves every
// transcript addressed to supervisors and administrators only, which is what
// the transcript actor decides when nobody has told it otherwise.
//
// It is a port for the same reason Tapper is: this package owns which agents
// are on a call and nothing else, and the transcript's ordering, storage and
// delivery are not its business.
type Audiences interface {
	SetAudience(callID uuid.UUID, agentIDs []uuid.UUID)
}

// Tapper starts and stops the media tap that feeds live transcription. Nil
// disables transcription without disabling anything else.
//
// It takes the agent's own leg, never the caller's. The leg's lifetime is
// exactly the human phase, so "never tap the bot, the queue or hold music"
// stops being a timing rule the code has to keep and becomes a property of the
// object; and the leg carries one agent, so every line has a known speaker
// rather than a "who is bridged right now" lookup that a second transfer would
// invalidate.
type Tapper interface {
	Attach(callID uuid.UUID, agentID, partyID *uuid.UUID, channelID, language string)
	Detach(channelID string)
	// DetachCall retires every tap a call has, driven from the call's own
	// termination rather than from a switch event, so transcription converges
	// whether or not CHANNEL_UNBRIDGE or CHANNEL_HANGUP ever arrives for the
	// tapped leg. It must be idempotent: it will usually run second.
	DetachCall(callID uuid.UUID)
	Pause(channelID string) error
	Resume(channelID string) error
}

// ErrNoTap reports that a channel has no live tap, so the command was not
// carried out. It lives here rather than in the implementation because it is
// part of the Tapper contract: the caller has to be able to tell "nothing was
// tapped here" — ordinary, since the caller's own leg never is — from a switch
// that refused the command, and a nil return for both makes a tap that died
// under us look exactly like one that was never there.
var ErrNoTap = errors.New("telephony: no live tap on this channel")

// AttachTaps points bridge and hold transitions at the transcription tap.
func (c *Coordinator) AttachTaps(t Tapper) { c.taps = t }

// AttachAudiences points party changes at the live transcript's addressing.
func (c *Coordinator) AttachAudiences(a Audiences) { c.audiences = a }

// announceAudience tells the transcript who is on this call.
//
// Every agent with a party on the call, which is a superset of whoever is
// bridged at this instant: a consulting agent and the agent they consulted
// were both on the conversation, and a transcript that flickered out of an
// agent's panel when a bridge moved would be worse than one that stays.
//
// Called at the bridge, which is the moment an agent's leg becomes part of
// this conversation. Not when that leg is created: a leg the switch dialled
// carries no call id of its own, so it is adopted onto a provisional call that
// has no transcript and is absorbed moments later. Addressing that one would
// look like coverage and reach nobody.
func (c *Coordinator) announceAudience(callID uuid.UUID) {
	if c.audiences == nil {
		return
	}
	var agentIDs []uuid.UUID
	if err := c.registry.Do(callID, func(call *Call) {
		agentIDs = call.AgentIDs()
	}); err != nil {
		// A call we can no longer read is not evidence that its audience
		// shrank, and clearing one on a failed read would take a live
		// transcript off an agent's screen.
		return
	}
	c.audiences.SetAudience(callID, agentIDs)
}

// reportTap classifies what the tap said about a command it did not carry out.
//
// Most holds are on a channel that was never tapped — the caller's own leg is
// not — so ErrNoTap is the ordinary case and says nothing. Anything else is
// the switch refusing a command against a stream we believe is live, which is
// the case that used to be swallowed whole.
func (c *Coordinator) reportTap(what, channelID string, err error) {
	switch {
	case err == nil, errors.Is(err, ErrNoTap):
		return
	default:
		slog.Warn("transcription tap refused a command",
			"command", what, "channelId", channelID, "error", err)
	}
}

// NewCoordinator builds a Coordinator.
// AttachCDR points call retirement and queue movements at the ledger.
func (c *Coordinator) AttachCDR(assembler *CDRAssembler) {
	c.cdr = assembler
	c.registry.OnCallFinished = assembler.CallFinished
}

func NewCoordinator(registry *Registry, adapter *Adapter, agents AgentLookup, pub Publisher) *Coordinator {
	return &Coordinator{registry: registry, adapter: adapter, agents: agents, pub: pub,
		waiting: NewWaitingLine()}
}

// Handle consumes one normalized switch event.
func (c *Coordinator) Handle(ctx context.Context, ev SwitchEvent) {
	// A harness leg is scaffolding around a scripted call — the loopback half
	// that only exists to push audio at the system under test. It is not a
	// party to any conversation and must never reach the ledger.
	if isHarnessLeg(ev) {
		return
	}

	switch ev.Kind {
	case KindChannelCreate:
		c.adopt(ctx, ev)
	case KindChannelBridge:
		c.join(ctx, ev)
	case KindQueueAgentOffered:
		c.offerToAgent(ctx, ev)
	}

	// What the media tap says about itself. It is not a state change to any
	// call — the tap's own health is the transcription path's business — but
	// it is the only account we get from the module, and until it was
	// subscribed to, a stream that never connected and one that errored were
	// indistinguishable from a working one.
	switch ev.Kind {
	case KindAudioStreamConnected:
		slog.InfoContext(ctx, "the media tap connected", "channelId", ev.ChannelID)
	case KindAudioStreamDisconnected:
		slog.InfoContext(ctx, "the media tap disconnected", "channelId", ev.ChannelID)
	case KindAudioStreamError:
		slog.ErrorContext(ctx, "the media tap reported an error",
			"channelId", ev.ChannelID, "error", ev.Cause)
	}

	// The tap follows the conversation rather than the channel. On hold the
	// agent's leg carries a private side-call and music, neither of which is
	// this conversation; when the bridge ends or the channel does, the tap
	// ends with it.
	if c.taps != nil && ev.ChannelID != "" {
		switch ev.Kind {
		case KindChannelHold:
			c.reportTap("pause", ev.ChannelID, c.taps.Pause(ev.ChannelID))
		case KindChannelUnhold:
			c.reportTap("resume", ev.ChannelID, c.taps.Resume(ev.ChannelID))
		case KindChannelUnbridge, KindChannelHangup:
			c.taps.Detach(ev.ChannelID)
		}
	}

	// The dialplan mints a call's identity after the channel already exists,
	// so the first event arrives too early to carry it. The moment a later
	// event does, the provisional call collapses into the minted one.
	c.reidentify(ctx, ev)

	// Who is waiting, for the agents staffing the queue. This runs before the
	// dispatch below so that a hangup still finds its entry: the last leg's
	// hangup retires the call, and with it the channel binding this reads.
	c.trackQueue(ctx, ev)

	// Queue movements feed the ledger: service level and abandonment reporting
	// read those rows, never the raw switch events.
	if c.cdr != nil {
		switch ev.Kind {
		case KindQueueMemberJoined, KindQueueAgentOffered,
			KindQueueBridgeStart, KindQueueMemberLeft:
			var callID *uuid.UUID
			if id, ok := c.registry.CallForChannel(ev.MemberChannelID); ok {
				callID = &id
			}
			var agentID *uuid.UUID
			if ev.AgentName != "" {
				if id, ok := c.agents.AgentByCallcenterName(ev.AgentName); ok {
					agentID = &id
				}
			}
			c.cdr.QueueEvent(ctx, ev, callID, agentID)
		}
	}

	// Who owned a leg has to be read before the call is told it ended: the
	// last hangup finishes the call and retires its actor, after which the
	// channel is no longer attributed to anyone and the agent would stay
	// marked on a call forever.
	var ended *endedLeg
	if ev.Kind == KindChannelHangup {
		ended = c.agentLegEnded(ev.ChannelID)
	}

	// Everything, including the events above, still drives the state machines
	// of whichever call owns the channel.
	c.registry.Dispatch(ev)

	if ended != nil {
		// Off the call first: being on one outranks wrap-up when availability
		// is derived, so the other order would show the agent as still
		// talking to somebody who has hung up.
		c.agents.SetOnCall(ctx, ended.agentID, false)
		// After-call work is for a conversation that happened. A leg that
		// rang and was never answered — a decline, a phone nobody picked up —
		// left the agent nothing to write up.
		if ended.wasAnswered {
			c.agents.BeginAfterCallWork(ctx, ended.agentID, ended.callID)
		}
	}
}

// endedLeg is what an agent's hangup means for that agent: which call it was,
// and whether they ever spoke on it.
type endedLeg struct {
	agentID     uuid.UUID
	callID      uuid.UUID
	wasAnswered bool
}

// isHarnessLeg recognizes scaffolding channels: scripted test calls originate
// through loopback with {aicc_harness=true}. Loopback copies the variable to
// both halves, so the name suffix picks out the -a half — the side that only
// exists to push audio; the -b half plays the caller and is tracked normally.
func isHarnessLeg(ev SwitchEvent) bool {
	return ev.Raw.Variable("aicc_harness") == "true" &&
		strings.HasSuffix(ev.ChannelName, "-a")
}

// isBotLeg recognizes the leg the switch dialed towards the AI gateway: an
// outbound channel whose destination is the DID the dialplan stamped on it.
// The caller's own leg carries the same variables but arrives inbound.
func isBotLeg(ev SwitchEvent) bool {
	did := ev.Raw.Variable("aicc_did")
	return ev.Direction == DirectionOutbound && did != "" && ev.DestinationNumber == did
}

// reidentify moves a channel from a provisional call to its minted identity
// once an event reveals it. The caller's CHANNEL_CREATE fires before the
// dialplan runs, so the caller is always adopted provisionally first; the
// minted id rides every event after the dialplan sets it.
func (c *Coordinator) reidentify(ctx context.Context, ev SwitchEvent) {
	if ev.ChannelID == "" {
		return
	}
	raw := ev.Raw.Variable("aicc_call_id")
	if raw == "" {
		return
	}
	minted, err := uuid.Parse(raw)
	if err != nil {
		return
	}
	bound, ok := c.registry.CallForChannel(ev.ChannelID)
	if !ok || bound == minted {
		return
	}
	if c.isMintedID(bound) {
		// The channel already lives on a minted call; a differing variable
		// here would mean the dialplan reminted mid-call, which it never does.
		return
	}

	// The minted call may not exist yet: this channel's event is the first
	// place the id appears. Create it so the provisional facts have a home.
	if _, err := c.registry.CreateCall(ctx, minted, callTypeOf(ev),
		ev.Raw.Variable("aicc_language"), true); err == nil {
		slog.DebugContext(ctx, "minted call created on reidentify", "callId", minted)
	}
	c.merge(ctx, minted, bound)
}

// adopt creates a call for a channel we have not seen before.
func (c *Coordinator) adopt(ctx context.Context, ev SwitchEvent) {
	if ev.ChannelID == "" || c.registry.Owns(ev.ChannelID) {
		return
	}

	// Our dialplan mints the call identity before the first leg exists, so a
	// caller arriving through it keeps one id across every transfer. A leg the
	// switch created on its own gets a fresh one.
	callID := uuid.Nil
	isMinted := false
	if raw := ev.Raw.Variable("aicc_call_id"); raw != "" {
		if parsed, err := uuid.Parse(raw); err == nil {
			callID = parsed
			isMinted = true
		}
	}

	// A leg dialed towards a signed-in agent belongs to that agent's call, not
	// to a new one: it is the delivery of a call already in a queue.
	agentID, agentExtension, isAgentLeg := c.agentForLeg(ev)
	if callID == uuid.Nil && ev.MemberChannelID != "" {
		if member, ok := c.registry.CallForChannel(ev.MemberChannelID); ok {
			// Joining the caller's call now, rather than waiting for the
			// bridge to merge two calls, is what makes the offer read as an
			// offer. A delivery leg on a call of its own is that call's first
			// party, so it comes out ORIGINATOR/DIALING, and the agent is
			// shown dialling the caller who is in fact ringing them. Worse,
			// a delivery mod_callcenter cancels before it answers never
			// bridges at all: the stray call ends unmerged and reaches the
			// ledger as an outbound CDR with caller and agent reversed, one
			// per retry.
			if err := c.registry.BindChannel(ev.ChannelID, member); err != nil {
				slog.WarnContext(ctx, "cannot bind delivery leg",
					"channelId", ev.ChannelID, "callId", member, "error", err)
				return
			}
			c.addParty(ctx, member, ev, agentID, agentExtension, isAgentLeg)
			return
		}
		// The caller's own leg is not on the books yet. The bridge will still
		// merge the two, which is the behaviour this replaced.
		slog.DebugContext(ctx, "delivery leg outran its caller",
			"channelId", ev.ChannelID, "memberChannelId", ev.MemberChannelID)
	}

	if callID == uuid.Nil {
		callID = uuid.Must(uuid.NewV7())
	}

	call, err := c.registry.CreateCall(ctx, callID, callTypeOf(ev), ev.Raw.Variable("aicc_language"), isMinted)
	if err != nil {
		// Another leg of the same call adopted it first, which is the normal
		// race between two channels of one conversation.
		if err := c.registry.BindChannel(ev.ChannelID, callID); err != nil {
			slog.WarnContext(ctx, "cannot bind channel", "channelId", ev.ChannelID, "error", err)
		}
		c.addParty(ctx, callID, ev, agentID, agentExtension, isAgentLeg)
		return
	}

	_ = call
	if err := c.registry.BindChannel(ev.ChannelID, callID); err != nil {
		slog.WarnContext(ctx, "cannot bind channel", "channelId", ev.ChannelID, "error", err)
		return
	}
	c.addParty(ctx, callID, ev, agentID, agentExtension, isAgentLeg)
}

// addParty appends a leg to a call and announces it.
func (c *Coordinator) addParty(ctx context.Context, callID uuid.UUID, ev SwitchEvent, agentID uuid.UUID, agentExtension string, isAgentLeg bool) {
	var (
		partyID  uuid.UUID
		callType events.CallType
		userData map[string]any
	)

	err := c.registry.Do(callID, func(call *Call) {
		if call.PartyByChannel(ev.ChannelID) != nil {
			return
		}
		p := call.AddParty(ev.ChannelID, legNumber(ev), ev.OccurredAt)
		if isAgentLeg {
			p.AgentID = &agentID
			p.OtherNumber = ev.ANI
		}
		p.IsBotLeg = isBotLeg(ev)
		partyID, callType, userData = p.PartyID, call.CallType, call.UserData
	})
	if err != nil || partyID == uuid.Nil {
		return
	}

	// A leg towards an agent is what puts a call on their screen, with enough
	// context to render it without asking for anything else.
	if isAgentLeg {
		c.publish(ctx, events.Event{
			Type:     events.TypePartyRinging,
			CallID:   &callID,
			CallType: callType,
			PartyID:  &partyID,
			AgentID:  &agentID,
			UserData: userData,
			Payload: map[string]any{
				"fromNumber": ev.ANI,
				// The extension the leg was attributed to, not the address the
				// switch dialled: a browser softphone's destination is the
				// random contact user it registered under, and telling an
				// agent they are being rung at "g7bih4lv" is telling them
				// nothing.
				"toNumber":        agentExtension,
				"extensionNumber": agentExtension,
			},
		}, events.Scope{AgentIDs: []uuid.UUID{agentID}})

		c.agents.SetOnCall(ctx, agentID, true)
	}
}

// join merges the two legs of a bridge into one call, which is how a queued
// caller and the agent the switch chose for them become one conversation.
func (c *Coordinator) join(ctx context.Context, ev SwitchEvent) {
	if ev.ChannelID == "" || ev.OtherChannelID == "" {
		return
	}
	callID, ok := c.registry.CallForChannel(ev.ChannelID)
	otherID, otherOK := c.registry.CallForChannel(ev.OtherChannelID)
	if !ok || !otherOK || callID == otherID {
		return
	}

	// Keep the identity everything else refers to. A minted id — chosen by
	// the dialplan before any leg existed — outranks a provisional one: the
	// bot's transcript and both halves of the CDR meet on it. An agent-only
	// call is always the one absorbed.
	keep, absorb := callID, otherID
	switch {
	case c.isAgentOnly(callID):
		keep, absorb = otherID, callID
	case c.isAgentOnly(otherID):
		// keep as is
	case c.isMintedID(otherID) && !c.isMintedID(callID):
		keep, absorb = otherID, callID
	}
	c.merge(ctx, keep, absorb)

	// The tap goes on now, at the bridge, on a leg that may be milliseconds
	// old — measured to survive, so there is no attach-on-answer-and-discard
	// fallback to maintain.
	c.tapAgentLeg(keep, ev.ChannelID, ev.OtherChannelID)
	// Parties moved between calls, so who is on this one has changed.
	c.announceAudience(keep)
}

// tapAgentLeg starts transcription on whichever of the bridged channels is an
// agent's.
//
// The leg is found through the call's parties, never by matching an extension
// number against the channel: an agent registered over WebRTC appears as a
// per-registration token bearing no resemblance to their extension, so digit
// matching works for a desk phone and fails silently for every browser agent —
// which is all of them in this design.
func (c *Coordinator) tapAgentLeg(callID uuid.UUID, channels ...string) {
	if c.taps == nil {
		return
	}
	_ = c.registry.Do(callID, func(call *Call) {
		for _, channelID := range channels {
			if channelID == "" {
				continue
			}
			for _, p := range call.Parties {
				if p.ChannelID != channelID || p.AgentID == nil {
					continue
				}
				agentID, partyID := *p.AgentID, p.PartyID
				c.taps.Attach(callID, &agentID, &partyID, channelID, call.Language)
			}
		}
	})
}

// merge folds one call into another: parties and facts move, channels rebind,
// and the absorbed call retires without ever reaching the ledger.
func (c *Coordinator) merge(ctx context.Context, keep, absorb uuid.UUID) {
	var moved []*Party
	var movedQueue QueueFacts
	var movedBot BotShare
	_ = c.registry.Do(absorb, func(call *Call) {
		moved = append(moved, call.Parties...)
		movedQueue = call.Queue
		movedBot = call.Bot
	})

	err := c.registry.Do(keep, func(call *Call) {
		for _, p := range moved {
			if call.PartyByChannel(p.ChannelID) != nil {
				continue
			}
			call.Parties = append(call.Parties, p)
		}
		// Facts recorded on the absorbed half move with it.
		if call.Queue.JoinedAt.IsZero() && !movedQueue.JoinedAt.IsZero() {
			call.Queue = movedQueue
		}
		call.Bot.Merge(movedBot)
		// One conversation has one originator: the earliest inbound leg.
		// Both provisional calls named their own first leg the originator,
		// and keeping two makes the CDR's from-number a coin toss.
		normalizeOriginator(call)
	})
	if err != nil {
		return
	}
	for _, p := range moved {
		if err := c.registry.BindChannel(p.ChannelID, keep); err != nil {
			slog.WarnContext(ctx, "cannot rebind channel", "channelId", p.ChannelID, "error", err)
		}
	}
	c.registry.Retire(absorb)
	c.announceMerge(ctx, keep, moved)
	slog.DebugContext(ctx, "calls merged", "callId", keep, "absorbed", absorb)
}

// announceMerge tells the agents whose legs just moved that their call has a
// different identity now.
//
// A merge is the one thing that changes a live call's id under a client that
// is already holding it. The absorbed call is retired without a word, and
// nothing is published for the kept call afterwards, so an agent's cockpit
// keeps the id it last read — which is the dead one. Observed live
// (2026-08-18): PARTY_ESTABLISHED carried the pre-merge id, the client
// refetched on it and raced the merge, and every CALL_TRANSCRIPT after that
// arrived under the kept id and was dropped by the panel as belonging to
// another call. The transcript was published perfectly and shown to nobody.
//
// PARTY_CHANGED because that is what happened: the party is the same, the call
// it belongs to is not. Scoped to the agent on the moved leg, who is the only
// one holding a stale id.
func (c *Coordinator) announceMerge(ctx context.Context, keep uuid.UUID, moved []*Party) {
	var callType events.CallType
	if err := c.registry.Do(keep, func(call *Call) { callType = call.CallType }); err != nil {
		return
	}
	for _, p := range moved {
		if p.AgentID == nil {
			continue
		}
		partyID, agentID := p.PartyID, *p.AgentID
		c.publish(ctx, events.Event{
			Type:     events.TypePartyChanged,
			CallID:   &keep,
			CallType: callType,
			PartyID:  &partyID,
			AgentID:  &agentID,
			Payload:  map[string]any{"reason": "CALL_MERGED"},
		}, events.Scope{AgentIDs: []uuid.UUID{agentID}})
	}
}

// offerToAgent announces a queued call on the chosen agent's screen, before
// their phone has even been dialled.
func (c *Coordinator) offerToAgent(ctx context.Context, ev SwitchEvent) {
	agentID, ok := c.agents.AgentByCallcenterName(ev.AgentName)
	if !ok {
		return
	}
	callID, known := c.registry.CallForChannel(ev.MemberChannelID)
	if !known {
		return
	}

	var (
		callType events.CallType
		userData map[string]any
		ani      string
	)
	_ = c.registry.Do(callID, func(call *Call) {
		callType, userData = call.CallType, call.UserData
		if o := call.Originator(); o != nil {
			ani = o.Number
		}
	})

	c.publish(ctx, events.Event{
		Type:     events.TypeQueueAgentOffered,
		CallID:   &callID,
		CallType: callType,
		AgentID:  &agentID,
		UserData: userData,
		Payload: map[string]any{
			"queue":      ev.Queue,
			"fromNumber": ani,
		},
	}, events.Scope{AgentIDs: []uuid.UUID{agentID}})
}

// agentLegEnded reports which agent, if any, owned a leg, and what became of
// it. Nil when the leg was nobody's.
func (c *Coordinator) agentLegEnded(channelID string) *endedLeg {
	callID, ok := c.registry.CallForChannel(channelID)
	if !ok {
		return nil
	}
	var ended *endedLeg
	_ = c.registry.Do(callID, func(call *Call) {
		p := call.PartyByChannel(channelID)
		if p == nil || p.AgentID == nil {
			return
		}
		ended = &endedLeg{
			agentID:     *p.AgentID,
			callID:      call.CallID,
			wasAnswered: !p.AnsweredAt.IsZero(),
		}
	})
	return ended
}

//
// Call control. Each operation acts on the caller's own leg or the agent's,
// whichever the request means, and never on a leg the caller has no part in.
//

// Answer picks up an agent's ringing leg through remote phone control.
func (c *Coordinator) Answer(ctx context.Context, callID, agentID uuid.UUID) error {
	channelID, err := c.agentChannel(callID, agentID)
	if err != nil {
		return err
	}
	return c.adapter.Answer(channelID)
}

// Hold and Retrieve drive the agent's own phone.
func (c *Coordinator) Hold(ctx context.Context, callID, agentID uuid.UUID) error {
	channelID, err := c.handledChannel(callID, agentID)
	if err != nil {
		return err
	}
	return c.adapter.Hold(channelID)
}

func (c *Coordinator) Retrieve(ctx context.Context, callID, agentID uuid.UUID) error {
	channelID, err := c.handledChannel(callID, agentID)
	if err != nil {
		return err
	}
	return c.adapter.Retrieve(channelID)
}

// Mute and Unmute silence the agent's own microphone at the switch.
//
// The flag is recorded only after the switch accepts the command, so a failed
// mute never leaves the cockpit claiming the agent is silent when they are
// not. PARTY_CHANGED then carries the new state to every screen watching.
func (c *Coordinator) Mute(ctx context.Context, callID, agentID uuid.UUID) error {
	return c.setMuted(ctx, callID, agentID, true)
}

func (c *Coordinator) Unmute(ctx context.Context, callID, agentID uuid.UUID) error {
	return c.setMuted(ctx, callID, agentID, false)
}

func (c *Coordinator) setMuted(ctx context.Context, callID, agentID uuid.UUID, muted bool) error {
	channelID, err := c.agentChannel(callID, agentID)
	if err != nil {
		return err
	}
	if muted {
		err = c.adapter.MuteLeg(channelID)
	} else {
		err = c.adapter.UnmuteLeg(channelID)
	}
	if err != nil {
		return err
	}

	var changed *Party
	if err := c.registry.Do(callID, func(call *Call) {
		if p := call.PartyByChannel(channelID); p != nil {
			p.IsMuted = muted
			changed = p
		}
	}); err != nil {
		return err
	}
	if changed != nil {
		partyID := changed.PartyID
		c.publish(ctx, events.Event{
			Type:    events.TypePartyChanged,
			CallID:  &callID,
			PartyID: &partyID,
			AgentID: &agentID,
			Payload: map[string]any{"isMuted": muted},
		}, events.Scope{AgentIDs: []uuid.UUID{agentID}})
	}
	return nil
}

// SendDTMF emits tones towards the far end of the conversation.
//
// The digits go to the other party's leg, not the agent's: the point is that
// whatever the caller is connected to — an IVR, a bank's menu — hears them.
// Sent at the agent's own leg they would only beep in the agent's ear.
func (c *Coordinator) SendDTMF(ctx context.Context, callID, agentID uuid.UUID, digits string) error {
	if !isDTMF(digits) {
		return fmt.Errorf("%w: %q", ErrInvalidDTMF, digits)
	}
	// Being on the call at all is the permission check; without it any agent
	// could push tones into any conversation.
	if _, err := c.agentChannel(callID, agentID); err != nil {
		return err
	}

	var farEnd string
	if err := c.registry.Do(callID, func(call *Call) {
		for _, p := range call.Parties {
			if p.IsActive() && (p.AgentID == nil || *p.AgentID != agentID) {
				farEnd = p.ChannelID
				return
			}
		}
	}); err != nil {
		return err
	}
	if farEnd == "" {
		return ErrNotCallParty
	}
	return c.adapter.SendDTMF(farEnd, digits)
}

// isDTMF reports whether every character is a tone the DTMF alphabet has.
func isDTMF(digits string) bool {
	if digits == "" || len(digits) > 32 {
		return false
	}
	for _, r := range digits {
		switch {
		case r >= '0' && r <= '9', r == '*', r == '#',
			r >= 'A' && r <= 'D', r >= 'a' && r <= 'd':
		default:
			return false
		}
	}
	return true
}

// Hangup ends the agent's leg, which ends the conversation for them.
func (c *Coordinator) Hangup(ctx context.Context, callID, agentID uuid.UUID) error {
	channelID, err := c.agentChannel(callID, agentID)
	if err != nil {
		return err
	}
	return c.adapter.Hangup(channelID, "NORMAL_CLEARING")
}

// Transfer sends the caller to another extension or queue and drops the agent
// out of the conversation.
func (c *Coordinator) Transfer(ctx context.Context, callID, agentID uuid.UUID, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("transfer destination is required")
	}
	// The caller is the leg that must survive the transfer.
	var callerChannel string
	var callType events.CallType
	err := c.registry.Do(callID, func(call *Call) {
		callType = call.CallType
		for _, p := range call.Parties {
			if p.IsActive() && p.AgentID == nil {
				callerChannel = p.ChannelID
				return
			}
		}
	})
	if err != nil {
		return err
	}
	if callType == events.CallTypeInternal {
		return ErrNotForCallType
	}
	if callerChannel == "" {
		return ErrNotCallParty
	}
	return c.adapter.TransferToExtension(callerChannel, destination, "default")
}

// CallsForAgent returns the live calls an agent is part of.
// The agent's leg has to still be on the call. Having once had one is not the
// same thing: after a transfer the agent's leg is released and the
// conversation belongs to somebody else, but the call stayed on the first
// agent's screen until the whole thing ended, showing them a call they had
// already passed on and controls for a leg the switch had hung up.
func (c *Coordinator) CallsForAgent(agentID uuid.UUID) []Snapshot {
	var out []Snapshot
	for _, snap := range c.registry.SnapshotAll() {
		for _, p := range snap.Parties {
			if p.AgentID != nil && *p.AgentID == agentID && p.IsActive() {
				out = append(out, snap)
				break
			}
		}
	}
	return out
}

// AllCalls returns every live call, for supervision.
func (c *Coordinator) AllCalls() []Snapshot { return c.registry.SnapshotAll() }

// agentChannel finds an agent's own live leg on a call.
// handledChannel is agentChannel for the operations that only make sense on a
// call somebody is handling. An internal call is refused before the switch is
// touched, so an agent is told no rather than shown a control that half works.
func (c *Coordinator) handledChannel(callID, agentID uuid.UUID) (string, error) {
	var callType events.CallType
	if err := c.registry.Do(callID, func(call *Call) { callType = call.CallType }); err != nil {
		return "", err
	}
	if callType == events.CallTypeInternal {
		return "", ErrNotForCallType
	}
	return c.agentChannel(callID, agentID)
}

func (c *Coordinator) agentChannel(callID, agentID uuid.UUID) (string, error) {
	var channelID string
	err := c.registry.Do(callID, func(call *Call) {
		for _, p := range call.Parties {
			if p.IsActive() && p.AgentID != nil && *p.AgentID == agentID {
				channelID = p.ChannelID
				return
			}
		}
	})
	if err != nil {
		return "", err
	}
	if channelID == "" {
		return "", ErrNoAgentLeg
	}
	return channelID, nil
}

// agentForLeg reports whether a new leg is being delivered to a signed-in
// agent, and to which one.
//
// The dialled number is not a reliable key. A browser softphone registers with
// a random contact user, so a leg dialled at user/1001 arrives with a
// destination like "hbp99nv8" and only the directory's own dialed_user still
// carries the extension.
// It also returns the extension it matched on. A browser softphone registers
// under a random contact user, so the switch's destination for a leg dialled
// at it is a token like "g7bih4lv" — which is what the agent's own screen was
// being told they were being rung at.
func (c *Coordinator) agentForLeg(ev SwitchEvent) (agentID uuid.UUID, extension string, ok bool) {
	if c.agents == nil {
		return uuid.Nil, "", false
	}
	for _, candidate := range []string{
		ev.Raw.Variable("dialed_user"),
		ev.Raw.Variable("aicc_extension"),
		ev.DestinationNumber,
	} {
		if candidate == "" {
			continue
		}
		if agentID, ok := c.agents.AgentAtExtension(candidate); ok {
			return agentID, candidate, true
		}
	}
	return uuid.Nil, "", false
}

// isMintedID reports whether a call's identity was minted by the dialplan.
func (c *Coordinator) isMintedID(callID uuid.UUID) bool {
	isMinted := false
	_ = c.registry.Do(callID, func(call *Call) { isMinted = call.IsMintedID })
	return isMinted
}

// normalizeOriginator leaves exactly one originator: the earliest leg that
// holds the role. Later claimants become targets.
func normalizeOriginator(call *Call) {
	var earliest *Party
	for _, p := range call.Parties {
		if p.Role != RoleOriginator {
			continue
		}
		if earliest == nil || p.CreatedAt.Before(earliest.CreatedAt) {
			earliest = p
		}
	}
	for _, p := range call.Parties {
		if p.Role == RoleOriginator && p != earliest {
			p.Role = RoleTarget
		}
	}
}

// isAgentOnly reports whether every leg of a call belongs to an agent, which
// marks it as the delivery half of a bridge rather than the caller's call.
func (c *Coordinator) isAgentOnly(callID uuid.UUID) bool {
	agentOnly := true
	_ = c.registry.Do(callID, func(call *Call) {
		for _, p := range call.Parties {
			if p.AgentID == nil {
				agentOnly = false
				return
			}
		}
	})
	return agentOnly
}

func (c *Coordinator) publish(ctx context.Context, ev events.Event, scope events.Scope) {
	if c.pub == nil {
		return
	}
	c.pub.Publish(ctx, ev, scope)
}

// callTypeOf classifies a call from the switch's own view of the channel.
func callTypeOf(ev SwitchEvent) events.CallType {
	// Whoever placed the call may already know what it is. The switch cannot
	// tell an extension from a carrier number here — both are simply legs it
	// created outbound — so a stamped type outranks the guess below.
	switch events.CallType(ev.CallTypeHint) {
	case events.CallTypeInbound, events.CallTypeOutbound, events.CallTypeInternal:
		return events.CallType(ev.CallTypeHint)
	}
	// A channel the switch created inbound came from outside; one it created
	// outbound is a call we or the dialplan placed.
	if ev.Direction == DirectionInbound {
		if ev.Context == "public" {
			return events.CallTypeInbound
		}
		return events.CallTypeInternal
	}
	return events.CallTypeOutbound
}

// legNumber is the address of this leg: the caller's number for an inbound
// originator, the dialled extension for a leg being delivered. The directory's
// dialed_user is preferred because a browser softphone's destination is a
// random contact user rather than an extension.
func legNumber(ev SwitchEvent) string {
	if ev.Direction == DirectionInbound {
		return ev.ANI
	}
	if dialed := ev.Raw.Variable("dialed_user"); dialed != "" {
		return dialed
	}
	if ev.DestinationNumber != "" {
		return ev.DestinationNumber
	}
	return ev.ANI
}
