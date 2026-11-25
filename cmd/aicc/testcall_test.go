// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// The built-in flow the command seeds must load under the same rules
// flowadd's Create applies — a flow the command could not publish would fail
// the first test call before it starts (#538).
func TestTheBuiltInTestCallFlowLoads(t *testing.T) {
	t.Parallel()
	spec, err := flow.Load(testCallFlow)
	if err != nil {
		t.Fatalf("the built-in flow does not load: %v", err)
	}
	if spec.ID != testCallFlowSlug {
		t.Errorf("flow id = %q, want %q", spec.ID, testCallFlowSlug)
	}
}

// The summary never prints a number somebody could dial back: it keeps the
// head and tail and replaces the middle (#538).
func TestMaskCalleeKeepsOnlyEdges(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"+8613800138000", "+86********000"},
		{"13800138000", "138*****000"},
		{"9901", "9…"},
	}
	for _, tc := range cases {
		if got := maskCallee(tc.in); got != tc.want {
			t.Errorf("maskCallee(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := maskCallee("+8613800138000"); got == "+8613800138000" {
		t.Error("the callee came back in full")
	}
}
