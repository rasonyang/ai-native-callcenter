// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"errors"
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

// notOnSwitchError mimics what the adapter returns for an agent the switch has
// not met yet.
type notOnSwitchError struct{}

func (notOnSwitchError) Error() string          { return "the switch does not know this agent yet" }
func (notOnSwitchError) AgentNotOnSwitch() bool { return true }

type deferringSwitch struct{ *fakeSwitch }

func (d deferringSwitch) AddCallcenterTier(string, string, int, int) error {
	return notOnSwitchError{}
}

// mod_callcenter only knows agents who are signed in, so a tier for anyone
// staffed while signed out cannot be held yet. That is the ordinary state, not
// drift: their sign-in applies it. Counting it as a failure would have every
// reconnect report a problem nothing could have fixed, which is how a warning
// becomes background noise and then becomes unread.
func TestATierForAnAgentTheSwitchHasNotMetIsDeferredNotFailed(t *testing.T) {
	queueID, agentID := uuid.New(), uuid.New()
	store := &fakeStore{
		queues:   []Queue{{ID: queueID, Name: "support-en"}},
		staffing: map[uuid.UUID][]QueueAgent{queueID: {{AgentID: agentID, Level: 1, Position: 1}}},
	}
	base := &fakeSwitch{isUp: true, onSwitch: map[string][]string{}}
	svc := NewService(store, deferringSwitch{base}, fakeNames{})

	got := svc.converge(context.Background(), "agent-wei",
		map[string]QueueAgent{"support-en": {Level: 1, Position: 1}}, map[string]struct{}{})

	if got.deferred != 1 {
		t.Errorf("deferred = %d, want 1", got.deferred)
	}
	if got.failed != 0 {
		t.Errorf("failed = %d, want 0 — a tier the switch cannot hold yet is not "+
			"drift, and counting it as such reports a problem on every reconnect "+
			"that nothing could have fixed", got.failed)
	}
	if got.added != 0 || got.removed != 0 {
		t.Errorf("changed something: %+v", got)
	}

	// And through the public path, nothing reaches the switch.
	svc.ReconcileAgentTiers(context.Background(), agentID)
	if len(base.added) != 0 || len(base.removed) != 0 {
		t.Errorf("added %+v removed %+v; neither should have happened", base.added, base.removed)
	}
	_ = queueID
}

// honestSwitch actually does what it is told, so its tier list changes. The
// bare fakeSwitch does not, which is not laziness — it is mod_callcenter,
// which answers +OK to `tier del` for a tier that was never there.
type honestSwitch struct{ *fakeSwitch }

func (h honestSwitch) AddCallcenterTier(queue, agent string, level, position int) error {
	if err := h.fakeSwitch.AddCallcenterTier(queue, agent, level, position); err != nil {
		return err
	}
	h.onSwitch[agent] = append(h.onSwitch[agent], queue)
	return nil
}

func (h honestSwitch) DeleteCallcenterTier(queue, agent string) error {
	if err := h.fakeSwitch.DeleteCallcenterTier(queue, agent); err != nil {
		return err
	}
	kept := h.onSwitch[agent][:0]
	for _, q := range h.onSwitch[agent] {
		if q != queue {
			kept = append(kept, q)
		}
	}
	h.onSwitch[agent] = kept
	return nil
}

// The half of C1 the queue-name fix did not reach. Live probe, verbatim:
//
//	callcenter_config tier del does-not-exist agent-wei  → +OK
//	callcenter_config tier del support-en agent-nobody   → +OK
//
// The switch says +OK for a tier that was never there, and converge counted a
// nil error as a removal. So `removed=` reported work that had not happened —
// it said 1 for a tier that had not existed since this machine changed
// address. VC-S3-02 cannot see this: it asserts that both sides agree
// afterwards, and a false report of how they came to agree does not disturb
// that. Only counting the switch's own before and after catches it.
func TestConvergeCountsWhatTheSwitchDidNotWhatItAccepted(t *testing.T) {
	t.Run("a removal the switch only said +OK to is not counted", func(t *testing.T) {
		sw := &fakeSwitch{isUp: true, onSwitch: map[string][]string{
			// The switch's staffing does not change when told to delete, which
			// is what a tier that was never really there looks like.
			"agent-wei": {"support-en"},
		}}
		svc := NewService(&fakeStore{}, sw, fakeNames{})

		got := svc.converge(context.Background(), "agent-wei",
			map[string]QueueAgent{}, map[string]struct{}{"support-en": {}})

		if len(sw.removed) != 1 {
			t.Fatalf("the delete was not attempted at all: %+v", sw.removed)
		}
		if got.removed != 0 {
			t.Errorf("removed = %d, want 0 — the switch answered +OK and removed nothing, "+
				"and reporting work that did not happen is the whole defect", got.removed)
		}
		if !got.isVerified {
			t.Error("isVerified = false, but the switch answered the second read")
		}
	})

	t.Run("a removal that happened is counted", func(t *testing.T) {
		base := &fakeSwitch{isUp: true, onSwitch: map[string][]string{"agent-wei": {"support-en"}}}
		svc := NewService(&fakeStore{}, honestSwitch{base}, fakeNames{})

		got := svc.converge(context.Background(), "agent-wei",
			map[string]QueueAgent{}, map[string]struct{}{"support-en": {}})

		if got.removed != 1 {
			t.Errorf("removed = %d, want 1 — the tier is gone from the switch's own list; "+
				"refusing to count a real removal would be the opposite lie", got.removed)
		}
	})

	t.Run("an addition that happened is counted", func(t *testing.T) {
		base := &fakeSwitch{isUp: true, onSwitch: map[string][]string{}}
		svc := NewService(&fakeStore{}, honestSwitch{base}, fakeNames{})

		got := svc.converge(context.Background(), "agent-wei",
			map[string]QueueAgent{"support-en": {Level: 1, Position: 1}}, map[string]struct{}{})

		if got.added != 1 {
			t.Errorf("added = %d, want 1", got.added)
		}
	})

	t.Run("a second read that fails is reported as unverified", func(t *testing.T) {
		sw := &blindSwitch{fakeSwitch: &fakeSwitch{isUp: true,
			onSwitch: map[string][]string{"agent-wei": {"support-en"}}}}
		svc := NewService(&fakeStore{}, sw, fakeNames{})

		got := svc.converge(context.Background(), "agent-wei",
			map[string]QueueAgent{}, map[string]struct{}{"support-en": {}})

		if got.isVerified {
			t.Error("isVerified = true although the switch could not be re-read")
		}
		// The attempted count is still the best answer available; what must not
		// happen is it passing for an observation.
		if got.removed != 1 {
			t.Errorf("removed = %d, want the attempted 1 — unverified, not discarded", got.removed)
		}
	})
}

// blindSwitch takes commands but cannot be asked what it holds, which is the
// one case the counts are a claim rather than an observation.
type blindSwitch struct{ *fakeSwitch }

func (blindSwitch) CallcenterTiers() (map[string][]string, error) {
	return nil, errors.New("no reply")
}
