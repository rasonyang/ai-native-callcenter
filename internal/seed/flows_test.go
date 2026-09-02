// SPDX-License-Identifier: Apache-2.0

package seed

import (
	"encoding/json"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// Voice names belong to the engine that answers, and these flows ship to
// deployments that have not chosen one yet. A name here is therefore a name
// that is right for exactly one of them: `longanqian` was Qwen's, and on a
// deployment running the gateway every one of these flows fell silent — the
// synthesiser rejected the voice on its first frame, so the call connected,
// heard the caller, and never said a word.
//
// Empty is what a shipped flow says instead, and it costs nothing on the
// vendor the name came from: the profile's own default is that same voice.
// A deployment with a persona in mind still names one — it just knows which
// engine it runs.
func TestNoShippedFlowNamesAProviderSpecificVoice(t *testing.T) {
	entries, err := flowFiles.ReadDir("flows")
	if err != nil {
		t.Fatalf("read the embedded flows: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no flows are embedded; this test would pass on an empty set")
	}

	for _, entry := range entries {
		data, err := flowFiles.ReadFile("flows/" + entry.Name())
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		var spec flow.Spec
		if err := json.Unmarshal(data, &spec); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if spec.Global.Voice != "" {
			t.Errorf("%s names the voice %q; a shipped flow cannot know which "+
				"engine answers, and a name the deployment's engine does not "+
				"offer is a call that connects and stays silent",
				entry.Name(), spec.Global.Voice)
		}
	}
}
