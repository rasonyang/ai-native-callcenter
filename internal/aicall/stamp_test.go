// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// A transfer's summary and reason are the model's own words, and a caller can
// talk a model into writing anything, line breaks included. A line break in a
// channel variable would end the command on the event socket and have the
// switch read the rest as a command of its own. The stamp is flattened to one
// line, and still reaches the agent.
func TestTheModelsWordsAreStampedOnOneLine(t *testing.T) {
	t.Parallel()
	sw := &fakeSwitch{}
	actions := &callActions{
		orchestrator:  testOrchestrator(t, sw),
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		callerChannel: "caller-channel-1",
	}

	actions.stampChannel("aicc_bot_summary", "wants a refund\n\nbgapi system id\r\nok")
	got := sw.variable("aicc_bot_summary")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("summary stamped with a line break: %q", got)
	}
	if !strings.Contains(got, "wants a refund") || !strings.Contains(got, "ok") {
		t.Errorf("summary = %q, want the words kept", got)
	}

	actions.stampChannel("aicc_bot_reason", "BILLING\x00\x1b")
	if got := sw.variable("aicc_bot_reason"); strings.IndexFunc(got, func(r rune) bool { return r < ' ' }) >= 0 {
		t.Errorf("reason stamped with a control character: %q", got)
	}

	// Ordinary text, spaces, quotes and other languages are the value's own.
	const plain = "客户要求退款, it's order #40 — 谢谢"
	actions.stampChannel("aicc_bot_summary", plain)
	if got := sw.variable("aicc_bot_summary"); got != plain {
		t.Errorf("summary = %q, want %q unchanged", got, plain)
	}

	// A value that was nothing but line breaks says nothing, and is not sent.
	actions.stampChannel("aicc_bot_finished", "\n\n")
	if got := sw.variable("aicc_bot_finished"); got != "" {
		t.Errorf("an empty stamp was sent as %q", got)
	}
}
