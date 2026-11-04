// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The capacity tests of issue #247. Like the campaign tests they run inside
// the environment's internal network, from the static test binary e2e.sh
// builds; each scenario needs the application started with its environment,
// which `make e2e-capacity` (deploy/e2e/e2e.sh capacity) does before it runs
// the scenario.

package e2e

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	flagCapacity  = flag.String("e2e.capacity", "", "the capacity scenario to run: K1, K2 or K3")
	flagSwitchLog = flag.String("e2e.switchlog", "", "the switch's log file, for TestRefusalSwitchLog")
)

func TestCapacity(t *testing.T) {
	sc, ok := CapacityScenarios[*flagCapacity]
	if !ok && *flagCapacity != K3.ID {
		t.Fatalf("-e2e.capacity is %q, want one of K1, K2, K3; run make e2e-capacity", *flagCapacity)
	}
	cfg := CampaignConfig{
		API: *flagAPI, ReadyURL: *flagReady, CalleeSim: *flagCalleeSim, MockProvider: *flagMockProvider,
		MetricsURL: *flagMetrics, ESLAddr: *flagESL, ESLPassword: os.Getenv("ESL_PASSWORD"),
		ComposeConfig: *flagCompose, ScenarioFile: *flagScenario, ProviderScenarioFile: *flagProviderScn, FlowFile: *flagFlow,
		Username: *flagUsername, Password: os.Getenv("E2E_PASSWORD"),
		OutDir: *flagOut, Wait: *flagWait,
	}
	for name, v := range map[string]string{
		"-e2e.api": cfg.API, "-e2e.ready": cfg.ReadyURL, "-e2e.calleesim": cfg.CalleeSim, "-e2e.mockprovider": cfg.MockProvider,
		"-e2e.metrics": cfg.MetricsURL, "-e2e.provider-scenario": cfg.ProviderScenarioFile,
		"-e2e.esl": cfg.ESLAddr, "-e2e.compose-config": cfg.ComposeConfig, "-e2e.scenario": cfg.ScenarioFile,
		"-e2e.flow": cfg.FlowFile, "-e2e.username": cfg.Username, "-e2e.out": cfg.OutDir,
		"ESL_PASSWORD": cfg.ESLPassword, "E2E_PASSWORD": cfg.Password,
	} {
		if v == "" {
			t.Fatalf("%s is required; run make e2e-capacity", name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if *flagCapacity == K3.ID {
		runRefusalTest(ctx, t, cfg)
		return
	}
	rep, err := RunCapacity(ctx, cfg, sc)
	if rep == nil {
		t.Fatalf("the run did not start: %v", err)
	}
	t.Run(sc.ID, func(t *testing.T) {
		for _, f := range rep.Checks.Failures {
			t.Error(f)
		}
	})
	for _, a := range rep.AfterRun {
		t.Run("after-run/"+a.Name, func(t *testing.T) {
			for _, f := range a.Failures {
				t.Error(f)
			}
		})
	}
	t.Logf("%s", CapacityTable(rep))
	t.Logf("run time %.0f s (latency, reported only: %v)", rep.DurationSec, rep.Latency)
	if err != nil {
		for _, f := range rep.Failures {
			if strings.HasPrefix(f, runIncomplete) {
				t.Error(f)
			}
		}
		t.Fatalf("%d failure(s); the report is %s", len(rep.Failures), filepath.Join(cfg.OutDir, "report.json"))
	}
}

// runRefusalTest is TestCapacity for K3 (issue #459). The switch's log is
// judged by TestRefusalSwitchLog, once e2e.sh has copied it out.
func runRefusalTest(ctx context.Context, t *testing.T, cfg CampaignConfig) {
	rep, err := RunRefusal(ctx, cfg, K3)
	if rep == nil {
		t.Fatalf("the run did not start: %v", err)
	}
	t.Run(K3.ID, func(t *testing.T) {
		for _, f := range rep.Checks.Failures {
			t.Error(f)
		}
	})
	for _, a := range rep.AfterRun {
		t.Run("after-run/"+a.Name, func(t *testing.T) {
			for _, f := range a.Failures {
				t.Error(f)
			}
		})
	}
	t.Logf("%s", RefusalTable(rep))
	t.Logf("run time %.0f s (latency, reported only: %v)", rep.DurationSec, rep.Latency)
	if err != nil {
		for _, f := range rep.Failures {
			if strings.HasPrefix(f, runIncomplete) {
				t.Error(f)
			}
		}
		t.Fatalf("%d failure(s); the report is %s", len(rep.Failures), filepath.Join(cfg.OutDir, "report.json"))
	}
}

// TestRefusalSwitchLog judges the switch's log against K3's report. It is run
// by e2e.sh after K3, with -e2e.out the scenario's output directory and
// -e2e.switchlog the log copied out of the switch's container.
func TestRefusalSwitchLog(t *testing.T) {
	if *flagOut == "" || *flagSwitchLog == "" {
		t.Fatal("-e2e.out and -e2e.switchlog are required; run make e2e-capacity")
	}
	rep, err := ApplySwitchLog(*flagOut, *flagSwitchLog)
	if rep != nil && rep.SwitchLog != nil {
		for _, f := range rep.SwitchLog.Checks.Failures {
			t.Error(f)
		}
		t.Logf("%s", RefusalTable(rep))
	}
	if err != nil {
		t.Fatalf("%v", err)
	}
}
