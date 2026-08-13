// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fakeReserver struct {
	next int64
	err  error
	hits int
}

func (f *fakeReserver) ReserveSeqBlock(_ context.Context, _ string, size int64) (int64, error) {
	f.hits++
	if f.err != nil {
		return 0, f.err
	}
	f.next += size
	return f.next, nil
}

func newTestHub() (*Hub, *fakeReserver) {
	r := &fakeReserver{}
	return NewHub(NewSequence(r, "events")), r
}

func TestSequenceReservesBlocksAndIncreasesMonotonically(t *testing.T) {
	r := &fakeReserver{}
	s := NewSequence(r, "events")
	ctx := context.Background()

	var prev int64
	for i := range 5 {
		id, durable := s.Next(ctx)
		if !durable {
			t.Fatalf("call %d: durable = false, want true", i)
		}
		if id <= prev {
			t.Fatalf("call %d: id = %d, want > %d", i, id, prev)
		}
		prev = id
	}
	if r.hits != 1 {
		t.Errorf("reserver hits = %d, want 1 (one block covers many ids)", r.hits)
	}
}

func TestSequenceDegradesWhenStorageFails(t *testing.T) {
	r := &fakeReserver{err: errors.New("database down")}
	s := NewSequence(r, "events")

	id, durable := s.Next(context.Background())
	if durable {
		t.Error("durable = true, want false when the reserver fails")
	}
	if id == 0 {
		t.Error("id = 0, want a usable in-memory id: publication must not stall")
	}
}

func TestPublishStampsEnvelope(t *testing.T) {
	h, _ := newTestHub()
	sub, _, _ := h.Subscribe(Subscriber{IsSupervisor: true}, 0)
	defer sub.Close()

	out := h.Publish(context.Background(), Event{Type: TypePartyRinging}, Scope{})
	if out.Version != Version {
		t.Errorf("Version = %d, want %d", out.Version, Version)
	}
	if out.Seq == 0 {
		t.Error("Seq = 0, want a stamped sequence number")
	}
	if out.OccurredAt.IsZero() {
		t.Error("OccurredAt is zero, want a stamped timestamp")
	}

	got := <-sub.C
	if got.Seq != out.Seq || got.Type != TypePartyRinging {
		t.Errorf("received %+v, want seq %d type %s", got, out.Seq, TypePartyRinging)
	}
}

func TestScopingByIdentity(t *testing.T) {
	agentA := uuid.New()
	agentB := uuid.New()
	queue1 := uuid.New()
	queue2 := uuid.New()

	tests := []struct {
		name string
		who  Subscriber
		ev   Event
		sc   Scope
		want bool
	}{
		{
			name: "supervisor sees everything",
			who:  Subscriber{IsSupervisor: true},
			sc:   Scope{AgentIDs: []uuid.UUID{agentA}, SupervisorOnly: true},
			want: true,
		},
		{
			name: "agent sees own party event",
			who:  Subscriber{AgentID: &agentA},
			sc:   Scope{AgentIDs: []uuid.UUID{agentA}},
			want: true,
		},
		{
			name: "agent does not see another agent's event",
			who:  Subscriber{AgentID: &agentA},
			sc:   Scope{AgentIDs: []uuid.UUID{agentB}},
			want: false,
		},
		{
			name: "agent sees queue event for a staffed queue",
			who:  Subscriber{AgentID: &agentA, QueueIDs: []uuid.UUID{queue1}},
			sc:   Scope{QueueID: &queue1},
			want: true,
		},
		{
			name: "agent does not see other queues",
			who:  Subscriber{AgentID: &agentA, QueueIDs: []uuid.UUID{queue1}},
			sc:   Scope{QueueID: &queue2},
			want: false,
		},
		{
			name: "agent never sees supervisor-only events",
			who:  Subscriber{AgentID: &agentA},
			sc:   Scope{SupervisorOnly: true},
			want: false,
		},
		{
			name: "unscoped system events reach agents",
			who:  Subscriber{AgentID: &agentA},
			ev:   Event{Type: TypeSystemLink},
			sc:   Scope{},
			want: true,
		},
		{
			name: "client type filter narrows further",
			who:  Subscriber{IsSupervisor: true, Types: []Type{TypeAgentReady}},
			ev:   Event{Type: TypePartyRinging},
			want: false,
		},
		{
			name: "client type filter keeps requested types",
			who:  Subscriber{IsSupervisor: true, Types: []Type{TypeAgentReady}},
			ev:   Event{Type: TypeAgentReady},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.who.wants(tt.ev, tt.sc); got != tt.want {
				t.Errorf("wants() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResumeReplaysAfterLastEventID(t *testing.T) {
	h, _ := newTestHub()
	ctx := context.Background()

	first := h.Publish(ctx, Event{Type: TypeAgentReady}, Scope{})
	second := h.Publish(ctx, Event{Type: TypeAgentNotReady}, Scope{})

	sub, replay, reset := h.Subscribe(Subscriber{IsSupervisor: true}, first.Seq)
	defer sub.Close()

	if reset {
		t.Fatal("reset = true, want false: the resume point is still in the ring")
	}
	if len(replay) != 1 || replay[0].Seq != second.Seq {
		t.Fatalf("replay = %+v, want only seq %d", replay, second.Seq)
	}
}

func TestResumeBeyondRingRequestsReset(t *testing.T) {
	h, _ := newTestHub()
	h.Publish(context.Background(), Event{Type: TypeAgentReady}, Scope{})

	// A resume point older than anything retained.
	sub, replay, reset := h.Subscribe(Subscriber{IsSupervisor: true}, 0)
	defer sub.Close()
	if reset || replay != nil {
		t.Fatal("fresh subscribe should neither reset nor replay")
	}

	sub2, _, reset2 := h.Subscribe(Subscriber{IsSupervisor: true}, -5)
	defer sub2.Close()
	if reset2 {
		t.Error("non-positive Last-Event-ID should start a fresh stream, not a reset")
	}

	h2, _ := newTestHub()
	sub3, _, reset3 := h2.Subscribe(Subscriber{IsSupervisor: true}, 42)
	defer sub3.Close()
	if !reset3 {
		t.Error("reset = false, want true when the ring cannot serve the resume point")
	}
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	h, _ := newTestHub()
	var droppedFor int
	h.OnDropped = func(Subscriber) { droppedFor++ }

	sub, _, _ := h.Subscribe(Subscriber{IsSupervisor: true}, 0)
	ctx := context.Background()

	// Never read from sub.C: overflow the buffer.
	for range subscriberBuffer + 5 {
		h.Publish(ctx, Event{Type: TypeQueueCount}, Scope{})
	}

	if !sub.Dropped {
		t.Error("Dropped = false, want true for a subscriber that never reads")
	}
	if droppedFor == 0 {
		t.Error("OnDropped was not called")
	}
	if h.SubscriberCount() != 0 {
		t.Errorf("SubscriberCount = %d, want 0 after the drop", h.SubscriberCount())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h, _ := newTestHub()
	sub, _, _ := h.Subscribe(Subscriber{IsSupervisor: true}, 0)
	sub.Close()
	sub.Close() // must not panic on a double close
	if h.SubscriberCount() != 0 {
		t.Errorf("SubscriberCount = %d, want 0", h.SubscriberCount())
	}
}
