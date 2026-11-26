// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// #635. A callee whose every turn is an acknowledgement said over the bot —
// each one held out of the transcript — once kept the call open until three
// silences or the duration cap: the acknowledgement was nothing the flow
// counted. OnCallerAcknowledged now counts those turns toward closingTarget
// the way OnNoInput counts silence.
//
// A run of acknowledgement-only caller turns ends the call at
// defaultAckOnlyLimit.
func TestAckOnlyDefaultFiresAfterARunOfAcknowledgements(t *testing.T) {
	t.Parallel()
	e := testEngineFor(t, testFlowWithClosing, "en")
	toReport(t, e)

	for i := range defaultAckOnlyLimit - 1 {
		if moved := e.OnCallerAcknowledged(); moved != "" {
			t.Fatalf("acknowledgement %d moved to %q, want none yet", i+1, moved)
		}
	}
	if moved := e.OnCallerAcknowledged(); moved != "farewell" {
		t.Fatalf("acknowledgement %d moved to %q, want the closing target", defaultAckOnlyLimit, moved)
	}
}

// Words, a keypress or a tool the conversation led to is the caller making
// progress, and any of them starts the run over — the same resets
// noInput.count has.
func TestAckOnlyRunResetsOnProgress(t *testing.T) {
	t.Parallel()
	e := testEngineFor(t, testFlowWithClosing, "en")
	toReport(t, e)

	acks := func(n int) {
		t.Helper()
		for i := range n {
			if moved := e.OnCallerAcknowledged(); moved != "" {
				t.Fatalf("acknowledgement %d moved to %q", i+1, moved)
			}
		}
	}
	acks(defaultAckOnlyLimit - 1)
	e.OnCallerSpoke("an actual answer")
	acks(defaultAckOnlyLimit - 1)
	e.OnCallerKeyed()
	acks(defaultAckOnlyLimit - 1)
	e.OnToolResult("note", nil, map[string]any{"ok": "1"})
	acks(defaultAckOnlyLimit - 1)
	if moved := e.OnCallerAcknowledged(); moved != "farewell" {
		t.Fatalf("acknowledgement %d moved to %q, want the closing target", defaultAckOnlyLimit, moved)
	}
}

// Without a closing target there is nowhere to send the call — the behaviour
// before this bound existed, not a new failure mode.
func TestAckOnlyDefaultDoesNothingWithoutAClosingTarget(t *testing.T) {
	t.Parallel()
	e := testEngine(t, "en") // plain testFlow: no closingTarget
	toReport(t, e)

	for i := range defaultAckOnlyLimit + 2 {
		if moved := e.OnCallerAcknowledged(); moved != "" {
			t.Fatalf("acknowledgement %d moved to %q with no closing target", i+1, moved)
		}
	}
	if got := e.countOf("ackOnly.count"); got != defaultAckOnlyLimit+2 {
		t.Errorf("ackOnly.count = %d, want %d counted even without a target", got, defaultAckOnlyLimit+2)
	}
}

// A terminal phase has already said its goodbye: acknowledgements after it
// must not move the call again.
func TestAckOnlyDefaultNeverLeavesATerminalPhase(t *testing.T) {
	t.Parallel()
	e := testEngineFor(t, testFlowWithClosing, "en")
	toReport(t, e)
	silences(t, e, defaultNoInputLimit-1)
	if moved := e.OnNoInput(); moved != "farewell" {
		t.Fatalf("the call did not reach its terminal phase: %q", moved)
	}
	for range defaultAckOnlyLimit + 1 {
		if moved := e.OnCallerAcknowledged(); moved != "" {
			t.Fatalf("an acknowledgement moved a terminal phase to %q", moved)
		}
	}
}

// The acknowledgement refusal names no answer word as the example of "not an
// answer" — that text is what taught the model to discount every later 「好的」
// — and it tells the model to ask its question again, with a short 「好的」 or
// 「好」 counting as the answer (#635).
func TestTheAcknowledgementRefusalNamesNoAnswerAsTheExample(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, lang := range []string{"zh", "en"} {
		spec, err := Load([]byte(denyFlow))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		r := NewRuntime(NewEngine(spec, lang, nil, log), &fakeActions{}, NewBackend(""),
			[]string{"support-en"}, log)
		out := r.RefuseAsAcknowledgement("classify", false)
		for _, absent := range []string{"mm-hmm", "“嗯”", "“好的”),并", "(a short"} {
			if strings.Contains(out, absent) {
				t.Errorf("%s refusal %q offers %q as the example of not-an-answer", lang, out, absent)
			}
		}
		if lang == "zh" {
			for _, want := range []string{"只是应了一声", "再问一遍", "“好的”或“好”也算回答"} {
				if !strings.Contains(out, want) {
					t.Errorf("zh refusal %q lacks %q", out, want)
				}
			}
		} else {
			for _, want := range []string{"cut the bot short", "Put the question to them again", "their answer to it counts", "included"} {
				if !strings.Contains(out, want) {
					t.Errorf("en refusal %q lacks %q", out, want)
				}
			}
		}
	}
}
