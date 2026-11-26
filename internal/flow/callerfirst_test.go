// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"strconv"
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

// global.callerFirstWaitMs bounds the silent-callee wait (#603): absent is the
// 2000 ms default, 500 to 4000 load, anything outside is rejected with a
// message that names the range, and the field without isCallerFirst is
// rejected. A valid flow bound to an inbound DID still loads — the field only
// moves an outbound call's wait, and load cannot tell which leg is placed.
func TestACallerFirstWaitIsBoundedAndNeedsIsCallerFirst(t *testing.T) {
	t.Parallel()
	load := func(global string) error {
		_, err := Load([]byte(strings.Replace(callerFirstFlow, "%s", global, 1)))
		return err
	}
	first := `, "isCallerFirst": true`

	if spec := callerFirstSpec(t, first); spec.Global.CallerFirstWaitMs != nil {
		t.Errorf("absent callerFirstWaitMs = %v, want nil (2000 ms)", *spec.Global.CallerFirstWaitMs)
	}
	for _, ms := range []int{MinCallerFirstWaitMs, 1200, MaxCallerFirstWaitMs} {
		spec, err := Load([]byte(strings.Replace(callerFirstFlow, "%s",
			first+`, "callerFirstWaitMs": `+strconv.Itoa(ms), 1)))
		if err != nil {
			t.Fatalf("callerFirstWaitMs %d: %v", ms, err)
		}
		if spec.Global.CallerFirstWaitMs == nil || *spec.Global.CallerFirstWaitMs != ms {
			t.Errorf("callerFirstWaitMs %d read as %v", ms, spec.Global.CallerFirstWaitMs)
		}
	}
	for _, ms := range []int{499, 4001, 0, -1} {
		err := load(first + `, "callerFirstWaitMs": ` + strconv.Itoa(ms))
		if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "4000") {
			t.Errorf("callerFirstWaitMs %d: err %v, want a refusal that names the range", ms, err)
		}
	}
	if err := load(`, "callerFirstWaitMs": 1000`); err == nil {
		t.Error("callerFirstWaitMs without isCallerFirst loaded")
	}
	if err := load(`, "isCallerFirst": false, "callerFirstWaitMs": 1000`); err == nil {
		t.Error("callerFirstWaitMs with isCallerFirst false loaded")
	}
}
