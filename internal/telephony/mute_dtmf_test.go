// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// A signed-in agent bridged to a caller — the state every operation below
// needs. Returns the coordinator, the fake switch, and the call and agent ids.
func onACallWithAnAgent(t *testing.T) (*Coordinator, *fakeCommander, uuid.UUID, uuid.UUID) {
	t.Helper()

	cmd := &fakeCommander{up: true}
	registry := NewRegistry(nullPublisher{})
	c := NewCoordinator(registry, NewAdapter(cmd, "aicc.test"), noAgents{}, nullPublisher{})

	ctx := t.Context()
	callID := uuid.New()
	agentID := uuid.New()
	if _, err := registry.CreateCallMinted(ctx, callID, "INBOUND", "en", true); err != nil {
		t.Fatalf("create call: %v", err)
	}
	for _, ch := range []string{"caller-chan", "agent-chan"} {
		if err := registry.BindChannel(ch, callID); err != nil {
			t.Fatalf("bind %s: %v", ch, err)
		}
	}
	if err := registry.Do(callID, func(call *Call) {
		caller := call.AddParty("caller-chan", "13800138000", call.CreatedAt)
		caller.State = PartyTalking
		agent := call.AddParty("agent-chan", "1001", call.CreatedAt)
		agent.State = PartyTalking
		agent.AgentID = &agentID
	}); err != nil {
		t.Fatalf("seed parties: %v", err)
	}
	return c, cmd, callID, agentID
}

func muteFlag(t *testing.T, c *Coordinator, callID uuid.UUID) bool {
	t.Helper()
	snap, err := c.registry.Snapshot(callID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, p := range snap.Parties {
		if p.AgentID != nil {
			return p.IsMuted
		}
	}
	t.Fatal("no agent leg in the snapshot")
	return false
}

// Mute acts on the agent's own leg and on the read direction: muting the write
// direction would deafen the agent instead of silencing them.
func TestMuteSilencesTheAgentsOwnLeg(t *testing.T) {
	c, cmd, callID, agentID := onACallWithAnAgent(t)

	if err := c.Mute(t.Context(), callID, agentID); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if got, want := cmd.last(), "uuid_audio agent-chan start read mute"; got != want {
		t.Errorf("mute sent %q, want %q", got, want)
	}
	if !muteFlag(t, c, callID) {
		t.Error("the agent's leg is not marked muted")
	}

	if err := c.Unmute(t.Context(), callID, agentID); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	if got, want := cmd.last(), "uuid_audio agent-chan stop"; got != want {
		t.Errorf("unmute sent %q, want %q", got, want)
	}
	if muteFlag(t, c, callID) {
		t.Error("the agent's leg is still marked muted")
	}
}

// A mute the switch refused must not leave the cockpit claiming the microphone
// is off: an agent who believes they are muted and is not will say something
// the caller hears.
func TestARefusedMuteLeavesTheFlagAlone(t *testing.T) {
	c, cmd, callID, agentID := onACallWithAnAgent(t)
	cmd.err = errors.New("switch refused")

	if err := c.Mute(t.Context(), callID, agentID); err == nil {
		t.Fatal("mute reported success though the switch refused")
	}
	if muteFlag(t, c, callID) {
		t.Error("the leg is marked muted after a refused command")
	}
}

// The tones must reach whatever the caller is connected to, so they go out on
// the far end's channel. Sent at the agent's own leg they would only beep in
// the agent's ear.
func TestDTMFGoesToTheFarEnd(t *testing.T) {
	c, cmd, callID, agentID := onACallWithAnAgent(t)

	if err := c.SendDTMF(t.Context(), callID, agentID, "12*#"); err != nil {
		t.Fatalf("send dtmf: %v", err)
	}
	if got, want := cmd.last(), "uuid_send_dtmf caller-chan 12*#"; got != want {
		t.Errorf("dtmf sent %q, want %q", got, want)
	}
}

func TestDTMFRejectsAnythingThatIsNotATone(t *testing.T) {
	c, cmd, callID, agentID := onACallWithAnAgent(t)

	for _, digits := range []string{"", "12 34", "hello", "1;drop", "+8613800138000"} {
		before := len(cmd.sent)
		err := c.SendDTMF(t.Context(), callID, agentID, digits)
		if !errors.Is(err, ErrInvalidDTMF) {
			t.Errorf("SendDTMF(%q) returned %v, want ErrInvalidDTMF", digits, err)
		}
		if len(cmd.sent) != before {
			t.Errorf("SendDTMF(%q) reached the switch anyway: %q", digits, cmd.last())
		}
	}
}

// Being on the call is the permission: without this check any agent could
// push tones into any conversation, or mute somebody else's microphone.
func TestMuteAndDTMFRefuseAnAgentWhoIsNotOnTheCall(t *testing.T) {
	c, cmd, callID, _ := onACallWithAnAgent(t)
	stranger := uuid.New()

	for name, act := range map[string]func() error{
		"mute":   func() error { return c.Mute(t.Context(), callID, stranger) },
		"unmute": func() error { return c.Unmute(t.Context(), callID, stranger) },
		"dtmf":   func() error { return c.SendDTMF(t.Context(), callID, stranger, "1") },
	} {
		before := len(cmd.sent)
		if err := act(); !errors.Is(err, ErrNoAgentLeg) {
			t.Errorf("%s by a stranger returned %v, want ErrNoAgentLeg", name, err)
		}
		if len(cmd.sent) != before {
			t.Errorf("%s by a stranger reached the switch: %q", name, cmd.last())
		}
	}
}
