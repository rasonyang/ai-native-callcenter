// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// The queue's waiting list is the one thing about a queue no other part of the
// system knows: the switch reports a bare count, and a caller listening to
// hold music looks exactly like a caller talking to somebody from the call
// registry's side. These pin what the coordinator makes of the queue events.

var (
	supportEN = QueueSummary{ID: uuid.New(), Name: "support-en",
		DisplayName: "English Support", SLAThresholdSec: 20}
	supportZH = QueueSummary{ID: uuid.New(), Name: "support-zh",
		DisplayName: "中文支持", SLAThresholdSec: 30}
)

// fakeQueues answers for the two queues above and nothing else, so an
// unconfigured queue name behaves the way it does in production.
type fakeQueues struct{}

func (fakeQueues) QueueByName(_ context.Context, name string) (QueueSummary, bool) {
	for _, q := range []QueueSummary{supportEN, supportZH} {
		if q.Name == name {
			return q, true
		}
	}
	return QueueSummary{}, false
}

func (fakeQueues) Queues(context.Context) ([]QueueSummary, error) {
	return []QueueSummary{supportEN, supportZH}, nil
}

// The queue panel is driven by a run of events rather than by one, so this
// adds "every event of a type" to the shared capturing publisher.
func ofType(p *capturingPublisher, t events.Type) []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []events.Event
	for _, ev := range p.events {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

// queueEvent builds a callcenter::info event the way mod_callcenter sends one.
func queueEvent(action, queue, memberChannel string, extra map[string]string) SwitchEvent {
	headers := map[string]string{
		"Event-Name":             "CUSTOM",
		"Event-Subclass":         "callcenter::info",
		"CC-Action":              action,
		"CC-Queue":               queue + "@default",
		"CC-Member-Session-UUID": memberChannel,
		"CC-Member-CID-Number":   "13800138000",
	}
	for k, v := range extra {
		headers[k] = v
	}
	ev, ok := Normalize(esl.NewEvent(headers, ""))
	if !ok {
		panic("unnormalizable queue event: " + action)
	}
	return ev
}

func waitingFixture(t *testing.T) (*Coordinator, *capturingPublisher) {
	t.Helper()
	pub := &capturingPublisher{}
	registry := NewRegistry(pub)
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, noAgents{}, pub)
	c.AttachQueues(fakeQueues{})
	return c, pub
}

func TestAQueuedCallerIsWaitingUntilAnAgentTakesThem(t *testing.T) {
	c, pub := waitingFixture(t)
	ctx := context.Background()

	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-1", nil))

	waiting := c.WaitingCalls([]uuid.UUID{supportEN.ID})
	if len(waiting) != 1 {
		t.Fatalf("waiting = %d callers, want 1", len(waiting))
	}
	got := waiting[0]
	if got.QueueID != supportEN.ID || got.QueueName != "support-en" ||
		got.QueueDisplayName != "English Support" || got.SLAThresholdSec != 20 {
		t.Errorf("waiting call = %+v, want the queue's own configuration on it", got)
	}
	if got.FromNumber != "13800138000" {
		t.Errorf("fromNumber = %q, want the caller's number", got.FromNumber)
	}
	if joined := ofType(pub, events.TypeQueueJoined); len(joined) != 1 {
		t.Fatalf("published %d QUEUE_JOINED events, want 1", len(joined))
	}

	// Bridged to an agent: no longer waiting, even though mod_callcenter will
	// not announce member-queue-end until the conversation is over.
	c.Handle(ctx, queueEvent("bridge-agent-start", "support-en", "caller-1", nil))
	if waiting := c.WaitingCalls([]uuid.UUID{supportEN.ID}); len(waiting) != 0 {
		t.Errorf("still waiting after the bridge: %+v — a caller talking to an "+
			"agent would sit in the queue panel for the whole call", waiting)
	}
	if left := ofType(pub, events.TypeQueueLeft); len(left) != 1 {
		t.Fatalf("published %d QUEUE_LEFT events, want 1", len(left))
	}
}

func TestAnAbandonedCallerLeavesTheQueueWithTheQueuesOwnReason(t *testing.T) {
	c, pub := waitingFixture(t)
	ctx := context.Background()

	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-1", nil))
	c.Handle(ctx, queueEvent("member-queue-end", "support-en", "caller-1", map[string]string{
		"CC-Cause":         "Cancel",
		"CC-Cancel-Reason": "BREAK_OUT",
	}))

	if waiting := c.WaitingCalls([]uuid.UUID{supportEN.ID}); len(waiting) != 0 {
		t.Fatalf("still waiting after abandoning: %+v", waiting)
	}
	left := ofType(pub, events.TypeQueueLeft)
	if len(left) != 1 {
		t.Fatalf("published %d QUEUE_LEFT events, want 1", len(left))
	}
	if got := left[0].Payload["cause"]; got != "Cancel" {
		t.Errorf("cause = %v, want the queue's own word", got)
	}
	if got := left[0].Payload["cancelReason"]; got != "BREAK_OUT" {
		t.Errorf("cancelReason = %v, want BREAK_OUT", got)
	}
}

// The one the switch does not owe us: a caller who hangs up mid-queue may
// never be announced as leaving it, and a ghost in the list never expires.
func TestAHangupTakesTheCallerOutOfTheQueue(t *testing.T) {
	c, _ := waitingFixture(t)
	ctx := context.Background()

	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-1", nil))
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", "caller-1", "inbound", nil))

	if waiting := c.WaitingCalls([]uuid.UUID{supportEN.ID}); len(waiting) != 0 {
		t.Errorf("a hung-up caller is still listed as waiting: %+v", waiting)
	}
}

func TestTheWaitingListIsLongestWaitFirstAndPerQueue(t *testing.T) {
	c, _ := waitingFixture(t)
	ctx := context.Background()

	base := time.Now().Add(-5 * time.Minute).Unix()
	joinAt := func(offsetSec int64) map[string]string {
		return map[string]string{"CC-Member-Joined-Time": strconv.FormatInt(base+offsetSec, 10)}
	}
	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "newest", joinAt(120)))
	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "oldest", joinAt(0)))
	c.Handle(ctx, queueEvent("member-queue-start", "support-zh", "other-queue", joinAt(60)))

	waiting := c.WaitingCalls([]uuid.UUID{supportEN.ID})
	if len(waiting) != 2 {
		t.Fatalf("waiting in support-en = %d, want 2 — the other queue leaked in", len(waiting))
	}
	if !waiting[0].JoinedAt.Before(waiting[1].JoinedAt) {
		t.Errorf("order = %v then %v, want longest wait first: that is who the "+
			"queue serves next", waiting[0].JoinedAt, waiting[1].JoinedAt)
	}

	// An agent staffing both queues sees both lines, still in wait order.
	both := c.WaitingCalls([]uuid.UUID{supportEN.ID, supportZH.ID})
	if len(both) != 3 {
		t.Fatalf("waiting across both queues = %d, want 3", len(both))
	}
	if len(c.AllWaitingCalls()) != 3 {
		t.Errorf("supervision sees %d waiting, want 3", len(c.AllWaitingCalls()))
	}
}

// The count is what the queue's own panel reads, and it is addressed to the
// agents who staff that queue — not broadcast, and not to nobody.
func TestQueueCountFollowsTheLineAndReachesTheQueuesAgents(t *testing.T) {
	c, pub := waitingFixture(t)
	ctx := context.Background()

	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-1", nil))
	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-2", nil))
	c.Handle(ctx, queueEvent("bridge-agent-start", "support-en", "caller-1", nil))

	counts := ofType(pub, events.TypeQueueCount)
	if len(counts) != 3 {
		t.Fatalf("published %d QUEUE_COUNT events, want one per movement", len(counts))
	}
	var got []any
	for _, ev := range counts {
		got = append(got, ev.Payload["waiting"])
	}
	want := []any{1, 2, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waiting counts = %v, want %v", got, want)
		}
	}
	if _, ok := counts[len(counts)-1].Payload["longestWaitAt"]; !ok {
		t.Error("the count says nothing about the longest wait while somebody is waiting")
	}

	_, scope, ok := pub.find(events.TypeQueueCount)
	if !ok {
		t.Fatal("no QUEUE_COUNT was published")
	}
	if scope.QueueID == nil || *scope.QueueID != supportEN.ID {
		t.Errorf("scope.queueId = %v, want the queue, so the agents staffing it "+
			"receive it and nobody else does", scope.QueueID)
	}
	if scope.IsBroadcast {
		t.Error("the queue's depth is not everybody's business")
	}
}

// A queue that is not configured here cannot be rendered, so nothing is
// recorded rather than a row nobody can read.
func TestAnUnknownQueueIsNotTracked(t *testing.T) {
	c, pub := waitingFixture(t)
	ctx := context.Background()

	c.Handle(ctx, queueEvent("member-queue-start", "vip", "caller-1", nil))

	if n := len(c.AllWaitingCalls()); n != 0 {
		t.Errorf("tracked %d callers in an unconfigured queue, want none", n)
	}
	if n := len(ofType(pub, events.TypeQueueJoined)); n != 0 {
		t.Errorf("published %d QUEUE_JOINED events for an unconfigured queue", n)
	}
}

// The same member announced twice is one caller. mod_callcenter re-announces
// a member on some transitions, and a queue panel that counted each one would
// drift up and never come back down.
func TestAReannouncedMemberIsNotASecondCaller(t *testing.T) {
	c, _ := waitingFixture(t)
	ctx := context.Background()

	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-1", nil))
	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-1", nil))

	if n := len(c.WaitingCalls([]uuid.UUID{supportEN.ID})); n != 1 {
		t.Errorf("waiting = %d, want 1", n)
	}
}

// The call's own facts ride the entry, so the panel can name the caller
// without a second request — and the envelope names the call, so a click can
// open it.
func TestAWaitingCallCarriesTheCallItBelongsTo(t *testing.T) {
	c, pub := waitingFixture(t)
	ctx := context.Background()

	callID := uuid.New()
	if _, err := c.registry.CreateCall(ctx, callID, events.CallTypeInbound, "zh", true, testTime); err != nil {
		t.Fatal(err)
	}
	if err := c.registry.BindChannel("caller-1", callID); err != nil {
		t.Fatal(err)
	}
	if err := c.registry.Do(callID, func(call *Call) {
		call.AddParty("caller-1", "+8613800138000", time.Now())
		call.UserData = map[string]any{"ticketId": "T-42"}
	}); err != nil {
		t.Fatal(err)
	}

	c.Handle(ctx, queueEvent("member-queue-start", "support-en", "caller-1", nil))

	waiting := c.WaitingCalls([]uuid.UUID{supportEN.ID})
	if len(waiting) != 1 {
		t.Fatalf("waiting = %d, want 1", len(waiting))
	}
	got := waiting[0]
	if got.CallID != callID {
		t.Errorf("callId = %s, want %s", got.CallID, callID)
	}
	if got.FromNumber != "+8613800138000" {
		t.Errorf("fromNumber = %q, want the originator's number", got.FromNumber)
	}
	if got.Language != "zh" || got.CallType != events.CallTypeInbound {
		t.Errorf("call facts = %s/%s, want zh/INBOUND", got.Language, got.CallType)
	}
	if got.UserData["ticketId"] != "T-42" {
		t.Errorf("userData = %v, want the business data the bot collected", got.UserData)
	}

	joined := ofType(pub, events.TypeQueueJoined)
	if len(joined) != 1 || joined[0].CallID == nil || *joined[0].CallID != callID {
		t.Errorf("QUEUE_JOINED names call %v, want %s", joined[0].CallID, callID)
	}
}

// A caller the registry has never heard of is still waiting: the entry stands
// on the switch's own account rather than being dropped.
func TestAWaitingCallerWithNoKnownCallIsStillListed(t *testing.T) {
	c, pub := waitingFixture(t)

	c.Handle(context.Background(), queueEvent("member-queue-start", "support-en", "stranger", nil))

	waiting := c.WaitingCalls([]uuid.UUID{supportEN.ID})
	if len(waiting) != 1 {
		t.Fatalf("waiting = %d, want 1", len(waiting))
	}
	if waiting[0].CallID != uuid.Nil {
		t.Errorf("callId = %s, want the nil id for a call we do not know", waiting[0].CallID)
	}
	joined := ofType(pub, events.TypeQueueJoined)
	if len(joined) != 1 || joined[0].CallID != nil {
		t.Error("the envelope names a call that does not exist; an unknown call " +
			"must be absent rather than the nil id")
	}
}
