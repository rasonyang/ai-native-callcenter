// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// callerNextFlow asks a question in offer and moves to wait, a phase where the
// caller speaks next.
const callerNextFlow = `{
  "id": "caller_next", "specVersion": "v2", "initialNode": "offer",
  "global": {"persona": "p", "maxTurnsWithoutTool": 2, "closingTarget": "close"},
  "tools": {
    "ask": {"kind": "slot", "slot": "question", "description": "Record the question."}
  },
  "nodes": {
    "offer": {"instruction": "Offer.", "tools": ["ask"],
      "transitions": [{"on": "TOOL_RESULT", "tool": "ask", "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "wait"}]},
    "wait": {"instruction": "Wait.", "isCallerNext": %s, "tools": ["ask"]},
    "close": {"instruction": "Bye.", "announce": "Goodbye.", "isTerminal": true}
  }
}`

func callerNextSpec(t *testing.T, flag string) *Spec {
	t.Helper()
	spec, err := Load([]byte(strings.Replace(callerNextFlow, "%s", flag, 1)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return spec
}

func TestAPhaseCanDeclareThatTheCallerSpeaksNext(t *testing.T) {
	spec := callerNextSpec(t, "true")
	if !spec.HasCallerNextPhase() {
		t.Error("HasCallerNextPhase = false for a flow that declares one")
	}
	if node, _ := spec.Node("wait"); !node.IsCallerNext {
		t.Error("the wait phase lost isCallerNext")
	}
	if callerNextSpec(t, "false").HasCallerNextPhase() {
		t.Error("HasCallerNextPhase = true for a flow that declares none")
	}
}

func TestTheFieldIsKnownToTheUnknownFieldCheck(t *testing.T) {
	data := []byte(strings.Replace(callerNextFlow, "%s", "true", 1))
	if found := unknownFields(data); len(found) != 0 {
		t.Errorf("isCallerNext reported as unknown: %v", found)
	}
	misspelt := []byte(strings.Replace(string(data), "isCallerNext", "isCallerNxt", 1))
	found := unknownFields(misspelt)
	if len(found) != 1 || found[0].Suggestion != "isCallerNext" {
		t.Errorf("a misspelt field = %v, want one suggesting isCallerNext", found)
	}
}

func TestIsCallerNextIsRejectedWhereTheCallerIsNotNext(t *testing.T) {
	for name, test := range map[string]struct {
		node string
		want string
	}{
		"terminal": {
			`"close": {"instruction": "Bye.", "isCallerNext": true, "isTerminal": true}`,
			`phase "close" is terminal but declares isCallerNext`,
		},
		"announce": {
			`"close": {"instruction": "Bye.", "announce": "Goodbye.", "isCallerNext": true}`,
			`phase "close" declares isCallerNext but has an announce`,
		},
		"announce per language": {
			`"close": {"instruction": "Bye.", "announce": {"en": "Goodbye."}, "isCallerNext": true}`,
			`phase "close" declares isCallerNext but has an announce`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(callerNextFlow, "%s", "true", 1)
			doc = strings.Replace(doc,
				`"close": {"instruction": "Bye.", "announce": "Goodbye.", "isTerminal": true}`, test.node, 1)
			_, err := Load([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Load = %v, want an error containing %q", err, test.want)
			}
		})
	}
}

func TestTheEngineKnowsWhenTheCallerSpeaksNext(t *testing.T) {
	engine := NewEngine(callerNextSpec(t, "true"), "en", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if engine.IsCallerNext() {
		t.Error("the opening phase is not caller-next")
	}
	engine.OnToolResult("ask", map[string]any{"value": "q"}, map[string]any{"ok": "1"})
	if engine.NodeID() != "wait" || !engine.IsCallerNext() {
		t.Errorf("node = %s, IsCallerNext = %v, want wait and true", engine.NodeID(), engine.IsCallerNext())
	}
}

// A turn that made a tool call makes the next turn "the answer to the tool's
// result", which the count skips. When the result was sent without a turn, the
// next turn is the caller's reply and counts.
func TestARepliesAfterAQuietResultCountTowardsTheWall(t *testing.T) {
	for name, isQuiet := range map[string]bool{"ordinary": false, "quiet": true} {
		t.Run(name, func(t *testing.T) {
			engine := NewEngine(callerNextSpec(t, "true"), "en", nil,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine.OnBotTurnDone(true, false)
			if isQuiet {
				engine.OnToolReplyNotComing()
			}
			engine.OnCallerSpoke("and what else?")
			engine.OnBotTurnDone(false, false)

			want := 0
			if isQuiet {
				want = 1
			}
			if got := engine.Slots()["turnsWithoutTool"]; got != want {
				t.Errorf("turnsWithoutTool = %v, want %d", got, want)
			}
		})
	}
}

func TestIsSlotToolAndIsToolOutputOK(t *testing.T) {
	engine := NewEngine(callerNextSpec(t, "true"), "en", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runtime := NewRuntime(engine, &fakeActions{}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !runtime.IsSlotTool("ask") {
		t.Error("ask is a slot tool the phase allows")
	}
	if runtime.IsSlotTool("take_message") || runtime.IsSlotTool("nothing") {
		t.Error("a built-in or an unknown tool is not a slot tool")
	}
	output, _ := runtime.Dispatch(context.Background(), "ask", `{"value":"how much"}`)
	if !IsToolOutputOK(output) {
		t.Errorf("IsToolOutputOK(%s) = false", output)
	}
	refused, _ := runtime.Dispatch(context.Background(), "ask", `{"value":""}`)
	if IsToolOutputOK(refused) {
		t.Errorf("IsToolOutputOK(%s) = true for a refusal", refused)
	}
	for _, output := range []string{"", "not json", `{"ok":"0"}`, `[]`} {
		if IsToolOutputOK(output) {
			t.Errorf("IsToolOutputOK(%q) = true", output)
		}
	}
}
