// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// everyShapeFlow uses every object the spec declares, each spelt correctly, so
// that one misspelt key anywhere in it is the only problem the flow has.
const everyShapeFlow = `{
  "id": "novanet_plan_upgrade",
  "specVersion": "v2",
  "entry": "plan_upgrade",
  "initialNode": "welcome",
  "global": {
    "persona": {"en": "You are NovaNet Broadband's plan assistant.", "zh": "你是 NovaNet 宽带的套餐助理。"},
    "rules": {"en": ["Quote only prices you looked up."], "zh": ["只报查到的价格。"]},
    "voice": "ballad",
    "temperature": 0.7,
    "fallbackTarget": "welcome",
    "closingTarget": "close",
    "maxTurns": 20,
    "maxTurnsWithoutTool": 6,
    "maxDurationSec": 600,
    "toolWaitLine": {"en": "One moment while I check your plan.", "zh": "请稍等，我查一下您的套餐。"},
    "alwaysAllowedTools": ["hangup"],
    "apiBaseEnv": "AICC_BOT_BACKEND_BASE",
    "transitions": [
      {"on": "NO_INPUT", "condition": {"slot": "noInput.count", "op": "GTE", "value": 3}, "target": "close", "priority": 10}
    ]
  },
  "nodes": {
    "welcome": {
      "instruction": {"en": "Ask for the account number and look the plan up.", "zh": "询问账号并查询套餐。"},
      "announce": {"en": "Welcome to NovaNet Broadband.", "zh": "欢迎致电 NovaNet 宽带。"},
      "tools": ["lookup_account", "record_plan"],
      "transitions": [
        {"on": "TOOL_RESULT", "tool": "lookup_account",
         "condition": {"all": [
           {"slot": "lookup_account.ok", "op": "EQ", "value": "1"},
           {"any": [{"slot": "plan", "op": "IN", "value": ["300M", "500M"]}, {"slot": "plan", "op": "IS_EMPTY"}]}
         ]},
         "target": "confirm_upgrade", "priority": 1}
      ]
    },
    "confirm_upgrade": {
      "instruction": "The upgrade to {slots.plan} is booked. Do not repeat it.",
      "announce": "Your plan changes to {slots.plan} on the 1st. Thank you for calling NovaNet, goodbye.",
      "isTerminal": true
    },
    "close": {
      "instruction": "Say goodbye.",
      "announce": {"en": "Thank you for calling NovaNet. Goodbye.", "zh": "感谢致电 NovaNet，再见。"},
      "isTerminal": true,
      "lead": {"intent": "LOW", "notes": {"en": "Plan {slots.plan}", "zh": "套餐 {slots.plan}"}}
    }
  },
  "tools": {
    "lookup_account": {
      "kind": "http",
      "description": {"en": "Look up the caller's broadband account.", "zh": "查询来电者的宽带账户。"},
      "parameters": {"type": "object", "properties": {"account": {"type": "string", "x-note": "a JSON Schema keyword"}}, "required": ["account"]},
      "http": {
        "baseUrl": "http://backend.novanet.test:8770",
        "path": "/api/accounts/{args.account}",
        "method": "GET",
        "query": {"view": "plan"},
        "headers": {"X-NovaNet-Channel": "voice"},
        "timeoutMs": 3000,
        "body": {"anything": {"the backend": "wants"}},
        "successWhen": {"path": "status", "equals": "OK"},
        "errorFrom": "error.code",
        "result": {"tier": "plan.tier"},
        "modelOnly": {"offers": {"path": "offers", "each": {"name": "name"}}, "status": "status"}
      },
      "collect": {"slot": "accounts.seen", "value": "{args.account}",
        "when": {"slot": "result.tier", "op": "IS_NOT_EMPTY"}}
    },
    "record_plan": {
      "kind": "slot",
      "description": "Record the plan the caller chose.",
      "slot": "plan",
      "enum": ["300M", "500M", "1000M"]
    }
  }
}`

// withKey returns everyShapeFlow with key set at the object path names (object
// keys and list indexes).
func withKey(t *testing.T, path []any, key string) []byte {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(everyShapeFlow), &doc); err != nil {
		t.Fatal(err)
	}
	at := doc
	for _, step := range path {
		switch s := step.(type) {
		case string:
			at = at.(map[string]any)[s]
		case int:
			at = at.([]any)[s]
		}
	}
	object, ok := at.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object in the fixture", path)
	}
	object[key] = "stray"
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheFixtureUsesEveryShapeAndLoads(t *testing.T) {
	t.Parallel()
	if _, err := Load([]byte(everyShapeFlow)); err != nil {
		t.Fatal(err)
	}
}

// One misspelt key at each level of the spec is the flow's only problem, and
// it names where the key is.
func TestAnUnknownFieldIsReportedWhereverItIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		path []any
		key  string
		want string
	}{
		{"the top level", nil, "comment",
			`the flow has unknown field "comment"; a flow has id, specVersion, entry, initialNode, global, nodes and tools`},
		{"global", []any{"global"}, "maxDurationSecs",
			`global has unknown field "maxDurationSecs"; did you mean "maxDurationSec"?`},
		{"global persona", []any{"global", "persona"}, "fr",
			`global.persona has unknown field "fr"; a text keyed by language has zh and en`},
		{"global rules", []any{"global", "rules"}, "fr",
			`global.rules has unknown field "fr"; a text list keyed by language has zh and en`},
		{"global toolWaitLine", []any{"global", "toolWaitLine"}, "eng",
			`global.toolWaitLine has unknown field "eng"; a text keyed by language has zh and en`},
		{"a global transition", []any{"global", "transitions", 0}, "priorty",
			`global transition 0 has unknown field "priorty"; did you mean "priority"?`},
		{"a global condition", []any{"global", "transitions", 0, "condition"}, "slott",
			`global transition 0 condition has unknown field "slott"; did you mean "slot"?`},
		{"a phase", []any{"nodes", "confirm_upgrade"}, "anounce",
			`phase "confirm_upgrade" has unknown field "anounce"; did you mean "announce"?`},
		{"a phase instruction", []any{"nodes", "welcome", "instruction"}, "en-GB",
			`phase "welcome" instruction has unknown field "en-GB"; a text keyed by language has zh and en`},
		{"a phase announce", []any{"nodes", "close", "announce"}, "ZH",
			`phase "close" announce has unknown field "ZH"; did you mean "zh"?`},
		{"a phase transition", []any{"nodes", "welcome", "transitions", 0}, "targt",
			`phase "welcome" transition 0 has unknown field "targt"; did you mean "target"?`},
		{"a condition", []any{"nodes", "welcome", "transitions", 0, "condition"}, "operator",
			`phase "welcome" transition 0 condition has unknown field "operator"; a condition has slot, op, value, all and any`},
		{"a nested condition", []any{"nodes", "welcome", "transitions", 0, "condition", "all", 1, "any", 0}, "vaule",
			`phase "welcome" transition 0 condition all 1 any 0 has unknown field "vaule"; did you mean "value"?`},
		{"a lead", []any{"nodes", "close", "lead"}, "intnet",
			`phase "close" lead has unknown field "intnet"; did you mean "intent"?`},
		{"lead notes", []any{"nodes", "close", "lead", "notes"}, "fr",
			`phase "close" lead notes has unknown field "fr"; a text keyed by language has zh and en`},
		{"a tool", []any{"tools", "lookup_account"}, "transitions",
			`tool "lookup_account" has unknown field "transitions"; a tool has kind, description, parameters, http, slot, enum and collect`},
		{"a slot tool", []any{"tools", "record_plan"}, "enums",
			`tool "record_plan" has unknown field "enums"; did you mean "enum"?`},
		{"a tool description", []any{"tools", "lookup_account", "description"}, "fr",
			`tool "lookup_account" description has unknown field "fr"; a text keyed by language has zh and en`},
		{"http", []any{"tools", "lookup_account", "http"}, "timeout",
			`tool "lookup_account" http has unknown field "timeout"; did you mean "timeoutMs"?`},
		{"successWhen", []any{"tools", "lookup_account", "http", "successWhen"}, "equal",
			`tool "lookup_account" http successWhen has unknown field "equal"; did you mean "equals"?`},
		{"a modelOnly projection", []any{"tools", "lookup_account", "http", "modelOnly", "offers"}, "limit",
			`tool "lookup_account" http modelOnly "offers" has unknown field "limit"; a projection has path and each`},
		{"collect", []any{"tools", "lookup_account", "collect"}, "values",
			`tool "lookup_account" collect has unknown field "values"; did you mean "value"?`},
		{"collect when", []any{"tools", "lookup_account", "collect", "when"}, "aly",
			`tool "lookup_account" collect when has unknown field "aly"; a condition has slot, op, value, all and any`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(withKey(t, tc.path, tc.key))
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want a ValidationError", err)
			}
			if len(invalid.Problems) != 1 || invalid.Problems[0] != tc.want {
				t.Errorf("problems = %q\nwant exactly %q", invalid.Problems, tc.want)
			}
			if invalid.FlowID != "novanet_plan_upgrade" {
				t.Errorf("FlowID = %q", invalid.FlowID)
			}
		})
	}
}

// The keys of a map are names the author chooses, and a tool's parameters are
// a JSON Schema passed to the model as written: none of them is a field.
func TestAuthorNamedKeysAreNotFields(t *testing.T) {
	t.Parallel()
	for _, path := range [][]any{
		{"tools", "lookup_account", "parameters"},
		{"tools", "lookup_account", "parameters", "properties"},
		{"tools", "lookup_account", "http", "query"},
		{"tools", "lookup_account", "http", "headers"},
		{"tools", "lookup_account", "http", "body"},
		{"tools", "lookup_account", "http", "result"},
		{"tools", "lookup_account", "http", "modelOnly", "offers", "each"},
	} {
		if _, err := Load(withKey(t, path, "x-comment")); err != nil {
			t.Errorf("%v: %v", path, err)
		}
	}
}

// Every unknown key is reported at once, with the flow's other problems, so an
// author fixes the document in one pass.
func TestUnknownFieldsAreReportedTogetherWithTheOtherProblems(t *testing.T) {
	t.Parallel()
	doc := strings.NewReplacer(
		`"initialNode": "welcome",`, `"intialNode": "welcome",`,
		`"maxDurationSec": 600,`, `"maxDurationSecs": 600,`,
		`"isTerminal": true,
      "lead"`, `"isterminal": true,
      "lead"`,
		`"enum": [`, `"enums": [`,
	).Replace(everyShapeFlow)
	_, err := Load([]byte(doc))
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	want := []string{
		`the flow has unknown field "intialNode"; did you mean "initialNode"?`,
		`global has unknown field "maxDurationSecs"; did you mean "maxDurationSec"?`,
		`phase "close" has unknown field "isterminal"; did you mean "isTerminal"?`,
		`tool "record_plan" has unknown field "enums"; did you mean "enum"?`,
		"initialNode is not set",
	}
	for _, w := range want {
		if !slices.Contains(invalid.Problems, w) {
			t.Errorf("problems lack %q:\n%s", w, strings.Join(invalid.Problems, "\n"))
		}
	}
	if len(invalid.Problems) != len(want) {
		t.Errorf("problems = %d, want %d:\n%s", len(invalid.Problems), len(want), strings.Join(invalid.Problems, "\n"))
	}
	// The unknown fields come first: they are often why the rest happened.
	if !strings.Contains(invalid.Problems[0], "unknown field") || !strings.Contains(invalid.Problems[len(want)-1], "initialNode is not set") {
		t.Errorf("problems out of order:\n%s", strings.Join(invalid.Problems, "\n"))
	}
}

// A near miss gets a suggestion; a key that is close to nothing, or close to
// two fields at once, gets none.
func TestOnlyANearMissOfOneFieldIsSuggested(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key     string
		known   []string
		suggest string
	}{
		{"anounce", declaredOrderOf[Node](), "announce"},
		{"isterminal", declaredOrderOf[Node](), "isTerminal"},
		{"terminal", declaredOrderOf[Node](), "isTerminal"},
		{"isTemrinal", declaredOrderOf[Node](), "isTerminal"},
		{"maxDurationSecs", declaredOrderOf[Global](), "maxDurationSec"},
		{"enums", declaredOrderOf[Tool](), "enum"},
		{"priorty", declaredOrderOf[Transition](), "priority"},
		{"foo", declaredOrderOf[Node](), ""},
		{"foo", declaredOrderOf[Spec](), ""},
		{"foo", declaredOrderOf[Transition](), ""},
		{"foo", declaredOrderOf[Global](), ""},
		{"x-comment", declaredOrderOf[Node](), ""},
		{"x-comment", declaredOrderOf[Spec](), ""},
		{"x-comment", declaredOrderOf[Global](), ""},
		// "aly" is one edit from both "all" and "any".
		{"aly", declaredOrderOf[Condition](), ""},
		// Two letters have none to spare.
		{"cn", []string{"zh", "en"}, ""},
	} {
		if got := suggestField(tc.key, tc.known); got != tc.suggest {
			t.Errorf("suggestField(%q) = %q, want %q", tc.key, got, tc.suggest)
		}
	}
}

func declaredOrderOf[T any]() []string {
	return declaredOrder(reflect.TypeFor[T]())
}

// A stored revision is read tolerantly: the unknown field comes back for the
// caller to log, and the flow loads. Its other problems still refuse it.
func TestLoadTolerantReturnsTheUnknownFieldsAndTheFlow(t *testing.T) {
	t.Parallel()
	doc := withKey(t, []any{"nodes", "confirm_upgrade"}, "anounce")
	spec, unknown, err := LoadTolerant(doc)
	if err != nil {
		t.Fatalf("tolerant load: %v", err)
	}
	if spec == nil || spec.ID != "novanet_plan_upgrade" {
		t.Fatalf("spec = %+v", spec)
	}
	if len(unknown) != 1 || unknown[0].Where != `phase "confirm_upgrade"` || unknown[0].Field != "anounce" ||
		unknown[0].Suggestion != "announce" {
		t.Errorf("unknown = %+v", unknown)
	}
	if _, err := Load(doc); err == nil {
		t.Error("the strict load accepted the unknown field")
	}

	broken := strings.Replace(string(doc), `"initialNode":"welcome"`, `"initialNode":"nowhere"`, 1)
	if _, _, err := LoadTolerant([]byte(broken)); err == nil || !strings.Contains(err.Error(), `initialNode "nowhere"`) {
		t.Errorf("tolerant load of a broken flow = %v, want its problem", err)
	}
}

// The walker reads a struct's json tags as its vocabulary. A type that reads
// itself (UnmarshalJSON) keeps that true only if its tags are its keys, as
// DeclaredLead's are, or if objectShapes names the struct its object form
// decodes into. A new such type that does neither would have its Go field
// names taken for its keys, and every correct document refused.
func TestEveryTypeThatReadsItselfDeclaresItsKeys(t *testing.T) {
	t.Parallel()
	unmarshaler := reflect.TypeFor[json.Unmarshaler]()
	seen := map[reflect.Type]bool{}
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		if shape, ok := objectShapes[typ]; ok {
			visit(shape)
			return
		}
		readsItself := reflect.PointerTo(typ).Implements(unmarshaler)
		for _, field := range reflect.VisibleFields(typ) {
			if !field.IsExported() {
				continue
			}
			if readsItself && field.Tag.Get("json") == "" {
				t.Errorf("%s reads itself, and its field %s has no json tag; tag it or add the type to objectShapes",
					typ, field.Name)
			}
			visit(field.Type)
		}
	}
	visit(reflect.TypeFor[Spec]())
	for _, typ := range []reflect.Type{reflect.TypeFor[DeclaredLead](), reflect.TypeFor[Text](), reflect.TypeFor[ModelOnlyField]()} {
		if !seen[typ] {
			t.Errorf("%s is not reachable from Spec; the walk above misses types", typ)
		}
	}
}
