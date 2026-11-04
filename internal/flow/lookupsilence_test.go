// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// twoLookupsFlow is factsFlow with a second HTTP tool, offered in "ask" only.
var twoLookupsFlow = strings.Replace(strings.Replace(factsFlow,
	`"tools": ["note_budget", "note_plan", "search"]`,
	`"tools": ["note_budget", "note_plan", "search", "check_stock"]`, 1),
	`"search": {`,
	`"check_stock": {
      "description": "d",
      "parameters": {"type": "object", "properties": {"q": {"type": "string"}}},
      "http": {"path": "/c", "body": {"q": "{args.q}"}}
    },
    "search": {`, 1)

func lookupRuntime(t *testing.T, flowJSON, lang string) *Runtime {
	t.Helper()
	spec, err := Load([]byte(flowJSON))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRuntime(NewEngine(spec, lang, nil, log), &fakeActions{}, NewBackend(""), nil, log)
}

// The standing instructions of a phase that allows an HTTP tool tell the model
// to say nothing in the reply that calls it, naming the tools, after the
// action-claim rule and ahead of the persona (#466).
func TestAPhaseWithAnHTTPToolTellsTheModelToSayNothingWhenCallingIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, flow, lang, want string
	}{
		{"one tool zh", factsFlow, LangZH,
			"调用 search 的那次回复只调用工具，不要说任何话，也不要说“好的，我查一下”这类话：" +
				"查询慢时系统会自己请来电者稍等。工具结果回来后，直接把结果告诉来电者。"},
		{"one tool en", factsFlow, LangEN,
			"In a reply that calls search, only call the tool and say nothing, not even " +
				"\"One moment, let me check.\" If the lookup is slow, the system asks the caller to hold on. " +
				"When the result comes back, tell the caller the result directly."},
		{"two tools zh", twoLookupsFlow, LangZH, "调用 check_stock或search 的那次回复只调用工具"},
		{"two tools en", twoLookupsFlow, LangEN, "In a reply that calls check_stock or search, only call the tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := lookupRuntime(t, tc.flow, tc.lang)
			got := r.Instructions()
			if !strings.Contains(got, "\n\n"+tc.want) {
				t.Fatalf("instructions lack the rule %q:\n%s", tc.want, got)
			}
			claim, persona := strings.Index(got, r.actionClaimRule()), strings.Index(got, "\n\n"+tc.want)
			if claim < 0 || persona < claim || got[claim+len(r.actionClaimRule()):][:2] != "\n\n" {
				t.Errorf("the rule must follow the action-claim rule after one blank line:\n%s", got)
			}
			if strings.Count(got, "只调用工具")+strings.Count(got, "only call the tool") != 1 {
				t.Errorf("the rule appears more than once:\n%s", got)
			}
		})
	}
}

// A phase with no HTTP tool (slot tools and built-ins do not count) gets no
// rule, in either language.
func TestAPhaseWithoutAnHTTPToolHasNoSilenceRule(t *testing.T) {
	t.Parallel()
	slotOnly := strings.Replace(factsFlow, `"tools": ["note_budget", "note_plan", "search"]`,
		`"tools": ["note_budget", "note_plan"]`, 1)
	for _, lang := range []string{LangZH, LangEN} {
		for name, flowJSON := range map[string]string{"slot tools only": slotOnly, "no tools": factsFlow} {
			r := lookupRuntime(t, flowJSON, lang)
			if name == "no tools" {
				if !r.ReturnTo("done") {
					t.Fatal("cannot return to done")
				}
			}
			got := r.Instructions()
			if strings.Contains(got, "只调用工具") || strings.Contains(got, "only call the tool") {
				t.Errorf("%s/%s: the rule is present:\n%s", name, lang, got)
			}
		}
	}
}

// The action-claim rule's example no longer endorses a filler with the call.
func TestTheActionClaimExampleDoesNotEndorseAFiller(t *testing.T) {
	t.Parallel()
	zh := lookupRuntime(t, factsFlow, LangZH).actionClaimRule()
	if !strings.HasSuffix(zh, "例如：没有调用查询工具，就不要说“我帮您查询一下”。") {
		t.Errorf("zh rule = %q", zh)
	}
	en := lookupRuntime(t, factsFlow, LangEN).actionClaimRule()
	if !strings.HasSuffix(en, "For example, if you are not calling the lookup tool, do not say \"Let me check that order for you.\"") {
		t.Errorf("en rule = %q", en)
	}
}

// The rule follows the phase: a phase change that alters the HTTP tool set
// changes the rendered instructions.
func TestTheSilenceRuleFollowsThePhase(t *testing.T) {
	t.Parallel()
	r := lookupRuntime(t, factsFlow, LangEN)
	before := r.Instructions()
	if !strings.Contains(before, "calls search,") {
		t.Fatalf("the ask phase lacks the rule:\n%s", before)
	}
	if !r.ReturnTo("done") {
		t.Fatal("cannot return to done")
	}
	after := r.Instructions()
	if strings.Contains(after, "only call the tool") || after == before {
		t.Errorf("the done phase still carries the rule:\n%s", after)
	}
}
