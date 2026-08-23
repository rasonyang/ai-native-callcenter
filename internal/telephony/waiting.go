// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// QueueSummary is what a queue is called and what it promises, as the waiting
// line needs it. The switch speaks in queue names; every screen speaks in ids,
// display names and a target to measure a wait against.
type QueueSummary struct {
	ID              uuid.UUID
	Name            string
	DisplayName     string
	SLAThresholdSec int
}

// QueueCatalog resolves the switch's name for a queue into its configuration.
//
// A port rather than the catalog service itself: this package owns which
// callers are waiting and nothing else, and how queues are configured is not
// its business.
type QueueCatalog interface {
	QueueByName(ctx context.Context, name string) (QueueSummary, bool)
	// Queues is every queue this system configures, which is the list to ask
	// the switch about when rebuilding who is waiting. The switch will only be
	// asked about queues we could render anyway.
	Queues(ctx context.Context) ([]QueueSummary, error)
}

// WaitingCall is one caller in a queue who has not reached anybody yet.
type WaitingCall struct {
	CallID           uuid.UUID       `json:"callId"`
	CallType         events.CallType `json:"callType"`
	FromNumber       string          `json:"fromNumber"`
	QueueID          uuid.UUID       `json:"queueId"`
	QueueName        string          `json:"queueName"`
	QueueDisplayName string          `json:"queueDisplayName"`
	SLAThresholdSec  int             `json:"slaThresholdSec"`
	JoinedAt         time.Time       `json:"joinedAt"`
	Language         string          `json:"language,omitempty"`
	UserData         map[string]any  `json:"userData,omitempty"`

	// channelID is the member's own channel, which is how the switch names
	// this caller and the only identifier that survives a merge.
	channelID string
}

// WaitingLine is who is queued right now, per queue.
//
// mod_callcenter holds this in its own tables and reports a bare count; a
// waiting *list* is the agent-facing form of the same fact and nothing else
// publishes it. It is kept here rather than derived from the call registry
// because "in a queue" is a queue fact: a call that is parked, ringing or
// talking looks identical from the call's own state.
//
// Keyed by the member channel: a call's identity can change under a merge, and
// the channel the switch queued cannot.
type WaitingLine struct {
	mu      sync.Mutex
	waiting map[string]WaitingCall
}

// NewWaitingLine builds an empty waiting line.
func NewWaitingLine() *WaitingLine {
	return &WaitingLine{waiting: make(map[string]WaitingCall)}
}

// join records a caller entering a queue, reporting false when that channel
// is already recorded as waiting there — a re-announced member is not a
// second caller.
func (w *WaitingLine) join(call WaitingCall) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if existing, ok := w.waiting[call.channelID]; ok && existing.QueueID == call.QueueID {
		return false
	}
	w.waiting[call.channelID] = call
	return true
}

// leave removes a caller from whichever queue they were waiting in.
func (w *WaitingLine) leave(channelID string) (WaitingCall, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	call, ok := w.waiting[channelID]
	if ok {
		delete(w.waiting, channelID)
	}
	return call, ok
}

// InQueues lists the callers waiting in any of the given queues, longest wait
// first — which is the order the queue will serve them in.
func (w *WaitingLine) InQueues(queueIDs []uuid.UUID) []WaitingCall {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]WaitingCall, 0, len(w.waiting))
	for _, call := range w.waiting {
		if slices.Contains(queueIDs, call.QueueID) {
			out = append(out, call)
		}
	}
	sortByWait(out)
	return out
}

// All lists every waiting caller, for supervision.
func (w *WaitingLine) All() []WaitingCall {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]WaitingCall, 0, len(w.waiting))
	for _, call := range w.waiting {
		out = append(out, call)
	}
	sortByWait(out)
	return out
}

// count reports how many are waiting in one queue and when the longest-waiting
// caller joined; the zero time means nobody is waiting.
func (w *WaitingLine) count(queueID uuid.UUID) (int, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := 0
	var longest time.Time
	for _, call := range w.waiting {
		if call.QueueID != queueID {
			continue
		}
		n++
		if longest.IsZero() || call.JoinedAt.Before(longest) {
			longest = call.JoinedAt
		}
	}
	return n, longest
}

func sortByWait(calls []WaitingCall) {
	slices.SortFunc(calls, func(a, b WaitingCall) int {
		if a.JoinedAt.Equal(b.JoinedAt) {
			return 0
		}
		if a.JoinedAt.Before(b.JoinedAt) {
			return -1
		}
		return 1
	})
}

//
// The coordinator's side: switch events in, waiting line and stream out.
//

// AttachQueues points queue membership at the queue configuration, which is
// what turns the switch's queue name into something a screen can render.
// Without it the waiting line stays empty rather than guessing.
func (c *Coordinator) AttachQueues(q QueueCatalog) { c.queues = q }

// WaitingCalls lists who is waiting in the given queues.
func (c *Coordinator) WaitingCalls(queueIDs []uuid.UUID) []WaitingCall {
	return c.waiting.InQueues(queueIDs)
}

// AllWaitingCalls lists every waiting caller, for supervision.
func (c *Coordinator) AllWaitingCalls() []WaitingCall { return c.waiting.All() }

// trackQueue keeps the waiting line in step with the switch and tells the
// screens that watch it.
//
// Bridging removes a caller as surely as leaving does: mod_callcenter only
// announces member-queue-end once the *bridge* ends, so a caller who is
// already talking to an agent would otherwise still be shown as waiting for
// the whole conversation. The switch's own count agrees — it counts members
// waiting or trying, never bridged ones.
func (c *Coordinator) trackQueue(ctx context.Context, ev SwitchEvent) {
	switch ev.Kind {
	case KindQueueMemberJoined:
		c.queueJoined(ctx, ev)
	case KindQueueBridgeStart, KindQueueMemberLeft:
		c.queueLeft(ctx, ev.MemberChannelID, ev)
	case KindChannelHangup:
		// A caller who hangs up in the queue may never be announced as
		// leaving it — the member-queue-end we do get can arrive after the
		// call is gone, and a queue we never hear from again would keep a
		// ghost in the list forever.
		c.queueLeft(ctx, ev.ChannelID, ev)
	}
}

func (c *Coordinator) queueJoined(ctx context.Context, ev SwitchEvent) {
	if c.queues == nil || ev.MemberChannelID == "" {
		return
	}
	queue, ok := c.queues.QueueByName(ctx, ev.Queue)
	if !ok {
		return
	}

	call := WaitingCall{
		QueueID:          queue.ID,
		QueueName:        queue.Name,
		QueueDisplayName: queue.DisplayName,
		SLAThresholdSec:  queue.SLAThresholdSec,
		JoinedAt:         ev.JoinedAt,
		channelID:        ev.MemberChannelID,
	}
	if call.JoinedAt.IsZero() {
		call.JoinedAt = ev.OccurredAt
	}

	// The call's own facts, so a screen can render the caller without asking
	// for anything else. A caller the registry does not know is still waiting,
	// so the entry stands with the switch's own view of the number.
	call.FromNumber = ev.ANI
	if callID, known := c.registry.CallForChannel(ev.MemberChannelID); known {
		call.CallID = callID
		_ = c.registry.Do(callID, func(inner *Call) {
			call.CallType = inner.CallType
			call.Language = inner.Language
			call.UserData = inner.UserData
			if o := inner.Originator(); o != nil && o.Number != "" {
				call.FromNumber = o.Number
			}
		})
	}

	if !c.waiting.join(call) {
		return
	}
	c.publish(ctx, events.Event{
		Type:     events.TypeQueueJoined,
		CallID:   callIDOrNil(call.CallID),
		CallType: call.CallType,
		QueueID:  &call.QueueID,
		UserData: call.UserData,
		Payload: map[string]any{
			"queueName":  call.QueueName,
			"fromNumber": call.FromNumber,
			"joinedAt":   call.JoinedAt,
		},
	}, events.Scope{QueueID: &call.QueueID})
	c.publishQueueCount(ctx, call.QueueID, call.QueueName)
}

// ReconcileWaiting converges the waiting line on the switch's own view of who
// is queued.
//
// A join is announced once. Miss that announcement and nothing ever says it
// again: the caller waits, the queue holds them, and every screen shows an
// empty line until an agent happens to answer. The whole of a restart is such
// a gap — a caller the bot hands to a queue while this process is down is
// announced to nobody — and that is not a rare case here, because the fallback
// that rescues a caller from a dying bot leg fires at exactly that moment.
//
// Converge rather than top up. The switch is the truth: a caller it holds and
// we do not is added, and one we hold and it does not is removed. Adding only
// would leave a ghost waiting forever for anyone who left during the gap,
// which is the same false report as a delete that never checked what it
// deleted.
//
// Restored entries go through queueJoined, so they publish and de-duplicate
// exactly as a live join does. That is what makes this safe on an ordinary
// reconnect, where the process never died and every entry is already held:
// the line's own key is the member's channel, so each restore is a no-op.
func (c *Coordinator) ReconcileWaiting(ctx context.Context) {
	if c.queues == nil || c.adapter == nil {
		return
	}
	queues, err := c.queues.Queues(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read queues to rebuild the waiting line", "error", err)
		return
	}

	held := map[string]bool{}
	read := map[uuid.UUID]bool{}
	restored := 0
	for _, queue := range queues {
		members, err := c.adapter.QueueMembers(queue.Name)
		if err != nil {
			// Leave this queue's entries alone: a queue we could not read is
			// not a queue we know to be empty.
			slog.WarnContext(ctx, "could not read a queue's members",
				"queue", queue.Name, "error", err)
			continue
		}
		read[queue.ID] = true
		for _, member := range members {
			if !member.IsWaiting() {
				continue
			}
			held[member.ChannelID] = true
			before := len(c.waiting.All())
			c.queueJoined(ctx, SwitchEvent{
				Kind:            KindQueueMemberJoined,
				Queue:           member.Queue,
				MemberChannelID: member.ChannelID,
				ANI:             member.Number,
				JoinedAt:        member.JoinedAt,
				OccurredAt:      time.Now().UTC(),
			})
			if len(c.waiting.All()) > before {
				restored++
			}
		}
	}

	dropped := 0
	for _, call := range c.waiting.All() {
		if held[call.channelID] || !read[call.QueueID] {
			continue
		}
		c.queueLeft(ctx, call.channelID, SwitchEvent{OccurredAt: time.Now().UTC()})
		dropped++
	}

	if restored > 0 || dropped > 0 {
		slog.InfoContext(ctx, "waiting line reconciled",
			"restored", restored, "dropped", dropped, "queues", len(read))
	}
}

func (c *Coordinator) queueLeft(ctx context.Context, channelID string, ev SwitchEvent) {
	if channelID == "" {
		return
	}
	call, ok := c.waiting.leave(channelID)
	if !ok {
		return
	}

	payload := map[string]any{
		"queueName":  call.QueueName,
		"fromNumber": call.FromNumber,
		"waitSec":    int(ev.OccurredAt.Sub(call.JoinedAt).Seconds()),
	}
	// The queue's own words for why, when it gave them: bridged, abandoned or
	// timed out are the queue's vocabulary, not ours to invent.
	if ev.Cause != "" {
		payload["cause"] = ev.Cause
	}
	if ev.CancelReason != "" {
		payload["cancelReason"] = ev.CancelReason
	}

	c.publish(ctx, events.Event{
		Type:     events.TypeQueueLeft,
		CallID:   callIDOrNil(call.CallID),
		CallType: call.CallType,
		QueueID:  &call.QueueID,
		Payload:  payload,
	}, events.Scope{QueueID: &call.QueueID})
	c.publishQueueCount(ctx, call.QueueID, call.QueueName)
}

// publishQueueCount announces the depth of one queue after it moved.
func (c *Coordinator) publishQueueCount(ctx context.Context, queueID uuid.UUID, queueName string) {
	waiting, longest := c.waiting.count(queueID)
	payload := map[string]any{"queueName": queueName, "waiting": waiting}
	if !longest.IsZero() {
		payload["longestWaitAt"] = longest
	}
	c.publish(ctx, events.Event{
		Type:    events.TypeQueueCount,
		QueueID: &queueID,
		Payload: payload,
	}, events.Scope{QueueID: &queueID})
}

// callIDOrNil keeps an unknown call out of the envelope rather than putting
// the nil UUID on it, which would read as a call that exists.
func callIDOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
