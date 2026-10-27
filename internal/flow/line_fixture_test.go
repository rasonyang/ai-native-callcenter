// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// TestRenderLineAfterMoveSession writes the session a phase's line is asked for
// in after a slot tool moved the call (#351), as this package renders it, for
// provider.TestLiveQwenLineAfterMove. The provider package cannot import this
// one, so the live test reads the rendering from a file. It is skipped unless
// AICC_LINE_FLOW names a flow:
//
//	AICC_LINE_FLOW=../provider/testdata/line_after_move_flow.json \
//	AICC_LINE_OUT=../provider/testdata/line_after_move.json \
//	go test -count=1 -run TestRenderLineAfterMoveSession ./internal/flow/
//
// The flow's entry phase must move on its slot tool confirm_upgrade (or
// AICC_LINE_AGREE_TOOL) into a phase with an announce. AICC_LINE_LANGUAGES
// (default "zh,en") lists the languages rendered. A deployment's own flow is
// rendered the same way and passed to the live test as AICC_LIVE_LINE_FIXTURE.
func TestRenderLineAfterMoveSession(t *testing.T) {
	path := os.Getenv("AICC_LINE_FLOW")
	if path == "" {
		t.Skip("set AICC_LINE_FLOW to render a flow for the #351 live test")
	}
	out := os.Getenv("AICC_LINE_OUT")
	if out == "" {
		t.Fatal("set AICC_LINE_OUT to where the rendering goes")
	}
	tool := os.Getenv("AICC_LINE_AGREE_TOOL")
	if tool == "" {
		tool = "confirm_upgrade"
	}
	languages := os.Getenv("AICC_LINE_LANGUAGES")
	if languages == "" {
		languages = "zh,en"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The field names are the live test's (lineAfterMoveSession).
	type session struct {
		Instructions      string              `json:"instructions"`
		Opening           string              `json:"opening"`
		Tools             []provider.ToolSpec `json:"tools"`
		Temperature       *float64            `json:"temperature,omitempty"`
		AgreeTool         string              `json:"agreeTool"`
		AgreeOutput       string              `json:"agreeOutput"`
		PhaseInstructions string              `json:"phaseInstructions"`
		PhaseLine         string              `json:"phaseLine"`
	}
	words := map[string]string{"zh": "好的", "en": "Okay, sure."}
	sessions := map[string]session{}
	for _, language := range strings.Split(languages, ",") {
		spec, err := Load(data)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		runtime := NewRuntime(NewEngine(spec, language, nil, log), &fakeActions{}, NewBackend(""), nil, log)
		s := session{Instructions: runtime.Instructions(), Opening: runtime.Announce(),
			Tools: runtime.Tools(), Temperature: spec.Global.Temperature, AgreeTool: tool}
		args, _ := json.Marshal(map[string]string{"value": words[language]})
		output, moved := runtime.Dispatch(t.Context(), tool, string(args))
		if moved == "" || runtime.Announce() == "" {
			t.Fatalf("%s: %s moved to %q, which has no line", language, tool, moved)
		}
		s.AgreeOutput, s.PhaseInstructions, s.PhaseLine = output, runtime.Instructions(), runtime.Announce()
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
