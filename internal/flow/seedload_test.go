// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"os"
	"path/filepath"
	"testing"
)

// The reference flows are part of the product, and every flow the repository
// ships or tests with is written in the spec's own vocabulary: every one must
// load, unknown fields included.
func TestEveryRepositoryFlowLoads(t *testing.T) {
	t.Parallel()
	var files []string
	for _, pattern := range []string{
		"../seed/flows/*.json",
		"../../docs/examples/flows/*.json",
		"../flowsim/testdata/flows/*.json",
		"../provider/testdata/*flow*.json",
		"../../deploy/e2e/flow.json",
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			t.Fatalf("no flows match %s: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Load(data); err != nil {
			t.Errorf("%s: %v", file, err)
		}
	}
}
