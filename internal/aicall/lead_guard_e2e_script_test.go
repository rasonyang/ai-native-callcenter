// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The isolated e2e stack's C01 case (deploy/e2e): the mock provider plays
// provider-scenario.json, whose C01 script records a HIGH lead, and the case
// expects the lead and its callback. Played here in-process, through the real
// Realtime client on the e2e's openai profile and the call loop, with the
// e2e's flow and a campaign call's wiring: with the callee's words heard first
// the lead is written; without them the lead guard (#612) refuses it, which is
// why the script hears the callee.
func TestTheE2ECampaignScriptStillRecordsItsLead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		isHeard    bool
		wantLeads  []string
		wantReason string
	}{
		{"as shipped, the callee heard", true, []string{"HIGH"}, "verdict=WORDS"},
		{"without the callee's words", false, nil, "verdict=NO_CALLER_TURN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, leads := playE2EScript(t, tc.isHeard)
			if got := leads.intents(); !slices.Equal(got, tc.wantLeads) {
				t.Errorf("leads written = %v, want %v", got, tc.wantLeads)
			}
			if !strings.Contains(logs, leadGuardLine) || !strings.Contains(logs, tc.wantReason) {
				t.Errorf("no decision line with %s:\n%s", tc.wantReason, logs)
			}
		})
	}
}

// playE2EScript runs one C01 call and returns its log and the leads written.
func playE2EScript(t *testing.T, isHeard bool) (string, *guardLeads) {
	t.Helper()
	scenario, err := mockprovider.LoadScenario("../../deploy/e2e/provider-scenario.json")
	if err != nil {
		t.Fatal(err)
	}
	script := scenario.Pick("E2E script: C01_LEAD_HIGH.")
	if script == nil || script.Name != "C01_LEAD_HIGH" {
		t.Fatalf("the scenario has no C01 script: %+v", script)
	}
	var steps []mockprovider.Step
	for _, step := range script.Steps {
		if step.Op == mockprovider.OpHear {
			if !isHeard {
				continue
			}
			// The fake leg plays out at once: nothing is left to hear by then.
			step.AfterSec = 0.05
		}
		steps = append(steps, step)
	}
	script.Steps = steps
	mock := mockprovider.New(mockprovider.Config{FirstAudio: 10 * time.Millisecond,
		DeltaAudio: 20 * time.Millisecond, Scenario: scenario,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	server := httptest.NewServer(mock)
	t.Cleanup(server.Close)

	t.Setenv("OPENAI_API_KEY", "not-a-key")
	profile, err := provider.ProfileFor(provider.NameOpenAI, provider.Override{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"})
	if err != nil {
		t.Fatal(err)
	}
	flowJSON, err := os.ReadFile("../../deploy/e2e/flow.json")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := flow.Load(flowJSON)
	if err != nil {
		t.Fatal(err)
	}

	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	leads := &guardLeads{}
	o, err := NewOrchestrator(OrchestratorConfig{
		Catalog: &fakeCatalog{queues: []catalog.Queue{}}, Flows: fakeFlows{}, Switch: &orderedSwitch{},
		Profile: profile, Leads: leads, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine := flow.NewEngine(spec, "en", flow.SeedSlots("13800138000", spec.Entry, "95001",
		map[string]any{"e2eScript": "C01_LEAD_HIGH"}), log)
	recorder := newStartedRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	actions := &callActions{orchestrator: o, log: log, callerChannel: "chan-9", recorder: recorder,
		facts: testFacts(), campaignID: "c-1", attemptID: "a-1"}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)
	runtime.Deny(campaignDeniedTools...)
	runtime.OfferRecordLead(t.Context())

	model, err := provider.New(profile, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := o.sessionConfig(sessionConfigFor(spec, runtime, "en"), actions.isArmed, log)
	cfg.OnCutAcknowledged = actions.onCutAcknowledged
	session, err := New(newFakeLeg(media.LawMu), model, profile, cfg)
	if err != nil {
		t.Fatal(err)
	}
	actions.session = session
	if err := session.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		o.drive(t.Context(), session, runtime, actions, recorder, sessionBudget{}, log)
		actions.disarm()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the scripted call did not end:\n%s", logs.String())
	}
	return logs.String(), leads
}
