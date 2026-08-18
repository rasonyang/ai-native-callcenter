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
	isUp bool
	// onSwitch is what the switch already believes, keyed by agent. Bare names:
	// the adapter owns the domain suffix in both directions, so this interface
	// only ever sees queues named the way the database names them.
	onSwitch map[string][]string
	added    []tierCall
	removed  []tierCall
}

func (f *fakeSwitch) ReloadQueue(string) error { return nil }
func (f *fakeSwitch) IsUp() bool               { return f.isUp }
func (f *fakeSwitch) AddCallcenterTier(queue, agent string, level, position int) error {
	f.added = append(f.added, tierCall{queue, agent, level, position})
	return nil
}
func (f *fakeSwitch) DeleteCallcenterTier(queue, agent string) error {
	f.removed = append(f.removed, tierCall{queue: queue, agent: agent})
	return nil
}

func (f *fakeSwitch) CallcenterTiers() (map[string][]string, error) {
	return f.onSwitch, nil
}

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

// The case this exists for: an agent staffed while signed out. mod_callcenter
// refuses a tier for an agent it does not know, so the staffing never reached
// the switch and nothing retried it. Signing in registers the agent, and the
// tier has to follow — otherwise they are Available, in a queue, and offered
// nothing, with our own database insisting they are staffed.
func TestSigningInAddsTheTierThatCouldNotBeAddedWhileSignedOut(t *testing.T) {
	queueID, agentID := uuid.New(), uuid.New()
	store := &fakeStore{
		queues:   []Queue{{ID: queueID, Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{queueID: {{AgentID: agentID, Level: 1, Position: 2}}},
	}
	// The switch knows the agent now, but holds no tier for them.
	sw := &fakeSwitch{isUp: true, onSwitch: map[string][]string{}}
	svc := NewService(store, sw, fakeNames{})

	svc.ReconcileAgentTiers(context.Background(), agentID)

	if len(sw.added) != 1 {
		t.Fatalf("added %d tiers, want 1: %+v", len(sw.added), sw.added)
	}
	if sw.added[0].queue != "support-en" || sw.added[0].level != 1 || sw.added[0].position != 2 {
		t.Errorf("tier = %+v, want support-en at level 1 position 2 — the level and "+
			"position are the queue's search order, not defaults", sw.added[0])
	}
	if len(sw.removed) != 0 {
		t.Errorf("removed %+v while adding a missing tier", sw.removed)
	}
}

// The mirror case, and the worse of the two: a queue unstaffed while the agent
// was signed out leaves the switch still offering them its calls.
func TestSigningInRemovesATierThisSystemNoLongerHolds(t *testing.T) {
	queueID, agentID := uuid.New(), uuid.New()
	store := &fakeStore{
		queues: []Queue{{ID: queueID, Name: "support-en"}},
		// Nobody staffs it any more.
		staffing: map[uuid.UUID][]QueueAgent{queueID: {}},
	}
	sw := &fakeSwitch{isUp: true, onSwitch: map[string][]string{
		"agent-" + agentID.String()[:4]: {"support-en"},
	}}
	svc := NewService(store, sw, fakeNames{})

	svc.ReconcileAgentTiers(context.Background(), agentID)

	if len(sw.removed) != 1 || sw.removed[0].queue != "support-en" {
		t.Fatalf("removed %+v, want the stale support-en tier — an agent taking "+
			"calls for a queue they were removed from is the worse failure", sw.removed)
	}
	if len(sw.added) != 0 {
		t.Errorf("added %+v while removing a stale tier", sw.added)
	}
}

// Staffing nothing is a normal state, not a fault. The invariant is desired
// against actual, and zero against zero satisfies it.
func TestAnAgentWhoStaffsNothingStaysAtZero(t *testing.T) {
	agentID := uuid.New()
	store := &fakeStore{
		queues:   []Queue{{ID: uuid.New(), Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{},
	}
	sw := &fakeSwitch{isUp: true, onSwitch: map[string][]string{}}
	NewService(store, sw, fakeNames{}).ReconcileAgentTiers(context.Background(), agentID)

	if len(sw.added) != 0 || len(sw.removed) != 0 {
		t.Errorf("added %+v removed %+v; staffing nothing is a legitimate state",
			sw.added, sw.removed)
	}
}

// A switch that already agrees is left alone — the reconcile must be idempotent,
// because it runs on every registration.
func TestAMatchingSwitchIsNotTouched(t *testing.T) {
	queueID, agentID := uuid.New(), uuid.New()
	name := "agent-" + agentID.String()[:4]
	store := &fakeStore{
		queues:   []Queue{{ID: queueID, Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{queueID: {{AgentID: agentID, Level: 1, Position: 1}}},
	}
	sw := &fakeSwitch{isUp: true, onSwitch: map[string][]string{name: {"support-en"}}}
	NewService(store, sw, fakeNames{}).ReconcileAgentTiers(context.Background(), agentID)

	if len(sw.added) != 0 || len(sw.removed) != 0 {
		t.Errorf("a matching switch was changed: added %+v removed %+v", sw.added, sw.removed)
	}
}

// Reconnect converges every agent either side knows about, not only the ones
// this system staffs. An agent the switch still holds a tier for and this
// system does not is the case the add-only version could not reach at all —
// and it is the one that leaves the switch offering calls on behalf of nobody.
func TestReconnectRemovesATierForAnAgentThisSystemDoesNotStaff(t *testing.T) {
	queueID, staffed := uuid.New(), uuid.New()
	store := &fakeStore{
		queues:   []Queue{{ID: queueID, Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{queueID: {{AgentID: staffed, Level: 1, Position: 1}}},
	}
	sw := &fakeSwitch{isUp: true, onSwitch: map[string][]string{
		// Staffed here and known to the switch: nothing to do.
		"agent-" + staffed.String()[:4]: {"support-en"},
		// Known only to the switch. There is no agent id to look up, and none
		// is needed: the tier is deleted by the name the switch reported.
		"agent-ghost": {"support-en"},
	}}

	NewService(store, sw, fakeNames{}).SyncTiers(context.Background())

	if len(sw.added) != 0 {
		t.Errorf("added %+v; both sides already agreed on the staffed agent", sw.added)
	}
	if len(sw.removed) != 1 || sw.removed[0].agent != "agent-ghost" {
		t.Fatalf("removed %+v, want only the ghost's tier", sw.removed)
	}
}

// Reconnect is the same convergence sign-in performs, so it must still add what
// is missing — the half it always covered.
func TestReconnectStillAddsMissingTiers(t *testing.T) {
	queueID, agentID := uuid.New(), uuid.New()
	store := &fakeStore{
		queues:   []Queue{{ID: queueID, Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{queueID: {{AgentID: agentID, Level: 2, Position: 3}}},
	}
	sw := &fakeSwitch{isUp: true, onSwitch: map[string][]string{}}

	NewService(store, sw, fakeNames{}).SyncTiers(context.Background())

	if len(sw.added) != 1 || sw.added[0].level != 2 || sw.added[0].position != 3 {
		t.Fatalf("added %+v, want support-en at level 2 position 3", sw.added)
	}
}
