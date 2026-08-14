// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

type noAgents struct{}

func (noAgents) AgentAtExtension(string) (uuid.UUID, bool)      { return uuid.Nil, false }
func (noAgents) AgentByCallcenterName(string) (uuid.UUID, bool) { return uuid.Nil, false }
func (noAgents) SetOnCall(context.Context, uuid.UUID, bool)     {}

type nullPublisher struct{}

func (nullPublisher) Publish(_ context.Context, ev events.Event, _ events.Scope) events.Event {
	return ev
}

// raw builds a channel event the way FreeSWITCH sends it, so these tests walk
// through Normalize like production events do.
func raw(name, channelID, direction string, extra map[string]string) SwitchEvent {
	headers := map[string]string{
		"Event-Name":                name,
		"Unique-ID":                 channelID,
		"Call-Direction":            direction,
		"Caller-Caller-ID-Number":   "13800138000",
		"Caller-Destination-Number": "95012",
		"Hangup-Cause":              "NORMAL_CLEARING",
	}
	for k, v := range extra {
		headers[k] = v
	}
	ev, ok := Normalize(esl.NewEvent(headers, ""))
	if !ok {
		panic("unnormalizable test event: " + name)
	}
	return ev
}

// finishedCollector records every finished call the registry reports.
type finishedCollector struct {
	mu    sync.Mutex
	snaps []Snapshot
}

func (f *finishedCollector) add(s Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps = append(f.snaps, s)
}

func (f *finishedCollector) all() []Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Snapshot(nil), f.snaps...)
}

// The live sequence of an AI call, captured from a real switch: the caller's
// channel is created before the dialplan mints the call id, so it is adopted
// provisionally; the id then arrives on CHANNEL_ANSWER, and the bot leg is
// created already carrying it. One conversation must leave exactly one
// finished call under the minted identity — the provisional duplicate was
// worth one phantom CDR per call in production.
func TestAProvisionalCallCollapsesIntoItsMintedIdentity(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	finished := &finishedCollector{}
	registry.OnCallFinished = finished.add
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})

	ctx := t.Context()
	minted := uuid.New().String()
	callerChan := "caller-chan"
	botChan := "bot-chan"
	mintedVars := map[string]string{
		"variable_aicc_call_id": minted,
		"variable_aicc_did":     "95012",
	}

	// The caller arrives before the dialplan has run: no variables yet.
	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", nil))
	if registry.Count() != 1 {
		t.Fatalf("after the caller's create, %d calls live", registry.Count())
	}

	// The dialplan answered and minted the identity. The absorbed actor
	// retires asynchronously, so the count settles rather than snaps.
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", mintedVars))
	waitFor(t, func() bool { return registry.Count() == 1 })
	boundTo, ok := registry.CallForChannel(callerChan)
	if !ok || boundTo.String() != minted {
		t.Fatalf("the caller's channel is bound to %s, want the minted id %s", boundTo, minted)
	}

	// The switch dials the bot; the exported variables ride the new leg.
	c.Handle(ctx, raw("CHANNEL_CREATE", botChan, "outbound", mintedVars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", botChan, "outbound", mintedVars))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", botChan, "outbound",
		merged(mintedVars, map[string]string{"Other-Leg-Unique-ID": callerChan})))
	waitFor(t, func() bool { return registry.Count() == 1 })

	// Both legs end; exactly one call finishes, under the minted identity.
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", botChan, "outbound", mintedVars))
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", callerChan, "inbound", mintedVars))

	waitFor(t, func() bool { return len(finished.all()) == 1 })
	snap := finished.all()[0]
	if snap.CallID.String() != minted {
		t.Errorf("the finished call is %s, want the minted id", snap.CallID)
	}
	if len(snap.Parties) != 2 {
		t.Fatalf("the finished call has %d parties, want the caller and the bot leg", len(snap.Parties))
	}
	if !hasBotLeg(snap) {
		t.Error("no party is marked as the bot leg")
	}
	if o := snap.Parties[0]; o.Number != "13800138000" && snap.Parties[1].Number != "13800138000" {
		t.Errorf("the caller's number was lost in the merge: %+v", snap.Parties)
	}
}

// A scripted call's scaffolding leg must never become a call of its own —
// while the loopback half playing the caller, which inherits the same
// variable, must still be tracked.
func TestHarnessLegsNeverBecomeCalls(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})

	harness := map[string]string{"variable_aicc_harness": "true"}
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "chan-loop-a", "outbound",
		merged(harness, map[string]string{"Channel-Name": "loopback/95012-a"})))
	if registry.Count() != 0 {
		t.Fatalf("a harness leg became %d call(s)", registry.Count())
	}

	c.Handle(t.Context(), raw("CHANNEL_CREATE", "chan-loop-b", "inbound",
		merged(harness, map[string]string{"Channel-Name": "loopback/95012-b"})))
	if registry.Count() != 1 {
		t.Fatalf("the caller half of a scripted call was not tracked: %d calls", registry.Count())
	}
}

func merged(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}
