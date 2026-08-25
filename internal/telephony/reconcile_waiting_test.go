// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// channelSaying answers uuid_getvar for the adopted channel as well as the
// member listing, so a test can say what the switch remembers about a call
// this process never saw start.
func channelSaying(vars map[string]string, rows map[string][]string) func(string) (string, error) {
	members := switchHolding(rows, nil)
	return func(cmd string) (string, error) {
		if after, ok := strings.CutPrefix(cmd, "uuid_getvar "); ok {
			fields := strings.Fields(after)
			if len(fields) == 2 {
				if v, ok := vars[fields[1]]; ok {
					return v, nil
				}
			}
			return "_undef_", nil
		}
		return members(cmd)
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

// The waiting entry is only half of it. A caller the registry does not know is
// a caller whose delivery leg has nothing to attach to, so mod_callcenter's
// offer becomes a call of its own — outbound, agent as originator, one per
// retry. That is what the switch showed live on 2026-08-23.
func TestAdoptingAQueuedCallerGivesTheDeliveryLegSomethingToBindTo(t *testing.T) {
	joined := time.Now().Add(-90 * time.Second).Truncate(time.Second).UTC()
	started := joined.Add(-19 * time.Second)
	callID := uuid.MustParse("01a02c54-c2a3-7dda-856b-80d7c8fbe00d")

	c, _ := reconcileFixture(t, channelSaying(map[string]string{
		"aicc_call_id":  callID.String(),
		"aicc_language": "en",
	}, map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}))

	c.ReconcileWaiting(context.Background())

	got, known := c.registry.CallForChannel("caller-1")
	if !known {
		t.Fatal("the caller's channel is still unknown; every offer will open a call of its own")
	}
	// Recovered, not minted: the recording and the transcript already name it.
	if got != callID {
		t.Errorf("callId = %s, want the id the dialplan minted %s", got, callID)
	}

	snap, err := c.registry.Snapshot(callID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.CallType != events.CallTypeInbound {
		t.Errorf("callType = %q, want INBOUND", snap.CallType)
	}
	if !snap.CreatedAt.Equal(started) {
		t.Errorf("createdAt = %s, want the call's own start %s — a recovered call "+
			"must not begin at the recovery", snap.CreatedAt, started)
	}
	if len(snap.Parties) != 1 {
		t.Fatalf("adopted %d parties, want 1: %+v", len(snap.Parties), snap.Parties)
	}
	party := snap.Parties[0]
	if party.Role != RoleOriginator || party.State != PartyDialing {
		t.Errorf("party = %s/%s, want ORIGINATOR/DIALING — the switch answered them long "+
			"ago, which the restored answeredAt records, but a caller waiting in a queue "+
			"is talking to nobody and reaches TALKING at the bridge like everyone else",
			party.Role, party.State)
	}
	// And the waiting entry now names the call rather than only the number.
	waiting := c.AllWaitingCalls()
	if len(waiting) != 1 || waiting[0].CallID != callID {
		t.Errorf("the waiting entry did not pick up the adopted call: %+v", waiting)
	}
}

// A caller adopted across a restart brings the bot's half of the call with
// them, because it is stamped on the channel and the channel is the switch's.
//
// Without this the AI phase disappears from the ledger while its transcript
// sits in the database proving it happened — bot_sec 0, no flow, no BOT leg in
// the journey, on a call that spent most of its life with a bot. Seen on
// 01a02dc8: ten transcript lines, and the application restarted three seconds
// after the last of them (C58).
func TestAnAdoptedCallerBringsTheBotsHalfOfTheCallWithThem(t *testing.T) {
	joined := time.Now().Add(-90 * time.Second).Truncate(time.Second).UTC()
	callID := uuid.MustParse("01a02c54-c2a3-7dda-856b-80d7c8fbe00d")
	flowID := uuid.MustParse("019ffd60-d8db-736b-a1eb-b005dda34d28")

	c, _ := reconcileFixture(t, channelSaying(map[string]string{
		"aicc_call_id":     callID.String(),
		"aicc_language":    "en",
		"aicc_bot_sec":     "28",
		"aicc_flow_id":     flowID.String(),
		"aicc_did":         "95002",
		"aicc_bot_summary": "wants a refund",
		"aicc_bot_reason":  "Caller asked for a person",
	}, map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}))

	c.ReconcileWaiting(context.Background())

	snap, err := c.registry.Snapshot(callID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Bot.Sec != 28 {
		t.Errorf("botSec = %d, want the 28 seconds stamped on the channel", snap.Bot.Sec)
	}
	if snap.Bot.FlowID == nil || *snap.Bot.FlowID != flowID {
		t.Errorf("flowId = %v, want %s", snap.Bot.FlowID, flowID)
	}
	if snap.Bot.Summary != "wants a refund" || snap.Bot.Reason != "Caller asked for a person" {
		t.Errorf("summary=%q reason=%q", snap.Bot.Summary, snap.Bot.Reason)
	}
	// IsStamped is what decides who writes the ledger row. A caller in a queue
	// got there by being handed on, and the duration is stamped just before
	// that — so this is the one thing the adoption must not lose.
	if !snap.Bot.HandedOver() {
		t.Error("the adopted call does not know the bot handed it over, so the human " +
			"path will not write the row the bot is waiting for it to write")
	}
}

// A caller who never met a bot is adopted without inventing one for them.
func TestAnAdoptedCallerWhoNeverMetABotHasNoBotPhase(t *testing.T) {
	joined := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	callID := uuid.MustParse("01a02c54-c2a3-7dda-856b-80d7c8fbe00d")

	c, _ := reconcileFixture(t, channelSaying(map[string]string{
		"aicc_call_id": callID.String(),
	}, map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}))

	c.ReconcileWaiting(context.Background())

	snap, err := c.registry.Snapshot(callID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Bot.IsZero() {
		t.Errorf("bot = %+v, want nothing at all", snap.Bot)
	}
	if snap.Bot.HandedOver() {
		t.Error("a call with no bot reads as handed over by one")
	}
}

// A channel that cannot name its call is left alone. Minting an id here would
// orphan the recording and the transcript that already carry the real one.
func TestAChannelThatCannotNameItsCallIsNotAdopted(t *testing.T) {
	joined := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	c, _ := reconcileFixture(t, channelSaying(map[string]string{}, map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}))

	c.ReconcileWaiting(context.Background())

	if _, known := c.registry.CallForChannel("caller-1"); known {
		t.Error("a call was invented for a channel that never said which call it was")
	}
	// The caller is still visible, on the switch's own view of their number.
	waiting := c.AllWaitingCalls()
	if len(waiting) != 1 || waiting[0].FromNumber != "18688886669" {
		t.Errorf("the caller vanished from the line as well: %+v", waiting)
	}
}

// An adoption runs once. A second reconcile must not try to create the call
// again, nor add the caller's leg twice.
func TestAdoptingIsIdempotent(t *testing.T) {
	joined := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	callID := uuid.MustParse("01a02c54-c2a3-7dda-856b-80d7c8fbe00d")
	c, _ := reconcileFixture(t, channelSaying(map[string]string{
		"aicc_call_id": callID.String(),
	}, map[string][]string{
		"support-en": {memberRow("support-en", "caller-1", "18688886669", joined, "Waiting")},
	}))

	c.ReconcileWaiting(context.Background())
	c.ReconcileWaiting(context.Background())

	snap, err := c.registry.Snapshot(callID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Parties) != 1 {
		t.Errorf("the caller's leg was added %d times", len(snap.Parties))
	}
}
