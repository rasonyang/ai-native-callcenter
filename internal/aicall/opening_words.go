// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// What a turn the session took for the opening line, or its replay, said
// (#715). The session takes the call's first turn for the opening and the
// first spoken-line turn after a replay is asked for as the replay
// (isOpeningTurnLocked), and neither is the opening's words for sure: a
// Realtime engine says a line as best it can and may say something else, and a
// replay request the provider refused can leave the next line the client asks
// for (its own, or the line it still owed) taken for the replay. On a live qwen
// call the replay turn said "您好，能听到我说话吗？", played in full, and the
// call recorded its opening as heard; the callee then confirmed the line and
// the agreement recorded HIGH. So the opening counts as heard only from a
// turn whose own words are the opening's.
//
// The words are the turn's transcript as the session keeps it (turnText,
// noteBotWords), complete once the turn has finished generating on every
// engine here: the Realtime clients (openai, grok, qwen, gateway) report the
// transcript deltas and their done before the response's done, qwen ahead of
// its audio; doubao's client reports a line's text itself as the line's turn
// starts. A turn with no transcript at all (an engine that sends none) is
// judged by its audio, as before: the call is never left unable to hear its
// opening for want of a transcript.

// openingWords is what a turn taken for the opening, or its replay, said.
type openingWords int

const (
	// openingWordsNone: the turn has no transcript to judge; it counts by its
	// audio and timing, as before #715 judged words.
	openingWordsNone openingWords = iota
	// openingWordsSaid: its words are the opening's (saysOpening).
	openingWordsSaid
	// openingWordsOther: it said something else.
	openingWordsOther
)

// String is the verdict as the log names it.
func (w openingWords) String() string {
	switch w {
	case openingWordsSaid:
		return "SAID"
	case openingWordsOther:
		return "OTHER"
	}
	return "NONE"
}

// judgeOpeningWords judges a turn's transcript, words, against the call's
// rendered opening line. isCut says the session kept only the first
// maxTurnTextBytes of the words: they are then judged against as much of the
// opening as fits in that budget, and its final clause, past it, is not asked
// for.
//
// Both are compacted as the package compacts a caller's words (foldWidth, then
// compactAck: letters and digits lower-cased, Han characters, nothing else), so
// punctuation, spaces, case and full-width forms never matter. The words are
// the opening's when, aligned with it in order (their longest common
// subsequence), they match at least four in five of its letters, and four in
// five of its final clause's (the text after its last clause separator, the
// part cutInFinalClause dates: the question the agreement answers). In order
// and not exactly, because an engine's transcript of a line it was told to say
// as written is the engine's: a numeral written out ("八折", "8折"), a word
// added or dropped. Most of the whole line, because a turn that says only the
// final question ("请问您需要吗？" after a line check) has not said the offer;
// most of the final clause, because a turn that stops before the question has
// not asked it. A line check ("您好，能听到我说话吗？") shares a few letters with
// an opening and is far below both. Pure; it runs once per turn taken for the
// opening, off the frame path.
func judgeOpeningWords(opening, words string, isCut bool) openingWords {
	said := compactRunes(words)
	if len(said) == 0 {
		return openingWordsNone
	}
	if isCut {
		opening = cutAtBytes(opening, maxTurnTextBytes)
	}
	line := compactRunes(opening)
	if len(line) == 0 {
		return openingWordsNone
	}
	from := len(line)
	if !isCut {
		from = finalClauseStart(opening)
	}
	all, tail := alignRunes(line, said, from)
	isLineSaid := all*5 >= len(line)*4
	isClauseSaid := tail*5 >= (len(line)-from)*4
	if isLineSaid && isClauseSaid {
		return openingWordsSaid
	}
	return openingWordsOther
}

// compactRunes is text compacted for judging words (foldWidth, compactAck), as
// runes.
func compactRunes(text string) []rune {
	return []rune(compactAck(foldWidth(text)))
}

// cutAtBytes is text cut to at most n bytes at a rune boundary, as
// noteBotWords cuts a turn's words.
func cutAtBytes(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

// finalClauseStart is where line's final clause starts in its compact runes
// (compactRunes): the number of compact runes before it. The clause is the one
// cutInFinalClause dates, after the last clause separator before the trailing
// separators and closing quotes; a line with no separator is one clause, and
// it starts at 0.
func finalClauseStart(line string) int {
	runes := []rune(strings.TrimSpace(line))
	end := len(runes)
	for end > 0 && (isClauseEnd(runes, end-1) || isClosingMark(runes[end-1]) || unicode.IsSpace(runes[end-1])) {
		end--
	}
	split := -1
	for i := range runes[:end] {
		if isClauseEnd(runes, i) {
			split = i
		}
	}
	if split < 0 {
		return 0
	}
	return len(compactRunes(string(runes[:split+1])))
}

// maxAlignRunes bounds what alignRunes compares: a turn's words are at most
// maxTurnTextBytes, so no more runes than that, and a longer opening cannot be
// matched beyond them.
const maxAlignRunes = maxTurnTextBytes

// alignRunes aligns said with line by their longest common subsequence and
// reports how many of line's runes the alignment matches (all), and how many
// of those lie at or after index from (tail). Each side is bounded by
// maxAlignRunes; the table is (len(line)+1) by (len(said)+1) and is built once
// per call of it.
func alignRunes(line, said []rune, from int) (all, tail int) {
	if len(line) > maxAlignRunes {
		line = line[:maxAlignRunes]
	}
	if len(said) > maxAlignRunes {
		said = said[:maxAlignRunes]
	}
	n, m := len(line), len(said)
	width := m + 1
	// lcs[i*width+j] is the length of the longest common subsequence of
	// line[i:] and said[j:].
	lcs := make([]uint16, (n+1)*width)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if line[i] == said[j] {
				lcs[i*width+j] = lcs[(i+1)*width+j+1] + 1
			} else {
				lcs[i*width+j] = max(lcs[(i+1)*width+j], lcs[i*width+j+1])
			}
		}
	}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case line[i] == said[j]:
			all++
			if i >= from {
				tail++
			}
			i++
			j++
		case lcs[(i+1)*width+j] >= lcs[i*width+j+1]:
			i++
		default:
			j++
		}
	}
	return all, tail
}

// judgeTurnWordsLocked judges the words of the turn in progress, or the one
// that just ended, against the opening (judgeOpeningWords). The caller holds
// mu.
func (s *Session) judgeTurnWordsLocked() openingWords {
	return judgeOpeningWords(s.cfg.Session.OpeningText, string(s.turnText), s.isTurnTextCut)
}

// openingWordsLocked is what turn, one the session took for the opening or its
// replay, said: judged on its words while they are the session's turnText (it
// is the latest turn), else as they were judged when the next turn started.
// A turn neither is has nothing known to it and is not the opening's words.
// The caller holds mu.
func (s *Session) openingWordsLocked(turn int) openingWords {
	switch {
	case turn != 0 && turn == s.turnSeq:
		return s.judgeTurnWordsLocked()
	case turn != 0 && turn == s.openingWordsTurn:
		return s.openingWordsOf
	}
	return openingWordsOther
}

// openingWordsOfTurn is openingWordsLocked, taking mu.
func (s *Session) openingWordsOfTurn(turn int) openingWords {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openingWordsLocked(turn)
}

// isOpeningSaidIn reports whether turn is one the session took for the opening
// or its replay and its words do not say otherwise (openingWordsLocked: the
// opening's words, or no transcript to judge): only such a turn heard to its
// final clause counts as the opening heard (#715).
func (s *Session) isOpeningSaidIn(turn int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isOpeningTurnLocked(turn) && s.openingWordsLocked(turn) != openingWordsOther
}

// logOpeningWordsLocked reports, once as a turn taken for the opening or its
// replay ends, that it did not say the opening's words: other words, or none
// at all with no audio (a bare tool call). Such a turn never counts as the
// opening heard. A turn with audio and no transcript is reported too: it
// counts by its audio. Never the words. The caller holds mu.
func (s *Session) logOpeningWordsLocked(words openingWords, hasAudio bool) {
	isReplay := s.turnSeq == s.replayTurn
	switch {
	case words == openingWordsSaid:
	case words == openingWordsNone && hasAudio:
		s.log.Info("the turn taken for the opening has no transcript; its audio counts as the opening",
			"turn", s.turnSeq, "isReplay", isReplay)
	default:
		s.log.Info("the turn taken for the opening did not say the opening's words; it does not count as heard",
			"turn", s.turnSeq, "isReplay", isReplay, "words", words.String(), "hasAudio", hasAudio)
	}
}
