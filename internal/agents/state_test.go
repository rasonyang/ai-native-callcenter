// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var now = time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

func TestLoginLandsInNotReady(t *testing.T) {
	var p Presence
	if err := p.Login("1001", now); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if p.State != StateNotReady || p.Reason != ReasonLogin {
		t.Errorf("state = %s(%s), want NOT_READY(LOGIN): nobody is routed a call on sign-in",
			p.State, p.Reason)
	}
	if p.ExtensionNumber != "1001" {
		t.Errorf("ExtensionNumber = %q, want the extension bound at login", p.ExtensionNumber)
	}
	if err := p.Login("1002", now); err != ErrAlreadyLoggedIn {
		t.Errorf("second Login() error = %v, want ErrAlreadyLoggedIn", err)
	}
}

func TestTransitions(t *testing.T) {
	tests := []struct {
		name       string
		start      func(*Presence)
		act        func(*Presence) error
		wantState  State
		wantReason Reason
		wantErr    error
	}{
		{
			name:      "not ready to ready",
			start:     func(p *Presence) { _ = p.Login("1001", now) },
			act:       func(p *Presence) error { return p.Ready(now) },
			wantState: StateReady,
		},
		{
			name: "ready to not ready with a reason",
			start: func(p *Presence) {
				_ = p.Login("1001", now)
				_ = p.Ready(now)
			},
			act:        func(p *Presence) error { return p.NotReady(ReasonLunch, now) },
			wantState:  StateNotReady,
			wantReason: ReasonLunch,
		},
		{
			name: "logout from ready",
			start: func(p *Presence) {
				_ = p.Login("1001", now)
				_ = p.Ready(now)
			},
			act:       func(p *Presence) error { return p.Logout(now) },
			wantState: StateLoggedOut,
		},
		{
			name:      "ready is refused when logged out",
			act:       func(p *Presence) error { return p.Ready(now) },
			wantState: StateLoggedOut,
			wantErr:   ErrNotLoggedIn,
		},
		{
			name:      "not ready is refused when logged out",
			act:       func(p *Presence) error { return p.NotReady(ReasonBreak, now) },
			wantState: StateLoggedOut,
			wantErr:   ErrNotLoggedIn,
		},
		{
			name:      "logout is refused when logged out",
			act:       func(p *Presence) error { return p.Logout(now) },
			wantState: StateLoggedOut,
			wantErr:   ErrNotLoggedIn,
		},
		{
			name:       "unknown reasons are refused",
			start:      func(p *Presence) { _ = p.Login("1001", now) },
			act:        func(p *Presence) error { return p.NotReady(Reason("COFFEE"), now) },
			wantState:  StateNotReady,
			wantReason: ReasonLogin,
			wantErr:    ErrUnknownReason,
		},
		{
			name: "ring no answer takes the agent out of routing",
			start: func(p *Presence) {
				_ = p.Login("1001", now)
				_ = p.Ready(now)
			},
			act:        func(p *Presence) error { return p.RingNoAnswer(now) },
			wantState:  StateNotReady,
			wantReason: ReasonSystem,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p Presence
			if tt.start != nil {
				tt.start(&p)
			}
			err := tt.act(&p)
			if err != tt.wantErr {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if p.CurrentState() != tt.wantState {
				t.Errorf("state = %s, want %s", p.CurrentState(), tt.wantState)
			}
			if tt.wantReason != "" && p.Reason != tt.wantReason {
				t.Errorf("reason = %s, want %s", p.Reason, tt.wantReason)
			}
		})
	}
}

func TestWrapUpHoldsUntilItIsFiled(t *testing.T) {
	var p Presence
	if err := p.Login("1001", now); err != nil {
		t.Fatal(err)
	}
	callID := uuid.New()
	if err := p.StartWrapUp(callID, now); err != nil {
		t.Fatal(err)
	}
	if p.State != StateNotReady || p.Reason != ReasonAfterCallWork {
		t.Fatalf("state = %s(%s), want NOT_READY(AFTER_CALL_WORK)", p.State, p.Reason)
	}
	if !p.IsInWrapUp() {
		t.Error("IsInWrapUp() is false during after-call work")
	}
	// EnteredAt is when the call ended, which is what the screen counts up
	// from; nothing here says when it should stop.
	if !p.EnteredAt.Equal(now) {
		t.Errorf("enteredAt = %v, want the moment the call ended (%v)", p.EnteredAt, now)
	}
	if p.WrapUpCallID == nil || *p.WrapUpCallID != callID {
		t.Errorf("wrapUpCallId = %v, want the call just finished", p.WrapUpCallID)
	}

	if err := p.Ready(now.Add(90 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if p.State != StateReady || p.WrapUpCallID != nil {
		t.Errorf("presence = %+v, want READY with the wrap-up cleared", p)
	}
}

// After-call work is a state the agent can leave for a reason of their own;
// what they had not filed stays unfiled, which is honest.
func TestAnAgentMayChooseSomethingElseDuringWrapUp(t *testing.T) {
	var p Presence
	_ = p.Login("1001", now)
	_ = p.StartWrapUp(uuid.New(), now)

	if err := p.NotReady(ReasonLunch, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if p.State != StateNotReady || p.Reason != ReasonLunch {
		t.Errorf("state = %s(%s), want NOT_READY(LUNCH)", p.State, p.Reason)
	}
	if p.WrapUpCallID != nil {
		t.Error("the wrap-up call survived a state the agent chose instead")
	}
}

func TestAvailabilityPrecedence(t *testing.T) {
	tests := []struct {
		name string
		p    Presence
		want Availability
	}{
		{
			name: "logged out wins over everything",
			p:    Presence{State: StateLoggedOut, IsOnCall: true},
			want: AvailLoggedOut,
		},
		{
			name: "on call wins over presence",
			p:    Presence{State: StateReady, IsOnCall: true, IsRegistered: true, IsDeviceInService: true},
			want: AvailOnCall,
		},
		{
			name: "wrap-up is distinguished from other not-ready reasons",
			p:    Presence{State: StateNotReady, Reason: ReasonAfterCallWork},
			want: AvailWrapUp,
		},
		{
			name: "other not-ready reasons",
			p:    Presence{State: StateNotReady, Reason: ReasonLunch},
			want: AvailNotReady,
		},
		{
			name: "a ready agent whose phone is gone is not available",
			p:    Presence{State: StateReady, IsRegistered: false},
			want: AvailDeviceUnreachable,
		},
		{
			name: "registered but failing its options ping is not available",
			p:    Presence{State: StateReady, IsRegistered: true, IsDeviceInService: false},
			want: AvailDeviceUnreachable,
		},
		{
			name: "ready and reachable",
			p:    Presence{State: StateReady, IsRegistered: true, IsDeviceInService: true},
			want: AvailReady,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Availability(); got != tt.want {
				t.Errorf("Availability() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCallcenterStatusMapping(t *testing.T) {
	tests := []struct {
		state  State
		reason Reason
		want   string
	}{
		{StateReady, "", "Available"},
		{StateNotReady, ReasonLunch, "On Break"},
		{StateNotReady, ReasonAfterCallWork, "On Break"},
		{StateNotReady, ReasonSystem, "On Break"},
		{StateLoggedOut, "", "Logged Out"},
	}
	for _, tt := range tests {
		p := Presence{State: tt.state, Reason: tt.reason}
		if got := p.CallcenterStatus(); got != tt.want {
			t.Errorf("%s(%s) mapped to %q, want %q", tt.state, tt.reason, got, tt.want)
		}
	}
}
