// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"
)

const (
	// ringCapacity bounds the replay buffer used by Last-Event-ID resume.
	ringCapacity = 65536
	// subscriberBuffer is how far one slow client may lag before it is
	// disconnected. The publisher must never block on a subscriber.
	subscriberBuffer = 64
)

// Subscriber describes who is listening, so the hub can scope delivery from
// the authenticated identity rather than from client-supplied filters.
type Subscriber struct {
	UserID uuid.UUID
	// AgentID is set when the user is an agent (nil for pure supervisors).
	AgentID *uuid.UUID
	// IsSupervisor grants the unrestricted view (supervisor and admin roles).
	IsSupervisor bool
	// QueueIDs are the queues this agent staffs.
	QueueIDs []uuid.UUID
	// Types, when non-empty, narrows delivery further at client request.
	Types []Type
}

// Subscription is an active stream. C yields envelopes until Close.
type Subscription struct {
	C      <-chan Event
	ch     chan Event
	hub    *Hub
	who    Subscriber
	closed bool
	// Dropped reports that the subscriber fell too far behind and the
	// stream was truncated; the handler ends the response so the browser
	// reconnects and resyncs.
	Dropped bool
}

// Hub fans events out to subscribers and keeps a replay ring.
//
// Delivery is non-blocking: a subscriber whose buffer is full is dropped
// rather than allowed to stall publication.
type Hub struct {
	seq *Sequence

	mu   sync.RWMutex
	subs map[*Subscription]struct{}
	// ring holds each event with the scope it was published under. Replay has
	// to answer the same question delivery did — may this subscriber see it? —
	// and the scope is the only thing that answers it, so the ring cannot hold
	// the envelope alone.
	ring   []ringEntry
	ringAt int   // next write position
	oldest int64 // oldest seq still replayable, 0 when the ring is empty

	// OnDropped is called when a subscriber is disconnected for lagging.
	OnDropped func(Subscriber)
}

// NewHub builds a Hub that stamps events with seq.
func NewHub(seq *Sequence) *Hub {
	return &Hub{
		seq:  seq,
		subs: make(map[*Subscription]struct{}),
		ring: make([]ringEntry, 0, ringCapacity),
	}
}

// Publish stamps ev with the next sequence number and fans it out.
// It never fails and never blocks.
func (h *Hub) Publish(ctx context.Context, ev Event, scope Scope) Event {
	id, _ := h.seq.Next(ctx)
	ev.Version = Version
	ev.Seq = id
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = nowFunc()
	}

	h.mu.Lock()
	h.appendRing(ev, scope)
	var dropped []*Subscription
	for sub := range h.subs {
		if !sub.who.wants(ev, scope) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			sub.Dropped = true
			dropped = append(dropped, sub)
		}
	}
	for _, sub := range dropped {
		h.removeLocked(sub)
	}
	h.mu.Unlock()

	if h.OnDropped != nil {
		for _, sub := range dropped {
			h.OnDropped(sub.who)
		}
	}
	return ev
}

// Subscribe registers who and replays everything after lastEventID.
//
// Registration and replay happen under one lock, so no event can slip between
// the replayed history and the live tail. resetNeeded reports that the
// requested resume point has already aged out of the ring: the caller must
// emit SYSTEM_RESET and the client must resync from snapshots.
func (h *Hub) Subscribe(who Subscriber, lastEventID int64) (sub *Subscription, replay []Event, resetNeeded bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub = &Subscription{ch: make(chan Event, subscriberBuffer), hub: h, who: who}
	sub.C = sub.ch
	h.subs[sub] = struct{}{}

	if lastEventID <= 0 {
		return sub, nil, false
	}
	if h.oldest == 0 || lastEventID+1 < h.oldest {
		return sub, nil, true
	}
	for _, entry := range h.snapshotLocked() {
		// The scope is the one the event was published under, not an empty
		// one: a resuming subscriber must be told exactly what a connected
		// subscriber would have been told, or reconnecting becomes a way to
		// read other people's calls.
		if entry.ev.Seq > lastEventID && who.wants(entry.ev, entry.scope) {
			replay = append(replay, entry.ev)
		}
	}
	return sub, replay, false
}

// Close removes the subscription; it is safe to call more than once.
func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.removeLocked(s)
}

// OldestSeq reports the oldest replayable sequence number, 0 when empty.
func (h *Hub) OldestSeq() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.oldest
}

// SubscriberCount reports how many streams are attached.
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

func (h *Hub) removeLocked(sub *Subscription) {
	if sub.closed {
		return
	}
	sub.closed = true
	delete(h.subs, sub)
	close(sub.ch)
}

// ringEntry is a published event and the scope that decided who received it.
type ringEntry struct {
	ev    Event
	scope Scope
}

func (h *Hub) appendRing(ev Event, scope Scope) {
	entry := ringEntry{ev: ev, scope: scope}
	if len(h.ring) < ringCapacity {
		h.ring = append(h.ring, entry)
	} else {
		h.ring[h.ringAt] = entry
		h.ringAt = (h.ringAt + 1) % ringCapacity
	}
	if len(h.ring) < ringCapacity {
		h.oldest = h.ring[0].ev.Seq
	} else {
		h.oldest = h.ring[h.ringAt].ev.Seq
	}
}

// snapshotLocked returns ring contents in sequence order.
func (h *Hub) snapshotLocked() []ringEntry {
	if len(h.ring) < ringCapacity {
		return h.ring
	}
	out := make([]ringEntry, 0, ringCapacity)
	out = append(out, h.ring[h.ringAt:]...)
	out = append(out, h.ring[:h.ringAt]...)
	return out
}

// wants reports whether this subscriber should receive ev.
func (s Subscriber) wants(ev Event, scope Scope) bool {
	if len(s.Types) > 0 && !containsType(s.Types, ev.Type) {
		return false
	}
	if s.IsSupervisor {
		return true
	}
	if scope.SupervisorOnly {
		return false
	}
	if len(scope.AgentIDs) > 0 {
		if s.AgentID == nil || !containsID(scope.AgentIDs, *s.AgentID) {
			// Not addressed to this agent; a queue match may still apply.
			if scope.QueueID == nil || !containsID(s.QueueIDs, *scope.QueueID) {
				return false
			}
		}
		return true
	}
	if scope.QueueID != nil {
		return containsID(s.QueueIDs, *scope.QueueID)
	}
	// Unscoped events (system notices) reach everyone.
	return true
}

func containsType(list []Type, v Type) bool { return slices.Contains(list, v) }

func containsID(list []uuid.UUID, v uuid.UUID) bool { return slices.Contains(list, v) }
