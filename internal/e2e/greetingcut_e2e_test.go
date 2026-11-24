// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The greeting-cut case of issue #596 (internal/e2e/greetingcut.go). e2e.sh
// greeting-cut runs this test in the environment's network, with the
// application on the gateway profile and the mock provider in VAD mode.

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGreetingCut(t *testing.T) {
	cfg := GreetingCutConfig{
		API: *flagAPI, ReadyURL: *flagReady, CalleeSim: *flagCalleeSim, MockProvider: *flagMockProvider,
		ESLAddr: *flagESL, ESLPassword: os.Getenv("ESL_PASSWORD"),
		ComposeConfig: *flagCompose, ScenarioFile: *flagScenario,
		FlowFile: *flagFlow, VADFile: *flagProviderVAD,
		Username: *flagUsername, Password: os.Getenv("E2E_PASSWORD"),
		OutDir: *flagOut, Wait: *flagWait,
	}
	for name, v := range map[string]string{
		"-e2e.api": cfg.API, "-e2e.ready": cfg.ReadyURL, "-e2e.calleesim": cfg.CalleeSim, "-e2e.mockprovider": cfg.MockProvider,
		"-e2e.esl": cfg.ESLAddr, "-e2e.compose-config": cfg.ComposeConfig, "-e2e.scenario": cfg.ScenarioFile,
		"-e2e.flow": cfg.FlowFile, "-e2e.provider-vad": cfg.VADFile, "-e2e.username": cfg.Username, "-e2e.out": cfg.OutDir,
		"ESL_PASSWORD": cfg.ESLPassword, "E2E_PASSWORD": cfg.Password,
	} {
		if v == "" {
			t.Fatalf("%s is required; run make e2e-greeting-cut", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := RunGreetingCut(ctx, cfg)
	if rep == nil {
		t.Fatalf("the run did not start: %v", err)
	}
	t.Logf("timeline:\n  %s", strings.Join(rep.Timeline, "\n  "))
	for _, c := range rep.Checks {
		t.Logf("check: %s", c)
	}
	for _, fl := range rep.Failures {
		t.Error(fl)
	}
	if err != nil {
		t.Fatalf("%d failure(s); the report is %s", len(rep.Failures), filepath.Join(cfg.OutDir, "report.json"))
	}
}
