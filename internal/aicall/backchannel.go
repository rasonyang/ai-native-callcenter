// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"unicode"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// backchannelVerdict is what a caller's words are, as far as taking the floor
// goes (docs/design/11-backchannels.md).
type backchannelVerdict int

const (
	// verdictContent is words that are not an acknowledgement and cannot become
	// one by saying more: the caller took the floor.
	verdictContent backchannelVerdict = iota
	// verdictAckSoFar is a partial transcript that is, or is a prefix of, an
	// acknowledgement. It may still turn into content, so the hold goes on.
	verdictAckSoFar
	// verdictAck is a complete acknowledgement: "mm-hmm", "嗯", "好的好的".
	verdictAck
	// verdictGreeting is a complete pickup greeting: "喂", "喂，您好", "hello?"
	// (classifyGreeting).
	verdictGreeting
	// verdictGreetingSoFar is a partial transcript that is, or is a prefix of,
	// a greeting. It may still turn into words, so the hold goes on.
	verdictGreetingSoFar
)

// String names a verdict for logs and test failures.
func (v backchannelVerdict) String() string {
	switch v {
	case verdictAck:
		return "ack"
	case verdictAckSoFar:
		return "ackSoFar"
	case verdictGreeting:
		return "greeting"
	case verdictGreetingSoFar:
		return "greetingSoFar"
	default:
		return "content"
	}
}

// ackMaxRepeats is how many times one acknowledgement may be said in a row and
// still be one ("对对对", "嗯嗯嗯").
const ackMaxRepeats = 3

// acknowledgements is the built-in list, fixed per language and shared by every
// flow. Entries are in compact form (see compactAck). The lists are the
// product's, not the flow's: an author cannot extend them.
var acknowledgements = map[string][]string{
	"zh": {"嗯", "嗯嗯", "嗯哼", "对", "好", "好的", "是", "是的", "哦", "噢", "行", "可以"},
	// mm, hmm, uh and oh are how the transcriber writes a spoken "mm-hmm" or
	// "uh-huh" (live, 2026-11-11).
	"en": {"mmhmm", "mhm", "mmhm", "mm", "mmm", "hm", "hmm", "uhhuh", "uhuh", "uh", "oh",
		"yeah", "yep", "okay", "ok", "right", "sure", "gotit", "isee"},
}

// classifyBackchannel judges a caller's transcript against the call language's
// acknowledgements. language is the call's language as the session holds it
// (any spelling flow.Lang understands, so "zh-CN" works); only that language's
// list counts, so another language's word is content. text is a partial or a
// final transcript, and the verdict is about the text alone: a partial that is
// a whole acknowledgement is verdictAck here and the caller of this function
// decides that more may still come.
//
// A question mark anywhere makes it content ("嗯？" asks something), as does
// any segment that is not the same acknowledgement as the others, so a comma
// followed by more words ("好的好的，那多少钱") is content. Nothing at all is
// verdictAckSoFar: no information yet. It is a pure function and runs off the
// frame path.
func classifyBackchannel(language, text string) backchannelVerdict {
	if strings.ContainsAny(text, "?？") {
		return verdictContent
	}
	list := acknowledgements[flow.Lang(language)]
	if list == nil {
		return verdictContent
	}
	segments := ackSegments(text)
	if len(segments) == 0 {
		return verdictAckSoFar
	}
	// Every segment but the last must be a whole acknowledgement; the last may
	// still be growing. All must be repeats of the one phrase.
	joined := strings.Join(segments, "")
	if phrase, ok := wholeAck(list, joined); ok && segmentsRepeat(segments, phrase) {
		return verdictAck
	}
	if isAckPrefix(list, joined) && segmentsAreAckPrefixes(list, segments) {
		return verdictAckSoFar
	}
	return verdictContent
}

// ackSegments splits a transcript at its clause punctuation and compacts each
// piece: lower case, letters and Han characters only. Empty pieces go.
func ackSegments(text string) []string {
	pieces := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ',', '，', '、', '.', '。', '!', '！', ';', '；', '…', '~', '～', '\n':
			return true
		}
		return false
	})
	var out []string
	for _, piece := range pieces {
		if compact := compactAck(piece); compact != "" {
			out = append(out, compact)
		}
	}
	return out
}

// compactAck keeps the letters (lower-cased) and Han characters of a piece, so
// "Mm-hmm", "mm hmm" and "MMHMM" compare equal.
func compactAck(piece string) string {
	var b strings.Builder
	for _, r := range piece {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// wholeAck reports whether s is one acknowledgement said up to ackMaxRepeats
// times, and which. The longest phrase that fits wins, so "嗯嗯" is itself and
// not "嗯" twice.
func wholeAck(list []string, s string) (phrase string, ok bool) {
	best := ""
	for _, candidate := range list {
		if candidate == "" || len(candidate) <= len(best) || !strings.HasPrefix(s, candidate) {
			continue
		}
		if isRepeat(s, candidate) {
			best = candidate
		}
	}
	if best != "" {
		return best, true
	}
	return "", false
}

// isRepeat reports whether s is phrase repeated one to ackMaxRepeats times.
func isRepeat(s, phrase string) bool {
	for n := 1; n <= ackMaxRepeats; n++ {
		if s == strings.Repeat(phrase, n) {
			return true
		}
	}
	return false
}

// segmentsRepeat reports whether every segment is the phrase repeated, and the
// phrase is said at most ackMaxRepeats times in all.
func segmentsRepeat(segments []string, phrase string) bool {
	total := 0
	for _, segment := range segments {
		n := len(segment) / len(phrase)
		if n == 0 || segment != strings.Repeat(phrase, n) {
			return false
		}
		total += n
	}
	return total <= ackMaxRepeats
}

// isAckPrefix reports whether s is a proper prefix of some acknowledgement said
// up to ackMaxRepeats times.
func isAckPrefix(list []string, s string) bool {
	for _, candidate := range list {
		full := strings.Repeat(candidate, ackMaxRepeats)
		if len(s) < len(full) && strings.HasPrefix(full, s) {
			return true
		}
	}
	return false
}

// segmentsAreAckPrefixes keeps a growing partial from being read as an
// acknowledgement across a clause break that makes it content: every segment
// before the last must itself be a whole acknowledgement.
func segmentsAreAckPrefixes(list []string, segments []string) bool {
	for _, segment := range segments[:len(segments)-1] {
		if _, ok := wholeAck(list, segment); !ok {
			return false
		}
	}
	return true
}

// ackRunMaxPhrases is how many acknowledgements a run may be made of and still
// be one (isAckRun): "嗯，好的", "对，是的，可以". A longer run is a caller
// insisting, and is words.
const ackRunMaxPhrases = 4

// isAckRun reports whether a transcript is a run of acknowledgements, different
// ones allowed: "嗯，可以", "对，是的", "嗯好的", "yeah, okay", "oh right". The
// compact form of the whole text (compactAck: punctuation and spaces dropped)
// must be one to ackRunMaxPhrases phrases of the call language's list and
// nothing else. Any split counts (嗯嗯 is one phrase or two), and no punctuation
// is needed between phrases, because the transcriber's is unreliable. A question
// mark anywhere makes it words, as for classifyBackchannel, and so does no text.
//
// It is for the verdict on a cut only (judgeCut), where a run taken for an
// answer moves the flow. The hold keeps the shared classifier's single phrase:
// a held "嗯，可以" is released as words and answered, as words always were.
// Pure; it runs off the frame path.
func isAckRun(language, text string) bool {
	if strings.ContainsAny(text, "?？") {
		return false
	}
	compact := compactAck(text)
	return compact != "" && isPhraseRun(acknowledgements[flow.Lang(language)], compact, ackRunMaxPhrases)
}

// greetingMaxWords is how many greeting words one utterance may be made of and
// still be a greeting ("喂，您好", "喂喂喂", "hi, hello").
const greetingMaxWords = 3

// greetings is the built-in list of pickup greetings, fixed per language like
// the acknowledgements and in compact form. A callee who picks up says one of
// these before anything else; it answers nothing the bot said.
var greetings = map[string][]string{
	"zh": {"喂", "你好", "您好"},
	"en": {"hello", "hi"},
}

// classifyGreeting judges a caller's transcript against the call language's
// greetings: an utterance made only of greeting words, at most greetingMaxWords
// of them, in any order and with any repetition, on the joined compact form the
// acknowledgement classifier uses (so "喂，您好", "喂喂喂", "你好你好" and "hi,
// hello" all count). Anything else in it makes it words, and so does another
// language's greeting. Unlike an acknowledgement, a question mark does not:
// "喂？" and "hello?" are greetings, which is how they are written when said
// into a ringing phone.
//
// A proper prefix of a greeting, or of a run of them, is verdictGreetingSoFar
// (a partial transcript still growing), and so is no text at all. The result is
// verdictGreeting, verdictGreetingSoFar or verdictContent. Pure; it runs off
// the frame path.
func classifyGreeting(language, text string) backchannelVerdict {
	list := greetings[flow.Lang(language)]
	if list == nil {
		return verdictContent
	}
	joined := strings.Join(ackSegments(text), "")
	if joined == "" {
		return verdictGreetingSoFar
	}
	if isPhraseRun(list, joined, greetingMaxWords) {
		return verdictGreeting
	}
	if isGreetingRunPrefix(list, joined, greetingMaxWords) {
		return verdictGreetingSoFar
	}
	return verdictContent
}

// isPhraseRun reports whether s is one to max phrases of the list, in any order
// and with any repetition, nothing else: greeting words (classifyGreeting) or
// acknowledgements (isAckRun). Every split is tried. Empty s is a run of none.
func isPhraseRun(list []string, s string, max int) bool {
	if s == "" {
		return true
	}
	if max == 0 {
		return false
	}
	for _, word := range list {
		if word != "" && strings.HasPrefix(s, word) && isPhraseRun(list, s[len(word):], max-1) {
			return true
		}
	}
	return false
}

// isGreetingRunPrefix reports whether s is a proper prefix of a run of at most
// max greeting words: whole words, then the beginning of one more.
func isGreetingRunPrefix(list []string, s string, max int) bool {
	if max == 0 {
		return false
	}
	for _, word := range list {
		if len(s) < len(word) && strings.HasPrefix(word, s) {
			return true
		}
		if strings.HasPrefix(s, word) && len(s) > len(word) &&
			isGreetingRunPrefix(list, s[len(word):], max-1) {
			return true
		}
	}
	return false
}

// isOpeningHoldWords reports whether the words of an utterance held over the
// opening keep the hold going: an acknowledgement, a greeting, or the start of
// either. Only over the opening does a greeting count; elsewhere it is words.
func isOpeningHoldWords(language, text string) bool {
	return classifyBackchannel(language, text) != verdictContent ||
		classifyGreeting(language, text) != verdictContent
}
