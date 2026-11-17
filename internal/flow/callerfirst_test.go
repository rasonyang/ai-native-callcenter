// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"strings"
	"testing"
)

const callerFirstFlow = `{
  "id": "caller_first", "specVersion": "v2", "initialNode": "open",
  "global": {"persona": "p"%s},
  "nodes": {
    "open": {"instruction": "Greet.", "announce": "Hello.", "isTerminal": true}
  }
}`

func callerFirstSpec(t *testing.T, global string) *Spec {
	t.Helper()
	spec, err := Load([]byte(strings.Replace(callerFirstFlow, "%s", global, 1)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return spec
}

func TestAFlowCanAskTheBotToListenFirst(t *testing.T) {
	t.Parallel()
	if !callerFirstSpec(t, `, "isCallerFirst": true`).Global.IsCallerFirst {
		t.Error("global.isCallerFirst = false for a flow that declares it")
	}
	if callerFirstSpec(t, "").Global.IsCallerFirst {
		t.Error("global.isCallerFirst = true for a flow that does not declare it")
	}
	if callerFirstSpec(t, `, "isCallerFirst": false`).Global.IsCallerFirst {
		t.Error("global.isCallerFirst = true for a flow that declares false")
	}
}

func TestIsCallerFirstIsKnownToTheUnknownFieldCheck(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(callerFirstFlow, "%s", `, "isCallerFirst": true`, 1)
	if found := unknownFields([]byte(doc)); len(found) != 0 {
		t.Errorf("isCallerFirst reported as unknown: %v", found)
	}
	found := unknownFields([]byte(strings.Replace(doc, "isCallerFirst", "isCallFirst", 1)))
	if len(found) != 1 || found[0].Suggestion != "isCallerFirst" {
		t.Errorf("a near miss = %v, want one suggesting isCallerFirst", found)
	}
}
