// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// The ownership rule behind agent playback: the same line /cdrs/mine draws.
func TestAgentWasOnCall(t *testing.T) {
	mine := uuid.New()
	other := uuid.New()

	if agentWasOnCall(store.CDR{PrimaryAgentID: &other}, mine) {
		t.Error("somebody else's call was granted")
	}
	if agentWasOnCall(store.CDR{}, mine) {
		t.Error("a call with no agents was granted")
	}
	if !agentWasOnCall(store.CDR{PrimaryAgentID: &mine}, mine) {
		t.Error("the primary agent was refused their own call")
	}
	// A transferred call: the agent took it, a colleague finished it.
	if !agentWasOnCall(store.CDR{PrimaryAgentID: &other, AgentIDs: []uuid.UUID{other, mine}}, mine) {
		t.Error("a transfer participant was refused the call")
	}
}
