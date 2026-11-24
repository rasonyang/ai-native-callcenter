// SPDX-License-Identifier: Apache-2.0

package leadjudge

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/textllm"
)

// TestEvalLive measures the real job's prompt, parsing and retry against a
// real model. It costs money, so it needs AICC_LIVE_PROVIDER_TEST=1, and it
// needs -count=1 or Go serves a cached result:
//
//	AICC_LIVE_PROVIDER_TEST=1 AICC_TEXT_LLM_MODEL=qwen-plus ALIYUN_API_KEY=... \
//	  go test -count=1 -run TestEvalLive -v ./internal/leadjudge/
//
// Endpoint, model and key come from AICC_TEXT_LLM_ENDPOINT (default DashScope's
// compatible mode), AICC_TEXT_LLM_MODEL (default qwen-plus) and TEXT_LLM_API_KEY
// (falling back to ALIYUN_API_KEY). It is a measurement: a verdict that differs
// from the expected one never fails it; a case the job could not judge at all
// (NOT_JUDGED: a transport error or an unusable answer twice) does.
func TestEvalLive(t *testing.T) {
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") != "1" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run the live evaluation")
	}
	cfg := textllm.Config{
		Endpoint: envOr("AICC_TEXT_LLM_ENDPOINT", "https://dashscope.aliyuncs.com/compatible-mode/v1"),
		Model:    envOr("AICC_TEXT_LLM_MODEL", "qwen-plus"),
		APIKey:   envOr("TEXT_LLM_API_KEY", os.Getenv("ALIYUN_API_KEY")),
	}
	client, err := textllm.New(cfg, nil)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}

	cases := loadEvalCases(t)
	results := make([]evalResult, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runEvalCase(t, client, c)
		}()
	}
	wg.Wait()

	var report strings.Builder
	fmt.Fprintf(&report, "\nmodel %s @ %s, %d cases\n", cfg.Model, cfg.Endpoint, len(cases))
	fmt.Fprintf(&report, "%-28s %-4s %-12s %-12s %-10s %s  %s\n", "case", "lang", "expected", "got", "status", "n", "reason")
	agree, unjudged := 0, 0
	byExpect := map[campaign.LeadVerdict][2]int{}
	for i, r := range results {
		c := cases[i]
		report.WriteString(formatEvalRow(c, r))
		s := byExpect[c.Expect]
		s[1]++
		if r.got == c.Expect {
			agree++
			s[0]++
		}
		if r.got == campaign.VerdictNotJudged {
			unjudged++
		}
		byExpect[c.Expect] = s
	}
	fmt.Fprintf(&report, "\nagreement: %d/%d\n", agree, len(cases))
	for _, v := range modelVerdicts {
		s := byExpect[v]
		fmt.Fprintf(&report, "  %-12s %d/%d agree\n", v, s[0], s[1])
	}
	t.Log(report.String())
	if unjudged > 0 {
		t.Errorf("%d cases got no usable verdict (see the table)", unjudged)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
