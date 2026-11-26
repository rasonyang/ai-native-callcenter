// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"maps"
	"strings"
	"testing"
)

// #612: which tool calls the application's lead guard judges. A call that
// would record the customer's interest: a move into a phase that declares a
// HIGH or MEDIUM lead, or record_lead with one. Nothing else.

func TestOnlyACallThatWouldRecordInterestClaimsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tool, args string
		want             bool
	}{
		// declaredLeadFlow's pitch: classify yes moves into close_high (HIGH).
		{"a slot tool's value that moves into a HIGH phase", "classify", `{"value": "yes"}`, true},
		{"the same tool's value that moves into DO_NOT_CALL", "classify", `{"value": "stop"}`, false},
		{"the same tool's value that moves into LOW", "classify", `{"value": "maybe"}`, false},
		{"the same tool's value that moves nowhere", "classify", `{"value": "other"}`, false},
		{"a value outside the enum: refused, it moves nowhere", "classify", `{"value": "sure"}`, false},
		{"a tool that never moves", "agree_words", `{"value": "yes please"}`, false},
		{"hangup", ToolHangup, `{"reason": "CALLER_GOODBYE"}`, false},
		{"record_lead HIGH", ToolRecordLead, `{"intent": "HIGH", "notes": "wants it"}`, true},
		{"record_lead MEDIUM, as the model may spell it", ToolRecordLead, `{"intent": " medium "}`, true},
		{"record_lead LOW", ToolRecordLead, `{"intent": "LOW"}`, false},
		{"record_lead NONE", ToolRecordLead, `{"intent": "NONE"}`, false},
		{"record_lead DO_NOT_CALL", ToolRecordLead, `{"intent": "DO_NOT_CALL"}`, false},
		{"record_lead with no intent", ToolRecordLead, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _ := declaredLeadRuntime(t, &fakeActions{}, true, declaredLeadFlow, LangEN, nil)
			if got := r.ClaimsInterest(tc.tool, tc.args); got != tc.want {
				t.Errorf("ClaimsInterest(%s, %s) = %v, want %v", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}

// Asking changes nothing: no slot, no turn, no phase, no lead, no log line of
// a phase entered.
func TestAskingWhetherACallClaimsInterestChangesNothing(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r, logs := declaredLeadRuntime(t, actions, true, declaredLeadFlow, LangEN, nil)
	before := maps.Clone(r.Engine().Slots())
	beforeLog := logs.String()
	if !r.ClaimsInterest("classify", `{"value": "yes"}`) {
		t.Fatal("classify yes does not claim interest")
	}
	if got := r.Engine().Slots(); !maps.EqualFunc(got, before, func(a, b any) bool { return a == b }) {
		t.Errorf("slots changed: %v, were %v", got, before)
	}
	if r.Engine().NodeID() != "pitch" || len(actions.leads) != 0 {
		t.Errorf("the call is in %q with leads %v", r.Engine().NodeID(), actions.leads)
	}
	if got := strings.TrimPrefix(logs.String(), beforeLog); got != "" {
		t.Errorf("asking logged %q", got)
	}
	// The real call then moves as predicted.
	dispatchOK(t, r, "classify", `{"value": "yes"}`)
	if r.Engine().NodeID() != "close_high" || len(actions.leads) != 1 || actions.leads[0].Intent != LeadHigh {
		t.Errorf("the call is in %q with leads %v", r.Engine().NodeID(), actions.leads)
	}
}

// A move is judged in the phase the response was made in, as the allowlist
// and the rules are; record_lead in the phase the call is in, as Dispatch
// judges it.
func TestInterestIsJudgedWhereTheCallWouldBeJudged(t *testing.T) {
	t.Parallel()
	t.Run("a move inside a batch", func(t *testing.T) {
		t.Parallel()
		r, _ := declaredLeadRuntime(t, &fakeActions{}, true, declaredLeadFlow, LangEN, nil)
		r.BeginToolBatch()
		dispatchOK(t, r, "classify", `{"value": "maybe"}`) // now in objection, the batch still pitch's
		if !r.ClaimsInterest("classify", `{"value": "yes"}`) {
			t.Error("a later move into the HIGH phase in the same response is not judged")
		}
		r.EndToolBatch()
	})
	t.Run("record_lead in a phase that declares its own", func(t *testing.T) {
		t.Parallel()
		r, _ := declaredLeadRuntime(t, &fakeActions{}, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "classify", `{"value": "maybe"}`) // objection declares LOW
		if r.ClaimsInterest(ToolRecordLead, `{"intent": "HIGH"}`) {
			t.Error("a record_lead Dispatch refuses is judged")
		}
	})
	t.Run("not a campaign call", func(t *testing.T) {
		t.Parallel()
		r, _ := declaredLeadRuntime(t, &fakeActions{}, false, declaredLeadFlow, LangEN, nil)
		if r.ClaimsInterest(ToolRecordLead, `{"intent": "HIGH"}`) {
			t.Error("record_lead is judged on a call that does not offer it")
		}
		if !r.ClaimsInterest("classify", `{"value": "yes"}`) {
			t.Error("a move into a phase whose declaration says the customer agreed is not judged")
		}
	})
	t.Run("from a terminal phase", func(t *testing.T) {
		t.Parallel()
		r, _ := declaredLeadRuntime(t, &fakeActions{}, true, declaredLeadFlow, LangEN, nil)
		dispatchOK(t, r, "classify", `{"value": "stop"}`)
		if r.ClaimsInterest("classify", `{"value": "yes"}`) {
			t.Error("a terminal phase moves nowhere")
		}
	})
}

// A tool whose result is the backend's or the call's (here a built-in) is
// judged on its rules: one that could lead into an interest phase counts.
func TestAToolWhoseResultIsUnknownIsJudgedOnItsRules(t *testing.T) {
	t.Parallel()
	flowJSON := strings.Replace(declaredLeadFlow,
		`"tools": ["classify", "agree_words", "hangup"],`,
		`"tools": ["classify", "agree_words", "hangup", "take_message"],`, 1)
	flowJSON = strings.Replace(flowJSON,
		`{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "yes"}, "target": "close_high"},`,
		`{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "yes"}, "target": "close_high"},
				{"on": "TOOL_RESULT", "tool": "take_message", "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "close_high"},`, 1)
	r, _ := declaredLeadRuntime(t, &fakeActions{}, false, flowJSON, LangEN, nil)
	if !r.ClaimsInterest(ToolTakeMessage, `{"message": "call me"}`) {
		t.Error("a built-in whose success leads into a HIGH phase is not judged")
	}
	if !r.Engine().CanMoveIntoInterest(ToolTakeMessage) || r.Engine().CanMoveIntoInterest(ToolHangup) {
		t.Error("CanMoveIntoInterest reads the wrong rules")
	}
}

// offerFlow opens with the offer, in the entry phase's announce: the first %s
// is spliced into global, the second into the entry phase.
const offerFlow = `{
  "id": "offer_in_opening", "specVersion": "v2", "initialNode": "offer",
  "global": {"persona": "p"%s},
  "nodes": {
    "offer": {"instruction": "Make the offer."%s, "tools": []}
  }
}`

func offerSpec(global, announce string) ([]byte, func() (*Spec, error)) {
	if announce != "" {
		announce = `, "announce": ` + announce
	}
	doc := []byte(strings.Replace(strings.Replace(offerFlow, "%s", global, 1), "%s", announce, 1))
	return doc, func() (*Spec, error) { return Load(doc) }
}

// #715: a flow declares that its opening carries the offer; absent is false.
func TestAFlowCanDeclareThatItsOpeningCarriesTheOffer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		global string
		want   bool
	}{
		{`, "isOfferInOpening": true`, true},
		{``, false},
		{`, "isOfferInOpening": false`, false},
	} {
		_, load := offerSpec(tc.global, `{"en": "Hello. Renew now at 20% off, would you like it?"}`)
		spec, err := load()
		if err != nil {
			t.Fatalf("load with %q: %v", tc.global, err)
		}
		logs := &logCapture{}
		r := NewRuntime(NewEngine(spec, LangEN, nil, logs.logger()), &fakeActions{}, NewBackend(""), nil, logs.logger())
		if spec.Global.IsOfferInOpening != tc.want || r.IsOfferInOpening() != tc.want {
			t.Errorf("global %q: isOfferInOpening = %v, runtime %v; want %v",
				tc.global, spec.Global.IsOfferInOpening, r.IsOfferInOpening(), tc.want)
		}
	}
}

// Load refuses the field on a flow whose entry phase says no line of its own:
// there is no opening for the caller to hear the offer in. An announce in one
// language is a line in both (Text.For falls back).
func TestIsOfferInOpeningIsRejectedWithoutAnOpeningLine(t *testing.T) {
	t.Parallel()
	want := `global.isOfferInOpening is set but the entry phase "offer" has no announce`
	for name, announce := range map[string]string{
		"no announce":    ``,
		"empty announce": `""`,
		"both empty":     `{"en": "", "zh": ""}`,
	} {
		_, load := offerSpec(`, "isOfferInOpening": true`, announce)
		if _, err := load(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: Load = %v, want an error containing %q", name, err, want)
		}
		_, load = offerSpec(``, announce)
		if _, err := load(); err != nil {
			t.Errorf("%s without the field: %v", name, err)
		}
	}
	_, load := offerSpec(`, "isOfferInOpening": true`, `{"zh": "您好，现在续约可以打八折，请问您需要吗？"}`)
	if _, err := load(); err != nil {
		t.Errorf("an opening in one language: %v", err)
	}
}

func TestIsOfferInOpeningIsKnownToTheUnknownFieldCheck(t *testing.T) {
	t.Parallel()
	doc, _ := offerSpec(`, "isOfferInOpening": true`, `"Hello."`)
	if found := unknownFields(doc); len(found) != 0 {
		t.Errorf("isOfferInOpening reported as unknown: %v", found)
	}
	found := unknownFields([]byte(strings.Replace(string(doc), "isOfferInOpening", "isOfferInOpenning", 1)))
	if len(found) != 1 || found[0].Suggestion != "isOfferInOpening" {
		t.Errorf("a near miss = %v, want one suggesting isOfferInOpening", found)
	}
}

// The refusals are conversations: ok 0, a reason that names the tool, and
// the phase's instruction as the way on, in the call's language.
func TestTheInterestRefusalsAreConversations(t *testing.T) {
	t.Parallel()
	for _, lang := range []string{LangEN, LangZH} {
		r, logs := declaredLeadRuntime(t, &fakeActions{}, true, declaredLeadFlow, lang, nil)
		started := logs.String()
		for name, output := range map[string]string{
			"unheard, said again":     r.RefuseAsUnheardOffer("classify", true),
			"unheard, not said again": r.RefuseAsUnheardOffer("classify", false),
			"declined":                r.RefuseAsDeclined("classify"),
			"unconfirmed":             r.RefuseAsUnconfirmed("classify"),
			"machine":                 r.RefuseAsMachine("classify"),
		} {
			got := decodeOutput(t, output)
			if got["ok"] != "0" || !strings.Contains(asString(got["error"]), "classify") ||
				!strings.Contains(asString(got["hint"]), r.Engine().Instruction()) {
				t.Errorf("%s %s: %s", lang, name, output)
			}
		}
		if got := strings.TrimPrefix(logs.String(), started); got != "" {
			t.Errorf("%s: the refusals logged %q; the caller logs the decision", lang, got)
		}
		if again, not := r.RefuseAsUnheardOffer("classify", true), r.RefuseAsUnheardOffer("classify", false); again == not {
			t.Errorf("%s: the opening said again or not reads the same", lang)
		}
	}
}

// #715: once the opening cannot be said again, the refusal tells the model to
// stop calling the tool and close the call. Its reason is the way on: it
// carries no hint, since the phase's instruction asks for the offer's answer.
func TestTheRefusalForAnOfferNeverHeardClosesTheCall(t *testing.T) {
	t.Parallel()
	for lang, want := range map[string][]string{
		LangEN: {"Do not call classify again on this call", "end the call", "no interest was recorded"},
		LangZH: {"这通电话不要再调用 classify", "结束通话", "没有记录任何意向"},
	} {
		r, logs := declaredLeadRuntime(t, &fakeActions{}, true, declaredLeadFlow, lang, nil)
		started := logs.String()
		output := r.RefuseAsOfferNeverHeard("classify")
		got := decodeOutput(t, output)
		if got["ok"] != "0" || got["hint"] != nil {
			t.Errorf("%s: %s, want ok 0 and no hint", lang, output)
		}
		for _, phrase := range want {
			if !strings.Contains(asString(got["error"]), phrase) {
				t.Errorf("%s: %s lacks %q", lang, output, phrase)
			}
		}
		if got := strings.TrimPrefix(logs.String(), started); got != "" {
			t.Errorf("%s: the refusal logged %q; the caller logs the decision", lang, got)
		}
		if r.Engine().NodeID() != "pitch" {
			t.Errorf("%s: the refusal moved the call to %q", lang, r.Engine().NodeID())
		}
	}
}
