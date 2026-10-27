// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// The flows under testdata exercise what no seed flow uses, with scenarios
// that reach every phase, as a seed flow's must.
const (
	testdataFlows     = "testdata/flows"
	testdataScenarios = "testdata/scenarios"
)

// A phase that declares a lead (#352) records it on entry on a campaign call,
// whatever moved the call there, and nowhere else; the last write wins
// between it and the model's record_lead.
func TestTestdataFlowsPassTheirScenarios(t *testing.T) {
	passTheirScenarios(t, testdataFlows, testdataScenarios)
}

// Notes rendered past the cap are cut, by characters, not refused: a HIGH lead
// is not lost to its notes. No slot holds more than flow.MaxSlotValueLen, so
// it takes several to get there.
func TestDeclaredNotesPastTheCapAreCut(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testdataFlows, "declared_lead.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	nodeOf(doc, "close_high")["lead"] = map[string]any{
		"intent": "HIGH",
		"notes":  "{slots.agree_words}{slots.userData.a}{slots.userData.b}{slots.userData.c}{slots.userData.d}",
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := flow.Load(encoded)
	if err != nil {
		t.Fatal(err)
	}

	part := strings.Repeat("长", flow.MaxSlotValueLen)
	words := `他说 "好" {ok}`
	want := []rune(words + strings.Repeat(part, 4))[:flow.MaxLeadNotesRunes]
	file, err := ParseScenarios([]byte(`{"scenarios": [{
		"name": "long notes", "lang": "zh", "isCampaign": true,
		"userData": {"a": "` + part + `", "b": "` + part + `", "c": "` + part + `", "d": "` + part + `"},
		"steps": [
			{"call": "agree_words", "args": {"value": ` + quote(t, words) + `}},
			{"call": "classify", "args": {"value": "interested"}, "expect": {"phase": "close_high"}}
		],
		"expect": {"lead": {"intent": "HIGH", "notes": ` + quote(t, string(want)) + `}}
	}]}`))
	if err != nil {
		t.Fatal(err)
	}
	report := Run(context.Background(), spec, MockBackends(), file.Scenarios)[0]
	if !report.Passed() {
		t.Fatal(report.Failure)
	}
}

func quote(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
