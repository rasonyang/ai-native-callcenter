// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// #715: a turn the session took for the opening, or its replay, counts as
// the opening heard only when its own words are the opening's.

const offerOpeningEN = "Hello, this is the renewals desk. Renewing now is 20% off with 100 more minutes a month, would you like it?"

func TestTheOpeningsWordsAreJudgedInOrderNotExactly(t *testing.T) {
	long := strings.Repeat("您好，这是一段很长的开场白。", 60) + "请问您需要吗？"
	for _, tc := range []struct {
		name, opening, words string
		isCut                bool
		want                 openingWords
	}{
		{"as written", twoPartOpeningLine, twoPartOpeningLine, false, openingWordsSaid},
		{"punctuation, spaces and width", twoPartOpeningLine,
			"您好 我是套餐续约专员小安 给您来电是想告诉您 现在续约套餐可以享受八折优惠 每月还多送一百分钟通话 请问您需要吗", false, openingWordsSaid},
		{"numerals written another way", twoPartOpeningLine,
			"您好，我是套餐续约专员小安。给您来电是想告诉您，现在续约套餐可以享受8折优惠，每月还多送100分钟通话，请问您需要吗？", false, openingWordsSaid},
		{"split into deltas with a word added", twoPartOpeningLine,
			"您好，我是套餐续约专员小安。今天给您来电是想告诉您，现在续约套餐可以享受八折优惠，每月还多送一百分钟通话，请问您需要吗？", false, openingWordsSaid},
		{"a line check", twoPartOpeningLine, "您好，能听到我说话吗？", false, openingWordsOther},
		{"the greeting only", twoPartOpeningLine, "您好，", false, openingWordsOther},
		{"the final question only", twoPartOpeningLine, "请问您需要吗？", false, openingWordsOther},
		{"a line check, then the final question", twoPartOpeningLine, "喂，您能听到我说话吗？请问您需要吗？", false, openingWordsOther},
		{"all but the final clause", twoPartOpeningLine,
			"您好，我是套餐续约专员小安。给您来电是想告诉您，现在续约套餐可以享受八折优惠，每月还多送一百分钟通话。", false, openingWordsOther},
		{"no transcript", twoPartOpeningLine, "", false, openingWordsNone},
		{"punctuation only", twoPartOpeningLine, "。。", false, openingWordsNone},
		{"English, case and width", offerOpeningEN,
			"HELLO, this is the renewals desk. Renewing now is 20% off with 100 more minutes a month — would you like it?", false, openingWordsSaid},
		{"English line check", offerOpeningEN, "Hello? Can you hear me?", false, openingWordsOther},
		{"English, the question alone", offerOpeningEN, "Would you like it?", false, openingWordsOther},
		{"words cut at the session's budget", long, cutAtBytes(long, maxTurnTextBytes), true, openingWordsSaid},
		{"other words cut at the budget", long, cutAtBytes(strings.Repeat("能听到我说话吗？", 80), maxTurnTextBytes), true, openingWordsOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := judgeOpeningWords(tc.opening, tc.words, tc.isCut); got != tc.want {
				t.Errorf("judgeOpeningWords = %v, want %v", got, tc.want)
			}
		})
	}
}

// The final clause is the text after the opening's last clause separator, in
// compact runes, as cutInFinalClause dates it.
func TestTheOpeningsFinalClauseStartsAfterItsLastSeparator(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{twoPartOpeningLine, "请问您需要吗"},
		{offerOpeningEN, "wouldyoulikeit"},
		{noCommaOpeningLine, "给您来电是想告诉您现在续约套餐可以享受八折优惠每月还多送一百分钟通话请问您需要吗"},
		{"每月只要1,000元，可以吗？」", "可以吗"},
		{"没有分隔的一句话", "没有分隔的一句话"},
	} {
		line := compactRunes(tc.line)
		if got := string(line[finalClauseStart(tc.line):]); got != tc.want {
			t.Errorf("final clause of %q = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// A turn taken for the opening that said other words, played in full, logs
// one line naming the turn and never the words, leaves no tail to be heard,
// and does not count; one that said the opening counts once heard; one with no
// transcript counts by its audio, as before, and says so.
func TestATurnTakenForTheOpeningCountsByItsWords(t *testing.T) {
	newOpeningSession := func(t *testing.T) (*Session, *syncBuffer) {
		t.Helper()
		logs := &syncBuffer{}
		cfg := quietConfig()
		cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
		cfg.Session.OpeningText = twoPartOpeningLine
		s, err := New(newFakeLeg(media.LawMu), newFakeModel(), provider.QwenProfile(), cfg)
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		return s, logs
	}
	playsInFull := func(s *Session, words string) {
		s.beginTurn(false, false)
		if words != "" {
			s.noteBotWords(words, true)
		}
		s.mu.Lock()
		s.framesQueued = 100
		s.mu.Unlock()
		s.finishTurn(false)
		s.settleOpeningTail()
		s.noteOpeningHeard(1, true)
	}
	const notSaid = "the turn taken for the opening did not say the opening's words"

	t.Run("other words", func(t *testing.T) {
		s, logs := newOpeningSession(t)
		playsInFull(s, lineCheckLine)
		if s.isOpeningHeard() {
			t.Error("a turn that said a line check counts as the opening heard")
		}
		s.mu.Lock()
		isTail := s.isOpeningTailQueued
		s.mu.Unlock()
		if isTail {
			t.Error("a turn that said other words left a tail to be heard")
		}
		line := logs.String()
		for _, want := range []string{notSaid, "turn=1", "isReplay=false", "words=OTHER", "hasAudio=true"} {
			if !strings.Contains(line, want) {
				t.Errorf("log %q lacks %s", line, want)
			}
		}
		if strings.Contains(line, "听到") || strings.Count(line, notSaid) != 1 {
			t.Errorf("log %q: want one line, without the words", line)
		}
	})
	t.Run("the opening's words", func(t *testing.T) {
		s, logs := newOpeningSession(t)
		playsInFull(s, twoPartOpeningLine)
		if !s.isOpeningHeard() {
			t.Error("a turn that said the opening and played in full is not heard")
		}
		if strings.Contains(logs.String(), notSaid) {
			t.Errorf("logged a turn that said the opening: %s", logs.String())
		}
	})
	t.Run("no transcript", func(t *testing.T) {
		s, logs := newOpeningSession(t)
		playsInFull(s, "")
		if !s.isOpeningHeard() {
			t.Error("a turn with no transcript is not counted by its audio")
		}
		if !strings.Contains(logs.String(), "has no transcript; its audio counts as the opening") {
			t.Errorf("no line for a turn with no transcript: %s", logs.String())
		}
	})
	t.Run("a bare tool call", func(t *testing.T) {
		s, logs := newOpeningSession(t)
		s.beginTurn(false, false)
		s.finishTurn(false)
		if s.isOpeningHeard() {
			t.Error("a turn with nothing said counts as heard")
		}
		for _, want := range []string{notSaid, "words=NONE", "hasAudio=false"} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("log %q lacks %s", logs.String(), want)
			}
		}
	})
}

// Between a turn's TURN_DONE and the hand-off of its playback marker (#627,
// endResponse), finishTurn has run and the watcher has nothing yet. A guarded
// call judged there finds an opening that said its words and is fully queued
// on its way, and heard as soon as its audio has left the leg, with no marker
// at all; one that said other words is neither, so the offer rule replays it.
func TestBeforeItsPlaybackMarkerIsHandedOverTheOpeningIsOnItsWayOrHeard(t *testing.T) {
	newEndedTurn := func(t *testing.T, words string) (*Session, *fakeLeg) {
		t.Helper()
		leg := newFakeLeg(media.LawMu)
		cfg := quietConfig()
		cfg.Logger = slog.New(slog.NewTextHandler(&syncBuffer{}, nil))
		cfg.Session.OpeningText = twoPartOpeningLine
		s, err := New(leg, newFakeModel(), provider.QwenProfile(), cfg)
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		s.beginTurn(false, false)
		s.noteBotWords(words, true)
		s.mu.Lock()
		s.framesQueued = 100
		s.mu.Unlock()
		leg.holdFrames(100)
		// The marker finishTurn returns is not handed to any watcher.
		s.finishTurn(false)
		return s, leg
	}
	t.Run("the opening's words", func(t *testing.T) {
		s, leg := newEndedTurn(t, twoPartOpeningLine)
		if !s.isOpeningOnItsWay() || s.isOpeningHeard() {
			t.Errorf("queued: on its way = %v, heard = %v; want on its way", s.isOpeningOnItsWay(), s.isOpeningHeard())
		}
		leg.holdFrames(0)
		if !s.isOpeningHeard() || s.isOpeningOnItsWay() {
			t.Errorf("played: heard = %v, on its way = %v; want heard", s.isOpeningHeard(), s.isOpeningOnItsWay())
		}
	})
	t.Run("other words", func(t *testing.T) {
		s, leg := newEndedTurn(t, lineCheckLine)
		if s.isOpeningOnItsWay() || s.isOpeningHeard() {
			t.Errorf("queued: on its way = %v, heard = %v; want neither", s.isOpeningOnItsWay(), s.isOpeningHeard())
		}
		leg.holdFrames(0)
		if s.isOpeningOnItsWay() || s.isOpeningHeard() {
			t.Errorf("played: on its way = %v, heard = %v; want neither", s.isOpeningOnItsWay(), s.isOpeningHeard())
		}
	})
}
