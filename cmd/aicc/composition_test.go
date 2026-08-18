// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

// Fakes that record what they were handed. None of them re-implements the
// composition: they observe it. A fake that reproduced the wiring would agree
// with a broken run() as readily as a correct one.

type fakeCoordinator struct {
	taps      telephony.Tapper
	audiences telephony.Audiences
	cdr       *telephony.CDRAssembler
	// hookWhenCDRAttached is what the registry's finish hook did at the moment
	// AttachCDR was called. The real coordinator *sets* that hook, so a
	// composition that wrapped it first would have its wrapper thrown away —
	// and the wrapper is the transcript's retirement.
	hookWhenCDRAttached func()
	reg                 *telephony.Registry
}

func (f *fakeCoordinator) AttachTaps(t telephony.Tapper) { f.taps = t }

func (f *fakeCoordinator) AttachAudiences(a telephony.Audiences) { f.audiences = a }

func (f *fakeCoordinator) AttachCDR(a *telephony.CDRAssembler) {
	f.cdr = a
	if f.reg != nil && f.reg.OnCallFinished != nil {
		hook := f.reg.OnCallFinished
		f.hookWhenCDRAttached = func() { hook(telephony.Snapshot{}) }
	}
}

type fakeAgents struct {
	staffing agents.Staffing
	synced   int
	observed []telephony.Registration
}

func (f *fakeAgents) AttachStaffing(s agents.Staffing) { f.staffing = s }
func (f *fakeAgents) SyncSwitch(context.Context)       { f.synced++ }
func (f *fakeAgents) ObserveDevice(_ context.Context, ext string, isRegistered, isInService bool) {
	if !isRegistered {
		return
	}
	f.observed = append(f.observed, telephony.Registration{Extension: ext, IsReachable: isInService})
}

type fakeCatalog struct {
	tiersSynced int
	reconciled  []uuid.UUID
}

func (f *fakeCatalog) SyncTiers(context.Context) { f.tiersSynced++ }
func (f *fakeCatalog) ReconcileAgentTiers(_ context.Context, agentID uuid.UUID) {
	f.reconciled = append(f.reconciled, agentID)
}

type fakeAudiences struct {
	set map[uuid.UUID][]uuid.UUID
}

func (f *fakeAudiences) SetAudience(callID uuid.UUID, agentIDs []uuid.UUID) {
	if f.set == nil {
		f.set = map[uuid.UUID][]uuid.UUID{}
	}
	f.set[callID] = agentIDs
}

type fakeLink struct{ onConnect func(context.Context) }

func (f *fakeLink) OnConnect(fn func(context.Context)) { f.onConnect = fn }

type fakeTapper struct {
	detachedCalls []uuid.UUID
}

func (f *fakeTapper) Attach(uuid.UUID, *uuid.UUID, *uuid.UUID, string, string) {}
func (f *fakeTapper) Detach(string)                                            {}
func (f *fakeTapper) DetachCall(id uuid.UUID)                                  { f.detachedCalls = append(f.detachedCalls, id) }
func (f *fakeTapper) Pause(string) error                                       { return nil }
func (f *fakeTapper) Resume(string) error                                      { return nil }

type wiringFixture struct {
	comp        composition
	registry    *telephony.Registry
	coordinator *fakeCoordinator
	agents      *fakeAgents
	catalog     *fakeCatalog
	link        *fakeLink
	taps        *fakeTapper
	audiences   *fakeAudiences
	transcripts *retirer
	cdr         *telephony.CDRAssembler
	regs        []telephony.Registration
	regsErr     error
	regsCalls   int
	// priorFinished and priorRetired stand in for whatever was already
	// listening, so "composes on top" is distinguishable from "replaces".
	priorFinished int
	priorRetired  int
}

func newWiringFixture(t *testing.T) *wiringFixture {
	t.Helper()
	f := &wiringFixture{
		registry:    telephony.NewRegistry(nullPub{}),
		agents:      &fakeAgents{},
		catalog:     &fakeCatalog{},
		link:        &fakeLink{},
		taps:        &fakeTapper{},
		audiences:   &fakeAudiences{},
		transcripts: &retirer{},
		cdr:         &telephony.CDRAssembler{},
		regs: []telephony.Registration{
			{Extension: "1001", IsReachable: true},
			{Extension: "1002", IsReachable: false},
		},
	}
	t.Cleanup(f.registry.Shutdown)
	f.coordinator = &fakeCoordinator{reg: f.registry}
	f.registry.OnCallFinished = func(telephony.Snapshot) { f.priorFinished++ }
	f.registry.OnCallRetired = func(uuid.UUID) { f.priorRetired++ }

	f.comp = composition{
		Registry:      f.registry,
		Coordinator:   f.coordinator,
		Agents:        f.agents,
		Catalog:       f.catalog,
		Link:          f.link,
		CDR:           f.cdr,
		Transcripts:   f.transcripts,
		Audiences:     f.audiences,
		Taps:          f.taps,
		Registrations: func() ([]telephony.Registration, error) { f.regsCalls++; return f.regs, f.regsErr },
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return f
}

type nullPub struct{}

func (nullPub) Publish(context.Context, events.Event, events.Scope) events.Event {
	return events.Event{}
}

// Every connection, asserted by identity rather than by "something happened".
//
// This is the test that would have caught three shipped defects, all of the
// same shape: a line in run() that nothing named, so deleting it left the
// build green, the suite green and the system quietly missing a behaviour.
func TestConnectMakesEveryConnection(t *testing.T) {
	f := newWiringFixture(t)
	f.comp.connect()

	t.Run("presence reconciles staffing through the catalog", func(t *testing.T) {
		if f.agents.staffing != agents.Staffing(f.catalog) {
			t.Fatalf("staffing = %#v, want the catalog service", f.agents.staffing)
		}
	})

	t.Run("the switch link has a reconnect handler", func(t *testing.T) {
		if f.link.onConnect == nil {
			t.Fatal("nothing is registered on the link's reconnect")
		}
	})

	t.Run("the coordinator has the cdr assembler", func(t *testing.T) {
		if f.coordinator.cdr != f.cdr {
			t.Fatalf("cdr = %p, want %p", f.coordinator.cdr, f.cdr)
		}
	})

	t.Run("the coordinator addresses transcripts", func(t *testing.T) {
		if f.coordinator.audiences != telephony.Audiences(f.audiences) {
			t.Fatalf("audiences = %#v, want the transcript registry — without it "+
				"every transcript event is an unscoped system notice",
				f.coordinator.audiences)
		}
	})

	t.Run("the coordinator has the tap", func(t *testing.T) {
		if f.coordinator.taps != telephony.Tapper(f.taps) {
			t.Fatalf("taps = %#v, want the media tap", f.coordinator.taps)
		}
	})

	t.Run("a finished call retires its transcript without displacing the cdr", func(t *testing.T) {
		f.registry.OnCallFinished(telephony.Snapshot{CallID: uuid.New()})
		if f.priorFinished != 1 {
			t.Errorf("the existing finish hook ran %d times, want 1 — a CDR is not "+
				"something to lose in order to free a goroutine", f.priorFinished)
		}
		if len(f.transcripts.closed) != 1 {
			t.Errorf("retired %v transcripts, want 1", f.transcripts.closed)
		}
	})

	t.Run("a retired call detaches its taps without displacing what listened", func(t *testing.T) {
		callID := uuid.New()
		f.registry.OnCallRetired(callID)
		if f.priorRetired != 1 {
			t.Errorf("the existing retire hook ran %d times, want 1", f.priorRetired)
		}
		if len(f.taps.detachedCalls) != 1 || f.taps.detachedCalls[0] != callID {
			t.Errorf("detached %v, want the retired call", f.taps.detachedCalls)
		}
	})
}

// The CDR assembler is attached by way of setting the registry's finish hook,
// so composing the transcript retirement on top of it has to happen after. The
// other order compiles, runs, and silently throws the retirement away.
func TestTheCDRIsAttachedBeforeTheFinishHookIsComposed(t *testing.T) {
	f := newWiringFixture(t)
	f.comp.connect()

	if f.coordinator.hookWhenCDRAttached == nil {
		t.Fatal("AttachCDR was never called with a finish hook in place")
	}
	f.coordinator.hookWhenCDRAttached()
	if len(f.transcripts.closed) != 0 {
		t.Error("the transcript retirement was already composed when AttachCDR ran, " +
			"so the real coordinator would have replaced it")
	}
}

// Transcription off must leave the rest of the composition intact and add
// nothing of its own.
func TestNoTapMeansNoTapConnections(t *testing.T) {
	f := newWiringFixture(t)
	f.comp.Taps = nil
	f.comp.connect()

	if f.coordinator.taps != nil {
		t.Errorf("a tap was attached with transcription off: %#v", f.coordinator.taps)
	}
	f.registry.OnCallRetired(uuid.New())
	if f.priorRetired != 1 {
		t.Errorf("the existing retire hook ran %d times, want 1", f.priorRetired)
	}
	if len(f.taps.detachedCalls) != 0 {
		t.Errorf("detached %v with no tap configured", f.taps.detachedCalls)
	}
	// The unconditional half is still wired.
	if f.agents.staffing == nil || f.link.onConnect == nil ||
		f.coordinator.cdr == nil || f.coordinator.audiences == nil {
		t.Error("turning transcription off dropped a connection that is not its own")
	}
}

// A reconnect rebuilds both directions: what the switch forgot about us, and
// what we never observed about it.
func TestReconnectRebuildsPresenceStaffingAndDevices(t *testing.T) {
	f := newWiringFixture(t)
	f.comp.connect()

	f.link.onConnect(t.Context())

	if f.agents.synced != 1 {
		t.Errorf("presence synced %d times, want 1", f.agents.synced)
	}
	if f.catalog.tiersSynced != 1 {
		t.Errorf("tiers synced %d times, want 1 — an agent the switch has no tier "+
			"for is offered nothing and the caller abandons", f.catalog.tiersSynced)
	}
	if f.regsCalls != 1 {
		t.Errorf("registrations read %d times, want 1", f.regsCalls)
	}
	want := []telephony.Registration{
		{Extension: "1001", IsReachable: true},
		{Extension: "1002", IsReachable: false},
	}
	if len(f.agents.observed) != len(want) {
		t.Fatalf("observed %v, want %v", f.agents.observed, want)
	}
	for i, reg := range want {
		if f.agents.observed[i] != reg {
			t.Errorf("observed[%d] = %+v, want %+v", i, f.agents.observed[i], reg)
		}
	}
}

// A switch that will not answer must not cost us the half of the reconnect
// that does not depend on it.
func TestReconnectStillSyncsWhenRegistrationsCannotBeRead(t *testing.T) {
	f := newWiringFixture(t)
	f.regsErr = errors.New("-ERR not connected")
	f.comp.connect()

	f.link.onConnect(t.Context())

	if f.agents.synced != 1 || f.catalog.tiersSynced != 1 {
		t.Errorf("synced presence %d and tiers %d, want 1 each even with the "+
			"registration read failing", f.agents.synced, f.catalog.tiersSynced)
	}
	if len(f.agents.observed) != 0 {
		t.Errorf("observed %v from a failed read", f.agents.observed)
	}
}

// --- the two struct literals -------------------------------------------------

type fakeStreamer struct{}

func (fakeStreamer) Open(context.Context, string) (io.ReadSeekCloser, int64, error) {
	return nil, 0, errors.New("not used")
}

// zeroFields names the fields of v that are still the zero value.
//
// Reflection rather than a written-out list of names because the list is what
// goes stale: a field added to httpapi.Deps or aicall.OrchestratorConfig and
// never plumbed is precisely the failure this is for, and a hand-maintained
// list would be updated by the same commit that forgot to plumb it.
func zeroFields(v any, allowed ...string) []string {
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	var missing []string
	for i := range rt.NumField() {
		if slices.Contains(allowed, rt.Field(i).Name) {
			continue
		}
		if rv.Field(i).IsZero() {
			missing = append(missing, rt.Field(i).Name)
		}
	}
	return missing
}

// Every dependency the HTTP server has is populated.
//
// The literal this replaced could drop any of its twelve lines and still
// compile: Deps.Transcripts is how the transcript snapshot learns what state
// transcription is in, and losing it degrades the panel rather than failing
// anything. Parameters cannot be dropped — that is a compile error — and this
// covers the other direction, a field added to Deps and never plumbed.
func TestEveryHTTPDependencyIsPlumbed(t *testing.T) {
	deps := apiDeps(
		&auth.Service{},
		&events.Hub{},
		&agents.Service{},
		agentDirectory{},
		&telephony.Coordinator{},
		&transcript.Registry{},
		&catalog.Service{},
		&store.LedgerStore{},
		fakeStreamer{},
		&store.LedgerStore{},
		&outbound.Service{},
		http.NewServeMux(),
	)

	if missing := zeroFields(deps); len(missing) > 0 {
		t.Errorf("httpapi.Deps fields left unset: %v — each one silently disables "+
			"the feature it serves", missing)
	}
}

// Same for the AI voice leg. Sessions and Logger are the orchestrator's own
// defaults and are named here so that the exemption is a decision on the
// record rather than a gap.
func TestEveryBotDependencyIsPlumbed(t *testing.T) {
	cfg := botConfig(
		botUAS(config.Config{BotSIPHost: "127.0.0.1", BotSIPPort: 6060,
			BotAdvertiseIP: "127.0.0.1", BotRTPPortLow: 40000, BotRTPPortHigh: 40999,
			BotMaxCalls: 10}),
		&catalog.Service{},
		&store.FlowStore{},
		&telephony.Adapter{},
		&store.LedgerStore{},
		&transcript.Registry{},
		"http://127.0.0.1:8080",
		provider.Profile{Name: "openai"},
		func(store.Callback) {},
	)

	if missing := zeroFields(cfg, "Sessions", "Logger"); len(missing) > 0 {
		t.Errorf("aicall.OrchestratorConfig fields left unset: %v", missing)
	}
	if missing := zeroFields(cfg.UAS, "Logger"); len(missing) > 0 {
		t.Errorf("voice.Config fields left unset: %v — RTPDeadTimeout at zero "+
			"disables the dead-media check and RTCPInterval at zero disables "+
			"reporting, both without a word", missing)
	}
}

type recordingPublisher struct {
	events []events.Event
	scopes []events.Scope
}

func (r *recordingPublisher) Publish(_ context.Context, ev events.Event, sc events.Scope) events.Event {
	r.events = append(r.events, ev)
	r.scopes = append(r.scopes, sc)
	return ev
}

// The bot's callback announcement is a broadcast, said rather than inferred.
//
// Under the old hub an empty scope reached everyone, so this worked by
// accident and would have stopped working silently the moment the hub became
// default-deny. It is stated now, and this is what holds it stated.
func TestABotCallbackIsAnnouncedToEveryone(t *testing.T) {
	pub := &recordingPublisher{}
	callID := uuid.New()

	announceCallback(t.Context(), pub)(store.Callback{CallID: &callID})

	if len(pub.events) != 1 {
		t.Fatalf("published %d events, want 1", len(pub.events))
	}
	if pub.events[0].Type != events.TypeCallbackCreated {
		t.Errorf("published %s, want CALLBACK_CREATED", pub.events[0].Type)
	}
	if !pub.scopes[0].IsBroadcast {
		t.Errorf("scope = %+v, want a broadcast — a default-deny hub delivers "+
			"an unaddressed callback to nobody but supervisors", pub.scopes[0])
	}
}
