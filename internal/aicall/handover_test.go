// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// handoverFlow is shaped like the seed novanet_support: transfer_to_agent is
// always allowed and a successful one moves the call to a terminal phase that
// says the caller is being put through; hangup goes to farewell, which is also
// the closing target and where three silences in welcome lead.
const handoverFlow = `{
	"id": "handover-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"closingTarget": "farewell",
		"alwaysAllowedTools": ["transfer_to_agent", "take_message", "hangup"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",
				"condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "finish_transfer"},
			{"on": "TOOL_RESULT", "tool": "hangup",
				"condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "farewell"}
		]
	},
	"nodes": {
		"welcome": {
			"announce": "Thanks for calling, how can I help?",
			"instruction": "Help the caller. If the transfer is refused, offer to take a message.",
			"tools": [],
			"transitions": [{"on": "NO_INPUT",
				"condition": {"slot": "noInput.count", "op": "GTE", "value": 3}, "target": "farewell"}]
		},
		"finish_transfer": {
			"announce": "I am putting you through now.",
			"instruction": "The caller is connected right after this line; ask no further questions.",
			"tools": [], "isTerminal": true
		},
		"farewell": {
			"announce": "Thank you for calling. Goodbye.",
			"instruction": "The line hangs up once this has been said.",
			"tools": [], "isTerminal": true
		}
	}
}`

const (
	welcomeInstruction = "Help the caller. If the transfer is refused"
	transferLine       = "I am putting you through now."
	farewellLine       = "Thank you for calling. Goodbye."
	// handoverLimit is the call's time budget; the manual clock fires it.
	handoverLimit = time.Hour
)

// manualClock hands drive its waits and fires them when the test says.
type manualClock struct {
	mu    sync.Mutex
	waits []clockWait
}

type clockWait struct {
	d time.Duration
	c chan time.Time
}

func (c *manualClock) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.waits = append(c.waits, clockWait{d, ch})
	c.mu.Unlock()
	return ch
}

// fire fires the oldest wait of d, waiting for drive to ask for one.
func (c *manualClock) fire(t *testing.T, d time.Duration) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("a wait of %v", d), func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		i := slices.IndexFunc(c.waits, func(w clockWait) bool { return w.d == d })
		if i < 0 {
			return false
		}
		c.waits[i].c <- time.Time{}
		c.waits = slices.Delete(c.waits, i, i+1)
		return true
	})
}

// handoverCall is a call under drive on a switch that does not answer
// uuid_transfer: the transfer is sent and its reply never comes (#303). ticks
// fires the handover's waits, one per send; clock fires drive's.
type handoverCall struct {
	actions  *callActions
	session  *Session
	leg      *fakeLeg
	model    *fakeModel
	engine   *flow.Engine
	recorder *callRecorder
	sw       *fakeSwitch
	ticks    chan time.Time
	clock    *manualClock
	done     chan struct{}
}

func newHandoverCall(t *testing.T) *handoverCall {
	t.Helper()
	sw := &fakeSwitch{transferErr: fmt.Errorf("%w: uuid_transfer", esl.ErrTimeout)}
	session, leg, model := startBridge(t, provider.OpenAIProfile())
	awaitBridgeEvent(t, session, EventTypeReady)

	o := testOrchestrator(t, sw)
	clock := &manualClock{}
	o.budgetAfter = clock.after
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	spec, err := flow.Load([]byte(handoverFlow))
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	engine := flow.NewEngine(spec, "en", nil, log)
	recorder := newStartedRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil)
	ticks := make(chan time.Time)
	actions := &callActions{
		orchestrator:  o,
		session:       session,
		log:           log,
		callerChannel: "caller-channel-1",
		recorder:      recorder,
		handoverAfter: func(time.Duration) <-chan time.Time { return ticks },
	}
	queues, _ := o.cfg.Catalog.Queues(t.Context())
	for _, queue := range queues {
		if queue.Name == "support" {
			actions.fallbackQueue = &queue.ID
		}
	}
	runtime := flow.NewRuntime(engine, actions, flow.NewBackend(""), nil, log)

	h := &handoverCall{actions: actions, session: session, leg: leg, model: model,
		engine: engine, recorder: recorder, sw: sw, ticks: ticks, clock: clock,
		done: make(chan struct{})}
	go func() {
		defer close(h.done)
		o.drive(t.Context(), session, runtime, actions, recorder,
			sessionBudget{limitAfter: handoverLimit, floorWait: time.Minute}, log)
		actions.disarm()
	}()
	return h
}

// turn plays one model turn: what it says ("" is nothing) and the tool it
// calls ("" is none).
func (h *handoverCall) turn(text, tool, args string) {
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	if text != "" {
		h.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
			Text: text, IsFinal: true}
	}
	if tool != "" {
		h.model.events <- provider.Event{Type: provider.EventTypeToolCall,
			ToolCallID: "fc_" + tool, ToolName: tool, ToolArgs: args}
	}
	if text != "" {
		h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta,
			Audio: make([]byte, media.FrameSamples)}
	}
	h.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
}

// toolResults waits until n tool calls have been answered and returns them.
func (h *handoverCall) toolResults(t *testing.T, n int) []toolResult {
	t.Helper()
	waitUntil(t, fmt.Sprintf("%d tool answers", n), func() bool {
		return len(h.model.recordedToolResults()) >= n
	})
	return h.model.recordedToolResults()
}

// transferStalls has the model hand the caller over: the tool result carries
// the transfer line, the line plays, and the armed transfer is sent to a
// switch that does not answer.
func (h *handoverCall) transferStalls(t *testing.T) {
	t.Helper()
	h.turn("", flow.ToolTransferToAgent,
		`{"queue":"support","reason":"BILLING","summary":"wants a refund"}`)
	h.toolResults(t, 1)
	h.turn(transferLine, "", "")
	h.awaitSettling(t)
}

// limitTransferStalls has the time limit hand the caller over, with the same
// outcome on the switch.
func (h *handoverCall) limitTransferStalls(t *testing.T) {
	t.Helper()
	h.clock.fire(t, handoverLimit)
	waitUntil(t, "the handover cue", func() bool {
		return slices.ContainsFunc(h.model.recordedUserText(), func(text string) bool {
			return strings.Contains(text, "putting them through")
		})
	})
	h.turn("I'll put you through to a colleague now.", "", "")
	h.awaitSettling(t)
}

func (h *handoverCall) awaitSettling(t *testing.T) {
	t.Helper()
	waitUntil(t, "the transfer to be sent", func() bool {
		return len(h.sw.recordedTransfers()) == 1 && h.actions.isSettlingHandover()
	})
}

// told waits for the model to be told the transfer did not go through.
func (h *handoverCall) told(t *testing.T) {
	t.Helper()
	waitUntil(t, "the caller to be told", func() bool {
		return slices.ContainsFunc(h.model.recordedUserText(), isHandoverFailedCue)
	})
}

func isHandoverFailedCue(text string) bool { return strings.Contains(text, "did not go through") }

func (h *handoverCall) awaitEnd(t *testing.T) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("drive did not return after the call ended")
	}
}

func (h *handoverCall) existsAsked() int {
	h.sw.mu.Lock()
	defer h.sw.mu.Unlock()
	return h.sw.existsAsked
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A transfer the switch did not answer is not a failed one: the bot leg stays
// up, silent both ways, with every other ending locked out, until the switch
// settles it. N is when it runs the transfer, which ends the bot leg: before
// the first check (handoverWait), after it, or never. Run late, the caller is
// moved once and nothing else is sent; never run, the conversation goes on and
// the caller is told, and no second transfer is ever sent.
func TestAHandoverTheSwitchDidNotConfirmIsSettledByTheSwitch(t *testing.T) {
	never := time.Duration(-1)
	for _, n := range []time.Duration{500 * time.Millisecond, 5 * time.Second, 30 * time.Second, never} {
		name := n.String()
		if n == never {
			name = "never"
		}
		t.Run(name, func(t *testing.T) {
			h := newHandoverCall(t)
			h.transferStalls(t)
			if h.session.isClosed() {
				t.Fatal("the bot leg was closed on a transfer the switch may still run")
			}
			if !h.session.isHushed.Load() {
				t.Fatal("the call is not held silent while the transfer is unconfirmed")
			}
			// Nothing the model asks for in the meantime arms an ending.
			h.turn("", flow.ToolHangup, goodbyeHangup)
			if results := h.toolResults(t, 2); !strings.Contains(results[1].output, `"ok":"1"`) ||
				h.actions.isArmed() {
				t.Fatalf("a hangup during the handover was armed: %+v", results[1])
			}

			if n != never && n < handoverWait {
				h.leg.Stop() // the switch moves the caller and hangs our leg up
				h.awaitEnd(t)
				if got := h.sw.recordedTransfers(); len(got) != 1 {
					t.Errorf("transfers = %v, want the one", got)
				}
				if asked := h.existsAsked(); asked != 0 {
					t.Errorf("the switch was asked about the caller %d times after the transfer ran", asked)
				}
				return
			}

			// The first check: the switch answers, and the caller is still
			// on the bot leg, so the transfer did not run.
			h.ticks <- time.Time{}
			h.told(t)
			if h.session.isHushed.Load() || h.actions.isSettlingHandover() {
				t.Fatal("the conversation did not go on")
			}
			if got := h.sw.variable("hangup_after_bridge"); got != "false" {
				t.Errorf("hangup_after_bridge = %q, want the bot bridge's rule back", got)
			}
			// The model asking again is refused, and nothing is sent.
			h.turn("", flow.ToolTransferToAgent,
				`{"queue":"support","reason":"BILLING","summary":"again"}`)
			if results := h.toolResults(t, 3); !strings.Contains(results[2].output, "TRANSFER_FAILED") {
				t.Errorf("a second transfer_to_agent answered %s, want TRANSFER_FAILED", results[2].output)
			}
			if got := h.sw.recordedTransfers(); len(got) != 1 {
				t.Errorf("transfers = %v, want the one", got)
			}

			if n == 30*time.Second {
				// The switch was answering on a replaced link and runs the
				// first transfer after all: the caller goes where they asked,
				// and the conversation ends with the leg.
				h.leg.Stop()
				h.awaitEnd(t)
				if got := h.sw.recordedTransfers(); len(got) != 1 {
					t.Errorf("transfers = %v, want the one", got)
				}
				return
			}
			// Never run: the call goes on and may end the ordinary way, the
			// flow's hangup rule taking it to its goodbye.
			h.turn("", flow.ToolHangup, goodbyeHangup)
			results := h.toolResults(t, 4)
			if !strings.Contains(results[3].output, farewellLine) || !h.actions.isArmed() {
				t.Errorf("a hangup after the failed handover did not reach the goodbye: %s armed=%v",
					results[3].output, h.actions.isArmed())
			}
			h.turn(farewellLine, "", "")
			h.awaitEnd(t)
			if got := h.engine.NodeID(); got != "farewell" {
				t.Errorf("phase = %s, want farewell", got)
			}
			if got := h.sw.variable("aicc_bot_finished"); got != "HANGUP" {
				t.Errorf("aicc_bot_finished = %q, want HANGUP", got)
			}
		})
	}
}

// A caller who is gone by the check leaves nothing to settle; a switch that
// still answers nothing after handoverChecks questions leaves the caller to the
// dialplan. Either way the bot leg closes and nothing is sent again.
func TestAHandoverThatCannotBeSettledClosesTheBotLeg(t *testing.T) {
	for _, tc := range []struct {
		name      string
		isGone    bool
		existsErr error
		checks    int
	}{
		{name: "the caller has gone", isGone: true, checks: 1},
		{name: "the switch answers nothing", existsErr: fmt.Errorf("%w: uuid_exists", esl.ErrTimeout),
			checks: handoverChecks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandoverCall(t)
			h.transferStalls(t)
			h.sw.mu.Lock()
			isUp := !tc.isGone
			h.sw.exists = &isUp
			h.sw.existsErr = tc.existsErr
			h.sw.mu.Unlock()
			for range tc.checks {
				h.ticks <- time.Time{}
			}
			h.awaitEnd(t)
			if asked := h.existsAsked(); asked != tc.checks {
				t.Errorf("the switch was asked %d times, want %d", asked, tc.checks)
			}
			if got := h.sw.recordedTransfers(); len(got) != 1 {
				t.Errorf("transfers = %v, want the one", got)
			}
			if slices.ContainsFunc(h.model.recordedUserText(), isHandoverFailedCue) {
				t.Error("the model was cued on a call that is over")
			}
		})
	}
}

// The time limit's own handover cannot be refused into a conversation: the
// limit has decided the call ends, so one that never ran ends it.
func TestATimeLimitHandoverThatNeverRanEndsTheCall(t *testing.T) {
	h := newHandoverCall(t)
	h.limitTransferStalls(t)
	h.ticks <- time.Time{}
	h.awaitEnd(t)
	if got := h.sw.variable("aicc_bot_finished"); got != hangupCauseSessionLimit {
		t.Errorf("aicc_bot_finished = %q, want %s", got, hangupCauseSessionLimit)
	}
	if slices.ContainsFunc(h.model.recordedUserText(), isHandoverFailedCue) {
		t.Error("the model was cued on a call the limit ended")
	}
	if got := h.sw.recordedTransfers(); len(got) != 1 {
		t.Errorf("transfers = %v, want the one", got)
	}
}

// A handover that never ran leaves the call in the phase the transfer was
// made from, not in the terminal one it led to ("the caller is connected right
// after this line"), where no rule fires and no guard runs. The model is
// re-instructed for that phase before it is told, and from there the call has
// its guards back: the time limit, the flow's silence rule, and the rule that
// a failed conversation never sends a second transfer.
func TestAHandoverThatNeverRanResumesThePhaseItWasMadeFrom(t *testing.T) {
	// resumed is a call whose transfer never ran, told so; with the time
	// limit falling due while the switch was being waited on, when
	// isLimitDuringHandover.
	resumed := func(t *testing.T, isLimitDuringHandover bool) *handoverCall {
		t.Helper()
		h := newHandoverCall(t)
		h.transferStalls(t)
		if isLimitDuringHandover {
			// The limit waits for the handover to settle.
			h.clock.fire(t, handoverLimit)
			waitUntil(t, "the limit to wait for the handover", func() bool {
				h.clock.mu.Lock()
				defer h.clock.mu.Unlock()
				return slices.ContainsFunc(h.clock.waits, func(w clockWait) bool { return w.d == handoverWait })
			})
		}
		h.ticks <- time.Time{}
		h.told(t)

		calls := h.model.recordedCalls()
		cue := slices.Index(calls, "SendUserText")
		if cue < 1 || calls[cue-1] != "UpdateInstructions" {
			t.Fatalf("calls = %v, want the instructions re-pinned right before the cue", calls)
		}
		h.model.mu.Lock()
		pinned := h.model.instructions[len(h.model.instructions)-1]
		h.model.mu.Unlock()
		if !strings.Contains(pinned, welcomeInstruction) || strings.Contains(pinned, "connected right after") {
			t.Errorf("instructions re-pinned to %q, want welcome's", pinned)
		}
		// The cue was the last thing drive did, and nothing has reached it
		// since.
		if got := h.engine.NodeID(); got != "welcome" || h.engine.IsTerminal() {
			t.Fatalf("phase = %s (terminal %v), want welcome", got, h.engine.IsTerminal())
		}
		if lines := h.model.spokenLines(); len(lines) != 0 {
			t.Errorf("lines spoken on the return = %v, want none: the caller heard welcome's", lines)
		}
		// The model apologises and offers a message.
		h.turn("Sorry, I could not put you through. Can I take a message?", "", "")
		waitUntil(t, "the apology to play", func() bool { return !h.session.isHoldingTheFloor() })
		return h
	}
	saysGoodbye := func(t *testing.T, h *handoverCall) {
		t.Helper()
		waitUntil(t, "the goodbye", func() bool { return slices.Contains(h.model.spokenLines(), farewellLine) })
		h.turn(farewellLine, "", "")
		h.awaitEnd(t)
		if got := h.engine.NodeID(); got != "farewell" {
			t.Errorf("phase = %s, want farewell", got)
		}
		if got := h.sw.recordedTransfers(); len(got) != 1 {
			t.Errorf("transfers = %v, want the one", got)
		}
	}

	t.Run("the time limit ends it at the closing target", func(t *testing.T) {
		h := resumed(t, true)
		// The limit's second look: the call is a conversation again, the
		// transfer the limit would make is refused, and the flow's goodbye
		// ends it.
		h.clock.fire(t, handoverWait)
		saysGoodbye(t, h)
		if slices.ContainsFunc(h.model.recordedUserText(), func(text string) bool {
			return strings.Contains(text, "putting them through")
		}) {
			t.Error("the model was asked to announce a transfer that is refused")
		}
		if got := h.sw.variable("aicc_bot_finished"); got != "FLOW_END" {
			t.Errorf("aicc_bot_finished = %q, want FLOW_END", got)
		}
	})

	t.Run("three silences end it at the closing target", func(t *testing.T) {
		h := resumed(t, false)
		for range 3 {
			h.session.emit(Event{Type: EventTypeNoInput})
		}
		saysGoodbye(t, h)
	})

	t.Run("a provider failure sends no second transfer", func(t *testing.T) {
		h := resumed(t, false)
		h.model.events <- provider.Event{Type: provider.EventTypeError, IsFatal: true,
			Text: "the provider went away"}
		h.awaitEnd(t)
		if got := h.sw.recordedTransfers(); len(got) != 1 {
			t.Errorf("transfers = %v, want the one", got)
		}
		// The bot leg goes unmarked, with the bot bridge's teardown rule, so
		// the dialplan's fallback takes the caller.
		if got := h.sw.handedOnCount(); got != 1 {
			t.Errorf("teardown rule put back %d times, want once, by the transfer", got)
		}
		if got := h.sw.variable("hangup_after_bridge"); got != "false" {
			t.Errorf("hangup_after_bridge = %q, want false", got)
		}
		if got := h.sw.variable("aicc_bot_finished"); got != "" {
			t.Errorf("aicc_bot_finished = %q, want unmarked", got)
		}
	})
}
