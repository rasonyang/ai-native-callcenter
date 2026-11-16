// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The test-number case of issue #578 (internal/e2e/frequency.go). e2e.sh
// frequency recreates the application with a frequency window and then runs
// this test.

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrequency(t *testing.T) {
	cfg := CampaignConfig{
		API: *flagAPI, ReadyURL: *flagReady, CalleeSim: *flagCalleeSim, MockProvider: *flagMockProvider,
		MetricsURL: *flagMetrics, ESLAddr: *flagESL, ESLPassword: os.Getenv("ESL_PASSWORD"),
		ComposeConfig: *flagCompose, ScenarioFile: *flagScenario, ProviderScenarioFile: *flagProviderScn, FlowFile: *flagFlow,
		Username: *flagUsername, Password: os.Getenv("E2E_PASSWORD"),
		OutDir: *flagOut, Wait: *flagWait,
	}
	for name, v := range map[string]string{
		"-e2e.api": cfg.API, "-e2e.ready": cfg.ReadyURL, "-e2e.calleesim": cfg.CalleeSim, "-e2e.mockprovider": cfg.MockProvider,
		"-e2e.provider-scenario": cfg.ProviderScenarioFile,
		"-e2e.esl":               cfg.ESLAddr, "-e2e.compose-config": cfg.ComposeConfig, "-e2e.scenario": cfg.ScenarioFile,
		"-e2e.flow": cfg.FlowFile, "-e2e.username": cfg.Username, "-e2e.out": cfg.OutDir,
		"ESL_PASSWORD": cfg.ESLPassword, "E2E_PASSWORD": cfg.Password,
	} {
		if v == "" {
			t.Fatalf("%s is required; run make e2e-frequency", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := RunFrequency(ctx, cfg)
	if rep == nil {
		t.Fatalf("the run did not start: %v", err)
	}
	t.Logf("timeline:\n  %s", strings.Join(rep.Timeline, "\n  "))
	for _, c := range rep.Checks.Checks {
		t.Logf("check: %s", c)
	}
	for _, fl := range rep.Checks.Failures {
		t.Error(fl)
	}
	if err != nil {
		for _, fl := range rep.Failures {
			if strings.HasPrefix(fl, runIncomplete) {
				t.Error(fl)
			}
		}
		t.Fatalf("%d failure(s); the report is %s", len(rep.Failures), filepath.Join(cfg.OutDir, "report.json"))
	}
}
