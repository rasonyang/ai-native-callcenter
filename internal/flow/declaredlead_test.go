// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// declaredLeadFlow is a campaign pitch whose classification moves straight
// into the phase that closes the call, with a lead declared on every phase
// the call can end in, and on one it can leave and come back to (objection).
// Every way into a phase is reachable from it: a tool's transition, an
// authored NO_INPUT rule, the default NO_INPUT rule to closingTarget, maxTurns
// to fallbackTarget, the turns-without-a-tool wall, the time limit and the
// goodbye without a hangup.
const declaredLeadFlow = `{
	"id": "declared-lead",
	"specVersion": "v2",
	"initialNode": "pitch",
	"global": {
		"persona": "You call customers about an offer.",
		"closingTarget": "close_silent",
		"maxTurnsWithoutTool": 2,
		"fallbackTarget": "fallback",
		"maxTurns": 6
	},
	"tools": {
		"classify": {"kind": "slot", "slot": "answer", "description": "The customer's answer.",
			"enum": ["yes", "stop", "maybe", "back", "other"]},
		"agree_words": {"kind": "slot", "slot": "agree_words", "description": "What they said, in their words."}
	},
	"nodes": {
		"pitch": {
			"instruction": "Make the offer.",
			"tools": ["classify", "agree_words", "hangup"],
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "yes"}, "target": "close_high"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "stop"}, "target": "close_dnc"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "maybe"}, "target": "objection"}
			]
		},
		"objection": {
			"instruction": "Answer the objection.",
			"tools": ["classify", "agree_words"],
			"lead": {"intent": "LOW", "notes": "{slots.agree_words}"},
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "yes"}, "target": "close_high"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "back"}, "target": "pitch"},
				{"on": "NO_INPUT", "condition": {"slot": "noInput.count", "op": "GTE", "value": 2}, "target": "close_none"}
			]
		},
		"close_high": {
			"instruction": "Close.", "isTerminal": true,
			"announce": "We will send it over. Goodbye.",
			"lead": {"intent": "HIGH", "notes": {"en": "Agreed: {slots.agree_words}", "zh": "已同意：{slots.agree_words}"}}
		},
		"close_dnc": {
			"instruction": "Close.", "isTerminal": true,
			"lead": {"intent": "DO_NOT_CALL", "notes": "Asked not to be called again."}
		},
		"close_none": {
			"instruction": "Close.", "isTerminal": true,
			"lead": {"intent": "NONE"}
		},
		"close_silent": {
			"instruction": "Close.", "isTerminal": true,
			"lead": {"intent": "NONE", "notes": "Closed in {slots.node}."}
		},
		"fallback": {
			"instruction": "Close.", "isTerminal": true,
			"lead": {"intent": "LOW", "notes": "runaway"}
		}
	}
}`

// declaredLeadRuntime runs flowJSON in lang with logs captured; a campaign
// call offers record_lead as aicall and flowsim do.
func declaredLeadRuntime(t *testing.T, actions Actions, isCampaign bool, flowJSON, lang string,
	slots map[string]any) (*Runtime, *logCapture) {
	t.Helper()
	spec, err := Load([]byte(flowJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logs := &logCapture{}
	r := NewRuntime(NewEngine(spec, lang, slots, logs.logger()), actions, NewBackend(""), nil, logs.logger())
	if isCampaign {
		r.Deny(ToolTransferToAgent, ToolTakeMessage)
		r.OfferRecordLead(t.Context())
	}
	return r, logs
}

func dispatchOK(t *testing.T, r *Runtime, tool, args string) {
	t.Helper()
	output, _ := r.Dispatch(t.Context(), tool, args)
	if decodeOutput(t, output)["ok"] != "1" {
		t.Fatalf("%s %s: %s", tool, args, output)
	}
}

func wantLeads(t *testing.T, got []LeadRequest, want ...LeadRequest) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("leads written = %q, want %q", got, want)
	}
}

// logLines decodes the JSON log lines whose msg is msg.
func logLines(t *testing.T, logs *logCapture, msg string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for line := range strings.Lines(logs.String()) {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if record["msg"] == msg {
			found = append(found, record)
		}
	}
	return found
}

//
// Loading.
//

func TestALeadDeclarationLoads(t *testing.T) {
	t.Parallel()
	spec, err := Load([]byte(declaredLeadFlow))
	if err != nil {
		t.Fatal(err)
	}
	if lead := spec.Nodes["close_high"].Lead; lead == nil || lead.Intent != LeadHigh ||
		lead.Notes.For(LangZH) != "已同意：{slots.agree_words}" || lead.Notes.For(LangEN) != "Agreed: {slots.agree_words}" {
		t.Errorf("close_high lead = %+v", lead)
	}
	if lead := spec.Nodes["close_none"].Lead; lead == nil || lead.Intent != LeadNone || !lead.Notes.IsEmpty() {
		t.Errorf("close_none lead = %+v, want NONE without notes", lead)
	}
	if spec.Nodes["pitch"].Lead != nil {
		t.Error("a phase that declares no lead has one")
	}
	for _, intent := range LeadIntents {
		if _, err := Load([]byte(leadOnClose(`{"intent": "` + intent + `"}`))); err != nil {
			t.Errorf("%s: %v", intent, err)
		}
	}
	if spec, err := Load([]byte(leadOnClose(`null`))); err != nil || spec.Nodes["close"].Lead != nil {
		t.Errorf("a null lead is no lead: %v", err)
	}
	userData := leadOnClose(`{"intent": "MEDIUM", "notes": "{slots.userData.plan} for {slots.caller}, {slots.said}"}`)
	if _, err := Load([]byte(userData)); err != nil {
		t.Errorf("notes reading userData, an engine slot and a slot tool's slot: %v", err)
	}
}

// leadOnClose is a two-phase flow whose terminal phase "close" declares lead,
// given as raw JSON.
func leadOnClose(lead string) string {
	return `{
		"id": "lead-load", "specVersion": "v2", "initialNode": "talk",
		"global": {"persona": "You call people."},
		"tools": {"said": {"kind": "slot", "slot": "said", "description": "What they said."}},
		"nodes": {
			"talk": {"instruction": "Talk.", "tools": ["said"],
				"transitions": [{"on": "TOOL_RESULT", "tool": "said", "target": "close"}]},
			"close": {"instruction": "Close.", "isTerminal": true, "lead": ` + lead + `}
		}
	}`
}

// A bad declaration is one of the flow's problems, named by its phase, so the
// draft-save path reports it with the rest (422 with params.problems) rather
// than as a document that cannot be parsed.
func TestABadLeadDeclarationIsAProblemOfItsPhase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, lead string
		want       string
		// notWant is a problem that must not be reported as well.
		notWant string
	}{
		{"a string", `"HIGH"`, `phase "close" lead must be an object {"intent", "notes"}`, "declares no intent"},
		{"a list", `["HIGH"]`, `phase "close" lead must be an object`, "declares no intent"},
		{"a number", `5`, `phase "close" lead must be an object`, ""},
		{"an intent outside the enum", `{"intent": "BANANAS"}`,
			`phase "close" lead intent is "BANANAS"; it is one of HIGH, MEDIUM, LOW, NONE, DO_NOT_CALL`, ""},
		{"a lowercase intent", `{"intent": "high"}`, `phase "close" lead intent is "high"; write it as "HIGH", in capitals`, ""},
		{"an intent with spaces", `{"intent": " LOW "}`, `write it as "LOW"`, ""},
		{"an empty intent", `{"intent": ""}`, `phase "close" lead declares no intent; it is one of HIGH`, ""},
		{"no intent", `{"notes": "x"}`, `phase "close" lead declares no intent`, ""},
		{"an empty object", `{}`, `phase "close" lead declares no intent`, ""},
		{"an intent that is not a string", `{"intent": 5}`, `phase "close" lead intent must be a string`, "declares no intent"},
		{"a misspelt field", `{"intent": "HIGH", "note": "typo"}`,
			`phase "close" lead has unknown field "note"; did you mean "notes"?`, ""},
		{"an unknown field", `{"intent": "HIGH", "comment": "typo"}`,
			`phase "close" lead has unknown field "comment"; a lead has intent and notes`, ""},
		{"notes that are not text", `{"intent": "HIGH", "notes": 5}`,
			`phase "close" lead notes must be a string or an object keyed by language`, ""},
		{"an unknown slot in the notes", `{"intent": "HIGH", "notes": "{slots.nowhere}"}`,
			`phase close lead notes references {slots.nowhere}, which nothing in this flow writes`, ""},
		{"an unknown slot in one language", `{"intent": "HIGH", "notes": {"en": "ok", "zh": "{slots.nowhere}"}}`,
			`phase close lead notes references {slots.nowhere}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(leadOnClose(tc.lead)))
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want a ValidationError", err)
			}
			joined := strings.Join(invalid.Problems, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("problems:\n%s\nwant one containing %q", joined, tc.want)
			}
			if tc.notWant != "" && strings.Contains(joined, tc.notWant) {
				t.Errorf("problems:\n%s\nreport %q as well", joined, tc.notWant)
			}
		})
	}
}

// Every problem at once: a bad lead does not hide the flow's other problems,
// nor do several of its own hide each other.
func TestBadLeadDeclarationsAreReportedTogether(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(leadOnClose(`{"intent": "high", "notes": "{slots.nowhere}", "extra": 1}`),
		`"persona": "You call people."`, `"persona": ""`, 1)
	_, err := Load([]byte(doc))
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	joined := strings.Join(invalid.Problems, "\n")
	for _, want := range []string{"global.persona is empty", `unknown field "extra"`, `write it as "HIGH"`, "{slots.nowhere}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems:\n%s\nlack %q", joined, want)
		}
	}
}

func TestUserDataNamesIncludeLeadNotes(t *testing.T) {
	t.Parallel()
	spec, err := Load([]byte(leadOnClose(`{"intent": "MEDIUM", "notes": {"en": "{slots.userData.plan}", "zh": "{slots.userData.planZh}"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := UserDataNames(spec); !slices.Equal(got, []string{"plan", "planZh"}) {
		t.Errorf("UserDataNames = %v", got)
	}
}

//
// Recording on entry.
//

func TestTheInitialPhaseRecordsItsLeadWhenTheCallIsMadeACampaignCall(t *testing.T) {
	t.Parallel()
	initial := strings.Replace(leadOnClose(`{"intent": "HIGH"}`), `"tools": ["said"],`,
		`"tools": ["said"], "lead": {"intent": "NONE", "notes": "answered by {slots.caller}"},`, 1)
	actions := &fakeActions{}
	r, _ := declaredLeadRuntime(t, actions, true, initial, LangEN, map[string]any{"caller": "13800138000"})
	wantLeads(t, actions.leads, LeadRequest{Intent: LeadNone, Notes: "answered by 13800138000"})

	r.OfferRecordLead(t.Context())
	wantLeads(t, actions.leads, LeadRequest{Intent: LeadNone, Notes: "answered by 13800138000"})
	if turns, _ := r.Engine().Slot("turns"); turns != nil {
		t.Errorf("a declared lead counted as a turn: %v", turns)
	}
}

// Each way the engine moves a call records the lead of the phase it enters.
func TestEveryWayIntoAPhaseRecordsItsLead(t *testing.T) {
	t.Parallel()
	silent := LeadRequest{Intent: LeadNone, Notes: "Closed in close_silent."}
	for _, tc := range []struct {
		name string
		move func(t *testing.T, r *Runtime)
		want []LeadRequest
	}{
		{"a tool's transition", func(t *testing.T, r *Runtime) {
			dispatchOK(t, r, "agree_words", `{"value": "send it, \"really\" {now}"}`)
			dispatchOK(t, r, "classify", `{"value": "yes"}`)
		}, []LeadRequest{{Intent: LeadHigh, Notes: `Agreed: send it, "really" {now}`}}},
		{"an authored NO_INPUT rule", func(t *testing.T, r *Runtime) {
			dispatchOK(t, r, "classify", `{"value": "maybe"}`)
			r.OnNoInput()
			r.OnNoInput()
		}, []LeadRequest{{Intent: LeadLow}, {Intent: LeadNone}}},
		{"the default NO_INPUT rule to closingTarget", func(t *testing.T, r *Runtime) {
			for range defaultNoInputLimit {
				r.OnNoInput()
			}
		}, []LeadRequest{silent}},
		{"maxTurns to fallbackTarget", func(t *testing.T, r *Runtime) {
			for range 7 {
				r.Dispatch(t.Context(), "agree_words", `{"value": "hm"}`)
			}
		}, []LeadRequest{{Intent: LeadLow, Notes: "runaway"}}},
		{"the turns-without-a-tool wall to closingTarget", func(t *testing.T, r *Runtime) {
			for range 3 {
				r.OnCallerSpoke("and?")
				r.OnBotTurnDone(false, false)
			}
			if r.CloseAtTurnsWithoutToolWall() != "close_silent" {
				t.Fatal("the wall did not close the call")
			}
		}, []LeadRequest{silent}},
		{"the time limit to closingTarget", func(t *testing.T, r *Runtime) {
			if r.CloseAtSessionLimit() != "close_silent" {
				t.Fatal("the limit did not close the call")
			}
		}, []LeadRequest{silent}},
		{"a goodbye with no hangup to closingTarget", func(t *testing.T, r *Runtime) {
			if r.CloseAtUnhungFarewell() != "close_silent" {
				t.Fatal("the farewell did not close the call")
			}
		}, []LeadRequest{silent}},
		{"a move in a tool batch", func(t *testing.T, r *Runtime) {
			r.BeginToolBatch()
			dispatchOK(t, r, "agree_words", `{"value": "ok"}`)
			dispatchOK(t, r, "classify", `{"value": "stop"}`)
			r.EndToolBatch()
		}, []LeadRequest{{Intent: LeadDoNotCall, Notes: "Asked not to be called again."}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actions := &fakeActions{}
			r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
			tc.move(t, r)
			wantLeads(t, actions.leads, tc.want...)
		})
	}
}

func TestEnteringAPhaseAgainRecordsItsLeadAgain(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
	dispatchOK(t, r, "agree_words", `{"value": "not sure"}`)
	dispatchOK(t, r, "classify", `{"value": "maybe"}`)
	dispatchOK(t, r, "classify", `{"value": "back"}`)
	dispatchOK(t, r, ToolRecordLead, `{"intent": "HIGH", "notes": "warming up"}`)
	dispatchOK(t, r, "agree_words", `{"value": "too pricey"}`)
	dispatchOK(t, r, "classify", `{"value": "maybe"}`)
	wantLeads(t, actions.leads,
		LeadRequest{Intent: LeadLow, Notes: "not sure"},
		LeadRequest{Intent: LeadHigh, Notes: "warming up"},
		LeadRequest{Intent: LeadLow, Notes: "too pricey"})
	if visits, _ := r.Engine().Slot("objection.visits"); visits != 2 {
		t.Errorf("objection.visits = %v", visits)
	}
}

// Last write wins: the declared lead is one more write, ordered with the
// model's own record_lead calls, inside a batch as across responses. A
// record_lead made after the move into a declared phase is not one of them
// (TestAPhaseThatDeclaresALeadRefusesRecordLead).
func TestTheLastLeadWrittenWinsWhoeverWroteIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		move func(t *testing.T, r *Runtime)
		last LeadRequest
	}{
		{"the model, then a phase", func(t *testing.T, r *Runtime) {
			dispatchOK(t, r, ToolRecordLead, `{"intent": "MEDIUM", "notes": "call back next week"}`)
			dispatchOK(t, r, "classify", `{"value": "stop"}`)
		}, LeadRequest{Intent: LeadDoNotCall, Notes: "Asked not to be called again."}},
		{"one response: the model, then a phase", func(t *testing.T, r *Runtime) {
			r.BeginToolBatch()
			dispatchOK(t, r, ToolRecordLead, `{"intent": "MEDIUM"}`)
			dispatchOK(t, r, "classify", `{"value": "yes"}`)
			r.EndToolBatch()
		}, LeadRequest{Intent: LeadHigh, Notes: "Agreed:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actions := &fakeActions{}
			r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
			tc.move(t, r)
			if len(actions.leads) != 2 || actions.leads[1] != tc.last {
				t.Fatalf("leads written = %q, want two ending in %q", actions.leads, tc.last)
			}
		})
	}
}

func TestADeclaredLeadIsIgnoredOutsideACampaignCall(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, logs := declaredLeadRuntime(t, actions, false, declaredLeadFlow, LangEN, nil)
	dispatchOK(t, r, "classify", `{"value": "maybe"}`)
	dispatchOK(t, r, "classify", `{"value": "yes"}`)
	if r.Engine().NodeID() != "close_high" {
		t.Fatalf("node = %s", r.Engine().NodeID())
	}
	wantLeads(t, actions.leads)
	ignored := logLines(t, logs, "declared lead ignored: not a campaign call")
	if len(ignored) != 2 || ignored[0]["level"] != "DEBUG" || ignored[1]["node"] != "close_high" ||
		ignored[1]["intent"] != LeadHigh {
		t.Errorf("ignored lines = %v", ignored)
	}
	if recorded := logLines(t, logs, "declared lead recorded"); len(recorded) != 0 {
		t.Errorf("recorded lines on a non-campaign call: %v", recorded)
	}
}

// The notes are rendered when the phase is entered, in the call's language;
// a slot written afterwards does not reach them.
func TestDeclaredNotesAreRenderedAtEntry(t *testing.T) {
	t.Parallel()
	t.Run("in the call's language", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangZH, nil)
		dispatchOK(t, r, "agree_words", `{"value": "好的，发我短信"}`)
		dispatchOK(t, r, "classify", `{"value": "yes"}`)
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadHigh, Notes: "已同意：好的，发我短信"})
	})
	t.Run("from the slots at entry", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "agree_words", `{"value": "maybe later"}`)
		dispatchOK(t, r, "classify", `{"value": "maybe"}`)
		dispatchOK(t, r, "agree_words", `{"value": "never mind"}`)
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadLow, Notes: "maybe later"})
	})
	t.Run("empty when the slot is", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "classify", `{"value": "maybe"}`)
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadLow, Notes: ""})
	})
	t.Run("cut, by characters, past the cap", func(t *testing.T) {
		t.Parallel()
		long := leadOnClose(`{"intent": "HIGH", "notes": "{slots.userData.a}{slots.userData.b}{slots.userData.c}{slots.userData.d}"}`)
		part := strings.Repeat("客", MaxSlotValueLen)
		actions := &fakeActions{}
		r, logs := declaredLeadRuntime(t, actions, true, long, LangEN,
			SeedSlots("1380", "", "", map[string]any{"a": part, "b": part, "c": part, "d": part}))
		dispatchOK(t, r, "said", `{"value": "yes"}`)
		if len(actions.leads) != 1 || utf8.RuneCountInString(actions.leads[0].Notes) != MaxLeadNotesRunes ||
			actions.leads[0].Notes != strings.Repeat("客", MaxLeadNotesRunes) {
			t.Fatalf("leads = %d, notes of %d runes", len(actions.leads), utf8.RuneCountInString(actions.leads[0].Notes))
		}
		if len(logLines(t, logs, "lead notes cut")) != 1 {
			t.Error("the cut was not logged")
		}
	})
}

func TestARecordedDeclaredLeadIsLoggedWithWhatItReplaced(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, logs := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
	dispatchOK(t, r, ToolRecordLead, `{"intent": "MEDIUM"}`)
	dispatchOK(t, r, "classify", `{"value": "maybe"}`)
	dispatchOK(t, r, "classify", `{"value": "yes"}`)

	lines := logLines(t, logs, "declared lead recorded")
	if len(lines) != 2 {
		t.Fatalf("recorded lines = %v", lines)
	}
	first, second := lines[0], lines[1]
	if first["level"] != "INFO" || first["node"] != "objection" || first["intent"] != LeadLow ||
		first["isReplacing"] != true || first["replacedIntent"] != LeadMedium || first["replacedBy"] != ToolRecordLead {
		t.Errorf("first = %v", first)
	}
	if second["node"] != "close_high" || second["intent"] != LeadHigh || second["isReplacing"] != true ||
		second["replacedIntent"] != LeadLow || second["replacedBy"] != "phase objection" {
		t.Errorf("second = %v", second)
	}
}

// leadRefuser stands for an attempt that is no longer in flight.
type leadRefuser struct{ fakeActions }

func (*leadRefuser) RecordLead(context.Context, LeadRequest) (Result, error) {
	return Failed("NOT_RECORDED", ""), nil
}

func TestADeclaredLeadThatCannotBeSavedIsLoggedAndTheCallGoesOn(t *testing.T) {
	t.Parallel()
	r, logs := declaredLeadRuntime(t, &leadRefuser{}, true, declaredLeadFlow, LangEN, nil)
	dispatchOK(t, r, "classify", `{"value": "maybe"}`)
	if r.Engine().NodeID() != "objection" {
		t.Fatalf("node = %s", r.Engine().NodeID())
	}
	failed := logLines(t, logs, "declared lead not recorded")
	if len(failed) != 1 || failed[0]["level"] != "WARN" || failed[0]["error"] != "NOT_RECORDED" {
		t.Errorf("failure lines = %v", failed)
	}
	if recorded := logLines(t, logs, "declared lead recorded"); len(recorded) != 0 {
		t.Errorf("a refused lead was logged as recorded: %v", recorded)
	}
}

// Returning to a phase after a handover that never happened is not entering
// it (Engine.ReturnTo): it writes nothing.
func TestReturningToAPhaseRecordsNothing(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
	dispatchOK(t, r, "classify", `{"value": "maybe"}`)
	dispatchOK(t, r, "classify", `{"value": "yes"}`)
	if !r.ReturnTo("objection") {
		t.Fatal("could not return")
	}
	wantLeads(t, actions.leads, LeadRequest{Intent: LeadLow}, LeadRequest{Intent: LeadHigh, Notes: "Agreed:"})
}

// refusedOutput is what the model reads when its record_lead is refused.
const refusedOutput = "this phase records the lead itself; do not call record_lead, and do not mention this to the customer"

func wantLeadRefused(t *testing.T, r *Runtime, args string) {
	t.Helper()
	output, node := r.Dispatch(t.Context(), ToolRecordLead, args)
	got := decodeOutput(t, output)
	if got["ok"] != "0" || got["error"] != refusedOutput || got["hint"] != nil || node != "" {
		t.Fatalf("record_lead %s: %s (node %q)", args, output, node)
	}
}

// While the call is in a phase that declares a lead, the model's record_lead
// is refused: nothing is written and the declared lead stands. The tool is
// still offered; it is answered, not withdrawn.
func TestAPhaseThatDeclaresALeadRefusesRecordLead(t *testing.T) {
	t.Parallel()
	t.Run("a terminal phase", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "agree_words", `{"value": "yes please"}`)
		dispatchOK(t, r, "classify", `{"value": "yes"}`)
		wantLeadRefused(t, r, `{"intent": "NONE", "notes": "changed my mind"}`)
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadHigh, Notes: "Agreed: yes please"})
		if r.lastLead != LeadHigh || r.lastLeadBy != "phase close_high" {
			t.Errorf("lastLead = %q by %q", r.lastLead, r.lastLeadBy)
		}
		if !slices.ContainsFunc(r.Tools(), func(tool provider.ToolSpec) bool { return tool.Name == ToolRecordLead }) {
			t.Error("record_lead is no longer offered")
		}
	})
	t.Run("a phase the call can leave", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "agree_words", `{"value": "pricey"}`)
		dispatchOK(t, r, "classify", `{"value": "maybe"}`)
		wantLeadRefused(t, r, `{"intent": "HIGH"}`)
		wantLeadRefused(t, r, `{"intent": "not an intent"}`)
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadLow, Notes: "pricey"})
	})
	t.Run("a phase without a declared lead accepts it", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, ToolRecordLead, `{"intent": "MEDIUM", "notes": "later"}`)
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadMedium, Notes: "later"})
	})
	t.Run("re-entering a declared phase still writes", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "agree_words", `{"value": "one"}`)
		dispatchOK(t, r, "classify", `{"value": "maybe"}`)
		wantLeadRefused(t, r, `{"intent": "HIGH"}`)
		dispatchOK(t, r, "classify", `{"value": "back"}`)
		dispatchOK(t, r, "agree_words", `{"value": "two"}`)
		dispatchOK(t, r, "classify", `{"value": "maybe"}`)
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadLow, Notes: "one"}, LeadRequest{Intent: LeadLow, Notes: "two"})
	})
	t.Run("a non-campaign call is refused as before", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, false, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "classify", `{"value": "yes"}`)
		if r.IsLeadSetByPhase() {
			t.Error("IsLeadSetByPhase on a call that records no leads")
		}
		output, _ := r.Dispatch(t.Context(), ToolRecordLead, `{"intent": "HIGH"}`)
		got := decodeOutput(t, output)
		if got["ok"] != "0" || !strings.Contains(got["error"].(string), "not available in the current phase") {
			t.Errorf("output = %s", output)
		}
		wantLeads(t, actions.leads)
	})
}

// The phase judged is the one the call is in when the tool is dispatched, not
// the one the response was made in.
func TestRecordLeadInABatchIsJudgedInThePhaseTheCallIsIn(t *testing.T) {
	t.Parallel()
	t.Run("before the move: written, then replaced on entry", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		r.BeginToolBatch()
		dispatchOK(t, r, ToolRecordLead, `{"intent": "MEDIUM"}`)
		dispatchOK(t, r, "classify", `{"value": "yes"}`)
		r.EndToolBatch()
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadMedium}, LeadRequest{Intent: LeadHigh, Notes: "Agreed:"})
	})
	t.Run("after the move into a declared phase: refused", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		r.BeginToolBatch()
		dispatchOK(t, r, "classify", `{"value": "yes"}`)
		wantLeadRefused(t, r, `{"intent": "NONE", "notes": "no thanks"}`)
		r.EndToolBatch()
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadHigh, Notes: "Agreed:"})
	})
	t.Run("after the call left the declared phase: accepted", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "classify", `{"value": "maybe"}`)
		r.BeginToolBatch()
		dispatchOK(t, r, "classify", `{"value": "back"}`)
		dispatchOK(t, r, ToolRecordLead, `{"intent": "MEDIUM", "notes": "left the phase"}`)
		r.EndToolBatch()
		wantLeads(t, actions.leads, LeadRequest{Intent: LeadLow}, LeadRequest{Intent: LeadMedium, Notes: "left the phase"})
	})
}

// The refusal line names the phase and both intents, and never the notes.
func TestTheLeadRefusalIsLoggedWithoutTheNotes(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, logs := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
	dispatchOK(t, r, "classify", `{"value": "yes"}`)
	wantLeadRefused(t, r, `{"intent": " none ", "notes": "ZEBRA-NOTES the customer's private words"}`)

	lines := logLines(t, logs, "lead refused: the phase declares its own")
	if len(lines) != 1 {
		t.Fatalf("refusal lines = %v", lines)
	}
	line := lines[0]
	if line["level"] != "INFO" || line["tool"] != ToolRecordLead || line["node"] != "close_high" ||
		line["intent"] != LeadNone || line["declaredIntent"] != LeadHigh {
		t.Errorf("line = %v", line)
	}
	if strings.Contains(logs.String(), "ZEBRA-NOTES") {
		t.Error("the notes reached the log")
	}
}
