// SPDX-License-Identifier: Apache-2.0

// Package agents owns agent presence: the state machine, its persistence, and
// the mirror into mod_callcenter.
package agents

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// State is an agent's routing intention. It never describes call media: a
// talking agent is READY-with-a-call, and the call's own state lives on the
// party.
type State string

// Agent states.
const (
	StateLoggedOut State = "LOGGED_OUT"
	StateNotReady  State = "NOT_READY"
	StateReady     State = "READY"
)

// Reason qualifies NOT_READY.
type Reason string

// Not-ready reasons.
const (
	ReasonLogin         Reason = "LOGIN"
	ReasonBreak         Reason = "BREAK"
	ReasonLunch         Reason = "LUNCH"
	ReasonTraining      Reason = "TRAINING"
	ReasonAfterCallWork Reason = "AFTER_CALL_WORK"
	ReasonSystem        Reason = "SYSTEM"
	ReasonSupervisor    Reason = "SUPERVISOR"
)

var validReasons = map[Reason]bool{
	ReasonLogin: true, ReasonBreak: true, ReasonLunch: true,
	ReasonTraining: true, ReasonAfterCallWork: true,
	ReasonSystem: true, ReasonSupervisor: true,
}

// Valid reports whether r is a known reason.
func (r Reason) Valid() bool { return validReasons[r] }

// Availability is the single word that answers "could this agent take a call,
// and if not, why". It is derived on read and never stored.
type Availability string

// Availability values, in precedence order.
const (
	AvailLoggedOut         Availability = "LOGGED_OUT"
	AvailOnCall            Availability = "ON_CALL"
	AvailWrapUp            Availability = "WRAP_UP"
	AvailNotReady          Availability = "NOT_READY"
	AvailDeviceUnreachable Availability = "DEVICE_UNREACHABLE"
	AvailReady             Availability = "READY"
)

// Presence is one agent's live presence.
type Presence struct {
	State  State
	Reason Reason
	// ExtensionNumber is bound at login, not in configuration: agents are
	// free-seating, so the desk is whichever phone they signed in on.
	ExtensionNumber string
	EnteredAt       time.Time
	// WrapUpCallID is the call the after-call work is for, so what the agent
	// files lands on that call and not on whichever one they take next.
	WrapUpCallID *uuid.UUID

	// Observed facts, not intentions.
	IsOnCall          bool
	IsRegistered      bool
	IsDeviceInService bool
}

// Errors returned by presence transitions.
var (
	ErrNotLoggedIn     = fmt.Errorf("agent is not logged in")
	ErrAlreadyLoggedIn = fmt.Errorf("agent is already logged in")
	ErrUnknownReason   = fmt.Errorf("unknown not-ready reason")
)

// CurrentState normalizes the state so the zero value of Presence is a
// logged-out agent rather than an unusable one.
func (p Presence) CurrentState() State {
	if p.State == "" {
		return StateLoggedOut
	}
	return p.State
}

// IsLoggedOut reports whether the agent is signed out.
func (p Presence) IsLoggedOut() bool { return p.CurrentState() == StateLoggedOut }

// Login binds the agent to an extension and lands in NOT_READY(LOGIN): nobody
// is handed a call the instant they sign in.
func (p *Presence) Login(extensionNumber string, at time.Time) error {
	if !p.IsLoggedOut() {
		return ErrAlreadyLoggedIn
	}
	p.State = StateNotReady
	p.Reason = ReasonLogin
	p.ExtensionNumber = extensionNumber
	p.EnteredAt = at
	p.clearWrapUp()
	return nil
}

// Logout releases the extension.
func (p *Presence) Logout(at time.Time) error {
	if p.IsLoggedOut() {
		return ErrNotLoggedIn
	}
	p.State = StateLoggedOut
	p.Reason = ""
	p.ExtensionNumber = ""
	p.EnteredAt = at
	p.clearWrapUp()
	return nil
}

// Ready makes the agent routable, ending any wrap-up early.
func (p *Presence) Ready(at time.Time) error {
	if p.IsLoggedOut() {
		return ErrNotLoggedIn
	}
	p.State = StateReady
	p.Reason = ""
	p.EnteredAt = at
	p.clearWrapUp()
	return nil
}

// NotReady takes the agent out of routing with a reason.
func (p *Presence) NotReady(reason Reason, at time.Time) error {
	if p.IsLoggedOut() {
		return ErrNotLoggedIn
	}
	if !reason.Valid() {
		return ErrUnknownReason
	}
	p.State = StateNotReady
	p.Reason = reason
	p.EnteredAt = at
	p.clearWrapUp()
	return nil
}

// StartWrapUp begins after-call work for the call that just ended.
//
// It has no deadline. After-call work ends when the agent files it, which is
// the only thing that can end it: a clock that released them would make the
// disposition optional in practice, and the routing they are kept out of is
// the point — an agent still writing up the last call is not ready for the
// next one. EnteredAt is when the call ended, so the screen counts up.
func (p *Presence) StartWrapUp(callID uuid.UUID, at time.Time) error {
	if p.IsLoggedOut() {
		return ErrNotLoggedIn
	}
	p.State = StateNotReady
	p.Reason = ReasonAfterCallWork
	p.EnteredAt = at
	p.WrapUpCallID = nil
	if callID != uuid.Nil {
		id := callID
		p.WrapUpCallID = &id
	}
	return nil
}

// IsInWrapUp reports whether the agent is doing after-call work right now.
func (p Presence) IsInWrapUp() bool {
	return p.State == StateNotReady && p.Reason == ReasonAfterCallWork
}

// clearWrapUp forgets which call the after-call work was for: every transition
// out of it ends it.
func (p *Presence) clearWrapUp() {
	p.WrapUpCallID = nil
}

// RingNoAnswer takes an agent out of routing after they ignored a delivered
// call, so one unattended phone stops absorbing the queue.
func (p *Presence) RingNoAnswer(at time.Time) error {
	if p.IsLoggedOut() {
		return ErrNotLoggedIn
	}
	return p.NotReady(ReasonSystem, at)
}

// Availability derives the wallboard word.
func (p Presence) Availability() Availability {
	switch {
	case p.IsLoggedOut():
		return AvailLoggedOut
	case p.IsOnCall:
		return AvailOnCall
	case p.State == StateNotReady && p.Reason == ReasonAfterCallWork:
		return AvailWrapUp
	case p.State == StateNotReady:
		return AvailNotReady
	case !p.IsRegistered || !p.IsDeviceInService:
		// Registered-but-dead phones are the number one agent-side failure:
		// a crashed browser tab looks exactly like a working one.
		return AvailDeviceUnreachable
	default:
		return AvailReady
	}
}

// CallcenterStatus maps our presence onto the three statuses mod_callcenter
// understands. Our states are richer, so several map onto "On Break": the
// distinction is ours to keep, not the switch's.
//
// Wanting calls is not the same as being able to take them. An agent whose
// phone has stopped answering is still READY — losing a phone says nothing
// about their intent — but the switch must not keep offering to a number that
// will not ring. Reading only the state is how a queue came to deliver every
// call to a dead browser tab, each one ringing out to timeout before being
// offered again, with nothing on any screen to say why.
func (p Presence) CallcenterStatus() string {
	switch p.CurrentState() {
	case StateReady:
		if !p.IsRegistered || !p.IsDeviceInService {
			return "On Break"
		}
		return "Available"
	case StateNotReady:
		return "On Break"
	default:
		return "Logged Out"
	}
}
