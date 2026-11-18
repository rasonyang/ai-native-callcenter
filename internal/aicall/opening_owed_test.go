// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"slices"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// A callee's pickup greeting that lands on the bot's opening line
// (docs/design/11-backchannels.md, "Greetings over the opening").

const openingLine = "Hi, this is Ann from Acme about your plan."

func TestTheClassifierSeparatesGreetingsFromWords(t *testing.T) {
	tests := []struct {
		language string
		text     string
		want     backchannelVerdict
	}{
		{"zh", "喂", verdictGreeting},
		{"zh", "喂？", verdictGreeting},
		{"zh", "喂?", verdictGreeting},
		{"zh", "喂，您好", verdictGreeting},
		{"zh", "喂喂喂", verdictGreeting},
		{"zh", "你好你好", verdictGreeting},
		{"zh", "您好。", verdictGreeting},
		{"zh", "你好，喂，您好", verdictGreeting},
		{"zh-CN", "喂喂", verdictGreeting},
		{"zh", "喂喂喂喂", verdictContent},
		{"zh", "喂你好您好喂", verdictContent},
		{"zh", "喂，请问你是哪位", verdictContent},
		{"zh", "你好，什么事", verdictContent},
		{"zh", "嗯", verdictContent},
		{"zh", "hello", verdictContent},
		{"zh", "", verdictGreetingSoFar},
		{"zh", "你", verdictGreetingSoFar},
		{"zh", "喂你", verdictGreetingSoFar},
		{"zh", "喂，您", verdictGreetingSoFar},
		{"en", "hello", verdictGreeting},
		{"en", "Hello?", verdictGreeting},
		{"en", "hi, hello", verdictGreeting},
		{"en", "Hi hi hi", verdictGreeting},
		{"en", "hellohi", verdictGreeting},
		{"en", "hi hi hi hi", verdictContent},
		{"en", "hello, who is this?", verdictContent},
		{"en", "hi there", verdictContent},
		{"en", "喂", verdictContent},
		{"en", "yeah", verdictContent},
		{"en", "hel", verdictGreetingSoFar},
		{"en", "h", verdictGreetingSoFar},
		{"en", "hello h", verdictGreetingSoFar},
		{"en", "hello hel", verdictGreetingSoFar},
		{"en", "hello hello hel", verdictGreetingSoFar},
		{"en", "hello hello hello hel", verdictContent},
		{"en", "hex", verdictContent},
	}
	for _, tt := range tests {
		if got := classifyGreeting(tt.language, tt.text); got != tt.want {
			t.Errorf("classifyGreeting(%q, %q) = %v, want %v", tt.language, tt.text, got, tt.want)
		}
	}
}

// openingRig is a damageRig on an engine that cuts by itself, whose call has an
// opening line, with the replay's wait on a clock the test drives.
type openingRig struct {
	*damageRig
	clock *holdClock
}

func newOpeningRig(t *testing.T) *openingRig {
	t.Helper()
	wait := newVerdictWait()
	clock := &holdClock{}
	h := newQuietHarnessBackend(t, quietFlow, false, quietConfig(), "", func(c *claimsHarness) {
		c.session.cutWait = wait.after
		c.session.holdAfter = clock.after
		c.session.cfg.OnCutAcknowledged = c.actions.onCutAcknowledged
		c.session.cfg.Session.OpeningText = openingLine
	})
	return &openingRig{damageRig: &damageRig{quietHarness: h, wait: wait}, clock: clock}
}

func (r *openingRig) spoken(t *testing.T, n int) []string {
	t.Helper()
	waitFor(t, "the opening to be said again", func() bool { return len(r.model.spokenLines()) >= n })
	return r.model.spokenLines()
}

func (r *openingRig) assertNothingSpoken(t *testing.T) {
	t.Helper()
	// The orchestrator handles events in order: a later call answered proves
	// the earlier events were handled.
	before := len(r.model.recordedToolResults())
	r.started()
	r.toolCall("fc_probe", "note", `{"value":"probe"}`)
	r.ended()
	waitFor(t, "the probe to be answered", func() bool { return len(r.model.recordedToolResults()) > before })
	if got := r.model.spokenLines(); len(got) != 0 {
		t.Errorf("said %v, want no line", got)
	}
}

// The greeting cuts the opening while audio is queued, the model's reply has
// started, and the reply calls a moving tool: the tool is refused, the reply is
// muted and the opening is said again, word for word, once.
func TestAGreetingOverTheOpeningMutesTheReplyAndSaysTheOpeningAgain(t *testing.T) {
	r := newOpeningRig(t)
	r.botSpeaksUntilCut(t, "u1") // the opening is turn 1
	r.started()                  // the model's reply to the greeting
	r.audio()
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	r.callerFinal("u1", "Hello?")
	r.awaitResult(t)
	r.spoken(t, 1)
	r.ended() // after the line: the reply was still being made when it was muted

	out := r.lastResult(t)
	if !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, "hello") {
		t.Errorf("tool result = %s, want a refusal that says the caller only greeted", out)
	}
	if r.node() != "offer" {
		t.Errorf("the call moved to %q on a greeting", r.node())
	}
	if got := r.spoken(t, 1); !slices.Equal(got, []string{openingLine}) {
		t.Errorf("spoken = %v, want the opening once, as written", got)
	}
	if got := r.model.spokenLinesClosing(); !slices.Equal(got, []bool{false}) {
		t.Errorf("closing flags = %v, want the opening said as an ordinary line", got)
	}
	r.session.mu.Lock()
	muted := r.session.mutedTurn
	r.session.mu.Unlock()
	if muted != 2 {
		t.Errorf("muted turn = %d, want the model's reply (turn 2, after the opening)", muted)
	}
}

// The greeting's transcript arrives before the model's reply starts: the line
// waits for the reply (it would be taken for it), mutes it, and then speaks.
func TestTheOpeningWaitsForTheRepliesStartWhenTheGreetingIsFirst(t *testing.T) {
	r := newOpeningRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.callerFinal("u1", "hello")
	waitFor(t, "the replay to wait", func() bool { return r.clock.pending() == 1 })
	if got := r.model.spokenLines(); len(got) != 0 {
		t.Fatalf("said %v before the model's reply started", got)
	}
	r.started()
	if got := r.spoken(t, 1); !slices.Equal(got, []string{openingLine}) {
		t.Errorf("spoken = %v, want the opening", got)
	}
	if r.clock.pending() != 0 {
		t.Error("the backstop is still running after the reply started")
	}
}

// A reply that never comes: the backstop says the opening anyway.
func TestTheOpeningIsSaidAtTheBackstopWhenNoReplyStarts(t *testing.T) {
	r := newOpeningRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.callerFinal("u1", "hello")
	waitFor(t, "the replay to wait", func() bool { return r.clock.pending() == 1 })
	r.clock.advance(replayWaitCap)
	if got := r.spoken(t, 1); !slices.Equal(got, []string{openingLine}) {
		t.Errorf("spoken = %v, want the opening", got)
	}
}

// Once per call: a greeting that cuts the replay still refuses a moving tool,
// but the opening is not said a third time.
func TestAGreetingOverTheReplayIsNotAnsweredWithAThirdTelling(t *testing.T) {
	r := newOpeningRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.started()
	r.callerFinal("u1", "hello")
	r.spoken(t, 1)

	// The replay is a line, a turn of its own; the callee greets again over it.
	r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted, IsSpokenLine: true}
	r.audio()
	r.leg.holdFrames(40)
	r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320*50)}
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"}
	waitFor(t, "the cut over the replay", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil && r.session.bc.cut.event.Turn == 3
	})
	r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
	r.leg.holdFrames(0)
	r.started()
	r.toolCall("fc_2", "classify", `{"value":"agree"}`)
	r.callerFinal("u2", "hello hello")
	r.ended()
	r.awaitResult(t)

	if out := r.lastResult(t); !strings.Contains(out, `"ok":"0"`) {
		t.Errorf("tool result = %s, want a refusal", out)
	}
	if r.node() != "offer" {
		t.Errorf("the call moved to %q", r.node())
	}
	if got := r.model.spokenLines(); len(got) != 1 {
		t.Errorf("spoken = %v, want the opening said again once", got)
	}
}

// Words after the greeting are an answer: nothing is refused or said again.
func TestWordsOverTheOpeningAreNotReplayed(t *testing.T) {
	r := newOpeningRig(t)
	r.botSpeaksUntilCut(t, "u1")
	r.started()
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	r.callerFinal("u1", "hello, I agree to renew")
	r.ended()
	r.awaitResult(t)
	if r.node() != "agreed" {
		t.Errorf("the call is in %q, want agreed", r.node())
	}
	r.assertNothingSpoken(t)
}

// A greeting that does not cut anything (the opening had finished and played)
// is an ordinary turn.
func TestAGreetingWhenNothingWasPlayingIsAnOrdinaryTurn(t *testing.T) {
	r := newOpeningRig(t)
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
	r.started()
	r.callerFinal("u1", "hello")
	r.assertNothingSpoken(t)
}

// An armed ending, or a call that has left the entry phase, is not given the
// opening again.
func TestTheOpeningIsNotSaidAgainOnceTheCallHasMovedOnOrIsEnding(t *testing.T) {
	t.Run("an ending is armed", func(t *testing.T) {
		r := newOpeningRig(t)
		r.botSpeaksUntilCut(t, "u1")
		r.actions.mu.Lock()
		r.actions.armed = func() {}
		r.actions.mu.Unlock()
		r.started()
		r.callerFinal("u1", "hello")
		r.assertNothingSpoken(t)
	})
	t.Run("the flow left the entry phase", func(t *testing.T) {
		r := newOpeningRig(t)
		r.botSpeaksUntilCut(t, "u1")
		r.started() // the response that answers the cut calls nothing
		r.ended()
		// A later response is past the cut's window: its move runs, whatever
		// the verdict. (A cut of the opening with no verdict in time no longer
		// runs as it always did, #610.)
		r.started()
		r.toolCall("fc_1", "classify", `{"value":"maybe"}`)
		r.ended()
		r.awaitResult(t)
		if r.node() != "plain" {
			t.Fatalf("the call is in %q, want plain", r.node())
		}
		r.callerFinal("u1", "hello")
		r.assertNothingSpoken(t)
	})
}

// A cut of the opening by the provider before any of it was queued is a cut of
// the opening all the same.
func TestAProviderCutBeforeAnyAudioOpensACutOverTheOpening(t *testing.T) {
	r := newOpeningRig(t)
	r.started() // the opening, nothing queued yet
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u1"}
	r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
	waitFor(t, "the cut to be recorded", func() bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.cut != nil && r.session.bc.cut.isOpening
	})
	r.started()
	r.callerFinal("u1", "hi")
	r.spoken(t, 1)
}

// A greeting over an ordinary turn (not the opening) is words.
func TestAGreetingOverAnotherTurnIsNotJudgedAsOne(t *testing.T) {
	r := newOpeningRig(t)
	r.started() // turn 1: the opening
	r.ended()
	r.botSpeaksUntilCut(t, "u1") // turn 2
	r.started()
	r.toolCall("fc_1", "classify", `{"value":"agree"}`)
	r.callerFinal("u1", "hello")
	r.ended()
	r.awaitResult(t)
	if r.node() != "agreed" {
		t.Errorf("the call is in %q, want the greeting over a later turn to be words", r.node())
	}
}

// openai: a greeting held over the opening is declined and the opening is
// never cut; the same greeting over any other turn is words.
func TestAGreetingHeldOverTheOpeningIsDeclined(t *testing.T) {
	for _, tt := range []struct {
		name, language, partial, final string
	}{
		{"zh", "zh", "喂", "喂？"},
		{"en", "en", "Hel", "Hello?"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := startHold(t, tt.language, Config{NoInput: -1})
			r.session.cfg.Session.OpeningText = openingLine
			r.botSpeaks() // turn 1: the opening
			r.started("item-1")
			r.partial("item-1", tt.partial)
			r.stopped("item-1")
			r.final("item-1", tt.final)
			events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
			r.assertNoCut(t, events)
			if !events[len(events)-1].IsBackchannel {
				t.Error("the greeting was not flagged")
			}
			if got := r.model.declines(); !slices.Equal(got, []string{"item-1"}) {
				t.Errorf("declined %v, want item-1", got)
			}
		})
	}
}

func TestAGreetingHeldOverALaterTurnIsWords(t *testing.T) {
	r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
	r.session.cfg.Session.OpeningText = openingLine
	r.send(provider.Event{Type: provider.EventTypeResponseStarted})
	r.send(provider.Event{Type: provider.EventTypeResponseDone})
	awaitBridgeEvent(t, r.session, EventTypeTurnDone)
	r.botSpeaks() // turn 2
	r.started("item-1")
	r.partial("item-1", "喂")
	events := eventsUntil(t, r.session, EventTypeCallerSpeech, nil)
	if got := typesOf(events); !slices.Equal(got, []EventType{EventTypeBargeIn, EventTypeCallerSpeech}) {
		t.Errorf("events = %v, want the greeting to take the floor outside the opening", got)
	}
}

// Words that follow the greeting over the opening take the floor at once.
func TestWordsAfterAGreetingOverTheOpeningTakeTheFloor(t *testing.T) {
	r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
	r.session.cfg.Session.OpeningText = openingLine
	r.botSpeaks()
	r.started("item-1")
	r.partial("item-1", "喂")
	waitFor(t, "the cap to run for the held greeting", func() bool { return r.clock.pending() == 1 })
	r.partial("item-1", "，请问你是")
	events := eventsUntil(t, r.session, EventTypeCallerSpeech, nil)
	if got := typesOf(events); !slices.Contains(got, EventTypeBargeIn) {
		t.Errorf("events = %v, want the words to take the floor", got)
	}
}

// At the cap a greeting so far over the opening does not cut it; with no text
// at all the cap takes the floor as ever.
func TestTheCapDoesNotCutTheOpeningForAGreetingSoFar(t *testing.T) {
	t.Run("a greeting so far", func(t *testing.T) {
		r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
		r.session.cfg.Session.OpeningText = openingLine
		r.botSpeaks()
		r.started("item-1")
		r.partial("item-1", "喂")
		waitFor(t, "the partial", func() bool {
			r.session.bc.mu.Lock()
			defer r.session.bc.mu.Unlock()
			u := r.session.bc.find("item-1")
			return u != nil && u.text == "喂"
		})
		waitFor(t, "the cap to run", func() bool { return r.clock.pending() == 1 })
		r.clock.advance(holdCap)
		r.stopped("item-1")
		r.final("item-1", "喂")
		events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
		r.assertNoCut(t, events)
	})
	t.Run("no final by the second cap", func(t *testing.T) {
		r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
		r.session.cfg.Session.OpeningText = openingLine
		r.botSpeaks()
		r.started("item-1")
		r.partial("item-1", "喂")
		waitFor(t, "the cap to run", func() bool { return r.clock.pending() == 1 })
		waitFor(t, "the partial", func() bool {
			r.session.bc.mu.Lock()
			defer r.session.bc.mu.Unlock()
			u := r.session.bc.find("item-1")
			return u != nil && u.text == "喂"
		})
		r.clock.advance(holdCap)
		waitFor(t, "the second cap to be armed", func() bool { return r.clock.pending() == 1 })
		if got := r.model.declines(); len(got) != 0 {
			t.Fatalf("declined %v at the first cap", got)
		}
		r.clock.advance(holdCap)
		waitFor(t, "the greeting to be declined", func() bool { return len(r.model.declines()) == 1 })
		if got := r.model.declines(); !slices.Equal(got, []string{"item-1"}) {
			t.Errorf("declined %v, want item-1", got)
		}
		if r.clock.pending() != 0 {
			t.Error("a cap is still running")
		}
		r.assertNoCut(t, nil)
		// A final that arrives late is recorded flagged and does nothing else.
		r.stopped("item-1")
		r.final("item-1", "喂")
		events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
		r.assertNoCut(t, events)
		if !events[len(events)-1].IsBackchannel {
			t.Error("the late final was not flagged")
		}
	})
	t.Run("words between the caps take the floor", func(t *testing.T) {
		r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
		r.session.cfg.Session.OpeningText = openingLine
		r.botSpeaks()
		r.started("item-1")
		r.partial("item-1", "喂")
		waitFor(t, "the cap to run", func() bool { return r.clock.pending() == 1 })
		waitFor(t, "the partial", func() bool {
			r.session.bc.mu.Lock()
			defer r.session.bc.mu.Unlock()
			u := r.session.bc.find("item-1")
			return u != nil && u.text == "喂"
		})
		r.clock.advance(holdCap)
		waitFor(t, "the second cap to be armed", func() bool { return r.clock.pending() == 1 })
		r.partial("item-1", "，请问你是哪位")
		eventsUntil(t, r.session, EventTypeCallerSpeech, nil)
		if r.clock.pending() != 0 {
			t.Error("the second cap is still running after the floor was taken")
		}
		if got := r.model.declines(); len(got) != 0 {
			t.Errorf("declined %v, want none", got)
		}
	})
	t.Run("no text", func(t *testing.T) {
		r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
		r.session.cfg.Session.OpeningText = openingLine
		r.botSpeaks()
		r.started("item-1")
		waitFor(t, "the cap to run", func() bool { return r.clock.pending() == 1 })
		r.clock.advance(holdCap)
		eventsUntil(t, r.session, EventTypeCallerSpeech, nil)
	})
}

// The opening is owed at call level (#594, the live sequence): qwen split
// "喂你好" in two, cut the opening at the first word, created a reply and
// cancelled that at the second. The final transcript is judged against the
// reply's cut, which is still a cut of the opening.
func TestAGreetingOverACancelledReplyStillOwesTheOpening(t *testing.T) {
	t.Run("the reply had queued nothing", func(t *testing.T) {
		r := newOpeningRig(t)
		r.botSpeaksUntilCut(t, "u1") // the opening, turn 1, cut
		r.started()                  // turn 2: the reply to the first word
		r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
		waitFor(t, "the cut of the reply", func() bool {
			r.session.bc.mu.Lock()
			defer r.session.bc.mu.Unlock()
			return r.session.bc.cut != nil && r.session.bc.cut.event.Turn == 2 && r.session.bc.cut.isOpening
		})
		r.started() // turn 3: the answer to the second word
		r.toolCall("fc_1", "classify", `{"value":"agree"}`)
		r.callerFinal("u1", "hello hi.")
		r.ended()
		r.awaitResult(t)
		if out := r.lastResult(t); !strings.Contains(out, `"ok":"0"`) || !strings.Contains(out, "hello") {
			t.Errorf("tool result = %s, want a refusal that says the caller only greeted", out)
		}
		if r.node() != "offer" {
			t.Errorf("the call moved to %q", r.node())
		}
		if got := r.spoken(t, 1); !slices.Equal(got, []string{openingLine}) {
			t.Errorf("spoken = %v, want the opening once", got)
		}
	})
	t.Run("the reply had queued audio", func(t *testing.T) {
		r := newOpeningRig(t)
		r.botSpeaksUntilCut(t, "u1")
		r.leg.holdFrames(40)
		r.started()
		r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320*50)}
		r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"}
		waitFor(t, "the cut of the reply", func() bool {
			r.session.bc.mu.Lock()
			defer r.session.bc.mu.Unlock()
			return r.session.bc.cut != nil && r.session.bc.cut.event.Turn == 2 && r.session.bc.cut.isOpening
		})
		r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
		r.leg.holdFrames(0)
		r.started()
		r.toolCall("fc_1", "classify", `{"value":"agree"}`)
		r.callerFinal("u2", "hello")
		r.ended()
		r.awaitResult(t)
		if r.node() != "offer" {
			t.Errorf("the call moved to %q", r.node())
		}
		if got := r.spoken(t, 1); !slices.Equal(got, []string{openingLine}) {
			t.Errorf("spoken = %v, want the opening once", got)
		}
	})
}

// A greeting that no cut of its own judges still owes the opening while it is
// owed.
func TestAGreetingStillOwesTheOpeningWhenItsCutWasSuperseded(t *testing.T) {
	r := newOpeningRig(t)
	r.botSpeaksUntilCut(t, "u2")
	r.callerFinal("u1", "hello") // another utterance's final: no cut judges it
	waitFor(t, "the replay to wait", func() bool { return r.clock.pending() == 1 })
	r.clock.advance(replayWaitCap)
	if got := r.spoken(t, 1); !slices.Equal(got, []string{openingLine}) {
		t.Errorf("spoken = %v, want the opening", got)
	}
}

// What clears the owed opening: words, and a reply heard in full.
func TestTheOpeningIsNoLongerOwedAfterWordsOrAFullyHeardReply(t *testing.T) {
	isOwed := func(r *openingRig) bool {
		r.session.bc.mu.Lock()
		defer r.session.bc.mu.Unlock()
		return r.session.bc.isOpeningOwed
	}
	t.Run("words", func(t *testing.T) {
		r := newOpeningRig(t)
		r.botSpeaksUntilCut(t, "u1")
		if !isOwed(r) {
			t.Fatal("a cut opening is not owed")
		}
		r.started()
		r.callerFinal("u1", "I agree to renew")
		waitFor(t, "the owed mark to clear", func() bool { return !isOwed(r) })
		// A later cut and a greeting are words now.
		r.ended()
		r.leg.holdFrames(40)
		r.started()
		r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320*50)}
		r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "u2"}
		waitFor(t, "the cut", func() bool {
			r.session.bc.mu.Lock()
			defer r.session.bc.mu.Unlock()
			return r.session.bc.cut != nil && r.session.bc.cut.event.Turn == 3
		})
		r.model.events <- provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSpeech}
		r.leg.holdFrames(0)
		r.started()
		r.toolCall("fc_1", "classify", `{"value":"agree"}`)
		r.callerFinal("u2", "hello")
		r.ended()
		r.awaitResult(t)
		if r.node() != "agreed" {
			t.Errorf("the call is in %q, want a greeting after words to be words", r.node())
		}
		r.assertNothingSpoken(t)
	})
	t.Run("a reply heard in full", func(t *testing.T) {
		r := newOpeningRig(t)
		r.botSpeaksUntilCut(t, "u1")
		r.started()
		r.audio()
		r.ended()
		waitFor(t, "the owed mark to clear", func() bool { return !isOwed(r) })
	})
	t.Run("a muted reply does not", func(t *testing.T) {
		r := newOpeningRig(t)
		r.botSpeaksUntilCut(t, "u1")
		r.started()
		r.session.MuteTurn(2, "test")
		r.audio()
		r.ended()
		r.assertNothingSpoken(t)
		if !isOwed(r) {
			t.Error("a muted turn cleared the owed opening")
		}
	})
}
