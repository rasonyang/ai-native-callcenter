// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

type fakeStore struct {
	mu        sync.Mutex
	presence  map[uuid.UUID]Presence
	profiles  map[uuid.UUID]Profile
	logs      int
	saveErr   error
	profErr   error
	rosterErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		presence: map[uuid.UUID]Presence{},
		profiles: map[uuid.UUID]Profile{},
	}
}

func (f *fakeStore) LoadPresence(_ context.Context, id uuid.UUID) (Presence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.presence[id], nil
}

func (f *fakeStore) SavePresence(_ context.Context, id uuid.UUID, p Presence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.presence[id] = p
	return nil
}

func (f *fakeStore) LogStateChange(context.Context, uuid.UUID, Presence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs++
	return nil
}

func (f *fakeStore) AgentProfile(_ context.Context, id uuid.UUID) (Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.profErr != nil {
		return Profile{}, f.profErr
	}
	p, ok := f.profiles[id]
	if !ok {
		return Profile{}, errors.New("no such agent")
	}
	return p, nil
}

func (f *fakeStore) CreateAgent(_ context.Context, cfg AgentConfig) (AgentConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg.AgentID = uuid.New()
	f.profiles[cfg.AgentID] = Profile{
		AgentID:        cfg.AgentID,
		UserID:         cfg.UserID,
		CallcenterName: cfg.CallcenterName,
		IsAutoAnswer:   cfg.IsAutoAnswer,
	}
	return cfg, nil
}

func (f *fakeStore) UpdateAgent(_ context.Context, cfg AgentConfig) (AgentConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prof, ok := f.profiles[cfg.AgentID]
	if !ok {
		return AgentConfig{}, errors.New("no such agent")
	}
	prof.CallcenterName = cfg.CallcenterName
	prof.IsAutoAnswer = cfg.IsAutoAnswer
	f.profiles[cfg.AgentID] = prof
	return cfg, nil
}

func (f *fakeStore) DeleteAgent(_ context.Context, agentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.profiles, agentID)
	delete(f.presence, agentID)
	return nil
}

func (f *fakeStore) Roster(context.Context) ([]RosterEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rosterErr != nil {
		return nil, f.rosterErr
	}
	out := make([]RosterEntry, 0, len(f.profiles))
	for id, prof := range f.profiles {
		p := f.presence[id]
		entry := RosterEntry{
			AgentID: id, UserID: prof.UserID, DisplayName: prof.DisplayName,
			State: p.CurrentState(), Reason: p.Reason, Extension: p.ExtensionNumber,
			EnteredAt: p.EnteredAt, WrapUpCallID: p.WrapUpCallID,
		}
		out = append(out, entry)
	}
	return out, nil
}

type fakeSwitch struct {
	mu       sync.Mutex
	commands []string
	up       bool
}

func (f *fakeSwitch) record(format string, args ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, strings.TrimSpace(fmt.Sprintf(format, args...)))
	return nil
}

func (f *fakeSwitch) AddCallcenterAgent(name string) error {
	return f.record("agent add %s", name)
}

func (f *fakeSwitch) SetCallcenterAgentContact(name, ext string, auto bool) error {
	return f.record("contact %s %s auto=%v", name, ext, auto)
}

func (f *fakeSwitch) SetCallcenterAgentStatus(name, status string) error {
	return f.record("status %s %s", name, status)
}

func (f *fakeSwitch) SetCallcenterAgentWrapUp(name string, sec int) error {
	return f.record("wrapup %s %d", name, sec)
}

func (f *fakeSwitch) IsUp() bool { return f.up }

func (f *fakeSwitch) seen(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

type fakePublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (f *fakePublisher) Publish(_ context.Context, ev events.Event, _ events.Scope) events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return ev
}

func (f *fakePublisher) types() []events.Type {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]events.Type, 0, len(f.events))
	for _, ev := range f.events {
		out = append(out, ev.Type)
	}
	return out
}

func newTestService(t *testing.T) (*Service, *fakeStore, *fakeSwitch, *fakePublisher, uuid.UUID) {
	t.Helper()
	store := newFakeStore()
	sw := &fakeSwitch{up: true}
	pub := &fakePublisher{}

	agentID := uuid.New()
	store.profiles[agentID] = Profile{
		AgentID: agentID, UserID: uuid.New(), CallcenterName: "agent-1001",
		DisplayName: "Wei",
	}
	return NewService(store, sw, pub), store, sw, pub, agentID
}

func TestLoginPersistsMirrorsAndPublishes(t *testing.T) {
	svc, store, sw, pub, agentID := newTestService(t)
	ctx := context.Background()

	p, err := svc.Login(ctx, agentID, "1001")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if p.State != StateNotReady || p.Reason != ReasonLogin {
		t.Errorf("presence = %s(%s), want NOT_READY(LOGIN)", p.State, p.Reason)
	}

	saved, _ := store.LoadPresence(ctx, agentID)
	if saved.State != StateNotReady || saved.ExtensionNumber != "1001" {
		t.Errorf("persisted presence = %+v", saved)
	}
	if !sw.seen("agent add agent-1001") {
		t.Error("agent was not registered with the switch")
	}
	if !sw.seen("contact agent-1001 1001") {
		t.Error("agent contact was not set on the switch")
	}
	if !sw.seen("wrapup agent-1001 0") {
		t.Error("switch wrap-up timer was not disabled: after-call work is ours")
	}
	if !sw.seen("status agent-1001 On Break") {
		t.Error("switch status was not mirrored")
	}
	if got := pub.types(); len(got) != 1 || got[0] != events.TypeAgentLoggedIn {
		t.Errorf("published %v, want one AGENT_LOGGED_IN", got)
	}
}

func TestReadyMirrorsAvailableToTheSwitch(t *testing.T) {
	svc, _, sw, pub, agentID := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, agentID); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	if !sw.seen("status agent-1001 Available") {
		t.Error("ready was not mirrored as Available")
	}
	types := pub.types()
	if types[len(types)-1] != events.TypeAgentReady {
		t.Errorf("last event = %s, want AGENT_READY", types[len(types)-1])
	}
}

func TestStorageFailureRollsBackAndReportsError(t *testing.T) {
	svc, store, sw, _, agentID := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.saveErr = errors.New("database is down")
	store.mu.Unlock()

	before := svc.Presence(agentID)
	_, err := svc.Ready(ctx, agentID)
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("Ready() error = %v, want ErrStorage", err)
	}
	after := svc.Presence(agentID)
	if after.State != before.State {
		t.Errorf("state moved to %s despite the write failing: an agent must never be told they are ready when nothing would route to them", after.State)
	}
	if sw.seen("status agent-1001 Available") {
		t.Error("switch was told the agent is available although the change was not recorded")
	}
}

func TestOneExtensionOneAgent(t *testing.T) {
	svc, store, _, _, first := newTestService(t)
	ctx := context.Background()

	second := uuid.New()
	store.profiles[second] = Profile{AgentID: second, CallcenterName: "agent-1002", DisplayName: "Li"}

	if _, err := svc.Login(ctx, first, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, second, "1001"); !errors.Is(err, ErrExtensionInUse) {
		t.Fatalf("second Login() error = %v, want ErrExtensionInUse", err)
	}

	// Once the first agent signs out, the desk is free again.
	if _, err := svc.Logout(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, second, "1001"); err != nil {
		t.Fatalf("Login() after the desk was freed error = %v", err)
	}
}

// After-call work does not let go by itself. The agent files it, and until
// they do the switch keeps them out of routing — an agent released by a clock
// would be handed the next call while still writing up the last.
func TestWrapUpDoesNotEndByItself(t *testing.T) {
	store := newFakeStore()
	sw := &fakeSwitch{up: true}
	agentID := uuid.New()
	store.profiles[agentID] = Profile{
		AgentID: agentID, CallcenterName: "agent-1001", DisplayName: "Wei",
	}
	svc := NewService(store, sw, &fakePublisher{})
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartWrapUp(ctx, agentID, uuid.New()); err != nil {
		t.Fatalf("StartWrapUp() error = %v", err)
	}
	if got := svc.Presence(agentID); got.Reason != ReasonAfterCallWork {
		t.Fatalf("reason = %s, want AFTER_CALL_WORK", got.Reason)
	}
	// The switch is told at once: "no new calls during after-call work" is the
	// mirror's job, not the browser's.
	if !sw.seen("status agent-1001 On Break") {
		t.Errorf("the switch was told %v, want the agent taken out of routing", sw.commands)
	}

	time.Sleep(300 * time.Millisecond)
	if got := svc.Presence(agentID); got.State != StateNotReady || got.Reason != ReasonAfterCallWork {
		t.Fatalf("presence = %s(%s) without anybody filing anything", got.State, got.Reason)
	}

	if _, err := svc.EndWrapUp(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	if got := svc.Presence(agentID); got.State != StateReady {
		t.Errorf("state = %s after filing, want READY", got.State)
	}
	if !sw.seen("status agent-1001 Available") {
		t.Errorf("the switch was told %v, want the agent routable again", sw.commands)
	}
}

func TestDeviceObservationAffectsAvailability(t *testing.T) {
	svc, _, _, _, agentID := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	svc.ObserveDevice(ctx, "1001", true, true)
	if got := svc.Presence(agentID).Availability(); got != AvailReady {
		t.Errorf("availability = %s, want READY", got)
	}

	// The phone stops answering keepalives while still registered.
	svc.ObserveDevice(ctx, "1001", true, false)
	if got := svc.Presence(agentID).Availability(); got != AvailDeviceUnreachable {
		t.Errorf("availability = %s, want DEVICE_UNREACHABLE", got)
	}
}

func TestRosterResolvesAvailability(t *testing.T) {
	svc, _, _, _, agentID := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, agentID); err != nil {
		t.Fatal(err)
	}
	svc.ObserveDevice(ctx, "1001", true, true)
	svc.SetOnCall(ctx, agentID, true)

	rows, err := svc.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("roster has %d rows", len(rows))
	}
	if rows[0].Availability != AvailOnCall {
		t.Errorf("availability = %s, want ON_CALL to outrank ready", rows[0].Availability)
	}
	if !rows[0].IsRegistered {
		t.Error("roster lost the registration fact")
	}
}

func TestSwitchDownDoesNotBlockSignIn(t *testing.T) {
	store := newFakeStore()
	sw := &fakeSwitch{up: false} // switch link is down
	agentID := uuid.New()
	store.profiles[agentID] = Profile{AgentID: agentID, CallcenterName: "agent-1001"}
	svc := NewService(store, sw, &fakePublisher{})

	if _, err := svc.Login(context.Background(), agentID, "1001"); err != nil {
		t.Fatalf("Login() error = %v, want sign-in to succeed with the switch down", err)
	}
	if len(sw.commands) != 0 {
		t.Errorf("commands were sent to a down switch: %v", sw.commands)
	}
}

func TestSyncSwitchRebuildsAfterReconnect(t *testing.T) {
	svc, _, sw, _, agentID := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	sw.mu.Lock()
	sw.commands = nil // the switch restarted and forgot everything
	sw.mu.Unlock()

	svc.SyncSwitch(ctx)
	if !sw.seen("agent add agent-1001") || !sw.seen("contact agent-1001 1001") {
		t.Errorf("presence was not rebuilt on the switch: %v", sw.commands)
	}
}

func TestUnknownAgentIsRejected(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	if _, err := svc.Login(context.Background(), uuid.New(), "1001"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("Login() error = %v, want ErrUnknownAgent", err)
	}
}

// A phone the switch already told us about must count the moment its agent
// signs in. Live events describe changes only, so an agent signing in at a
// phone that registered earlier would otherwise read as unreachable and be
// treated as unroutable.
func TestLoginAdoptsAlreadyKnownDeviceState(t *testing.T) {
	svc, _, _, _, agentID := newTestService(t)
	ctx := context.Background()

	// The reconciliation on connect saw this phone before anyone signed in.
	svc.ObserveDevice(ctx, "1001", true, true)

	if _, err := svc.Login(ctx, agentID, "1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, agentID); err != nil {
		t.Fatal(err)
	}

	if got := svc.Presence(agentID).Availability(); got != AvailReady {
		t.Errorf("availability = %s, want READY: the phone was known to be registered", got)
	}
}
