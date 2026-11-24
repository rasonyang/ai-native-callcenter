// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
)

func text(speaker string, seq int) api.TranscriptLine {
	return api.TranscriptLine{Seq: seq, Speaker: api.Speaker(speaker),
		Kind: api.TranscriptKind("TEXT"), Content: map[string]any{"text": "..."}}
}

// The order the case produces: the tool call landed first, the caller's line
// after it — marked, with the earlier speaking time beside the arrival offset.
func TestAssessTranscriptLagAcceptsTheMarkedOrder(t *testing.T) {
	tool := text("BOT", 3)
	tool.Kind = api.TranscriptKind("TOOL_CALL")
	tool.OffsetMs = 4000
	caller := text("CUSTOMER", 4)
	start := int64(3200)
	caller.SpeechStartMs = &start
	caller.OffsetMs = 4500
	caller.Content["transcribedLate"] = true

	col := &collector{}
	assessTranscriptLag(col, []api.TranscriptLine{text("CUSTOMER", 1), text("BOT", 2), tool, caller})
	if len(col.failures) != 0 {
		t.Fatalf("the case's own order failed: %v", col.failures)
	}
}

func TestAssessTranscriptLagRefusesAnUnmarkedLateLine(t *testing.T) {
	tool := text("BOT", 3)
	tool.Kind = api.TranscriptKind("TOOL_CALL")
	caller := text("CUSTOMER", 4)
	start := int64(3200)
	caller.SpeechStartMs = &start

	col := &collector{}
	assessTranscriptLag(col, []api.TranscriptLine{tool, caller})
	if len(col.failures) == 0 {
		t.Fatal("a late caller line with no transcribedLate mark passed")
	}
}

func TestAssessTranscriptLagRefusesNoSpeakingTime(t *testing.T) {
	tool := text("BOT", 3)
	tool.Kind = api.TranscriptKind("TOOL_CALL")

	col := &collector{}
	assessTranscriptLag(col, []api.TranscriptLine{tool, text("CUSTOMER", 4)})
	if len(col.failures) == 0 {
		t.Fatal("a caller line with no speechStartMs passed")
	}
}
