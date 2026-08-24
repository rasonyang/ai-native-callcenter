// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
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
func (noAgents) BenchForNoAnswer(context.Context, uuid.UUID)              {}

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
func (oneAgent) AgentByCallcenterName(string) (uuid.UUID, bool) { return uuid.Nil, false }

// twoAgents tells the two ends of an internal call apart: an agent owns one
// extension, so a leg at somebody else's extension is not theirs.
const otherExtension = "1002"

var otherAgentID = uuid.MustParse("00000000-0000-4000-8000-00000000a002")

type twoAgents struct{}

func (twoAgents) AgentAtExtension(ext string) (uuid.UUID, bool) {
	switch ext {
	case agentExtension:
		return testAgentID, true
	case otherExtension:
		return otherAgentID, true
	}
	return uuid.Nil, false
}
func (twoAgents) AgentByCallcenterName(string) (uuid.UUID, bool)           { return uuid.Nil, false }
func (twoAgents) SetOnCall(context.Context, uuid.UUID, bool)               {}
func (twoAgents) BeginAfterCallWork(context.Context, uuid.UUID, uuid.UUID) {}
func (twoAgents) BenchForNoAnswer(context.Context, uuid.UUID)              {}
func (oneAgent) SetOnCall(context.Context, uuid.UUID, bool)                {}
func (oneAgent) BeginAfterCallWork(context.Context, uuid.UUID, uuid.UUID)  {}
func (oneAgent) BenchForNoAnswer(context.Context, uuid.UUID)               {}

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

// The same bridge, with the delivery leg shaped the way mod_callcenter really
// sends it: carrying the caller's channel, so the leg is bound to the caller's
// call the moment it is created and there is nothing left to merge.
//
// That binding landed on 2026-08-21, one day after VC-S9-01 last ran, and the
// tap lived inside the merge branch. For two days every queue-delivered call
// went untranscribed and the test above kept passing, because it models a
// delivery leg with no member pointer — the shape production stopped sending.
func TestTheTapGoesOnEvenWhenThereIsNothingToMerge(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	taps := newRecordingTapper()
	c.AttachTaps(taps)
	audiences := newRecordingAudiences()
	c.AttachAudiences(audiences)

	ctx := t.Context()
	minted := uuid.New().String()
	callerChan, agentChan := "caller-chan", "agent-chan"
	vars := map[string]string{"variable_aicc_call_id": minted, "variable_aicc_language": "en"}

	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", vars))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", vars))

	// mod_callcenter stamps the member's channel onto the leg it dials, which
	// is what binds the two before any bridge.
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound", map[string]string{
		"variable_dialed_user":            agentExtension,
		"variable_cc_member_session_uuid": callerChan,
	}))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", agentChan, "outbound",
		merged(vars, map[string]string{"Other-Leg-Unique-ID": callerChan})))

	waitFor(t, func() bool {
		attached, _, _, _ := taps.snapshot()
		return len(attached) > 0
	})
	attached, _, _, _ := taps.snapshot()
	if len(attached) != 1 || attached[0] != agentChan {
		t.Fatalf("attached to %v, want the agent's leg %s — a bridge that needed no "+
			"merge is still a bridge, and the human phase still needs transcribing",
			attached, agentChan)
	}
	if taps.agents[agentChan] != testAgentID {
		t.Errorf("the tap carries agent %s, want %s", taps.agents[agentChan], testAgentID)
	}
	// And the audience, for the same reason at the same moment: the transcript
	// scopes itself to the agents on the call, so an unannounced audience
	// means every line is written to the database and published to nobody.
	// The panel stays empty for the whole conversation and nothing errors.
	got, ok := audiences.forCall(uuid.MustParse(minted))
	if !ok || len(got) != 1 || got[0] != testAgentID {
		t.Errorf("audience = %v (announced=%v), want just %s — a transcript with "+
			"no audience never reaches the agent it is about", got, ok, testAgentID)
	}
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
// One extension calling another: the same shape as a queue delivery, but
// mod_callcenter knows nothing about it, so the pointer back to the first leg
// comes from our own dialplan instead. It cannot be a call id — the caller's
// CHANNEL_CREATE reaches us before the dialplan runs, so whichever leg the
// identity is minted on, the other has already been adopted on its own.
//
// Without the pointer the second leg is that new call's first party and comes
// out ORIGINATOR/DIALING, which is what the agent being rung was shown: their
// caller's leg, labelled "Calling out", on their own screen.
// A caller's own leg belongs to the caller, not to the person they dialled.
//
// The switch's destination for a leg identifies an agent only when the switch
// raised that leg towards them. A phone-originated leg carries the number it is
// calling, so matching on it gave both legs of an internal call the same agent
// id — and the cockpit, taking the first party with an id as its own, showed
// the agent being rung their caller's leg: Calling out, dialling, on a screen
// whose phone was ringing.
// An agent works one leg, so their stream is about that leg. On a call between
// two agents each of them should see their own party ring, establish and
// release — not six events about both of them.
//
// The exception is a party with no agent: the caller's leg on an inbound call
// belongs to nobody, and scoping it to its own agent would address the
// customer's hangup to nobody at all.
func TestALegEventGoesToTheAgentWhoseLegItIs(t *testing.T) {
	pub := &capturingPublisher{}
	registry := NewRegistry(pub)
	c := NewCoordinator(registry, nil, twoAgents{}, pub)

	ctx := t.Context()
	callerChan, calleeChan := "caller-chan", "callee-chan"
	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", map[string]string{
		"Caller-Destination-Number": agentExtension,
		"Caller-Caller-ID-Number":   otherExtension,
		"Caller-Context":            "aicc",
	}))
	c.Handle(ctx, raw("CHANNEL_CREATE", calleeChan, "outbound", map[string]string{
		"Caller-Destination-Number":    agentExtension,
		"variable_aicc_parent_channel": callerChan,
		"Caller-Context":               "aicc",
	}))
	c.Handle(ctx, raw("CHANNEL_ANSWER", calleeChan, "outbound", map[string]string{
		"Caller-Destination-Number":    agentExtension,
		"variable_aicc_parent_channel": callerChan,
	}))
	// Answering is not joining: the leg is established when the two are
	// bridged, which is the event that carries PARTY_ESTABLISHED.
	c.Handle(ctx, raw("CHANNEL_BRIDGE", calleeChan, "outbound", map[string]string{
		"Caller-Destination-Number": agentExtension,
		"Other-Leg-Unique-ID":       callerChan,
	}))

	// Parties publish from their call's own goroutine, so wait rather than read.
	deadline := time.After(2 * time.Second)
	for {
		pub.mu.Lock()
		var scope *events.Scope
		for i, ev := range pub.events {
			if ev.Type == events.TypePartyEstablished && ev.AgentID != nil && *ev.AgentID == testAgentID {
				sc := pub.scopes[i]
				scope = &sc
			}
		}
		pub.mu.Unlock()
		if scope != nil {
			if len(scope.AgentIDs) != 1 || scope.AgentIDs[0] != testAgentID {
				t.Errorf("the established event for the agent's own leg is addressed to %v, want only %s — a colleague's leg is not their business",
					scope.AgentIDs, testAgentID)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("no PARTY_ESTABLISHED naming that agent was published")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestACallersOwnLegIsNotAttributedToWhoTheyDialled(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, twoAgents{}, nullPublisher{})

	ctx := t.Context()
	callerChan, calleeChan := "caller-chan", "callee-chan"

	// One agent dials the other. The caller's leg is inbound: they raised it,
	// it sits at their own extension, and its destination is the colleague.
	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", map[string]string{
		"Caller-Destination-Number": agentExtension,
		"Caller-Caller-ID-Number":   otherExtension,
		"Caller-Context":            "aicc",
	}))
	// The leg the dialplan raises towards that agent is outbound, and its
	// destination does identify them.
	c.Handle(ctx, raw("CHANNEL_CREATE", calleeChan, "outbound", map[string]string{
		"Caller-Destination-Number":    agentExtension,
		"variable_aicc_parent_channel": callerChan,
		"Caller-Context":               "aicc",
	}))

	callID, ok := registry.CallForChannel(callerChan)
	if !ok {
		t.Fatal("the caller is bound to no call")
	}
	var callerAgent, calleeAgent *uuid.UUID
	if err := registry.Do(callID, func(call *Call) {
		if p := call.PartyByChannel(callerChan); p != nil {
			callerAgent = p.AgentID
		}
		if p := call.PartyByChannel(calleeChan); p != nil {
			calleeAgent = p.AgentID
		}
	}); err != nil {
		t.Fatalf("reading the call: %v", err)
	}
	if calleeAgent == nil || *calleeAgent != testAgentID {
		t.Errorf("the leg dialled towards the agent is attributed to %v, want the agent at that extension", calleeAgent)
	}
	// An agent owns one extension. The caller is sitting at theirs, not at the
	// one they dialled, so the call belongs on both screens — as the placer on
	// one and the person being rung on the other.
	if callerAgent == nil || *callerAgent != otherAgentID {
		t.Errorf("the caller's own leg is attributed to %v, want the agent sitting at %s", callerAgent, otherExtension)
	}
	if callerAgent != nil && calleeAgent != nil && *callerAgent == *calleeAgent {
		t.Error("both legs carry one agent id; the cockpit cannot tell which party is its own")
	}
}

func TestTheSecondLegOfAnInternalCallJoinsTheFirst(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})

	ctx := t.Context()
	callerChan, calleeChan := "caller-chan", "callee-chan"

	// The caller's leg arrives before the dialplan has run: no aicc variable
	// on it at all, which is the whole reason a call id cannot do this job.
	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", map[string]string{
		"Caller-Context": "aicc",
	}))

	// The dialplan bridges, and the leg it raises names the channel it was
	// raised for. export also puts the value on the caller's own leg, where it
	// equals that leg's own id and is therefore ignored.
	c.Handle(ctx, raw("CHANNEL_CREATE", calleeChan, "outbound", map[string]string{
		"variable_dialed_user":         agentExtension,
		"variable_aicc_parent_channel": callerChan,
		"variable_aicc_call_type":      "INTERNAL",
		"Caller-Context":               "aicc",
	}))

	calleeCall, ok := registry.CallForChannel(calleeChan)
	if !ok {
		t.Fatal("the called leg is bound to no call")
	}
	callerCall, ok := registry.CallForChannel(callerChan)
	if !ok {
		t.Fatal("the caller is bound to no call")
	}
	if calleeCall != callerCall {
		t.Fatalf("the two legs are on separate calls (%s and %s); one call was dialled, not two",
			calleeCall, callerCall)
	}

	var calleeRole, callerRole PartyRole
	var calleeState, callerState PartyState
	if err := registry.Do(callerCall, func(call *Call) {
		if p := call.PartyByChannel(calleeChan); p != nil {
			calleeRole, calleeState = p.Role, p.State
		}
		if p := call.PartyByChannel(callerChan); p != nil {
			callerRole, callerState = p.Role, p.State
		}
	}); err != nil {
		t.Fatalf("reading the call: %v", err)
	}
	if callerRole != RoleOriginator || callerState != PartyDialing {
		t.Errorf("caller = %s/%s, want ORIGINATOR/DIALING — they placed the call", callerRole, callerState)
	}
	if calleeRole != RoleTarget || calleeState != PartyRinging {
		t.Errorf("callee = %s/%s, want TARGET/RINGING — their phone is the one ringing", calleeRole, calleeState)
	}
}

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

// The whole shape of the loss C11 recorded: on a transferred call the bot's
// leg hangs up first, carrying only the DID the dialplan exported to it, and
// the caller's leg delivers the bot's actual tally a conversation later. The
// call must end up with both halves.
func TestTheBotShareSurvivesTheLegThatHangsUpFirst(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	finished := &finishedCollector{}
	registry.OnCallFinished = finished.add
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})

	ctx := t.Context()
	minted := uuid.New().String()
	flowID := uuid.New()
	callerChan, botChan := "caller-chan", "bot-chan"
	// aicc_inbound.lua exports aicc_call_id, aicc_did and aicc_language, so
	// the leg dialled towards the bot answers for the DID and nothing else.
	exported := map[string]string{
		"variable_aicc_call_id":  minted,
		"variable_aicc_did":      "95001",
		"variable_aicc_language": "en",
		"Caller-Context":         "public",
	}

	c.Handle(ctx, raw("CHANNEL_CREATE", callerChan, "inbound", exported))
	c.Handle(ctx, raw("CHANNEL_ANSWER", callerChan, "inbound", exported))
	c.Handle(ctx, raw("CHANNEL_CREATE", botChan, "outbound", exported))
	c.Handle(ctx, raw("CHANNEL_ANSWER", botChan, "outbound", exported))

	// The bot transfers the caller and closes its own leg immediately.
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", botChan, "outbound", exported))

	// A conversation later the caller hangs up, carrying what the bot stamped.
	c.Handle(ctx, raw("CHANNEL_HANGUP_COMPLETE", callerChan, "inbound",
		merged(exported, map[string]string{
			"variable_aicc_bot_sec":     "42",
			"variable_aicc_flow_id":     flowID.String(),
			"variable_aicc_bot_summary": "billing question",
			"variable_aicc_bot_reason":  "AGENT_REQUESTED",
		})))

	waitFor(t, func() bool { return len(finished.all()) == 1 })
	snap := finished.all()[0]

	if snap.Bot.Sec != 42 {
		t.Errorf("Bot.Sec = %d, want 42 — the bot leg's DID-only share shut this out", snap.Bot.Sec)
	}
	if snap.Bot.FlowID == nil || *snap.Bot.FlowID != flowID {
		t.Errorf("Bot.FlowID = %v, want %v", snap.Bot.FlowID, flowID)
	}
	if snap.Bot.Summary != "billing question" {
		t.Errorf("Bot.Summary = %q, want the summary stamped for the agent", snap.Bot.Summary)
	}
	if snap.Bot.DID != "95001" {
		t.Errorf("Bot.DID = %q, want 95001", snap.Bot.DID)
	}
}

// A browser softphone registers under a random contact user, so the switch's
// destination for a leg dialled at it is a token like "g7bih4lv". The leg is
// still attributed to the right agent — the directory's dialed_user says so —
// but the event that puts the call on their screen was quoting the token back
// at them as the extension they were being rung at (found live, 2026-08-20).
func TestARingingLegNamesTheExtensionNotTheContactToken(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	pub := &capturingPublisher{}
	c := NewCoordinator(registry, nil, oneAgent{}, pub)

	ctx := t.Context()
	// A leg being rung, which is a delivery: the caller exists first and the
	// agent's leg names them. A lone agent leg is the originator of its own
	// call and is dialling, not ringing — that is PARTY_DIALING's subject.
	c.Handle(ctx, raw("CHANNEL_CREATE", "caller-chan", "inbound",
		map[string]string{"variable_aicc_call_id": uuid.New().String()}))
	c.Handle(ctx, raw("CHANNEL_CREATE", "agent-chan", "outbound", map[string]string{
		// What a browser phone's leg actually looks like: the destination is
		// the registration token, and only dialed_user names the extension.
		"Caller-Destination-Number":       "g7bih4lv",
		"variable_dialed_user":            agentExtension,
		"variable_cc_member_session_uuid": "caller-chan",
	}))

	waitFor(t, func() bool { return pub.has(events.TypePartyRinging) })

	ev, _, _ := pub.find(events.TypePartyRinging)
	for _, field := range []string{"extensionNumber", "toNumber"} {
		got, _ := ev.Payload[field].(string)
		if got != agentExtension {
			t.Errorf("payload.%s = %q, want the agent's extension %q — an agent told they are "+
				"being rung at a registration token has been told nothing", field, got, agentExtension)
		}
	}
}

// One extension calling another is two people on a line, not a call being
// handled: there is no third party to pass it to and no queue to put it back
// into. Owner's ruling (2026-08-20) is that the three controls are refused,
// and the refusal happens before the switch is touched.
func TestInternalCallsRefuseTheControlsThatMeanNothingOnThem(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	ctx := t.Context()

	agentID := testAgentID
	internalID := uuid.New()
	call, err := registry.CreateCall(ctx, internalID, events.CallTypeInternal, "en", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = registry.Do(internalID, func(call *Call) {
		p := call.AddParty("caller-chan", "1007", at(0))
		p.State = PartyTalking
		a := call.AddParty("agent-chan", "1008", at(1))
		a.AgentID = &agentID
		a.State = PartyTalking
	})
	_ = call

	for name, op := range map[string]func() error{
		"hold":     func() error { return c.Hold(ctx, internalID, agentID) },
		"retrieve": func() error { return c.Retrieve(ctx, internalID, agentID) },
		"transfer": func() error { return c.Transfer(ctx, internalID, agentID, "1009") },
	} {
		if err := op(); !errors.Is(err, ErrNotForCallType) {
			t.Errorf("%s on an internal call returned %v, want ErrNotForCallType", name, err)
		}
	}

	// An inbound call is not refused for this reason. Given one with nothing
	// to act on, the refusal that comes back is about the call's own state —
	// which is how we know the type check let it through rather than that the
	// operation happened to fail.
	inboundID := uuid.New()
	if _, err := registry.CreateCall(ctx, inboundID, events.CallTypeInbound, "en", true); err != nil {
		t.Fatal(err)
	}
	_ = registry.Do(inboundID, func(call *Call) {
		p := call.AddParty("caller-2", "18688886669", at(0))
		p.State = PartyTalking
	})
	switch err := c.Hold(ctx, inboundID, agentID); {
	case errors.Is(err, ErrNotForCallType):
		t.Error("an inbound call was refused as though it were internal")
	case !errors.Is(err, ErrNoAgentLeg):
		t.Errorf("hold on an agentless inbound call returned %v, want ErrNoAgentLeg", err)
	}
}

// Having once had a leg on a call is not the same as being on it. After a
// transfer the first agent's leg is released and the conversation is somebody
// else's, but the call stayed on their screen until the whole thing ended —
// showing them a call they had passed on, with controls for a leg the switch
// had already hung up (found live, 2026-08-21).
func TestACallPassedOnLeavesTheFirstAgentsScreen(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})
	ctx := t.Context()

	wei, ben := uuid.New(), uuid.New()
	callID := uuid.New()
	if _, err := registry.CreateCall(ctx, callID, events.CallTypeInbound, "en", true); err != nil {
		t.Fatal(err)
	}
	_ = registry.Do(callID, func(call *Call) {
		caller := call.AddParty("caller", "18688886669", at(0))
		caller.State = PartyTalking
		first := call.AddParty("wei-chan", "1008", at(1))
		first.AgentID = &wei
		first.State = PartyTalking
	})

	if got := c.CallsForAgent(wei); len(got) != 1 {
		t.Fatalf("wei has %d calls while talking, want 1", len(got))
	}

	// The transfer: wei's leg goes, ben's arrives.
	_ = registry.Do(callID, func(call *Call) {
		call.PartyByChannel("wei-chan").State = PartyReleased
		second := call.AddParty("ben-chan", "1007", at(60))
		second.AgentID = &ben
		second.State = PartyTalking
	})

	if got := c.CallsForAgent(wei); len(got) != 0 {
		t.Errorf("wei still has %d calls after passing the conversation on", len(got))
	}
	if got := c.CallsForAgent(ben); len(got) != 1 {
		t.Errorf("ben has %d calls after taking the conversation over, want 1", len(got))
	}
}

// The signal has to be one our own mirror cannot produce. Status was the first
// choice, and a live run showed why it was wrong: this application mirrors
// presence to the switch with the same word the switch uses to bench an agent,
// the echo comes straight back, and every restart read it as calls the agents
// had ignored.
func TestOnlyACallThatRangOutTakesAnAgentOutOfRouting(t *testing.T) {
	cases := map[string]struct {
		action, cause string
		wantBenched   bool
	}{
		"a delivery that rang out":        {"bridge-agent-fail", "NO_ANSWER", true},
		"a phone that was busy":           {"bridge-agent-fail", "USER_BUSY", false},
		"an offer the queue withdrew":     {"bridge-agent-fail", "ORIGINATOR_CANCEL", false},
		"a status change we mirrored":     {"agent-status-change", "", false},
		"a delivery that failed silently": {"bridge-agent-fail", "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			registry := NewRegistry(nullPublisher{})
			t.Cleanup(registry.Shutdown)
			agents := &presenceCalls{}
			c := NewCoordinator(registry, nil, agents, nullPublisher{})

			headers := map[string]string{
				"Event-Name": "CUSTOM", "Event-Subclass": "callcenter::info",
				"CC-Action": tc.action, "CC-Agent": agentCallcenterName,
				"CC-Queue": "support-zh", "CC-Hangup-Cause": tc.cause,
				"CC-Agent-Status": "On Break",
			}
			ev, ok := Normalize(esl.NewEvent(headers, ""))
			if !ok {
				t.Fatalf("unnormalizable: %v", headers)
			}
			c.Handle(t.Context(), ev)

			agents.mu.Lock()
			benched := len(agents.benched)
			agents.mu.Unlock()
			if (benched > 0) != tc.wantBenched {
				t.Errorf("benched=%d, want benched=%v — %s", benched, tc.wantBenched, name)
			}
		})
	}
}

// The leg that starts a call is created dialling and said so to nobody: on the
// stream a party's first appearance was PARTY_ESTABLISHED, so it arrived
// already talking and the state machine's own starting point was invisible.
// The owner found it by reading a live stream of one extension calling
// another (2026-08-22).
func TestTheLegThatStartsACallAnnouncesThatItIsDialling(t *testing.T) {
	t.Run("an agent placing a call sees their own leg before it is answered", func(t *testing.T) {
		registry := NewRegistry(nullPublisher{})
		t.Cleanup(registry.Shutdown)
		pub := &capturingPublisher{}
		c := NewCoordinator(registry, nil, oneAgent{}, pub)

		// The agent's own leg starts the call, so it is the originator.
		c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound",
			map[string]string{"variable_dialed_user": agentExtension}))

		ev, _, ok := pub.find(events.TypePartyDialing)
		if !ok {
			t.Fatal("nothing announced the dialling leg; the workbench shows nothing " +
				"until the other end picks up")
		}
		if ev.AgentID == nil || *ev.AgentID != testAgentID {
			t.Errorf("agentId = %v, want the agent whose leg it is", ev.AgentID)
		}
		if ev.PartyID == nil {
			t.Error("the event names no party")
		}
	})

	t.Run("it is the agent's own leg and nobody else's", func(t *testing.T) {
		registry := NewRegistry(nullPublisher{})
		t.Cleanup(registry.Shutdown)
		pub := &capturingPublisher{}
		c := NewCoordinator(registry, nil, oneAgent{}, pub)

		c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound",
			map[string]string{"variable_dialed_user": agentExtension}))

		_, scope, ok := pub.find(events.TypePartyDialing)
		if !ok {
			t.Fatal("no PARTY_DIALING")
		}
		if len(scope.AgentIDs) != 1 || scope.AgentIDs[0] != testAgentID {
			t.Errorf("scope = %v, want only the agent whose leg it is — a colleague's "+
				"cockpit cannot tell whose leg it is being shown", scope.AgentIDs)
		}
	})

	t.Run("a caller's own dialling leg reaches no agent", func(t *testing.T) {
		registry := NewRegistry(nullPublisher{})
		t.Cleanup(registry.Shutdown)
		pub := &capturingPublisher{}
		c := NewCoordinator(registry, nil, noAgents{}, pub)

		c.Handle(t.Context(), raw("CHANNEL_CREATE", "caller-chan", "inbound",
			map[string]string{"variable_aicc_call_id": uuid.New().String()}))

		ev, scope, ok := pub.find(events.TypePartyDialing)
		if !ok {
			t.Fatal("the caller's own leg was not announced at all")
		}
		if ev.AgentID != nil {
			t.Errorf("agentId = %v on a leg with no agent", ev.AgentID)
		}
		if len(scope.AgentIDs) != 0 {
			t.Errorf("scope = %v, want nobody — a caller's leg is a supervisor's "+
				"business, not another agent's", scope.AgentIDs)
		}
	})

	// Two live internal calls on 2026-08-23 caught the payload facing the wrong
	// way: 1008 dialling 1002 announced {from:1002, to:1008}, and 1002
	// dialling 1008 announced {from:1002, to:1002}. Both came of borrowing the
	// pair PARTY_RINGING renders, which is the callee's view — who is ringing
	// you — and is backwards on the leg doing the ringing.
	t.Run("it reads from the dialling leg's own point of view", func(t *testing.T) {
		registry := NewRegistry(nullPublisher{})
		t.Cleanup(registry.Shutdown)
		pub := &capturingPublisher{}
		c := NewCoordinator(registry, nil, oneAgent{}, pub)

		c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound",
			map[string]string{
				"variable_dialed_user":      agentExtension,
				"Caller-Caller-ID-Number":   agentExtension,
				"Caller-Destination-Number": "1002",
			}))

		ev, _, ok := pub.find(events.TypePartyDialing)
		if !ok {
			t.Fatal("no PARTY_DIALING")
		}
		if got := ev.Payload["fromNumber"]; got != agentExtension {
			t.Errorf("fromNumber = %v, want the dialling leg's own number %s",
				got, agentExtension)
		}
		if got := ev.Payload["toNumber"]; got != "1002" {
			t.Errorf("toNumber = %v, want the number being dialled — announcing the "+
				"callee as the caller is worse than announcing nothing", got)
		}
	})

	// A browser softphone is dialled at the random contact user it registered
	// under: "doskp0mj" here, verbatim from a live 1008→1002 call. Announcing
	// that as the number being called is a wrong answer wearing the shape of a
	// right one, and the callee's real number arrives moments later anyway.
	t.Run("a registration token is not a number and is left out", func(t *testing.T) {
		got := dialingPayload("1008", "doskp0mj")
		if got["fromNumber"] != "1008" {
			t.Errorf("fromNumber = %v", got["fromNumber"])
		}
		if to, present := got["toNumber"]; present {
			t.Errorf("toNumber = %v; a workbench told the agent is calling %q has "+
				"been told nothing", to, to)
		}
		if withNumber := dialingPayload("1002", "1008"); withNumber["toNumber"] != "1008" {
			t.Errorf("a real destination was dropped: %v", withNumber)
		}
	})

	t.Run("a leg that answers a call is ringing, not dialling", func(t *testing.T) {
		registry := NewRegistry(nullPublisher{})
		t.Cleanup(registry.Shutdown)
		pub := &capturingPublisher{}
		c := NewCoordinator(registry, nil, oneAgent{}, pub)
		minted := uuid.New().String()
		vars := map[string]string{"variable_aicc_call_id": minted}

		c.Handle(t.Context(), raw("CHANNEL_CREATE", "caller-chan", "inbound", vars))
		before := len(pub.events)
		// The delivery leg joins an existing call, so it is not its originator.
		c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound",
			map[string]string{
				"variable_dialed_user":            agentExtension,
				"variable_cc_member_session_uuid": "caller-chan",
			}))

		for _, ev := range pub.events[before:] {
			if ev.Type == events.TypePartyDialing {
				t.Error("the delivery leg announced itself as dialling; the agent is " +
					"being rung, not ringing somebody")
			}
		}
	})
}

// stashedData stands in for the store an outbound request leaves its business
// data in. Keyed by call id, because that is the only thing the switch's own
// event carries back that the request also chose.
type stashedData map[uuid.UUID]map[string]any

func (d stashedData) CallData(callID uuid.UUID) map[string]any { return d[callID] }

// Business data attached when a call is placed has to reach the agent's
// screen, and it does not travel through the switch to get there: it is left
// here when the call is placed and picked up when the call comes into being.
func TestBusinessDataAttachedToAPlacedCallReachesTheAgent(t *testing.T) {
	minted := uuid.New()
	sent := map[string]any{"ticketId": "T-1", "tier": "VIP", "订单": "ORD-9"}

	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	pub := &capturingPublisher{}
	c := NewCoordinator(registry, nil, oneAgent{}, pub)
	c.AttachCallData(stashedData{minted: sent})

	// The customer's leg, minted by the request that placed the call.
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "customer-chan", "outbound",
		map[string]string{"variable_aicc_call_id": minted.String()}))
	// The agent is rung for it, which is the event their screen pops from.
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound", map[string]string{
		"variable_dialed_user":            agentExtension,
		"variable_cc_member_session_uuid": "customer-chan",
	}))

	ev, _, ok := pub.find(events.TypePartyRinging)
	if !ok {
		t.Fatal("the agent was never rung")
	}
	// Non-empty first: two empty maps compare equal, and a test that only
	// compared them would pass while nothing was carried at all.
	if len(ev.UserData) == 0 {
		t.Fatal("the ringing event carried no business data; the screen pops with " +
			"nothing on it but the number")
	}
	if len(ev.UserData) != len(sent) {
		t.Errorf("carried %d keys, sent %d: %v", len(ev.UserData), len(sent), ev.UserData)
	}
	for k, want := range sent {
		if got := ev.UserData[k]; got != want {
			t.Errorf("userData[%q] = %v, want %v", k, got, want)
		}
	}
}

// An inbound call was never placed through this application, so there is
// nothing stashed for it and nothing to pick up. The regression this pins is
// the obvious wrong shape: reading the stash by anything less specific than
// the call's own id — the extension, say — would hand one call's business data
// to another.
func TestAnInboundCallCarriesNoBusinessDataOfItsOwn(t *testing.T) {
	somebodyElse := uuid.New()

	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	pub := &capturingPublisher{}
	c := NewCoordinator(registry, nil, oneAgent{}, pub)
	c.AttachCallData(stashedData{somebodyElse: {"ticketId": "not-yours"}})

	c.Handle(t.Context(), raw("CHANNEL_CREATE", "caller-chan", "inbound",
		map[string]string{"variable_aicc_call_id": uuid.New().String()}))
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound", map[string]string{
		"variable_dialed_user":            agentExtension,
		"variable_cc_member_session_uuid": "caller-chan",
	}))

	ev, _, ok := pub.find(events.TypePartyRinging)
	if !ok {
		t.Fatal("the agent was never rung")
	}
	if len(ev.UserData) != 0 {
		t.Errorf("an inbound call arrived carrying %v — that belongs to another call",
			ev.UserData)
	}
}

// The contract says a call event repeats callType and userData so a screen-pop
// needs no further request. Two PARTY_CHANGED publishes did not: on a live
// call carrying business data, eight of ten events had it and these two came
// through empty. Nothing broke, because the panel reads its cache — but the
// promise was only true of the events that happened to keep it.
func TestEveryCallEventRepeatsTheContextTheEnvelopePromises(t *testing.T) {
	minted := uuid.New()
	sent := map[string]any{"orderId": "9999000000000000"}

	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	pub := &capturingPublisher{}
	c := NewCoordinator(registry, nil, oneAgent{}, pub)
	c.AttachCallData(stashedData{minted: sent})

	// A placed call whose agent leg then merges into it: the shape click-to-dial
	// produces, and where the merge announcement is made.
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound",
		map[string]string{
			"variable_aicc_call_id": minted.String(),
			"variable_dialed_user":  agentExtension,
		}))
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "far-chan", "inbound",
		map[string]string{"variable_aicc_call_id": uuid.New().String()}))
	c.Handle(t.Context(), raw("CHANNEL_BRIDGE", "agent-chan", "outbound",
		map[string]string{"Other-Leg-Unique-ID": "far-chan"}))

	var checked int
	pub.mu.Lock()
	seen := append([]events.Event(nil), pub.events...)
	pub.mu.Unlock()
	for _, ev := range seen {
		if ev.Type != events.TypePartyChanged {
			continue
		}
		checked++
		if len(ev.UserData) == 0 {
			t.Errorf("%s carried no userData; a consumer rendering from the envelope "+
				"watches the call's context vanish and come back", ev.Type)
		}
		if ev.CallType == "" {
			t.Errorf("%s carried no callType", ev.Type)
		}
	}
	if checked == 0 {
		t.Fatal("no PARTY_CHANGED was published, so this asserted nothing")
	}
}

// A leg the agent placed themselves says it is dialling, once. Announcing it
// as ringing as well says the same transition twice — and the second time in
// the callee's words, which on an outgoing leg name the number being dialled
// as the one doing the ringing. Being on the call is a separate fact and
// still follows.
func TestALegTheAgentPlacedIsNotAlsoAnnouncedAsRinging(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	pub := &capturingPublisher{}
	agents := &presenceCalls{}
	c := NewCoordinator(registry, nil, agents, pub)

	c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound",
		map[string]string{"variable_dialed_user": agentExtension}))

	if !pub.has(events.TypePartyDialing) {
		t.Fatal("the leg did not announce that it was dialling")
	}
	if pub.has(events.TypePartyRinging) {
		ev, _, _ := pub.find(events.TypePartyRinging)
		t.Errorf("the same leg was announced as ringing too: %v", ev.Payload)
	}
	agents.mu.Lock()
	onCall := len(agents.steps)
	agents.mu.Unlock()
	if onCall == 0 {
		t.Error("the agent was not put on the call; dropping the ringing event " +
			"must not drop that with it")
	}
}

// A call the agent placed keeps the identity the dialplan minted for it, and
// the agent is never told it changed — because it does not.
//
// At the moment such a call bridges it holds nothing but the agent's own leg,
// which read as "provisional, give way" — a rule written for a queue delivery,
// where the agent's leg really did arrive on a call of its own. So the minted
// identity was discarded in favour of the far end's, and reidentify put it
// back milliseconds later. Live, that told the agent their call id twice
// within twelve milliseconds, the first one already on its way out (C46).
func TestACallTheAgentPlacedKeepsItsMintedIdentityInOneMove(t *testing.T) {
	minted := uuid.New()

	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	pub := &capturingPublisher{}
	c := NewCoordinator(registry, nil, oneAgent{}, pub)

	// The agent's own leg, raised with the identity the request minted.
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "agent-chan", "outbound", map[string]string{
		"variable_aicc_call_id": minted.String(),
		"variable_dialed_user":  agentExtension,
	}))
	// The far end, raised by the dialplan and carrying no identity of its own.
	c.Handle(t.Context(), raw("CHANNEL_CREATE", "far-chan", "outbound", nil))
	c.Handle(t.Context(), raw("CHANNEL_BRIDGE", "agent-chan", "outbound",
		map[string]string{"Other-Leg-Unique-ID": "far-chan"}))

	pub.mu.Lock()
	var merges []events.Event
	for _, ev := range pub.events {
		if ev.Type == events.TypePartyChanged {
			merges = append(merges, ev)
		}
	}
	pub.mu.Unlock()

	// None at all, which is better than one: the agent's own party never
	// moved, so there is nothing to tell them. Before this, it moved out of
	// the minted call and back again, and they were told twice.
	if len(merges) != 0 {
		ids := make([]string, 0, len(merges))
		for _, ev := range merges {
			ids = append(ids, ev.CallID.String())
		}
		t.Errorf("announced %d id changes (%v) to an agent whose call never changed "+
			"identity; each one sends a panel to refetch", len(merges), ids)
	}
	// And the surviving call is the minted one, which is what every other
	// record of this call refers to.
	if id, ok := c.registry.CallForChannel("agent-chan"); !ok || id != minted {
		t.Errorf("the agent's channel ended on call %s, want %s", id, minted)
	}
}

// C24's closure turned this up in the ledger: three internal calls nobody
// answered, each written down with the caller in both columns. Every agent leg
// read its far end off the ANI, which is the caller on a leg the switch
// delivers and the agent themselves on a leg the agent's own phone raised.
// A bridge overwrites the field the moment two legs meet, so only a call that
// never connected ever kept the wrong answer — and that is exactly the call
// whose record has nothing else to say who was dialled.
func TestALegKnowsWhoItFacesBeforeAnythingHasBridged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vars  map[string]string
		dir   string
		agent string
		want  string
	}{
		{
			// An agent picks up their handset and dials a colleague. Their own
			// leg is inbound and already carries their extension; where it is
			// headed is the destination.
			name: "the leg an agent's own phone raises faces where it is headed",
			dir:  "inbound",
			vars: map[string]string{
				"Caller-Caller-ID-Number":   otherExtension,
				"Caller-Destination-Number": agentExtension,
				"Caller-Context":            "aicc",
			},
			want: agentExtension,
		},
		{
			// mod_callcenter dials the agent to deliver a waiting caller. The
			// leg is outbound, its destination is the agent, and the customer
			// it is bringing them is the ANI.
			name: "a delivery leg faces the caller it is bringing",
			dir:  "outbound",
			vars: map[string]string{
				"variable_dialed_user":            agentExtension,
				"variable_cc_side":                "agent",
				"Caller-Caller-ID-Number":         "13800138000",
				"Caller-Destination-Number":       agentExtension,
				"variable_cc_member_session_uuid": "no-such-caller",
			},
			want: "13800138000",
		},
		{
			// Click-to-dial rings the agent first. Its leg is outbound and
			// reaches this branch correctly only because Dial() puts the
			// destination in origination_caller_id_number so the agent's
			// handset shows who it is ringing — take that away and this
			// becomes the agent's own caller id again.
			name: "the leg click-to-dial originates faces the number it will reach",
			dir:  "outbound",
			vars: map[string]string{
				"variable_dialed_user":      agentExtension,
				"Caller-Caller-ID-Number":   otherExtension,
				"Caller-Destination-Number": agentExtension,
				"variable_aicc_extension":   agentExtension,
				"Caller-Context":            "aicc",
			},
			want: otherExtension,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry(nullPublisher{})
			c := NewCoordinator(registry, nil, twoAgents{}, nullPublisher{})

			ctx := t.Context()
			const channel = "the-leg"
			c.Handle(ctx, raw("CHANNEL_CREATE", channel, tc.dir, tc.vars))

			callID, ok := registry.CallForChannel(channel)
			if !ok {
				t.Fatal("the leg is bound to no call")
			}
			var got string
			var agentID *uuid.UUID
			if err := registry.Do(callID, func(call *Call) {
				if p := call.PartyByChannel(channel); p != nil {
					got, agentID = p.OtherNumber, p.AgentID
				}
			}); err != nil {
				t.Fatalf("reading the call: %v", err)
			}
			if agentID == nil {
				t.Fatal("the leg is not attributed to an agent, so this case tests nothing")
			}
			if got != tc.want {
				t.Errorf("otherNumber = %q, want %q", got, tc.want)
			}
		})
	}
}

// The owner read a live stream of a click-to-dial nobody picked up
// (2026-08-24, 01a0310d-5613): wei clicked to dial ben, wei's own leg
// auto-answered a second later, and the cockpit was told PARTY_ESTABLISHED /
// TALKING while ben's phone rang for thirty seconds and was never answered.
//
// Owner's rule: ESTABLISHED means both ends are on the call, and it comes off
// the switch's bridge event. Answering is a leg's own fact — the ledger has
// always kept the two apart, and now so does the party.
func TestAnAgentIsNotToldTheyAreTalkingUntilSomebodyIsThere(t *testing.T) {
	pub := &capturingPublisher{}
	registry := NewRegistry(pub)
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, twoAgents{}, pub)

	ctx := t.Context()
	agentChan, calleeChan := "agent-chan", "callee-chan"

	// Click-to-dial rings the agent first, and their phone auto-answers.
	c.Handle(ctx, raw("CHANNEL_CREATE", agentChan, "outbound", map[string]string{
		"variable_dialed_user":    agentExtension,
		"variable_aicc_extension": agentExtension,
		"Caller-Caller-ID-Number": otherExtension,
		"Caller-Context":          "aicc",
	}))
	c.Handle(ctx, raw("CHANNEL_ANSWER", agentChan, "outbound", map[string]string{
		"variable_dialed_user":    agentExtension,
		"variable_aicc_extension": agentExtension,
	}))
	// The colleague's phone rings and rings.
	c.Handle(ctx, raw("CHANNEL_CREATE", calleeChan, "outbound", map[string]string{
		"Caller-Destination-Number":    otherExtension,
		"variable_aicc_parent_channel": agentChan,
		"Caller-Context":               "aicc",
	}))

	// Nothing to wait for — the assertion is that nothing arrives — so give
	// the actor a moment to have published it if it were going to.
	time.Sleep(50 * time.Millisecond)

	pub.mu.Lock()
	var established []events.Type
	for _, ev := range pub.events {
		if ev.Type == events.TypePartyEstablished {
			established = append(established, ev.Type)
		}
	}
	pub.mu.Unlock()
	if len(established) != 0 {
		t.Errorf("%d PARTY_ESTABLISHED published while the far end was still ringing; "+
			"the agent's own leg auto-answering is not a conversation", len(established))
	}

	callID, ok := registry.CallForChannel(agentChan)
	if !ok {
		t.Fatal("the agent's leg is bound to no call")
	}
	var state PartyState
	var answered time.Time
	if err := registry.Do(callID, func(call *Call) {
		if p := call.PartyByChannel(agentChan); p != nil {
			state, answered = p.State, p.AnsweredAt
		}
	}); err != nil {
		t.Fatalf("reading the call: %v", err)
	}
	if state == PartyTalking {
		t.Error("the agent's party is TALKING with nobody on the other end")
	}
	// The answer is still recorded: it is what the carrier bills, and dropping
	// it would have moved bill_sec, which is a different question entirely.
	if answered.IsZero() {
		t.Error("the leg's own answer was not recorded; bill_sec asks that question")
	}

	// Now the colleague picks up and the switch bridges the two.
	c.Handle(ctx, raw("CHANNEL_ANSWER", calleeChan, "outbound", map[string]string{
		"Caller-Destination-Number": otherExtension,
	}))
	c.Handle(ctx, raw("CHANNEL_BRIDGE", calleeChan, "outbound", map[string]string{
		"Caller-Destination-Number": otherExtension,
		"Other-Leg-Unique-ID":       agentChan,
	}))

	deadline := time.After(2 * time.Second)
	for {
		pub.mu.Lock()
		n := 0
		for _, ev := range pub.events {
			if ev.Type == events.TypePartyEstablished {
				n++
			}
		}
		pub.mu.Unlock()
		// Both halves of a connected call are established, at the same moment.
		if n == 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%d PARTY_ESTABLISHED after the bridge, want 2 — one per leg", n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// The one leg with nobody to ask about its far end is the one that needed it
// most: a call to a number this system does not serve is rejected before a
// second leg exists, so the row said somebody had been turned away without
// saying what they had dialled. Eleven such rows in the ledger, while the
// switch logged "unknown number 95009 from …" as it rejected each one (C31).
func TestARejectedCallersLegKnowsWhatTheyDialled(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})

	const channel = "rejected-caller"
	c.Handle(t.Context(), raw("CHANNEL_CREATE", channel, "inbound", map[string]string{
		"Caller-Caller-ID-Number":   "18688886669",
		"Caller-Destination-Number": "95009",
		"Caller-Context":            "public",
	}))

	callID, ok := registry.CallForChannel(channel)
	if !ok {
		t.Fatal("the caller is bound to no call")
	}
	var number, other string
	var agentID *uuid.UUID
	if err := registry.Do(callID, func(call *Call) {
		if p := call.PartyByChannel(channel); p != nil {
			number, other, agentID = p.Number, p.OtherNumber, p.AgentID
		}
	}); err != nil {
		t.Fatalf("reading the call: %v", err)
	}
	if agentID != nil {
		t.Fatal("this leg belongs to an agent, so it tests the wrong thing")
	}
	if number != "18688886669" {
		t.Errorf("number = %q, want the caller", number)
	}
	if other != "95009" {
		t.Errorf("otherNumber = %q, want 95009 — without it a disconnected number "+
			"rung all day and somebody scanning numbers are the same row", other)
	}
}

// The DID now rides the customer's leg of an AI outbound from creation (C53).
// It must not make that leg look like the bot's: isBotLeg asks whether the leg
// was dialled *at* the DID, and this one is dialled at the customer.
func TestTheCustomerLegOfAnAIOutboundIsNotMistakenForTheBots(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	t.Cleanup(registry.Shutdown)
	c := NewCoordinator(registry, nil, oneAgent{}, nullPublisher{})

	const customer, bot = "customer-leg", "bot-leg"
	c.Handle(t.Context(), raw("CHANNEL_CREATE", customer, "outbound", map[string]string{
		"variable_aicc_did":         "95012",
		"Caller-Destination-Number": "13912345678",
	}))
	c.Handle(t.Context(), raw("CHANNEL_CREATE", bot, "outbound", map[string]string{
		"variable_aicc_did":            "95012",
		"Caller-Destination-Number":    "95012",
		"variable_aicc_parent_channel": customer,
	}))

	callID, ok := registry.CallForChannel(customer)
	if !ok {
		t.Fatal("the customer's leg is bound to no call")
	}
	var customerIsBot, botIsBot bool
	if err := registry.Do(callID, func(call *Call) {
		if p := call.PartyByChannel(customer); p != nil {
			customerIsBot = p.IsBotLeg
		}
		if p := call.PartyByChannel(bot); p != nil {
			botIsBot = p.IsBotLeg
		}
	}); err != nil {
		t.Fatalf("reading the call: %v", err)
	}
	if customerIsBot {
		t.Error("the customer's leg is marked as the bot's; the ledger would hand " +
			"the row to the bot's writer for a call the bot never took")
	}
	if !botIsBot {
		t.Error("the leg dialled at the DID is not marked as the bot's")
	}
}
