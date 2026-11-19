// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// #612: a tool call that would record the customer's interest is judged
// before it runs, by the timing of the caller words it answers (A) and by
// their final transcript (B). Each test feeds the provider events in the order
// a recorded call delivered them.

// leadGuardFlow is a campaign offer whose classification moves into a phase
// that declares a HIGH lead on "agree" and into one that declares LOW on
// "decline"; note is a slot tool that moves nothing.
const leadGuardFlow = `{
	"id": "lead-guard",
	"specVersion": "v2",
	"initialNode": "offer",
	"global": {"persona": "You renew plans.", "closingTarget": "bye"},
	"tools": {
		"classify": {"kind": "slot", "slot": "answer", "description": "Record the decision.",
			"enum": ["agree", "decline"]},
		"note": {"kind": "slot", "slot": "note", "description": "Record a note."}
	},
	"nodes": {
		"offer": {
			"instruction": "Offer the renewal.",
			"tools": ["classify", "note", "hangup"],
			"isFlowHangupAllowed": true,
			"transitions": [
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "agree"}, "target": "agreed"},
				{"on": "TOOL_RESULT", "tool": "classify", "condition": {"slot": "answer", "op": "EQ", "value": "decline"}, "target": "declined"},
				{"on": "TOOL_RESULT", "tool": "hangup", "target": "bye"}
			]
		},
		"agreed": {"instruction": "Tell them how to order.", "tools": ["note"],
			"lead": {"intent": "HIGH", "notes": "Agreed to renew."}},
		"declined": {"instruction": "Thank them.", "tools": ["note"],
			"lead": {"intent": "LOW", "notes": "Declined."}},
		"bye": {"instruction": "Close.", "announce": {"en": "Goodbye.", "zh": "再见。"}, "tools": [], "isTerminal": true}
	}
}`

const leadGuardLine = "a tool that would record the customer's interest was judged"

// guardLeads is a LeadSink that records what was written.
type guardLeads struct {
	mu     sync.Mutex
	writes []flow.LeadRequest
}

func (l *guardLeads) SetLead(_ context.Context, _ uuid.UUID, intent campaign.LeadIntent, notes string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes = append(l.writes, flow.LeadRequest{Intent: string(intent), Notes: notes})
	return true, nil
}

func (l *guardLeads) intents() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, w := range l.writes {
		out = append(out, w.Intent)
	}
	return out
}

// leadRig is a campaign call on leadGuardFlow through drive, with the final
// wait, the cut's verdict wait and the replay's backstop on clocks the test
// owns.
type leadRig struct {
	*claimsHarness
	finalWait *verdictWait
	cut       *verdictWait
	replay    *holdClock
	leads     *guardLeads
	// hold is the model on an engine whose client holds speech (openai).
	hold *holdModel
	// quiet is the model when the rig answers quietly.
	quiet *quietModel
	// opening is the call's opening line, as written (leadRigOptions.opening).
	opening string
}

type leadRigOptions struct {
	profile provider.Profile
	lang    string
	// opening is the call's opening line; empty for none.
	opening string
	// flow replaces leadGuardFlow.
	flow string
	// isQuiet gives the model quiet tool answers (provider.QuietToolAnswerer).
	isQuiet bool
}

func newLeadRig(t *testing.T, opts leadRigOptions) *leadRig {
	t.Helper()
	if opts.profile.Name == "" {
		opts.profile = provider.QwenProfile()
	}
	if opts.lang == "" {
		opts.lang = "zh"
	}
	if opts.flow == "" {
		opts.flow = leadGuardFlow
	}
	r := &leadRig{finalWait: newVerdictWait(), cut: newVerdictWait(), replay: &holdClock{}, leads: &guardLeads{},
		opening: opts.opening}
	wrap := func(m *fakeModel) provider.VoiceSession {
		switch {
		case opts.profile.ControlsInterruption:
			r.hold = &holdModel{fakeModel: m}
			return r.hold
		case opts.isQuiet:
			r.quiet = &quietModel{fakeModel: m}
			return r.quiet
		}
		return m
	}
	r.claimsHarness = newClaimsHarnessModel(t, opts.profile, opts.flow, opts.lang, "", quietConfig(), wrap,
		func(h *claimsHarness) {
			h.session.finalWait = r.finalWait.after
			h.session.cutWait = r.cut.after
			h.session.holdAfter = r.replay.after
			h.session.cfg.OnCutAcknowledged = h.actions.onCutAcknowledged
			h.session.cfg.IsEndingArmed = h.actions.isArmed
			h.session.cfg.Session.Language = opts.lang
			h.session.cfg.Session.OpeningText = opts.opening
			// What runCall does for a campaign call.
			h.actions.campaignID, h.actions.attemptID = "c-1", "a-1"
			h.o.cfg.Leads = r.leads
			h.runtime.Deny(campaignDeniedTools...)
			h.runtime.OfferRecordLead(t.Context())
		})
	return r
}

func (r *leadRig) send(e provider.Event) { r.model.events <- e }

func (r *leadRig) speechStarted(item string) {
	r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item})
}

func (r *leadRig) speechStopped(item string) {
	r.send(provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: item})
}

// callerSpoke is one caller utterance the engine detected, start and end.
func (r *leadRig) callerSpoke(item string) {
	r.speechStarted(item)
	r.speechStopped(item)
}

func (r *leadRig) final(item, text string) {
	r.send(provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: text, IsFinal: true})
}

func (r *leadRig) started() { r.send(provider.Event{Type: provider.EventTypeResponseStarted}) }

func (r *leadRig) speaks(text string) {
	r.send(provider.Event{Type: provider.EventTypeOutputTranscript, Text: text, IsFinal: true})
	r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples)})
}

func (r *leadRig) call(id, tool, args string) {
	r.send(provider.Event{Type: provider.EventTypeToolCall, ToolCallID: id, ToolName: tool, ToolArgs: args})
}

func (r *leadRig) agree(id string) { r.call(id, "classify", `{"value":"agree"}`) }

func (r *leadRig) ended() { r.send(provider.Event{Type: provider.EventTypeResponseDone}) }

// awaitFinalWaits waits until the guard has started n waits for a final.
func (r *leadRig) awaitFinalWaits(t *testing.T, n int) {
	t.Helper()
	waitFor(t, "the guard to wait for the final", func() bool { return len(r.finalWait.waits()) >= n })
}

// result waits for the n-th tool answer (from 1) and returns its output.
func (r *leadRig) result(t *testing.T, n int) string {
	t.Helper()
	waitFor(t, "the tool call to be answered", func() bool { return len(r.model.recordedToolResults()) >= n })
	return r.model.recordedToolResults()[n-1].output
}

func (r *leadRig) node() string { return r.runtime.Engine().NodeID() }

// guardLines are the guard's decision lines so far.
func (r *leadRig) guardLines() []string {
	var lines []string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, leadGuardLine) {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertLine checks the n-th decision line (from 1) carries every field.
func (r *leadRig) assertLine(t *testing.T, n int, fields ...string) {
	t.Helper()
	lines := r.guardLines()
	if len(lines) < n {
		t.Fatalf("%d decision lines, want at least %d:\n%s", len(lines), n, r.logs.String())
	}
	for _, field := range fields {
		if !strings.Contains(lines[n-1], field) {
			t.Errorf("decision line %q lacks %s", lines[n-1], field)
		}
	}
}

// assertRefused checks a refusal as a conversation that names the reason,
// with the call still in its offer and no lead written.
func (r *leadRig) assertRefused(t *testing.T, out, reason string) {
	t.Helper()
	if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, reason) {
		t.Errorf("tool result = %s, want a refusal saying %q", out, reason)
	}
	if r.node() != "offer" {
		t.Errorf("the call moved to %q", r.node())
	}
	if got := r.leads.intents(); len(got) != 0 {
		t.Errorf("leads written = %v, want none", got)
	}
}

// assertAgreed checks the agreement ran: the call is in agreed and HIGH was
// written once.
func (r *leadRig) assertAgreed(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, `"ok":"1"`) || r.node() != "agreed" {
		t.Errorf("the agreement did not run: the call is in %q, tool result %s", r.node(), out)
	}
	if got := r.leads.intents(); !slices.Equal(got, []string{"HIGH"}) {
		t.Errorf("leads written = %v, want HIGH", got)
	}
}

// probe has a later response call a tool that moves nothing and waits for its
// answer: everything sent before it has been handled.
func (r *leadRig) probe(t *testing.T) {
	t.Helper()
	before := len(r.model.recordedToolResults())
	r.started()
	r.call("fc_probe", "note", `{"value":"probe"}`)
	r.ended()
	r.result(t, before+1)
}

//
// The issue's cases.
//

// 1. The opening played in full and ended in a question; the callee answers
// "不要不要。", the agreement tool arrives first and the final 300 ms after it,
// while the call waits: refused, no lead.
func TestARefusalThatLandsAfterTheAgreementToolRefusesIt(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.callerSpoke("u1")
	r.started()
	r.speaks("好的，我帮您办理。")
	r.agree("fc_1")
	r.awaitFinalWaits(t, 1)
	r.final("u1", "不要不要。")
	r.ended()
	r.assertRefused(t, r.result(t, 1), "只是拒绝")
	r.assertLine(t, 1, "tool=classify", "guard=B", "verdict=DECLINED", "isRefused=true",
		"final=AFTER_CALL", "waitedMs=", "callerTurn=1")
	if strings.Contains(r.guardLines()[0], "不要") {
		t.Error("the decision line carries the caller's words")
	}
	if got := r.finalWait.waits(); !slices.Equal(got, []time.Duration{leadFinalWait}) {
		t.Errorf("waits = %v, want one of %v", got, leadFinalWait)
	}
	// The model is told again: a second agreement on the same refusal is
	// refused again, without waiting.
	r.started()
	r.agree("fc_2")
	r.ended()
	r.assertRefused(t, r.result(t, 2), "只是拒绝")
	if got := len(r.finalWait.waits()); got != 1 {
		t.Errorf("%d waits, want the first only", got)
	}
}

// 2. "好的。" runs, and the phase's HIGH lead is written, whether the final
// came before the call or during its wait.
func TestAnAgreementRunsAndRecordsTheHighLead(t *testing.T) {
	t.Run("final before the call", func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{})
		r.callerSpoke("u1")
		r.final("u1", "好的。")
		r.started()
		r.agree("fc_1")
		r.ended()
		r.assertAgreed(t, r.result(t, 1))
		r.assertLine(t, 1, "guard=B", "verdict=WORDS", "isRefused=false", "final=BEFORE_CALL", "waitedMs=0")
		if got := len(r.finalWait.waits()); got != 0 {
			t.Errorf("%d waits for a final that was in", got)
		}
	})
	t.Run("final during the wait", func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{})
		r.callerSpoke("u1")
		r.started()
		r.agree("fc_1")
		r.awaitFinalWaits(t, 1)
		r.final("u1", "好的。")
		r.ended()
		r.assertAgreed(t, r.result(t, 1))
		r.assertLine(t, 1, "verdict=WORDS", "final=AFTER_CALL")
	})
}

// 3. No final within the wait: refused once, the model asked to have the
// callee confirm; the next guarded call answering the same caller turn, in a
// later response, runs without waiting again.
func TestNoFinalRefusesOnceAndTheNextCallOfTheCallerTurnRuns(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.callerSpoke("u1")
	r.started()
	r.agree("fc_1")
	r.awaitFinalWaits(t, 1)
	r.finalWait.expire()
	r.ended()
	r.assertRefused(t, r.result(t, 1), "没有听清")
	r.assertLine(t, 1, "verdict=NO_FINAL", "isRefused=true", "final=NONE")

	// The turn the refusal asked for calls the tool again.
	r.started()
	r.agree("fc_2")
	r.ended()
	r.assertAgreed(t, r.result(t, 2))
	r.assertLine(t, 2, "verdict=NO_FINAL", "isRefused=false", "callerTurn=1")
	if got := len(r.finalWait.waits()); got != 1 {
		t.Errorf("%d waits, want one", got)
	}
}

// 4. A blank final is as no final.
func TestABlankFinalIsAsNoFinal(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.callerSpoke("u1")
	r.final("u1", "")
	r.started()
	r.agree("fc_1")
	r.ended()
	r.assertRefused(t, r.result(t, 1), "没有听清")
	r.assertLine(t, 1, "verdict=BLANK_FINAL", "isRefused=true", "final=BEFORE_CALL")
	r.started()
	r.agree("fc_2")
	r.ended()
	r.assertAgreed(t, r.result(t, 2))
}

// openingGenerated makes the call's opening (r.opening: heldOpeningLine, two
// sentences, the second the offer, or twoPartOpeningLine), 11 s long
// (heldOpeningFrames), and queues all of it.
func (r *leadRig) openingGenerated(t *testing.T) (frameBytes int) {
	t.Helper()
	r.leg.holdFrames(1 << 20)
	frameBytes = 2 * media.RateProviderOut / 50
	r.started()
	for sent := 0; sent < heldOpeningFrames; sent += 50 {
		r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 50*frameBytes)})
	}
	r.send(provider.Event{Type: provider.EventTypeOutputTranscript, Text: r.opening, IsFinal: true})
	r.ended()
	waitLong(t, "the opening to be queued", func() bool {
		r.session.mu.Lock()
		defer r.session.mu.Unlock()
		return r.session.turnSeq == 1 && !r.session.isResponding && r.session.framesQueued >= heldOpeningFrames-2
	})
	return frameBytes
}

// heard leaves the callee having heard frames of what is queued.
func (r *leadRig) heard(frames int) {
	r.session.mu.Lock()
	queued := r.session.framesQueued
	r.session.mu.Unlock()
	r.leg.holdFrames(queued - frames)
}

// cutOpeningWithWords plays the opening, has the callee's speech cut it once
// they have heard heard frames, the engine end the speech and its reply start
// to speak; the reply's tool call is left to the test.
func (r *leadRig) cutOpeningWithWords(t *testing.T, heard int, words string) {
	t.Helper()
	frameBytes := r.openingGenerated(t)
	r.heard(heard)
	r.speechStarted("u1")
	waitFor(t, "the cut", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil
	})
	r.leg.holdFrames(0)
	r.speechStopped("u1")
	r.started()
	r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 5*frameBytes)})
	r.final("u1", words)
}

// 5. Words ("好吧。", judged WORDS: no cut refusal and, before #612, no replay)
// cut the opening before its final clause, as on production calls at 1.1 to
// 3.4 s of play, and the reply calls the agreement: the callee has not heard
// the offer, so it is refused and the opening is said again, the reply muted.
// Words in the final clause (#598), the question, are an answer to it: the
// agreement runs.
func TestWordsCuttingTheOpeningEarlyRefuseTheAgreementAndSayItAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		heard int
	}{
		{"1.1 s in", 55},
		{"3.0 s in", 150},
		{"3.4 s in", 170},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLeadRig(t, leadRigOptions{opening: heldOpeningLine})
			r.cutOpeningWithWords(t, tc.heard, "好吧。")
			r.agree("fc_1")
			out := r.result(t, 1)
			waitFor(t, "the opening to be said again", func() bool { return len(r.model.spokenLines()) >= 1 })
			r.ended()
			r.assertRefused(t, out, "开场白会再对他说一遍")
			if got := r.model.spokenLines(); !slices.Equal(got, []string{heldOpeningLine}) {
				t.Errorf("spoken = %v, want the opening once, as written", got)
			}
			r.session.mu.Lock()
			muted := r.session.mutedTurn
			r.session.mu.Unlock()
			if muted != 2 {
				t.Errorf("muted turn = %d, want the reply that made the call (2)", muted)
			}
			r.assertLine(t, 1, "guard=A", "verdict=OPENING_UNHEARD", "isRefused=true", "isOpeningSaidAgain=true")
			if line := r.cutDecisionLine(t); !strings.Contains(line, "verdict=WORDS") || !strings.Contains(line, "isRefused=false") {
				t.Errorf("the cut's own decision = %q, want words, not refused", line)
			}
			if got := len(r.finalWait.waits()); got != 0 {
				t.Errorf("guard A waited %d times for a final", got)
			}
		})
	}
	t.Run("in the final clause", func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{opening: heldOpeningLine})
		r.cutOpeningWithWords(t, heldOpeningFrames-50, "好的，要的。") // 1 s of 11 s left: in "请问您需要吗？"
		r.agree("fc_1")
		r.ended()
		r.assertAgreed(t, r.result(t, 1))
		r.assertLine(t, 1, "guard=B", "verdict=WORDS")
		r.probe(t)
		if got := r.model.spokenLines(); len(got) != 0 {
			t.Errorf("said %v, want no line", got)
		}
	})
}

// cutDecisionLine is the cut refusals' own line about the tool (#403, #610).
func (r *leadRig) cutDecisionLine(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "a tool that could move the call answers a cut the engine made") {
			return line
		}
	}
	t.Fatalf("no cut decision line:\n%s", r.logs.String())
	return ""
}

// The production shape after a replay: the opening is cut about 1.4 s in, said
// again, and the agreement arrives while the replay is still playing, with no
// new words from the callee: in the replay's own response, or in a response
// that starts after the replay was made and before it has played out. The
// response that answered the cut is long gone, so the cut refusals never see
// these calls; the words they answer are still the ones that cut the opening,
// so they are refused, and the opening is not said a third time.
func TestAnAgreementWhileTheReplayStillPlaysIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		// words cut the opening; isAgreedFirst has the reply to the cut call
		// the agreement too (words are replayed only on a refused one).
		words         string
		isAgreedFirst bool
		// isLaterResponse makes the call in a response after the replay's.
		isLaterResponse bool
	}{
		{"a greeting cut, the call in the replay's own response", "喂？", false, false},
		{"a greeting cut, the call in a later response", "喂？", false, true},
		{"a words cut refused, the call in the replay's own response", "好吧。", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLeadRig(t, leadRigOptions{opening: heldOpeningLine})
			r.cutOpeningWithWords(t, 70, tc.words) // 1.4 s
			calls := 0
			if tc.isAgreedFirst {
				r.agree("fc_first")
				calls++
				r.result(t, calls)
			}
			waitFor(t, "the opening to be said again", func() bool { return len(r.model.spokenLines()) >= 1 })
			r.ended()

			// The replay: a line of its own, still playing.
			frameBytes := 2 * media.RateProviderOut / 50
			r.leg.holdFrames(1 << 20)
			r.send(provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true})
			r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 100*frameBytes)})
			if tc.isLaterResponse {
				r.send(provider.Event{Type: provider.EventTypeOutputTranscript, Text: heldOpeningLine, IsFinal: true})
				r.ended()
				r.started()
			}
			waitFor(t, "the replay to be playing", func() bool {
				r.session.mu.Lock()
				defer r.session.mu.Unlock()
				return r.session.framesQueued >= 98 || r.session.turnSeq == 4
			})
			r.agree("fc_replay")
			r.ended()
			calls++
			out := r.result(t, calls)
			r.assertRefused(t, out, "先确认对方听清了内容")
			r.assertLine(t, calls, "guard=A", "verdict=OPENING_UNHEARD", "callerTurn=1", "isOpeningSaidAgain=false")
			r.leg.holdFrames(0)
			r.probe(t)
			if got := r.model.spokenLines(); len(got) != 1 {
				t.Errorf("spoken = %v, want the opening said again once", got)
			}
		})
	}
}

// 6. The replay is cut before its final clause (the engine cancels it before
// it reports the speech) and the reply calls the agreement: refused, and the
// opening is not said a second time. The callee's answer after hearing the
// reply in full then runs.
func TestAnAgreementOverTheReplayIsRefusedWithoutASecondReplay(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{opening: heldOpeningLine})
	frameBytes := r.openingGenerated(t)
	r.heard(150)
	r.speechStarted("u1")
	waitFor(t, "the cut", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil
	})
	r.leg.holdFrames(0)
	r.speechStopped("u1")
	r.started()
	r.final("u1", "好吧。")
	r.agree("fc_1")
	r.result(t, 1)
	waitFor(t, "the opening to be said again", func() bool { return len(r.model.spokenLines()) >= 1 })
	r.ended()

	// The replay, a line of its own, being made and heard; qwen cancels it
	// the moment it hears the callee, then reports the speech.
	r.leg.holdFrames(40)
	r.send(provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true})
	r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 50*frameBytes)})
	r.send(provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech})
	waitFor(t, "the cut of the replay", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil && r.session.bc.cut.event.Turn == 3 && r.session.bc.cut.isOpening
	})
	r.leg.holdFrames(0)
	r.callerSpoke("u2")
	r.started()
	r.final("u2", "可以，我要续约。")
	r.agree("fc_2")
	r.ended()
	out := r.result(t, 2)
	r.assertRefused(t, out, "先确认对方听清了内容")
	r.assertLine(t, 2, "guard=A", "verdict=OPENING_UNHEARD", "isOpeningSaidAgain=false", "callerTurn=2")
	r.probe(t)
	if got := r.model.spokenLines(); len(got) != 1 {
		t.Errorf("spoken = %v, want no second replay", got)
	}

	// The model's own words, heard to the end; then the callee agrees.
	r.started()
	r.speaks("我们续约可以打八折，您需要吗？")
	r.ended()
	r.callerSpoke("u3")
	r.final("u3", "好的，续约吧。")
	r.started()
	r.agree("fc_3")
	r.ended()
	r.assertAgreed(t, r.result(t, 4))
}

// 7. "No thanks." on an English call is a refusal.
func TestNoThanksOnAnEnglishCallRefuses(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{lang: "en"})
	r.callerSpoke("u1")
	r.final("u1", "No thanks.")
	r.started()
	r.agree("fc_1")
	r.ended()
	r.assertRefused(t, r.result(t, 1), "only a refusal")
	r.assertLine(t, 1, "verdict=DECLINED")
}

// 8. "要不要钱" is a question, not a refusal: the guard has nothing to say about
// it and the call runs.
func TestRefusalWordsInsideAQuestionAreNoRefusal(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.callerSpoke("u1")
	r.final("u1", "要不要钱")
	r.started()
	r.agree("fc_1")
	r.ended()
	r.assertAgreed(t, r.result(t, 1))
	r.assertLine(t, 1, "verdict=WORDS")
}

// 9. A move into a LOW phase after "不要" is not judged: it runs at once, with
// no wait and no decision line.
func TestAMoveIntoALowPhaseAfterARefusalRuns(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.callerSpoke("u1")
	r.started()
	r.call("fc_1", "classify", `{"value":"decline"}`)
	r.ended()
	out := r.result(t, 1)
	r.final("u1", "不要。")
	r.probe(t)
	if !strings.Contains(out, `"ok":"1"`) || r.node() != "declined" {
		t.Errorf("the decline did not run: the call is in %q, tool result %s", r.node(), out)
	}
	if got := r.leads.intents(); !slices.Equal(got, []string{"LOW"}) {
		t.Errorf("leads written = %v, want LOW", got)
	}
	if got := len(r.finalWait.waits()); got != 0 || len(r.guardLines()) != 0 {
		t.Errorf("a move into a LOW phase was judged (%d waits, lines %q)", got, r.guardLines())
	}
}

//
// record_lead and batches.
//

// record_lead HIGH is judged as the move is; record_lead NONE is not.
func TestRecordLeadWithInterestIsJudgedAndWithoutIsNot(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.callerSpoke("u1")
	r.final("u1", "不需要，谢谢。")
	r.started()
	r.call("fc_1", flow.ToolRecordLead, `{"intent":"HIGH","notes":"wants it"}`)
	r.ended()
	r.assertRefused(t, r.result(t, 1), "只是拒绝")
	r.started()
	r.call("fc_2", flow.ToolRecordLead, `{"intent":"NONE","notes":"not interested"}`)
	r.ended()
	if out := r.result(t, 2); !strings.Contains(out, `"ok":"1"`) {
		t.Errorf("record_lead NONE = %s", out)
	}
	if got := r.leads.intents(); !slices.Equal(got, []string{"NONE"}) {
		t.Errorf("leads written = %v, want NONE", got)
	}
	if got := len(r.guardLines()); got != 1 {
		t.Errorf("%d decision lines, want the HIGH one", got)
	}
}

// The guarded calls of one response share its verdict: the second does not
// wait again and is refused with the first, so a record_lead HIGH beside a
// refused agreement writes nothing. A later response's call runs.
func TestTheGuardedCallsOfOneResponseShareItsVerdict(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.callerSpoke("u1")
	r.started()
	r.agree("fc_1")
	r.awaitFinalWaits(t, 1)
	r.finalWait.expire()
	r.call("fc_2", flow.ToolRecordLead, `{"intent":"HIGH","notes":"agreed"}`)
	r.ended()
	r.assertRefused(t, r.result(t, 1), "没有听清")
	r.assertRefused(t, r.result(t, 2), "没有听清")
	r.assertLine(t, 2, "verdict=NO_FINAL", "isRefused=true", "isSameResponse=true")
	if got := len(r.finalWait.waits()); got != 1 {
		t.Errorf("%d waits, want one", got)
	}
	r.started()
	r.agree("fc_3")
	r.ended()
	r.assertAgreed(t, r.result(t, 3))
}

// An agreement before the callee has said anything (in the opening's own
// response) is refused once; there are no words to lose, but a provider that
// reported no speech must not refuse every agreement.
func TestAnAgreementBeforeTheCalleeHasSpokenIsRefusedOnce(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.started()
	r.speaks("您好，续约可以打八折。")
	r.agree("fc_1")
	r.ended()
	r.assertRefused(t, r.result(t, 1), "没有听清")
	r.assertLine(t, 1, "verdict=NO_CALLER_TURN", "callerTurn=0", "final=NONE")
	r.started()
	r.agree("fc_2")
	r.ended()
	r.assertAgreed(t, r.result(t, 2))
}

// Over an ending under way there is no turn left to confirm in: words not
// caught let the call run; a refusal still refuses it.
func TestOverAnEndingUnderwayOnlyARefusalRefuses(t *testing.T) {
	t.Run("no final", func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{})
		r.actions.mu.Lock()
		r.actions.isEndingLocked = true
		r.actions.mu.Unlock()
		r.callerSpoke("u1")
		r.started()
		r.call("fc_1", flow.ToolRecordLead, `{"intent":"HIGH","notes":"agreed"}`)
		r.awaitFinalWaits(t, 1)
		r.finalWait.expire()
		r.ended()
		if out := r.result(t, 1); !strings.Contains(out, `"ok":"1"`) {
			t.Errorf("record_lead = %s, want it run", out)
		}
		r.assertLine(t, 1, "verdict=NO_FINAL", "isRefused=false", "isEndingUnderway=true")
	})
	t.Run("a refusal", func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{})
		r.actions.mu.Lock()
		r.actions.isEndingLocked = true
		r.actions.mu.Unlock()
		r.callerSpoke("u1")
		r.final("u1", "不用了。")
		r.started()
		r.call("fc_1", flow.ToolRecordLead, `{"intent":"HIGH","notes":"agreed"}`)
		r.ended()
		r.assertRefused(t, r.result(t, 1), "只是拒绝")
	})
}

// On a model that answers quietly, a refused agreement asks for a turn, which
// releases a slot answer held for the response first: the model is to answer
// the refusal, and nothing of the batch goes quietly.
func TestARefusedAgreementAsksForATurnOnAModelThatAnswersQuietly(t *testing.T) {
	quietLeadFlow := strings.Replace(leadGuardFlow, `"isFlowHangupAllowed": true,`,
		`"isFlowHangupAllowed": true, "isCallerNext": true,`, 1)
	r := newLeadRig(t, leadRigOptions{flow: quietLeadFlow, isQuiet: true})
	r.callerSpoke("u1")
	r.final("u1", "不要不要。")
	r.started()
	r.speaks("好的。")
	r.call("fc_1", "note", `{"value":"asked about price"}`)
	r.agree("fc_2")
	r.ended()
	r.assertRefused(t, r.result(t, 2), "只是拒绝")
	if got := r.result(t, 1); !strings.Contains(got, `"ok":"1"`) {
		t.Errorf("the note's answer = %s", got)
	}
	if got := r.quiet.quietResults(); len(got) != 0 {
		t.Errorf("answered quietly: %v", got)
	}
}

//
// How each engine links a call to the caller's words.
//

// openai holds speech over audible playback and asks for each caller turn
// itself: the call answers the utterance whose turn was asked for, and waits
// for its final, which this engine often sends after the response began.
func TestOnOpenAITheCallAnswersTheTurnItWasAskedFor(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{profile: provider.OpenAIProfile()})
	r.callerSpoke("u1")
	waitFor(t, "the caller's turn to be asked for", func() bool { return len(r.hold.answers()) == 1 })
	r.started()
	r.agree("fc_1")
	r.awaitFinalWaits(t, 1)
	r.final("u1", "不用了，谢谢。")
	r.ended()
	r.assertRefused(t, r.result(t, 1), "只是拒绝")
	r.assertLine(t, 1, "verdict=DECLINED", "final=AFTER_CALL", "callerTurn=1")
}

// heldWordsOverOpening plays the opening on openai, which holds speech over it
// and never cuts by itself, with left of its heldOpeningFrames still to be
// heard when the callee speaks: the words ("好吧", not an acknowledgement) take
// the floor once known, the caller's turn is asked for, its final comes and
// the reply starts. The reply's tool call is left to the test.
func (r *leadRig) heldWordsOverOpening(t *testing.T, left int) {
	t.Helper()
	r.leg.holdFrames(left)
	r.started()
	r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*heldOpeningFrames)})
	r.ended()
	waitFor(t, "the opening to be queued", func() bool {
		r.session.mu.Lock()
		defer r.session.mu.Unlock()
		return r.session.turnSeq == 1 && !r.session.isResponding && r.session.framesQueued >= heldOpeningFrames-2
	})
	r.speechStarted("u1")
	r.send(provider.Event{Type: provider.EventTypeInputTranscript, ItemID: "u1", Text: "好吧"})
	waitFor(t, "the held words to take the floor", func() bool { return r.leg.clears() >= 1 })
	r.leg.holdFrames(0)
	r.speechStopped("u1")
	waitFor(t, "the caller's turn to be asked for", func() bool { return len(r.hold.answers()) == 1 })
	r.final("u1", "好吧。")
	r.started()
}

// On openai words held over the opening take the floor once known; an
// agreement made in answer to them is refused under A as on any engine, and
// the opening is said again.
func TestOnOpenAIWordsHeldOverTheOpeningRefuseTheAgreement(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{profile: provider.OpenAIProfile(), opening: heldOpeningLine})
	r.heldWordsOverOpening(t, 400)
	r.agree("fc_1")
	out := r.result(t, 1)
	waitFor(t, "the opening to be said again", func() bool { return len(r.model.spokenLines()) >= 1 })
	r.ended()
	r.assertRefused(t, out, "开场白会再对他说一遍")
	r.assertLine(t, 1, "guard=A", "verdict=OPENING_UNHEARD", "isOpeningSaidAgain=true")
}

// doubao reports the caller's words with no item ids, and the final before the
// end of the speech: the call that follows is judged at once.
func TestOnDoubaoTheFinalIsInBeforeTheCall(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{profile: provider.DoubaoProfile()})
	r.speechStarted("")
	r.final("", "不要了。")
	r.speechStopped("")
	r.started()
	r.agree("fc_1")
	r.ended()
	r.assertRefused(t, r.result(t, 1), "只是拒绝")
	r.assertLine(t, 1, "verdict=DECLINED", "final=BEFORE_CALL", "waitedMs=0")
}

// An engine that reports a transcript with no speech events: the transcript
// is the caller's turn.
func TestATranscriptWithNoSpeechEventsIsTheCallersTurn(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{})
	r.final("i1", "不需要。")
	r.started()
	r.agree("fc_1")
	r.ended()
	r.assertRefused(t, r.result(t, 1), "只是拒绝")
	if got := len(r.finalWait.waits()); got != 0 {
		t.Errorf("%d waits", got)
	}
}

// A response answers what the caller had said when it began: speech that
// starts while it runs is not its caller turn, and finals are matched to their
// own utterance by item id whatever order they come in.
func TestAToolCallAnswersTheUtteranceThatEndedBeforeItsResponse(t *testing.T) {
	session, _, model := startBridgeModel(t, provider.QwenProfile(), quietConfig(), nil)
	awaitBridgeEvent(t, session, EventTypeReady)
	send := func(e provider.Event) { model.events <- e }
	send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"})
	send(provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: "u1"})
	send(provider.Event{Type: provider.EventTypeResponseStarted})
	send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"})
	send(provider.Event{Type: provider.EventTypeToolCall, ToolCallID: "fc_1", ToolName: "classify", ToolArgs: `{}`})
	call := awaitBridgeEvent(t, session, EventTypeToolCall)
	if call.answered == nil || call.answered.seq != 1 || call.answered.itemID != "u1" {
		t.Fatalf("the call answers %+v, want u1", call.answered)
	}
	if call.calledAt.IsZero() {
		t.Error("the call carries no time")
	}
	send(provider.Event{Type: provider.EventTypeInputTranscript, ItemID: "u2", Text: "等一下", IsFinal: true})
	send(provider.Event{Type: provider.EventTypeInputTranscript, ItemID: "u1", Text: "不要", IsFinal: true})
	awaitBridgeEvent(t, session, EventTypeCustomerSaid)
	waitFor(t, "both finals", func() bool {
		_, isFinal, _, _ := session.callerFinal(call.answered)
		return isFinal
	})
	if text, _, _, _ := session.callerFinal(call.answered); text != "不要" {
		t.Errorf("u1's final = %q", text)
	}
}

//
// #598: rule A hears the opening from its final clause.
//

// noCommaOpeningLine is twoPartOpeningLine with no separator inside its long
// sentence: that whole sentence, 40 of the line's 52 letters, is its final
// clause, as it is its final sentence.
const noCommaOpeningLine = "您好，我是套餐续约专员小安。给您来电是想告诉您现在续约套餐可以享受八折优惠每月还多送一百分钟通话请问您需要吗？"

// openingHeardRows are the openings and moments #598 is about, over the
// 11 s of heldOpeningFrames. twoPartOpeningLine's final sentence is about the
// last 8.5 s, its final clause ("请问您需要吗") about the last 1.3 s; each gets
// finalSentenceSlack.
var openingHeardRows = []struct {
	name  string
	line  string
	heard int // frames of heldOpeningFrames heard when the callee speaks
	// isUnheard is rule A's verdict, by the final clause. isSentenceHeard is
	// what the final sentence says of the same moment (before #598).
	isUnheard, isSentenceHeard bool
}{
	{"two-part, 3 s in: inside the offer", twoPartOpeningLine, 150, true, true},
	{"two-part, 2 s left: just before the question", twoPartOpeningLine, heldOpeningFrames - 100, true, true},
	{"two-part, 1 s left: in the question", twoPartOpeningLine, heldOpeningFrames - 50, false, true},
	{"no comma, 3 s in: inside its last sentence", noCommaOpeningLine, 150, false, true},
	{"no comma, 1.5 s in: inside the greeting", noCommaOpeningLine, 75, true, false},
}

// Both of rule A's inputs date the opening by its final clause (#598): a
// speech start over it (isOverUnheardOpening: openai's held speech, and speech
// no cut accompanies on any engine) and an engine's own cut of it once it has
// finished generating (noteOpeningCut: the utterance under way is marked, or,
// with none under way, the next speech start). An opening whose last sentence
// has no separator is dated as it was by the sentence.
func TestRuleADatesTheOpeningByItsFinalClause(t *testing.T) {
	for _, tc := range openingHeardRows {
		t.Run(tc.name, func(t *testing.T) {
			played := time.Duration(tc.heard*frameDurationMs) * time.Millisecond
			unplayed := time.Duration((heldOpeningFrames-tc.heard)*frameDurationMs) * time.Millisecond
			if _, ok := cutInFinalSentence(tc.line, played, unplayed); ok != tc.isSentenceHeard {
				t.Fatalf("by the final sentence the moment is heard = %v, want %v: not the case asked for",
					ok, tc.isSentenceHeard)
			}
			newSession := func() *Session {
				leg := newFakeLeg(media.LawMu)
				leg.holdFrames(heldOpeningFrames - tc.heard)
				s := &Session{leg: leg, turnSeq: 1, framesQueued: heldOpeningFrames}
				s.cfg.Session.OpeningText = tc.line
				return s
			}
			cut := &openCut{isOpening: true, isOpeningLine: true, event: Event{Played: played, Unplayed: unplayed}}

			s := newSession()
			if got := s.isOverUnheardOpening(); got != tc.isUnheard {
				t.Errorf("a speech start: isOverUnheardOpening = %v, want %v", got, tc.isUnheard)
			}

			s = newSession()
			s.ct.add(&callerTurn{seq: 1, done: make(chan struct{})})
			s.bc.mu.Lock()
			s.noteOpeningCut(cut)
			s.bc.mu.Unlock()
			if got := s.ct.list[0].isOpeningUnheard; got != tc.isUnheard {
				t.Errorf("a cut with speech under way: isOpeningUnheard = %v, want %v", got, tc.isUnheard)
			}

			s = newSession()
			s.bc.mu.Lock()
			s.noteOpeningCut(cut)
			s.bc.mu.Unlock()
			if got := !s.ct.openingCutAt.IsZero(); got != tc.isUnheard {
				t.Errorf("a cut before the speech start: the next start marked = %v, want %v", got, tc.isUnheard)
			}
		})
	}
}

// cutTimes is how much of the opening the callee had heard at the cut
// (cutOpeningWithWords), and how much was left.
func (r *leadRig) cutTimes(t *testing.T) (played, unplayed time.Duration) {
	t.Helper()
	r.session.bc.mu.Lock()
	defer r.session.bc.mu.Unlock()
	if r.session.bc.cut == nil {
		t.Fatal("the opening was not cut")
	}
	return r.session.bc.cut.event.Played, r.session.bc.cut.event.Unplayed
}

// assertOpeningUnheard checks the first guarded call was refused under A with
// the opening said again, once and as written, on #610's once-per-call budget.
func (r *leadRig) assertOpeningUnheard(t *testing.T) {
	t.Helper()
	out := r.result(t, 1)
	waitFor(t, "the opening to be said again", func() bool { return len(r.model.spokenLines()) >= 1 })
	r.ended()
	r.assertRefused(t, out, "开场白会再对他说一遍")
	if got := r.model.spokenLines(); !slices.Equal(got, []string{r.opening}) {
		t.Errorf("spoken = %v, want the opening once, as written", got)
	}
	if !r.session.isOpeningSaidAgain() {
		t.Error("the opening was said again outside the once-per-call budget")
	}
	r.assertLine(t, 1, "guard=A", "verdict=OPENING_UNHEARD", "isRefused=true", "isOpeningSaidAgain=true")
}

// assertNothingSaidAgain checks, once everything sent has been handled, that
// no line was said.
func (r *leadRig) assertNothingSaidAgain(t *testing.T) {
	t.Helper()
	r.probe(t)
	if got := r.model.spokenLines(); len(got) != 0 {
		t.Errorf("said %v, want no line", got)
	}
}

// The production call of #598: on the two-part opening (twoPartOpeningLine:
// a short greeting, then one long sentence that carries the offer and ends in
// the question) background speech transcribed "好吧。" began about 3 s in.
// Dated by the final sentence it was past A, B found no refusal in it, and the
// agreement recorded HIGH. Dated by the final clause, words that cut the
// opening anywhere before its question refuse the agreement, and the opening
// is said again; words in the question are an answer, and B judges them. On an
// engine that cuts by itself both of A's inputs see the cut (qwen).
func TestWordsBeforeATwoPartOpeningsFinalClauseRefuseTheAgreement(t *testing.T) {
	for _, tc := range openingHeardRows[:2] {
		t.Run(tc.name, func(t *testing.T) {
			r := newLeadRig(t, leadRigOptions{opening: tc.line})
			r.cutOpeningWithWords(t, tc.heard, "好吧。")
			played, unplayed := r.cutTimes(t)
			if _, ok := cutInFinalSentence(tc.line, played, unplayed); !ok {
				t.Fatalf("a cut %v in with %v left is not in the final sentence: not the case of #598", played, unplayed)
			}
			r.agree("fc_1")
			r.assertOpeningUnheard(t)
			if line := r.cutDecisionLine(t); !strings.Contains(line, "verdict=WORDS") || !strings.Contains(line, "isRefused=false") {
				t.Errorf("the cut's own decision = %q, want words, not refused", line)
			}
		})
	}
	t.Run(openingHeardRows[2].name, func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{opening: twoPartOpeningLine})
		r.cutOpeningWithWords(t, openingHeardRows[2].heard, "好吧。")
		r.agree("fc_1")
		r.ended()
		r.assertAgreed(t, r.result(t, 1))
		r.assertLine(t, 1, "guard=B", "verdict=WORDS", "isRefused=false")
		r.assertNothingSaidAgain(t)
	})
}

// record_lead with an interest is judged as the agreement is (#598): HIGH or
// MEDIUM on words before the two-part opening's question is refused, nothing
// is written and the opening is said again; on words in the question it is
// written.
func TestRecordLeadOverATwoPartOpeningIsJudgedByItsFinalClause(t *testing.T) {
	for _, intent := range []string{"HIGH", "MEDIUM"} {
		args := `{"intent":"` + intent + `","notes":"wants the renewal"}`
		t.Run(intent+", 3 s in", func(t *testing.T) {
			r := newLeadRig(t, leadRigOptions{opening: twoPartOpeningLine})
			r.cutOpeningWithWords(t, openingHeardRows[0].heard, "好吧。")
			r.call("fc_1", flow.ToolRecordLead, args)
			r.assertOpeningUnheard(t)
			r.assertLine(t, 1, "tool="+flow.ToolRecordLead)
		})
		t.Run(intent+", in the question", func(t *testing.T) {
			r := newLeadRig(t, leadRigOptions{opening: twoPartOpeningLine})
			r.cutOpeningWithWords(t, openingHeardRows[2].heard, "好吧。")
			r.call("fc_1", flow.ToolRecordLead, args)
			r.ended()
			if out := r.result(t, 1); !strings.Contains(out, `"ok":"1"`) {
				t.Errorf("record_lead = %s, want it run", out)
			}
			if got := r.leads.intents(); !slices.Equal(got, []string{intent}) {
				t.Errorf("leads written = %v, want %s", got, intent)
			}
			r.assertLine(t, 1, "tool="+flow.ToolRecordLead, "guard=B", "verdict=WORDS", "isRefused=false")
			r.assertNothingSaidAgain(t)
		})
	}
}

// On openai, which holds speech over the opening and never cuts by itself, the
// speech start is A's one input (isOverUnheardOpening): "好吧" 3 s into the
// two-part opening refuses the agreement and the opening is said again; in its
// question the agreement runs and HIGH is written (#598).
func TestOnOpenAIATwoPartOpeningIsHeardFromItsFinalClause(t *testing.T) {
	t.Run(openingHeardRows[0].name, func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{profile: provider.OpenAIProfile(), opening: twoPartOpeningLine})
		r.heldWordsOverOpening(t, heldOpeningFrames-openingHeardRows[0].heard)
		r.agree("fc_1")
		r.assertOpeningUnheard(t)
	})
	t.Run(openingHeardRows[2].name, func(t *testing.T) {
		r := newLeadRig(t, leadRigOptions{profile: provider.OpenAIProfile(), opening: twoPartOpeningLine})
		r.heldWordsOverOpening(t, heldOpeningFrames-openingHeardRows[2].heard)
		r.agree("fc_1")
		r.ended()
		r.assertAgreed(t, r.result(t, 1))
		r.assertLine(t, 1, "guard=B", "verdict=WORDS", "isRefused=false")
		r.assertNothingSaidAgain(t)
	})
}

// An opening whose long last sentence has no separator is one clause there,
// as it was one sentence (#598): words 3 s in, inside that sentence, are past
// A as before, and B lets "好吧。" run.
func TestAnOpeningWithNoCommaInItsLastSentenceIsHeardAsBefore(t *testing.T) {
	r := newLeadRig(t, leadRigOptions{opening: noCommaOpeningLine})
	r.cutOpeningWithWords(t, openingHeardRows[3].heard, "好吧。")
	r.agree("fc_1")
	r.ended()
	r.assertAgreed(t, r.result(t, 1))
	r.assertLine(t, 1, "guard=B", "verdict=WORDS", "isRefused=false")
	r.assertNothingSaidAgain(t)
}

//
// The classifier.
//

func TestTheRefusalClassifier(t *testing.T) {
	for _, tc := range []struct {
		language, text string
		want           bool
	}{
		{"zh", "不要不要。", true},
		{"zh", "不需要，谢谢。", true},
		{"zh", "不用了", true},
		{"zh", "不用不用了", true},
		{"zh", "嗯，不用了，谢谢", true},
		{"zh-CN", "不感兴趣", true},
		{"zh", "我不需要", true},
		{"zh", "算了吧", true},
		{"zh", "别再打了", true},
		{"en", "No thanks.", true},
		{"en", "not interested", true},
		{"en", "I'm not interested, thank you.", true},
		{"en", "No, no.", true},
		{"en", "Um, no thank you", true},

		{"zh", "要不要钱", false},
		{"zh", "有没有合约", false},
		{"zh", "没问题", false},
		{"zh", "不错", false},
		{"zh", "不客气", false},
		{"zh", "好的", false},
		{"zh", "不要钱吗？", false},
		{"zh", "不要吗", false},
		{"zh", "不用谢", false},
		{"zh", "不要紧", false},
		{"zh", "不了解", false},
		{"zh", "谢谢", false},
		{"zh", "嗯", false},
		{"zh", "好的，不用了", false},
		{"zh", "不需要什么资料", false},
		// A stance the seed flows record as MEDIUM: interest, undecided.
		{"zh", "算了，我再考虑考虑。", false},
		{"zh", "还是算了，我再想想。", false},
		{"zh", "", false},
		{"zh", "no thanks", false},
		{"en", "No problem.", false},
		{"en", "Not at all.", false},
		{"en", "nothing else", false},
		{"en", "Thanks.", false},
		{"en", "No?", false},
		{"en", "Okay, no thanks", false},
		{"en", "不要", false},
		{"fr", "non merci", false},
	} {
		if got := isRefusalOnly(tc.language, tc.text); got != tc.want {
			t.Errorf("isRefusalOnly(%q, %q) = %v, want %v", tc.language, tc.text, got, tc.want)
		}
	}
}
