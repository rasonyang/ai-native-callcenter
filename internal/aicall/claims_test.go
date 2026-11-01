// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// The positives are the bot's own lines from the calls in #44; the negatives
// are the offers and conditions said around them, which claim nothing.
func TestDetectClaimsReadsWhatTheBotSaidItDid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want string // "" is no claim
	}{
		{"I'm transferring you to a support agent now who can assist with your internet outage.", store.UnbackedClaimTransfer},
		{"I've already transferred you to a support agent who can answer your questions about our services.", store.UnbackedClaimTransfer},
		{"I've connected you to a support agent who can provide details about our cable TV services.", store.UnbackedClaimTransfer},
		{"I've initiated the transfer to a support agent who can help with your internet outage.", store.UnbackedClaimTransfer},
		{"I'm here. I've started the transfer to a support agent for your internet issue.", store.UnbackedClaimTransfer},
		{"The transfer to the support agent is being processed now.", store.UnbackedClaimTransfer},
		{"Great, I’m connecting you to a support agent now to help with your internet outage.", store.UnbackedClaimTransfer},
		{"好的，我帮您转接人工客服。", store.UnbackedClaimTransfer},
		{"正在为您转接，请稍候。", store.UnbackedClaimTransfer},
		{"好的，维修单号是 RMA1003，我帮您查询一下。", store.UnbackedClaimLookup},
		{"Let me check that order for you.", store.UnbackedClaimLookup},
		{"I've passed on your message to the team.", store.UnbackedClaimMessage},
		{"已为您记录留言。", store.UnbackedClaimMessage},
		{"Goodbye.", store.UnbackedClaimFarewell},
		{"祝您生活愉快，再见！", store.UnbackedClaimFarewell},
		{"Thanks for your time, and have a great day!", store.UnbackedClaimFarewell},
		{"Thank you for your time. If you need anything, you can reach out anytime.", store.UnbackedClaimFarewell},
		{"Thank you for your cooperation.", store.UnbackedClaimFarewell},
		{"Have a good day.", store.UnbackedClaimFarewell},
		{"Alright, take care.", store.UnbackedClaimFarewell},
		{"好的，那就等您本周五处理。感谢您的配合，祝您生活愉快！", store.UnbackedClaimFarewell},
		{"感谢配合。", store.UnbackedClaimFarewell},
		{"祝您周末愉快", store.UnbackedClaimFarewell},

		{"I can connect you to a support agent who can provide detailed information about our TV services", ""},
		{"For account-specific troubleshooting, I'll need to connect you to a support agent.", ""},
		{"Would you like me to transfer you?", ""},
		{"If you'd like, I'll transfer you.", ""},
		{"需要我帮您转接人工吗？", ""},
		{"我可以帮您转接人工客服。", ""},
		{"如果您需要，我帮您转接人工。", ""},
		{"Are you still there? I", ""},
		{"I'm not able to transfer you right now.", ""},
		{"What is your repair order number?", ""},
		{"Thank you, let me note that down.", ""},
		{"Thanks for calling NovaNet, how can I help you today?", ""},
		{"I will take care of that now.", ""},
		{"好的，谢谢您，请问您的预算大概是多少？", ""},
		{"感谢致电 NovaNet，请问有什么可以帮您？", ""},
	}
	for _, tc := range cases {
		claims := detectClaims(tc.text)
		switch {
		case tc.want == "" && len(claims) > 0:
			t.Errorf("%q: claimed %s (%q), want nothing", tc.text, claims[0].kind, claims[0].phrase)
		case tc.want != "" && (len(claims) != 1 || claims[0].kind != tc.want):
			t.Errorf("%q: claims = %+v, want one %s", tc.text, claims, tc.want)
		}
	}
}

// One line naming the same action twice is one claim of it; two actions are
// two.
func TestDetectClaimsCountsEachKindOnce(t *testing.T) {
	t.Parallel()
	claims := detectClaims("I'm transferring you now. I've transferred you. Goodbye.")
	if len(claims) != 2 || claims[0].kind != store.UnbackedClaimTransfer ||
		claims[1].kind != store.UnbackedClaimFarewell {
		t.Errorf("claims = %+v, want TRANSFER then FAREWELL", claims)
	}
}

func TestClaimWatchReportsOnlyWhatNoToolBacks(t *testing.T) {
	t.Parallel()
	const line = "I'm transferring you to a support agent now."

	t.Run("a claim with no tool behind it", func(t *testing.T) {
		var w claimWatch
		w.onBotSaid(3, line)
		if claims := w.onTurnDone(3, false, false); len(claims) != 1 ||
			claims[0].kind != store.UnbackedClaimTransfer {
			t.Errorf("claims = %+v, want the transfer", claims)
		}
	})

	t.Run("in the turn that made the tool call", func(t *testing.T) {
		var w claimWatch
		// The words land before the call does, and are judged with it.
		w.onBotSaid(3, line)
		if claims := w.onTurnDone(3, true, false); len(claims) != 0 {
			t.Errorf("claims = %+v in the tool's own turn", claims)
		}
	})

	t.Run("in the reply to the tool's result", func(t *testing.T) {
		var w claimWatch
		w.onTurnDone(3, true, false)
		w.onBotSaid(4, line)
		if claims := w.onTurnDone(4, false, false); len(claims) != 0 {
			t.Errorf("claims = %+v in the reply to the tool", claims)
		}
		// The reply after that is on its own again.
		w.onBotSaid(5, line)
		if claims := w.onTurnDone(5, false, false); len(claims) != 1 {
			t.Errorf("claims = %+v two turns after the tool, want the transfer", claims)
		}
	})

	t.Run("while an ending is armed", func(t *testing.T) {
		var w claimWatch
		w.onBotSaid(6, "Goodbye.")
		if claims := w.onTurnDone(6, false, true); len(claims) != 0 {
			t.Errorf("claims = %+v for a goodbye the platform armed", claims)
		}
	})

	t.Run("words that arrive after their turn", func(t *testing.T) {
		var w claimWatch
		w.onTurnDone(7, false, false)
		if claims := w.onBotSaid(7, line); len(claims) != 1 {
			t.Errorf("claims = %+v for a late line of an unbacked turn", claims)
		}
		w.onTurnDone(8, true, false)
		if claims := w.onBotSaid(8, line); len(claims) != 0 {
			t.Errorf("claims = %+v for a late line of the tool's turn", claims)
		}
		if claims := w.onBotSaid(7, line); len(claims) != 0 {
			t.Errorf("claims = %+v for a line of a turn long past", claims)
		}
	})
}

// The state a re-ask is made from: which turn is owed one, and what takes it back.
func TestClaimWatchStandsForTheLatestUnbackedActionableClaim(t *testing.T) {
	t.Parallel()
	const lookup = "好的，我帮您查询一下。"

	t.Run("owed at the claim turn's playback, once", func(t *testing.T) {
		var w claimWatch
		w.onBotSaid(3, lookup)
		w.onTurnDone(3, false, false)
		if w.standingClaim() == nil || w.standingClaim().kind != store.UnbackedClaimLookup {
			t.Fatalf("standing = %v, want the lookup", w.standingClaim())
		}
		if c := w.onPlaybackDone(2); c != nil {
			t.Errorf("re-ask owed at another turn's playback: %v", c)
		}
		if c := w.onPlaybackDone(3); c == nil {
			t.Error("no re-ask at the claim turn's playback")
		}
		if c := w.onPlaybackDone(3); c != nil {
			t.Error("the re-ask was owed twice")
		}
		if w.standingClaim() == nil {
			t.Error("a re-ask took the claim from the dead-air path before a cue was sent")
		}
		w.cueSent(store.UnbackedClaimLookup, claimCueOnPlayback)
		if w.standingClaim() != nil {
			t.Error("a claim cue was sent and the claim still stands")
		}
	})
	t.Run("the answer to a cue is not owed one but still stands", func(t *testing.T) {
		var w claimWatch
		w.cueSent(store.UnbackedClaimLookup, claimCueOnPlayback)
		w.onBotSaid(4, lookup)
		w.onTurnDone(4, false, false)
		if c := w.onPlaybackDone(4); c != nil {
			t.Errorf("the answer to a cue was re-asked: %v", c)
		}
		if w.standingClaim() == nil {
			t.Error("a second promise does not stand for the dead-air cue")
		}
		out, ok := w.takeCueOutcome()
		if !ok || out.isBacked {
			t.Errorf("outcome = %+v, %v; want a cue that was not backed", out, ok)
		}
		if _, again := w.takeCueOutcome(); again {
			t.Error("the outcome was reported twice")
		}
	})
	t.Run("a tool call, a later turn and an interruption take it back", func(t *testing.T) {
		var w claimWatch
		w.onBotSaid(1, lookup)
		w.onTurnDone(1, false, false)
		w.onToolCall()
		if w.standingClaim() != nil {
			t.Error("a tool call left the claim standing")
		}
		w.onBotSaid(2, lookup)
		w.onTurnDone(2, false, false)
		w.onTurnDone(3, false, false)
		if w.standingClaim() != nil {
			t.Error("a later turn left the claim standing")
		}
		w.onBotSaid(4, lookup)
		w.onTurnDone(4, false, false)
		w.onTurnInterrupted()
		if w.standingClaim() != nil || w.onPlaybackDone(4) != nil {
			t.Error("an interrupted turn left the claim standing")
		}
		w.onBotSaid(5, "Goodbye.")
		w.onTurnDone(5, false, false)
		if w.standingClaim() != nil {
			t.Error("a farewell stands for a claim cue; it has its own path")
		}
	})
	t.Run("the first actionable claim in detection order", func(t *testing.T) {
		var w claimWatch
		w.onBotSaid(1, "再见。好的，我马上为您转接人工客服，我帮您查询一下。")
		w.onTurnDone(1, false, false)
		if c := w.standingClaim(); c == nil || c.kind != store.UnbackedClaimTransfer {
			t.Errorf("standing = %v, want the transfer", c)
		}
	})
}
