// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// leadFlow's only phase allows hangup and nothing else, and the flow lists no
// always-allowed tool: record_lead has to be offered and callable without
// either.
const leadFlow = `{
	"id": "lead-test",
	"specVersion": "v2",
	"initialNode": "talk",
	"global": {"persona": "You call people."},
	"nodes": {
		"talk": {"instruction": "Talk.", "tools": ["hangup"]}
	}
}`

func leadRuntime(t *testing.T, actions *fakeActions, isCampaign bool, flowJSON string) *Runtime {
	t.Helper()
	spec, err := Load([]byte(flowJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRuntime(NewEngine(spec, "en", nil, log), actions, NewBackend(""), nil, log)
	if isCampaign {
		r.Deny("transfer_to_agent", "take_message")
		r.OfferRecordLead(t.Context())
	}
	return r
}

func TestRecordLeadIsOfferedOnACampaignCallInAnyPhase(t *testing.T) {
	t.Parallel()
	for name, flowJSON := range map[string]string{"not in the allowlist": leadFlow, "wildcard": denyFlow} {
		r := leadRuntime(t, &fakeActions{}, true, flowJSON)
		if !slices.Contains(offered(r), ToolRecordLead) {
			t.Errorf("%s: record_lead not offered on a campaign call: %v", name, offered(r))
		}
	}
}

func TestRecordLeadIsNeverOfferedOutsideACampaignCall(t *testing.T) {
	t.Parallel()
	for name, flowJSON := range map[string]string{"not in the allowlist": leadFlow, "wildcard": denyFlow} {
		actions := &fakeActions{}
		r := leadRuntime(t, actions, false, flowJSON)
		if slices.Contains(offered(r), ToolRecordLead) {
			t.Errorf("%s: record_lead offered on a non-campaign call: %v", name, offered(r))
		}
		output, _ := r.Dispatch(t.Context(), ToolRecordLead, `{"intent":"HIGH"}`)
		if decodeOutput(t, output)["ok"] != "0" || len(actions.leads) != 0 {
			t.Errorf("%s: a non-campaign call reached RecordLead: %s %v", name, output, actions.leads)
		}
	}
}

func TestRecordLeadHasBilingualDescriptionsAndAnEnum(t *testing.T) {
	t.Parallel()
	en := builtinSchemas(LangEN, nil)[ToolRecordLead]
	zh := builtinSchemas(LangZH, nil)[ToolRecordLead]
	if en.Description.For(LangEN) == "" || zh.Description.For(LangZH) == "" ||
		en.Description.For(LangEN) == zh.Description.For(LangZH) {
		t.Error("record_lead needs a description in each language")
	}
	for _, intent := range LeadIntents {
		if !strings.Contains(string(en.ModelParameters()), `"`+intent+`"`) {
			t.Errorf("schema lacks %s: %s", intent, en.ModelParameters())
		}
	}
}

func TestRecordLeadPassesAValidLeadToTheActionsAndLeavesThePhaseAlone(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r := leadRuntime(t, actions, true, leadFlow)
	output, moved := r.Dispatch(t.Context(), ToolRecordLead, `{"intent":"high","notes":"  send the offer  "}`)
	if decodeOutput(t, output)["ok"] != "1" || moved != "" {
		t.Fatalf("output %s moved %q", output, moved)
	}
	if len(actions.leads) != 1 || actions.leads[0] != (LeadRequest{Intent: "HIGH", Notes: "send the offer"}) {
		t.Fatalf("leads = %v", actions.leads)
	}
	if turns, ok := r.Engine().Slots()["turns"]; ok && turns != 0 {
		t.Errorf("record_lead counted as a turn: %v", turns)
	}
}

func TestRecordLeadRefusesAnInvalidIntentAndStoresNothing(t *testing.T) {
	t.Parallel()
	for _, args := range []string{`{"intent":"BANANAS"}`, `{"intent":""}`, `{}`, `{"intent":5}`, `{"intent":"HIGH MEDIUM"}`} {
		actions := &fakeActions{}
		r := leadRuntime(t, actions, true, leadFlow)
		output, _ := r.Dispatch(t.Context(), ToolRecordLead, args)
		decoded := decodeOutput(t, output)
		if decoded["ok"] != "0" || decoded["error"] == nil || len(actions.leads) != 0 {
			t.Errorf("%s: output %s leads %v, want a tool error and nothing written", args, output, actions.leads)
		}
	}
}

func TestRecordLeadNotesAreCappedByRunes(t *testing.T) {
	t.Parallel()
	for name, unit := range map[string]string{"english": "a", "chinese": "客", "emoji": "😀"} {
		for _, n := range []int{MaxLeadNotesRunes - 1, MaxLeadNotesRunes, MaxLeadNotesRunes + 50} {
			actions := &fakeActions{}
			r := leadRuntime(t, actions, true, leadFlow)
			r.Dispatch(t.Context(), ToolRecordLead,
				`{"intent":"LOW","notes":"`+strings.Repeat(unit, n)+`"}`)
			if len(actions.leads) != 1 {
				t.Fatalf("%s %d: leads = %v", name, n, actions.leads)
			}
			want := min(n, MaxLeadNotesRunes)
			if got := len([]rune(actions.leads[0].Notes)); got != want {
				t.Errorf("%s %d runes: stored %d, want %d", name, n, got, want)
			}
		}
	}
}
