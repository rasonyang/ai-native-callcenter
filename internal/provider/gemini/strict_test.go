// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"os"
	"testing"
)

// requireStrict skips a test that drives a session against the fake Live
// endpoint unless AICC_GEMINI_STRICT_TESTS=1.
//
// Owner directive, 2026-09-28: Gemini Live support is immature, and these tests
// depend on wall-clock waits and goroutine scheduling (the pacer, the speech
// detector, the watchdog), which shared CI runners do not keep steady. The
// default `go test -race ./...` runs only this package's deterministic unit
// tests. Set AICC_GEMINI_STRICT_TESTS=1 to run all of them.
func requireStrict(t *testing.T) {
	t.Helper()
	if os.Getenv("AICC_GEMINI_STRICT_TESTS") != "1" {
		t.Skip("gemini live timing test; set AICC_GEMINI_STRICT_TESTS=1 to run")
	}
}
