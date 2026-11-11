// SPDX-License-Identifier: Apache-2.0

package aicall

import "testing"

func TestTheClassifierSeparatesAcknowledgementsFromWords(t *testing.T) {
	tests := []struct {
		language string
		text     string
		want     backchannelVerdict
	}{
		// zh: the list, with punctuation and repeats.
		{"zh", "嗯", verdictAck},
		{"zh", "嗯。", verdictAck},
		{"zh", "嗯嗯", verdictAck},
		{"zh", "嗯嗯嗯", verdictAck},
		{"zh", "嗯哼", verdictAck},
		{"zh", "对", verdictAck},
		{"zh", "对对", verdictAck},
		{"zh", "对对对", verdictAck},
		{"zh", "好", verdictAck},
		{"zh", "好的", verdictAck},
		{"zh", "好的好的", verdictAck},
		{"zh", "好的，好的。", verdictAck},
		{"zh", "是", verdictAck},
		{"zh", "是的", verdictAck},
		{"zh", "哦", verdictAck},
		{"zh", "噢", verdictAck},
		{"zh", "行", verdictAck},
		{"zh", "可以", verdictAck},
		// zh: words, questions and what follows a comma.
		{"zh", "喂", verdictContent},
		{"zh", "等等", verdictContent},
		{"zh", "等一下", verdictContent},
		{"zh", "停", verdictContent},
		{"zh", "不对", verdictContent},
		{"zh", "不是", verdictContent},
		{"zh", "不要", verdictContent},
		{"zh", "什么", verdictContent},
		{"zh", "啊？", verdictContent},
		{"zh", "嗯？", verdictContent},
		{"zh", "嗯?", verdictContent},
		{"zh", "对，但是…", verdictContent},
		{"zh", "嗯…我想问一下", verdictContent},
		{"zh", "好的好的，那多少钱", verdictContent},
		{"zh", "对对对对", verdictContent},
		{"zh", "是不是", verdictContent},
		// en.
		{"en", "mm-hmm", verdictAck},
		{"en", "Mm-hmm.", verdictAck},
		{"en", "mm hmm", verdictAck},
		{"en", "mhm", verdictAck},
		{"en", "uh-huh", verdictAck},
		{"en", "uh huh", verdictAck},
		{"en", "yeah", verdictAck},
		{"en", "Yeah.", verdictAck},
		{"en", "yep", verdictAck},
		{"en", "okay", verdictAck},
		{"en", "ok", verdictAck},
		{"en", "right", verdictAck},
		{"en", "sure", verdictAck},
		{"en", "got it", verdictAck},
		{"en", "I see", verdictAck},
		{"en", "yeah, yeah", verdictAck},
		{"en", "okay okay okay", verdictAck},
		{"en", "wait", verdictContent},
		{"en", "stop", verdictContent},
		{"en", "no", verdictContent},
		{"en", "hello", verdictContent},
		{"en", "what", verdictContent},
		{"en", "sorry", verdictContent},
		{"en", "okay but", verdictContent},
		{"en", "okay, but", verdictContent},
		{"en", "yeah, no", verdictContent},
		{"en", "yeah?", verdictContent},
		{"en", "right now", verdictContent},
		{"en", "okay okay okay okay", verdictContent},
		// Another language's word is words.
		{"en", "嗯", verdictContent},
		{"en", "好的", verdictContent},
		{"zh", "mm-hmm", verdictContent},
		{"zh", "okay", verdictContent},
		{"zh-CN", "嗯", verdictAck},
		{"zh-CN", "yeah", verdictContent},
		{"en-US", "yeah", verdictAck},
		// A growing partial that may still become one.
		{"zh", "", verdictAckSoFar},
		{"zh", " ", verdictAckSoFar},
		{"zh", "好的好", verdictAckSoFar},
		{"en", "mm", verdictAck},
		{"en", "uh", verdictAck},
		{"en", "got", verdictAckSoFar},
		{"en", "ye", verdictAckSoFar},
		{"en", "I", verdictAckSoFar},
	}
	for _, tt := range tests {
		if got := classifyBackchannel(tt.language, tt.text); got != tt.want {
			t.Errorf("classifyBackchannel(%q, %q) = %s, want %s", tt.language, tt.text, got, tt.want)
		}
	}
}
