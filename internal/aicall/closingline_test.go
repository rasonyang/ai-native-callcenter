// SPDX-License-Identifier: Apache-2.0

package aicall

import "testing"

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
