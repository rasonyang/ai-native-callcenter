// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The campaign tests of issues #245 and #246. They need the environment of deploy/e2e
// and run inside its internal network: e2e.sh builds this file into a static
// test binary (go test -c -tags e2e), puts it in the harness image and runs it
// with docker compose run. `make e2e-campaign` is the way to run them.

package e2e

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	flagAPI          = flag.String("e2e.api", "", "application REST base, e.g. http://10.1.2.10:8080")
	flagReady        = flag.String("e2e.ready", "", "application readiness URL")
	flagCalleeSim    = flag.String("e2e.calleesim", "", "callee simulator report base")
	flagMockProvider = flag.String("e2e.mockprovider", "", "mock provider HTTP base, for its session counters")
	flagMetrics      = flag.String("e2e.metrics", "", "application metrics exposition, e.g. http://10.1.2.10:9090/metrics")
	flagProviderScn  = flag.String("e2e.provider-scenario", "", "the mock provider's scenario file")
	flagESL          = flag.String("e2e.esl", "", "switch event socket address")
	flagCompose      = flag.String("e2e.compose-config", "", "resolved compose configuration (JSON)")
	flagScenario     = flag.String("e2e.scenario", "", "the simulator's scenario file")
	flagFlow         = flag.String("e2e.flow", "", "flow spec file")
	flagUsername     = flag.String("e2e.username", "", "administrator login")
	flagOut          = flag.String("e2e.out", "", "run directory for report.json, probes.md and bundles/")
	flagWait         = flag.Duration("e2e.wait", 4*time.Minute, "bound for each phase of the run")
)

func TestCampaigns(t *testing.T) {
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
			t.Fatalf("%s is required; run make e2e-campaign", name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := RunCampaigns(ctx, cfg)
	if rep == nil {
		t.Fatalf("the run did not start: %v", err)
	}

	// One subtest per region and case, so the output names what failed.
	for _, sc := range rep.Scenarios {
		t.Run(sc.Region+"/"+sc.ID, func(t *testing.T) {
			for _, a := range sc.Attempts {
				t.Logf("attempt %d: %s %s, outcome %s, cause %s, source %s", a.Index, a.CallID, a.Number, a.Outcome, a.HangupCause, a.Source)
			}
			for _, f := range sc.Failures {
				t.Error(f)
			}
		})
	}
	for _, group := range []struct {
		name string
		list CheckList
	}{{"run", rep.Run}, {"cross-region", rep.CrossRegion}} {
		t.Run(group.name, func(t *testing.T) {
			for _, f := range group.list.Failures {
				t.Error(f)
			}
		})
	}
	for _, a := range rep.AfterRun {
		t.Run("after-run/"+a.Name, func(t *testing.T) {
			for _, f := range a.Failures {
				t.Error(f)
			}
		})
	}

	t.Logf("measured table (dial form %s):%s", rep.DialForm, ProbeTable(rep.Probes))
	t.Logf("%s", FirstAudioTable(rep.FirstAudio))
	t.Logf("%s", SwitchClockTable(rep.Scenarios))
	if len(rep.Findings) > 0 {
		t.Logf("findings:\n  %s", strings.Join(rep.Findings, "\n  "))
	}
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
