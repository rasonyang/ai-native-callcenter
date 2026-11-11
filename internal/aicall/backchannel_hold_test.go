// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// holdModel is a model whose client controls interruption
// (provider.CallerTurnController), as *provider.Realtime does on openai.
type holdModel struct {
	*fakeModel
	hmu      sync.Mutex
	answered []string
	declined []string
}

func (m *holdModel) AnswerCallerTurn(itemID string) error {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	m.answered = append(m.answered, itemID)
	return nil
}

func (m *holdModel) DeclineCallerTurn(itemID string) error {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	m.declined = append(m.declined, itemID)
	return nil
}

func (m *holdModel) answers() []string {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	return slices.Clone(m.answered)
}

func (m *holdModel) declines() []string {
	m.hmu.Lock()
	defer m.hmu.Unlock()
	return slices.Clone(m.declined)
}

// holdClock is the hold's cap in virtual time: advance fires the timers that
// came due, in order, without a real sleep.
type holdClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*holdTimer
}

type holdTimer struct {
	due     time.Duration
	f       func()
	stopped bool
	fired   bool
}

func (c *holdClock) after(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &holdTimer{due: c.now + d, f: f}
	c.timers = append(c.timers, timer)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		wasActive := !timer.stopped && !timer.fired
		timer.stopped = true
		return wasActive
	}
}

func (c *holdClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var due []*holdTimer
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired && timer.due <= c.now {
			timer.fired = true
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.f()
	}
}

// pending is how many caps are running.
func (c *holdClock) pending() (n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired {
			n++
		}
	}
	return n
}

type holdRig struct {
	session *Session
	leg     *fakeLeg
	model   *holdModel
	clock   *holdClock
}

func startHold(t *testing.T, language string, cfg Config) *holdRig {
	t.Helper()
	r := &holdRig{clock: &holdClock{}}
	session, leg, _ := startBridgeModel(t, provider.OpenAIProfile(), cfg,
		func(m *fakeModel) provider.VoiceSession {
			r.model = &holdModel{fakeModel: m}
			return r.model
		})
	r.session, r.leg = session, leg
	session.holdAfter = r.clock.after
	session.cfg.Session.Language = language
	awaitBridgeEvent(t, session, EventTypeReady)
	return r
}

func (r *holdRig) send(e provider.Event) { r.model.events <- e }

// botSpeaks starts a turn whose audio is 50 frames, 40 of them still queued.
func (r *holdRig) botSpeaks() {
	r.leg.holdFrames(40)
	r.send(provider.Event{Type: provider.EventTypeResponseStarted})
	r.send(provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*50)})
}

func (r *holdRig) started(item string) {
	r.send(provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: item})
}

func (r *holdRig) stopped(item string) {
	r.send(provider.Event{Type: provider.EventTypeSpeechStopped, ItemID: item})
}

func (r *holdRig) partial(item, text string) {
	r.send(provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: text})
}

func (r *holdRig) final(item, text string) {
	r.send(provider.Event{Type: provider.EventTypeInputTranscript, ItemID: item, Text: text, IsFinal: true})
}

// eventsUntil returns every event up to and including the first of type want
// that match accepts (nil accepts any).
func eventsUntil(t *testing.T, session *Session, want EventType, match func(Event) bool) []Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var seen []Event
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatalf("the stream closed while waiting for %s (saw %v)", want, typesOf(seen))
			}
			seen = append(seen, event)
			if event.Type == want && (match == nil || match(event)) {
				return seen
			}
		case <-deadline:
			t.Fatalf("no %s event arrived (saw %v)", want, typesOf(seen))
		}
	}
}

func isFinalWords(e Event) bool { return e.IsFinal }

func typesOf(events []Event) (types []EventType) {
	for _, e := range events {
		types = append(types, e.Type)
	}
	return types
}

// assertNoCut fails when the events or the model show the bot was cut.
func (r *holdRig) assertNoCut(t *testing.T, events []Event) {
	t.Helper()
	for _, e := range events {
		switch e.Type {
		case EventTypeBargeIn, EventTypeCallerSpeech:
			t.Errorf("a %s event was published for a held acknowledgement (events %v)", e.Type, typesOf(events))
		}
	}
	if got := r.model.recordedInterrupts(); len(got) != 0 {
		t.Errorf("the model was told of %d interruptions, want none", len(got))
	}
	if got := r.leg.clears(); got != 0 {
		t.Errorf("the playback queue was flushed %d times, want none", got)
	}
	if got := r.model.answers(); len(got) != 0 {
		t.Errorf("the caller's turn was asked for %v, want no answer to an acknowledgement", got)
	}
}

// An acknowledgement over the bot's generation is held and declined: nothing is
// cut, nothing answers it, and it is recorded as the caller's words, flagged.
func TestAnAcknowledgementOverGenerationIsHeldAndDeclined(t *testing.T) {
	for _, tt := range []struct {
		name, language string
		deltas         []string
		final          string
	}{
		{"zh", "zh", []string{"嗯"}, "嗯。"},
		{"zh-CN", "zh-CN", []string{" ", "嗯", "嗯"}, "嗯嗯"},
		{"en", "en", []string{"Mm", "-hmm", "."}, "Mm-hmm."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := startHold(t, tt.language, Config{NoInput: -1})
			r.botSpeaks()
			r.started("item-1")
			for _, d := range tt.deltas {
				r.partial("item-1", d)
			}
			r.stopped("item-1")
			r.final("item-1", tt.final)
			events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)

			r.assertNoCut(t, events)
			last := events[len(events)-1]
			if !last.IsBackchannel || last.Text != tt.final {
				t.Errorf("the final words = %+v, want the transcript flagged as a backchannel", last)
			}
			if got := r.model.declines(); !slices.Equal(got, []string{"item-1"}) {
				t.Errorf("declined %v, want item-1", got)
			}
			if r.clock.pending() != 0 {
				t.Error("the cap of a decided utterance is still running")
			}
		})
	}
}

// The same in the tail of a finished turn, with the default 800 ms guard on: the
// guard is not what holds it, and nothing else would answer such a turn.
func TestAnAcknowledgementOverTheTailIsHeldInsideTheGuard(t *testing.T) {
	r := startHold(t, "en", Config{NoInput: -1}) // BargeGuard 0 is the 800 ms default
	r.botSpeaks()
	r.send(provider.Event{Type: provider.EventTypeResponseDone})
	awaitBridgeEvent(t, r.session, EventTypeTurnDone)

	r.started("item-1")
	r.partial("item-1", "yeah")
	r.stopped("item-1")
	r.final("item-1", "Yeah.")
	events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
	r.assertNoCut(t, events)
	if !events[len(events)-1].IsBackchannel {
		t.Error("the acknowledgement was not flagged")
	}
}

// Words in a partial take the floor at once, with the played time at the frame
// reached, and the turn is answered when the speech stops.
func TestWordsInAPartialTakeTheFloorAtOnce(t *testing.T) {
	r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
	r.botSpeaks()
	r.leg.holdFrames(30) // 50 queued, 30 still to play: 20 heard

	r.started("item-1")
	r.partial("item-1", "wait")
	events := eventsUntil(t, r.session, EventTypeCallerSpeech, nil)

	if got := typesOf(events); !slices.Equal(got, []EventType{EventTypeBargeIn, EventTypeCallerSpeech}) {
		t.Fatalf("events = %v, want the cut and then the speech", got)
	}
	if cut := events[0]; cut.Played != 400*time.Millisecond || !cut.IsGenerating {
		t.Errorf("BARGE_IN played %v generating %v, want 400ms while generating", cut.Played, cut.IsGenerating)
	}
	waitFor(t, "the model told of the cut", func() bool { return len(r.model.recordedInterrupts()) == 1 })
	if got := r.model.recordedInterrupts()[0]; got.reason != provider.InterruptReasonSpeech || got.playedMs != 400 {
		t.Errorf("interrupt = %+v, want speech at 400 ms", got)
	}
	if got := r.model.answers(); len(got) != 0 {
		t.Errorf("answered %v before the speech stopped", got)
	}

	r.stopped("item-1")
	waitFor(t, "the turn asked for at the stop", func() bool { return len(r.model.answers()) == 1 })
	r.final("item-1", "wait a second")
	words := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
	if words[len(words)-1].IsBackchannel {
		t.Error("words were flagged as a backchannel")
	}
	if got := r.model.declines(); len(got) != 0 {
		t.Errorf("declined %v, want none", got)
	}
	if got := r.model.answers(); !slices.Equal(got, []string{"item-1"}) {
		t.Errorf("answers = %v, want item-1 once", got)
	}
}

// With no partial, the final decides; speech that has stopped is answered the
// moment the floor is taken.
func TestWordsInAFinalTakeTheFloorAndAreAnsweredAtOnce(t *testing.T) {
	r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
	r.botSpeaks()
	r.started("item-1")
	r.stopped("item-1")
	r.final("item-1", "等一下")
	events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)

	if got := typesOf(events); !slices.Equal(got, []EventType{EventTypeBargeIn, EventTypeCallerSpeech, EventTypeCustomerSaid}) {
		t.Errorf("events = %v, want cut, speech, words", got)
	}
	if got := r.model.answers(); !slices.Equal(got, []string{"item-1"}) {
		t.Errorf("answers = %v, want item-1 once", got)
	}
}

// Utterances overlap: the first is an acknowledgement, the second is words.
func TestOverlappingUtterancesAreJudgedEachOnItsOwn(t *testing.T) {
	r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
	r.botSpeaks()
	r.started("a")
	r.stopped("a")
	r.started("b")
	r.partial("b", "uh")
	r.final("a", "uh-huh")
	r.partial("b", " no")
	r.stopped("b")
	r.final("b", "uh no")
	events := eventsUntil(t, r.session, EventTypeCustomerSaid, func(e Event) bool {
		return e.IsFinal && !e.IsBackchannel
	})

	if got := r.model.declines(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("declined %v, want only a", got)
	}
	if got := r.model.answers(); !slices.Equal(got, []string{"b"}) {
		t.Errorf("answers = %v, want only b", got)
	}
	cuts := 0
	for _, e := range events {
		if e.Type == EventTypeBargeIn {
			cuts++
		}
	}
	if cuts != 1 {
		t.Errorf("%d cuts, want one for b", cuts)
	}
}

// No transcript at all: the cap takes the floor at exactly 2000 ms.
func TestAnUtteranceWithNoTranscriptTakesTheFloorAtTheCap(t *testing.T) {
	r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
	r.botSpeaks()
	r.started("item-1")
	r.stopped("item-1")
	waitFor(t, "the cap to start", func() bool { return r.clock.pending() == 1 })

	r.clock.advance(holdCap - time.Millisecond)
	settle()
	if got := r.leg.clears(); got != 0 {
		t.Fatalf("flushed %d times one millisecond before the cap", got)
	}
	r.clock.advance(time.Millisecond)
	events := eventsUntil(t, r.session, EventTypeCallerSpeech, nil)
	if got := typesOf(events); !slices.Equal(got, []EventType{EventTypeBargeIn, EventTypeCallerSpeech}) {
		t.Errorf("events = %v, want the cut and the speech", got)
	}
	waitFor(t, "the stopped speech answered", func() bool { return len(r.model.answers()) == 1 })
}

// A keypress during a hold takes the floor at once, as it always did.
func TestAKeypressDuringAHoldTakesTheFloor(t *testing.T) {
	r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
	r.botSpeaks()
	r.started("item-1")
	waitFor(t, "the utterance to be held", func() bool { return r.clock.pending() == 1 })
	r.leg.digits <- "1"
	cut := awaitBridgeEvent(t, r.session, EventTypeBargeIn)
	if cut.Text != string(provider.InterruptReasonDTMF) {
		t.Errorf("cut by %q, want the keypress", cut.Text)
	}
}

// A cough: an empty final takes the floor, as today; an empty partial is no
// information.
func TestAnEmptyFinalTakesTheFloor(t *testing.T) {
	r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
	r.botSpeaks()
	r.started("item-1")
	waitFor(t, "the cap to start", func() bool { return r.clock.pending() == 1 })
	r.partial("item-1", " ")
	r.clock.advance(holdCap / 2)
	settle()
	if got := r.leg.clears(); got != 0 {
		t.Fatalf("an empty partial took the floor (flushed %d times)", got)
	}
	r.stopped("item-1")
	r.final("item-1", "")
	eventsUntil(t, r.session, EventTypeCallerSpeech, nil)
	waitFor(t, "the turn asked for", func() bool { return len(r.model.answers()) == 1 })
}

// A caller who says "mm-hmm" every second for half a minute never takes the
// floor, and the cap never fires.
func TestAnAcknowledgementEverySecondForHalfAMinuteNeverCutsTheBot(t *testing.T) {
	r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
	r.botSpeaks()
	for i := range 30 {
		item := "item-" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		r.started(item)
		r.partial(item, "嗯")
		r.stopped(item)
		r.final(item, "嗯。")
		// A second of the call passes only once this utterance is decided, as
		// it would on a call: the caps run in virtual time, not in the pump's.
		waitFor(t, "the acknowledgement to be declined", func() bool { return len(r.model.declines()) == i+1 })
		r.clock.advance(time.Second)
	}
	r.send(provider.Event{Type: provider.EventTypeResponseDone})
	events := eventsUntil(t, r.session, EventTypeTurnDone, nil)
	r.assertNoCut(t, events)
	if got := len(r.model.declines()); got != 30 {
		t.Errorf("declined %d acknowledgements, want 30", got)
	}
	flagged := 0
	for _, e := range events {
		if e.Type == EventTypeCustomerSaid && e.IsFinal && e.IsBackchannel {
			flagged++
		}
	}
	if flagged != 30 {
		t.Errorf("%d flagged transcripts, want 30", flagged)
	}
}

// The turn ends in the middle of an utterance: the verdict still applies.
func TestTheVerdictAppliesAfterPlaybackEnded(t *testing.T) {
	t.Run("acknowledgement", func(t *testing.T) {
		r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
		r.botSpeaks()
		r.started("item-1")
		r.send(provider.Event{Type: provider.EventTypeResponseDone})
		awaitBridgeEvent(t, r.session, EventTypeTurnDone)
		r.leg.holdFrames(0) // playback ended
		r.stopped("item-1")
		r.final("item-1", "okay")
		events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
		r.assertNoCut(t, events)
		if got := r.model.declines(); !slices.Equal(got, []string{"item-1"}) {
			t.Errorf("declined %v, want item-1", got)
		}
	})
	t.Run("words", func(t *testing.T) {
		r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
		r.botSpeaks()
		r.started("item-1")
		r.send(provider.Event{Type: provider.EventTypeResponseDone})
		awaitBridgeEvent(t, r.session, EventTypeTurnDone)
		r.leg.holdFrames(0)
		r.stopped("item-1")
		r.final("item-1", "what is the price")
		events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
		for _, e := range events {
			if e.Type == EventTypeBargeIn {
				t.Error("a BARGE_IN was published with nothing left to cut")
			}
		}
		if got := r.model.answers(); !slices.Equal(got, []string{"item-1"}) {
			t.Errorf("answers = %v, want the caller's turn answered", got)
		}
	})
}

// An acknowledgement over an armed closing line is invisible to the ending: no
// BARGE_IN reaches it, and the dead-air watch is left alone.
func TestAnAcknowledgementOverAnArmedLineIsInvisibleToTheEnding(t *testing.T) {
	r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1,
		IsEndingArmed: func() bool { return true }})
	r.botSpeaks()
	r.send(provider.Event{Type: provider.EventTypeResponseDone})
	awaitBridgeEvent(t, r.session, EventTypeTurnDone)

	idleBefore := idleGenerationOf(r.session)
	r.started("item-1")
	r.stopped("item-1")
	r.final("item-1", "mm-hmm")
	events := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
	r.assertNoCut(t, events)
	if idleBefore != idleGenerationOf(r.session) {
		t.Error("the dead-air watch was invalidated by an acknowledgement")
	}
}

func idleGenerationOf(s *Session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idleGeneration
}

// Speech while nothing is playing is an ordinary turn: no hold, the caller is
// heard at once and answered at the stop, without the transcript. A "嗯" there
// is a real answer.
func TestSpeechWithNothingPlayingIsNotHeld(t *testing.T) {
	r := startHold(t, "zh", Config{BargeGuard: -1, NoInput: -1})
	r.started("item-1")
	events := eventsUntil(t, r.session, EventTypeCallerSpeech, nil)
	if got := typesOf(events); !slices.Equal(got, []EventType{EventTypeCallerSpeech}) {
		t.Errorf("events = %v, want the speech alone", got)
	}
	r.stopped("item-1")
	waitFor(t, "the turn asked for at the stop", func() bool { return len(r.model.answers()) == 1 })
	if r.clock.pending() != 0 {
		t.Error("a cap was started for speech with nothing to hold")
	}

	r.final("item-1", "嗯")
	words := eventsUntil(t, r.session, EventTypeCustomerSaid, isFinalWords)
	if words[len(words)-1].IsBackchannel {
		t.Error("an answer to a question that finished playing was flagged as a backchannel")
	}
	if got := r.model.declines(); len(got) != 0 {
		t.Errorf("declined %v, want none", got)
	}
}

// Speech over a response that is being made and has not been heard: nothing
// cancels it any more, so the session does, as the provider's own detection did.
func TestSpeechOverASilentResponseCancelsIt(t *testing.T) {
	r := startHold(t, "en", Config{BargeGuard: -1, NoInput: -1})
	r.send(provider.Event{Type: provider.EventTypeResponseStarted})
	r.started("item-1")
	waitFor(t, "the response cancelled", func() bool { return len(r.model.recordedInterrupts()) == 1 })
	if got := r.model.recordedInterrupts()[0]; got.reason != provider.InterruptReasonSpeech || got.playedMs != -1 {
		t.Errorf("interrupt = %+v, want speech with nothing heard", got)
	}
	if got := r.leg.clears(); got != 0 {
		t.Errorf("flushed %d times with nothing queued", got)
	}
}

// A session whose model has no control keeps the engine's own interruption: the
// speech cuts at once and nothing is held.
func TestWithoutTheControlSpeechCutsAtOnce(t *testing.T) {
	session, leg, model := startBridgeWith(t, provider.OpenAIProfile(), Config{BargeGuard: -1, NoInput: -1})
	awaitBridgeEvent(t, session, EventTypeReady)
	leg.holdFrames(40)
	model.events <- provider.Event{Type: provider.EventTypeResponseStarted}
	model.events <- provider.Event{Type: provider.EventTypeAudioDelta, Audio: make([]byte, media.FrameSamples*50)}
	model.events <- provider.Event{Type: provider.EventTypeSpeechStarted, ItemID: "item-1"}
	awaitBridgeEvent(t, session, EventTypeBargeIn)
	if session.isHolding() {
		t.Error("a session whose model has no control holds")
	}
}
