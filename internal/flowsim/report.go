// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/mockbackend"
)

// BackendSource hands each scenario (tier 1) or run (tier 2) its backend, and
// the function that releases it. The mock backend keeps state (a booking made
// in one run is found by the next), and the mock's contract is that every run
// starts from the same facts, so each run gets a fresh one.
type BackendSource func() (*flow.Backend, func())

// MockBackends serves a new in-process mock backend for every call.
func MockBackends() BackendSource {
	return func() (*flow.Backend, func()) {
		server := httptest.NewServer(mockbackend.New(nil))
		// Pinned: a tool with a baseUrl of its own still calls the mock, so a
		// scenario never reaches a real service, and with a placeholder for
		// every secret, so a real credential is never read and an unset one
		// does not fail the scenario.
		return flow.NewBackend(server.URL, flow.WithPinnedBase(),
			flow.WithSecrets(PlaceholderSecrets)), server.Close
	}
}

// PlaceholderSecret is what every {secrets.NAME} resolves to against the mock.
const PlaceholderSecret = "flowsim-placeholder-secret"

// PlaceholderSecrets resolves every secret to PlaceholderSecret.
func PlaceholderSecrets(string) (string, bool) { return PlaceholderSecret, true }

// SharedBackend hands every run the same backend, for one the simulator cannot
// reset (an external -backend URL). State it keeps carries from run to run.
func SharedBackend(b *flow.Backend) BackendSource {
	return func() (*flow.Backend, func()) { return b, func() {} }
}

// Write prints reports and coverage for one flow. verbose adds each step.
// It returns the number of failed scenarios.
func Write(w io.Writer, flowID string, reports []Report, coverage Coverage, verbose bool) int {
	failed := 0
	fmt.Fprintf(w, "flow %s\n", flowID)
	for _, r := range reports {
		status := "PASS"
		if !r.Passed() {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "  %s  %s\n", status, r.Scenario)
		fmt.Fprintf(w, "        path:   %s\n", strings.Join(r.Path, " > "))
		if r.Ending == flow.ToolHangup && r.HangupReason != "" {
			fmt.Fprintf(w, "        ending: %s (%s)\n", r.Ending, r.HangupReason)
		} else {
			fmt.Fprintf(w, "        ending: %s\n", r.Ending)
		}
		if verbose {
			for i, line := range r.Log {
				fmt.Fprintf(w, "        %2d. %s\n", i+1, line)
			}
		}
		if !r.Passed() {
			fmt.Fprintf(w, "        failure: %s\n", r.Failure)
		}
	}
	fmt.Fprintf(w, "  coverage: %d/%d phases reached, %d/%d phase transitions walked\n",
		coverage.Phases-len(coverage.UnreachedPhases), coverage.Phases,
		coverage.Transitions-len(coverage.UnwalkedTransitions), coverage.Transitions)
	if len(coverage.UnreachedPhases) > 0 {
		fmt.Fprintf(w, "  unreached phases: %s\n", strings.Join(coverage.UnreachedPhases, ", "))
	}
	if len(coverage.UnwalkedTransitions) > 0 {
		fmt.Fprintf(w, "  unwalked transitions: %s\n", strings.Join(coverage.UnwalkedTransitions, "; "))
	}
	return failed
}
