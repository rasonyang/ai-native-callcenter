// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The rule that sends a slot tool's result without a turn (a phase that says
// the caller speaks next) is off on doubao until it has been measured there:
// the engine waits for every call's answer, and what it does with a result
// that asks for nothing is not known. The call actor reads this by type
// assertion, so the session must not satisfy the interface.
func TestDoubaoDoesNotAnswerToolCallsQuietly(t *testing.T) {
	var session any = (*Session)(nil)
	if _, ok := session.(provider.QuietToolAnswerer); ok {
		t.Fatal("doubao implements provider.QuietToolAnswerer; the rule is meant to be off until measured")
	}
}
