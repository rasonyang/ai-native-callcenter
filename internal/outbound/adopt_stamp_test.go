// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"strings"
	"testing"
)

// What recovery reads has to be on the leg when it is originated.
func TestTheOriginatedLegsCarryWhatRecoveryNeeds(t *testing.T) {
	rig := newRecoveryRig(t)
	if _, err := rig.svc.DialAI(context.Background(), AIDialRequest{To: "18688886666", DIDNumber: "95099"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.svc.Dial(context.Background(), AgentDialRequest{AgentExtension: "1007", To: "18688886667"}); err != nil {
		t.Fatal(err)
	}
	rig.sw.mu.Lock()
	defer rig.sw.mu.Unlock()
	if len(rig.sw.commands) != 2 {
		t.Fatalf("commands = %v", rig.sw.commands)
	}
	for _, want := range []string{
		"aicc_placed_by=DIAL_AI", "aicc_placed_to=18688886666", "aicc_did=95099", "aicc_language=en",
	} {
		if !strings.Contains(rig.sw.commands[0], want) {
			t.Errorf("AI outbound originate lacks %s: %s", want, rig.sw.commands[0])
		}
	}
	for _, want := range []string{"aicc_placed_by=DIAL", "aicc_destination=18688886667"} {
		if !strings.Contains(rig.sw.commands[1], want) {
			t.Errorf("click-to-dial originate lacks %s: %s", want, rig.sw.commands[1])
		}
	}
}
