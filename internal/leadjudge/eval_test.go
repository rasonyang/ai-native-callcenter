// SPDX-License-Identifier: Apache-2.0

package leadjudge

// The evaluation set for the lead judge (#661, "Test data and verification"):
// synthetic transcripts written for these tests, never taken from a
// deployment, each with the verdict expected of it. TestEvalSetIsWellFormed
// always runs; TestEvalLive measures a real model on demand.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/textllm"
)

// evalCase is one file entry. Turns are "B: text" (the bot), "C: text" (the
// callee) and "T: name args" (a tool call, which the judge must never see).
type evalCase struct {
	ID   string `json:"id"`
	Lang string `json:"lang"`
	Type string `json:"type"`
	Lead struct {
		Intent campaign.LeadIntent `json:"intent"`
		Notes  string              `json:"notes"`
	} `json:"lead"`
	Turns  []string             `json:"turns"`
	Expect campaign.LeadVerdict `json:"expect"`
}

func loadEvalCases(t *testing.T) []evalCase {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "eval", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no evaluation files: %v", err)
	}
	var all []evalCase
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var cases []evalCase
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		all = append(all, cases...)
	}
	return all
}

// transcript turns the case into ledger rows, tool calls included.
func (c evalCase) transcript(t *testing.T) []store.TranscriptLine {
	t.Helper()
	var lines []store.TranscriptLine
	for i, turn := range c.Turns {
		who, said, ok := strings.Cut(turn, ": ")
		if !ok {
			t.Fatalf("%s turn %d: %q has no speaker", c.ID, i, turn)
		}
		switch who {
		case "B":
			lines = append(lines, bot(said))
		case "C":
			lines = append(lines, callee(said))
		case "T":
			name, args, _ := strings.Cut(said, " ")
			lines = append(lines,
				store.TranscriptLine{Speaker: store.SpeakerBot, Kind: store.TranscriptKindToolCall,
					Content: map[string]any{"name": name, "args": args}},
				store.TranscriptLine{Speaker: store.SpeakerBot, Kind: store.TranscriptKindToolResult,
					Content: map[string]any{"name": name, "output": `{"ok":"1"}`}})
		default:
			t.Fatalf("%s turn %d: unknown speaker %q", c.ID, i, who)
		}
	}
	return lines
}

// fake is the fake store holding one case.
func (c evalCase) fake(t *testing.T, deadlineIn time.Duration) *fakeStore {
	st := newFakeStore(c.Lead.Intent, c.Lang, deadlineIn, c.transcript(t)...)
	st.j.LeadNotes = c.Lead.Notes
	return st
}

// The set covers what the issue's test list asks for, in both languages, and
// every case can be judged: a valid lead and verdict, a transcript that
// renders, tool rows that never reach the prompt, and a call with no callee
// words decided in code.
func TestEvalSetIsWellFormed(t *testing.T) {
	t.Parallel()
	cases := loadEvalCases(t)
	seen := map[string]bool{}
	byLang := map[string]map[campaign.LeadVerdict]int{"zh": {}, "en": {}}
	for _, c := range cases {
		if seen[c.ID] {
			t.Errorf("%s: duplicate id", c.ID)
		}
		seen[c.ID] = true
		if byLang[c.Lang] == nil {
			t.Errorf("%s: language %q is neither zh nor en", c.ID, c.Lang)
			continue
		}
		if !campaign.IsLeadJudged(c.Lead.Intent) {
			t.Errorf("%s: lead %q is not one that is judged", c.ID, c.Lead.Intent)
		}
		valid := false
		for _, v := range modelVerdicts {
			valid = valid || v == c.Expect
		}
		if !valid {
			t.Errorf("%s: expected verdict %q is not one the model may give", c.ID, c.Expect)
		}
		byLang[c.Lang][c.Expect]++

		in := inputOf(c.transcript(t))
		if !hasCallee(in) {
			if c.Expect != campaign.VerdictNoEvidence {
				t.Errorf("%s: no callee line, so NO_EVIDENCE in code, but %s is expected", c.ID, c.Expect)
			}
			st := c.fake(t, time.Minute)
			llm := &fakeLLM{replies: []func(context.Context) (string, error){says(`{"verdict":"CONFIRMED"}`)}}
			v, _, n := newTestService(st, llm).decide(context.Background(), quiet(), st.j)
			if v != campaign.VerdictNoEvidence || n != 0 {
				t.Errorf("%s: decided %s with %d requests, want NO_EVIDENCE with none", c.ID, v, n)
			}
			continue
		}
		_, user, ok := render(in, c.Lead.Intent, c.Lead.Notes, languageOf(c.Lang, in), DefaultBudgetChars)
		if !ok {
			t.Errorf("%s: does not render", c.ID)
		}
		for _, turn := range c.Turns {
			if tool, isTool := strings.CutPrefix(turn, "T: "); isTool {
				name, args, _ := strings.Cut(tool, " ")
				if strings.Contains(user, name) || strings.Contains(user, args) {
					t.Errorf("%s: the prompt carries the tool row %q", c.ID, tool)
				}
			}
		}
	}
	for lang, counts := range byLang {
		for _, v := range modelVerdicts {
			if counts[v] == 0 {
				t.Errorf("%s: no case expects %s", lang, v)
			}
		}
	}
	for _, id := range []string{
		"clear-yes", "question-then-yes", "polite-refusal", "deferral", "voicemail", "call-assistant",
		"acks-only", "no-callee", "other-script", "agree-then-retract",
	} {
		for _, lang := range []string{"zh", "en"} {
			if !seen[lang+"-"+id] {
				t.Errorf("the issue's case %s-%s is missing", lang, id)
			}
		}
	}
}

// evalResult is one case's outcome against a real model.
type evalResult struct {
	got      campaign.LeadVerdict
	reason   string
	requests int
}

// runEvalCase runs the production decision on the case: the same filtering,
// prompt, budget, parsing and one retry the job makes.
func runEvalCase(t *testing.T, client textllm.Client, c evalCase) evalResult {
	st := c.fake(t, time.Minute)
	s := New(st, client, Config{Timeout: DefaultTimeout * 3, BudgetChars: DefaultBudgetChars}, quiet())
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout*3)
	defer cancel()
	v, reason, n := s.decide(ctx, quiet(), st.j)
	return evalResult{got: v, reason: reason, requests: n}
}

func formatEvalRow(c evalCase, r evalResult) string {
	status := "AGREE"
	switch {
	case r.got == campaign.VerdictNotJudged:
		status = "NOT_JUDGED"
	case r.got != c.Expect:
		status = "DIFF"
	}
	return fmt.Sprintf("%-28s %-4s %-12s %-12s %-10s %d  %s\n", c.ID, c.Lang, c.Expect, r.got, status,
		r.requests, strings.ReplaceAll(r.reason, "\n", " "))
}
