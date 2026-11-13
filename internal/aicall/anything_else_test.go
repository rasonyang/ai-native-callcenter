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
// Issue #555: after a refused hangup (#484), the refusal never asks for a turn
// on a model that answers quietly, and the first dead air after an answer that
// ended in a statement gets the anything-else cue in place of the silent-caller
// cue, whose "repeat the current question" had qwen flash say the answer again.
// Each test says what the model was sent, in order: one turn asked for per
// dead air, none by the refusal.
//

// deadAirConfig watches for dead air soon after the bot's audio has played.
func deadAirConfig() Config { return Config{BargeGuard: -1, NoInput: 150 * time.Millisecond} }

// isSilentCallerCue reports the generic silent-caller cue, in either language.
func isSilentCallerCue(text string) bool {
	return strings.Contains(text, silentCueMarker) || strings.Contains(text, "来电者一直没有说话")
}

// What the model says to the anything-else cue, and to the silent-caller cue
// after it.
var (
	anythingElseReply = map[string]string{"en": "Is there anything else I can help you with?", "zh": "请问还有其他需要吗？"}
	stillThereReply   = map[string]string{"en": "Are you still there?", "zh": "请问您还在吗？"}
	followUpQuestion  = map[string]string{"en": "And how much is the fibre plan?", "zh": "那宽带多少钱？"}
	followUpAnswer    = map[string]string{"en": "The fibre plan is fifty a month.", "zh": "宽带每月五十元。"}
	guardClosingLine  = map[string]string{"en": "Thank you for calling NovaNet. Goodbye.", "zh": "感谢致电，再见。"}
)

const anythingElseLog = "dead air after the answer of a refused hangup"

// (c) The answer ended in a statement and the caller stays silent: the dead
// air gets the anything-else cue, once. The next dead air gets the
// silent-caller cue, and the third in a row closes the call through the
// closing target, as ever: the cue's words differ, the count does not.
func TestADeadAirAfterARefusedStatementGetsTheAnythingElseCueOnce(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h := newGuardHarnessWith(t, provider.QwenProfile(), guardFlow, lang, true, deadAirConfig())

			h.answerAndHangUp(lang, false, goodbyeHangup)
			h.awaitSends(t, "SendToolResultQuietly,SendUserText")
			if got := h.model.recordedUserText()[0]; got != anythingElseCue(lang) {
				t.Errorf("cue = %q, want the anything-else cue", got)
			}
			// The cue is sent after the flow was told, so the count is settled.
			if n := noInputCount(t, h.runtime.Engine()); n != 1 {
				t.Errorf("noInput.count = %d after the first silence, want 1", n)
			}

			h.answered(anythingElseReply[lang])
			h.awaitSends(t, "SendToolResultQuietly,SendUserText,SendUserText")
			if got := h.model.recordedUserText()[1]; !isSilentCallerCue(got) {
				t.Errorf("second cue = %q, want the silent-caller cue", got)
			}
			if n := noInputCount(t, h.runtime.Engine()); n != 2 {
				t.Errorf("noInput.count = %d after the second silence, want 2", n)
			}

			h.answered(stillThereReply[lang])
			h.awaitSends(t, "SendToolResultQuietly,SendUserText,SendUserText,SpeakText")
			if got := h.model.spokenLines(); len(got) != 1 || got[0] != guardClosingLine[lang] {
				t.Errorf("lines = %q, want the closing target's line", got)
			}
			if h.node() != "farewell" || !h.actions.isArmed() {
				t.Errorf("node %q, armed %v: want the call closing", h.node(), h.actions.isArmed())
			}
			if n := strings.Count(h.logs.String(), anythingElseLog); n != 1 {
				t.Errorf("the anything-else cue was logged %d times, want once:\n%s", n, h.logs.String())
			}
		})
	}
}

// (b) The answer ended in a question and the caller stays silent: the
// refusal goes quietly and the dead air gets the silent-caller cue, which
// repeats the question the caller was asked.
func TestADeadAirAfterARefusedQuestionGetsTheSilentCallerCue(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h := newGuardHarnessWith(t, provider.QwenProfile(), guardFlow, lang, true, deadAirConfig())

			h.answerAndHangUp(lang, true, goodbyeHangup)
			h.awaitSends(t, "SendToolResultQuietly,SendUserText")
			if got := h.model.recordedUserText()[0]; !isSilentCallerCue(got) {
				t.Errorf("cue = %q, want the silent-caller cue", got)
			}
			if strings.Contains(h.logs.String(), anythingElseLog) {
				t.Errorf("the anything-else cue was used after a question:\n%s", h.logs.String())
			}
		})
	}
}

// (a) The answer ended in a statement and the caller speaks next: the quiet
// refusal is all the model is sent, and the reply to the caller is the
// provider's own turn. The answer and the reply are held on the line, so no
// dead air can come.
//
// (d) The caller then goes silent after the reply to them: that dead air
// follows the reply, not the answer, and gets the silent-caller cue.
func TestARefusedStatementThenTheCallersWordsLeavesTheSilentCallerCue(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		t.Run(lang, func(t *testing.T) {
			h := newGuardHarnessWith(t, provider.QwenProfile(), guardFlow, lang, true, deadAirConfig())

			h.leg.holdFrames(1)
			h.answerAndHangUp(lang, false, goodbyeHangup)
			h.awaitSends(t, "SendToolResultQuietly")
			h.callerSays(followUpQuestion[lang])
			h.answered(followUpAnswer[lang])
			// The reply's turn has begun, and with it the caller's words before
			// it have been handled: the answer's playback can no longer end and
			// start a dead-air watch of its own once the line is released.
			waitFor(t, "the reply's turn", func() bool { return h.session.currentTurn() == 2 })
			settle()
			if got := h.sends(); got != "SendToolResultQuietly" {
				t.Fatalf("sends = %s, want the quiet refusal alone: the caller spoke next", got)
			}

			h.leg.holdFrames(0)
			h.awaitSends(t, "SendToolResultQuietly,SendUserText")
			if got := h.model.recordedUserText()[0]; !isSilentCallerCue(got) {
				t.Errorf("cue = %q, want the silent-caller cue", got)
			}
			if strings.Contains(h.logs.String(), anythingElseLog) {
				t.Errorf("the anything-else cue was used after the caller spoke:\n%s", h.logs.String())
			}
		})
	}
}

// A NO_INPUT rule that moves the call to its goodbye takes the silence: the
// closing line is said and no cue is sent.
func TestASilenceThatClosesTheCallAfterARefusedStatementSaysTheLineAndNoCue(t *testing.T) {
	closesOnSilence := strings.Replace(guardFlow,
		`"welcome": {"instruction": "Help the caller.", "tools": ["note"]}`,
		`"welcome": {"instruction": "Help the caller.", "tools": ["note"],
			"transitions": [{"on": "NO_INPUT", "target": "farewell"}]}`, 1)
	if closesOnSilence == guardFlow {
		t.Fatal("the NO_INPUT rule was not added to the flow")
	}
	h := newGuardHarnessWith(t, provider.QwenProfile(), closesOnSilence, "en", true, deadAirConfig())

	h.answerAndHangUp("en", false, goodbyeHangup)
	h.awaitSends(t, "SendToolResultQuietly,SpeakText")
	settle()
	if got := h.sends(); got != "SendToolResultQuietly,SpeakText" {
		t.Errorf("sends = %s, want the closing line and no cue", got)
	}
	if got := h.model.spokenLines(); len(got) != 1 || got[0] != guardClosingLine["en"] {
		t.Errorf("lines = %q, want the closing line", got)
	}
}

// After a quiet refusal the reply bookkeeping is as after any quiet close: the
// reply to the anything-else cue is the model's own words, judged for claims,
// and it is no reply to the caller, so it does not count towards
// maxTurnsWithoutTool. With a limit of one, the call closes on the second
// tool-less reply to the caller, not on the first.
func TestTheReplyToTheAnythingElseCueIsJudgedAndNotCountedTowardsTheWall(t *testing.T) {
	withWall := strings.Replace(guardFlow, `"closingTarget": "farewell",`,
		`"closingTarget": "farewell", "maxTurnsWithoutTool": 1,`, 1)
	if withWall == guardFlow {
		t.Fatal("the wall was not added to the flow")
	}
	h := newGuardHarnessWith(t, provider.QwenProfile(), withWall, "en", true, quietConfig())

	h.answerAndHangUp("en", false, goodbyeHangup)
	h.awaitSends(t, "SendToolResultQuietly")
	// The silence the session would report; drive handles it after the
	// response's end, which it is still handling or has handled.
	h.session.emit(Event{Type: EventTypeNoInput})
	if got := h.awaitUserTexts(t, 1)[0]; got != anythingElseCue("en") {
		t.Fatalf("cue = %q, want the anything-else cue", got)
	}

	h.answered("I've passed on your message.")
	h.awaitLog(t, unbackedClaimLog)

	h.callerSays("What about my router?")
	h.answered("Routers are included.")
	settle()
	if slices.Contains(h.model.spokenLines(), guardClosingLine["en"]) {
		t.Fatal("the call closed on the first reply to the caller: the cue's reply was counted")
	}
	h.callerSays("And the installation?")
	h.answered("Installation is free.")
	waitFor(t, "the wall to close the call", func() bool {
		return slices.Contains(h.model.spokenLines(), guardClosingLine["en"])
	})
}

// The guard's record of the cue it owes, step by step.
func TestTheAnythingElseCueIsOwedToOneDeadAirAndDroppedByWhatComesBetween(t *testing.T) {
	statement := guardAnswers["en"].statement
	free := &callActions{}
	owing := func() *answerGuard {
		var g answerGuard
		g.onRefusalAnsweredQuietly(3, "welcome", statement, false, discard())
		return &g
	}

	t.Run("owed once", func(t *testing.T) {
		g := owing()
		if !g.takeAnythingElse("welcome", free) {
			t.Fatal("the first dead air after a statement is not owed the cue")
		}
		if g.takeAnythingElse("welcome", free) {
			t.Error("the cue is owed to a second dead air")
		}
	})
	t.Run("not after a question, nor after a cut answer", func(t *testing.T) {
		var g answerGuard
		g.onRefusalAnsweredQuietly(3, "welcome", guardAnswers["en"].question, false, discard())
		if g.takeAnythingElse("welcome", free) {
			t.Error("owed after a question")
		}
		g.onRefusalAnsweredQuietly(3, "welcome", statement, true, discard())
		if g.takeAnythingElse("welcome", free) {
			t.Error("owed after an answer the caller cut")
		}
	})
	for name, between := range map[string]func(g *answerGuard){
		"caller words":             func(g *answerGuard) { g.onCallerWords("What about my router?") },
		"a keypress or a barge-in": func(g *answerGuard) { g.onCallerActed() },
		"another turn":             func(g *answerGuard) { g.onTurnDone(4) },
	} {
		t.Run("dropped by "+name, func(t *testing.T) {
			g := owing()
			between(g)
			if g.takeAnythingElse("welcome", free) {
				t.Errorf("still owed after %s", name)
			}
		})
	}
	for name, between := range map[string]func(g *answerGuard){
		"an empty transcript":   func(g *answerGuard) { g.onCallerWords("  ") },
		"the answer's own turn": func(g *answerGuard) { g.onTurnDone(3) },
	} {
		t.Run("kept over "+name, func(t *testing.T) {
			g := owing()
			between(g)
			if !g.takeAnythingElse("welcome", free) {
				t.Errorf("dropped by %s", name)
			}
		})
	}
	t.Run("not after a move", func(t *testing.T) {
		if owing().takeAnythingElse("billing", free) {
			t.Error("owed in a phase the answer was not given in")
		}
	})
	t.Run("not over an ending under way", func(t *testing.T) {
		armed := &callActions{armed: func() {}}
		if owing().takeAnythingElse("welcome", armed) {
			t.Error("owed over an armed ending")
		}
		decided := &callActions{}
		decided.lockEnding()
		if owing().takeAnythingElse("welcome", decided) {
			t.Error("owed over an ending the time limit decided")
		}
	})
	t.Run("a nil guard owes nothing", func(t *testing.T) {
		var g *answerGuard
		if g.takeAnythingElse("welcome", free) {
			t.Error("a nil guard owes the cue")
		}
	})
}

// The cue itself, in both languages: it asks only whether anything else is
// needed and says not to repeat the answer.
func TestTheAnythingElseCue(t *testing.T) {
	for lang, want := range map[string][]string{
		flow.LangEN: {"has heard your answer", "Do not repeat any of it", "whether they need anything else"},
		flow.LangZH: {"已经听到了你的回答", "不要重复", "还有没有其他需要"},
	} {
		cue := anythingElseCue(lang)
		for _, fragment := range want {
			if !strings.Contains(cue, fragment) {
				t.Errorf("%s cue = %q, lacks %q", lang, cue, fragment)
			}
		}
		if isSilentCallerCue(cue) || strings.Contains(cue, "repeat the current question") {
			t.Errorf("%s cue = %q reads as the silent-caller cue", lang, cue)
		}
	}
}
