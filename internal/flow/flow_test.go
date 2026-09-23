// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"errors"
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
			"announce": {"en": "Thanks for calling NovaNet billing.", "zh": "感谢致电 NovaNet 账务热线。"},
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
			"announce": "Here is the balance for {slots.lookup_account.name}.",
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
		{"closing target names no phase", func(s string) string {
			return strings.Replace(s, `"fallbackTarget": "handoff",`,
				`"fallbackTarget": "handoff", "closingTarget": "nowhere",`, 1)
		}, "closingTarget"},
		{"closing target is not terminal", func(s string) string {
			return strings.Replace(s, `"fallbackTarget": "handoff",`,
				`"fallbackTarget": "handoff", "closingTarget": "report",`, 1)
		}, "terminal phase"},
		{"turns-without-tool wall with no closing target", func(s string) string {
			return strings.Replace(s, `"maxTurns": 6,`,
				`"maxTurns": 6, "maxTurnsWithoutTool": 3,`, 1)
		}, "maxTurnsWithoutTool"},
		{"negative turns-without-tool wall", func(s string) string {
			return strings.Replace(s, `"maxTurns": 6,`,
				`"maxTurns": 6, "maxTurnsWithoutTool": -1,`, 1)
		}, "maxTurnsWithoutTool is -1"},
		{"NO_INPUT rule that names a tool", func(s string) string {
			return strings.Replace(s, `{"on": "NO_INPUT",`,
				`{"on": "NO_INPUT", "tool": "lookup_account",`, 1)
		}, "a silence never carries"},
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

//
// The NO_INPUT default, consecutive silence, and the turns-without-a-tool
// wall (issue #9: a caller's decline never ended the call, only the model
// calling hangup did).
//

// testFlowWithClosing adds global.closingTarget to testFlow, pointed at the
// one terminal phase the fixture already has.
var testFlowWithClosing = strings.Replace(testFlow, `"fallbackTarget": "handoff",`,
	`"fallbackTarget": "handoff", "closingTarget": "farewell",`, 1)

// testFlowWithWall adds both closingTarget and a three-reply
// maxTurnsWithoutTool wall.
var testFlowWithWall = strings.Replace(testFlowWithClosing, `"maxTurns": 6,`,
	`"maxTurns": 6, "maxTurnsWithoutTool": 3,`, 1)

// welcomeNoInputRule is the fixture's own NO_INPUT rule in welcome, for tests
// that rewrite it.
const welcomeNoInputRule = `{"on": "NO_INPUT",
				 "condition": {"slot": "noInput.count", "op": "GTE", "value": 2},
				 "target": "farewell"}`

func testEngineFor(t *testing.T, spec string, lang string) *Engine {
	t.Helper()
	loaded, err := Load([]byte(spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return NewEngine(loaded, lang, map[string]any{"caller": "13800138000"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// withWelcomeNoInputRule swaps welcome's NO_INPUT rule for another.
func withWelcomeNoInputRule(t *testing.T, spec, rule string) string {
	t.Helper()
	if !strings.Contains(spec, welcomeNoInputRule) {
		t.Fatal("the fixture no longer carries the NO_INPUT rule this test rewrites")
	}
	return strings.Replace(spec, welcomeNoInputRule, rule, 1)
}

// toReport moves an engine on the fixture to "report", the phase that declares
// nothing about silence.
func toReport(t *testing.T, e *Engine) {
	t.Helper()
	if moved := e.OnToolResult("lookup_account",
		map[string]any{"found": "1", "name": "Alice"}); moved != "report" {
		t.Fatalf("did not reach the phase under test: moved to %q", moved)
	}
}

// silences reports n silences and fails if any of them moves the call.
func silences(t *testing.T, e *Engine, n int) {
	t.Helper()
	for i := range n {
		if moved := e.OnNoInput(); moved != "" {
			t.Fatalf("silence %d moved to %q", i+1, moved)
		}
	}
}

// The "report" phase declares no NO_INPUT rule of its own, unlike "welcome":
// with a closing target named, silence there must still end the call rather
// than re-prompt forever, and it does so on the same count novanet_support's
// own rule already used.
func TestNoInputDefaultFiresAfterRepeatedUndeclaredSilence(t *testing.T) {
	e := testEngineFor(t, testFlowWithClosing, "en")
	toReport(t, e)

	silences(t, e, defaultNoInputLimit-1)
	if moved := e.OnNoInput(); moved != "farewell" {
		t.Fatalf("silence %d moved to %q, want the closing target", defaultNoInputLimit, moved)
	}
}

// A phase whose own NO_INPUT rule has not tripped yet keeps exactly that rule:
// the default must not fire at its own count ahead of it.
func TestNoInputDefaultDoesNotOverrideAnAuthoredRule(t *testing.T) {
	e := testEngineFor(t, withWelcomeNoInputRule(t, testFlowWithClosing,
		`{"on": "NO_INPUT", "condition": {"slot": "noInput.count", "op": "GTE", "value": 5},
		  "target": "handoff"}`), "en")

	// Past the default's limit: had the authored rule been overlooked, the
	// default would have closed the call here.
	silences(t, e, defaultNoInputLimit+1)
	if moved := e.OnNoInput(); moved != "handoff" {
		t.Fatalf("silence 5 moved to %q, want the authored rule's target", moved)
	}
}

// A rule with no `on` matches every event, NO_INPUT included — fire() would
// consider it for a silence, so it is the author's word on silence here and
// the default stands aside.
func TestNoInputDefaultStandsAsideForARuleThatMatchesEveryEvent(t *testing.T) {
	e := testEngineFor(t, withWelcomeNoInputRule(t, testFlowWithClosing,
		`{"condition": {"slot": "noInput.count", "op": "GTE", "value": 5},
		  "target": "handoff"}`), "en")

	silences(t, e, defaultNoInputLimit+1)
	if moved := e.OnNoInput(); moved != "handoff" {
		t.Fatalf("silence 5 moved to %q, want the authored rule's target", moved)
	}
}

// A rule that names a tool can never fire on a silence, which carries none; it
// says nothing about silence and must not switch the default off.
func TestNoInputDefaultIgnoresARuleThatCannotFireOnSilence(t *testing.T) {
	spec := strings.Replace(testFlowWithClosing, `"transitions": [
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",`, `"transitions": [
			{"tool": "take_message", "target": "farewell"},
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",`, 1)
	if spec == testFlowWithClosing {
		t.Fatal("the fixture no longer has the global transitions this test extends")
	}
	e := testEngineFor(t, spec, "en")
	toReport(t, e)

	silences(t, e, defaultNoInputLimit-1)
	if moved := e.OnNoInput(); moved != "farewell" {
		t.Fatalf("silence %d moved to %q, want the default's closing target", defaultNoInputLimit, moved)
	}
}

// Without a closing target there is nowhere to send the call, so a phase that
// declared nothing about silence keeps re-prompting — the behaviour before
// this feature existed, not a new failure mode.
func TestNoInputDefaultDoesNothingWithoutAClosingTarget(t *testing.T) {
	e := testEngine(t, "en") // plain testFlow: no closingTarget
	toReport(t, e)
	silences(t, e, defaultNoInputLimit+2)
}

// noInput.count is consecutive, not cumulative: any sign of the caller making
// progress — words, a keypress, a tool the conversation led to — resets it.
// A final transcript with nothing in it is not progress: on a noisy abandoned
// line it would otherwise keep the count from ever reaching a rule.
func TestConsecutiveSilenceResetsOnCallerProgressOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		progress  func(e *Engine)
		wantReset bool
	}{
		{"words", func(e *Engine) { e.OnCallerSpoke("hello?") }, true},
		{"a keypress", func(e *Engine) { e.OnCallerKeyed() }, true},
		{"a tool result", func(e *Engine) {
			e.OnToolResult("take_message", map[string]any{"ok": "1"})
		}, true},
		{"an empty transcript", func(e *Engine) { e.OnCallerSpoke("") }, false},
		{"a blank transcript", func(e *Engine) { e.OnCallerSpoke("  ") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := testEngineFor(t, testFlowWithClosing, "en")
			toReport(t, e)

			silences(t, e, defaultNoInputLimit-1)
			tc.progress(e)
			moved := e.OnNoInput()
			switch {
			case tc.wantReset && moved != "":
				t.Fatalf("one silence after %s moved to %q; the count should have reset", tc.name, moved)
			case !tc.wantReset && moved != "farewell":
				t.Fatalf("silence after %s moved to %q; it is not progress and the count should have reached the default", tc.name, moved)
			}
		})
	}
}

// reply is one exchange the wall counts: the caller says something, and the
// model finishes a reply to it without calling a tool.
func reply(e *Engine) bool {
	e.OnCallerSpoke("no")
	return e.OnBotTurnDone(false, false)
}

// The wall guards a tool-less loop: a caller who keeps answering — "anything
// else?", "no" — without the model ever dispatching a tool reaches the closing
// target once the replies EXCEED the wall, and only when asked to close, which
// the orchestrator does once the reply that crossed it has been heard.
func TestTheTurnsWithoutToolWallClosesTheCallOnceExceeded(t *testing.T) {
	e := testEngineFor(t, testFlowWithWall, "en") // maxTurnsWithoutTool = 3

	for i := range 3 {
		if reply(e) {
			t.Fatalf("reply %d crossed a wall of 3", i+1)
		}
		if moved := e.CloseAtTurnsWithoutToolWall(); moved != "" {
			t.Fatalf("closing after reply %d moved to %q", i+1, moved)
		}
	}
	if !reply(e) {
		t.Fatal("the fourth tool-less reply did not cross a wall of 3")
	}
	if e.IsTerminal() {
		t.Fatal("crossing the wall moved the call by itself; the move waits for the reply to be heard")
	}
	if got, _ := e.Slot("turnsWithoutTool"); got != 4 {
		t.Errorf("turnsWithoutTool = %v, want 4", got)
	}
	if moved := e.CloseAtTurnsWithoutToolWall(); moved != "farewell" {
		t.Fatalf("closing moved to %q, want the closing target", moved)
	}
	if !e.IsTerminal() {
		t.Error("closing target was not entered as terminal")
	}
	if e.IsPastTurnsWithoutToolWall() {
		t.Error("a terminal phase still reports standing past the wall")
	}
}

// Only a completed reply to caller speech counts. The opening turn, a dead-air
// re-prompt and a phase's own line have no caller behind them; a turn cut short
// replied to nobody; an empty transcript is not speech.
func TestTheTurnsWithoutToolWallCountsOnlyRepliesToTheCaller(t *testing.T) {
	e := testEngineFor(t, testFlowWithWall, "en")

	for range 10 {
		e.OnBotTurnDone(false, false) // no caller utterance behind it
	}
	e.OnCallerSpoke("")
	e.OnBotTurnDone(false, false)
	e.OnCallerSpoke("no")
	e.OnBotTurnDone(false, true) // cut short: the caller is still owed a reply
	if got, _ := e.Slot("turnsWithoutTool"); got != 0 {
		t.Fatalf("turnsWithoutTool = %v after no counted reply, want 0", got)
	}
	e.OnBotTurnDone(false, false) // the reply the utterance above was owed
	if got, _ := e.Slot("turnsWithoutTool"); got != 1 {
		t.Fatalf("turnsWithoutTool = %v, want 1", got)
	}
	// Two finals for one utterance (gemini) still earn one reply.
	e.OnCallerSpoke("no")
	e.OnCallerSpoke("no thanks")
	e.OnBotTurnDone(false, false)
	e.OnBotTurnDone(false, false)
	if got, _ := e.Slot("turnsWithoutTool"); got != 2 {
		t.Fatalf("turnsWithoutTool = %v, want 2", got)
	}
}

// A turn that makes a tool call resets the count, and the turn after it
// answers the tool's result rather than the caller — even when the caller's
// transcript lands after the tool call, as it can on the Realtime clients.
func TestTheTurnsWithoutToolWallResetsOnAToolCall(t *testing.T) {
	e := testEngineFor(t, testFlowWithWall, "en")
	reply(e)
	reply(e)
	reply(e)

	// Transcript first (gemini, doubao): the utterance leads to a tool call.
	e.OnCallerSpoke("check my balance")
	e.OnToolResult("take_message", map[string]any{"ok": "1"}) // matches no rule in welcome
	if e.OnBotTurnDone(true, false) {
		t.Fatal("the tool-call turn reported the wall crossed")
	}
	if e.OnBotTurnDone(false, false) {
		t.Fatal("the tool's answer reported the wall crossed")
	}
	if got, _ := e.Slot("turnsWithoutTool"); got != 0 {
		t.Fatalf("turnsWithoutTool = %v after a tool call, want 0", got)
	}

	// Transcript last (openai): it arrives after the tool-call turn and must
	// not be charged to the tool's answer.
	e.OnToolResult("take_message", map[string]any{"ok": "1"})
	e.OnBotTurnDone(true, false)
	e.OnCallerSpoke("check my balance")
	e.OnBotTurnDone(false, false)
	if got, _ := e.Slot("turnsWithoutTool"); got != 0 {
		t.Fatalf("turnsWithoutTool = %v after a late transcript, want 0", got)
	}

	for i := range 3 {
		if reply(e) {
			t.Fatalf("reply %d after the reset crossed the wall", i+1)
		}
	}
	if !reply(e) {
		t.Fatal("the fourth reply after the reset did not cross the wall")
	}
}

// A phase change is progress too: the wall bounds replies within one phase.
func TestTheTurnsWithoutToolWallResetsOnAPhaseChange(t *testing.T) {
	e := testEngineFor(t, testFlowWithWall, "en")
	reply(e)
	reply(e)
	reply(e)
	if moved := e.enter("report"); moved != "report" {
		t.Fatalf("entered %q", moved)
	}
	if got, _ := e.Slot("turnsWithoutTool"); got != 0 {
		t.Fatalf("turnsWithoutTool = %v after a phase change, want 0", got)
	}
	if reply(e) {
		t.Fatal("the first reply in a new phase crossed the wall")
	}
}

// Zero (the field's absence) is off: a flow that never opted in must never
// have its calls ended by a wall it did not set.
func TestTheTurnsWithoutToolWallIsOffByDefault(t *testing.T) {
	e := testEngine(t, "en") // plain testFlow: maxTurnsWithoutTool is 0
	for i := range 20 {
		if reply(e) {
			t.Fatalf("reply %d crossed a wall that is not set", i+1)
		}
	}
	if moved := e.CloseAtTurnsWithoutToolWall(); moved != "" {
		t.Fatalf("closing moved to %q with the wall unset", moved)
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

//
// Announcements.
//

// A phase's announce is a line the bot says, not a brief it works from: it is
// taken from the flow word for word, in the call's own language.
func TestAPhaseAnnouncementIsTakenVerbatimInTheCallsLanguage(t *testing.T) {
	english := testEngine(t, "en")
	if got := english.Announce(); got != "Thanks for calling NovaNet billing." {
		t.Errorf("announce = %q, want the English line unchanged", got)
	}

	chinese := testEngine(t, "zh-CN")
	if got := chinese.Announce(); got != "感谢致电 NovaNet 账务热线。" {
		t.Errorf("announce = %q, want the Chinese line", got)
	}
}

// The same substitution the instruction gets: a line naming the caller is
// worth nothing if it reaches them as a template. The bare-string form is the
// other half of this — one wording in every language, as Text already allows.
func TestAnAnnouncementRendersSlotsAndAcceptsOneWordingForEveryLanguage(t *testing.T) {
	e := testEngine(t, "en")
	if moved := e.OnToolResult("lookup_account",
		map[string]any{"found": "1", "name": "Alice"}); moved != "report" {
		t.Fatalf("moved to %q, want report", moved)
	}

	if got := e.Announce(); got != "Here is the balance for Alice." {
		t.Errorf("announce = %q, want the collected name substituted", got)
	}
	if got := testEngine(t, "zh").Spec().Nodes["report"].Announce.For(LangZH); got == "" {
		t.Error("a bare-string announce did not reach the Chinese side")
	}
}

// Most phases say nothing of their own, and the field is optional: a phase
// with no announce must report emptiness rather than something to speak.
func TestAPhaseWithNoAnnouncementHasNothingToSay(t *testing.T) {
	e := testEngine(t, "en")
	if moved := e.OnNoInput(); moved != "" {
		t.Fatalf("one silence moved to %q", moved)
	}
	if moved := e.OnNoInput(); moved != "farewell" {
		t.Fatalf("two silences moved to %q, want farewell", moved)
	}
	if got := e.Announce(); got != "" {
		t.Errorf("announce = %q, want nothing", got)
	}
}

// A phase the conversation does not leave has one job: say the closing words.
// Where the engine that answers cannot be prompted into a turn by text, a
// terminal phase with no line of its own says nothing at all — the caller
// hears the bot stop mid-call — so the flow is refused before it can be
// published rather than discovered on a call.
func TestTerminalPhasesMustCarryALineWhereTheProviderCannotBeCued(t *testing.T) {
	spec := loadTestFlow(t)

	err := RequireTerminalAnnounce(spec)
	if err == nil {
		t.Fatal("a flow whose terminal phase says nothing of its own was accepted")
	}
	var missing *MissingAnnounceError
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T, want one a handler can answer with its own code", err)
	}
	if len(missing.Nodes) != 1 || missing.Nodes[0] != "farewell" {
		t.Errorf("nodes = %v, want exactly the terminal phase at fault", missing.Nodes)
	}
	if !strings.Contains(err.Error(), "farewell") {
		t.Errorf("error %q does not name the phase", err)
	}

	// The phases a conversation passes through are the model's to speak for;
	// only the ones it cannot leave are at issue.
	withLine := strings.Replace(testFlow,
		`"instruction": {"en": "Say goodbye.", "zh": "道别。"},`,
		`"instruction": {"en": "Say goodbye.", "zh": "道别。"},
			"announce": {"en": "Goodbye.", "zh": "再见。"},`, 1)
	fixed, err := Load([]byte(withLine))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := RequireTerminalAnnounce(fixed); err != nil {
		t.Errorf("a flow whose terminal phase carries its line was refused: %v", err)
	}
}

// The rule is the deployment's, not the dialect's: the same flow is perfectly
// valid on an engine that can be asked to greet, and loading must not depend
// on which one this installation runs.
func TestLoadingDoesNotApplyTheDeploymentsOwnRule(t *testing.T) {
	if _, err := Load([]byte(testFlow)); err != nil {
		t.Errorf("a flow with no terminal line failed to load: %v", err)
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
