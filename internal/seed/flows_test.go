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

// A flow in the directory that no entry of demoFlows names ships in the binary
// and never answers: it is embedded, it is not published, and no number points
// at it. The reverse — a table entry naming a file that is not there — is a
// seed that fails at boot. Both are invisible to a reading of either side, so
// the directory and the table are compared here.
func TestEveryShippedFlowIsSeeded(t *testing.T) {
	entries, err := flowFiles.ReadDir("flows")
	if err != nil {
		t.Fatalf("read the embedded flows: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no flows are embedded; this test would pass on an empty set")
	}

	seeded := map[string]bool{}
	for _, f := range demoFlows {
		seeded[f.file] = true
	}
	for _, entry := range entries {
		file := "flows/" + entry.Name()
		if !seeded[file] {
			t.Errorf("%s is embedded but no demoFlows entry publishes it, so it "+
				"ships in the binary and never answers a call", file)
		}
	}

	// The other direction, and the two identities a demo flow must not share
	// with another: its numbers and its slug.
	numbers := map[string]string{}
	slugs := map[string]string{}
	for _, f := range demoFlows {
		spec, err := flowFiles.ReadFile(f.file)
		if err != nil {
			t.Errorf("demoFlows names %s, which is not embedded: %v", f.file, err)
			continue
		}
		slug := specID(spec)
		if other, dup := slugs[slug]; dup {
			t.Errorf("%s and %s both carry the slug %q; the second would find the "+
				"first's flow and publish nothing", other, f.file, slug)
		}
		slugs[slug] = f.file
		for _, n := range f.numbers() {
			if other, dup := numbers[n.number]; dup {
				t.Errorf("%s and %s both claim %s; only the first to be seeded "+
					"gets it (ON CONFLICT DO NOTHING)", other, f.file, n.number)
			}
			numbers[n.number] = f.file
		}
	}
	if len(numbers) != 2*len(demoFlows) {
		t.Errorf("%d distinct numbers for %d flows, want %d",
			len(numbers), len(demoFlows), 2*len(demoFlows))
	}
}
