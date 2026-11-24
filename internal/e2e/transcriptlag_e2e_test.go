// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// The transcript-lag case of issue #587 (internal/e2e/transcriptlag.go).
// e2e.sh transcriptlag runs this test in the environment's network.

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptLag(t *testing.T) {
	cfg := TranscriptLagConfig{
		API: *flagAPI, CalleeSim: *flagCalleeSim, MockProvider: *flagMockProvider,
		FlowFile: *flagFlow, Username: *flagUsername, Password: os.Getenv("E2E_PASSWORD"),
		OutDir: *flagOut, Wait: *flagWait,
	}
	for name, v := range map[string]string{
		"-e2e.api": cfg.API, "-e2e.calleesim": cfg.CalleeSim, "-e2e.mockprovider": cfg.MockProvider,
		"-e2e.flow": cfg.FlowFile, "-e2e.username": cfg.Username, "-e2e.out": cfg.OutDir,
		"E2E_PASSWORD": cfg.Password,
	} {
		if v == "" {
			t.Fatalf("%s is required; run make e2e-transcriptlag", name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := RunTranscriptLag(ctx, cfg)
	for _, c := range rep.Checks {
		t.Logf("check: %s", c)
	}
	for _, fl := range rep.Failures {
		t.Error(fl)
	}
	if err != nil {
		t.Fatalf("%v (the report is %s)", err, filepath.Join(cfg.OutDir, "report.json"))
	}
}
