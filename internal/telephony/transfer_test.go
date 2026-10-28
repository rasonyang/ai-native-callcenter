// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The agent on the call moves the caller: the caller's leg is the one that is
// transferred, never the agent's.
func TestAnAgentOnTheCallTransfersTheCaller(t *testing.T) {
	t.Parallel()
	c, cmd, callID, agentID := onACallWithAnAgent(t)

	if err := c.Transfer(t.Context(), callID, agentID, "2001"); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if got, want := cmd.last(), "uuid_transfer caller-chan 2001 XML aicc"; got != want {
		t.Errorf("transfer sent %q, want %q", got, want)
	}
}

// The destination becomes the dialplan's destination number, and an agent on
// the call is the one choosing it. Anything but an extension or a number is
// refused before the switch is touched; a space alone would let it name a
// dialplan of its own.
func TestATransferGoesOnlyToAnExtensionOrANumber(t *testing.T) {
	t.Parallel()
	for _, destination := range []string{
		"system:id inline", "7001 XML default", "7001'", "${system(id)}", "7001,7002", "7001\n",
		"7001\n\nbgapi status", "+", "++8613800138000", "1-800-FLOWERS", "７００１", "123456789012345678901",
		"support-en",
	} {
		c, cmd, callID, agentID := onACallWithAnAgent(t)
		err := c.Transfer(t.Context(), callID, agentID, destination)
		if !errors.Is(err, ErrInvalidDestination) {
			t.Errorf("transfer to %q returned %v, want ErrInvalidDestination", destination, err)
		}
		if len(cmd.sent) != 0 {
			t.Errorf("transfer to %q reached the switch: %q", destination, cmd.sent)
		}
	}
	for _, destination := range []string{"7001", "1", "+8613800138000", "12345678901234567890"} {
		c, cmd, callID, agentID := onACallWithAnAgent(t)
		if err := c.Transfer(t.Context(), callID, agentID, destination); err != nil {
			t.Errorf("transfer to %q: %v", destination, err)
		}
		if want := "uuid_transfer caller-chan " + destination + " XML aicc"; cmd.last() != want {
			t.Errorf("transfer sent %q, want %q", cmd.last(), want)
		}
	}
}

// A call id is not an authority. An agent who is not on the call, knowing its
// id, could otherwise move a colleague's customer to another queue and end the
// colleague's call under them (#239). The refusal comes before the switch is
// touched.
func TestAnAgentNotOnTheCallCannotTransferIt(t *testing.T) {
	t.Parallel()
	c, cmd, callID, _ := onACallWithAnAgent(t)

	err := c.Transfer(t.Context(), callID, uuid.New(), "2001")
	if !errors.Is(err, ErrNotCallParty) {
		t.Fatalf("transfer by an agent not on the call returned %v, want ErrNotCallParty", err)
	}
	if len(cmd.sent) != 0 {
		t.Errorf("the switch was sent %q for a refused transfer", cmd.sent)
	}
}

// Having once had a leg is not being on the call: after a hangup the agent has
// nothing left to transfer from.
func TestAnAgentWhoseLegEndedCannotTransferTheCall(t *testing.T) {
	t.Parallel()
	c, cmd, callID, agentID := onACallWithAnAgent(t)
	if err := c.registry.Do(callID, func(call *Call) {
		call.PartyByChannel("agent-chan").State = PartyReleased
	}); err != nil {
		t.Fatal(err)
	}

	err := c.Transfer(t.Context(), callID, agentID, "2001")
	if !errors.Is(err, ErrNotCallParty) {
		t.Fatalf("transfer after the agent's leg ended returned %v, want ErrNotCallParty", err)
	}
	if len(cmd.sent) != 0 {
		t.Errorf("the switch was sent %q for a refused transfer", cmd.sent)
	}
}

// With two agents on the call (a consultation, a supervisor who joined as a
// party) each of them is on it, so each may pass the caller on.
func TestEitherAgentOnATwoAgentCallCanTransferIt(t *testing.T) {
	t.Parallel()
	c, cmd, callID, firstID := onACallWithAnAgent(t)
	secondID := uuid.New()
	if err := c.registry.Do(callID, func(call *Call) {
		second := call.AddParty("second-agent-chan", "1002", call.CreatedAt)
		second.State = PartyTalking
		second.AgentID = &secondID
	}); err != nil {
		t.Fatal(err)
	}

	for name, agentID := range map[string]uuid.UUID{"first": firstID, "second": secondID} {
		cmd.sent = nil
		if err := c.Transfer(t.Context(), callID, agentID, "2001"); err != nil {
			t.Fatalf("transfer by the %s agent: %v", name, err)
		}
		if got, want := cmd.last(), "uuid_transfer caller-chan 2001 XML aicc"; got != want {
			t.Errorf("transfer by the %s agent sent %q, want %q", name, got, want)
		}
	}
}
