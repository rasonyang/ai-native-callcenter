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
	// WrapUpEndsAt is when after-call work expires by itself.
	WrapUpEndsAt time.Time
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

// StartWrapUp begins after-call work for the configured duration, for the
// call that just ended. A zero or negative duration means the agent returns to
// READY immediately.
func (p *Presence) StartWrapUp(callID uuid.UUID, duration time.Duration, at time.Time) error {
	if p.IsLoggedOut() {
		return ErrNotLoggedIn
	}
	if duration <= 0 {
		return p.Ready(at)
	}
	p.State = StateNotReady
	p.Reason = ReasonAfterCallWork
	p.EnteredAt = at
	p.WrapUpEndsAt = at.Add(duration)
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

// clearWrapUp forgets the wrap-up window and its call: every transition out
// of after-call work, chosen or expired, ends both.
func (p *Presence) clearWrapUp() {
	p.WrapUpEndsAt = time.Time{}
	p.WrapUpCallID = nil
}

// ExpireWrapUp returns the agent to READY when their wrap-up window has run
// out. It reports whether anything changed, so a timer firing late or after an
// explicit request cannot override the agent's own choice.
func (p *Presence) ExpireWrapUp(at time.Time) bool {
	if !p.IsInWrapUp() {
		return false
	}
	if p.WrapUpEndsAt.IsZero() || at.Before(p.WrapUpEndsAt) {
		return false
	}
	_ = p.Ready(at)
	return true
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
func (p Presence) CallcenterStatus() string {
	switch p.CurrentState() {
	case StateReady:
		return "Available"
	case StateNotReady:
		return "On Break"
	default:
		return "Logged Out"
	}
}
