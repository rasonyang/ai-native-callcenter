// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

//
// The model's own goodbye is the goodbye (#453): an unhung FAREWELL that played
// in full ends the call after a short grace (shape A), and the second one in a
// row ends it as soon as it has played (shape C), both without the closing
// target's announce. Through the whole drive loop, on a fake clock.
//

// graceClock is drive's clock for these tests: every wait is recorded and fires
// only when the test says. Unlike manualClock it can fire the newest wait of a
// duration, because a cancelled grace leaves its wait behind.
type graceClock struct {
	mu    sync.Mutex
	waits []clockWait
}

func (c *graceClock) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.waits = append(c.waits, clockWait{d, ch})
	c.mu.Unlock()
	return ch
}

func (c *graceClock) count(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, w := range c.waits {
		if w.d == d {
			n++
		}
	}
	return n
}

// awaitWaits waits until drive has asked for n waits of d in all.
func (c *graceClock) awaitWaits(t *testing.T, d time.Duration, n int) {
	t.Helper()
	waitUntil(t, "a wait of "+d.String(), func() bool { return c.count(d) >= n })
}

// fireNewest fires the most recent wait of d.
func (c *graceClock) fireNewest(t *testing.T, d time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.waits) - 1; i >= 0; i-- {
		if c.waits[i].d == d {
			c.waits[i].c <- time.Time{}
			return
		}
	}
	t.Fatalf("no wait of %v to fire", d)
}

type farewellRig struct {
	*claimsHarness
	clock *graceClock
}

// farewellFlow is farewellBackstopFlow with a tool the model can call without
// ending the call, for the streak a tool call resets.
const farewellFlow = `{
	"id": "farewell-test",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {"persona": "You answer the phone.", "closingTarget": "finish",
		"alwaysAllowedTools": ["take_message", "hangup"]},
	"nodes": {
		"welcome": {"instruction": "Greet.", "tools": []},
		"finish": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true,
			"announce": {"en": "Sorry to have bothered you, goodbye.", "zh": "不好意思打扰了，再见。"}}
	}
}`

// The same call with nowhere to go, and with a closing phase that has no line.
const (
	farewellNoTargetFlow = `{
		"id": "farewell-no-target", "specVersion": "v2", "initialNode": "welcome",
		"global": {"persona": "You answer the phone."},
		"nodes": {"welcome": {"instruction": "Greet.", "tools": []}}
	}`
	farewellNoAnnounceFlow = `{
		"id": "farewell-no-announce", "specVersion": "v2", "initialNode": "welcome",
		"global": {"persona": "You answer the phone.", "closingTarget": "finish"},
		"nodes": {
			"welcome": {"instruction": "Greet.", "tools": []},
			"finish": {"instruction": "Say goodbye.", "tools": [], "isTerminal": true}
		}
	}`
)

func newFarewellRig(t *testing.T, profile provider.Profile, flowJSON, lang string,
	setup func(h *claimsHarness)) *farewellRig {
	t.Helper()
	clock := &graceClock{}
	h := newClaimsHarnessSetup(t, profile, flowJSON, lang, "", Config{BargeGuard: -1, NoInput: -1},
		func(h *claimsHarness) {
			h.o.budgetAfter = clock.after
			if setup != nil {
				setup(h)
			}
		})
	return &farewellRig{claimsHarness: h, clock: clock}
}

// bye is a goodbye the model says in words, per language.
func bye(lang string) string {
	if lang == "zh" {
		return "好的，那祝您生活愉快，再见。"
	}
	return "Alright, thanks for your time. Goodbye."
}

var farewellLangs = []string{"en", "zh"}

// assertNeverReopened is the invariant of every case: the closing target's
// line, the goodbye cue and the silent-caller cue are never sent.
func (r *farewellRig) assertNeverReopened(t *testing.T) {
	t.Helper()
	if got := r.model.spokenLines(); len(got) != 0 {
		t.Errorf("lines asked for = %q, want none", got)
	}
	if got := r.model.recordedUserText(); len(got) != 0 {
		t.Errorf("cues sent = %q, want none", got)
	}
}

// awaitHungUp waits for the call to have been ended by the bot.
func (r *farewellRig) awaitHungUp(t *testing.T) {
	t.Helper()
	waitUntil(t, "the call to be hung up", r.session.isClosed)
}

func (r *farewellRig) assertOpen(t *testing.T, what string) {
	t.Helper()
	settle()
	if r.session.isClosed() {
		t.Errorf("%s: the call was hung up", what)
	}
}

// answered is the caller answering the goodbye: speech, then its transcript.
func (r *farewellRig) answered(text string) {
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	r.callerSays(text)
}

func TestTheGraceIsTwoSeconds(t *testing.T) {
	if ownFarewellGrace != 2*time.Second {
		t.Fatalf("ownFarewellGrace = %v, want 2s (#453)", ownFarewellGrace)
	}
}

// 1. Played in full, then silence: hung up at the playback end plus the grace,
// no line, the engine at the closing target and the record a flow end.
func TestAnUnhungGoodbyePlayedInFullThenSilenceHangsUpAfterTheGraceWithoutTheClosingLine(t *testing.T) {
	for _, lang := range farewellLangs {
		eachProfile(t, func(t *testing.T, profile provider.Profile) {
			r := newFarewellRig(t, profile, farewellFlow, lang, nil)

			r.turn(bye(lang), "", "")
			r.clock.awaitWaits(t, ownFarewellGrace, 1) // the playback has ended
			if r.session.isClosed() {
				t.Fatal("the call was hung up before the grace ran out")
			}
			r.clock.fireNewest(t, ownFarewellGrace)
			r.awaitHungUp(t)

			if got := r.runtime.Engine().NodeID(); got != "finish" {
				t.Errorf("node = %q, want the closing target", got)
			}
			r.assertNeverReopened(t)
			if r.logs.count("hanging up without the closing line") != 1 {
				t.Errorf("the rule was not logged once:\n%s", r.logs.String())
			}
			if strings.Contains(r.logs.String(), bye(lang)) {
				t.Error("the log carries the transcript")
			}
			cdr := r.awaitEnd(t)
			if !cdr.IsContained {
				t.Error("a call the bot ended was not marked contained")
			}
			if got := r.sw.indexOf("set chan-9 aicc_bot_finished=FLOW_END"); got < 0 {
				t.Errorf("the dialplan was not told the flow ended: %v", r.sw.recorded())
			}
		})
	}
}

// 2. The caller talks over the goodbye (400 ms in, simulated by a held tail):
// it does not count, and the conversation goes on.
func TestAGoodbyeTheCallerTalkedOverDoesNotCount(t *testing.T) {
	for _, lang := range farewellLangs {
		eachProfile(t, func(t *testing.T, profile provider.Profile) {
			r := newFarewellRig(t, profile, farewellFlow, lang, nil)

			release := r.heldTurn(bye(lang), "", "")
			r.awaitLog(t, unbackedClaimLog)
			r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
			waitUntil(t, "the barge-in", func() bool { return r.leg.clears() > 0 })
			release()
			r.callerSays("wait")

			r.assertOpen(t, "a goodbye cut by the caller")
			if n := r.clock.count(ownFarewellGrace); n != 0 {
				t.Errorf("%d graces started for a goodbye nobody heard to the end", n)
			}
			if r.runtime.Engine().IsTerminal() {
				t.Error("the flow moved on a goodbye the caller talked over")
			}
			r.assertNeverReopened(t)
		})
	}
}

// 3. The caller speaks inside the grace: no hangup at the grace, however late it
// fires.
func TestACallerWhoSpeaksInsideTheGraceKeepsTheCallOpen(t *testing.T) {
	for _, lang := range farewellLangs {
		eachProfile(t, func(t *testing.T, profile provider.Profile) {
			r := newFarewellRig(t, profile, farewellFlow, lang, nil)

			r.turn(bye(lang), "", "")
			r.clock.awaitWaits(t, ownFarewellGrace, 1)
			// A speech start alone, no transcript yet: a caller mid-sentence.
			r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
			settle()
			r.clock.fireNewest(t, ownFarewellGrace)

			r.assertOpen(t, "a caller speaking inside the grace")
			if r.runtime.Engine().IsTerminal() {
				t.Error("the flow moved while the caller was speaking")
			}
			r.assertNeverReopened(t)
		})
	}
}

// 4. The observed call (shape C report): the caller starts speaking 6 s after
// the goodbye's playback end. The grace is far shorter, so the call is over
// before they do.
func TestTheObservedCallIsEndedByTheGraceBeforeTheCallerSpeaks(t *testing.T) {
	r := newFarewellRig(t, provider.QwenProfile(), farewellFlow, "zh", nil)

	r.turn(bye("zh"), "", "")
	r.clock.awaitWaits(t, ownFarewellGrace, 1)
	if ownFarewellGrace >= 6*time.Second {
		t.Fatalf("the grace %v does not end the call before a caller's 6 s answer", ownFarewellGrace)
	}
	r.clock.fireNewest(t, ownFarewellGrace)
	r.awaitHungUp(t)
	r.assertNeverReopened(t)
}

// 5. Shape C: a caller who answers the goodbye inside the grace gets the
// second goodbye hung up on, at its playback end, with no wait.
func TestTheSecondUnhungGoodbyeInARowHangsUpAtItsPlaybackEnd(t *testing.T) {
	for _, lang := range farewellLangs {
		eachProfile(t, func(t *testing.T, profile provider.Profile) {
			r := newFarewellRig(t, profile, farewellFlow, lang, nil)

			r.turn(bye(lang), "", "")
			r.clock.awaitWaits(t, ownFarewellGrace, 1)
			r.answered("你怎么不挂？")
			r.assertOpen(t, "a caller answering the first goodbye")

			r.turn(bye(lang), "", "")
			r.awaitHungUp(t) // no clock fired: the playback end is the hangup

			if n := r.clock.count(ownFarewellGrace); n != 1 {
				t.Errorf("%d graces started, want only the first goodbye's", n)
			}
			if got := r.runtime.Engine().NodeID(); got != "finish" {
				t.Errorf("node = %q, want the closing target", got)
			}
			r.assertNeverReopened(t)
			if r.logs.count("said goodbye twice without hanging up") != 1 {
				t.Errorf("the rule was not logged once:\n%s", r.logs.String())
			}
		})
	}
}

// 6. A second goodbye the caller cut neither counts nor resets; the third that
// plays in full is the second.
func TestACutGoodbyeNeitherCountsNorResetsTheStreak(t *testing.T) {
	for _, lang := range farewellLangs {
		eachProfile(t, func(t *testing.T, profile provider.Profile) {
			r := newFarewellRig(t, profile, farewellFlow, lang, nil)

			r.turn(bye(lang), "", "")
			r.clock.awaitWaits(t, ownFarewellGrace, 1)
			r.answered("嗯")
			settle()

			release := r.heldTurn(bye(lang), "", "")
			r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
			waitUntil(t, "the barge-in", func() bool { return r.leg.clears() > 0 })
			release()
			r.callerSays("等一下")
			r.assertOpen(t, "a second goodbye the caller cut")

			r.turn(bye(lang), "", "")
			r.awaitHungUp(t)
			r.assertNeverReopened(t)
		})
	}
}

// 7. A non-goodbye turn between two goodbyes resets the streak: the second is
// the first of a new streak and shape A's grace decides.
func TestAnAnswerBetweenTwoGoodbyesResetsTheStreak(t *testing.T) {
	for _, lang := range farewellLangs {
		eachProfile(t, func(t *testing.T, profile provider.Profile) {
			r := newFarewellRig(t, profile, farewellFlow, lang, nil)

			r.turn(bye(lang), "", "")
			r.clock.awaitWaits(t, ownFarewellGrace, 1)
			r.answered("那个套餐多少钱？")
			settle()
			r.turn("The plan is ten dollars a month. 套餐每月十元。", "", "")
			settle()

			r.turn(bye(lang), "", "")
			r.clock.awaitWaits(t, ownFarewellGrace, 2)
			r.assertOpen(t, "a second goodbye after an answer")
			r.clock.fireNewest(t, ownFarewellGrace)
			r.awaitHungUp(t)
			r.assertNeverReopened(t)
		})
	}
}

// 8. A tool call between two goodbyes resets the streak too.
func TestAToolCallBetweenTwoGoodbyesResetsTheStreak(t *testing.T) {
	r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, "en", nil)

	r.turn(bye("en"), "", "")
	r.clock.awaitWaits(t, ownFarewellGrace, 1)
	r.answered("Actually, leave a message for me.")
	settle()
	r.turn("", flow.ToolTakeMessage, `{"message":"call me back"}`)
	r.awaitToolResults(t, 1)
	r.turn("Noted. Anything else?", "", "")
	settle()

	r.turn(bye("en"), "", "")
	r.clock.awaitWaits(t, ownFarewellGrace, 2)
	r.assertOpen(t, "a goodbye after a tool call")
	r.clock.fireNewest(t, ownFarewellGrace)
	r.awaitHungUp(t)
	if got := r.model.spokenLines(); len(got) != 0 {
		t.Errorf("lines asked for = %q, want none", got)
	}
}

// 9. A goodbye whose transcript lands after its turn is done: before its
// playback ends, or after.
func TestAGoodbyeWhoseTranscriptLandsAfterTurnDoneStillCounts(t *testing.T) {
	late := func(r *farewellRig, text string, isHeld bool) (release func()) {
		if isHeld {
			r.leg.holdFrames(3)
		}
		r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320)}
		r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		return func() { r.leg.holdFrames(0) }
	}
	for _, lang := range farewellLangs {
		t.Run("after the playback/"+lang, func(t *testing.T) {
			r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, lang, nil)
			late(r, bye(lang), false)
			settle() // turn done and played, no words yet
			r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
				Text: bye(lang), IsFinal: true}
			r.clock.awaitWaits(t, ownFarewellGrace, 1)
			r.clock.fireNewest(t, ownFarewellGrace)
			r.awaitHungUp(t)
			r.assertNeverReopened(t)
		})
		t.Run("before the playback ends/"+lang, func(t *testing.T) {
			r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, lang, nil)
			release := late(r, bye(lang), true)
			settle()
			r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
				Text: bye(lang), IsFinal: true}
			r.awaitLog(t, unbackedClaimLog)
			release()
			r.clock.awaitWaits(t, ownFarewellGrace, 1)
			r.clock.fireNewest(t, ownFarewellGrace)
			r.awaitHungUp(t)
			r.assertNeverReopened(t)
		})
	}
	t.Run("caller spoke before the words landed", func(t *testing.T) {
		r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, "en", nil)
		late(r, bye("en"), false)
		settle()
		r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
		settle()
		r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript,
			Text: bye("en"), IsFinal: true}
		r.assertOpen(t, "a late transcript after the caller spoke")
		if n := r.clock.count(ownFarewellGrace); n != 0 {
			t.Errorf("%d graces started for a caller already speaking", n)
		}
	})
}

// 10. A campaign call is included. The attempt ends as any bot-ended call does:
// answered, contained, not failed, and classified ANSWERED.
func TestACampaignCallIsHungUpOnItsOwnGoodbyeAndTheAttemptIsAnswered(t *testing.T) {
	for _, lang := range farewellLangs {
		r := newFarewellRig(t, provider.QwenProfile(), farewellFlow, lang, func(h *claimsHarness) {
			h.actions.campaignID = "c-1"
			h.actions.attemptID = "a-1"
		})

		r.turn(bye(lang), "", "")
		r.clock.awaitWaits(t, ownFarewellGrace, 1)
		r.clock.fireNewest(t, ownFarewellGrace)
		r.awaitHungUp(t)
		r.assertNeverReopened(t)

		cdr := r.awaitEnd(t)
		if cdr.Status == store.CDRStatusFailed || !cdr.IsContained {
			t.Errorf("%s: cdr = %+v, want an answered, contained call", lang, cdr)
		}
		got := campaign.Classify(campaign.CallEnd{HangupCause: cdr.HangupCause, IsAnswered: true,
			IsFailed: cdr.Status == store.CDRStatusFailed, HasBot: true})
		if got != campaign.OutcomeAnswered {
			t.Errorf("%s: attempt outcome = %v, want ANSWERED", lang, got)
		}
	}
}

// 11. No closing target, a closing phase with no line, a call already in a
// terminal phase, and doubao (whose terminal announce is its own rule): hung up
// after the grace, no second line, no cue.
func TestWhereThereIsNoClosingLineTheCallIsStillHungUpWithoutAskingForOne(t *testing.T) {
	cases := []struct {
		name    string
		profile provider.Profile
		flow    string
		setup   func(h *claimsHarness)
		node    string
	}{
		{"no closing target", provider.OpenAIProfile(), farewellNoTargetFlow, nil, "welcome"},
		{"closing phase with no announce", provider.OpenAIProfile(), farewellNoAnnounceFlow, nil, "finish"},
		{"already terminal", provider.OpenAIProfile(), farewellFlow, func(h *claimsHarness) {
			h.runtime.CloseAtUnhungFarewell()
		}, "finish"},
		{"doubao", provider.DoubaoProfile(), farewellFlow, nil, "finish"},
	}
	for _, c := range cases {
		for _, lang := range farewellLangs {
			t.Run(c.name+"/"+lang, func(t *testing.T) {
				r := newFarewellRig(t, c.profile, c.flow, lang, c.setup)

				r.turn(bye(lang), "", "")
				r.clock.awaitWaits(t, ownFarewellGrace, 1)
				r.clock.fireNewest(t, ownFarewellGrace)
				r.awaitHungUp(t)

				r.assertNeverReopened(t)
				if got := r.runtime.Engine().NodeID(); got != c.node {
					t.Errorf("node = %q, want %q", got, c.node)
				}
			})
		}
	}
}

// An ending already armed keeps its own behaviour: the goodbye of the hangup
// tool's own closing turn is backed, and a later one does not start a grace.
func TestAnArmedEndingIsLeftAlone(t *testing.T) {
	r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, "en", nil)
	r.actions.arm(t.Context(), func() {})

	r.turn(bye("en"), "", "")
	settle()
	if n := r.clock.count(ownFarewellGrace); n != 0 {
		t.Errorf("%d graces started over an armed ending", n)
	}
	r.assertOpen(t, "an armed ending")
	r.actions.disarm()
}

// The dead-air backstop remains the fallback and closes the same way.
func TestTheDeadAirBackstopStillCatchesAGoodbyeTheGraceDidNot(t *testing.T) {
	r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, "en", nil)

	r.turn(bye("en"), "", "")
	r.clock.awaitWaits(t, ownFarewellGrace, 1)
	// The grace never fires here; the session's silence watch does.
	r.session.emit(Event{Type: EventTypeNoInput})
	r.awaitHungUp(t)
	r.assertNeverReopened(t)
	if !slices.ContainsFunc([]string{r.logs.String()}, func(s string) bool {
		return strings.Contains(s, "the caller went quiet")
	}) {
		t.Errorf("the backstop was not logged:\n%s", r.logs.String())
	}
}

// A cut goodbye is not left standing for the dead-air backstop either.
func TestTheBackstopDoesNotCountAGoodbyeTheCallerTalkedOver(t *testing.T) {
	r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, "en", nil)

	release := r.heldTurn(bye("en"), "", "")
	r.awaitLog(t, unbackedClaimLog)
	r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
	waitUntil(t, "the barge-in", func() bool { return r.leg.clears() > 0 })
	release()
	settle()

	r.session.emit(Event{Type: EventTypeNoInput})
	texts := r.awaitUserTexts(t, 1)
	if !strings.Contains(texts[0], silentCueMarker) {
		t.Errorf("cue = %q, want the ordinary silent-caller cue", texts[0])
	}
	if r.session.isClosed() || r.runtime.Engine().IsTerminal() {
		t.Error("the backstop ended the call on a goodbye the caller talked over")
	}
}

// A closing question expects an answer: 2 s of thinking is normal, so there is
// no grace, no count and no backstop hangup; the 8 s path sends the ordinary
// silent-caller cue.
func TestATurnThatEndsInAQuestionStartsNoGraceAndIsNotHungUpOn(t *testing.T) {
	lines := map[string]string{
		"en": "Thanks for your cooperation. Is there anything else I can help with?",
		"zh": "感谢您的配合。请问还有其他问题吗？",
	}
	for _, lang := range farewellLangs {
		eachProfile(t, func(t *testing.T, profile provider.Profile) {
			r := newFarewellRig(t, profile, farewellFlow, lang, nil)

			r.turn(lines[lang], "", "")
			settle()
			if n := r.clock.count(ownFarewellGrace); n != 0 {
				t.Errorf("%d graces started for a turn that asks a question", n)
			}
			if r.logs.count(unbackedClaimLog) != 0 {
				t.Error("the question was reported as an unbacked goodbye")
			}
			r.assertOpen(t, "a closing question")

			r.session.emit(Event{Type: EventTypeNoInput})
			texts := r.awaitUserTexts(t, 1)
			if !strings.Contains(texts[0], "silent") && !strings.Contains(texts[0], "没有说话") {
				t.Errorf("cue = %q, want the ordinary silent-caller cue", texts[0])
			}
			if r.session.isClosed() || r.runtime.Engine().IsTerminal() {
				t.Error("the backstop ended a call that had asked a question")
			}
		})
	}
	t.Run("a goodbye after an earlier question still starts the grace", func(t *testing.T) {
		r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, "en", nil)
		r.turn("Is there anything else? Okay, goodbye.", "", "")
		r.clock.awaitWaits(t, ownFarewellGrace, 1)
	})
}

// A goodbye the caller cut whose transcript lands after the barge-in neither
// counts nor resets the streak: the third goodbye is the second.
func TestACutGoodbyeWhoseTranscriptLandsLateDoesNotResetTheStreak(t *testing.T) {
	for _, lang := range farewellLangs {
		r := newFarewellRig(t, provider.OpenAIProfile(), farewellFlow, lang, nil)

		r.turn(bye(lang), "", "")
		r.clock.awaitWaits(t, ownFarewellGrace, 1)
		r.answered("嗯")
		settle()

		// Goodbye 2: done with no words yet, cut over its tail, words after.
		r.leg.holdFrames(3)
		r.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
		r.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320)}
		r.model.events <- provider.Event{Type: provider.EventTypeResponseDone}
		settle()
		r.model.events <- provider.Event{Type: provider.EventTypeSpeechStarted}
		waitUntil(t, "the barge-in", func() bool { return r.leg.clears() > 0 })
		r.leg.holdFrames(0)
		r.model.events <- provider.Event{Type: provider.EventTypeOutputTranscript, Text: bye(lang), IsFinal: true}
		r.callerSays("等一下")
		r.assertOpen(t, "a cut goodbye with a late transcript")

		r.turn(bye(lang), "", "")
		r.awaitHungUp(t)
		r.assertNeverReopened(t)
	}
}
