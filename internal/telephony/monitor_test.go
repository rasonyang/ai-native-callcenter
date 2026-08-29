// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A supervisor listening in raises their own phone against the agent's leg —
// the agent's, because the whisper flags are named from the eavesdropped
// leg's point of view — and the command carries neither the call id nor
// anything else that would make the listener a party.
func TestMonitorRaisesTheSupervisorAgainstTheAgentLeg(t *testing.T) {
	c, cmd, callID, agentID := onACallWithAnAgent(t)

	if err := c.Monitor(t.Context(), callID, agentID, "1099", "WHISPER"); err != nil {
		t.Fatalf("monitor: %v", err)
	}
	sent := cmd.sent[len(cmd.sent)-1]
	for _, want := range []string{
		"originate {", "user/1099@aicc.test", "&eavesdrop(agent-chan)",
		"eavesdrop_whisper_bleg=true", "aicc_observer=WHISPER",
		"sip_h_X-AICC-Call-Id=" + callID.String(),
	} {
		if !strings.Contains(sent, want) {
			t.Errorf("sent %q, want it to contain %q", sent, want)
		}
	}
	if strings.Contains(sent, "aicc_call_id") {
		t.Errorf("sent %q: the listener's leg must not carry the call id, or it is adopted as a party", sent)
	}
}

// A supervisor has one phone, so changing mode ends the leg already at it
// before raising the next: two eavesdrops at one handset would mean the second
// never answers, or answers over the first.
func TestChangingModeEndsThePreviousMonitoringLeg(t *testing.T) {
	c, cmd, callID, agentID := onACallWithAnAgent(t)

	if err := c.Monitor(t.Context(), callID, agentID, "1099", "LISTEN"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	first := observerIDOf(t, cmd.sent[len(cmd.sent)-1])
	before := len(cmd.sent)

	if err := c.Monitor(t.Context(), callID, agentID, "1099", "WHISPER"); err != nil {
		t.Fatalf("whisper: %v", err)
	}
	kill := "uuid_kill " + first + " NORMAL_CLEARING"
	if !slices.Contains(cmd.sent[before:], kill) {
		t.Errorf("sent %v, want %q — the listening leg is ended before the whisper is raised", cmd.sent[before:], kill)
	}
	if slices.Index(cmd.sent[before:], kill) > slices.IndexFunc(cmd.sent[before:], func(s string) bool {
		return strings.Contains(s, "&eavesdrop(")
	}) {
		t.Error("the new leg was raised before the old one was ended")
	}

	// A supervisor who hung up their own phone leaves nothing to end: the leg
	// is forgotten when the switch says it is gone, and the next mode is
	// raised without a kill nobody needs.
	second := observerIDOf(t, cmd.sent[len(cmd.sent)-1])
	c.Handle(t.Context(), raw("CHANNEL_HANGUP_COMPLETE", second, "outbound",
		map[string]string{"variable_aicc_observer": "WHISPER"}))
	before = len(cmd.sent)
	if err := c.Monitor(t.Context(), callID, agentID, "1099", "BARGE"); err != nil {
		t.Fatalf("barge: %v", err)
	}
	for _, sent := range cmd.sent[before:] {
		if strings.HasPrefix(sent, "uuid_kill ") {
			t.Errorf("sent %q for a leg the switch had already ended", sent)
		}
	}
}

// observerIDOf reads the leg id out of an originate command, which is the only
// place a supervisor's leg is named: it is on no call and in no registry.
func observerIDOf(t *testing.T, command string) string {
	t.Helper()
	_, rest, ok := strings.Cut(command, "origination_uuid=")
	if !ok {
		t.Fatalf("no origination_uuid in %q", command)
	}
	id, _, _ := strings.Cut(rest, ",")
	return id
}

// Somebody who is not on the call cannot be listened to on it: the supervisor
// gets a refusal, not an eavesdrop on whichever leg came first.
func TestMonitorRefusesAnAgentWhoIsNotOnTheCall(t *testing.T) {
	c, cmd, callID, _ := onACallWithAnAgent(t)
	before := len(cmd.sent)

	err := c.Monitor(t.Context(), callID, uuid.New(), "1099", "LISTEN")
	if !errors.Is(err, ErrNoAgentLeg) {
		t.Fatalf("err = %v, want ErrNoAgentLeg", err)
	}
	if len(cmd.sent) != before {
		t.Errorf("the switch was sent %v for a refused request", cmd.sent[before:])
	}
	if err := c.Monitor(t.Context(), uuid.New(), uuid.New(), "1099", "LISTEN"); !errors.Is(err, ErrCallNotFound) {
		t.Errorf("unknown call: err = %v, want ErrCallNotFound", err)
	}
}

// The supervisor's leg is scaffolding, not a party: its events create no call
// and join none, so it reaches neither the stream nor the ledger.
func TestAnObserverLegIsNeverAdopted(t *testing.T) {
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, nil, noAgents{}, nullPublisher{})

	c.Handle(t.Context(), raw("CHANNEL_CREATE", "chan-sup", "outbound",
		map[string]string{"variable_aicc_observer": "LISTEN"}))
	if registry.Count() != 0 {
		t.Fatalf("a supervisor's listening leg became %d call(s)", registry.Count())
	}
	if registry.Owns("chan-sup") {
		t.Error("the observer channel was bound to a call")
	}
}
