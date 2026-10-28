// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"unicode"
)

// closingLineCoverage is the share of the closing line's tokens a turn must
// carry, in order, for the turn to count as having said the line.
//
// 0.6 sits in the gap the measured utterances leave. A turn that says the line
// with a preface, a word of drift or one clause dropped scores 0.8 and up; the
// confirmations a model put in its place after a batch in live calls ("好的，那就不
// 打扰您了", "好的，会有同事稍后联系您", "Alright, drive safely, goodbye") share only
// a few function words and a goodbye with it and score 0.3 at most. Tight enough
// that a paraphrase is asked for again, loose enough that a recogniser's own
// spelling of the words or a "Sure." in front does not cost a second goodbye.
const closingLineCoverage = 0.6

// saysTheClosingLine reports how much of line the words of a turn carry and
// whether that is enough to count as saying it (closingLineCoverage). A line
// with nothing to compare (only punctuation or emoji) is taken as said.
//
// The comparison is on tokens, in order: every Han character is a token of its
// own (Chinese has no spaces, and a transcript may segment it however it
// likes), a run of other letters or digits is one. Case, punctuation, emoji and
// full-width forms do not matter. The coverage is the longest common
// subsequence of the two token lists over the line's length, so extra words
// around the line cost nothing and a missing one costs its share.
func saysTheClosingLine(line, said string) (coverage float64, isSaid bool) {
	want := closingTokens(line)
	if len(want) == 0 {
		return 1, true
	}
	got := closingTokens(said)
	coverage = float64(commonSubsequence(want, got)) / float64(len(want))
	return coverage, coverage >= closingLineCoverage
}

// closingTokens splits text into the tokens saysTheClosingLine compares.
func closingTokens(text string) []string {
	var tokens []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	for _, r := range text {
		// Full-width ASCII (ＡＢＣ１２３，) folds to its half-width form.
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			tokens = append(tokens, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word.WriteRune(unicode.ToLower(r))
		case r == '\'' || r == '’':
			// "don't" is one word, not "don" and "t".
		default:
			flush()
		}
	}
	flush()
	return tokens
}

// commonSubsequence is the length of the longest common subsequence of a and b.
func commonSubsequence(a, b []string) int {
	row := make([]int, len(b)+1)
	for _, x := range a {
		diagonal := 0
		for j, y := range b {
			above := row[j+1]
			if x == y {
				row[j+1] = diagonal + 1
			} else if row[j] > row[j+1] {
				row[j+1] = row[j]
			}
			diagonal = above
		}
	}
	return row[len(b)]
}
