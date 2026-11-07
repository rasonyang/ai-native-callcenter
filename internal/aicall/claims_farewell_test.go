// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Closings a model says instead of calling hangup (#453). The first block is
// the lines seen on live calls; the negatives each pin one guard.
func TestMoreClosingsAreFarewellClaims(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		// Shape B, as observed: the condition is a clause of its own.
		"好的，那如果您没有其他问题，我就先不打扰您了。",
		"我就先不打扰您了。",
		"好的，先不打扰了。",
		"那我先不打扰你了。",
		"好的，不再打扰您了。",
		"好的，那就先这样。",
		"那就先这样吧。",
		"好的，那就先这样啦！",
		"好的，我这就挂断。",
		"那我先挂了。",
		"Alright, I'll let you go.",
		"If there's nothing else, I'll let you go now.",
		"Okay, that's all from me.",
		"That is all from my side.",
		"I won't take any more of your time.",
		"I will not take up any more of your time.",
		// Seen in the #453 live round: a closing with no goodbye word in it.
		"好的，那您先忙，有需要随时来电。",
		"好的，您先忙。",
		"欢迎您随时来电。",
		"Okay, feel free to call back anytime.",
		// Shape C, as observed.
		"好的，那就不打扰您了，祝您生活愉快。",
		"好的，那我就不打扰您了，再见。",
		"好的，那我这就挂断，再见。",
		"Okay, I won't take up any more of your time. Goodbye.",
	} {
		if !hasFarewell(detectClaims(text)) {
			t.Errorf("%q: not a FAREWELL, want one", text)
		}
	}
}

// Each guard has its own case: phrases that carry on, or only offer, claim
// nothing.
func TestClosingsThatCarryOnAreNoClaim(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"zh conditional continues":      "不打扰您的话，我再确认一下您的手机号。",
		"zh question":                   "那我就先不打扰您了，可以吗？",
		"zh question word":              "我先不打扰您了吗",
		"zh conditional same clause":    "如果没问题我们就先这样。",
		"zh just-so is not a closing":   "就先这样处理，我再确认一下。",
		"zh before":                     "在挂断之前，我再确认一下。",
		"en before I let you go":        "Before I let you go, let me confirm your order number.",
		"en before, question":           "Before I let you go, is there anything else?",
		"en let you go after":           "I'll let you go after I confirm your number.",
		"en conditional same clause":    "I'll let you go if that is all.",
		"en question":                   "Is that all from you?",
		"en all from me is not closing": "That's all you need from me to file it, right?",
		"zh busy, question":             "您先忙吗",
		"en call anytime, question":     "Feel free to call back anytime, okay?",
	} {
		if hasFarewell(detectClaims(text)) {
			t.Errorf("%s: %q claimed a FAREWELL, want none", name, text)
		}
	}
}

// The guard refinement is the FAREWELL's alone: a conditional promise still
// claims nothing, in every other kind.
func TestTheClauseGuardLeavesTheOtherClaimsUnchanged(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"如果您需要，我帮您转接人工。",
		"If you'd like, I'll transfer you.",
		"如果您愿意，我帮您查询一下。",
		"If you want, let me check that order for you.",
		"Whether I've saved your message, I can't say.",
	} {
		if got := detectClaims(text); len(got) != 0 {
			t.Errorf("%q claimed %+v, want none", text, got)
		}
	}
	got := detectClaims("好的，我帮您转接人工客服。")
	if len(got) != 1 || got[0].kind != store.UnbackedClaimTransfer {
		t.Errorf("a plain transfer claim = %+v", got)
	}
}

// A later clause that carries on makes the farewell clause no goodbye; a later
// clause that only adds a courtesy does not.
func TestALaterClauseThatCarriesOnIsNoFarewell(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"那就不打扰您了，不过最后确认一下您的地址。",
		"I'll let you go, but first let me confirm your address.",
		"I'll let you go, before that let me confirm your address.",
	} {
		if hasFarewell(detectClaims(text)) {
			t.Errorf("%q claimed a FAREWELL, want none", text)
		}
	}
	for _, text := range []string{
		"那我就先不打扰您了，如果还有问题可以随时联系我们。",
		"好的，那就不打扰您了，之后有问题随时联系，再见。",
		"我就不打扰您了，不过如果您之后有问题可以随时来电，再见。",
	} {
		if !hasFarewell(detectClaims(text)) {
			t.Errorf("%q: not a FAREWELL, want one", text)
		}
	}
}

// A turn that ends in a question is no goodbye, however its first sentence
// reads; a goodbye after an earlier question stays one. Not reported either.
func TestATurnThatEndsInAQuestionIsNoUnhungFarewell(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		"感谢您的配合。请问还有其他问题吗？":                                                    false,
		"Thanks for your cooperation. Is there anything else I can help with?": false,
		"还有问题吗？好的，再见。":                                                         true,
		"Is there anything else? Okay, goodbye.":                               true,
	} {
		var w claimWatch
		w.onBotSaid(1, text)
		found := w.onTurnDone(1, false, false)
		if got := w.hasFarewellWithoutHangup(); got != want {
			t.Errorf("%q: unhung = %v, want %v", text, got, want)
		}
		if got := hasFarewell(found); got != want {
			t.Errorf("%q: FAREWELL reported = %v, want %v", text, got, want)
		}
		// The same words arriving after the turn finished.
		var late claimWatch
		late.onTurnDone(1, false, false)
		late.onBotSaid(1, text)
		if got := late.hasFarewellWithoutHangup(); got != want {
			t.Errorf("late %q: unhung = %v, want %v", text, got, want)
		}
	}
}
