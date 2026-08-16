// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// testFlow is a small but complete flow: identify the caller, look their
// account up, and either report or transfer.
const testFlow = `{
	"id": "billing",
	"specVersion": "v2",
	"entry": "billing",
	"initialNode": "welcome",
	"global": {
		"persona": {"en": "You answer for NovaNet billing.", "zh": "你是 NovaNet 账务热线客服。"},
		"rules": {"en": ["Keep replies short."], "zh": ["回复要简短。"]},
		"fallbackTarget": "handoff",
		"maxTurns": 6,
		"alwaysAllowedTools": ["transfer_to_agent", "hangup"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",
			 "condition": {"slot": "result.ok", "op": "EQ", "value": "1"},
			 "target": "handoff"}
		]
	},
	"tools": {
		"lookup_account": {
			"description": {"en": "Look an account up by phone number.", "zh": "按手机号查询账户。"},
			"parameters": {"type": "object", "properties": {"phone": {"type": "string"}}, "required": ["phone"]},
			"http": {
				"path": "/api/account/lookup",
				"body": {"phoneNumber": "{args.phone}", "callerId": "{slots.caller}"},
				"successWhen": {"path": "retCode", "equals": "000000"},
				"errorFrom": "retMsg",
				"result": {"found": "data.found", "name": "data.name", "balance": "data.balance"}
			}
		}
	},
	"nodes": {
		"welcome": {
			"instruction": {"en": "Greet and ask for the account phone number.", "zh": "问候并询问账户手机号。"},
			"tools": ["lookup_account"],
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "lookup_account",
				 "condition": {"slot": "result.found", "op": "EQ", "value": "1"},
				 "target": "report"},
				{"on": "TOOL_RESULT", "tool": "lookup_account",
				 "condition": {"slot": "lookup_account.calls", "op": "GTE", "value": 3},
				 "target": "handoff", "priority": 1},
				{"on": "NO_INPUT",
				 "condition": {"slot": "noInput.count", "op": "GTE", "value": 2},
				 "target": "farewell"}
			]
		},
		"report": {
			"instruction": {"en": "Report the balance for {slots.lookup_account.name}.",
			                "zh": "向 {slots.lookup_account.name} 播报余额。"},
			"tools": ["lookup_account", "take_message"]
		},
		"handoff": {
			"instruction": {"en": "Announce the transfer.", "zh": "播报转接话术。"},
			"tools": []
		},
		"farewell": {
			"instruction": {"en": "Say goodbye.", "zh": "道别。"},
			"tools": [],
			"isTerminal": true
		}
	}
}`

func loadTestFlow(t *testing.T) *Spec {
	t.Helper()
	spec, err := Load([]byte(testFlow))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return spec
}

func testEngine(t *testing.T, lang string) *Engine {
	t.Helper()
	return NewEngine(loadTestFlow(t), lang,
		map[string]any{"caller": "13800138000"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

//
// Loading and validation.
//

func TestLoadAcceptsACompleteFlow(t *testing.T) {
	spec := loadTestFlow(t)

	if spec.ID != "billing" || spec.InitialNode != "welcome" {
		t.Errorf("id=%q initialNode=%q", spec.ID, spec.InitialNode)
	}
	if node, ok := spec.Node("welcome"); !ok || node.ID() != "welcome" {
		t.Error("nodes did not get their ids from the map keys")
	}
	if tool, ok := spec.Tools["lookup_account"]; !ok || tool.Name() != "lookup_account" {
		t.Error("tools did not get their names from the map keys")
	}
}

// Every mistake here would otherwise surface mid-call, as a conversation that
// dead-ends with a caller on the line.
func TestLoadRejectsBrokenFlows(t *testing.T) {
	breakages := []struct {
		name  string
		alter func(string) string
		want  string
	}{
		{"wrong version", func(s string) string {
			return strings.Replace(s, `"specVersion": "v2"`, `"specVersion": "v1"`, 1)
		}, "specVersion"},
		{"initial node missing", func(s string) string {
			return strings.Replace(s, `"initialNode": "welcome"`, `"initialNode": "nowhere"`, 1)
		}, "initialNode"},
		{"transition to nowhere", func(s string) string {
			return strings.Replace(s, `"target": "report"`, `"target": "nowhere"`, 1)
		}, "nowhere"},
		{"unknown operator", func(s string) string {
			return strings.Replace(s, `"op": "EQ"`, `"op": "EQUALS_ISH"`, 1)
		}, "operator"},
		{"unknown tool in a phase", func(s string) string {
			return strings.Replace(s, `"tools": ["lookup_account"]`, `"tools": ["mystery_tool"]`, 1)
		}, "mystery_tool"},
		{"terminal phase with transitions", func(s string) string {
			return strings.Replace(s, `"tools": [],
			"isTerminal": true`,
				`"tools": [], "isTerminal": true,
				"transitions": [{"target": "welcome"}]`, 1)
		}, "terminal"},
		{"no persona", func(s string) string {
			return strings.Replace(s,
				`"persona": {"en": "You answer for NovaNet billing.", "zh": "你是 NovaNet 账务热线客服。"},`,
				`"persona": "",`, 1)
		}, "persona"},
	}

	for _, tt := range breakages {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.alter(testFlow)))
			if err == nil {
				t.Fatal("a broken flow loaded without complaint")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

//
// Localisation.
//

func TestTextFallsBackAcrossLanguagesRatherThanToSilence(t *testing.T) {
	text := Text{EN: "hello"}
	if got := text.For(LangZH); got != "hello" {
		t.Errorf("missing translation rendered %q, want the other language", got)
	}
	both := Text{EN: "hello", ZH: "你好"}
	if got := both.For(LangZH); got != "你好" {
		t.Errorf("got %q", got)
	}
}

func TestLangNormalisation(t *testing.T) {
	for input, want := range map[string]string{
		"zh": LangZH, "zh-CN": LangZH, "ZH": LangZH,
		"en": LangEN, "en-GB": LangEN, "": LangEN, "fr": LangEN,
	} {
		if got := Lang(input); got != want {
			t.Errorf("Lang(%q) = %q, want %q", input, got, want)
		}
	}
}

//
// Predicates.
//

func TestPredicates(t *testing.T) {
	slots := map[string]any{
		"found":   "1",
		"count":   float64(3),
		"name":    "Alice",
		"emptied": "",
		"status":  "IN_REPAIR",
	}

	tests := []struct {
		name      string
		condition Condition
		want      bool
	}{
		{"numeric tolerance: string 1 equals number 1",
			Condition{Slot: "found", Op: OpEqual, Value: float64(1)}, true},
		{"ne", Condition{Slot: "found", Op: OpNotEqual, Value: "0"}, true},
		{"gte on a numeric string", Condition{Slot: "count", Op: OpGreaterThanOrEqual, Value: "3"}, true},
		{"lt fails when equal", Condition{Slot: "count", Op: OpLessThan, Value: 3}, false},
		{"ordering never matches a missing slot",
			Condition{Slot: "absent", Op: OpGreaterThan, Value: 0}, false},
		{"is_null on a missing slot", Condition{Slot: "absent", Op: OpIsNull}, true},
		{"is_not_null", Condition{Slot: "name", Op: OpIsNotNull}, true},
		{"is_empty on an empty string", Condition{Slot: "emptied", Op: OpIsEmpty}, true},
		{"is_not_empty", Condition{Slot: "name", Op: OpIsNotEmpty}, true},
		{"in a list", Condition{Slot: "status", Op: OpIn,
			Value: []any{"RECEIVED", "IN_REPAIR"}}, true},
		{"not_in", Condition{Slot: "status", Op: OpNotIn, Value: []any{"DONE"}}, true},
		{"contains", Condition{Slot: "status", Op: OpContains, Value: "REPAIR"}, true},
		{"not_contains on a missing slot", Condition{Slot: "absent", Op: OpNotContains, Value: "x"}, true},
		{"all requires every branch",
			Condition{All: []Condition{
				{Slot: "found", Op: OpEqual, Value: "1"},
				{Slot: "count", Op: OpGreaterThan, Value: 5},
			}}, false},
		{"any needs one branch",
			Condition{Any: []Condition{
				{Slot: "found", Op: OpEqual, Value: "0"},
				{Slot: "name", Op: OpEqual, Value: "Alice"},
			}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evaluate(&tt.condition, slots)
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}

	if ok, err := evaluate(nil, slots); err != nil || !ok {
		t.Error("a nil condition must always hold")
	}
}

//
// Engine semantics.
//

func TestEngineStartsAtTheInitialPhase(t *testing.T) {
	e := testEngine(t, "en")

	if e.NodeID() != "welcome" {
		t.Errorf("started at %q", e.NodeID())
	}
	if got := e.Instruction(); !strings.Contains(got, "Greet") {
		t.Errorf("instruction = %q", got)
	}
	// Call metadata seeds the slots.
	if caller, _ := e.Slot("caller"); caller != "13800138000" {
		t.Errorf("caller slot = %v", caller)
	}
}

func TestAMatchingResultMovesThePhase(t *testing.T) {
	e := testEngine(t, "en")

	moved := e.OnToolResult("lookup_account",
		map[string]any{"found": "1", "name": "Alice", "balance": "42.10"})
	if moved != "report" {
		t.Fatalf("moved to %q, want report", moved)
	}
	// The new phase's instruction can reference what the tool collected.
	if got := e.Instruction(); !strings.Contains(got, "Alice") {
		t.Errorf("instruction did not substitute the collected name: %q", got)
	}
}

// No match means stay and ask again — not an error, and not a move.
func TestANonMatchingResultStaysPut(t *testing.T) {
	e := testEngine(t, "en")

	if moved := e.OnToolResult("lookup_account", map[string]any{"found": "0"}); moved != "" {
		t.Fatalf("moved to %q on a result no rule matches", moved)
	}
	if e.NodeID() != "welcome" {
		t.Errorf("phase is %q", e.NodeID())
	}
}

// The third failed lookup gives up and hands the caller over; the counter the
// rule reads is maintained by the engine itself.
func TestRepeatedFailuresEscalateThroughThePriorityRule(t *testing.T) {
	e := testEngine(t, "en")

	notFound := map[string]any{"found": "0"}
	if moved := e.OnToolResult("lookup_account", notFound); moved != "" {
		t.Fatalf("first failure moved to %q", moved)
	}
	if moved := e.OnToolResult("lookup_account", notFound); moved != "" {
		t.Fatalf("second failure moved to %q", moved)
	}
	if moved := e.OnToolResult("lookup_account", notFound); moved != "handoff" {
		t.Fatalf("third failure moved to %q, want handoff", moved)
	}
}

// A stale result.x from an earlier tool must never satisfy a later rule.
func TestTransientResultSlotsAreReplacedWholesale(t *testing.T) {
	e := testEngine(t, "en")

	e.OnToolResult("lookup_account", map[string]any{"found": "0", "extra": "stale"})
	e.OnToolResult("lookup_account", map[string]any{"found": "0"})

	if _, ok := e.Slot("result.extra"); ok {
		t.Error("a transient slot from the previous result survived")
	}
	// The namespaced copy is the durable one.
	if _, ok := e.Slot("lookup_account.extra"); !ok {
		t.Error("the namespaced slot did not persist")
	}
}

func TestGlobalTransitionsApplyInEveryPhase(t *testing.T) {
	e := testEngine(t, "en")

	if moved := e.OnToolResult("transfer_to_agent", map[string]any{"ok": "1"}); moved != "handoff" {
		t.Fatalf("the flow-wide transfer rule did not fire: moved to %q", moved)
	}
}

// A model that loops on a tool no rule matches would otherwise keep the caller
// on the line indefinitely.
func TestTheTurnLimitForcesTheFallback(t *testing.T) {
	e := testEngine(t, "en")

	// No rule matches take_message in the welcome phase, so nothing moves —
	// until the limit of six dispatches is crossed.
	for i := range 6 {
		if moved := e.OnToolResult("take_message", map[string]any{"ok": "1"}); moved != "" {
			t.Fatalf("dispatch %d moved to %q before the limit", i+1, moved)
		}
	}
	if moved := e.OnToolResult("take_message", map[string]any{"ok": "1"}); moved != "handoff" {
		t.Fatalf("the dispatch past the limit moved to %q, want the fallback", moved)
	}
}

func TestATerminalPhaseNeverMoves(t *testing.T) {
	e := testEngine(t, "en")

	e.OnNoInput()
	if moved := e.OnNoInput(); moved != "farewell" {
		t.Fatalf("two silences moved to %q, want farewell", moved)
	}
	if !e.IsTerminal() {
		t.Fatal("farewell is not terminal")
	}
	if moved := e.OnToolResult("transfer_to_agent", map[string]any{"ok": "1"}); moved != "" {
		t.Errorf("a terminal phase moved to %q", moved)
	}
}

func TestDeadAirMovesThePhaseOnlyWhenTheRuleSays(t *testing.T) {
	e := testEngine(t, "en")

	if moved := e.OnNoInput(); moved != "" {
		t.Fatalf("one silence moved to %q; the rule wants two", moved)
	}
	if moved := e.OnNoInput(); moved != "farewell" {
		t.Fatalf("two silences moved to %q, want farewell", moved)
	}
}

func TestToolAllowlistPerPhase(t *testing.T) {
	e := testEngine(t, "en")

	if !e.IsToolAllowed("lookup_account") {
		t.Error("the phase's own tool is not allowed")
	}
	if e.IsToolAllowed("take_message") {
		t.Error("a tool from another phase is allowed")
	}
	// The always-available set is exempt from phase rules.
	if !e.IsToolAllowed("transfer_to_agent") || !e.IsToolAllowed("hangup") {
		t.Error("an always-available tool was blocked by the phase")
	}
}

func TestInstructionRendersMissingSlotsAsNothing(t *testing.T) {
	e := testEngine(t, "en")
	e.OnToolResult("lookup_account", map[string]any{"found": "1"}) // no name collected

	if got := e.Instruction(); strings.Contains(got, "{slots.") {
		t.Errorf("an unfilled template leaked to the model: %q", got)
	}
}

func TestChineseCallsGetChineseInstructions(t *testing.T) {
	e := testEngine(t, "zh-CN")

	if got := e.Instruction(); !strings.Contains(got, "问候") {
		t.Errorf("instruction = %q, want the Chinese text", got)
	}
}

// The bot's voice is part of its character, so it travels with the published
// flow rather than with the deployment.
func TestTheFlowCarriesTheBotsVoice(t *testing.T) {
	spec := loadTestFlow(t)
	if spec.Global.Voice != "" {
		t.Errorf("a flow that names no voice reported %q, want the provider's default",
			spec.Global.Voice)
	}

	withVoice := strings.Replace(testFlow,
		`"fallbackTarget": "handoff"`,
		`"voice": "cherry", "fallbackTarget": "handoff"`, 1)
	loaded, err := Load([]byte(withVoice))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Global.Voice != "cherry" {
		t.Errorf("global.voice = %q, want cherry", loaded.Global.Voice)
	}
}
