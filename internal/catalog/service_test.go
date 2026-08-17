// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// tierCall is one staffing command the service sent to the switch.
type tierCall struct {
	queue, agent    string
	level, position int
}

type fakeSwitch struct {
	isUp  bool
	added []tierCall
}

func (f *fakeSwitch) ReloadQueue(string) error { return nil }
func (f *fakeSwitch) IsUp() bool               { return f.isUp }
func (f *fakeSwitch) AddCallcenterTier(queue, agent string, level, position int) error {
	f.added = append(f.added, tierCall{queue, agent, level, position})
	return nil
}
func (f *fakeSwitch) DeleteCallcenterTier(string, string) error { return nil }

// fakeStore carries only what SyncTiers reads.
type fakeStore struct {
	Store
	queues   []Queue
	staffing map[uuid.UUID][]QueueAgent
}

func (f *fakeStore) ListQueues(context.Context) ([]Queue, error) { return f.queues, nil }
func (f *fakeStore) ListQueueAgents(_ context.Context, queueID uuid.UUID) ([]QueueAgent, error) {
	return f.staffing[queueID], nil
}

type fakeNames struct{ missing uuid.UUID }

func (f fakeNames) CallcenterName(_ context.Context, agentID uuid.UUID) (string, error) {
	if agentID == f.missing {
		return "", fmt.Errorf("no such agent")
	}
	return "agent-" + agentID.String()[:4], nil
}

// A switch restart forgets its tiers. Presence is already rebuilt on reconnect;
// without this the agent exists on the switch but staffs nothing, so the queue
// has nobody to offer to and every caller waits out max_wait_time and abandons
// — while our own database still says the agent staffs the queue. The failure
// is invisible from this side, which is why it is restored unconditionally.
func TestSyncTiersRestoresEveryQueuesStaffing(t *testing.T) {
	support, sales := uuid.New(), uuid.New()
	alice, bob := uuid.New(), uuid.New()

	store := &fakeStore{
		queues: []Queue{{ID: support, Name: "support-en"}, {ID: sales, Name: "sales"}},
		staffing: map[uuid.UUID][]QueueAgent{
			support: {{AgentID: alice, Level: 1, Position: 1}, {AgentID: bob, Level: 2, Position: 1}},
			sales:   {{AgentID: alice, Level: 1, Position: 3}},
		},
	}
	sw := &fakeSwitch{isUp: true}
	svc := NewService(store, sw, fakeNames{})

	svc.SyncTiers(context.Background())

	if len(sw.added) != 3 {
		t.Fatalf("restored %d tiers, want 3: %+v", len(sw.added), sw.added)
	}
	// Level and position are carried through, not defaulted: they are the
	// order in which a queue widens its search.
	var found bool
	for _, c := range sw.added {
		if c.queue == "sales" && c.level == 1 && c.position == 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("the sales tier lost its level/position: %+v", sw.added)
	}
}

func TestSyncTiersDoesNothingWhenTheSwitchIsDown(t *testing.T) {
	queueID := uuid.New()
	store := &fakeStore{
		queues:   []Queue{{ID: queueID, Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{queueID: {{AgentID: uuid.New(), Level: 1, Position: 1}}},
	}
	sw := &fakeSwitch{isUp: false}
	NewService(store, sw, fakeNames{}).SyncTiers(context.Background())

	if len(sw.added) != 0 {
		t.Errorf("sent %d commands to a switch that is down", len(sw.added))
	}
}

// An agent that no longer exists must not stop the queues behind it from being
// restored: one stale row would otherwise cost the whole call centre its
// routing.
func TestSyncTiersSkipsAMissingAgentAndKeepsGoing(t *testing.T) {
	queueID := uuid.New()
	gone, present := uuid.New(), uuid.New()
	store := &fakeStore{
		queues: []Queue{{ID: queueID, Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{
			queueID: {{AgentID: gone, Level: 1, Position: 1}, {AgentID: present, Level: 1, Position: 2}},
		},
	}
	sw := &fakeSwitch{isUp: true}
	NewService(store, sw, fakeNames{missing: gone}).SyncTiers(context.Background())

	if len(sw.added) != 1 {
		t.Fatalf("restored %d tiers, want 1: %+v", len(sw.added), sw.added)
	}
	if sw.added[0].position != 2 {
		t.Errorf("restored the wrong tier: %+v", sw.added[0])
	}
}
