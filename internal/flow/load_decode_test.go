// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"errors"
	"strings"
	"testing"
)

// A document that does not decode is a flow that is not usable, reported the
// way every other problem is, so a caller sees one kind of refusal.
func TestADocumentThatDoesNotDecodeIsAValidationError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, doc, problem, flowID string
	}{
		{"a wrong-typed field", `{"id":"probe","initialNode":5}`, "initialNode", "probe"},
		{"a wrong-typed wording", `{"id":"probe","nodes":{"a":{"instruction":42}}}`, "text must be a string", "probe"},
		{"not JSON", `{"id":"pro`, "invalid JSON", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(tc.doc))
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %T %v, want *ValidationError", err, err)
			}
			if len(invalid.Problems) != 1 || !strings.Contains(invalid.Problems[0], tc.problem) {
				t.Errorf("problems = %q, want one containing %q", invalid.Problems, tc.problem)
			}
			if invalid.FlowID != tc.flowID {
				t.Errorf("FlowID = %q, want %q", invalid.FlowID, tc.flowID)
			}
		})
	}
}
