// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"testing"
	"time"
)

func TestSaysTheClosingLine(t *testing.T) {
	const zhLine = "感谢您的时间,打扰了,祝您生活愉快,再见。"
	const enLine = "Thank you for your time, and sorry for the interruption. Have a good day, goodbye."
	tests := []struct {
		name, line, said string
		want             bool
	}{
		{"zh exact", zhLine, "感谢您的时间,打扰了,祝您生活愉快,再见。", true},
		{"zh full-width punctuation", zhLine, "感谢您的时间，打扰了，祝您生活愉快，再见！", true},
		{"zh preface", zhLine, "好的，感谢您的时间，打扰了，祝您生活愉快，再见。", true},
		{"zh drift", zhLine, "感谢您的时间，打扰您了，祝您生活愉快，再见。", true},
		{"zh dropped clause", zhLine, "感谢您的时间，祝您生活愉快，再见。", true},
		{"zh just not disturbing", zhLine, "好的，那就不打扰您了。", false},
		{"zh drive safely", zhLine, "好的，那祝您行车安全，再见。", false},
		{"zh colleague will call", zhLine, "好的，会有同事稍后联系您。", false},
		{"zh bare goodbye", zhLine, "再见。", false},
		{"en exact", enLine, "Thank you for your time, and sorry for the interruption. Have a good day, goodbye.", true},
		{"en case and punctuation", enLine, "THANK YOU for your time -- and sorry for the interruption! Have a good day; goodbye", true},
		{"en preface", enLine, "Sure. Thank you for your time, and sorry for the interruption. Have a good day, goodbye.", true},
		{"en drift", enLine, "Thank you for your time and sorry about the interruption. Have a good day, goodbye.", true},
		{"en colleague", enLine, "Okay, a colleague will be in touch with you shortly.", false},
		{"en drive safely", enLine, "Alright, drive safely, goodbye.", false},
		{"en bare goodbye", enLine, "Goodbye!", false},
		{"emoji and slot", "Thank you Ann, goodbye 👋", "Thank you Ann, goodbye", true},
		{"apostrophe", "We'll call you back, goodbye", "We’ll call you back goodbye.", true},
		{"mixed script", "谢谢您Ann，再见 👋", "谢谢您 ann 再见", true},
		{"nothing to compare", "👋", "anything", true},
		{"nothing said", zhLine, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coverage, got := saysTheClosingLine(tt.line, tt.said)
			if got != tt.want {
				t.Errorf("saysTheClosingLine(%q, %q) = %v (coverage %.2f), want %v",
					tt.line, tt.said, got, coverage, tt.want)
			}
		})
	}
}

// The final sentence's audio is estimated from its share of the line's letters
// and digits, and a cut counts when what was left to hear is no more than that
// plus finalSentenceSlack (#465).
func TestCutInFinalSentence(t *testing.T) {
	const second = time.Second
	// total is the line's length: played = total - unplayed.
	cut := func(line string, total, unplayed time.Duration) (time.Duration, bool) {
		return cutInFinalSentence(line, total-unplayed, unplayed)
	}
	// "aaaaaaa. bbb." weighs 10 letters, 3 of them in the final sentence: of a
	// 10 s line, 3 s.
	const threeInTen = "aaaaaaa. bbb."
	tests := []struct {
		name      string
		line      string
		total     time.Duration
		unplayed  time.Duration
		wantFinal time.Duration
		wantOK    bool
	}{
		{"boundary: the final sentence plus the slack counts", threeInTen,
			10 * second, 3*second + finalSentenceSlack, 3 * second, true},
		{"boundary: one millisecond more does not", threeInTen,
			10 * second, 3*second + finalSentenceSlack + time.Millisecond, 3 * second, false},
		{"the whole final sentence unplayed", threeInTen, 10 * second, 3 * second, 3 * second, true},
		{"nothing unplayed", threeInTen, 10 * second, 0, 3 * second, true},
		{"cut at 50 percent", threeInTen, 10 * second, 5 * second, 3 * second, false},
		// The call from the issue: a 15.12 s zh line, 4.04 s unplayed, cut inside
		// the closing goodbye (8 of 30 letters, 4.032 s).
		{"issue call, zh", "感谢您的来电。您的留言已经记录，同事明天会联系您。祝您生活愉快，再见！",
			15120 * time.Millisecond, 4040 * time.Millisecond, 4032 * time.Millisecond, true},
		{"zh, cut in the middle sentence", "感谢您的来电。您的留言已经记录，同事明天会联系您。祝您生活愉快，再见！",
			15120 * time.Millisecond, 8 * second, 4032 * time.Millisecond, false},
		{"en, three sentences, cut at 73 percent",
			"Thank you for calling. Your message has been taken and a colleague will call you back tomorrow. Goodbye and take care of yourself, Ann.",
			15 * second, 4050 * time.Millisecond, 4305555555, true},
		{"en, same line, cut at 50 percent",
			"Thank you for calling. Your message has been taken and a colleague will call you back tomorrow. Goodbye and take care of yourself, Ann.",
			15 * second, 7500 * time.Millisecond, 4305555555, false},
		{"closing exclamation and quote", "aaaaaaa. bbb!\"", 10 * second, 3 * second, 3 * second, true},
		{"closing full-width stop and bracket", "aaaaaaa。bbb。）", 10 * second, 3 * second, 3 * second, true},
		{"trailing ellipsis and spaces", "aaaaaaa. bbb...  ", 10 * second, 3 * second, 3 * second, true},
		// The final sentence of "a 3.5 aa. bbb." is "bbb": 3 of 8 letters and digits.
		{"a decimal point weighs as digits", "a 3.5 aa. bbb.", 10 * second, 4 * second, 3750 * time.Millisecond, true},
		{"a decimal point does not make a second sentence", "a 3.5 aa", 10 * second, 100 * time.Millisecond, 0, false},
		{"one sentence line never applies", "aaaa bbb.", 10 * second, 100 * time.Millisecond, 0, false},
		{"one sentence without a terminator", "aaaa bbb", 10 * second, 0, 0, false},
		{"nothing to weigh after the last terminator", "aaaa. ...", 10 * second, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			final, ok := cut(tc.line, tc.total, tc.unplayed)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (final sentence %v), want %v", ok, final, tc.wantOK)
			}
			if tc.wantFinal != 0 && final != tc.wantFinal {
				t.Errorf("final sentence = %v, want %v", final, tc.wantFinal)
			}
		})
	}
}

func TestACutWhoseTimeIsNotKnownNeverCountsAsInTheFinalSentence(t *testing.T) {
	if _, ok := cutInFinalSentence("One two. Three.", -1, time.Second); ok {
		t.Error("an unknown played time counted")
	}
}

// The slack stays under the 500 ms the owner set.
func TestTheFinalSentenceSlackStaysUnderHalfASecond(t *testing.T) {
	if finalSentenceSlack >= 500*time.Millisecond {
		t.Fatalf("finalSentenceSlack = %v, want under 500ms", finalSentenceSlack)
	}
}
