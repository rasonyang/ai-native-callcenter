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
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
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

	// OnCallRetired fires exactly once per call actor, from the actor's own
	// goroutine as it exits, whatever ended it.
	//
	// It is not OnCallFinished. That one fires when the last party releases,
	// which is the *expected* ending and not the only one: a call absorbed
	// into another is retired mid-life and never finishes, a shutdown stops
	// every actor where it stands, and a hangup this application never
	// received leaves the call to end some other way. Anything holding a
	// switch-side resource on a call's behalf has to be released on the path
	// that always runs, not the one that usually does.
	OnCallRetired func(callID uuid.UUID)

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
	obs.CallStarted(obs.CallKindSwitch)

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
	return r.do(callID, func(a *actor) { fn(a.call) })
}

// do is Do for the callers that need to announce what they changed as well as
// change it. Publishing is the actor's, not the call's — it is where the
// audience of an event is decided — so a mutation that has to be announced
// runs here and mutates through a.call.
func (r *Registry) do(callID uuid.UUID, fn func(*actor)) error {
	r.mu.RLock()
	a, ok := r.byCall[callID]
	r.mu.RUnlock()
	if !ok {
		return ErrCallNotFound
	}
	if !a.postSync(fn) {
		return ErrCallNotFound
	}
	return nil
}

// MergeUserData applies a patch to a call's business data and announces what
// moved, in one visit to the actor.
//
// The two are one operation on purpose. A merge that is not announced leaves
// every screen on the call holding data the call no longer has, and the only
// way to be sure that never happens is for a caller to have no way of doing
// the first without the second.
func (r *Registry) MergeUserData(callID uuid.UUID, patch map[string]any) (UserDataChange, error) {
	var change UserDataChange
	err := r.do(callID, func(a *actor) { change = a.mergeUserData(patch) })
	return change, err
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

// remove unregisters a finished call. This is the one exit every call actor
// takes, so it is where per-call resources outside this package are released.
func (r *Registry) remove(a *actor) {
	obs.CallEnded(obs.CallKindSwitch)

	r.mu.Lock()
	delete(r.byCall, a.call.CallID)
	for channelID, owner := range r.byChannel {
		if owner == a {
			delete(r.byChannel, channelID)
		}
	}
	retired := r.OnCallRetired
	r.mu.Unlock()

	// Outside the lock: the handler reaches the switch, and holding the
	// registry across an ESL round trip would stall every other call.
	if retired != nil {
		retired(a.call.CallID)
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
		// A leg answering is a billing fact, not a conversation. An
		// auto-answer phone picks up in front of nobody, a leg whose codec
		// cannot meet the caller's returns a clean 200 with no media at all,
		// and a caller the switch answers to play music is talking to a queue.
		// The ledger has always kept the two apart — bill_sec asks when this
		// leg answered, talk_sec asks when it was bridged — and the party's
		// own state now says the same thing.
		//
		// So record the answer and announce nothing: PARTY_ESTABLISHED waits
		// for the bridge. Owner's rule (2026-08-24), from reading a live
		// stream of a click-to-dial nobody picked up: the agent's own leg
		// auto-answers a second after the click, and the cockpit was told
		// TALKING while the colleague's phone rang for thirty seconds.
		if party.AnsweredAt.IsZero() {
			party.AnsweredAt = ev.OccurredAt
		}
	case KindChannelHold:
		a.transition(party, TriggerHold, ev, events.TypePartyHeld)
	case KindChannelUnhold:
		a.transition(party, TriggerRetrieve, ev, events.TypePartyRetrieved)
	case KindChannelHangup:
		party.ReleaseCause = ev.HangupCause
		party.TransferredAway = ev.TransferredAway
		// Whatever the leg was still bridged to, it is not any more.
		party.CloseBridge(ev.OccurredAt)
		party.BilledSec = ev.BilledSec
		// The AI leg's share arrives as channel variables on hangup, and the
		// legs carry different parts of it: each fills in what is still
		// missing rather than claiming the whole share for whichever hung up
		// first.
		a.call.Bot.Merge(ev.Bot)
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
		// It is, though, the only moment that says a conversation actually
		// started — answering does not, since a phone can answer with nobody
		// in front of it and a leg whose codec cannot meet the caller's
		// answers with no media at all.
		if other := a.call.PartyByChannel(ev.OtherChannelID); other != nil {
			party.OtherNumber = other.Number
			other.OtherNumber = party.Number
			other.OpenBridge(party.ChannelID, ev.OccurredAt)
			a.establish(other, ev)
		}
		party.OpenBridge(ev.OtherChannelID, ev.OccurredAt)
		a.establish(party, ev)

	case KindChannelUnbridge:
		// A leg on hold has not left the conversation — the caller hears music
		// instead of a person, and the agent is still on the call. Owner's
		// ruling (2026-08-21): hold counts as talk. Saying so here rather than
		// relying on the switch not to unbridge on hold, which is not
		// established either way.
		if party.State != PartyHeld {
			party.CloseBridge(ev.OccurredAt)
		}
		if other := a.call.PartyByChannel(ev.OtherChannelID); other != nil && other.State != PartyHeld {
			other.CloseBridge(ev.OccurredAt)
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

	// Whether this conversation is being recorded is a fact about the call and
	// not about a leg of it, so it goes to everyone on the call — which is
	// what publishing with no party does.
	//
	// The switch names the file it is writing and that name is not sent on.
	// It is a path on the switch's own disk, meaningful to nobody holding a
	// browser, and the recording is fetched through the recordings API by call
	// id when there is one to fetch. What a screen needs from this event is
	// that it happened, and when — both of which the envelope already carries.
	case KindRecordStart:
		a.publish(events.TypeCallRecordingStarted, nil, nil)

	case KindRecordStop:
		a.publish(events.TypeCallRecordingStopped, nil, nil)
	}
}

// transition applies a party trigger and publishes the matching event.
// establish moves a leg into the conversation, which is what a bridge means
// and what answering does not.
//
// Only a leg still waiting to be connected has anywhere to go. A leg already
// TALKING is being re-bridged — a transfer, a re-invite — and has nothing new
// to announce; a HELD leg comes back through RETRIEVE, not through here, and
// hold deliberately leaves the bridge open so that a caller on music still
// counts as being on the call.
func (a *actor) establish(p *Party, ev SwitchEvent) {
	if p.State != PartyDialing && p.State != PartyRinging {
		return
	}
	a.transition(p, TriggerAnswer, ev, events.TypePartyEstablished)
}

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
// patchUserData is mergeUserData for a caller who can be answered.
//
// All of the patch lands or none of it does. A request has somebody waiting on
// the reply, and half-applied business data is the state this product refuses
// everywhere else: a screen showing part of a customer's details, and a caller
// who was told it worked. So the plan is read first, and a patch that would
// not fit whole leaves the call exactly as it was — nothing written, nothing
// announced — naming the keys that were the problem.
//
// The plan is then computed a second time inside the merge. Thirty-two keys
// twice is not worth a way of applying a plan that could be stale by the time
// it is applied.
func (a *actor) patchUserData(patch map[string]any) (UserDataChange, error) {
	if plan := a.call.planUserDataMerge(patch); len(plan.Dropped) > 0 {
		return plan, ErrUserDataWouldNotFit
	}
	return a.mergeUserData(patch), nil
}

// mergeUserData applies the patch and tells the call about it.
//
// Silent when nothing moved. A merge that sets a key to the value it already
// holds is not news, and the case that makes this matter is the ordinary one:
// two calls becoming one merges the absorbed half's business data into the
// kept half, and on a consultation transfer that is very often the identical
// data. Announced anyway, every consultation would report a change nobody
// made, on the event a screen uses to decide something is worth showing.
//
// Call-scoped — published with no party — because business data belongs to the
// conversation rather than to a leg of it. On a call nobody has answered yet
// that reaches supervisors and administrators only, which is the whole
// audience there is at that moment.
func (a *actor) mergeUserData(patch map[string]any) UserDataChange {
	change := a.call.MergeUserData(patch)
	if change.IsEmpty() {
		return change
	}
	a.publish(events.TypeCallUserData, nil, map[string]any{
		"changedKeys": change.Changed,
		"deletedKeys": change.Deleted,
	})
	return change
}

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
	// A leg event goes to the agent whose leg it is. An agent works one leg,
	// and the states it moves through — ringing, established, released — are
	// the ones their own screen is about; a colleague's leg on the same call
	// is not their business. Two agents talking to each other therefore each
	// see three events about themselves rather than six about both.
	//
	// A party with no agent is addressed to no agent. That is not a gap to
	// widen: a customer hanging up ends the agent's leg too, so the agent is
	// told by their own PARTY_RELEASED — the one their screen is about. If an
	// agent's bar ever stays up after a customer leaves, the missing event is
	// the agent's own, and broadcasting somebody else's leg would hide that
	// rather than fix it (owner directive 2026-08-22, replacing a scope that
	// had been widened after the 2026-08-18 incident).
	//
	// A caller who abandons a queue before reaching anybody therefore reaches
	// no agent's stream at all, which is correct: there is nobody whose screen
	// it is about. It is in the CDR, and supervisors see everything.
	//
	// Call-scoped events (p == nil) keep the whole conversation: they are about
	// the call, not about a leg of it.
	scope := events.Scope{AgentIDs: a.call.AgentIDs()}
	if p != nil {
		partyID := p.PartyID
		ev.PartyID = &partyID
		ev.AgentID = p.AgentID
		scope.AgentIDs = nil
		if p.AgentID != nil {
			scope.AgentIDs = []uuid.UUID{*p.AgentID}
		}
	}
	if a.call.QueueID != nil {
		scope.QueueID = a.call.QueueID
	}
	a.registry.pub.Publish(context.Background(), ev, scope)
}
