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

// AgentLookup resolves which agent, if any, is signed in at an extension.
type AgentLookup interface {
	AgentAtExtension(extensionNumber string) (uuid.UUID, bool)
	AgentByCallcenterName(name string) (uuid.UUID, bool)
	SetOnCall(ctx context.Context, agentID uuid.UUID, onCall bool)
}

// Errors returned by call operations.
var (
	ErrNotCallParty = errors.New("not a party to this call")
	ErrNoAgentLeg   = errors.New("no agent leg on this call")
)

// Coordinator turns switch events into calls and carries out call control.
//
// Calls are not created by this application: FreeSWITCH answers the phone and
// tells us afterwards. So a channel we have never seen is adopted rather than
// rejected, which is also what makes the application safe to restart while
// calls are up.
type Coordinator struct {
	registry *Registry
	adapter  *Adapter
	agents   AgentLookup
	pub      Publisher
}

// NewCoordinator builds a Coordinator.
func NewCoordinator(registry *Registry, adapter *Adapter, agents AgentLookup, pub Publisher) *Coordinator {
	return &Coordinator{registry: registry, adapter: adapter, agents: agents, pub: pub}
}

// Handle consumes one normalized switch event.
func (c *Coordinator) Handle(ctx context.Context, ev SwitchEvent) {
	switch ev.Kind {
	case KindChannelCreate:
		c.adopt(ctx, ev)
	case KindChannelBridge:
		c.join(ctx, ev)
	case KindQueueAgentOffered:
		c.offerToAgent(ctx, ev)
	}

	// Who owned a leg has to be read before the call is told it ended: the
	// last hangup finishes the call and retires its actor, after which the
	// channel is no longer attributed to anyone and the agent would stay
	// marked on a call forever.
	var freedAgent *uuid.UUID
	if ev.Kind == KindChannelHangup {
		freedAgent = c.agentOnChannel(ev.ChannelID)
	}

	// Everything, including the events above, still drives the state machines
	// of whichever call owns the channel.
	c.registry.Dispatch(ev)

	if freedAgent != nil {
		c.agents.SetOnCall(ctx, *freedAgent, false)
	}
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
	if raw := ev.Raw.Variable("aicc_call_id"); raw != "" {
		if parsed, err := uuid.Parse(raw); err == nil {
			callID = parsed
		}
	}

	// A leg dialed towards a signed-in agent belongs to that agent's call, not
	// to a new one: it is the delivery of a call already in a queue.
	agentID, isAgentLeg := c.agentForLeg(ev)
	if isAgentLeg && callID == uuid.Nil {
		// The bridge event will join this leg to the caller's call; until then
		// it is tracked on its own so its ringing state is not lost.
		slog.DebugContext(ctx, "agent leg created", "channelId", ev.ChannelID, "agentId", agentID)
	}

	if callID == uuid.Nil {
		callID = uuid.Must(uuid.NewV7())
	}

	call, err := c.registry.CreateCall(ctx, callID, callTypeOf(ev), ev.Raw.Variable("aicc_language"))
	if err != nil {
		// Another leg of the same call adopted it first, which is the normal
		// race between two channels of one conversation.
		if err := c.registry.BindChannel(ev.ChannelID, callID); err != nil {
			slog.WarnContext(ctx, "cannot bind channel", "channelId", ev.ChannelID, "error", err)
		}
		c.addParty(ctx, callID, ev, agentID, isAgentLeg)
		return
	}

	_ = call
	if err := c.registry.BindChannel(ev.ChannelID, callID); err != nil {
		slog.WarnContext(ctx, "cannot bind channel", "channelId", ev.ChannelID, "error", err)
		return
	}
	c.addParty(ctx, callID, ev, agentID, isAgentLeg)
}

// addParty appends a leg to a call and announces it.
func (c *Coordinator) addParty(ctx context.Context, callID uuid.UUID, ev SwitchEvent, agentID uuid.UUID, isAgentLeg bool) {
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
				"fromNumber":      ev.ANI,
				"toNumber":        ev.DestinationNumber,
				"extensionNumber": ev.DestinationNumber,
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

	// Keep the caller's call: it carries the identity minted at answer time
	// and any business context collected since.
	keep, absorb := callID, otherID
	if c.isAgentOnly(callID) {
		keep, absorb = otherID, callID
	}

	var moved []*Party
	_ = c.registry.Do(absorb, func(call *Call) {
		moved = append(moved, call.Parties...)
	})

	err := c.registry.Do(keep, func(call *Call) {
		for _, p := range moved {
			if call.PartyByChannel(p.ChannelID) != nil {
				continue
			}
			call.Parties = append(call.Parties, p)
		}
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
	slog.DebugContext(ctx, "bridged legs joined", "callId", keep, "absorbed", absorb)
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

// agentOnChannel reports which agent, if any, owns a leg.
func (c *Coordinator) agentOnChannel(channelID string) *uuid.UUID {
	callID, ok := c.registry.CallForChannel(channelID)
	if !ok {
		return nil
	}
	var agentID *uuid.UUID
	_ = c.registry.Do(callID, func(call *Call) {
		if p := call.PartyByChannel(channelID); p != nil {
			agentID = p.AgentID
		}
	})
	return agentID
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
	channelID, err := c.agentChannel(callID, agentID)
	if err != nil {
		return err
	}
	return c.adapter.Hold(channelID)
}

func (c *Coordinator) Retrieve(ctx context.Context, callID, agentID uuid.UUID) error {
	channelID, err := c.agentChannel(callID, agentID)
	if err != nil {
		return err
	}
	return c.adapter.Retrieve(channelID)
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
	err := c.registry.Do(callID, func(call *Call) {
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
	if callerChannel == "" {
		return ErrNotCallParty
	}
	return c.adapter.TransferToExtension(callerChannel, destination, "default")
}

// CallsForAgent returns the live calls an agent is part of.
func (c *Coordinator) CallsForAgent(agentID uuid.UUID) []Snapshot {
	var out []Snapshot
	for _, snap := range c.registry.SnapshotAll() {
		for _, p := range snap.Parties {
			if p.AgentID != nil && *p.AgentID == agentID {
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
func (c *Coordinator) agentForLeg(ev SwitchEvent) (uuid.UUID, bool) {
	if c.agents == nil {
		return uuid.Nil, false
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
			return agentID, true
		}
	}
	return uuid.Nil, false
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
