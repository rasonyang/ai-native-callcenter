// SPDX-License-Identifier: Apache-2.0

package leadjudge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

func TestTheModelsAnswerIsParsedStrictly(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("很", MaxReasonRunes+50)
	for _, c := range []struct {
		raw     string
		verdict campaign.LeadVerdict
		reason  string
		isValid bool
	}{
		{`{"verdict":"CONFIRMED","reason":"客户说：可以，帮我办一个。"}`, campaign.VerdictConfirmed, "客户说：可以，帮我办一个。", true},
		{" ```json\n{\"verdict\": \" refused \", \"reason\": \" Declined. \"}\n``` ", campaign.VerdictRefused, "Declined.", true},
		{`{"verdict":"MACHINE"}`, campaign.VerdictMachine, "", true},
		{`{"verdict":"NO_EVIDENCE","reason":"` + long + `"}`, campaign.VerdictNoEvidence, long[:len(long)-50*len("很")], true},
		{`{"verdict":"UNDECIDED","reason":"Asked the price only.","extra":1}`, campaign.VerdictUndecided, "Asked the price only.", true},
		{``, "", "", false},
		{`not json`, "", "", false},
		{`["CONFIRMED"]`, "", "", false},
		{`{"reason":"no verdict"}`, "", "", false},
		{`{"verdict":""}`, "", "", false},
		{`{"verdict":"YES"}`, "", "", false},
		{`{"verdict":"NO EVIDENCE"}`, "", "", false},
		{`{"verdict":"NOT_JUDGED"}`, "", "", false},
		{`{"verdict":"PENDING"}`, "", "", false},
		{`{"verdict":7}`, "", "", false},
	} {
		v, reason, err := parseAnswer(c.raw)
		if (err == nil) != c.isValid {
			t.Errorf("%q: error %v, want valid=%v", c.raw, err, c.isValid)
			continue
		}
		if c.isValid && (v != c.verdict || reason != c.reason) {
			t.Errorf("%q: %s %q, want %s %q", c.raw, v, reason, c.verdict, c.reason)
		}
	}
}

// The prompt fits the deployment's budget the way wrapupdraft's does: the
// oldest lines go first, the omission is marked, and the lead and the newest
// lines stay.
func TestALongCallIsShortenedFromItsOldestLines(t *testing.T) {
	t.Parallel()
	var lines []store.TranscriptLine
	for i := range 400 {
		lines = append(lines, bot(fmt.Sprintf("Line %d of the bot, telling the callee about the plan in detail.", i)),
			callee(fmt.Sprintf("Answer %d of the callee.", i)))
	}
	lines = append(lines, callee("OK, sign me up."))
	in := inputOf(lines)
	budget := 8000
	system, user, ok := render(in, campaign.LeadHigh, "notes", "en", budget)
	if !ok {
		t.Fatal("the call did not render")
	}
	if got := runes(system) + runes(user); got > budget {
		t.Errorf("the prompt totals %d runes, over the budget of %d", got, budget)
	}
	for _, want := range []string{"[earlier lines omitted]", "[Callee] OK, sign me up.", "- intent: HIGH", "- notes: notes"} {
		if !strings.Contains(user, want) {
			t.Errorf("the shortened prompt lacks %q", want)
		}
	}
	if strings.Contains(user, "Line 0 of the bot") {
		t.Error("the oldest line was kept")
	}
	if _, _, ok := render(in, campaign.LeadHigh, "", "en", runes(systemPromptEN)); ok {
		t.Error("a budget the instructions alone fill rendered a prompt")
	}
	if _, user, _ := render(in[:2], campaign.LeadMedium, " ", "zh", DefaultBudgetChars); !strings.Contains(user, "- 备注：（无）") {
		t.Errorf("empty notes are not marked as none:\n%s", user)
	}
}

// Only the callee's and the bot's words are read; a human agent's line (a
// judged call has none) and every non-text row are left out.
func TestOnlyTextRowsOfTheCalleeAndTheBotAreRead(t *testing.T) {
	t.Parallel()
	in := inputOf([]store.TranscriptLine{
		bot(" Hello. "),
		callee("Hi"),
		text(store.SpeakerHumanAgent, "agent words"),
		{Speaker: store.SpeakerCustomer, Kind: store.TranscriptKindToolResult, Content: map[string]any{"text": "result text"}},
		{Speaker: store.SpeakerCustomer, Kind: store.TranscriptKindText, Content: map[string]any{"text": 7}},
	})
	if len(in) != 2 || in[0] != (line{text: "Hello."}) || in[1] != (line{isCallee: true, text: "Hi"}) {
		t.Errorf("read %+v", in)
	}
}

func TestOnlyGreetingsAndAcknowledgementSoundsAreDecidedInCode(t *testing.T) {
	sounds := []string{"喂", "嗯。", "嗯嗯", "喂，你好？", "哦……啊", "Mm.", "uh-huh", "Hello? Hi.", "Mm-hmm"}
	for _, s := range sounds {
		if !isSounds(s) {
			t.Errorf("isSounds(%q) = false, want true", s)
		}
	}
	// Answer words and anything else stay the model's to read.
	words := []string{"好", "行", "可以", "嗯，好的", "不需要", "yes", "ok", "sure", "hello, who is this", "여보세요", "喂，哪位"}
	for _, s := range words {
		if isSounds(s) {
			t.Errorf("isSounds(%q) = true, want false", s)
		}
	}
	in := []line{{text: "您好，需要升级吗？"}, {isCallee: true, text: "嗯。"}, {isCallee: true, text: "喂？"}}
	if !saysOnlySounds(in) {
		t.Error("a callee who only says 嗯 and 喂 must be decided in code")
	}
	in = append(in, line{isCallee: true, text: "好"})
	if saysOnlySounds(in) {
		t.Error("a callee who says 好 must reach the model")
	}
}
