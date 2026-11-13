// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"strings"
	"testing"
)

// #553: FLOW_INSTRUCTED needs no act of the caller, so it is the reason a wrong
// hangup carries. It is accepted only in a phase that declares
// isFlowHangupAllowed; anywhere else a hangup with it is refused as a
// conversation and runs nothing. The phase is the one the tool batch was made
// in, as for the allowlist.

const (
	flowHangupRefusedLog = "hangup refused: the phase does not let the flow order a hangup"
	flowInstructed       = `{"reason":"FLOW_INSTRUCTED"}`
)

func TestFlowInstructedIsRefusedInAPhaseThatDoesNotDeclareIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lang, args string
		want       []string
	}{
		"en": {LangEN, flowInstructed, []string{"the call was not ended", "the flow does not order an ending in this phase",
			"Answer the caller", "says goodbye", "asks to end the call"}},
		"en, written loosely": {LangEN, `{"reason":" flow_instructed "}`, []string{"the call was not ended"}},
		"zh": {LangZH, flowInstructed, []string{"通话没有结束", "当前环节的流程没有要求结束通话", "回答来电者",
			"来电者道别", "来电者要求结束通话"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			actions := &fakeActions{}
			r, logs := reasonRuntime(t, actions, tc.lang)
			turnsBefore, _ := r.Engine().Slot("turns")
			if !r.IsHangupReasonRefused(tc.args) {
				t.Error("IsHangupReasonRefused says the hangup would run")
			}

			output, moved := r.Dispatch(t.Context(), ToolHangup, tc.args)
			got := decodeOutput(t, output)
			if got["ok"] != "0" {
				t.Errorf("ok = %v, want 0", got["ok"])
			}
			errText, _ := got["error"].(string)
			for _, fragment := range append(tc.want, ToolHangup, HangupCallerGoodbye,
				HangupCallerDeclinedMoreHelp, HangupCallerAskedToEnd) {
				if !strings.Contains(errText, fragment) {
					t.Errorf("error = %q, lacks %q", errText, fragment)
				}
			}
			// Offered back, the reason would only be given again.
			if strings.Contains(errText, HangupFlowInstructed) {
				t.Errorf("error = %q, offers %s back", errText, HangupFlowInstructed)
			}
			if hint, _ := got["hint"].(string); !strings.Contains(hint, r.Engine().Instruction()) {
				t.Errorf("hint = %q, want the current phase's instruction", hint)
			}
			if moved != "" || r.Engine().NodeID() != "talk" {
				t.Errorf("moved to %q (now in %q), want no move", moved, r.Engine().NodeID())
			}
			if len(actions.hangups) != 0 {
				t.Errorf("the application was asked to hang up: %v", actions.hangups)
			}
			if turns, _ := r.Engine().Slot("turns"); turns != turnsBefore {
				t.Errorf("turns = %v, want %v: a refused hangup is no turn", turns, turnsBefore)
			}
			for _, fragment := range []string{flowHangupRefusedLog, "node=talk", "reason=FLOW_INSTRUCTED"} {
				if !strings.Contains(logs.String(), fragment) {
					t.Errorf("the log lacks %q:\n%s", fragment, logs.String())
				}
			}
			if strings.Contains(logs.String(), "tool ran") {
				t.Errorf("the refused hangup ran:\n%s", logs.String())
			}
		})
	}
}

func TestFlowInstructedIsAcceptedInAPhaseThatDeclaresIt(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, logs := reasonRuntimeOn(t, declaredReasonFlow, actions, LangZH)
	if r.IsHangupReasonRefused(flowInstructed) {
		t.Error("IsHangupReasonRefused says a FLOW_INSTRUCTED hangup in a declared phase is refused")
	}
	output, moved := r.Dispatch(t.Context(), ToolHangup, flowInstructed)
	if decodeOutput(t, output)["ok"] != "1" || moved != "goodbye" {
		t.Errorf("output %s, moved %q; want it run into goodbye", output, moved)
	}
	if len(actions.hangups) != 1 || actions.hangups[0].Reason != HangupFlowInstructed {
		t.Errorf("the application got %+v, want reason %s", actions.hangups, HangupFlowInstructed)
	}
	if strings.Contains(logs.String(), "hangup refused") {
		t.Errorf("a hangup the phase allows was refused:\n%s", logs.String())
	}
}

// The field changes nothing for the caller's reasons, declared or not.
func TestTheCallersReasonsDoNotDependOnTheField(t *testing.T) {
	t.Parallel()
	for _, flowJSON := range []string{reasonFlow, declaredReasonFlow} {
		for _, reason := range []string{HangupCallerGoodbye, HangupCallerDeclinedMoreHelp, HangupCallerAskedToEnd} {
			actions := &fakeActions{}
			r, _ := reasonRuntimeOn(t, flowJSON, actions, LangEN)
			args := `{"reason":"` + reason + `"}`
			if r.IsHangupReasonRefused(args) {
				t.Errorf("%s: IsHangupReasonRefused says it would be refused", reason)
			}
			output, moved := r.Dispatch(t.Context(), ToolHangup, args)
			if decodeOutput(t, output)["ok"] != "1" || moved != "goodbye" || len(actions.hangups) != 1 {
				t.Errorf("%s: output %s, moved %q, hangups %v; want it run", reason, output, moved, actions.hangups)
			}
		}
	}
}

// Over an ending under way, or in a terminal phase, the reason is not judged,
// FLOW_INSTRUCTED included: the closing paths answer the hangup as before.
func TestFlowInstructedIsNotJudgedOnceTheEndingIsUnderwayOrInATerminalPhase(t *testing.T) {
	t.Parallel()
	t.Run("an ending under way", func(t *testing.T) {
		t.Parallel()
		actions := &underwayActions{fakeActions: &fakeActions{}, isUnderway: true}
		r, logs := reasonRuntime(t, actions, LangEN)
		if r.IsHangupReasonRefused(flowInstructed) {
			t.Error("IsHangupReasonRefused says a hangup over an ending under way is refused")
		}
		output, _ := r.Dispatch(t.Context(), ToolHangup, flowInstructed)
		if decodeOutput(t, output)["ok"] != "1" || len(actions.hangups) != 1 {
			t.Errorf("output %s, hangups %v; want it handed to the application", output, actions.hangups)
		}
		if strings.Contains(logs.String(), "hangup refused") {
			t.Errorf("a hangup over an ending under way was judged:\n%s", logs.String())
		}
	})
	t.Run("a terminal phase", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, logs := reasonRuntime(t, actions, LangEN)
		if _, moved := r.Dispatch(t.Context(), ToolHangup, `{"reason":"CALLER_GOODBYE"}`); moved != "goodbye" {
			t.Fatalf("moved to %q, want goodbye", moved)
		}
		if r.IsHangupReasonRefused(flowInstructed) {
			t.Error("IsHangupReasonRefused says a hangup in a terminal phase is refused")
		}
		output, _ := r.Dispatch(t.Context(), ToolHangup, flowInstructed)
		if decodeOutput(t, output)["ok"] != "1" || len(actions.hangups) != 2 {
			t.Errorf("output %s, hangups %v; want the second hangup handed over unjudged", output, actions.hangups)
		}
		if strings.Contains(logs.String(), "hangup refused") {
			t.Errorf("a hangup in a terminal phase was judged:\n%s", logs.String())
		}
	})
}

// batchReasonFlow moves between ask, where the flow orders no hangup, and
// wrong_party, where it does, on every note.
const batchReasonFlow = `{
	"id": "batch-reason-test",
	"specVersion": "v2",
	"initialNode": "ask",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["hangup"],
		"transitions": [{"on": "TOOL_RESULT", "tool": "hangup", "target": "goodbye"}]
	},
	"tools": {
		"note": {"kind": "slot", "slot": "note", "description": "Record a note."}
	},
	"nodes": {
		"ask": {"instruction": "Ask whether you are speaking with the account holder.", "tools": ["note"],
			"transitions": [{"on": "TOOL_RESULT", "tool": "note", "target": "wrong_party"}]},
		"wrong_party": {
			"instruction": "Not the account holder: call hangup with reason FLOW_INSTRUCTED.",
			"isFlowHangupAllowed": true,
			"tools": ["note"],
			"transitions": [{"on": "TOOL_RESULT", "tool": "note", "target": "ask"}]
		},
		"goodbye": {"instruction": "Say goodbye.", "announce": "Goodbye.", "tools": [], "isTerminal": true}
	}
}`

// A hangup is judged in the phase its response was made in: a move made by an
// earlier tool of the same response neither grants FLOW_INSTRUCTED nor takes
// it away, and IsHangupReasonRefused says the same as Dispatch.
func TestFlowInstructedIsJudgedInThePhaseTheBatchWasMadeIn(t *testing.T) {
	t.Parallel()
	t.Run("made where it is not allowed, after a move into a phase where it is", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, logs := reasonRuntimeOn(t, batchReasonFlow, actions, LangEN)
		r.BeginToolBatch()
		if _, moved := r.Dispatch(t.Context(), "note", `{"value":"not the holder"}`); moved != "wrong_party" {
			t.Fatalf("moved to %q, want wrong_party", moved)
		}
		if !r.IsHangupReasonRefused(flowInstructed) {
			t.Error("IsHangupReasonRefused says the hangup would run")
		}
		output, moved := r.Dispatch(t.Context(), ToolHangup, flowInstructed)
		r.EndToolBatch()
		if decodeOutput(t, output)["ok"] != "0" || moved != "" || len(actions.hangups) != 0 {
			t.Errorf("output %s, moved %q, hangups %v; want the refusal", output, moved, actions.hangups)
		}
		if !strings.Contains(logs.String(), flowHangupRefusedLog) || !strings.Contains(logs.String(), "node=ask") {
			t.Errorf("the refusal does not name the phase it was judged in:\n%s", logs.String())
		}
		// The next response is made in wrong_party, where the flow orders it.
		if r.IsHangupReasonRefused(flowInstructed) {
			t.Error("IsHangupReasonRefused refuses it in wrong_party")
		}
		if output, moved := r.Dispatch(t.Context(), ToolHangup, flowInstructed); decodeOutput(t, output)["ok"] != "1" ||
			moved != "goodbye" {
			t.Errorf("output %s, moved %q; want it run from wrong_party", output, moved)
		}
	})
	t.Run("made where it is allowed, after a move into a phase where it is not", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, logs := reasonRuntimeOn(t, batchReasonFlow, actions, LangEN)
		if _, moved := r.Dispatch(t.Context(), "note", `{"value":"not the holder"}`); moved != "wrong_party" {
			t.Fatalf("moved to %q, want wrong_party", moved)
		}
		r.BeginToolBatch()
		if _, moved := r.Dispatch(t.Context(), "note", `{"value":"after all"}`); moved != "ask" {
			t.Fatalf("moved to %q, want ask", moved)
		}
		if r.IsHangupReasonRefused(flowInstructed) {
			t.Error("IsHangupReasonRefused says the hangup would be refused")
		}
		output, moved := r.Dispatch(t.Context(), ToolHangup, flowInstructed)
		r.EndToolBatch()
		if decodeOutput(t, output)["ok"] != "1" || moved != "goodbye" || len(actions.hangups) != 1 {
			t.Errorf("output %s, moved %q, hangups %v; want it run", output, moved, actions.hangups)
		}
		if strings.Contains(logs.String(), "hangup refused") {
			t.Errorf("refused:\n%s", logs.String())
		}
	})
}

func TestIsFlowHangupAllowedIsRejectedOnATerminalPhase(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(reasonFlow, `"tools": [], "isTerminal": true`,
		`"tools": [], "isTerminal": true, "isFlowHangupAllowed": true`, 1)
	_, err := Load([]byte(doc))
	want := `phase "goodbye" is terminal but declares isFlowHangupAllowed`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Load = %v, want an error containing %q", err, want)
	}
	if _, err := Load([]byte(declaredReasonFlow)); err != nil {
		t.Errorf("a phase that is not terminal may declare it: %v", err)
	}
}

func TestIsFlowHangupAllowedIsKnownToTheUnknownFieldCheck(t *testing.T) {
	t.Parallel()
	if found := unknownFields([]byte(declaredReasonFlow)); len(found) != 0 {
		t.Errorf("isFlowHangupAllowed reported as unknown: %v", found)
	}
	misspelt := strings.Replace(declaredReasonFlow, "isFlowHangupAllowed", "isFlowHangupAlowed", 1)
	found := unknownFields([]byte(misspelt))
	if len(found) != 1 || found[0].Suggestion != "isFlowHangupAllowed" {
		t.Errorf("a misspelt field = %v, want one suggesting isFlowHangupAllowed", found)
	}
}

// The reason's description says what FLOW_INSTRUCTED is for: a phase whose
// instructions order the ending, not a conversation the caller has not ended.
func TestFlowInstructedIsDescribedAsThePhasesOrder(t *testing.T) {
	t.Parallel()
	for lang, want := range map[string][]string{
		LangEN: {"FLOW_INSTRUCTED: only when the instructions of the current phase order you to end the call now",
			"never to end a conversation the caller has not ended"},
		LangZH: {"FLOW_INSTRUCTED：只在当前环节的指令要求你现在结束通话时使用", "来电者没有结束对话时不要用它"},
	} {
		parameters := string(builtinSchemas(lang, nil)[ToolHangup].Parameters)
		for _, fragment := range want {
			if !strings.Contains(parameters, fragment) {
				t.Errorf("%s: the reason's description lacks %q: %s", lang, fragment, parameters)
			}
		}
	}
}
