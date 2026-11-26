// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The tool-talk gate (#623). A qwen callee once heard the bot say 工具调用 aloud:
// a turn whose words are a tool's name, those words, or "tool call", is the
// model reading out its tool plumbing, and never something to play.
//
// Each turn's words are judged as they arrive (toolTalkWords), normalized to
// letters and digits, lower-cased, full-width folded. They are tool talk when
// they are one or more whole tokens and nothing else; the tokens are the call's
// tool names, 工具调用, "tool call" and the connective 调用 (so "调用 hangup" is
// tool talk). While they are empty, or whole tokens followed by the start of
// one, they could still be.
//
// On an engine whose transcript leads its audio (Profile.TranscriptLeadsAudio,
// qwen) the session holds a turn's audio while its words could still be tool
// talk, releases it in order the moment they diverge, and at the response's end
// (ResponseDone, not the final transcript) drops it when the words are tool talk
// and releases it otherwise. An ordinary turn is not held at all there: its
// words have diverged before its first audio arrives. The hold is bounded
// (toolTalkHoldCap), checked when the next event arrives; at the cap the audio
// is released, and a turn that then turns out to be tool talk is MISSED. Every
// other engine only detects: no hold, and a turn that played tool talk counts
// as MISSED. Tool talk among other words is played and counted as CONTAINED.
//
// A dropped turn is one the caller heard none of. Its TURN_DONE is an
// interrupted one with no audio, so the turns-without-a-tool wall, an armed
// ending and the claim watch treat it as a turn cut before anything was heard,
// with one difference its IsDropped flag makes: an owed closing line due in it
// is asked for once more, as after a heard turn that missed it
// (callActions.onTurnDropped); it is muted (its words are not the bot's in the transcript); the turn timer is
// put back as for an abandoned response that queued nothing, so the dead air
// after it is #448's when it answered the caller; the playback watcher is handed
// a marker that arms the dead-air watch and publishes no PLAYBACK_DONE; and an
// opening line (turn 1 or its replay) that made no tool call is said again, once
// per call. Nothing is flushed: none of the turn was queued, and an earlier
// turn's tail plays on.

// toolTalkHoldCap bounds how long a turn's audio is held for its words: from
// the first chunk held, and as much audio as plays in that time. Longer than a
// tool name or two takes to say, so a turn that is only tool talk is still held
// when its response ends. It costs an ordinary turn nothing on the one engine
// that holds, where the transcript leads the audio (design 02 §5): only a turn
// whose transcript lags or never comes waits for the cap, once, at its start.
const toolTalkHoldCap = 2 * time.Second

// toolTalkHoldNoted is how long a hold must have lasted for its release to be
// logged at INFO rather than DEBUG (releaseToolTalkLocked). An ordinary turn on
// the engine that holds should not be held at all, its words arriving before
// its audio; a hold this long is one a caller could hear as a late start.
const toolTalkHoldNoted = 200 * time.Millisecond

// Why the gate let a turn's held audio go (releaseToolTalkLocked).
type toolTalkRelease uint8

const (
	// releasedDiverged: the words diverged from tool talk.
	releasedDiverged toolTalkRelease = iota
	// releasedAtEnd: the response ended and the words were not tool talk.
	releasedAtEnd
	// releasedAtCap: the hold reached toolTalkHoldCap.
	releasedAtCap
)

// toolTalkPhrases are tool talk on their own, beside the call's tool names.
var toolTalkPhrases = []string{"工具调用", "tool call", "调用"}

// The spellings containment looks for in a turn's raw words (containsToolTalk).
var (
	toolTalkZH   = []byte("工具调用")
	toolTalkTool = []byte("tool")
	toolTalkCall = []byte("call")
)

// toolTalkSettled is what the gate concluded about a turn at its end.
type toolTalkSettled struct {
	// verdict is the metric's label (obs.ToolTalk*), empty for none.
	verdict string
	turn    int
	// words are the turn's words, for the log, when there is a verdict.
	words string
	// isDropped says the gate dropped audio it held at the response's end.
	isDropped bool
	// isOpening and toolCalls decide whether a dropped turn's opening line is
	// said again (sayDroppedOpeningAgain).
	isOpening bool
	toolCalls int
}

// toolTalkWords judges one turn's words as they arrive. It allocates nothing
// once its buffers have grown, which they do in newToolTalkGate.
type toolTalkWords struct {
	// tokens are normalized (appendToolTalkNorm); maxToken is the longest.
	tokens   [][]byte
	maxToken int
	// norm is the turn's words so far, normalized.
	norm []byte
	// reach[i] says norm[:i] is a sequence of whole tokens; reach[0] is true.
	reach []bool
	// isDiverged says the words can no longer be tool talk, whatever follows.
	isDiverged bool
}

// addToken adds one token, normalized, unless it is empty or known.
func (w *toolTalkWords) addToken(text string) {
	token := appendToolTalkNorm(nil, text)
	if len(token) == 0 {
		return
	}
	for _, known := range w.tokens {
		if bytes.Equal(known, token) {
			return
		}
	}
	w.tokens = append(w.tokens, token)
	w.maxToken = max(w.maxToken, len(token))
}

// reset starts a turn.
func (w *toolTalkWords) reset() {
	w.norm = w.norm[:0]
	w.reach = append(w.reach[:0], true)
	w.isDiverged = false
}

// add judges the next piece of the turn's words.
func (w *toolTalkWords) add(text string) {
	if w.isDiverged {
		return
	}
	start := len(w.norm)
	w.norm = appendToolTalkNorm(w.norm, text)
	for j := start + 1; j <= len(w.norm); j++ {
		isReached := false
		for _, token := range w.tokens {
			if n := len(token); n <= j && w.reach[j-n] && bytes.Equal(w.norm[j-n:j], token) {
				isReached = true
				break
			}
		}
		w.reach = append(w.reach, isReached)
	}
	if !w.couldBeToolTalk() {
		w.isDiverged = true
	}
}

// couldBeToolTalk reports whether the words so far are whole tokens followed
// by a proper prefix of one, the empty prefix included. Only the last maxToken
// positions can start that prefix.
func (w *toolTalkWords) couldBeToolTalk() bool {
	n := len(w.norm)
	for i := n; i >= 0 && n-i < w.maxToken; i-- {
		if !w.reach[i] {
			continue
		}
		rest := w.norm[i:]
		if len(rest) == 0 {
			return true
		}
		for _, token := range w.tokens {
			if len(rest) < len(token) && bytes.HasPrefix(token, rest) {
				return true
			}
		}
	}
	return false
}

// isToolTalk reports whether the words are one or more whole tokens and
// nothing else.
func (w *toolTalkWords) isToolTalk() bool {
	return !w.isDiverged && len(w.norm) > 0 && w.reach[len(w.norm)]
}

// appendToolTalkNorm appends text's letters and digits, lower-cased, with the
// full-width ASCII forms folded as foldWidth folds them.
func appendToolTalkNorm(dst []byte, text string) []byte {
	for _, r := range text {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			dst = utf8.AppendRune(dst, unicode.ToLower(r))
		}
	}
	return dst
}

// toolTalkGate is the session's tool-talk state, guarded by Session.mu.
type toolTalkGate struct {
	// isHolding says the engine's transcript leads its audio, so the gate
	// holds audio (Profile.TranscriptLeadsAudio); capBytes is toolTalkHoldCap
	// in the provider's output format.
	isHolding bool
	capBytes  int
	words     toolTalkWords
	// names are the call's tool names, lower-cased, for containment; folded
	// is containsToolTalk's buffer.
	names  [][]byte
	folded []byte

	// The turn being judged. isSettled is set once its verdict is in, and
	// before the call's first turn.
	turn      int
	isSettled bool
	// isUnjudged says the turn's words may hold another turn's late
	// transcript: it is not held and not judged (beginToolTalkLocked).
	isUnjudged bool
	// isReleased says the hold is over for the turn: its audio plays as it
	// comes.
	isReleased bool
	// hasHeld says some of the turn's audio was held, hasPlayed that some of
	// it went to the leg.
	hasHeld   bool
	hasPlayed bool
	// held is the audio held, the provider's own chunks kept by reference
	// (each audio delta is a fresh allocation, as the caller-first hold relies
	// on too), in a slice reused across turns.
	held      [][]byte
	heldBytes int
	heldSince time.Time
	// finals are the turn's final BOT_SAIDs, held back while the turn is held
	// so that a dropped turn's are emitted after it is muted; isFinalsDue says
	// the hold is over and they are owed to the stream (emitDueBotFinals).
	finals      []Event
	isFinalsDue bool

	// isLastEndAbandoned says the previous turn ended because the provider's
	// watchdog gave it up: its transcript may still be arriving.
	isLastEndAbandoned bool
	// isOpeningResaid says a dropped opening has been said again: once per
	// call.
	isOpeningResaid bool
}

// newToolTalkGate is the gate of a call whose provider's output is output.
func newToolTalkGate(isHolding bool, output media.AudioFormat) toolTalkGate {
	bytesPerSecond := output.RateHz
	if output.Encoding == media.EncodingPCM16 {
		bytesPerSecond *= 2
	}
	g := toolTalkGate{
		isHolding: isHolding,
		capBytes:  int(int64(bytesPerSecond) * toolTalkHoldCap.Milliseconds() / 1000),
		isSettled: true,
		words: toolTalkWords{
			norm:  make([]byte, 0, maxTurnTextBytes),
			reach: make([]bool, 1, maxTurnTextBytes+1),
		},
		folded: make([]byte, 0, maxTurnTextBytes),
		held:   make([][]byte, 0, 64),
	}
	g.words.reach[0] = true
	for _, phrase := range toolTalkPhrases {
		g.words.addToken(phrase)
	}
	return g
}

// setTools adds the call's tool names to what is tool talk. Session.Start
// calls it, before anything is judged.
func (g *toolTalkGate) setTools(tools []provider.ToolSpec) {
	for _, tool := range tools {
		g.words.addToken(tool.Name)
		if name := strings.ToLower(tool.Name); name != "" {
			g.names = append(g.names, []byte(name))
		}
	}
}

// containsToolTalk reports whether a turn's raw words name tool calls or a
// tool: 工具调用 anywhere, "tool call" (or "tool-call", "toolcall", "tool calls")
// and a tool's name as whole words, so the hangup of "hangups" is not one.
func (g *toolTalkGate) containsToolTalk(raw []byte) bool {
	g.folded = g.folded[:0]
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		raw = raw[size:]
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		g.folded = utf8.AppendRune(g.folded, r)
	}
	if bytes.Contains(g.folded, toolTalkZH) || hasToolCallWords(g.folded) {
		return true
	}
	for _, name := range g.names {
		if hasWholeWord(g.folded, name) {
			return true
		}
	}
	return false
}

// isWordByte says b belongs to an ASCII word (after lower-casing): a byte of a
// tool name. Anything else, a Han character included, bounds a word.
func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

// hasWholeWord reports whether word occurs in s bounded on both sides.
func hasWholeWord(s, word []byte) bool {
	for i := 0; i < len(s); {
		j := bytes.Index(s[i:], word)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(word)
		if (start == 0 || !isWordByte(s[start-1])) && (end == len(s) || !isWordByte(s[end])) {
			return true
		}
		i = start + 1
	}
	return false
}

// hasToolCallWords reports whether s says "tool call": "tool", any run of
// spaces, hyphens or underscores, "call", an optional "s", as whole words.
func hasToolCallWords(s []byte) bool {
	for i := 0; i < len(s); {
		j := bytes.Index(s[i:], toolTalkTool)
		if j < 0 {
			return false
		}
		start := i + j
		i = start + 1
		if start > 0 && isWordByte(s[start-1]) {
			continue
		}
		k := start + len(toolTalkTool)
		for k < len(s) && (s[k] == ' ' || s[k] == '-' || s[k] == '_') {
			k++
		}
		if !bytes.HasPrefix(s[k:], toolTalkCall) {
			continue
		}
		k += len(toolTalkCall)
		if k < len(s) && s[k] == 's' {
			k++
		}
		if k == len(s) || !isWordByte(s[k]) {
			return true
		}
	}
	return false
}

// toolTalkClock is the gate's time source: time.Now unless a test set one.
func (s *Session) toolTalkClock() time.Time {
	if s.talkNow != nil {
		return s.talkNow()
	}
	return time.Now()
}

// holdToolTalkLocked decides one chunk of the turn's audio that passed the
// hush check and the cut-off fence, and reports whether it is held. On an
// engine that only detects, nothing is. The cap is judged here, as the next
// chunk arrives: past it the held audio is released and this chunk plays. The
// caller holds mu.
func (s *Session) holdToolTalkLocked(audio []byte) bool {
	g := &s.talk
	if !g.isHolding || g.isSettled || g.isReleased || g.isUnjudged || g.words.isDiverged {
		return false
	}
	now := s.toolTalkClock()
	if len(g.held) > 0 && (now.Sub(g.heldSince) >= toolTalkHoldCap || g.heldBytes+len(audio) > g.capBytes) {
		s.releaseToolTalkLocked(releasedAtCap)
		return false
	}
	if len(g.held) == 0 {
		g.heldSince = now
	}
	g.held = append(g.held, audio)
	g.heldBytes += len(audio)
	g.hasHeld = true
	return true
}

// noteToolTalkWordsLocked judges the next piece of the turn's words
// (noteBotWords) and ends the hold when they diverge or the cap has passed.
// The caller holds mu.
func (s *Session) noteToolTalkWordsLocked(text string) {
	g := &s.talk
	if g.isSettled || g.isUnjudged {
		return
	}
	g.words.add(text)
	if !g.isHolding || g.isReleased {
		return
	}
	switch {
	case g.words.isDiverged:
		s.releaseToolTalkLocked(releasedDiverged)
	case len(g.held) > 0 && s.toolTalkClock().Sub(g.heldSince) >= toolTalkHoldCap:
		s.releaseToolTalkLocked(releasedAtCap)
	}
}

// releaseToolTalkLocked ends the turn's hold: what is held plays, in order,
// as it would have on arrival, unless the call is hushed or the turn is fenced
// off by now, when it goes as anything arriving then would. why says what
// ended it.
//
// Each release of held audio is logged with how long it was held, so a live
// round can confirm that the hold costs an ordinary qwen turn nothing: at the
// cap at INFO, otherwise at DEBUG, or at INFO once the hold has lasted
// toolTalkHoldNoted. The DEBUG line is built only when DEBUG is on. The caller
// holds mu.
func (s *Session) releaseToolTalkLocked(why toolTalkRelease) {
	g := &s.talk
	g.isReleased = true
	if len(g.finals) > 0 {
		g.isFinalsDue = true
	}
	if len(g.held) == 0 {
		return
	}
	held := s.toolTalkClock().Sub(g.heldSince)
	switch {
	case why == releasedAtCap:
		s.log.Info("released a turn's held audio at the cap; its words could still be tool talk",
			"turn", g.turn, "heldMs", held.Milliseconds(), "heldChunks", len(g.held),
			"heldBytes", g.heldBytes)
	case held >= toolTalkHoldNoted:
		s.log.Info("released a turn's held audio; it was held long enough to be heard as a late start",
			"turn", g.turn, "heldMs", held.Milliseconds(), "heldChunks", len(g.held),
			"isWordsDiverged", why == releasedDiverged)
	case s.log.Enabled(context.Background(), slog.LevelDebug):
		s.log.Debug("released a turn's held audio",
			"turn", g.turn, "heldMs", held.Milliseconds(), "heldChunks", len(g.held),
			"isWordsDiverged", why == releasedDiverged)
	}
	isGone := s.isHushed.Load() || s.isCutOff
	for i, chunk := range g.held {
		if !isGone {
			s.playChunkLocked(chunk)
		}
		g.held[i] = nil
	}
	g.held = g.held[:0]
	g.heldBytes = 0
}

// discardToolTalkLocked drops what the gate holds. Every flush of the turn's
// audio does (stopPlaybackLocked: a barge-in, a keypress, a mute, a
// cancellation), and so does a drop. The caller holds mu.
func (s *Session) discardToolTalkLocked() {
	g := &s.talk
	clear(g.held)
	g.held = g.held[:0]
	g.heldBytes = 0
}

// deferBotFinal holds back a final BOT_SAID of the turn the gate is holding,
// to be emitted when its verdict is in, and reports whether it did. A dropped
// turn is muted first, so drive leaves its words out of the transcript.
func (s *Session) deferBotFinal(said Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := &s.talk
	if !g.isHolding || g.isSettled || g.isReleased || g.isUnjudged || g.words.isDiverged ||
		said.Turn != g.turn {
		return false
	}
	g.finals = append(g.finals, said)
	return true
}

// emitDueBotFinals emits the final BOT_SAIDs the gate held back, once the hold
// is over. Rare: only a turn whose words could still be tool talk at its final
// transcript has any.
func (s *Session) emitDueBotFinals() {
	s.mu.Lock()
	if !s.talk.isFinalsDue {
		s.mu.Unlock()
		return
	}
	finals := s.talk.finals
	s.talk.finals, s.talk.isFinalsDue = nil, false
	s.mu.Unlock()
	for _, said := range finals {
		s.emit(said)
	}
}

// settleUnendedToolTalkLocked judges a turn that was never ended, because the
// next one began while it was open, as a turn cut before it was heard.
// beginTurn calls it before anything of the new turn is set. The caller holds
// mu.
func (s *Session) settleUnendedToolTalkLocked() {
	if s.talk.isSettled {
		return
	}
	s.reportToolTalk(s.settleToolTalkLocked(false, false))
}

// beginToolTalkLocked starts judging the turn beginTurn has just begun.
//
// A transcript event carries no response id, so a late delta or final of one
// turn can land in the next turn's words, and could make an ordinary turn read
// as tool talk. It happens only when the turn began before the last one's
// stream had ended: that one was still open (a provider that starts a response
// before it reports the end of the one it cancelled, openTurns), or the
// provider's watchdog gave it up (its transcript still passes). Such a turn is
// neither held nor judged. Every other turn's events, its transcript's final
// included, arrive before its response ends, and so before the next one starts.
// The caller holds mu, before beginTurn counts the new turn open.
func (s *Session) beginToolTalkLocked() {
	g := &s.talk
	g.isUnjudged = s.openTurns > 0 || g.isLastEndAbandoned
	g.isLastEndAbandoned = false
	g.turn = s.turnSeq
	g.isSettled, g.isReleased, g.hasHeld, g.hasPlayed = false, false, false, false
	g.words.reset()
	s.discardToolTalkLocked()
}

// settleToolTalk is settleToolTalkLocked with mu taken.
func (s *Session) settleToolTalk(isResponseEnd, isAbandoned bool) toolTalkSettled {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settleToolTalkLocked(isResponseEnd, isAbandoned)
}

// settleToolTalkLocked gives the turn its verdict, once, at its end:
// isResponseEnd at ResponseDone (endResponse), where held audio of a turn that
// is tool talk is dropped; otherwise the turn was cut or replaced, and a flush
// has taken what was held. isAbandoned says the provider's watchdog gave the
// response up. Its finals become due. The caller holds mu.
func (s *Session) settleToolTalkLocked(isResponseEnd, isAbandoned bool) toolTalkSettled {
	g := &s.talk
	if g.isSettled {
		return toolTalkSettled{}
	}
	g.isSettled = true
	g.isLastEndAbandoned = isAbandoned
	if len(g.finals) > 0 {
		g.isFinalsDue = true
	}
	r := toolTalkSettled{turn: g.turn}
	if g.isUnjudged {
		return r
	}
	isTalk := g.words.isToolTalk()
	switch {
	case isTalk && !g.hasPlayed && len(g.held) > 0 && isResponseEnd:
		s.discardToolTalkLocked()
		s.mutedTurn = g.turn
		r.verdict, r.isDropped = obs.ToolTalkDropped, true
		r.isOpening, r.toolCalls = s.isOpeningTurnLocked(g.turn), s.turnToolCalls
	case isTalk && !g.hasPlayed && g.hasHeld:
		// Held until a flush took it, or the turn was cut or replaced: the
		// caller heard none of it either.
		s.discardToolTalkLocked()
		s.mutedTurn = g.turn
		r.verdict = obs.ToolTalkDropped
	default:
		s.releaseToolTalkLocked(releasedAtEnd)
		switch {
		case isTalk && g.hasPlayed:
			r.verdict = obs.ToolTalkMissed
		case !isTalk && g.hasPlayed && g.containsToolTalk(s.turnText):
			r.verdict = obs.ToolTalkContained
		}
	}
	if r.verdict != "" {
		r.words = string(s.turnText)
	}
	return r
}

// reportToolTalk counts and logs a verdict, at WARN: each is a turn the model
// read its plumbing out in.
func (s *Session) reportToolTalk(r toolTalkSettled) {
	if r.verdict == "" {
		return
	}
	obs.RecordToolTalk(r.verdict, s.providerName)
	switch r.verdict {
	case obs.ToolTalkDropped:
		s.log.Warn("dropped a bot turn that was only tool talk; the caller heard none of it",
			"turn", r.turn, "words", r.words, "verdict", r.verdict)
	case obs.ToolTalkMissed:
		s.log.Warn("a bot turn that was only tool talk was played to the caller",
			"turn", r.turn, "words", r.words, "verdict", r.verdict)
	default:
		s.log.Warn("a bot turn named a tool or tool calls among its words; it was played",
			"turn", r.turn, "words", r.words, "verdict", r.verdict)
	}
}

// sayDroppedOpeningAgain says the opening line again when the turn the gate
// dropped was the opening or its replay and made no tool call: the caller has
// heard none of it, and nothing else would say it (the replay's budget and the
// owed opening were spent when it was asked for). Once per call, and not over
// an armed ending or a closed call. A turn that made a tool call is answered by
// the tool's result instead, and the dead-air watch takes any other.
func (s *Session) sayDroppedOpeningAgain(r toolTalkSettled) {
	text := s.cfg.Session.OpeningText
	if !r.isOpening || r.toolCalls > 0 || text == "" || s.isClosed() ||
		(s.cfg.IsEndingArmed != nil && s.cfg.IsEndingArmed()) {
		return
	}
	s.mu.Lock()
	isResaid := s.talk.isOpeningResaid
	s.talk.isOpeningResaid = true
	s.mu.Unlock()
	if isResaid {
		s.log.Info("a dropped opening was said again once already; not again", "turn", r.turn)
		return
	}
	s.log.Info("the opening was dropped as tool talk; saying it again", "turn", r.turn)
	s.sayOpening(text)
}
