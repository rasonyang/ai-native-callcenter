// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// TestRenderCutOpenerSession writes the session a campaign call opens with and
// the answers the orchestrator gives when the caller asks not to be called
// again (record_lead DO_NOT_CALL, then hangup into the closing phase, the
// closing line riding in the hangup's result as answerToolCall does on a
// Realtime profile), for provider.TestLiveQwenCutOpener (#360). The provider
// package cannot import this one. Skipped unless AICC_CUT_FLOW names a flow:
//
//	AICC_CUT_FLOW=../seed/flows/campaign_lead_followup.json \
//	AICC_CUT_OUT=../provider/testdata/cut_opener.json \
//	go test -count=1 -run TestRenderCutOpenerSession ./internal/flow/
func TestRenderCutOpenerSession(t *testing.T) {
	path := os.Getenv("AICC_CUT_FLOW")
	if path == "" {
		t.Skip("set AICC_CUT_FLOW to render a flow for the #360 live test")
	}
	out := os.Getenv("AICC_CUT_OUT")
	if out == "" {
		t.Fatal("set AICC_CUT_OUT to where the rendering goes")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The field names are the live test's (cutOpenerSession).
	type session struct {
		Instructions       string              `json:"instructions"`
		Opening            string              `json:"opening"`
		Tools              []provider.ToolSpec `json:"tools"`
		Temperature        *float64            `json:"temperature,omitempty"`
		RecordOutput       string              `json:"recordOutput"`
		HangupOutput       string              `json:"hangupOutput"`
		FinishInstructions string              `json:"finishInstructions"`
		ClosingLine        string              `json:"closingLine"`
	}
	sessions := map[string]session{}
	for _, language := range []string{"zh", "en"} {
		spec, err := Load(data)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		slots := SeedSlots("13800000000", spec.Entry, "95061", map[string]any{
			"name": "Chen", "product": "5G Plan", "offer": "20% off the first six months"})
		runtime := NewRuntime(NewEngine(spec, language, slots, log), &fakeActions{}, NewBackend(""), nil, log)
		runtime.OfferRecordLead(t.Context())
		s := session{Instructions: runtime.Instructions(), Opening: runtime.Announce(),
			Tools: runtime.Tools(), Temperature: spec.Global.Temperature}
		s.RecordOutput, _ = runtime.Dispatch(t.Context(), ToolRecordLead,
			`{"intent":"DO_NOT_CALL","notes":"The customer asked not to be called again."}`)
		output, moved := runtime.Dispatch(t.Context(), "hangup", `{}`)
		if moved == "" || runtime.Announce() == "" {
			t.Fatalf("%s: hangup moved to %q, which has no line", language, moved)
		}
		s.HangupOutput = provider.MergeHint(output, provider.SayExactly(runtime.Announce(), runtime.Engine().Lang()))
		s.FinishInstructions, s.ClosingLine = runtime.Instructions(), runtime.Announce()
		sessions[language] = s
	}
	encoded, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
