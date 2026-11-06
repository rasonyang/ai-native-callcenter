// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// A caller turn the engine dropped, with words in it (#448), through the whole
// drive loop. The NO_INPUT event is emitted as the session emits it for a turn
// measurement that closed unanswered; the session half is pinned in
// turn_unanswered_test.go.
//

const (
	answerCueEN = "(The caller spoke and got no reply. Answer what they last said; " +
		"if you did not catch it, briefly ask them to repeat it.)"
	answerCueZH = "（来电者说了话，但没有得到回复。请回答对方刚才说的话；如果你没有听清，请简短地请对方再说一遍。）"

	silentCueMarker = "The caller has been silent"
)

// droppedTurn is a NO_INPUT for a caller turn the engine never answered.
func droppedTurn(words string) Event {
	return Event{Type: EventTypeNoInput, IsCallerUnanswered: true, Text: words}
}

// noInputCount is the flow's count of consecutive silences.
func noInputCount(t *testing.T, engine *flow.Engine) int {
	t.Helper()
	v, _ := engine.Slot("noInput.count")
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case nil:
		return 0
	}
	t.Fatalf("noInput.count = %v (%T)", v, v)
	return 0
}

func TestACallerTurnTheEngineDroppedGetsTheAnswerCueAndIsNotCountedAsSilence(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		h := newClaimsHarnessOn(t, profile, bareCueFlow, "en", "", Config{BargeGuard: -1, NoInput: -1})

		h.session.emit(droppedTurn(rma1003Words))
		texts := h.awaitUserTexts(t, 1)
		if texts[0] != answerCueEN {
			t.Errorf("cue = %q, want the answer cue", texts[0])
		}
		if strings.Contains(texts[0], silentCueMarker) {
			t.Errorf("a caller who spoke was told to be silent: %q", texts[0])
		}
		if n := noInputCount(t, h.runtime.Engine()); n != 0 {
			t.Errorf("noInput.count = %d, want 0", n)
		}
	})
}

func TestThreeDroppedTurnsInARowAreAnsweredEachTimeAndNeverCloseTheCall(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		h := newClaimsHarnessOn(t, profile, farewellBackstopFlow, "en", "", Config{BargeGuard: -1, NoInput: -1})

		for i := 1; i <= 3; i++ {
			h.session.emit(droppedTurn(rma1003Words))
			texts := h.awaitUserTexts(t, i)
			if texts[i-1] != answerCueEN {
				t.Errorf("cue %d = %q, want the answer cue", i, texts[i-1])
			}
		}
		settle()
		if h.runtime.Engine().IsTerminal() || h.actions.isArmed() {
			t.Error("three answered drops closed the call through no-input")
		}
		if n := noInputCount(t, h.runtime.Engine()); n != 0 {
			t.Errorf("noInput.count = %d, want 0", n)
		}
	})
}

func TestTheSameSpeechWithAnEmptyTranscriptStaysASilenceUntilTheClosingTarget(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		h := newClaimsHarnessOn(t, profile, farewellBackstopFlow, "en", "", Config{BargeGuard: -1, NoInput: -1})
		h.model.answerLinesWithATurn(h.session)

		for i := 1; i <= 2; i++ {
			h.session.emit(droppedTurn(""))
			texts := h.awaitUserTexts(t, i)
			if !strings.Contains(texts[i-1], silentCueMarker) {
				t.Errorf("cue %d = %q, want the silent-caller cue", i, texts[i-1])
			}
			// The cue is sent after the flow was told, so the count is settled.
			if n := noInputCount(t, h.runtime.Engine()); n != i {
				t.Errorf("noInput.count = %d after %d silences", n, i)
			}
		}
		h.session.emit(droppedTurn(""))
		waitFor(t, "the closing line", func() bool {
			return slices.Contains(h.model.spokenLines(), "Sorry to have bothered you, goodbye.")
		})
		if !h.runtime.Engine().IsTerminal() {
			t.Error("the flow did not reach its closing target")
		}
	})
}

func TestACallerWhoNeverSpokeKeepsTodaysCueAndCount(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		h := newClaimsHarnessOn(t, profile, farewellBackstopFlow, "en", "", Config{BargeGuard: -1, NoInput: -1})

		h.session.emit(Event{Type: EventTypeNoInput})
		texts := h.awaitUserTexts(t, 1)
		if !strings.Contains(texts[0], silentCueMarker) || !strings.Contains(texts[0], "still there") {
			t.Errorf("cue = %q, want the silent-caller cue", texts[0])
		}
		if n := noInputCount(t, h.runtime.Engine()); n != 1 {
			t.Errorf("noInput.count = %d, want 1", n)
		}
	})
}

// A settling handover has the floor, and a time limit waiting for it is
// enforced: the existing branches win over a caller's words.
func TestAnUnansweredTurnWithWordsDuringASettlingHandoverSendsNothing(t *testing.T) {
	h := newHandoverCall(t)
	h.transferStalls(t)
	before := len(h.model.recordedUserText())

	h.session.emit(droppedTurn(rma1003Words))
	settle()

	if got := h.model.recordedUserText(); len(got) != before {
		t.Errorf("texts sent = %q, want nothing new while the handover settles", got[before:])
	}
	if !h.actions.isSettlingHandover() {
		t.Error("the handover stopped settling")
	}
}

func TestAnUnansweredTurnWithWordsDoesNotStopATimeLimitWaitingForTheFloor(t *testing.T) {
	h, _ := driveHarness(t, sessionBudget{limitAfter: 20 * time.Millisecond, floorWait: time.Minute})
	// The bot holds the floor past the limit, so the limit waits.
	h.model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	h.model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, 320)}
	waitFor(t, "the bot to hold the floor", h.session.isHoldingTheFloor)
	settle()

	h.session.emit(droppedTurn(rma1003Words))
	waitFor(t, "the limit to be enforced", func() bool { return h.sw.variable("aicc_bot_reason") == "SESSION_LIMIT" })
	if slices.Contains(h.model.recordedUserText(), answerCueEN) {
		t.Error("the answer cue was sent instead of enforcing the limit")
	}
}

// One merged cue: the claim cue, with the caller's words and without the
// silent prefix. The claim's bookkeeping is as for any NO_INPUT cue.
func TestAStandingClaimAndAnUnansweredTurnWithWordsGetOneMergedCue(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		h := newClaimsHarnessOn(t, profile, lookupCueFlow, "en", repairBackend(t),
			Config{BargeGuard: -1, NoInput: -1})

		h.turn("Let me check on that repair for you.", "", "")
		h.awaitLog(t, unbackedClaimLog)
		// The PLAYBACK_DONE re-ask is not what is under test.
		h.awaitUserTexts(t, 1)
		h.turn("Let me check on that repair for you.", "", "")
		h.awaitLog(t, "outcome=NOT_BACKED")
		settle()
		sent := len(h.model.recordedUserText())

		h.session.emit(droppedTurn(rma1003Words))
		texts := h.awaitUserTexts(t, sent+1)
		got := texts[sent]
		want := "(Your last reply told the caller \"let me check\", but no lookup ran, so nothing is being looked up " +
			"and the caller is waiting. Call repair_status now, without saying anything first. " +
			"If you do not have what it needs (for example which order), ask the caller for it instead. " +
			"Do not say again that you are checking. " +
			"The caller also said \"" + rma1003Words + "\" and got no reply; answer that too.)"
		if got != want {
			t.Errorf("merged cue = %q\nwant        %q", got, want)
		}
		if !strings.Contains(h.logs.String(), "trigger=NO_INPUT") {
			t.Errorf("the claim cue was not logged with trigger NO_INPUT:\n%s", h.logs.String())
		}
		if n := noInputCount(t, h.runtime.Engine()); n != 0 {
			t.Errorf("noInput.count = %d, want 0", n)
		}
		// Only one cue went out for this dead air.
		settle()
		if len(h.model.recordedUserText()) != sent+1 {
			t.Errorf("texts = %q, want exactly one cue for the dead air", h.model.recordedUserText()[sent:])
		}
	})
}

func TestAStandingClaimAndAnUnansweredTurnWithNoWordsGetTheClaimCueAloneAndCountTheSilence(t *testing.T) {
	h := newCueHarness(t, lookupCueFlow, "en")

	h.turn("Let me check on that repair for you.", "", "")
	h.awaitUserTexts(t, 1)
	h.turn("Let me check on that repair for you.", "", "")
	h.awaitLog(t, "outcome=NOT_BACKED")
	settle()

	h.session.emit(droppedTurn(""))
	got := h.awaitUserTexts(t, 2)[1]
	if !strings.HasPrefix(got, "(The caller has been silent, waiting for you. Your last reply told") ||
		strings.Contains(got, "also said") {
		t.Errorf("cue = %q, want the claim cue alone with the silent prefix", got)
	}
	if n := noInputCount(t, h.runtime.Engine()); n != 1 {
		t.Errorf("noInput.count = %d, want 1", n)
	}
}

// An unhung farewell followed by a dropped turn with words: the caller is not
// silent, the call is not closed.
func TestAnUnhungFarewellFollowedByAnUnansweredTurnWithWordsIsAnsweredNotHungUp(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		h := newClaimsHarnessOn(t, profile, farewellBackstopFlow, "en", "", Config{BargeGuard: -1, NoInput: -1})
		h.model.answerLinesWithATurn(h.session)

		h.turn("Alright, thanks for your time. Goodbye.", "", "")
		h.awaitLog(t, unbackedClaimLog)
		settle()

		h.session.emit(droppedTurn("Wait, one more thing about my order."))
		texts := h.awaitUserTexts(t, 1)
		if texts[0] != answerCueEN {
			t.Errorf("cue = %q, want the answer cue", texts[0])
		}
		settle()
		if h.runtime.Engine().IsTerminal() || h.actions.isArmed() {
			t.Error("a caller who spoke after the goodbye was hung up on")
		}
		if got := h.model.spokenLines(); len(got) != 0 {
			t.Errorf("lines spoken = %q, want none", got)
		}
	})
}

func TestAnUnhungFarewellFollowedByAnUnansweredTurnWithNoWordsRunsTheBackstop(t *testing.T) {
	eachProfile(t, func(t *testing.T, profile provider.Profile) {
		h := newClaimsHarnessOn(t, profile, farewellBackstopFlow, "en", "", Config{BargeGuard: -1, NoInput: -1})
		h.model.answerLinesWithATurn(h.session)

		h.turn("Alright, thanks for your time. Goodbye.", "", "")
		h.awaitLog(t, unbackedClaimLog)
		settle()

		h.session.emit(droppedTurn(""))
		waitFor(t, "the closing line", func() bool {
			return slices.Contains(h.model.spokenLines(), "Sorry to have bothered you, goodbye.")
		})
		if !h.runtime.Engine().IsTerminal() {
			t.Error("the flow did not reach its closing target")
		}
	})
}

func TestAChineseFlowGetsTheChineseAnswerCueAndMergedCue(t *testing.T) {
	t.Run("answer cue", func(t *testing.T) {
		h := newCueHarness(t, bareCueFlow, "zh")
		h.session.emit(droppedTurn("我的维修进度怎么样了，单号是 RMA1003？"))
		if got := h.awaitUserTexts(t, 1)[0]; got != answerCueZH {
			t.Errorf("cue = %q, want the zh answer cue", got)
		}
	})
	t.Run("merged cue", func(t *testing.T) {
		h := newCueHarness(t, lookupCueFlow, "zh")
		h.turn(zhLookupClaim, "", "")
		h.awaitUserTexts(t, 1)
		h.turn(zhLookupClaim, "", "")
		h.awaitLog(t, "outcome=NOT_BACKED")
		settle()

		h.session.emit(droppedTurn("单号是 RMA1003。"))
		got := h.awaitUserTexts(t, 2)[1]
		want := "（你上一轮对来电者说了“我帮您查询”，但没有调用任何查询工具，查询并没有发生，来电者在等。" +
			"现在就调用 repair_status，调用之前不要说话。如果不知道要查什么，就直接问来电者。不要再说你在查。" +
			"来电者还说了“单号是 RMA1003。”，但没有得到回复，请一并回答。）"
		if got != want {
			t.Errorf("merged cue = %q\nwant        %q", got, want)
		}
	})
}

// The no-tool variants of the claim cue carry the caller's words too.
func TestEveryClaimCueVariantCarriesTheCallersWords(t *testing.T) {
	c := claim{kind: "LOOKUP", phrase: "let me check"}
	for _, tools := range [][]string{nil, {"repair_status"}} {
		en := claimCue(flow.LangEN, c, tools, true, "RMA1003")
		if strings.Contains(en, "has been silent") || !strings.HasSuffix(en, " The caller also said \"RMA1003\" and got no reply; answer that too.)") {
			t.Errorf("en cue (%v) = %q", tools, en)
		}
		zh := claimCue(flow.LangZH, c, tools, true, "RMA1003")
		if strings.Contains(zh, "一直没有说话") || !strings.HasSuffix(zh, "来电者还说了“RMA1003”，但没有得到回复，请一并回答。）") {
			t.Errorf("zh cue (%v) = %q", tools, zh)
		}
	}
}
