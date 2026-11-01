// SPDX-License-Identifier: Apache-2.0

package wrapupdraft

// The evaluation set for the wrap-up draft (issue #138): synthetic transcripts
// with the expected disposition and a note checklist written before any model
// ran. TestEvalSetIsWellFormed always runs; TestEvalLive measures a real model.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// evalCase is one file entry. Turns are "C: text" (customer), "A: text" (the
// human agent) and "B: text" (the AI assistant).
type evalCase struct {
	ID   string `json:"id"`
	Lang string `json:"lang"`
	Type string `json:"type"`
	// ExpectCode is a code from the seeded vocabulary, or null.
	ExpectCode *string `json:"expect_code"`
	// AcceptNull also counts a null answer as agreement.
	AcceptNull bool `json:"accept_null"`
	// AcceptCode also counts this code as agreement, for a case whose honest
	// answer is either null or that code.
	AcceptCode      string `json:"accept_code"`
	NoteExpectation string `json:"note_expectation"`
	// Agent is the segment of the agent whose draft this is.
	Agent []string `json:"agent"`
	// Bot is the AI phase; HandoffSummary the transfer_to_agent summary.
	Bot            []string `json:"bot"`
	HandoffSummary string   `json:"handoff_summary"`
	// Other is another agent's segment on the same call (consult, transfer).
	Other []string `json:"other"`
	// Pad is a block of turns repeated Times before Agent, with {n} replaced
	// by the repetition number: a long call.
	Pad *struct {
		Times int      `json:"times"`
		Turns []string `json:"turns"`
	} `json:"pad"`
	// ExpectTrim says the rendered prompt must have dropped earlier lines.
	ExpectTrim bool `json:"expect_trim"`
	// Must: every group needs one of its alternatives in the note.
	Must [][]string `json:"must"`
	// MustNot: no alternative of any group may appear in the note.
	MustNot [][]string `json:"must_not"`
}

// seededDispositions is the vocabulary migration 00012 installs.
var seededDispositions = []store.Disposition{
	{Code: "RESOLVED", Label: "Resolved"},
	{Code: "FOLLOW_UP_REQUIRED", Label: "Follow-up Required"},
	{Code: "NO_ANSWER", Label: "No Answer"},
	{Code: "OTHER", Label: "Other"},
}

func loadEvalCases(t testing.TB) []evalCase {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "eval", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no evaluation files: %v", err)
	}
	sort.Strings(files)
	var all []evalCase
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var cs []evalCase
		if err := json.Unmarshal(raw, &cs); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		all = append(all, cs...)
	}
	return all
}

// transcript builds the ledger lines of a case as the tap writes them, and the
// agent id whose draft it is.
func (c evalCase) transcript(t testing.TB) ([]store.TranscriptLine, uuid.UUID) {
	t.Helper()
	agent, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	agentParty, otherParty := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var out []store.TranscriptLine
	add := func(turn string, party, agentID *uuid.UUID) {
		who, text, ok := strings.Cut(turn, ": ")
		if !ok {
			t.Fatalf("%s: bad turn %q", c.ID, turn)
		}
		speaker := ""
		switch who {
		case "C":
			speaker = store.SpeakerCustomer
		case "A":
			speaker = store.SpeakerHumanAgent
		case "B":
			speaker = store.SpeakerBot
		default:
			t.Fatalf("%s: bad speaker in %q", c.ID, turn)
		}
		var aid *uuid.UUID
		if speaker == store.SpeakerHumanAgent {
			aid = agentID
		}
		out = append(out, store.TranscriptLine{
			Seq: len(out) + 1, Speaker: speaker, Kind: store.TranscriptKindText,
			Content: map[string]any{"text": text}, PartyID: party, AgentID: aid,
		})
	}
	for _, turn := range c.Bot {
		add(turn, nil, nil)
	}
	if c.HandoffSummary != "" {
		args, _ := json.Marshal(map[string]string{"summary": c.HandoffSummary})
		out = append(out, store.TranscriptLine{
			Seq: len(out) + 1, Speaker: store.SpeakerBot, Kind: store.TranscriptKindToolCall,
			Content: map[string]any{"name": flow.ToolTransferToAgent, "args": string(args)},
		})
	}
	for _, turn := range c.Other {
		add(turn, &otherParty, &other)
	}
	if c.Pad != nil {
		for n := 1; n <= c.Pad.Times; n++ {
			for _, turn := range c.Pad.Turns {
				add(strings.ReplaceAll(turn, "{n}", fmt.Sprint(n)), &agentParty, &agent)
			}
		}
	}
	for _, turn := range c.Agent {
		add(turn, &agentParty, &agent)
	}
	return out, agent
}

func TestEvalSetIsWellFormed(t *testing.T) {
	cases := loadEvalCases(t)
	if len(cases) < 40 {
		t.Fatalf("the evaluation set has %d cases, the issue asks for at least 40", len(cases))
	}
	codes := map[string]bool{}
	for _, d := range seededDispositions {
		codes[d.Code] = true
	}
	ids := map[string]bool{}
	langs := map[string]int{}
	types := map[string]int{}
	for _, c := range cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			if ids[c.ID] {
				t.Fatalf("duplicate id %q", c.ID)
			}
			ids[c.ID] = true
			if c.Lang != "en" && c.Lang != "zh" {
				t.Errorf("lang %q", c.Lang)
			}
			langs[c.Lang]++
			types[c.Type]++
			if c.Type == "" {
				t.Error("no type")
			}
			if c.AcceptCode != "" && !codes[c.AcceptCode] {
				t.Errorf("accepted code %q is not in the seeded vocabulary", c.AcceptCode)
			}
			if c.ExpectCode != nil && !codes[*c.ExpectCode] {
				t.Errorf("expected code %q is not in the seeded vocabulary", *c.ExpectCode)
			}
			for _, g := range append(slices.Clone(c.Must), c.MustNot...) {
				if len(g) == 0 {
					t.Error("an empty checklist group")
				}
			}
			if c.ExpectCode == nil && len(c.Must) == 0 && c.Type != "nothing_business" {
				t.Error("a null case with no checklist must be a nothing_business case")
			}
			if len(c.MustNot) > 0 && len(c.Must) == 0 {
				t.Error("must_not without must")
			}
			lines, agent := c.transcript(t)
			seg := split(lines, agent, uuid.Nil)
			if len(seg.agent) == 0 {
				t.Fatal("the agent segment is empty after split")
			}
			if (len(c.Bot) > 0) != (len(seg.bot) > 0) {
				t.Errorf("bot lines: case has %d, split found %d", len(c.Bot), len(seg.bot))
			}
			if (c.HandoffSummary != "") != (seg.summary != "") {
				t.Errorf("handoff summary lost in split: %q", seg.summary)
			}
			if len(c.Other) > 0 {
				for _, l := range seg.agent {
					for _, o := range c.Other {
						if strings.HasSuffix(o, l.text) && l.text != "" {
							t.Errorf("another agent's line leaked into the segment: %q", l.text)
						}
					}
				}
			}
			prompt, ok := render(seg, seededDispositions, DefaultBudgetChars)
			if !ok {
				t.Fatal("render does not fit the default budget")
			}
			if trimmed := strings.Contains(prompt, omittedMarker); trimmed != c.ExpectTrim {
				t.Errorf("trimmed = %v, case expects %v", trimmed, c.ExpectTrim)
			}
			if c.ExpectTrim && c.Pad == nil {
				t.Error("expect_trim without pad")
			}
		})
	}
	if langs["zh"] < 15 || langs["en"] < 15 {
		t.Errorf("language balance: %v", langs)
	}
	for _, want := range []string{"ambiguous", "topic_change", "self_correction", "nothing_business", "asr_noise",
		"mixed", "short", "long", "callback", "wrong_number", "bot_handoff", "consult"} {
		if types[want] == 0 {
			t.Errorf("no case of type %q", want)
		}
	}
}

// The language of the note is decided by the code, and the evaluation set's own
// language labels are the check: every case must get the language it is
// labelled with, the mixed English-led case included.
func TestEvalSetGetsItsOwnNoteLanguage(t *testing.T) {
	for _, c := range loadEvalCases(t) {
		lines, agent := c.transcript(t)
		if got := noteLanguage(split(lines, agent, uuid.Nil).agent); got != c.Lang {
			t.Errorf("%s: note language %q, case is %q", c.ID, got, c.Lang)
		}
	}
}
