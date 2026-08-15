// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// mailboxSize bounds one call's pending work. A call that falls this far
// behind is pathological; the bound keeps one bad call from consuming memory.
const mailboxSize = 256

// Errors returned by the registry.
var (
	ErrCallNotFound = errors.New("call not found")
	ErrCallBusy     = errors.New("call actor mailbox full")
)

// Publisher receives domain events. events.Hub satisfies it.
type Publisher interface {
	Publish(ctx context.Context, ev events.Event, scope events.Scope) events.Event
}

// Registry owns every live call.
//
// Each call is served by one actor goroutine that is the sole mutator of its
// state, so no call state is ever guarded by a lock. The registry itself only
// maps identifiers to actors; reads of call state go through the actor's
// mailbox, which is why snapshots can never be torn.
type Registry struct {
	pub Publisher

	// OnCallFinished fires once per call, from the call's own goroutine, with
	// the final snapshot. Set it before the first call arrives; it must not
	// block.
	OnCallFinished func(Snapshot)

	mu        sync.RWMutex
	byCall    map[uuid.UUID]*actor
	byChannel map[string]*actor

	wg sync.WaitGroup
}

// NewRegistry builds a Registry publishing to pub.
func NewRegistry(pub Publisher) *Registry {
	return &Registry{
		pub:       pub,
		byCall:    make(map[uuid.UUID]*actor),
		byChannel: make(map[string]*actor),
	}
}

// command is a unit of work for a call actor.
type command struct {
	run  func(*actor)
	done chan struct{}
}

// actor serializes all access to one call.
type actor struct {
	registry *Registry
	call     *Call
	mailbox  chan command
	quit     chan struct{}
	once     sync.Once
}

// CreateCall registers a new call and starts its actor. The caller mints the
// call id so an origination request can be retried without redialling.
//
// isMinted records the identity's provenance: minted means the dialplan chose
// this id before any leg existed, and a merge keeps it.
func (r *Registry) CreateCall(ctx context.Context, callID uuid.UUID, callType events.CallType, language string, isMinted bool) (*Call, error) {
	r.mu.Lock()
	if _, exists := r.byCall[callID]; exists {
		r.mu.Unlock()
		return nil, errors.New("call already exists")
	}

	call := NewCall(callID, callType, time.Now().UTC())
	call.Language = language
	call.IsMintedID = isMinted
	a := &actor{
		registry: r,
		call:     call,
		mailbox:  make(chan command, mailboxSize),
		quit:     make(chan struct{}),
	}
	r.byCall[callID] = a
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		a.loop()
	}()
	return call, nil
}

// BindChannel associates a FreeSWITCH channel with a call, so later events on
// that channel reach the right actor. Binding happens before the channel is
// created wherever we originate it, so no event can arrive unrouted.
func (r *Registry) BindChannel(channelID string, callID uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byCall[callID]
	if !ok {
		return ErrCallNotFound
	}
	r.byChannel[channelID] = a
	return nil
}

// Dispatch routes a normalized switch event to the owning call actor. Events
// for unknown channels are reported so the caller can decide whether to adopt
// them (an inbound call we have not seen yet) or ignore them.
func (r *Registry) Dispatch(ev SwitchEvent) bool {
	if ev.ChannelID == "" {
		return false
	}
	r.mu.RLock()
	a, ok := r.byChannel[ev.ChannelID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return a.post(func(a *actor) { a.applySwitchEvent(ev) })
}

// Snapshot returns a consistent view of one call, read through its mailbox.
func (r *Registry) Snapshot(callID uuid.UUID) (Snapshot, error) {
	r.mu.RLock()
	a, ok := r.byCall[callID]
	r.mu.RUnlock()
	if !ok {
		return Snapshot{}, ErrCallNotFound
	}

	var snap Snapshot
	if !a.postSync(func(a *actor) { snap = a.call.Snapshot() }) {
		return Snapshot{}, ErrCallNotFound
	}
	return snap, nil
}

// SnapshotAll returns every live call.
func (r *Registry) SnapshotAll() []Snapshot {
	r.mu.RLock()
	actors := make([]*actor, 0, len(r.byCall))
	for _, a := range r.byCall {
		actors = append(actors, a)
	}
	r.mu.RUnlock()

	out := make([]Snapshot, 0, len(actors))
	for _, a := range actors {
		var snap Snapshot
		if a.postSync(func(a *actor) { snap = a.call.Snapshot() }) {
			out = append(out, snap)
		}
	}
	return out
}

// Do runs fn against a call inside its actor, the only safe way to mutate it
// from outside.
func (r *Registry) Do(callID uuid.UUID, fn func(*Call)) error {
	r.mu.RLock()
	a, ok := r.byCall[callID]
	r.mu.RUnlock()
	if !ok {
		return ErrCallNotFound
	}
	if !a.postSync(func(a *actor) { fn(a.call) }) {
		return ErrCallNotFound
	}
	return nil
}

// Owns reports whether a channel is already bound to a call.
func (r *Registry) Owns(channelID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byChannel[channelID]
	return ok
}

// CallForChannel returns the call a channel belongs to.
func (r *Registry) CallForChannel(channelID string) (uuid.UUID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byChannel[channelID]
	if !ok {
		return uuid.Nil, false
	}
	return a.call.CallID, true
}

// Retire stops a call's actor without waiting for its legs to end, used when
// two calls turn out to be one conversation and the duplicate is absorbed.
func (r *Registry) Retire(callID uuid.UUID) {
	r.mu.RLock()
	a, ok := r.byCall[callID]
	r.mu.RUnlock()
	if ok {
		a.stop()
	}
}

// Count reports how many calls are live.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byCall)
}

// Shutdown stops every actor and waits for them.
func (r *Registry) Shutdown() {
	r.mu.Lock()
	actors := make([]*actor, 0, len(r.byCall))
	for _, a := range r.byCall {
		actors = append(actors, a)
	}
	r.mu.Unlock()

	for _, a := range actors {
		a.stop()
	}
	r.wg.Wait()
}

// remove unregisters a finished call.
func (r *Registry) remove(a *actor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byCall, a.call.CallID)
	for channelID, owner := range r.byChannel {
		if owner == a {
			delete(r.byChannel, channelID)
		}
	}
}

// post enqueues work without blocking the caller.
func (a *actor) post(run func(*actor)) bool {
	select {
	case <-a.quit:
		return false
	default:
	}
	select {
	case a.mailbox <- command{run: run}:
		return true
	default:
		slog.Error("call actor mailbox full, dropping work", "callId", a.call.CallID)
		return false
	}
}

// postSync enqueues work and waits for it, which is how state is read.
func (a *actor) postSync(run func(*actor)) bool {
	done := make(chan struct{})
	select {
	case <-a.quit:
		return false
	case a.mailbox <- command{run: run, done: done}:
	}
	select {
	case <-done:
		return true
	case <-a.quit:
		return false
	}
}

func (a *actor) stop() { a.once.Do(func() { close(a.quit) }) }

func (a *actor) loop() {
	defer a.registry.remove(a)
	for {
		select {
		case <-a.quit:
			return
		case cmd := <-a.mailbox:
			cmd.run(a)
			if cmd.done != nil {
				close(cmd.done)
			}
		}
	}
}

// applySwitchEvent moves the call in response to one switch event. It runs
// inside the actor, so it may touch call state freely.
func (a *actor) applySwitchEvent(ev SwitchEvent) {
	party := a.call.PartyByChannel(ev.ChannelID)
	if party == nil {
		return
	}

	switch ev.Kind {
	case KindChannelAnswer:
		a.transition(party, TriggerAnswer, ev, events.TypePartyEstablished)
	case KindChannelHold:
		a.transition(party, TriggerHold, ev, events.TypePartyHeld)
	case KindChannelUnhold:
		a.transition(party, TriggerRetrieve, ev, events.TypePartyRetrieved)
	case KindChannelHangup:
		party.ReleaseCause = ev.HangupCause
		party.TransferredAway = ev.TransferredAway
		// The AI leg's share arrives as channel variables on the caller's
		// hangup; any leg of the call may carry them, the first wins.
		if a.call.Bot.IsZero() && !ev.Bot.IsZero() {
			a.call.Bot = ev.Bot
		}
		a.transition(party, TriggerRelease, ev, events.TypePartyReleased)
		if a.call.Finish(ev.OccurredAt) {
			a.publish(events.TypeCallCDR, nil, map[string]any{
				"answeredAt": a.call.AnsweredAt(),
				"endedAt":    a.call.EndedAt,
			})
			if a.registry.OnCallFinished != nil {
				// The snapshot is taken inside the actor, so it is the final,
				// consistent view; the handler must not block this goroutine.
				a.registry.OnCallFinished(a.call.Snapshot())
			}
			a.stop()
		}
	case KindChannelBridge:
		// A bridge tells us who is talking to whom; it is not a state change.
		if other := a.call.PartyByChannel(ev.OtherChannelID); other != nil {
			party.OtherNumber = other.Number
			other.OtherNumber = party.Number
		}

	// The queue's own view of the caller, recorded for the CDR's timings.
	case KindQueueMemberJoined:
		a.call.Queue.Name = ev.Queue
		if !ev.JoinedAt.IsZero() {
			a.call.Queue.JoinedAt = ev.JoinedAt
		} else {
			a.call.Queue.JoinedAt = ev.OccurredAt
		}
	case KindQueueBridgeStart:
		a.call.Queue.BridgedAt = ev.OccurredAt
	case KindQueueMemberLeft:
		a.call.Queue.Cause = ev.Cause
		a.call.Queue.CancelReason = ev.CancelReason
		if !ev.LeftAt.IsZero() {
			a.call.Queue.LeftAt = ev.LeftAt
		} else {
			a.call.Queue.LeftAt = ev.OccurredAt
		}
	case KindDTMF:
		a.publish(events.TypePartyDTMF, party, map[string]any{
			"digit":      ev.Digit,
			"durationMs": ev.DurationMs,
		})
	}
}

// transition applies a party trigger and publishes the matching event.
func (a *actor) transition(p *Party, trigger PartyTrigger, ev SwitchEvent, eventType events.Type) {
	if err := p.apply(trigger, ev.OccurredAt); err != nil {
		// An out-of-order or duplicate switch event is a fact about the
		// world, not a crash: log it and keep the machine consistent.
		slog.Warn("rejected party transition",
			"callId", a.call.CallID, "partyId", p.PartyID,
			"state", p.State, "trigger", trigger, "error", err)
		return
	}
	payload := map[string]any{"role": string(p.Role), "state": string(p.State)}
	if trigger == TriggerRelease {
		payload["cause"] = p.ReleaseCause
		payload["isTransferredAway"] = p.TransferredAway
	}
	a.publish(eventType, p, payload)
}

// publish emits a domain event carrying enough context for a screen pop.
func (a *actor) publish(t events.Type, p *Party, payload map[string]any) {
	if a.registry.pub == nil {
		return
	}
	callID := a.call.CallID
	ev := events.Event{
		Type:     t,
		CallID:   &callID,
		CallType: a.call.CallType,
		QueueID:  a.call.QueueID,
		Payload:  payload,
		UserData: a.call.UserData,
	}
	scope := events.Scope{}
	if p != nil {
		partyID := p.PartyID
		ev.PartyID = &partyID
		ev.AgentID = p.AgentID
		if p.AgentID != nil {
			scope.AgentIDs = []uuid.UUID{*p.AgentID}
		}
	}
	if a.call.QueueID != nil {
		scope.QueueID = a.call.QueueID
	}
	a.registry.pub.Publish(context.Background(), ev, scope)
}
