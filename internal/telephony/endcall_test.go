// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Ending a call is not hanging up a leg with a different argument.
//
// An agent hangs up their leg, named by who they are; somebody with no leg has
// nobody to name, so the leg is found by the call. What follows is the same
// either way — one kill at the extension, the bridge collapses, the switch
// releases the far end.
func TestEndCallHangsUpTheExtensionLeg(t *testing.T) {
	c, cmd, callID, _ := onACallWithAnAgent(t)
	if err := c.registry.Do(callID, func(call *Call) {
		for _, p := range call.Parties {
			if p.ChannelID == "agent-chan" {
				p.ExtensionNumber = "1001"
			}
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := c.EndCall(t.Context(), callID); err != nil {
		t.Fatalf("end call: %v", err)
	}

	var kills []string
	for _, sent := range cmd.sent {
		if strings.HasPrefix(sent, "uuid_kill ") {
			kills = append(kills, sent)
		}
	}
	if !slices.Equal(kills, []string{"uuid_kill agent-chan NORMAL_CLEARING"}) {
		t.Errorf("sent %v, want one kill at the extension leg — the far end is the "+
			"switch's to release, as it is when the person at that phone hangs up", kills)
	}
}

// The leg keeps the cause its own state deserves: a leg that never answered is
// recorded as rejected, not as a conversation that ended normally. The same
// rule Hangup follows, because it is the party that knows.
func TestEndCallGivesTheLegTheCauseItsStateDeserves(t *testing.T) {
	c, cmd, callID, _ := onACallWithAnAgent(t)
	if err := c.registry.Do(callID, func(call *Call) {
		for _, p := range call.Parties {
			if p.ChannelID == "agent-chan" {
				p.ExtensionNumber = "1001"
				p.State = PartyRinging
			}
		}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := c.EndCall(t.Context(), callID); err != nil {
		t.Fatalf("end call: %v", err)
	}
	if !slices.Contains(cmd.sent, "uuid_kill agent-chan CALL_REJECTED") {
		t.Errorf("sent %v, want the unanswered leg ended as rejected — the ledger reads "+
			"this to tell a conversation from a call nobody took", cmd.sent)
	}
}

// A call with no phone of ours on it — a caller still with the bot, say — has
// nothing this operation can hang up, and says so rather than reporting a
// success that killed nothing.
func TestEndCallRefusesACallWithNoExtensionLeg(t *testing.T) {
	c, cmd, callID, _ := onACallWithAnAgent(t)
	before := len(cmd.sent)

	if err := c.EndCall(t.Context(), callID); !errors.Is(err, ErrNoExtensionLeg) {
		t.Fatalf("err = %v, want ErrNoExtensionLeg", err)
	}
	for _, sent := range cmd.sent[before:] {
		if strings.HasPrefix(sent, "uuid_kill") {
			t.Errorf("something was killed anyway: %q", sent)
		}
	}
}

// A call the registry has never heard of is refused: a client that named the
// wrong id must be told, not reassured.
func TestEndCallOnAnUnknownCallIsRefused(t *testing.T) {
	c, _, _, _ := onACallWithAnAgent(t)
	if err := c.EndCall(t.Context(), uuid.New()); err == nil {
		t.Error("ending a call that does not exist reported success")
	}
}
