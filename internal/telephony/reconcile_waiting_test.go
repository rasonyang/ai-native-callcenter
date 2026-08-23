// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

const memberHeader = `queue|instance_id|uuid|session_uuid|cid_number|cid_name|system_epoch|` +
	`joined_epoch|rejoined_epoch|bridge_epoch|abandoned_epoch|base_score|skill_score|` +
	`serving_agent|serving_system|state|score`

// memberRow renders one member the way the switch does, so these tests read
// the same shape the live parser was built against.
func memberRow(queue, channelID, number string, joined time.Time, state string) string {
	return fmt.Sprintf("%s|single_box|member-uuid|%s|%s|%s|%d|%d|0|0|0|0|0|agent-wei|single_box|%s|50",
		queue, channelID, number, number, joined.Unix()-19, joined.Unix(), state)
}

// switchHolding answers the member listing for whichever queues are given and
// nothing for the rest, so "an empty queue" and "a queue we could not read"
// stay distinguishable.
func switchHolding(rows map[string][]string, failing map[string]bool) func(string) (string, error) {
	return func(cmd string) (string, error) {
		const prefix = "callcenter_config queue list members "
		if !strings.HasPrefix(cmd, prefix) {
			return "", nil
		}
		queue := strings.TrimPrefix(cmd, prefix)
		if failing[queue] {
			return "", fmt.Errorf("-ERR no reply")
		}
		return strings.Join(append([]string{memberHeader}, rows[queue]...), "\n") + "\n+OK\n", nil
	}
}

func reconcileFixture(t *testing.T, replies func(string) (string, error)) (*Coordinator, *capturingPublisher) {
	t.Helper()
	pub := &capturingPublisher{}
	registry := NewRegistry(pub)
	t.Cleanup(registry.Shutdown)
	cmd := &fakeCommander{up: true, replyFor: replies}
	c := NewCoordinator(registry, NewAdapter(cmd, "aicc.test"), noAgents{}, pub)
	c.AttachQueues(fakeQueues{})
	return c, pub
}

// The join is announced once. Miss it — and a restart misses every one — and
// the caller waits in a queue nobody can see, which is what happened live on
// 2026-08-23: the switch held them, /calls/waiting was empty, and each offer
// to an agent surfaced as a call of its own.
func TestACallerQueuedWhileWeWereDownIsFoundAgain(t *testing.T) {
	joined := time.Now().Add(-2 * time.Minute).Truncate(time.Second).UTC()
	c, pub := reconcileFixture(t, switchHolding(map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Trying")},
	}, nil))

	c.ReconcileWaiting(context.Background())

	waiting := c.AllWaitingCalls()
	if len(waiting) != 1 {
		t.Fatalf("rebuilt %d waiting calls, want 1", len(waiting))
	}
	if waiting[0].QueueID != supportEN.ID || waiting[0].FromNumber != "18688886669" {
		t.Errorf("restored the wrong caller: %+v", waiting[0])
	}
	// The switch's own joined_epoch, not the moment we noticed. Restoring with
	// now would report a two-minute wait as a fresh call and quietly improve
	// the queue's service level for having lost track of them.
	if !waiting[0].JoinedAt.Equal(joined) {
		t.Errorf("joinedAt = %s, want the switch's %s — their wait was reset",
			waiting[0].JoinedAt, joined)
	}
	if !pub.has(events.TypeQueueJoined) {
		t.Error("nothing was published; the screens still do not know they are there")
	}
}

// Converge, not top up: the switch is the truth in both directions.
func TestACallerWhoLeftWhileWeWereDownIsNotStillWaiting(t *testing.T) {
	joined := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	rows := map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}
	replies := switchHolding(rows, nil)
	c, pub := reconcileFixture(t, func(cmd string) (string, error) { return replies(cmd) })

	c.ReconcileWaiting(context.Background())
	if len(c.AllWaitingCalls()) != 1 {
		t.Fatalf("setup did not restore the caller")
	}

	// They hung up while we were away; the switch no longer holds them.
	delete(rows, "support-en")
	c.ReconcileWaiting(context.Background())

	if got := c.AllWaitingCalls(); len(got) != 0 {
		t.Errorf("%d ghosts left waiting forever: %+v", len(got), got)
	}
	if !pub.has(events.TypeQueueLeft) {
		t.Error("the screens were never told they had gone")
	}
}

// An ordinary reconnect — the process never died, every entry already held —
// must be silent. Otherwise every blip republishes the whole waiting line.
func TestReconnectingWithNothingChangedSaysNothing(t *testing.T) {
	joined := time.Now().Add(-30 * time.Second).Truncate(time.Second).UTC()
	c, pub := reconcileFixture(t, switchHolding(map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}, nil))

	c.ReconcileWaiting(context.Background())
	before := len(pub.events)
	c.ReconcileWaiting(context.Background())

	if len(pub.events) != before {
		t.Errorf("a second reconcile published %d more events; a reconnect is not news",
			len(pub.events)-before)
	}
	if len(c.AllWaitingCalls()) != 1 {
		t.Errorf("the caller was duplicated or lost: %+v", c.AllWaitingCalls())
	}
}

// A queue that could not be read is not a queue known to be empty. Dropping
// its callers on a failed read would be the same false report as a delete that
// never checked what it deleted.
func TestAQueueWeCouldNotReadKeepsItsCallers(t *testing.T) {
	joined := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	rows := map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}
	failing := map[string]bool{}
	c, _ := reconcileFixture(t, func(cmd string) (string, error) {
		return switchHolding(rows, failing)(cmd)
	})

	c.ReconcileWaiting(context.Background())
	if len(c.AllWaitingCalls()) != 1 {
		t.Fatalf("setup did not restore the caller")
	}

	failing["support-en"] = true
	c.ReconcileWaiting(context.Background())

	if len(c.AllWaitingCalls()) != 1 {
		t.Errorf("a failed read emptied the queue: %+v", c.AllWaitingCalls())
	}
}

// A caller an agent has answered is not waiting, and the switch says so in the
// same listing. Restoring them would put a talking caller back in the line.
func TestAnAnsweredCallerIsNotRestoredToTheLine(t *testing.T) {
	joined := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	c, _ := reconcileFixture(t, switchHolding(map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Answered")},
	}, nil))

	c.ReconcileWaiting(context.Background())

	if got := c.AllWaitingCalls(); len(got) != 0 {
		t.Errorf("a caller already talking to an agent was put back in the queue: %+v", got)
	}
}
