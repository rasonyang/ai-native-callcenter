// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"path/filepath"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
)

// The transcript-lag case of issue #587, as data. A real transcriber answers
// late: the completed transcript of the caller's utterance can arrive after
// the model already reacted to the words — here the mock provider plays that
// shape exactly (HEAR with transcriptDelaySec 0.5, a tool call in the gap).
// The call's transcript must carry the caller's speaking time beside the
// arrival offset (speechStartMs) and mark the line that arrived after a later
// one (transcribedLate), rather than read as if the bot answered words nobody
// had said yet.

// TranscriptLagScenarioID names the case in reports.
const TranscriptLagScenarioID = "T1"

// transcriptLagScript is the mock provider script the call's flow instruction
// selects: it hears the callee, delays the transcription 500 ms, and calls
// record_lead in the gap.
const transcriptLagScript = "C02_LATE_TRANSCRIPT"

type TranscriptLagConfig struct {
	API string
	// CalleeSim is the simulator's report endpoint base.
	CalleeSim string
	// MockProvider is the mock provider's HTTP base, for its session counters.
	MockProvider string

	Username string
	Password string
	// FlowFile is the flow spec to publish (its entry instruction renders the
	// script marker from userData.e2eScript).
	FlowFile string

	// OutDir receives report.json. Wait bounds each wait.
	OutDir string
	Wait   time.Duration
}

// TranscriptLagReport is what a run writes to report.json.
type TranscriptLagReport struct {
	CallID   string               `json:"callId"`
	Script   string               `json:"script"`
	Passed   bool                 `json:"passed"`
	Failures []string             `json:"failures"`
	Checks   []string             `json:"checks"`
	Lines    []api.TranscriptLine `json:"lines"`
}

// RunTranscriptLag places one AI outbound call whose mock-provider script
// delays the caller's completed transcription 500 ms past the tool call that
// reacted to the words, then judges the stored transcript.
func RunTranscriptLag(ctx context.Context, cfg TranscriptLagConfig) (*TranscriptLagReport, error) {
	rep := &TranscriptLagReport{Script: transcriptLagScript}
	col := &collector{}
	c := &httpc{base: cfg.API, client: newClient()}
	if err := c.signInWithKey(ctx, cfg.Username, cfg.Password, "e2e-transcriptlag", keyScopes); err != nil {
		return rep, err
	}
	did := fmt.Sprintf("1380000%04d", 1000+time.Now().UnixNano()%9000)
	if _, err := c.publishFlowAndDID(ctx, cfg.FlowFile, "e2e-transcriptlag-"+did, "e2e transcript lag "+did, did, nil); err != nil {
		return rep, err
	}

	var mockBefore struct {
		Total int64 `json:"total"`
	}
	_ = c.getJSON(ctx, cfg.MockProvider+"/_stats", &mockBefore)

	callID := uuid.Must(uuid.NewV7())
	rep.CallID = callID.String()
	kind := api.CreateCallRequestKind("AI_OUTBOUND")
	userData := api.UserData{ScriptSlot: transcriptLagScript}
	var created api.CreateCallResponse
	if err := c.do(ctx, http.MethodPost, "/calls", api.CreateCallRequest{
		CallID: &callID, Kind: &kind, To: SmokeNumber, DID: &did, UserData: &userData,
	}, &created, http.StatusCreated); err != nil {
		return rep, fmt.Errorf("place call: %w", err)
	}

	// The script ends the session; the callee's own bye (9 s on the smoke
	// number) is the bound in the other direction. Wait on the CDR: the
	// transcript is complete once the call is over.
	var cdr api.CDRList
	cdrPath := "/cdrs?did=" + did
	deadline := time.Now().Add(cfg.Wait)
	for {
		if err := c.do(ctx, http.MethodGet, cdrPath, nil, &cdr, http.StatusOK); err == nil && len(cdr.Items) > 0 {
			break
		}
		if time.Now().After(deadline) {
			return rep, fmt.Errorf("no CDR for call %s within %s", callID, cfg.Wait)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return rep, err
		}
	}

	var page api.CallTranscript
	if err := c.do(ctx, http.MethodGet, "/calls/"+callID.String()+"/transcript", nil, &page, http.StatusOK); err != nil {
		return rep, fmt.Errorf("read the transcript: %w", err)
	}
	rep.Lines = page.Items

	assessTranscriptLag(col, page.Items)
	rep.Checks, rep.Failures = col.checks, col.failures
	rep.Passed = len(rep.Failures) == 0
	_ = writeJSON(filepath.Join(cfg.OutDir, "report.json"), rep)
	if rep.Passed {
		return rep, nil
	}
	return rep, fmt.Errorf("%d check(s) failed", len(rep.Failures))
}

// assessTranscriptLag judges the stored lines: the caller's utterance,
// transcribed 500 ms after the tool call that reacted to it, keeps its
// speaking time beside the arrival offset and is marked — and seq itself
// stays in receipt order.
func assessTranscriptLag(col *collector, lines []api.TranscriptLine) {
	var caller, toolCall *api.TranscriptLine
	for i := range lines {
		l := &lines[i]
		if l.Kind == api.TranscriptKind("TOOL_CALL") && toolCall == nil {
			toolCall = l
		}
		if l.Speaker == api.Speaker("CUSTOMER") && l.Kind == api.TranscriptKind("TEXT") && l.SpeechStartMs != nil {
			caller = l
		}
	}
	col.that(toolCall != nil, "the script's record_lead reached the transcript as a TOOL_CALL line")
	if caller == nil {
		col.that(false, "no caller line carries speechStartMs — the speaking time did not reach the ledger")
		return
	}
	col.that(*caller.SpeechStartMs > 0, "the caller line's speechStartMs is %d ms after the answer", *caller.SpeechStartMs)
	if toolCall != nil {
		col.that(caller.Seq > toolCall.Seq,
			"seq keeps receipt order: the late transcript lands after the tool call (seq %d > %d)", caller.Seq, toolCall.Seq)
		col.that(*caller.SpeechStartMs <= toolCall.OffsetMs,
			"the words (said at %d ms) belong before the tool call that landed at %d ms, though the line arrived after it",
			*caller.SpeechStartMs, toolCall.OffsetMs)
	}
	col.that(caller.Content["transcribedLate"] == true,
		"a caller line transcribed after a later one is marked (content.transcribedLate, content %v)", caller.Content)
}
