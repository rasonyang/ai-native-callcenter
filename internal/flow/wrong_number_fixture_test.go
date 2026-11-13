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

// TestRenderWrongNumberSession writes the session an inbound call opens with
// (the standing instructions, the entry phase's opening line and the tools), as
// this package renders it, for provider.TestLiveQwenWrongNumber (#554). The
// provider package cannot import this one, so the live test reads the
// rendering from a file. It is skipped unless AICC_WRONG_OUT says where the
// rendering goes:
//
//	AICC_WRONG_OUT=../provider/testdata/wrong_number.json \
//	go test -count=1 -run TestRenderWrongNumberSession ./internal/flow/
//
// AICC_WRONG_FLOW names the flow (default ../seed/flows/novanet_support.json).
// A variant wording is measured by rendering a scratch copy of the flow to a
// file of its own and passing that file to the live test as
// AICC_LIVE_WRONG_FIXTURE; the committed fixture stays the shipped flow's.
// AICC_WRONG_LANGUAGES (default "zh,en") lists the languages rendered, and
// AICC_WRONG_QUEUES (default "support-en,support-zh", the demo's queues) the
// queue names transfer_to_agent offers, as a deployment's runtime is given
// them.
func TestRenderWrongNumberSession(t *testing.T) {
	out := os.Getenv("AICC_WRONG_OUT")
	if out == "" {
		t.Skip("set AICC_WRONG_OUT to render a flow for the #554 live test")
	}
	path := os.Getenv("AICC_WRONG_FLOW")
	if path == "" {
		path = "../seed/flows/novanet_support.json"
	}
	languages := os.Getenv("AICC_WRONG_LANGUAGES")
	if languages == "" {
		languages = "zh,en"
	}
	queues := os.Getenv("AICC_WRONG_QUEUES")
	if queues == "" {
		queues = "support-en,support-zh"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The field names are the live test's (wrongNumberSession).
	type session struct {
		Flow         string              `json:"flow"`
		Instructions string              `json:"instructions"`
		Opening      string              `json:"opening"`
		Tools        []provider.ToolSpec `json:"tools"`
		Temperature  *float64            `json:"temperature,omitempty"`
	}
	sessions := map[string]session{}
	for _, language := range strings.Split(languages, ",") {
		spec, err := Load(data)
		if err != nil {
			t.Fatal(err)
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		slots := SeedSlots("8005550100", spec.Entry, "", nil)
		runtime := NewRuntime(NewEngine(spec, language, slots, log), &fakeActions{}, NewBackend(""),
			strings.Split(queues, ","), log)
		s := session{Flow: spec.ID, Instructions: runtime.Instructions(), Opening: runtime.Announce(),
			Tools: runtime.Tools(), Temperature: spec.Global.Temperature}
		if s.Opening == "" {
			t.Fatalf("%s: the entry phase %q has no opening line", language, spec.InitialNode)
		}
		isHangupOffered := false
		for _, tool := range s.Tools {
			isHangupOffered = isHangupOffered || tool.Name == ToolHangup
		}
		if !isHangupOffered {
			t.Fatalf("%s: the entry phase %q does not offer hangup", language, spec.InitialNode)
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
