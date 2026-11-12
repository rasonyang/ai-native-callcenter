// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

func fieldService(t *testing.T) *flow.Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(seedFlows, "field_service_appointment.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := flow.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// bookingBot looks the caller up, books a slot when nothing is found, and
// remembers what each lookup returned.
func bookingBot(lookups *[]string) *fakeChat {
	return &fakeChat{answer: func(_ int, req ChatRequest) (ChatResponse, error) {
		last := req.Messages[len(req.Messages)-1]
		switch {
		case last.Role == "tool" && strings.Contains(last.Content, `"found"`):
			if strings.Contains(last.Content, `"found":"0"`) {
				*lookups = append(*lookups, "0")
				return tool("appt_book", `{"date_choice":1,"item":"screen","address":"12 Lakeside Road"}`), nil
			}
			*lookups = append(*lookups, "1")
			return text("You already have a booking."), nil
		case last.Role == "tool" && strings.Contains(last.Content, "appt_id"):
			return tool("hangup", `{"reason":"CALLER_GOODBYE"}`), nil
		case last.Role == "tool":
			return text("Bye."), nil
		}
		return tool("appt_lookup", `{}`), nil
	}}
}

// A booking one run makes must not be found by the next: every run starts from
// the same facts, which is the mock backend's contract. Before each run had its
// own mock, run 2 found run 1's booking and went down the manage branch.
func TestEveryModelRunStartsFromFreshBackendState(t *testing.T) {
	sc := scenarioOf(t, `{"name":"books","caller":"13600001111","callerPersona":{"goal":"book a visit"},
		"model":{"visits":["done_book"]}}`)
	var lookups []string
	reports := RunModel(context.Background(), fieldService(t), MockBackends(),
		ModelConfig{Bot: bookingBot(&lookups), Caller: queue(text("hi")), Runs: 3, MaxCallerTurns: 1}, []Scenario{sc})

	if reports[0].Passed != 3 {
		t.Fatalf("passed %d of 3; lookups: %v; first failure: %s", reports[0].Passed, lookups, reports[0].Runs[len(reports[0].Runs)-1].Failure)
	}
	if strings.Join(lookups, "") != "000" {
		t.Errorf("lookups = %v, want every run to find nothing", lookups)
	}
}

// The shared backend is the external-URL case and does leak between runs;
// this pins down why the fresh one matters.
func TestASharedBackendLeaksBetweenRuns(t *testing.T) {
	source, stop := sharedMock()
	defer stop()
	sc := scenarioOf(t, `{"name":"books","caller":"13600001111","callerPersona":{"goal":"book a visit"},
		"model":{"visits":["done_book"]}}`)
	var lookups []string
	reports := RunModel(context.Background(), fieldService(t), source,
		ModelConfig{Bot: bookingBot(&lookups), Caller: queue(text("hi")), Runs: 2, MaxCallerTurns: 1}, []Scenario{sc})
	if strings.Join(lookups, "") != "01" || reports[0].Passed != 1 {
		t.Errorf("lookups = %v, passed = %d; want the second run to find the first run's booking", lookups, reports[0].Passed)
	}
}

func sharedMock() (BackendSource, func()) {
	backend, release := MockBackends()()
	return SharedBackend(backend), release
}

// Tier 1 scenarios are independent too: one that books must not change what
// the next one finds.
func TestEveryScenarioStartsFromFreshBackendState(t *testing.T) {
	file, err := ParseScenarios([]byte(`{"scenarios":[
	 {"name":"books","caller":"13600001111","steps":[
	   {"call":"appt_lookup","expect":{"phase":"book"}},
	   {"call":"appt_book","args":{"date_choice":1,"item":"x","address":"y"},"expect":{"phase":"done_book"}}]},
	 {"name":"finds nothing","caller":"13600001111","steps":[
	   {"call":"appt_lookup","expect":{"phase":"book","result":{"found":"0"}}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Run(context.Background(), fieldService(t), MockBackends(), file.Scenarios) {
		if !r.Passed() {
			t.Errorf("%s: %s", r.Scenario, r.Failure)
		}
	}
}
