// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
)

// #484: hangup takes a required reason. A hangup without one, or with one
// outside HangupReasons, is refused as a conversation and runs nothing; a
// hangup over an ending already under way, or in a terminal phase, is not
// judged.

// reasonFlow hangs up from talk into a terminal goodbye on any hangup result,
// the shape six of the seven shipped flows have.
const reasonFlow = `{
	"id": "reason-test",
	"specVersion": "v2",
	"initialNode": "talk",
	"global": {
		"persona": "You answer the phone.",
		"alwaysAllowedTools": ["hangup"],
		"transitions": [{"on": "TOOL_RESULT", "tool": "hangup", "target": "goodbye"}]
	},
	"nodes": {
		"talk": {"instruction": "Help the caller.", "tools": []},
		"goodbye": {
			"instruction": "Say goodbye.",
			"announce": {"en": "Goodbye.", "zh": "再见。"},
			"tools": [], "isTerminal": true
		}
	}
}`

// underwayActions is fakeActions that also says whether the call's ending is
// under way, as the application's actions do.
type underwayActions struct {
	*fakeActions
	mu         sync.Mutex
	isUnderway bool
}

func (a *underwayActions) IsEndingUnderway() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isUnderway
}

// reasonRuntime is a runtime on reasonFlow whose log is kept.
func reasonRuntime(t *testing.T, actions Actions, lang string) (*Runtime, *bytes.Buffer) {
	t.Helper()
	spec, err := Load([]byte(reasonFlow))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	return NewRuntime(NewEngine(spec, lang, nil, log), actions, NewBackend(""), nil, log), &logs
}

func TestHangupOffersARequiredReasonEnum(t *testing.T) {
	t.Parallel()
	for _, lang := range []string{LangEN, LangZH} {
		r, _ := reasonRuntime(t, &fakeActions{}, lang)
		var spec []byte
		for _, tool := range r.Tools() {
			if tool.Name == ToolHangup {
				spec = tool.Parameters
			}
		}
		var schema struct {
			Properties map[string]struct {
				Type        string   `json:"type"`
				Description string   `json:"description"`
				Enum        []string `json:"enum"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(spec, &schema); err != nil {
			t.Fatalf("%s: hangup's parameters are not a schema: %v\n%s", lang, err, spec)
		}
		reason, ok := schema.Properties["reason"]
		if !ok || reason.Type != "string" || !slices.Equal(reason.Enum, HangupReasons) {
			t.Errorf("%s: reason = %+v, want a string enum of %v", lang, reason, HangupReasons)
		}
		if !slices.Equal(schema.Required, []string{"reason"}) {
			t.Errorf("%s: required = %v, want [reason]", lang, schema.Required)
		}
		for _, value := range HangupReasons {
			if !strings.Contains(reason.Description, value+":") && !strings.Contains(reason.Description, value+"：") {
				t.Errorf("%s: the description does not say what %s means: %q", lang, value, reason.Description)
			}
		}
		if lang == LangZH && !strings.Contains(reason.Description, "来电者") {
			t.Errorf("zh: the reason is not described in Chinese: %q", reason.Description)
		}
	}
}

// The description says when, not how fast. A mistaken call is a sentence of
// its own, stated as the caller's intent and ended with no words first (#554):
// as a slot in the only-when list, followed by an unscoped "never in reply to a
// question: answer, ask whether there is anything else", flash met a wrong
// number at the greeting by asking again how it could help. The question clause
// is about a question the caller asked, and asking whether this is the right
// number is one.
func TestHangupIsDescribedByWhenNotHowSoon(t *testing.T) {
	t.Parallel()
	type text struct {
		// when is the only-when list, which no longer holds the mistake; mistake
		// is the sentence that does.
		when, mistake string
		want          []string
		stale         []string
	}
	for lang, tc := range map[string]text{
		LangEN: {
			when:    "Call it only when",
			mistake: "When the caller says this call is a mistake",
			want: []string{
				"they have the wrong number, did not mean to call, or were looking for someone else",
				"call it straight away with reason CALLER_ASKED_TO_END, saying nothing first",
				`no reassurance such as "no problem"`,
				"without asking again how you can help",
				"Never call it in the same reply as an answer",
				"never in reply to a question the caller asked",
				"A caller who only asks whether they have the right number is asking such a question.",
			},
			stale: []string{"as soon as", "in reply to a question:", "called by mistake, or"},
		},
		LangZH: {
			when:    "只在以下情况调用",
			mistake: "来电者说这通电话打错了",
			want: []string{
				"拨错号码、不小心打来，或者要找的是别人",
				"直接调用，reason 用 CALLER_ASKED_TO_END，调用前什么都不要说",
				"不要先说“没关系”",
				"也不要再问有什么可以帮您",
				"不要在回答来电者的同一次回复里调用",
				"来电者提问时也不要调用",
				"来电者只是问自己有没有打对电话，也是提问。",
			},
			stale: []string{"立即调用", "来电者说打错电话或拨错号码了；"},
		},
	} {
		description := builtinSchemas(lang, nil)[ToolHangup].Description.For(lang)
		for _, fragment := range append([]string{tc.when, tc.mistake}, tc.want...) {
			if !strings.Contains(description, fragment) {
				t.Errorf("%s: the description lacks %q: %q", lang, fragment, description)
			}
		}
		for _, stale := range tc.stale {
			if strings.Contains(description, stale) {
				t.Errorf("%s: the description still says %q", lang, stale)
			}
		}
		// Its own sentence: the only-when list ends before the mistake starts.
		when, mistake := strings.Index(description, tc.when), strings.Index(description, tc.mistake)
		if when < 0 || mistake < 0 {
			continue
		}
		list := description[when:mistake]
		if strings.Contains(list, "CALLER_ASKED_TO_END") || strings.Contains(list, "wrong number") ||
			strings.Contains(list, "打错") || strings.Contains(list, "拨错") {
			t.Errorf("%s: the only-when list still names the mistaken call: %q", lang, list)
		}
	}
}

// The reason's description gives a mistaken call to CALLER_ASKED_TO_END in
// the same intent words as the tool's description (#554).
func TestTheAskedToEndReasonNamesAMistakenCall(t *testing.T) {
	t.Parallel()
	for lang, want := range map[string]string{
		LangEN: "CALLER_ASKED_TO_END: the caller asked to end the call, or said this call is a mistake " +
			"(the wrong number, a call they did not mean to make, someone else they were looking for).",
		LangZH: "CALLER_ASKED_TO_END：来电者要求结束通话，或者说这通电话打错了（拨错号码、不小心打来、要找的是别人）。",
	} {
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(hangupParameters(lang == LangZH), &schema); err != nil {
			t.Fatal(err)
		}
		if got := schema.Properties["reason"].Description; !strings.Contains(got, want) {
			t.Errorf("%s: the reason's description lacks %q: %q", lang, want, got)
		}
	}
}

func TestAHangupWithoutAValidReasonIsRefusedAndRunsNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ lang, args, want string }{
		"no arguments":       {LangEN, `{}`, "hangup needs a reason"},
		"empty":              {LangEN, `{"reason":""}`, "hangup needs a reason"},
		"unknown":            {LangEN, `{"reason":"CALLER_LEFT"}`, `"CALLER_LEFT" is not a hangup reason`},
		"not a string":       {LangEN, `{"reason":7}`, `"7" is not a hangup reason`},
		"only a stray claim": {LangEN, `{"isFarewellSpoken":true}`, "hangup needs a reason"},
		"zh, no arguments":   {LangZH, `{}`, "hangup 需要 reason 参数"},
		"zh, unknown":        {LangZH, `{"reason":"BYE"}`, `"BYE" 不是有效的 hangup 原因`},
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
			if !strings.Contains(errText, tc.want) {
				t.Errorf("error = %q, want it to contain %q", errText, tc.want)
			}
			for _, value := range HangupReasons {
				if !strings.Contains(errText, value) {
					t.Errorf("error = %q, does not name %s", errText, value)
				}
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
			if !strings.Contains(logs.String(), "hangup refused: no valid reason") {
				t.Errorf("the refusal left no trace in the log:\n%s", logs.String())
			}
		})
	}
}

func TestEveryReasonIsAcceptedAndReachesTheApplication(t *testing.T) {
	t.Parallel()
	for _, value := range HangupReasons {
		for _, written := range []string{value, " " + strings.ToLower(value) + " "} {
			actions := &fakeActions{}
			r, logs := reasonRuntime(t, actions, LangEN)
			if r.IsHangupReasonRefused(`{"reason":"` + written + `"}`) {
				t.Errorf("%q: IsHangupReasonRefused says it would be refused", written)
			}
			output, moved := r.Dispatch(t.Context(), ToolHangup, `{"reason":"`+written+`"}`)
			if decodeOutput(t, output)["ok"] != "1" || moved != "goodbye" {
				t.Errorf("%q: output %s, moved %q; want it run into goodbye", written, output, moved)
			}
			if len(actions.hangups) != 1 || actions.hangups[0].Reason != value {
				t.Errorf("%q: the application got %+v, want reason %s", written, actions.hangups, value)
			}
			if !strings.Contains(logs.String(), `msg="tool ran" flowId=reason-test tool=hangup`) ||
				!strings.Contains(logs.String(), "reason="+value) {
				t.Errorf("%q: the reason is not in the tool ran line:\n%s", written, logs.String())
			}
		}
	}
}

// A stray argument beside the reason is ignored, as isFarewellSpoken has been
// since #441.
func TestAStrayArgumentBesideTheReasonIsIgnored(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, _ := reasonRuntime(t, actions, LangEN)
	output, moved := r.Dispatch(t.Context(), ToolHangup,
		`{"reason":"CALLER_GOODBYE","isFarewellSpoken":true,"note":"bye"}`)
	if decodeOutput(t, output)["ok"] != "1" || moved != "goodbye" {
		t.Errorf("output %s, moved %q; want it run", output, moved)
	}
	if len(actions.hangups) != 1 || actions.hangups[0].Reason != HangupCallerGoodbye {
		t.Errorf("the application got %+v", actions.hangups)
	}
}

// The closing paths answer a hangup as before: over an ending under way, or in
// a terminal phase, nothing about the reason is judged.
func TestTheReasonIsNotJudgedOnceTheEndingIsUnderwayOrInATerminalPhase(t *testing.T) {
	t.Parallel()
	t.Run("an ending under way", func(t *testing.T) {
		t.Parallel()
		actions := &underwayActions{fakeActions: &fakeActions{}, isUnderway: true}
		r, logs := reasonRuntime(t, actions, LangEN)
		if r.IsHangupReasonRefused(`{}`) {
			t.Error("IsHangupReasonRefused says a hangup over an ending under way is refused")
		}
		output, _ := r.Dispatch(t.Context(), ToolHangup, `{}`)
		if decodeOutput(t, output)["ok"] != "1" || len(actions.hangups) != 1 {
			t.Errorf("output %s, hangups %v; want it handed to the application", output, actions.hangups)
		}
		if strings.Contains(logs.String(), "hangup refused") {
			t.Errorf("a hangup over an ending under way was judged:\n%s", logs.String())
		}
	})
	t.Run("not under way", func(t *testing.T) {
		t.Parallel()
		actions := &underwayActions{fakeActions: &fakeActions{}}
		r, _ := reasonRuntime(t, actions, LangEN)
		output, _ := r.Dispatch(t.Context(), ToolHangup, `{}`)
		if decodeOutput(t, output)["ok"] != "0" || len(actions.hangups) != 0 {
			t.Errorf("output %s, hangups %v; want the refusal", output, actions.hangups)
		}
	})
	t.Run("a terminal phase", func(t *testing.T) {
		t.Parallel()
		actions := &fakeActions{}
		r, _ := reasonRuntime(t, actions, LangEN)
		if _, moved := r.Dispatch(t.Context(), ToolHangup, `{"reason":"CALLER_GOODBYE"}`); moved != "goodbye" {
			t.Fatalf("moved to %q, want goodbye", moved)
		}
		if r.IsHangupReasonRefused(`{}`) {
			t.Error("IsHangupReasonRefused says a hangup in a terminal phase is refused")
		}
		output, _ := r.Dispatch(t.Context(), ToolHangup, `{}`)
		if decodeOutput(t, output)["ok"] != "1" || len(actions.hangups) != 2 {
			t.Errorf("output %s, hangups %v; want the second hangup handed over unjudged", output, actions.hangups)
		}
	})
}

// The guard's refusal (#484, the orchestrator's) runs nothing and says why, in
// the call's language. The one that asks for a turn has the model ask whether
// anything else is needed; the one sent without a turn (#555), read with the
// caller's next words, has it answer them and asks nothing now.
func TestRefuseWhileAnsweringIsAConversation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lang        string
		isTurnAsked bool
		want, not   []string
	}{
		"en, turn asked": {LangEN, true,
			[]string{"the call was not ended", "Do not repeat your answer", "Ask whether they need anything else", "goodbye"},
			[]string{"Answer what they say next"}},
		"zh, turn asked": {LangZH, true,
			[]string{"通话没有结束", "不要重复", "问对方还有没有其他需要", "道别"},
			[]string{"回应对方接下来说的话"}},
		"en, quiet": {LangEN, false,
			[]string{"the call was not ended", "The caller has heard your answer; do not say it again",
				"Answer what they say next", "goodbye", "need nothing more"},
			[]string{"anything else", "Ask"}},
		"zh, quiet": {LangZH, false,
			[]string{"通话没有结束", "来电者已经听到了你的回答，不要再说一遍", "回应对方接下来说的话", "道别"},
			[]string{"问对方还有没有其他需要"}},
	} {
		actions := &fakeActions{}
		r, _ := reasonRuntime(t, actions, tc.lang)
		got := decodeOutput(t, r.RefuseWhileAnswering(ToolHangup, tc.isTurnAsked))
		if got["ok"] != "0" {
			t.Errorf("%s: ok = %v, want 0", name, got["ok"])
		}
		if _, hasHint := got["hint"]; hasHint {
			t.Errorf("%s: the refusal carries a hint: %v", name, got["hint"])
		}
		errText, _ := got["error"].(string)
		for _, fragment := range append(tc.want, ToolHangup) {
			if !strings.Contains(errText, fragment) {
				t.Errorf("%s: error = %q, lacks %q", name, errText, fragment)
			}
		}
		for _, fragment := range tc.not {
			if strings.Contains(errText, fragment) {
				t.Errorf("%s: error = %q, says %q", name, errText, fragment)
			}
		}
		if len(actions.hangups) != 0 || r.Engine().NodeID() != "talk" {
			t.Errorf("%s: the refusal acted: hangups %v, node %s", name, actions.hangups, r.Engine().NodeID())
		}
	}
}

// The action-claim rule no longer endorses announcing the end of the call in
// the reply that calls hangup.
func TestTheActionClaimRuleDoesNotEndorseAnnouncingTheEnd(t *testing.T) {
	t.Parallel()
	for lang, want := range map[string]string{
		LangEN: "in the reply that calls hangup, say nothing about ending the call and no goodbye",
		LangZH: "调用 hangup 的那次回复里不要说要结束通话，也不要道别",
	} {
		r, _ := reasonRuntime(t, &fakeActions{}, lang)
		rule := r.actionClaimRule()
		if !strings.Contains(rule, want) {
			t.Errorf("%s: the rule lacks %q: %q", lang, want, rule)
		}
		for _, stale := range []string{"saving a message, ending the call", "记录留言、结束通话"} {
			if strings.Contains(rule, stale) {
				t.Errorf("%s: the rule still lists ending the call among the claims a tool call backs", lang)
			}
		}
	}
}
