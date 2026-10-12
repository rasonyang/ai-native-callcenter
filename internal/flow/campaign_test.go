// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// flowJSON builds a loadable flow whose phases and global section carry the
// pieces a test varies. nodeTools is the allowlist of the "ask" phase; always
// is global.alwaysAllowedTools.
func flowJSON(t *testing.T, nodeTools, always []string, extra func(m map[string]any)) *Spec {
	t.Helper()
	if nodeTools == nil {
		nodeTools = []string{}
	}
	if always == nil {
		always = []string{}
	}
	m := map[string]any{
		"id": "t", "specVersion": "v2", "initialNode": "ask",
		"global": map[string]any{
			"persona":            "A polite caller.",
			"alwaysAllowedTools": always,
		},
		"nodes": map[string]any{
			"ask": map[string]any{
				"instruction": "Ask the customer how they are.",
				"tools":       nodeTools,
			},
			"bye": map[string]any{"instruction": "Say goodbye.", "isTerminal": true},
		},
	}
	if extra != nil {
		extra(m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := Load(raw)
	if err != nil {
		t.Fatalf("the test flow does not load: %v\n%s", err, raw)
	}
	return spec
}

func TestCanTransfer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		nodeTools []string
		always    []string
		extra     func(m map[string]any)
		want      bool
	}{
		{name: "no tools at all", want: false},
		{name: "hangup only", nodeTools: []string{"hangup"}, always: []string{"hangup"}, want: false},
		{name: "take_message is not a way to a person", nodeTools: []string{"take_message"}, always: []string{"take_message", "hangup"}, want: false},
		{name: "a phase allows it by name", nodeTools: []string{"hangup", "transfer_to_agent"}, want: true},
		{name: "a phase allows everything", nodeTools: []string{"*"}, want: true},
		{name: "every phase has it", always: []string{"transfer_to_agent"}, want: true},
		{
			name: "only an unreachable terminal phase offers it", nodeTools: []string{"hangup"},
			extra: func(m map[string]any) {
				m["nodes"].(map[string]any)["bye"].(map[string]any)["tools"] = []string{"transfer_to_agent"}
			},
			want: true,
		},
		{
			// The session-limit hand-over and the fallback-queue rescue are the
			// call's, not the flow's: a flow with a time budget is not a
			// transferring flow.
			name: "a time budget is not a transfer", nodeTools: []string{"hangup"},
			extra: func(m map[string]any) {
				m["global"].(map[string]any)["maxDurationSec"] = 600
			},
			want: false,
		},
		{
			name: "a flow tool named like a transfer does not count", nodeTools: []string{"lookup_transfer"},
			extra: func(m map[string]any) {
				m["tools"] = map[string]any{"lookup_transfer": map[string]any{
					"description": "Look up a transfer.", "http": map[string]any{"path": "/x"},
				}}
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CanTransfer(flowJSON(t, tc.nodeTools, tc.always, tc.extra)); got != tc.want {
				t.Errorf("CanTransfer = %v, want %v", got, tc.want)
			}
		})
	}
	if CanTransfer(nil) {
		t.Error("a nil spec cannot transfer")
	}
}

// Every reference flow is a support or collections flow that hands over to a
// person, so none of them can back a campaign. The table is the fact, and a
// seed flow written for campaigns will change its row.
func TestCanTransferOnTheSeedFlows(t *testing.T) {
	t.Parallel()
	want := map[string]bool{
		"early_collections.json":         true,
		"field_service_appointment.json": true,
		"lead_qualification.json":        true,
		"mobile_support.json":            true,
		"novanet_support.json":           true,
		"plan_change.json":               true,
	}
	files, err := filepath.Glob("../seed/flows/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no seed flows: %v", err)
	}
	for _, file := range files {
		name := filepath.Base(file)
		expected, known := want[name]
		if !known {
			t.Errorf("%s is a new seed flow: add its row to this table", name)
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := Load(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := CanTransfer(spec); got != expected {
			t.Errorf("%s: CanTransfer = %v, want %v", name, got, expected)
		}

		// Take transfer_to_agent out of every allowlist, as an author making
		// the flow campaign-safe would, and the answer must flip.
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		strip := func(list any) any {
			var out []any
			for _, v := range list.([]any) {
				if v != "transfer_to_agent" {
					out = append(out, v)
				}
			}
			if out == nil {
				out = []any{}
			}
			return out
		}
		global := doc["global"].(map[string]any)
		if v, ok := global["alwaysAllowedTools"]; ok {
			global["alwaysAllowedTools"] = strip(v)
		}
		for _, node := range doc["nodes"].(map[string]any) {
			n := node.(map[string]any)
			if v, ok := n["tools"]; ok {
				n["tools"] = strip(v)
			}
		}
		stripped, _ := json.Marshal(doc)
		sspec, err := Load(stripped)
		if err != nil {
			t.Fatalf("%s without transfer does not load: %v", name, err)
		}
		hasStar := strings.Contains(string(stripped), `"*"`)
		if got := CanTransfer(sspec); got != hasStar {
			t.Errorf("%s without transfer_to_agent: CanTransfer = %v, want %v (only a \"*\" allowlist should still say yes)", name, got, hasStar)
		}
	}
}

func TestUserDataNames(t *testing.T) {
	t.Parallel()
	spec := flowJSON(t, []string{"hangup", "lookup"}, nil, func(m map[string]any) {
		nodes := m["nodes"].(map[string]any)
		ask := nodes["ask"].(map[string]any)
		ask["instruction"] = map[string]any{
			"en": "Greet {slots.userData.name} about the {slots.userData.plan} plan.",
			"zh": "向{slots.userData.name}介绍{slots.userData.city}的套餐。",
		}
		ask["announce"] = "Hello {slots.userData.name}"
		ask["transitions"] = []any{map[string]any{
			"on": "TOOL_RESULT", "target": "bye",
			"condition": map[string]any{"all": []any{
				map[string]any{"slot": "userData.tier", "op": "EQ", "value": "gold"},
				map[string]any{"any": []any{map[string]any{"slot": "userData.region", "op": "IS_NOT_EMPTY"}}},
			}},
		}}
		m["global"].(map[string]any)["transitions"] = []any{map[string]any{
			"target": "bye", "condition": map[string]any{"slot": "userData.vip", "op": "EQ", "value": "1"},
		}}
		m["tools"] = map[string]any{"lookup": map[string]any{
			"description": "Look the customer up.",
			"http": map[string]any{"path": "/lookup", "body": map[string]any{
				"account": "{slots.userData.account}",
				"nested":  map[string]any{"list": []any{"{slots.userData.deep}", "{args.q}", "{slots.caller}"}},
			}},
		}}
	})
	got := UserDataNames(spec)
	want := []string{"account", "city", "deep", "name", "plan", "region", "tier", "vip"}
	if !slices.Equal(got, want) {
		t.Errorf("UserDataNames = %v, want %v", got, want)
	}
	if names := UserDataNames(flowJSON(t, nil, nil, nil)); len(names) != 0 {
		t.Errorf("a flow that reads no business data declares none, got %v", names)
	}
	if names := UserDataNames(nil); names != nil {
		t.Errorf("a nil spec declares none, got %v", names)
	}
}
