// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// Store is the persistence this service needs. *queries.Queries satisfies it
// through a thin adapter, so the service can be tested without a database.
type Store interface {
	LoadPresence(ctx context.Context, agentID uuid.UUID) (Presence, error)
	SavePresence(ctx context.Context, agentID uuid.UUID, p Presence) error
	LogStateChange(ctx context.Context, agentID uuid.UUID, p Presence) error
	AgentProfile(ctx context.Context, agentID uuid.UUID) (Profile, error)
	Roster(ctx context.Context) ([]RosterEntry, error)
}

// SwitchControl is the switch-side mirror of agent presence.
type SwitchControl interface {
	AddCallcenterAgent(name string) error
	SetCallcenterAgentContact(name, extensionNumber string, autoAnswer bool) error
	SetCallcenterAgentStatus(name, status string) error
	SetCallcenterAgentWrapUp(name string, sec int) error
	IsUp() bool
}

// Publisher receives agent events.
type Publisher interface {
	Publish(ctx context.Context, ev events.Event, scope events.Scope) events.Event
}

// Profile is the configuration behind one agent.
type Profile struct {
	AgentID        uuid.UUID
	UserID         uuid.UUID
	CallcenterName string
	DisplayName    string
	WrapUpTimeSec  int
	IsAutoAnswer   bool
}

// RosterEntry is one row of the agent roster, with presence resolved.
type RosterEntry struct {
	AgentID      uuid.UUID    `json:"agentId"`
	UserID       uuid.UUID    `json:"userId"`
	Username     string       `json:"username"`
	DisplayName  string       `json:"displayName"`
	State        State        `json:"state"`
	Reason       Reason       `json:"reason,omitempty"`
	Availability Availability `json:"availability"`
	Extension    string       `json:"extensionNumber,omitempty"`
	EnteredAt    time.Time    `json:"enteredAt"`
	WrapUpEndsAt *time.Time   `json:"wrapUpEndsAt,omitempty"`
	IsOnCall     bool         `json:"isOnCall"`
	IsRegistered bool         `json:"isRegistered"`
}

// Errors returned by the service.
var (
	ErrStorage        = errors.New("cannot record agent state")
	ErrExtensionInUse = errors.New("extension is already in use")
	ErrUnknownAgent   = errors.New("unknown agent")
)

// Service owns agent presence.
//
// Presence is written to the database inside the request that changes it:
// acknowledging a state change we could not record would tell an agent they
// are ready when nothing would route to them. The switch mirror is best
// effort by comparison - it is rebuilt on reconnect - so a switch that is down
// degrades an agent to unroutable rather than failing their sign-in.
type Service struct {
	store     Store
	switchCtl SwitchControl
	pub       Publisher

	mu sync.Mutex
	// live is the in-memory presence, authoritative for reads between writes.
	live map[uuid.UUID]*Presence
	// devices maps extension number to its observed reachability.
	devices map[string]deviceState
	// wrapUpGen guards against a wrap-up timer that fires after the agent has
	// already chosen something else.
	wrapUpGen map[uuid.UUID]uint64

	now func() time.Time
}

type deviceState struct {
	isRegistered bool
	isInService  bool
}

// NewService builds a Service.
func NewService(store Store, switchCtl SwitchControl, pub Publisher) *Service {
	return &Service{
		store:     store,
		switchCtl: switchCtl,
		pub:       pub,
		live:      make(map[uuid.UUID]*Presence),
		devices:   make(map[string]deviceState),
		wrapUpGen: make(map[uuid.UUID]uint64),
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// Login signs an agent in at an extension.
func (s *Service) Login(ctx context.Context, agentID uuid.UUID, extensionNumber string) (Presence, error) {
	profile, err := s.store.AgentProfile(ctx, agentID)
	if err != nil {
		return Presence{}, fmt.Errorf("%w: %w", ErrUnknownAgent, err)
	}

	s.mu.Lock()
	if holder, busy := s.extensionHolderLocked(extensionNumber); busy && holder != agentID {
		s.mu.Unlock()
		return Presence{}, ErrExtensionInUse
	}
	p := s.presenceLocked(agentID)
	if err := p.Login(extensionNumber, s.now()); err != nil {
		s.mu.Unlock()
		return *p, err
	}
	// The switch already told us about this phone, either through a live
	// registration event or through the reconciliation on connect. Without
	// this an agent signing in at a perfectly good phone reads as
	// unreachable until the phone happens to re-register.
	s.applyDeviceLocked(p)
	snapshot := *p
	s.mu.Unlock()

	if err := s.persist(ctx, agentID, snapshot); err != nil {
		return Presence{}, err
	}

	// Registering with the switch is what makes the agent addressable at all,
	// so it happens on sign-in rather than on first ready.
	s.mirrorRegistration(profile, snapshot)
	s.publish(ctx, events.TypeAgentLoggedIn, profile, snapshot)
	return snapshot, nil
}

// Logout signs an agent out.
func (s *Service) Logout(ctx context.Context, agentID uuid.UUID) (Presence, error) {
	return s.change(ctx, agentID, events.TypeAgentLoggedOut, func(p *Presence) error {
		return p.Logout(s.now())
	})
}

// Ready makes an agent routable.
func (s *Service) Ready(ctx context.Context, agentID uuid.UUID) (Presence, error) {
	return s.change(ctx, agentID, events.TypeAgentReady, func(p *Presence) error {
		return p.Ready(s.now())
	})
}

// NotReady takes an agent out of routing.
func (s *Service) NotReady(ctx context.Context, agentID uuid.UUID, reason Reason) (Presence, error) {
	return s.change(ctx, agentID, events.TypeAgentNotReady, func(p *Presence) error {
		return p.NotReady(reason, s.now())
	})
}

// StartWrapUp begins after-call work and schedules its expiry.
func (s *Service) StartWrapUp(ctx context.Context, agentID uuid.UUID) (Presence, error) {
	profile, err := s.store.AgentProfile(ctx, agentID)
	if err != nil {
		return Presence{}, fmt.Errorf("%w: %w", ErrUnknownAgent, err)
	}
	wrapUp := time.Duration(profile.WrapUpTimeSec) * time.Second

	p, err := s.change(ctx, agentID, events.TypeAgentNotReady, func(p *Presence) error {
		return p.StartWrapUp(wrapUp, s.now())
	})
	if err != nil || wrapUp <= 0 {
		return p, err
	}

	s.mu.Lock()
	s.wrapUpGen[agentID]++
	generation := s.wrapUpGen[agentID]
	s.mu.Unlock()

	time.AfterFunc(wrapUp, func() { s.expireWrapUp(agentID, generation) })
	return p, nil
}

// expireWrapUp returns an agent to ready when their wrap-up window ends. A
// timer whose generation is stale belongs to a superseded wrap-up and is
// ignored, so an agent who chose lunch mid-wrap-up is never dragged back.
func (s *Service) expireWrapUp(agentID uuid.UUID, generation uint64) {
	ctx := context.Background()

	s.mu.Lock()
	if s.wrapUpGen[agentID] != generation {
		s.mu.Unlock()
		return
	}
	p := s.presenceLocked(agentID)
	if !p.ExpireWrapUp(s.now()) {
		s.mu.Unlock()
		return
	}
	snapshot := *p
	s.mu.Unlock()

	if err := s.persist(ctx, agentID, snapshot); err != nil {
		// A timer cannot fail a request, so the in-memory state stands and
		// the discrepancy is logged rather than lost.
		slog.ErrorContext(ctx, "wrap-up expiry not recorded", "agentId", agentID, "error", err)
	}
	if profile, err := s.store.AgentProfile(ctx, agentID); err == nil {
		s.mirrorStatus(profile, snapshot)
		s.publish(ctx, events.TypeAgentReady, profile, snapshot)
	}
}

// RingNoAnswer takes an agent out of routing after they ignored a call.
func (s *Service) RingNoAnswer(ctx context.Context, agentID uuid.UUID) (Presence, error) {
	return s.change(ctx, agentID, events.TypeAgentNotReady, func(p *Presence) error {
		return p.RingNoAnswer(s.now())
	})
}

// SetOnCall records that an agent is on a call, which outranks their presence
// when the roster is read.
func (s *Service) SetOnCall(ctx context.Context, agentID uuid.UUID, onCall bool) {
	s.mu.Lock()
	p := s.presenceLocked(agentID)
	if p.IsOnCall == onCall {
		s.mu.Unlock()
		return
	}
	p.IsOnCall = onCall
	snapshot := *p
	s.mu.Unlock()

	if profile, err := s.store.AgentProfile(ctx, agentID); err == nil {
		s.publish(ctx, events.TypeAgentAvailability, profile, snapshot)
	}
}

// ObserveDevice records what the switch reports about a phone.
//
// A registered phone that stops answering keepalives is the agent-side failure
// that matters most: a crashed browser tab looks exactly like a working one,
// so an agent can sit in ready while every call to them fails.
func (s *Service) ObserveDevice(ctx context.Context, extensionNumber string, isRegistered, isInService bool) {
	s.mu.Lock()
	s.devices[extensionNumber] = deviceState{isRegistered: isRegistered, isInService: isInService}

	var (
		agentID  uuid.UUID
		snapshot Presence
		found    bool
	)
	for id, p := range s.live {
		if p.ExtensionNumber == extensionNumber && !p.IsLoggedOut() {
			p.IsRegistered = isRegistered
			p.IsDeviceInService = isInService
			agentID, snapshot, found = id, *p, true
			break
		}
	}
	s.mu.Unlock()

	if !found {
		return
	}
	if profile, err := s.store.AgentProfile(ctx, agentID); err == nil {
		s.publish(ctx, events.TypeDeviceInService, profile, snapshot)
	}
}

// Presence returns an agent's live presence.
func (s *Service) Presence(agentID uuid.UUID) Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.presenceLocked(agentID)
}

// Roster returns every agent with presence and derived availability resolved.
func (s *Service) Roster(ctx context.Context) ([]RosterEntry, error) {
	rows, err := s.store.Roster(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStorage, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range rows {
		p := s.presenceLocked(rows[i].AgentID)
		// The database row is the durable truth; live memory carries the
		// observed facts the database never stores.
		p.State, p.Reason, p.ExtensionNumber = rows[i].State, rows[i].Reason, rows[i].Extension
		if dev, ok := s.devices[p.ExtensionNumber]; ok {
			p.IsRegistered, p.IsDeviceInService = dev.isRegistered, dev.isInService
		}
		rows[i].Availability = p.Availability()
		rows[i].IsOnCall = p.IsOnCall
		rows[i].IsRegistered = p.IsRegistered
	}
	return rows, nil
}

// Restore loads persisted presence at startup so a restart does not sign
// everyone out.
func (s *Service) Restore(ctx context.Context) error {
	rows, err := s.store.Roster(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStorage, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range rows {
		p := s.presenceLocked(row.AgentID)
		p.State, p.Reason, p.ExtensionNumber, p.EnteredAt = row.State, row.Reason, row.Extension, row.EnteredAt
		if row.WrapUpEndsAt != nil {
			p.WrapUpEndsAt = *row.WrapUpEndsAt
		}
		s.applyDeviceLocked(p)
	}
	return nil
}

// SyncSwitch rebuilds the switch's view of every signed-in agent. It runs on
// each reconnect, because the switch forgets agents when it restarts and we
// are the source of truth.
func (s *Service) SyncSwitch(ctx context.Context) {
	s.mu.Lock()
	type pending struct {
		agentID uuid.UUID
		p       Presence
	}
	work := make([]pending, 0, len(s.live))
	for id, p := range s.live {
		if !p.IsLoggedOut() {
			work = append(work, pending{agentID: id, p: *p})
		}
	}
	s.mu.Unlock()

	for _, w := range work {
		profile, err := s.store.AgentProfile(ctx, w.agentID)
		if err != nil {
			continue
		}
		s.mirrorRegistration(profile, w.p)
	}
	slog.InfoContext(ctx, "agent presence mirrored to the switch", "agents", len(work))
}

// change applies a presence transition, persists it, mirrors it and publishes.
func (s *Service) change(ctx context.Context, agentID uuid.UUID, eventType events.Type, apply func(*Presence) error) (Presence, error) {
	profile, err := s.store.AgentProfile(ctx, agentID)
	if err != nil {
		return Presence{}, fmt.Errorf("%w: %w", ErrUnknownAgent, err)
	}

	s.mu.Lock()
	p := s.presenceLocked(agentID)
	before := *p
	if err := apply(p); err != nil {
		s.mu.Unlock()
		return before, err
	}
	// Any explicit change supersedes a running wrap-up timer.
	s.wrapUpGen[agentID]++
	snapshot := *p
	s.mu.Unlock()

	if err := s.persist(ctx, agentID, snapshot); err != nil {
		// Roll back rather than report a state we could not record.
		s.mu.Lock()
		*s.presenceLocked(agentID) = before
		s.mu.Unlock()
		return before, err
	}

	s.mirrorStatus(profile, snapshot)
	s.publish(ctx, eventType, profile, snapshot)
	return snapshot, nil
}

func (s *Service) persist(ctx context.Context, agentID uuid.UUID, p Presence) error {
	if err := s.store.SavePresence(ctx, agentID, p); err != nil {
		return fmt.Errorf("%w: %w", ErrStorage, err)
	}
	if err := s.store.LogStateChange(ctx, agentID, p); err != nil {
		// History is for reporting: losing a row must not fail the agent's
		// request.
		slog.WarnContext(ctx, "agent state history not recorded", "agentId", agentID, "error", err)
	}
	return nil
}

// mirrorRegistration makes the switch aware of an agent and their phone.
func (s *Service) mirrorRegistration(profile Profile, p Presence) {
	if s.switchCtl == nil || !s.switchCtl.IsUp() {
		return
	}
	name := profile.CallcenterName
	if err := s.switchCtl.AddCallcenterAgent(name); err != nil {
		slog.Warn("callcenter agent add failed", "agent", name, "error", err)
	}
	// After-call work is ours, so the switch's own timer stays off.
	if err := s.switchCtl.SetCallcenterAgentWrapUp(name, 0); err != nil {
		slog.Warn("callcenter wrap-up reset failed", "agent", name, "error", err)
	}
	if p.ExtensionNumber != "" {
		if err := s.switchCtl.SetCallcenterAgentContact(name, p.ExtensionNumber, profile.IsAutoAnswer); err != nil {
			slog.Warn("callcenter contact update failed", "agent", name, "error", err)
		}
	}
	s.mirrorStatus(profile, p)
}

func (s *Service) mirrorStatus(profile Profile, p Presence) {
	if s.switchCtl == nil || !s.switchCtl.IsUp() {
		return
	}
	if err := s.switchCtl.SetCallcenterAgentStatus(profile.CallcenterName, p.CallcenterStatus()); err != nil {
		slog.Warn("callcenter status update failed", "agent", profile.CallcenterName, "error", err)
	}
}

func (s *Service) publish(ctx context.Context, t events.Type, profile Profile, p Presence) {
	if s.pub == nil {
		return
	}
	agentID := profile.AgentID
	payload := map[string]any{
		"state":        string(p.CurrentState()),
		"availability": string(p.Availability()),
		"displayName":  profile.DisplayName,
	}
	if p.Reason != "" {
		payload["reason"] = string(p.Reason)
	}
	if p.ExtensionNumber != "" {
		payload["extensionNumber"] = p.ExtensionNumber
	}
	if !p.WrapUpEndsAt.IsZero() {
		payload["wrapUpEndsAt"] = p.WrapUpEndsAt
	}
	s.pub.Publish(ctx, events.Event{
		Type:    t,
		AgentID: &agentID,
		Payload: payload,
	}, events.Scope{AgentIDs: []uuid.UUID{agentID}})
}

// applyDeviceLocked copies what the switch has told us about an agent's phone
// onto their presence. The caller must hold the mutex.
func (s *Service) applyDeviceLocked(p *Presence) {
	dev, known := s.devices[p.ExtensionNumber]
	if !known {
		return
	}
	p.IsRegistered, p.IsDeviceInService = dev.isRegistered, dev.isInService
}

// presenceLocked returns the mutable presence for an agent, creating it on
// first use. The caller must hold the mutex.
func (s *Service) presenceLocked(agentID uuid.UUID) *Presence {
	p, ok := s.live[agentID]
	if !ok {
		p = &Presence{}
		s.live[agentID] = p
	}
	return p
}

// extensionHolderLocked reports which agent currently occupies an extension.
func (s *Service) extensionHolderLocked(extensionNumber string) (uuid.UUID, bool) {
	for id, p := range s.live {
		if p.ExtensionNumber == extensionNumber && !p.IsLoggedOut() {
			return id, true
		}
	}
	return uuid.Nil, false
}
