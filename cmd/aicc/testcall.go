// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// Exit codes, so a bring-up script can tell apart "the carrier did not answer"
// from "the platform lost the call's trace" without parsing the summary.
const (
	testCallAnswered    = 0 // answered; a recording that was started was booked or is still on its way
	testCallNotAnswered = 2 // placed, ended without an answer
	testCallPlaceFailed = 3 // the switch or the platform refused the placement
	testCallNoCDR       = 4 // no ledger row within the wait
	testCallNoRecording = 5 // answered and recording was started, but no object arrived in time
)

// testCallFlow is the minimal conversation the command can seed so a fresh
// deployment can place its first call with nothing configured: one sentence,
// a terminal closing, very few turns.
const testCallFlowSlug = "aicc_testcall"

var testCallFlow = []byte(`{
  "id": "` + testCallFlowSlug + `",
  "specVersion": "v2",
  "initialNode": "greet",
  "global": {
    "persona": {
      "en": "You are a short, polite voice assistant used by an automated test call.",
      "zh": "你是自动测试电话使用的简短、礼貌的语音助手。"
    },
    "closingTarget": "farewell",
    "maxTurns": 3
  },
  "nodes": {
    "greet": {
      "announce": {
        "en": "Hello. This is a test call from your AI call center. Goodbye.",
        "zh": "你好，这是你的 AI 呼叫中心拨出的测试电话，再见。"
      },
      "instruction": {
        "en": "This is an automated test call. Greet the caller briefly; if they speak, acknowledge and end the call.",
        "zh": "这是自动测试电话。简短问候；对方说话时简单回应并结束通话。"
      },
      "tools": []
    },
    "farewell": {
      "isTerminal": true,
      "announce": {"en": "Goodbye.", "zh": "再见。"},
      "instruction": {"en": "Say goodbye.", "zh": "道别。"},
      "tools": []
    }
  },
  "tools": {}
}`)

// runTestCall implements `aicc testcall`: place one AI outbound call through
// the configured trunk and report what the call left behind — the carrier
// bring-up check deploy/carrier.md points at (#538). It runs in the container
// like doctor, talks to the switch over its own ESL link, and leaves no
// credentials, keys or numbers in what it prints.
func runTestCall(args []string) error {
	fs := flag.NewFlagSet("testcall", flag.ExitOnError)
	to := fs.String("to", "", "the number to call, as the trunk is sent it (required)")
	didNumber := fs.String("did", "", "the caller-ID number whose flow takes the call (default: AICC_OUTBOUND_CALLER_ID)")
	flowSlug := fs.String("flow", "", "bind this flow to the DID first; when unset a missing DID is bound to a built-in one-sentence flow")
	wait := fs.Duration("wait", 180*time.Second, "how long to wait for the CDR, and for the recording when one was started")
	language := fs.String("language", "", "override the DID's language")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == "" {
		fs.Usage()
		return errors.New("--to is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	did := *didNumber
	if did == "" {
		did = cfg.OutboundCallerID
	}
	if did == "" {
		return errors.New("no DID: pass --did or set AICC_OUTBOUND_CALLER_ID, the number the carrier is shown")
	}
	if err := ensureTestCallLine(ctx, st, did, *flowSlug); err != nil {
		return err
	}

	// One ESL link for the command's own commands and event feed. The armed
	// continuation that bridges an answered leg lives in this process, so the
	// link and the loop stay up until the outcome is decided.
	link := esl.NewLink(cfg.ESLAddr, cfg.ESLPassword, telephony.Subscriptions)
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go link.Run(lctx)
	adapter := telephony.NewAdapter(link, cfg.SwitchDomain)
	catalogSvc := catalog.NewService(st.Catalog(), adapter, st.Catalog())

	svc := outbound.New(outbound.Config{
		OverflowHold:         cfg.OverflowHold,
		EndpointFormat:       cfg.OutboundEndpoint,
		SIPHeaders:           cfg.OutboundSIPHeaders,
		CallerID:             cfg.OutboundCallerID,
		RingTimeout:          cfg.OutboundRingTimeout,
		DefaultCountryCode:   cfg.DefaultCountryCode,
		BotMaxCallDuration:   cfg.BotMaxCallDuration,
		AgentMaxCallDuration: cfg.AgentMaxCallDuration,
	}, adapter, catalogSvc, catalogSvc, st.Ledger().HasCDR,
		func(uuid.UUID) bool { return false }, slog.Default()).WithDNC(st.DNC())

	go func() {
		for raw := range link.Events() {
			if ev, ok := telephony.Normalize(raw); ok {
				svc.HandleSwitchEvent(ev)
			}
		}
	}()

	callID := uuid.Must(uuid.NewV7())
	req := outbound.AIDialRequest{CallID: callID, To: *to, DIDNumber: did, Language: *language}
	_, err = svc.PlaceAI(ctx, req)
	if err != nil {
		fmt.Printf("callId: %s\noutcome: PLACEMENT FAILED (%v)\n", callID, err)
		os.Exit(testCallPlaceFailed)
	}

	out := awaitTestCall(ctx, st, callID, *wait)
	printTestCallReport(out)
	os.Exit(out.code)
	return nil
}

// ensureTestCallLine makes the caller-ID number placeable: created with a
// flow when it is missing, repointed when --flow names another. A DID that
// exists but may not place outbound is refused rather than silently edited.
func ensureTestCallLine(ctx context.Context, st *store.Store, number, flowSlug string) error {
	flows := st.Flows()

	var existing *queries.Did
	did, err := st.Queries.GetDIDByNumber(ctx, number)
	switch {
	case err == nil:
		existing = &did
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return fmt.Errorf("read number %s: %w", number, err)
	}

	// Which flow the call runs: the asked-for slug, the DID's own, or the
	// built-in one-sentence flow this command seeds for a fresh deployment.
	var flowID *uuid.UUID
	switch {
	case flowSlug != "":
		f, err := st.Queries.GetFlowBySlug(ctx, flowSlug)
		if err != nil {
			return fmt.Errorf("flow %q: %w (seed it with aicc flowadd)", flowSlug, err)
		}
		flowID = &f.ID
	case existing != nil && existing.FlowID != nil:
		flowID = existing.FlowID
	default:
		f, err := st.Queries.GetFlowBySlug(ctx, testCallFlowSlug)
		var id uuid.UUID
		if err == nil {
			id = f.ID
		} else {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read flow %q: %w", testCallFlowSlug, err)
			}
			id, err = flows.Create(ctx, testCallFlowSlug, "Built-in test-call flow", testCallFlow)
			if err != nil {
				return fmt.Errorf("seed the test flow: %w", err)
			}
			if err := flows.Publish(ctx, id, "testcall"); err != nil {
				return fmt.Errorf("publish the test flow: %w", err)
			}
			fmt.Printf("seeded the built-in flow %q\n", testCallFlowSlug)
		}
		flowID = &id
	}

	if existing == nil {
		if _, err := st.Queries.CreateDID(ctx, queries.CreateDIDParams{
			ID:                 uuid.Must(uuid.NewV7()),
			Number:             number,
			Language:           "en",
			FlowID:             flowID,
			IsRecordingEnabled: true,
			Description:        "created by aicc testcall",
			IsEnabled:          true,
			AllowOutbound:      true,
		}); err != nil {
			return fmt.Errorf("create number %s: %w", number, err)
		}
		fmt.Printf("created number %s bound to flow %s (outbound enabled)\n", maskCallee(number), *flowID)
		return nil
	}
	if !existing.AllowOutbound {
		return fmt.Errorf("number %s is inbound only; set allowOutbound on it (PUT /dids/{didId}) or pass --did", maskCallee(number))
	}
	if flowSlug != "" && (existing.FlowID == nil || *existing.FlowID != *flowID) {
		if _, err := st.Queries.UpdateDID(ctx, queries.UpdateDIDParams{
			ID: existing.ID, Language: existing.Language, FlowID: flowID,
			FallbackQueueID: existing.FallbackQueueID, IsRecordingEnabled: existing.IsRecordingEnabled,
			Description: existing.Description, IsEnabled: existing.IsEnabled,
			AllowInbound: existing.AllowInbound, AllowOutbound: existing.AllowOutbound,
			IsDefaultOutbound: existing.IsDefaultOutbound, MaxOutboundChannels: existing.MaxOutboundChannels,
		}); err != nil {
			return fmt.Errorf("bind flow to %s: %w", number, err)
		}
		fmt.Printf("bound number %s to flow %s\n", maskCallee(number), flowSlug)
	}
	return nil
}

// testCallOutcome is what the wait resolved to: the row, whether a recording
// arrived, and the exit code it maps to.
type testCallOutcome struct {
	callID   uuid.UUID
	cdr      *store.CDR
	recorded bool
	lines    int
	code     int
}

// awaitTestCall polls the ledger until the CDR exists, and — on an answered
// call that started a recording — until the object is booked, both inside the
// same wait budget.
func awaitTestCall(ctx context.Context, st *store.Store, callID uuid.UUID, wait time.Duration) testCallOutcome {
	deadline := time.Now().Add(wait)
	out := testCallOutcome{callID: callID}
	for {
		cdr, err := st.Ledger().GetCDR(ctx, callID)
		if err == nil {
			out.cdr = &cdr
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) && time.Now().After(deadline) {
			out.code = testCallNoCDR
			return out
		}
		if time.Now().After(deadline) {
			out.code = testCallNoCDR
			return out
		}
		select {
		case <-ctx.Done():
			out.code = testCallNoCDR
			return out
		case <-time.After(2 * time.Second):
		}
	}

	answered := !out.cdr.AnsweredAt.IsZero()
	if !answered {
		out.code = testCallNotAnswered
		return out
	}
	if !out.cdr.RecordingExpected {
		out.code = testCallAnswered
		out.lines = transcriptLineCount(ctx, st, callID, deadline)
		return out
	}
	// A recording was announced: the object lands after hangup. Keep waiting,
	// inside the same budget, for it — the whole reason this command exists.
	for {
		recs, err := st.Ledger().RecordingsByCall(ctx, callID)
		if err == nil && len(recs) > 0 {
			out.recorded = true
			out.cdr.HasRecording = true
			out.code = testCallAnswered
			out.lines = transcriptLineCount(ctx, st, callID, deadline)
			return out
		}
		if time.Now().After(deadline) {
			out.code = testCallNoRecording
			out.lines = transcriptLineCount(ctx, st, callID, deadline)
			return out
		}
		select {
		case <-ctx.Done():
			out.code = testCallNoRecording
			return out
		case <-time.After(2 * time.Second):
		}
	}
}

func transcriptLineCount(ctx context.Context, st *store.Store, callID uuid.UUID, deadline time.Time) int {
	for {
		lines, err := st.Ledger().ListTranscript(ctx, callID)
		if err == nil && len(lines) > 0 {
			return len(lines)
		}
		if time.Now().After(deadline) {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(2 * time.Second):
		}
	}
}

// maskCallee renders a number nobody outside can dial back from: its length
// and its head and tail, the middle replaced — never the whole number (#538).
func maskCallee(number string) string {
	digits := utf8.RuneCountInString(number)
	if digits <= 6 {
		return number[:1] + "…"
	}
	return number[:3] + repeatStar(digits-6) + number[digits-3:]
}

func repeatStar(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '*'
	}
	return string(out)
}

// printTestCallReport writes the fixed summary: what the carrier did with the
// call and what it left behind, with the callee only masked.
func printTestCallReport(out testCallOutcome) {
	fmt.Printf("callId: %s\n", out.callID)
	switch {
	case out.cdr == nil:
		fmt.Println("outcome: NO CDR within the wait (the platform may still write one; check later)")
		return
	default:
		c := out.cdr
		fmt.Printf("called: %s (%d digits)\n", maskCallee(c.ToNumber), utf8.RuneCountInString(c.ToNumber))
		fmt.Printf("status: %s\n", c.Status)
		if c.HangupCause != "" {
			fmt.Printf("hangupCause: %s\n", c.HangupCause)
		}
		if !c.AnsweredAt.IsZero() {
			fmt.Printf("answeredAt: %s\n", c.AnsweredAt.Format(time.RFC3339))
		}
		fmt.Printf("ringSec: %d\nbillSec: %d\n", c.RingSec, c.BillSec)
		fmt.Printf("recordingState: %s\n", c.RecordingState)
		fmt.Printf("transcript lines: %d\n", out.lines)
	}
	switch out.code {
	case testCallAnswered:
		fmt.Println("outcome: ANSWERED")
	case testCallNotAnswered:
		fmt.Println("outcome: NOT ANSWERED")
	case testCallNoRecording:
		fmt.Println("outcome: ANSWERED but the recording never arrived in time")
	}
}
