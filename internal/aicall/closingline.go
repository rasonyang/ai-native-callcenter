// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"time"
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

// finalSentenceSlack widens the final sentence on the early side when a cut is
// dated (cutInFinalSentence). The share of the audio a sentence takes is
// estimated from its share of the text, which is off by a few hundred
// milliseconds on a line spoken at a different pace in different places, and
// the pause at a sentence boundary belongs to neither sentence. A caller whose
// speech was detected 400 ms before the final sentence began has heard the line
// for every purpose that matters here. Kept under 500 ms on the owner's
// instruction: any more and a cut in the middle of the last-but-one sentence
// would count as said.
const finalSentenceSlack = 400 * time.Millisecond

// cutInFinalSentence reports whether a caller who spoke over the closing line,
// having heard played of it with unplayed still to come, cut in on its final
// sentence, and so heard all of it but a goodbye. finalSentence is the
// estimated audio length of that sentence: (played + unplayed) times the
// sentence's share of the line's letters and digits (Han characters count as
// letters). ok is true when unplayed is no more than finalSentence plus
// finalSentenceSlack.
//
// The sentence ends at the last terminator (. ! ? and their full-width forms)
// before the line's trailing terminators and closing quotes; a '.' between two
// ASCII digits ("3.5") is not one. A line with no such terminator is one
// sentence, and a cut anywhere in it is a cut before its final sentence (ok is
// false): an early cut on a one-sentence line still gets the line said. A
// negative played (not known) or a line with nothing to weigh never applies.
func cutInFinalSentence(line string, played, unplayed time.Duration) (finalSentence time.Duration, ok bool) {
	if played < 0 || unplayed < 0 {
		return 0, false
	}
	runes := []rune(strings.TrimSpace(line))
	end := len(runes)
	for end > 0 && (isSentenceEnd(runes, end-1) || isClosingMark(runes[end-1]) || unicode.IsSpace(runes[end-1])) {
		end--
	}
	runes = runes[:end]
	split := -1
	for i := range runes {
		if isSentenceEnd(runes, i) {
			split = i
		}
	}
	if split < 0 {
		return 0, false
	}
	total, last := weigh(runes), weigh(runes[split+1:])
	if total == 0 || last == 0 {
		return 0, false
	}
	finalSentence = time.Duration(float64(played+unplayed) * float64(last) / float64(total))
	return finalSentence, unplayed <= finalSentence+finalSentenceSlack
}

// isSentenceEnd reports whether runes[i] ends a sentence. A '.' with an ASCII
// digit on both sides is a decimal point.
func isSentenceEnd(runes []rune, i int) bool {
	switch runes[i] {
	case '。', '！', '？', '!', '?':
		return true
	case '.':
		isDigit := func(r rune) bool { return r >= '0' && r <= '9' }
		return !(i > 0 && i+1 < len(runes) && isDigit(runes[i-1]) && isDigit(runes[i+1]))
	}
	return false
}

// isClosingMark reports whether r is a quote or bracket that closes a sentence
// after its terminator.
func isClosingMark(r rune) bool {
	return strings.ContainsRune("\"'”’」』）)»", r)
}

// weigh counts the letters and digits in runes, the stand-in for how long they
// take to say.
func weigh(runes []rune) int {
	n := 0
	for _, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n
}
