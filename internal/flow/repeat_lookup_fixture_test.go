// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/mockbackend"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// TestRenderRepeatLookupSession writes the session of a call that looks the
// same kind of thing up three times in a row (#466), as this package renders
// it, for provider.TestLiveQwenRepeatLookup. The provider package cannot
// import this one, so the live test reads the rendering from a file. It is
// skipped unless AICC_REPEAT_FLOW names a flow:
//
//	AICC_REPEAT_FLOW=../seed/flows/mobile_support.json \
//	AICC_REPEAT_OUT=../provider/testdata/repeat_lookup.json \
//	go test -count=1 -run TestRenderRepeatLookupSession ./internal/flow/
//
// The flow must declare an HTTP tool repair_status (or AICC_REPEAT_TOOL) that
// the mock backend (internal/mockbackend) answers. AICC_REPEAT_RMAS (default
// "RMA9999,RMA1002,RMA1001": not found, found, found) lists the order numbers
// looked up, in order. AICC_REPEAT_LANGUAGES (default "zh,en") lists the
// languages rendered.
//
// Each lookup is dispatched as the orchestrator does (answerToolCall): inside
// a tool batch, and afterwards the standing instructions are re-rendered when
// the lookup moved the call or changed what the phase's instruction renders
// (IsInstructionStale), which the orchestrator sends before the tool result.
func TestRenderRepeatLookupSession(t *testing.T) {
	path := os.Getenv("AICC_REPEAT_FLOW")
	if path == "" {
		t.Skip("set AICC_REPEAT_FLOW to render a flow for the #466 live test")
	}
	out := os.Getenv("AICC_REPEAT_OUT")
	if out == "" {
		t.Fatal("set AICC_REPEAT_OUT to where the rendering goes")
	}
	tool := os.Getenv("AICC_REPEAT_TOOL")
	if tool == "" {
		tool = "repair_status"
	}
	rmas := os.Getenv("AICC_REPEAT_RMAS")
	if rmas == "" {
		rmas = "RMA9999,RMA1002,RMA1001"
	}
	languages := os.Getenv("AICC_REPEAT_LANGUAGES")
	if languages == "" {
		languages = "zh,en"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(mockbackend.New(nil))
	defer backend.Close()

	// The field names are the live test's (repeatLookupSession).
	type lookup struct {
		RMA          string `json:"rma"`
		Output       string `json:"output"`
		Moved        string `json:"moved"`
		Instructions string `json:"instructions"`
	}
	type session struct {
		Instructions string              `json:"instructions"`
		Opening      string              `json:"opening"`
		Tools        []provider.ToolSpec `json:"tools"`
		Temperature  *float64            `json:"temperature,omitempty"`
		Tool         string              `json:"tool"`
		WaitLine     string              `json:"waitLine"`
		Lookups      []lookup            `json:"lookups"`
	}
	sessions := map[string]session{}
	for _, language := range strings.Split(languages, ",") {
		spec, err := Load(data)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		runtime := NewRuntime(NewEngine(spec, language, nil, log), &fakeActions{}, NewBackend(backend.URL), nil, log)
		s := session{Instructions: runtime.Instructions(), Opening: runtime.Announce(),
			Tools: runtime.Tools(), Temperature: spec.Global.Temperature, Tool: tool,
			WaitLine: runtime.ToolWaitLine()}
		for _, rma := range strings.Split(rmas, ",") {
			args, _ := json.Marshal(map[string]string{"rma": rma})
			runtime.BeginToolBatch()
			output, moved := runtime.Dispatch(t.Context(), tool, string(args))
			l := lookup{RMA: rma, Output: output, Moved: moved}
			if moved != "" || runtime.IsInstructionStale() {
				l.Instructions = runtime.Instructions()
			}
			runtime.EndToolBatch()
			s.Lookups = append(s.Lookups, l)
		}
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
