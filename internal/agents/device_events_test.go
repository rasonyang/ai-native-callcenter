// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// scopedPublisher keeps who each event was addressed to, which is what the
// stream's scoping is made of.
type scopedPublisher struct {
	mu   sync.Mutex
	sent []sentEvent
}

type sentEvent struct {
	ev    events.Event
	scope events.Scope
}

func (p *scopedPublisher) Publish(_ context.Context, ev events.Event, scope events.Scope) events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, sentEvent{ev, scope})
	return ev
}

func (p *scopedPublisher) all() []sentEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sentEvent(nil), p.sent...)
}

// phones is a small floor: agents bound to 1001, 1002 and 1003, one agent with
// no phone, and an extension (1004) bound to nobody.
type phones struct {
	svc   *Service
	store *fakeStore
	sw    *fakeSwitch
	pub   *scopedPublisher
	a     uuid.UUID // 1001
	b     uuid.UUID // 1002
	c     uuid.UUID // 1003
	d     uuid.UUID // no binding
}

func newPhones(t *testing.T) *phones {
	t.Helper()
	f := &phones{
		store: newFakeStore(), sw: &fakeSwitch{up: true}, pub: &scopedPublisher{},
		a: uuid.New(), b: uuid.New(), c: uuid.New(), d: uuid.New(),
	}
	f.store.extNumbers = map[uuid.UUID]string{}
	bind := func(id uuid.UUID, name, number string) {
		prof := Profile{AgentID: id, UserID: uuid.New(), CallcenterName: name, DisplayName: name}
		if number != "" {
			extID := uuid.New()
			f.store.extNumbers[extID] = number
			prof.ExtensionNumber, prof.ExtensionID = number, &extID
		}
		f.store.profiles[id] = prof
	}
	bind(f.a, "agent-a", "1001")
	bind(f.b, "agent-b", "1002")
	bind(f.c, "agent-c", "1003")
	bind(f.d, "agent-d", "")
	f.svc = NewService(f.store, f.sw, f.pub)
	return f
}

func (f *phones) name(id uuid.UUID) string {
	switch id {
	case f.a:
		return "a"
	case f.b:
		return "b"
	case f.c:
		return "c"
	}
	return "d"
}

func deviceType(s DeviceSignal) events.Type { return s.eventType() }

func isDeviceEvent(t events.Type) bool {
	switch t {
	case events.TypeDeviceRegistered, events.TypeDeviceUnregistered,
		events.TypeDeviceReachable, events.TypeDeviceUnreachable:
		return true
	}
	return false
}

// checkDevicePayload asserts the rule every payload obeys: the device fields
// come as a pair, true only with the bound extension named.
func checkDevicePayload(t *testing.T, ev events.Event, wantRegistered bool, bound string) {
	t.Helper()
	got, _ := ev.Payload["isDeviceRegistered"].(bool)
	if got != wantRegistered {
		t.Errorf("%s: isDeviceRegistered = %v, want %v", ev.Type, got, wantRegistered)
	}
	account, present := ev.Payload["deviceAccount"]
	if !present {
		t.Fatalf("%s: deviceAccount is absent, want present (null when unregistered)", ev.Type)
	}
	if wantRegistered {
		if account != bound {
			t.Errorf("%s: deviceAccount = %v, want %q", ev.Type, account, bound)
		}
	} else if account != nil {
		t.Errorf("%s: deviceAccount = %v, want null", ev.Type, account)
	}
}

func TestASignedOutAgentHearsAboutTheirOwnPhoneAndNobodyElseDoes(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()

	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)

	sent := f.pub.all()
	if len(sent) != 1 {
		t.Fatalf("published %d events, want 1", len(sent))
	}
	if sent[0].ev.Type != events.TypeDeviceRegistered {
		t.Errorf("type = %s, want DEVICE_REGISTERED", sent[0].ev.Type)
	}
	if got := sent[0].scope.AgentIDs; len(got) != 1 || got[0] != f.a {
		t.Errorf("scope = %v, want only the agent bound to 1001", got)
	}
	checkDevicePayload(t, sent[0].ev, true, "1001")
	if _, ok := sent[0].ev.Payload["extensionNumber"]; ok {
		t.Error("extensionNumber present for an agent who is signed out")
	}
	if sent[0].ev.Payload["state"] != string(StateLoggedOut) {
		t.Errorf("state = %v, want LOGGED_OUT", sent[0].ev.Payload["state"])
	}

	// Publish only: nothing was told to the switch, nothing persisted, and the
	// presence stands where it did.
	if len(f.sw.commands) != 0 {
		t.Errorf("switch commands %v, want none", f.sw.commands)
	}
	if len(f.store.presence) != 0 {
		t.Errorf("presence was written: %v", f.store.presence)
	}
	if !f.svc.Presence(f.a).IsLoggedOut() {
		t.Error("the agent's presence moved")
	}
}

func TestAnExtensionBoundToNobodyReachesNoAgentAndKeepsNoBaseline(t *testing.T) {
	t.Parallel()
	f := newPhones(t)

	f.svc.ObserveDevice(t.Context(), "1004", SignalRegistered)
	f.svc.ObserveDevice(t.Context(), "1004", SignalRegistered)

	if sent := f.pub.all(); len(sent) != 0 {
		t.Errorf("published %d events for an extension nobody holds", len(sent))
	}
	if len(f.svc.announced) != 0 {
		t.Errorf("baseline kept for an unbound extension: %v", f.svc.announced)
	}
	// The device table still learns it: a click-to-dial reads it.
	if reg, _, known := f.svc.DeviceAtExtension("1004"); !reg || !known {
		t.Error("the device table did not learn the phone")
	}
}

func TestDuplicateDeviceSignalsPublishOnce(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()

	for range 3 {
		f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	}
	for range 3 {
		f.svc.ObserveDevice(ctx, "1001", SignalReachable)
	}
	if n := len(f.pub.all()); n != 1 {
		t.Fatalf("repeated REGISTERED/REACHABLE for an unchanged phone published %d events, want 1", n)
	}

	// Real changes still get through, once each.
	f.svc.ObserveDevice(ctx, "1001", SignalUnreachable)
	f.svc.ObserveDevice(ctx, "1001", SignalUnreachable)
	f.svc.ObserveDevice(ctx, "1001", SignalUnregistered)
	f.svc.ObserveDevice(ctx, "1001", SignalUnregistered)
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	var got []events.Type
	for _, s := range f.pub.all() {
		got = append(got, s.ev.Type)
	}
	want := []events.Type{
		events.TypeDeviceRegistered, events.TypeDeviceUnreachable,
		events.TypeDeviceUnregistered, events.TypeDeviceRegistered,
	}
	if len(got) != len(want) {
		t.Fatalf("published %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published %v, want %v", got, want)
		}
	}
}

// The reconnect sweep learns the phones quietly and then announces them. The
// baseline is what the stream was last told, not what the table holds, so a
// phone that changed while the link was down is announced and one that did not
// is not.
func TestTheReconnectSweepAnnouncesOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()

	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	f.svc.ObserveDevice(ctx, "1002", SignalRegistered)
	told := len(f.pub.all())

	// While the link was down 1001 went away and 1002 stayed; 1003 appeared.
	sweep := map[string]DeviceSignal{"1001": SignalUnregistered, "1002": SignalReachable, "1003": SignalReachable}
	f.svc.NoteDevice("1001", false, false)
	f.svc.NoteDevice("1002", true, true)
	f.svc.NoteDevice("1003", true, true)
	for _, ext := range []string{"1001", "1002", "1003"} {
		f.svc.ObserveDevice(ctx, ext, sweep[ext])
	}

	sent := f.pub.all()[told:]
	if len(sent) != 2 {
		t.Fatalf("the sweep published %d events, want 2 (1001 gone, 1003 new)", len(sent))
	}
	if sent[0].scope.AgentIDs[0] != f.a || sent[0].ev.Type != events.TypeDeviceUnregistered {
		t.Errorf("first = %s to %v, want DEVICE_UNREGISTERED to agent a", sent[0].ev.Type, sent[0].scope.AgentIDs)
	}
	checkDevicePayload(t, sent[0].ev, false, "1001")
	if sent[1].scope.AgentIDs[0] != f.c {
		t.Errorf("second went to %v, want agent c", sent[1].scope.AgentIDs)
	}
	checkDevicePayload(t, sent[1].ev, true, "1003")
}

func TestLoggingOutOfARegisteredPhoneStillTellsTheTruthAndKeepsTelling(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	if _, err := f.svc.Login(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Logout(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	// Signed out, the payload no longer has an extensionNumber, and the phone
	// is still the registered one.
	logout := f.pub.all()[len(f.pub.all())-1].ev
	if logout.Type != events.TypeAgentLoggedOut {
		t.Fatalf("last event %s, want AGENT_LOGGED_OUT", logout.Type)
	}
	checkDevicePayload(t, logout, true, "1001")
	if _, ok := logout.Payload["extensionNumber"]; ok {
		t.Error("extensionNumber present after sign-out")
	}

	commands := len(f.sw.commands)
	told := len(f.pub.all())
	f.svc.ObserveDevice(ctx, "1001", SignalUnregistered)
	sent := f.pub.all()[told:]
	if len(sent) != 1 || sent[0].ev.Type != events.TypeDeviceUnregistered {
		t.Fatalf("published %v, want one DEVICE_UNREGISTERED", sent)
	}
	checkDevicePayload(t, sent[0].ev, false, "1001")
	if len(f.sw.commands) != commands {
		t.Errorf("the signed-out path told the switch: %v", f.sw.commands[commands:])
	}
	if !f.svc.Presence(f.a).IsLoggedOut() {
		t.Error("presence moved")
	}
}

func TestAnUnreachablePhoneIsStillRegisteredInEveryPayload(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	f.svc.ObserveDevice(ctx, "1002", SignalRegistered)
	if _, err := f.svc.Login(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	f.svc.ObserveDevice(ctx, "1002", SignalUnreachable)
	if _, err := f.svc.Logout(ctx, f.b); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.pub.all()[1:] {
		checkDevicePayload(t, s.ev, true, "1002")
	}
}

func TestAnAgentWithNoPhoneNeverClaimsOne(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	if _, err := f.svc.Login(t.Context(), f.d); !errors.Is(err, ErrNoExtensionBound) {
		t.Fatalf("Login() error = %v, want ErrNoExtensionBound", err)
	}
	f.svc.ObserveDevice(t.Context(), "", SignalRegistered)
	if sent := f.pub.all(); len(sent) != 0 {
		t.Errorf("published %d events for an agent without a phone", len(sent))
	}
}

// A signed-in agent still has their phone mirrored and released, and
// deduplication takes none of it away: it concerns publication only.
func TestDeduplicationLeavesTheSignedInAgentsRoutingAlone(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	if _, err := f.svc.Login(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Ready(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	// The stream was told "unregistered" already, through a quiet table
	// write and an earlier announcement; the agent is READY regardless.
	f.svc.ObserveDevice(ctx, "1001", SignalUnregistered)
	if st := f.svc.Presence(f.a); st.CurrentState() != StateNotReady || st.Reason != ReasonDeviceLost {
		t.Fatalf("presence = %s/%s, want NOT_READY/DEVICE_LOST", st.CurrentState(), st.Reason)
	}
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	if st := f.svc.Presence(f.a); st.CurrentState() != StateReady {
		t.Fatalf("presence = %s, want READY after the phone returned", st.CurrentState())
	}
	// A duplicate REGISTERED publishes nothing and changes nothing.
	told := len(f.pub.all())
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	if n := len(f.pub.all()) - told; n != 0 {
		t.Errorf("a duplicate REGISTERED published %d events", n)
	}
}

func TestEventsFollowTheBindingAfterARebindWhileSignedOut(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	told := len(f.pub.all())

	oldID := f.store.profiles[f.a].ExtensionID
	if _, err := f.svc.UpdateAgent(ctx, AgentConfig{AgentID: f.a, CallcenterName: "agent-a"}); err != nil {
		t.Fatalf("clearing a signed-out agent's binding: %v", err)
	}
	if _, err := f.svc.UpdateAgent(ctx, AgentConfig{AgentID: f.d, CallcenterName: "agent-d", DefaultExtensionID: oldID}); err != nil {
		t.Fatalf("binding 1001 to another agent: %v", err)
	}

	// The same registration again: unchanged for the previous holder, news for
	// the new one, who has been told nothing about this phone.
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered)
	sent := f.pub.all()[told:]
	if len(sent) != 1 {
		t.Fatalf("published %d events, want 1 to the new holder", len(sent))
	}
	if got := sent[0].scope.AgentIDs; len(got) != 1 || got[0] != f.d {
		t.Errorf("scope = %v, want the new holder", got)
	}
	checkDevicePayload(t, sent[0].ev, true, "1001")

	// And the old holder, now unbound, hears nothing of 1001 at all.
	f.svc.ObserveDevice(ctx, "1001", SignalUnregistered)
	for _, s := range f.pub.all()[told:] {
		if s.scope.AgentIDs[0] == f.a {
			t.Error("the previous holder still receives the phone's events")
		}
	}
}

func TestASignedInAgentCannotBeRebound(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	if _, err := f.svc.Login(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	prof := f.store.profiles[f.a]
	other := uuid.New()
	f.store.extNumbers[other] = "1009"
	events0 := len(f.pub.all())

	for name, cfg := range map[string]AgentConfig{
		"another extension": {AgentID: f.a, CallcenterName: "agent-a", DefaultExtensionID: &other},
		"cleared":           {AgentID: f.a, CallcenterName: "agent-a"},
	} {
		if _, err := f.svc.UpdateAgent(ctx, cfg); !errors.Is(err, ErrAgentSignedIn) {
			t.Errorf("%s: error = %v, want ErrAgentSignedIn", name, err)
		}
		if got := f.store.profiles[f.a]; got.ExtensionNumber != "1001" || got.ExtensionID == nil || *got.ExtensionID != *prof.ExtensionID {
			t.Errorf("%s: binding changed to %q", name, got.ExtensionNumber)
		}
		if got := f.svc.Presence(f.a); got.ExtensionNumber != "1001" || got.IsLoggedOut() {
			t.Errorf("%s: presence changed: %+v", name, got)
		}
	}
	if n := len(f.pub.all()); n != events0 {
		t.Errorf("a refused rebind published %d events", n-events0)
	}

	// An edit that keeps the extension is not a rebind.
	cfg := AgentConfig{AgentID: f.a, CallcenterName: "agent-a2", IsAutoAnswer: true, DefaultExtensionID: prof.ExtensionID}
	if _, err := f.svc.UpdateAgent(ctx, cfg); err != nil {
		t.Errorf("an edit that leaves the extension alone: %v", err)
	}

	// Signed out, the same rebind goes through.
	if _, err := f.svc.Logout(ctx, f.a); err != nil {
		t.Fatal(err)
	}
	cfg.DefaultExtensionID = &other
	if _, err := f.svc.UpdateAgent(ctx, cfg); err != nil {
		t.Errorf("rebinding a signed-out agent: %v", err)
	}
}

func TestRestoreSignsOutAPresenceAtAnExtensionThatIsNotTheBoundOne(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	now := f.svc.now()
	// a is signed in at a colleague's phone (the override that no longer
	// exists); b is signed in at their own.
	f.store.presence[f.a] = Presence{State: StateReady, ExtensionNumber: "1002", EnteredAt: now}
	f.store.presence[f.b] = Presence{State: StateReady, ExtensionNumber: "1002", EnteredAt: now}

	if err := f.svc.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.svc.Presence(f.a).IsLoggedOut() {
		t.Errorf("a stayed %s at a phone that is not theirs", f.svc.Presence(f.a).CurrentState())
	}
	if got := f.store.presence[f.a]; !got.IsLoggedOut() || got.ExtensionNumber != "" {
		t.Errorf("persisted presence = %+v, want LOGGED_OUT without an extension", got)
	}
	if !f.sw.seen("status agent-a Logged Out") {
		t.Errorf("the switch was not told a is logged out: %v", f.sw.commands)
	}
	if st := f.svc.Presence(f.b); st.CurrentState() != StateReady || st.ExtensionNumber != "1002" {
		t.Errorf("b = %+v, want READY at 1002", st)
	}
	if n := len(f.pub.all()); n != 0 {
		t.Errorf("restore published %d events, want none", n)
	}
}

func TestTheRosterAnswersFromTheBoundPhoneSignedInOrNot(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	f.svc.ObserveDevice(ctx, "1001", SignalRegistered) // a: signed out, registered
	f.svc.ObserveDevice(ctx, "1002", SignalRegistered) // b: signed in, registered
	f.svc.ObserveDevice(ctx, "1003", SignalRegistered)
	f.svc.ObserveDevice(ctx, "1003", SignalUnregistered) // c: gone
	if _, err := f.svc.Login(ctx, f.b); err != nil {
		t.Fatal(err)
	}

	rows, err := f.svc.Roster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]bool{f.a: true, f.b: true, f.c: false, f.d: false}
	for _, row := range rows {
		if row.IsRegistered != want[row.AgentID] {
			t.Errorf("agent %s: isRegistered = %v, want %v", f.name(row.AgentID), row.IsRegistered, want[row.AgentID])
		}
	}
}

// Every event the service publishes agrees with the device table at the
// agent's bound extension, whoever receives it, through an interleaving of
// sign-ins, sign-outs and signals including duplicates.
func TestEveryPublishedPayloadAgreesWithThePhoneAndGoesToItsOwnAgent(t *testing.T) {
	t.Parallel()
	f := newPhones(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(106))
	agents := []uuid.UUID{f.a, f.b, f.c}
	exts := []string{"1001", "1002", "1003", "1004"}
	boundTo := map[string]uuid.UUID{"1001": f.a, "1002": f.b, "1003": f.c}
	signals := []DeviceSignal{SignalRegistered, SignalReachable, SignalUnreachable, SignalUnregistered}

	for step := range 400 {
		told := len(f.pub.all())
		var signalled string
		var signal DeviceSignal
		switch rng.Intn(4) {
		case 0:
			_, _ = f.svc.Login(ctx, agents[rng.Intn(3)])
		case 1:
			_, _ = f.svc.Logout(ctx, agents[rng.Intn(3)])
		default:
			signalled, signal = exts[rng.Intn(len(exts))], signals[rng.Intn(len(signals))]
			f.svc.ObserveDevice(ctx, signalled, signal)
		}
		for _, s := range f.pub.all()[told:] {
			if len(s.scope.AgentIDs) != 1 || *s.ev.AgentID != s.scope.AgentIDs[0] {
				t.Fatalf("step %d: %s addressed to %v for agent %v", step, s.ev.Type, s.scope.AgentIDs, s.ev.AgentID)
			}
			owner := s.scope.AgentIDs[0]
			bound := f.store.profiles[owner].ExtensionNumber
			if isDeviceEvent(s.ev.Type) {
				if boundTo[signalled] != owner {
					t.Fatalf("step %d: %s for %s reached %s", step, s.ev.Type, signalled, f.name(owner))
				}
				if s.ev.Type != deviceType(signal) {
					t.Fatalf("step %d: %s, want %s", step, s.ev.Type, deviceType(signal))
				}
				reg, _ := signal.state()
				checkDevicePayload(t, s.ev, reg, bound)
				continue
			}
			reg, _, _ := f.svc.DeviceAtExtension(bound)
			checkDevicePayload(t, s.ev, reg, bound)
		}
	}
}
