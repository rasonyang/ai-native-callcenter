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

func (noAgents) AgentAtExtension(string) (uuid.UUID, bool)                { return uuid.Nil, false }
func (noAgents) AgentByCallcenterName(string) (uuid.UUID, bool)           { return uuid.Nil, false }
func (noAgents) SetOnCall(context.Context, uuid.UUID, bool)               {}
func (noAgents) BeginAfterCallWork(context.Context, uuid.UUID, uuid.UUID) {}

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

// oneAgent is a directory with a single signed-in agent, so a leg dialed at
// their extension is recognised as a delivery rather than a new call.
var testAgentID = uuid.New()

const agentExtension = "1001"

type oneAgent struct{}

func (oneAgent) AgentAtExtension(ext string) (uuid.UUID, bool) {
	if ext == agentExtension {
		return testAgentID, true
	}
	return uuid.Nil, false
}
func (oneAgent) AgentByCallcenterName(string) (uuid.UUID, bool)           { return uuid.Nil, false }
func (oneAgent) SetOnCall(context.Context, uuid.UUID, bool)               {}
func (oneAgent) BeginAfterCallWork(context.Context, uuid.UUID, uuid.UUID) {}

// recordingTapper captures what the coordinator asked of the media tap.
type recordingTapper struct {
	mu           sync.Mutex
	attached     []string
	paused       []string
	resumed      []string
	detached     []string
	detachedCall []uuid.UUID
	agents       map[string]uuid.UUID
	languages    map[string]string
	// tapped is the set of channels this fake believes it has a tap on, so it
	// can answer ErrNoTap the way the real one does.
	tapped map[string]bool
}

func newRecordingTapper() *recordingTapper {
	return &recordingTapper{agents: map[string]uuid.UUID{}, languages: map[string]string{},
		tapped: map[string]bool{}}
}

func (r *recordingTapper) Attach(_ uuid.UUID, agentID, _ *uuid.UUID, channelID, language string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attached = append(r.attached, channelID)
	r.tapped[channelID] = true
	r.languages[channelID] = language
	if agentID != nil {
		r.agents[channelID] = *agentID
	}
}
func (r *recordingTapper) Pause(c string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.tapped[c] {
		return ErrNoTap
	}
	r.paused = append(r.paused, c)
	return nil
}

func (r *recordingTapper) Resume(c string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.tapped[c] {
		return ErrNoTap
	}
	r.resumed = append(r.resumed, c)
	return nil
}

func (r *recordingTapper) Detach(c string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.tapped[c] {
		return
	}
	delete(r.tapped, c)
	r.detached = append(r.detached, c)
}

func (r *recordingTapper) DetachCall(callID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detachedCall = append(r.detachedCall, callID)
}

func (r *recordingTapper) callsDetached() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.detachedCall...)
}

func (r *recordingTapper) snapshot() ([]string, []string, []string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.attached...), append([]string(nil), r.paused...),
		append([]string(nil), r.resumed...), append([]string(nil), r.detached...)
}

// The tap goes on the agent's leg and never the caller's. That is the whole
// attribution model: the agent leg's lifetime is exactly the human phase, and
// it carries one known agent, so a line's speaker is structural rather than
// inferred from whoever happens to be bridged.
func TestTheTapGoesOnTheAgentLegAtTheBridge(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	taps := newRecordingTapper()
	c.AttachTaps(taps)

	ctx := t.Context()
	minted := uuid.New().String()
	callerChan, agentChan := "caller-chan", "agent-chan"
	// The dialplan stamps the language alongside the call id, as it does live.
	vars := map[string]string{"variable_aicc_call_id": minted, "variable_aicc_language": "zh"}

	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", vars))

	// The agent's leg arrives the way a queue delivery does: its own channel,
	// dialed at the agent's extension, with no call id of its own.
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", agentChan, "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": callerChan})))

	waitFor(t, func() bool {
		attached, _, _, _ := taps.snapshot()
		return len(attached) > 0
	})
	attached, _, _, _ := taps.snapshot()
	for _, ch := range attached {
		if ch == callerChan {
			t.Errorf("the tap went on the caller's leg (%s); every line would then need a "+
				"who-is-bridged-now lookup", ch)
		}
	}
	if len(attached) != 1 || attached[0] != agentChan {
		t.Fatalf("attached to %v, want only the agent's leg %s", attached, agentChan)
	}
	if taps.agents[agentChan] != testAgentID {
		t.Errorf("the tap carries agent %s, want %s", taps.agents[agentChan], testAgentID)
	}
	// A recogniser given no language hint still works and is quietly worse, so
	// the call's own language has to reach it.
	if taps.languages[agentChan] != "zh" {
		t.Errorf("the tap carries language %q, want zh — the call's own, not the "+
			"deployment's; a recogniser left to guess is quietly worse, not broken",
			taps.languages[agentChan])
	}
}

// bridgeAnAgentLeg drives the coordinator through a queue delivery until the
// agent's own leg is tapped, and returns that channel.
func bridgeAnAgentLeg(t *testing.T, c *Coordinator, taps *recordingTapper) string {
	t.Helper()
	ctx := t.Context()
	minted := uuid.New().String()
	callerChan, agentChan := "caller-chan", "agent-chan"
	vars := map[string]string{"variable_aicc_call_id": minted}

	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", agentChan, "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": callerChan})))

	waitFor(t, func() bool {
		attached, _, _, _ := taps.snapshot()
		return len(attached) > 0
	})
	return agentChan
}

// Hold on a leg nobody is transcribing — the caller's own, every time — is
// ordinary, and must not turn into a command against a stream that is not
// there. This is the half of "no silent action on a dead tap" that stays
// silent: the tap says ErrNoTap and the coordinator, which knows that the
// caller's leg is never tapped, says nothing further.
func TestHoldOnAnUntappedLegCommandsNothing(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	taps := newRecordingTapper()
	c.AttachTaps(taps)

	ctx := t.Context()
	_ = bridgeAnAgentLeg(t, c, taps)
	vars := map[string]string{"variable_aicc_call_id": uuid.New().String()}
	c.Handle(ctx, raw("CHANNEL_HOLD", "caller-chan", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_UNHOLD", "caller-chan", "inbound", vars))

	_, paused, resumed, _ := taps.snapshot()
	if len(paused) != 0 || len(resumed) != 0 {
		t.Errorf("paused = %v, resumed = %v, want neither on a leg with no tap", paused, resumed)
	}
}

// Hold is a private side-conversation and music, neither of which belongs in a
// transcript of this call. It is also the case where a stereo stream delivers
// nothing at all rather than silence, so pausing is what makes the gap
// deliberate instead of mysterious.
func TestHoldPausesTheTapAndUnholdResumesIt(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	taps := newRecordingTapper()
	c.AttachTaps(taps)

	ctx := t.Context()
	// The tap has to exist before hold means anything. This used to run
	// against a coordinator that had never attached one, and passed, because
	// the tap swallowed a pause on a channel it did not hold — the swallow was
	// the whole of what the test proved.
	agentChan := bridgeAnAgentLeg(t, c, taps)
	vars := map[string]string{"variable_aicc_call_id": uuid.New().String()}
	c.Handle(ctx, raw("CHANNEL_HOLD", agentChan, "outbound", vars))
	c.Handle(ctx, raw("CHANNEL_UNHOLD", agentChan, "outbound", vars))
	c.Handle(ctx, raw("CHANNEL_UNBRIDGE", agentChan, "outbound", vars))

	_, paused, resumed, detached := taps.snapshot()
	if len(paused) != 1 || paused[0] != "agent-chan" {
		t.Errorf("paused = %v, want the agent's channel once", paused)
	}
	if len(resumed) != 1 || resumed[0] != "agent-chan" {
		t.Errorf("resumed = %v, want the agent's channel once", resumed)
	}
	if len(detached) != 1 || detached[0] != "agent-chan" {
		t.Errorf("detached = %v, want the agent's channel once", detached)
	}
}

// Transcription off must leave the telephony path byte-identical.
func TestNoTapperMeansNoTranscriptionPath(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})

	ctx := t.Context()
	vars := map[string]string{"variable_aicc_call_id": uuid.New().String()}
	c.Handle(ctx, raw("CHANNEL_CREATE", "a", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_HOLD", "a", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", "a", "inbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": "b"})))
	// Reaching here without a nil dereference is the assertion.
}

// recordingAudiences captures who the coordinator said may see a transcript.
type recordingAudiences struct {
	mu  sync.Mutex
	set map[uuid.UUID][]uuid.UUID
}

func newRecordingAudiences() *recordingAudiences {
	return &recordingAudiences{set: map[uuid.UUID][]uuid.UUID{}}
}

func (r *recordingAudiences) SetAudience(callID uuid.UUID, agentIDs []uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set[callID] = append([]uuid.UUID(nil), agentIDs...)
}

func (r *recordingAudiences) forCall(callID uuid.UUID) ([]uuid.UUID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	got, ok := r.set[callID]
	return got, ok
}

// Who may see a call's live transcript comes from who is on the call, and only
// this side knows that. The alternative is what shipped: nobody ever said, the
// transcript actor's scope stayed empty, and the hub reads an empty scope as an
// unscoped system notice bound for every subscriber.
func TestAnAgentLegAddressesTheTranscriptToThatAgent(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	audiences := newRecordingAudiences()
	c.AttachAudiences(audiences)

	ctx := t.Context()
	minted := uuid.New()
	vars := map[string]string{"variable_aicc_call_id": minted.String()}

	// The bot phase: a caller and nobody else.
	c.Handle(ctx, raw("CHANNEL_CREATE", "caller-chan", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", "caller-chan", "inbound", vars))
	if got, ok := audiences.forCall(minted); ok && len(got) != 0 {
		t.Fatalf("the bot phase was addressed to %v, want nobody", got)
	}

	// The agent's leg arrives the way a queue delivery does — its own channel,
	// no call id of its own — and becomes part of this conversation at the
	// bridge, which is also where the two calls are folded into one.
	c.Handle(ctx, raw("CHANNEL_CREATE", "agent-chan", "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", "agent-chan", "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": "caller-chan"})))

	waitFor(t, func() bool {
		got, ok := audiences.forCall(minted)
		return ok && len(got) == 1
	})
	got, _ := audiences.forCall(minted)
	if len(got) != 1 || got[0] != testAgentID {
		t.Fatalf("addressed to %v, want the agent on the call (%s)", got, testAgentID)
	}
}

// Nothing attached is nothing driven: transcription off must leave this path
// as absent as the tap's.
func TestNoAudiencesMeansNoAddressing(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})

	ctx := t.Context()
	vars := map[string]string{"variable_aicc_call_id": uuid.New().String()}
	c.Handle(ctx, raw("CHANNEL_CREATE", "caller-chan", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_CREATE", "agent-chan", "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	// Reaching here without a nil dereference is the assertion.
}

// An agent whose leg is folded into another call must be told, or their client
// keeps a call id that no longer exists.
//
// Observed live on 2026-08-18. The agent's leg was adopted as its own OUTBOUND
// call, the bridge merged it into the INBOUND one, and every CALL_TRANSCRIPT
// afterwards carried the kept id while the cockpit was still holding the
// absorbed one — so the panel dropped every line as belonging to another call.
// The transcript was published correctly and shown to nobody. The merge was
// silent: the absorbed call is retired without a word and nothing is published
// for the kept call after it.
func TestAMergeTellsTheMovedAgentTheCallHasANewIdentity(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	pub := &capturingPublisher{}
	c := NewCoordinator(registry, nil, oneAgent{}, pub)

	ctx := t.Context()
	minted := uuid.New()
	vars := map[string]string{"variable_aicc_call_id": minted.String()}

	c.Handle(ctx, raw("CHANNEL_CREATE", "caller-chan", "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", "caller-chan", "inbound", vars))
	// The agent's leg arrives with no call id of its own and is adopted onto a
	// provisional call, exactly as a queue delivery does.
	c.Handle(ctx, raw("CHANNEL_CREATE", "agent-chan", "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", "agent-chan", "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": "caller-chan"})))

	waitFor(t, func() bool { return pub.has(events.TypePartyChanged) })

	ev, scope, ok := pub.find(events.TypePartyChanged)
	if !ok {
		t.Fatal("the merge was silent; the agent still holds the absorbed call id")
	}
	if ev.CallID == nil || *ev.CallID != minted {
		t.Errorf("announced call %v, want the surviving call %s", ev.CallID, minted)
	}
	if len(scope.AgentIDs) != 1 || scope.AgentIDs[0] != testAgentID {
		t.Errorf("addressed to %v, want the agent whose leg moved", scope.AgentIDs)
	}
	if ev.Payload["reason"] != "CALL_MERGED" {
		t.Errorf("reason = %v, want CALL_MERGED", ev.Payload["reason"])
	}
}

// capturingPublisher keeps what was published and under which scope.
type capturingPublisher struct {
	mu     sync.Mutex
	events []events.Event
	scopes []events.Scope
}

func (p *capturingPublisher) Publish(_ context.Context, ev events.Event, sc events.Scope) events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	p.scopes = append(p.scopes, sc)
	return ev
}

func (p *capturingPublisher) has(t events.Type) bool {
	_, _, ok := p.find(t)
	return ok
}

func (p *capturingPublisher) find(t events.Type) (events.Event, events.Scope, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, ev := range p.events {
		if ev.Type == t {
			return ev, p.scopes[i], true
		}
	}
	return events.Event{}, events.Scope{}, false
}

// A queue delivery leg joins the caller's call the moment it is created, not
// at the bridge. On a call of its own it would be that call's first party —
// ORIGINATOR/DIALING — and the agent's screen would show them dialling the
// caller who is in fact ringing them; a delivery mod_callcenter cancels before
// it answers never bridges at all, so the stray call reached the ledger as an
// outbound CDR with caller and agent reversed, one per retry.
func TestQueueDeliveryLegJoinsTheCallerImmediately(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})

	ctx := t.Context()
	minted := uuid.New().String()
	callerChan, agentChan := "caller-chan", "agent-chan"
	// Context "public" is how a call off the carrier arrives, which is what
	// makes this an INBOUND call rather than an extension calling an extension.
	vars := map[string]string{"variable_aicc_call_id": minted, "Caller-Context": "public"}

	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", vars))

	// mod_callcenter dials the agent and stamps the waiting caller's channel.
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound", map[string]string{
		"variable_dialed_user":            agentExtension,
		"variable_cc_side":                "agent",
		"variable_cc_member_session_uuid": callerChan,
	}))

	callID, ok := registry.CallForChannel(agentChan)
	if !ok {
		t.Fatal("the delivery leg is bound to no call")
	}
	callerID, ok := registry.CallForChannel(callerChan)
	if !ok {
		t.Fatal("the caller is bound to no call")
	}
	if callID != callerID {
		t.Fatalf("delivery leg is on call %s, want the caller's %s", callID, callerID)
	}

	var role PartyRole
	var state PartyState
	var callType events.CallType
	if err := registry.Do(callID, func(call *Call) {
		callType = call.CallType
		if p := call.PartyByChannel(agentChan); p != nil {
			role, state = p.Role, p.State
		}
	}); err != nil {
		t.Fatalf("reading the call: %v", err)
	}
	if role != RoleTarget {
		t.Errorf("delivery leg role = %q, want TARGET — the agent is being offered the call, not placing it", role)
	}
	if state != PartyRinging {
		t.Errorf("delivery leg state = %q, want RINGING", state)
	}
	// The caller's inbound classification is not restated by the leg the
	// switch dials outward to reach the agent.
	if callType != events.CallTypeInbound {
		t.Errorf("CallType = %q, want INBOUND", callType)
	}
}
