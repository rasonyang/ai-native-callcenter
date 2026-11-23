// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// Issue #568: a terminal phase may split its closing line into the announce —
// the instructions the call ends on — and a short farewell the caller is
// owed. The line spoken on entering the phase is the two as one, the farewell
// last; the line the caller must have heard is the farewell alone.

// farewellFlow ends in a terminal phase whose announce carries an instruction
// and whose farewell carries the goodbye, both rendering a slot.
const farewellSplitFlow = `{
  "id": "farewell_split", "specVersion": "v2", "initialNode": "offer",
  "global": {"persona": "p", "alwaysAllowedTools": ["hangup"],
    "transitions": [{"on": "TOOL_RESULT", "tool": "hangup", "target": "close"}]},
  "tools": {
    "note_name": {"kind": "slot", "slot": "name", "description": "Record the name."}
  },
  "nodes": {
    "offer": {"instruction": "Offer.", "tools": ["note_name"]},
    "close": {"instruction": "Bye.", "isTerminal": true,
      "announce": {"en": "Your repair is booked for Thursday.", "zh": "您的维修已安排在周四。"},
      "farewell": {"en": "Goodbye and take care, {slots.name}.", "zh": "再见，{slots.name}。"}}
  }
}`

func farewellEngine(t *testing.T) *Engine {
	t.Helper()
	spec, err := Load([]byte(farewellSplitFlow))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	engine := NewEngine(spec, "en", map[string]any{"name": "Ann"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.OnToolResult("hangup", map[string]any{"reason": "CALLER_GOODBYE"}, map[string]any{"ok": "1"})
	return engine
}

// The line the phase speaks is announce then farewell as one; the line the
// caller is owed is the farewell alone, rendered like the announce.
func TestATerminalPhaseSpeaksItsAnnounceThenItsFarewell(t *testing.T) {
	engine := farewellEngine(t)
	if engine.NodeID() != "close" {
		t.Fatalf("node = %s, want close", engine.NodeID())
	}
	if got, want := engine.Farewell(), "Goodbye and take care, Ann."; got != want {
		t.Errorf("Farewell = %q, want %q", got, want)
	}
	want := "Your repair is booked for Thursday.\nGoodbye and take care, Ann."
	if got := engine.TerminalLine(); got != want {
		t.Errorf("TerminalLine = %q, want %q", got, want)
	}
	if got := engine.OwedLine(); got != engine.Farewell() {
		t.Errorf("OwedLine = %q, want the farewell %q", got, engine.Farewell())
	}
}

// Without a farewell the terminal line is the announce, and the announce is
// what the caller is owed — unchanged.
func TestATerminalPhaseWithoutAFarewellOwesItsAnnounce(t *testing.T) {
	doc := strings.Replace(farewellSplitFlow,
		`,
      "farewell": {"en": "Goodbye and take care, {slots.name}.", "zh": "再见，{slots.name}。"}`, "", 1)
	spec, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	engine := NewEngine(spec, "en", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.OnToolResult("hangup", nil, map[string]any{"ok": "1"})
	if got, want := engine.TerminalLine(), "Your repair is booked for Thursday."; got != want {
		t.Errorf("TerminalLine = %q, want %q", got, want)
	}
	if got := engine.OwedLine(); got != engine.Announce() {
		t.Errorf("OwedLine = %q, want the announce %q", got, engine.Announce())
	}
}

// A farewell belongs on a terminal phase with an announce, nowhere else.
func TestAFarewellIsRejectedOutsideATerminalAnnounce(t *testing.T) {
	for name, test := range map[string]struct {
		node string
		want string
	}{
		"not terminal": {
			`"close": {"instruction": "Bye.", "announce": "Goodbye.", "farewell": "Bye!"}`,
			`phase "close" declares a farewell but is not terminal`,
		},
		"no announce": {
			`"close": {"instruction": "Bye.", "isTerminal": true, "farewell": {"en": "Bye!"}}`,
			`phase "close" declares a farewell but no announce`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(farewellSplitFlow,
				`"close": {"instruction": "Bye.", "isTerminal": true,
      "announce": {"en": "Your repair is booked for Thursday.", "zh": "您的维修已安排在周四。"},
      "farewell": {"en": "Goodbye and take care, {slots.name}.", "zh": "再见，{slots.name}。"}}`, test.node, 1)
			_, err := Load([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Load = %v, want an error containing %q", err, test.want)
			}
		})
	}
}

// A slot the farewell quotes must be one the flow writes, as for an announce.
func TestAFarewellsSlotsAreCheckedLikeAnAnnounces(t *testing.T) {
	doc := strings.Replace(farewellSplitFlow, "{slots.name}", "{slots.nme}", 1)
	_, err := Load([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), `close farewell references`) {
		t.Errorf("Load = %v, want an error on the farewell's slot", err)
	}
}
