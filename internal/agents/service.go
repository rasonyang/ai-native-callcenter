// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

	// Configuration, as administration edits it.
	CreateAgent(ctx context.Context, cfg AgentConfig) (AgentConfig, error)
	UpdateAgent(ctx context.Context, cfg AgentConfig) (AgentConfig, error)
	DeleteAgent(ctx context.Context, agentID uuid.UUID) error
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
	IsAutoAnswer   bool
	// ExtensionNumber is the phone this agent is bound to in configuration.
	// The binding is static: an agent signs in at their own extension and
	// nowhere else, so sign-in never asks which phone they are at.
	ExtensionNumber string
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
	WrapUpCallID *uuid.UUID   `json:"wrapUpCallId,omitempty"`
	IsOnCall     bool         `json:"isOnCall"`
	IsRegistered bool         `json:"isRegistered"`

	// Configuration. Extension is the phone the agent is signed in at right
	// now; DefaultExtension is the one bound to them, which survives sign-out
	// and is what administration edits.
	CallcenterName         string     `json:"callcenterName"`
	IsAutoAnswer           bool       `json:"isAutoAnswer"`
	DefaultExtensionID     *uuid.UUID `json:"defaultExtensionId,omitempty"`
	DefaultExtensionNumber string     `json:"defaultExtensionNumber,omitempty"`
}

// AgentConfig is the configuration behind one agent, as administration edits
// it. Presence lives on RosterEntry; this is what is stored.
type AgentConfig struct {
	AgentID            uuid.UUID  `json:"agentId"`
	UserID             uuid.UUID  `json:"userId"`
	CallcenterName     string     `json:"callcenterName"`
	IsAutoAnswer       bool       `json:"isAutoAnswer"`
	DefaultExtensionID *uuid.UUID `json:"defaultExtensionId,omitempty"`
}

// Errors returned by the service.
var (
	ErrStorage        = errors.New("cannot record agent state")
	ErrExtensionInUse = errors.New("extension is already in use")
	ErrUnknownAgent   = errors.New("unknown agent")
	// ErrNoExtensionBound means configuration never gave this agent a phone,
	// so there is nothing for them to sign in at.
	ErrNoExtensionBound = errors.New("no extension bound to this agent")
	// ErrValidation is a rejected configuration change.
	ErrValidation = errors.New("invalid agent configuration")
)

// Service owns agent presence.
//
// Staffing reconciles an agent's queue membership on the switch.
//
// Declared here rather than imported because the two packages are siblings:
// presence knows when an agent becomes addressable, staffing knows which
// queues they belong to, and neither needs the other's types to say so.
type Staffing interface {
	ReconcileAgentTiers(ctx context.Context, agentID uuid.UUID)
}

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
	// lastWrapUpCall is the call each agent most recently began after-call
	// work for. It outlives the wrap-up window on purpose: an agent whose
	// timer ran out while they were still typing the note has not lost the
	// right to file it, and the call it belongs to is this one until the
	// next wrap-up begins.
	lastWrapUpCall map[uuid.UUID]uuid.UUID

	now func() time.Time

	// staffing is optional: without it presence still mirrors, and an agent
	// staffed while signed out simply stays unroutable until the next
	// reconnect — which is the behaviour this field exists to remove.
	staffing Staffing
}

// AttachStaffing points agent registration at queue reconciliation.
func (s *Service) AttachStaffing(st Staffing) { s.staffing = st }

type deviceState struct {
	isRegistered bool
	isInService  bool
}

// NewService builds a Service.
func NewService(store Store, switchCtl SwitchControl, pub Publisher) *Service {
	return &Service{
		store:          store,
		switchCtl:      switchCtl,
		pub:            pub,
		live:           make(map[uuid.UUID]*Presence),
		devices:        make(map[string]deviceState),
		lastWrapUpCall: make(map[uuid.UUID]uuid.UUID),
		now:            func() time.Time { return time.Now().UTC() },
	}
}

// Login signs an agent in at an extension.
func (s *Service) Login(ctx context.Context, agentID uuid.UUID, extensionNumber string) (Presence, error) {
	profile, err := s.store.AgentProfile(ctx, agentID)
	if err != nil {
		return Presence{}, fmt.Errorf("%w: %w", ErrUnknownAgent, err)
	}

	// The agent↔extension binding is static configuration. A caller may still
	// name an extension explicitly, but the ordinary sign-in sends none and
	// lands on the phone the agent is bound to.
	if extensionNumber == "" {
		extensionNumber = profile.ExtensionNumber
	}
	if extensionNumber == "" {
		return Presence{}, ErrNoExtensionBound
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

// Logout signs an agent out. Whatever call they last wrapped up is forgotten
// with the session: the next sign-in starts with nothing to file.
func (s *Service) Logout(ctx context.Context, agentID uuid.UUID) (Presence, error) {
	p, err := s.change(ctx, agentID, events.TypeAgentLoggedOut, func(p *Presence) error {
		return p.Logout(s.now())
	})
	if err == nil {
		s.mu.Lock()
		delete(s.lastWrapUpCall, agentID)
		s.mu.Unlock()
	}
	return p, err
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

// StartWrapUp begins after-call work for a call the agent just finished.
//
// It ends when the agent files it and at no other time. Nothing here schedules
// a release: the switch mirror takes them out of routing for as long as they
// are in it, which is what "do not deliver new calls during after-call work"
// means, and a timer that let go early would deliver one to somebody still
// writing up the last.
func (s *Service) StartWrapUp(ctx context.Context, agentID, callID uuid.UUID) (Presence, error) {
	if callID != uuid.Nil {
		s.mu.Lock()
		s.lastWrapUpCall[agentID] = callID
		s.mu.Unlock()
	}

	return s.change(ctx, agentID, events.TypeAgentNotReady, func(p *Presence) error {
		return p.StartWrapUp(callID, s.now())
	})
}

// BeginAfterCallWork starts an agent's wrap-up for a call whose agent leg has
// just ended.
//
// The fire-and-forget form of StartWrapUp, for the switch path: a call is over
// whether or not presence could be recorded, and there is nobody on that path
// to hand a failure to. An agent who is not signed in — the leg outlived their
// session — is not an error either, which is why this reports nothing.
func (s *Service) BeginAfterCallWork(ctx context.Context, agentID, callID uuid.UUID) {
	if _, err := s.StartWrapUp(ctx, agentID, callID); err != nil {
		slog.WarnContext(ctx, "after-call work not started",
			"agentId", agentID, "callId", callID, "error", err)
	}
}

// WrapUpCall reports the call the agent most recently began after-call work
// for. It stays addressable after they have moved on, so a filing made late
// still lands on the right call; it is forgotten at sign-out.
func (s *Service) WrapUpCall(agentID uuid.UUID) (uuid.UUID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.lastWrapUpCall[agentID]; ok {
		return id, true
	}
	// After a restart the memory is empty, but a wrap-up that was under way
	// was persisted with its call.
	if p := s.presenceLocked(agentID); p.WrapUpCallID != nil {
		return *p.WrapUpCallID, true
	}
	return uuid.Nil, false
}

// EndWrapUp returns an agent to READY if they are still in after-call work.
// An agent whose window already expired, or who chose something else in the
// meantime, is left exactly where they are: the filing they just made is what
// mattered, and their presence is not to be second-guessed by it.
func (s *Service) EndWrapUp(ctx context.Context, agentID uuid.UUID) (Presence, error) {
	s.mu.Lock()
	inWrapUp := s.presenceLocked(agentID).IsInWrapUp()
	s.mu.Unlock()
	if !inWrapUp {
		return s.Presence(agentID), nil
	}
	return s.Ready(ctx, agentID)
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
// CreateAgent gives a user an agent identity: a callcenter name the switch
// knows them by, and the phone they are bound to.
func (s *Service) CreateAgent(ctx context.Context, cfg AgentConfig) (AgentConfig, error) {
	cfg, err := normalizeAgentConfig(cfg)
	if err != nil {
		return AgentConfig{}, err
	}
	out, err := s.store.CreateAgent(ctx, cfg)
	if err != nil {
		return AgentConfig{}, err
	}
	s.mirrorConfig(ctx, out.AgentID)
	return out, nil
}

// UpdateAgent rewrites one agent's configuration, rebinding their phone.
func (s *Service) UpdateAgent(ctx context.Context, cfg AgentConfig) (AgentConfig, error) {
	cfg, err := normalizeAgentConfig(cfg)
	if err != nil {
		return AgentConfig{}, err
	}
	out, err := s.store.UpdateAgent(ctx, cfg)
	if err != nil {
		return AgentConfig{}, err
	}
	s.mirrorConfig(ctx, out.AgentID)
	return out, nil
}

// DeleteAgent removes the agent identity. Presence is dropped with it, so a
// signed-in agent stops being addressable.
func (s *Service) DeleteAgent(ctx context.Context, agentID uuid.UUID) error {
	if err := s.store.DeleteAgent(ctx, agentID); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.live, agentID)
	delete(s.lastWrapUpCall, agentID)
	s.mu.Unlock()
	return nil
}

// normalizeAgentConfig applies the defaults and rejects what the switch cannot
// carry. The callcenter name reaches mod_callcenter as an identifier, so it
// stays to characters that survive that trip.
func normalizeAgentConfig(cfg AgentConfig) (AgentConfig, error) {
	cfg.CallcenterName = strings.TrimSpace(cfg.CallcenterName)
	if cfg.CallcenterName == "" {
		return cfg, fmt.Errorf("%w: callcenterName is required", ErrValidation)
	}
	for _, r := range cfg.CallcenterName {
		if r == '@' || r == ' ' || r == '\'' {
			return cfg, fmt.Errorf("%w: callcenterName cannot contain spaces, @ or quotes", ErrValidation)
		}
	}
	return cfg, nil
}

// mirrorConfig pushes a changed binding to the switch, so a rebound phone
// takes calls without waiting for the agent to sign in again.
func (s *Service) mirrorConfig(ctx context.Context, agentID uuid.UUID) {
	profile, err := s.store.AgentProfile(ctx, agentID)
	if err != nil {
		return
	}
	s.mu.Lock()
	p := *s.presenceLocked(agentID)
	s.mu.Unlock()
	if p.ExtensionNumber == "" {
		p.ExtensionNumber = profile.ExtensionNumber
	}
	s.mirrorRegistration(profile, p)
}

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
		if row.WrapUpCallID != nil {
			id := *row.WrapUpCallID
			p.WrapUpCallID = &id
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

	// Now, and not before: mod_callcenter refuses a tier for an agent it does
	// not know, so this is the first moment one can succeed. An agent staffed
	// while signed out has no tier until here, and without a tier they are
	// Available, in a queue, and offered nothing.
	//
	// The caller's lock is already released at every site that reaches this,
	// which matters because reconciliation reads the database and the switch:
	// holding presence while doing either would serialise every sign-in behind
	// them.
	if s.staffing != nil {
		s.staffing.ReconcileAgentTiers(context.Background(), profile.AgentID)
	}
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
	if p.WrapUpCallID != nil {
		payload["wrapUpCallId"] = *p.WrapUpCallID
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

// AgentAtExtension reports which agent is signed in at an extension, so a leg
// being delivered there can be attributed to them.
func (s *Service) AgentAtExtension(extensionNumber string) (uuid.UUID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.extensionHolderLocked(extensionNumber)
}

// AgentByCallcenterName resolves the switch's own name for an agent.
func (s *Service) AgentByCallcenterName(name string) (uuid.UUID, bool) {
	s.mu.Lock()
	ids := make([]uuid.UUID, 0, len(s.live))
	for id := range s.live {
		ids = append(ids, id)
	}
	s.mu.Unlock()

	ctx := context.Background()
	for _, id := range ids {
		if profile, err := s.store.AgentProfile(ctx, id); err == nil && profile.CallcenterName == name {
			return id, true
		}
	}
	return uuid.Nil, false
}

// DeviceState reports what the switch has told us about an agent's phone.
func (s *Service) DeviceState(agentID uuid.UUID) (isRegistered, isInService bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.presenceLocked(agentID)
	return p.IsRegistered, p.IsDeviceInService
}
