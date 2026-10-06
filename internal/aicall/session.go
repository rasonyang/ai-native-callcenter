// SPDX-License-Identifier: Apache-2.0

// Package aicall joins one telephone leg to one speech model: audio in both
// directions, barge-in, keypresses, and the lifecycle that ends both sides
// together. It makes no conversational decisions — those belong to the flow
// engine, which consumes the events this package publishes.
package aicall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// frameDurationMs is how much audio one frame carries. It is the unit played
// time is counted in.
const frameDurationMs = 20

// EventType is what the bridge reports upwards.
type EventType string

const (
	// EventTypeReady means the model is configured and the greeting is coming.
	EventTypeReady EventType = "READY"
	// EventTypeCustomerSaid and EventTypeBotSaid are transcript events.
	EventTypeCustomerSaid EventType = "CUSTOMER_SAID"
	EventTypeBotSaid      EventType = "BOT_SAID"
	// EventTypeToolCall is the model asking for something to be done.
	EventTypeToolCall EventType = "TOOL_CALL"
	// EventTypeTurnDone closes a turn, whether it completed or was cut short.
	// It means the model has finished producing, not that the caller has
	// finished hearing.
	EventTypeTurnDone EventType = "TURN_DONE"
	// EventTypePlaybackDone means the last of that audio has left for the
	// caller. Anything that must not happen mid-sentence — a transfer, a
	// hangup — waits for this rather than for TURN_DONE.
	EventTypePlaybackDone EventType = "PLAYBACK_DONE"
	// EventTypeNoInput is dead air: the caller has said nothing since the bot
	// stopped speaking.
	EventTypeNoInput EventType = "NO_INPUT"
	// EventTypeBargeIn is the caller taking the floor back.
	EventTypeBargeIn EventType = "BARGE_IN"
	// EventTypeDigit is a keypress.
	EventTypeDigit EventType = "DIGIT"
	// EventTypeFailed means the conversation cannot continue and the call must
	// go somewhere a person can take it.
	EventTypeFailed EventType = "FAILED"
	// EventTypeEnded is the last event on the stream.
	EventTypeEnded EventType = "ENDED"
)

// Event is one thing that happened on an AI call.
type Event struct {
	Type EventType

	// Turn numbers the model turn an event belongs to (TURN_DONE,
	// PLAYBACK_DONE, TOOL_CALL and BOT_SAID). A consumer sequencing an action "after the line that is
	// about to be spoken" compares turn numbers: the turn a tool call arrived
	// in is not the turn its closing line plays in.
	Turn int

	// Text is transcript text, a digit, or a failure message.
	Text    string
	IsFinal bool

	ToolCallID string
	ToolName   string
	ToolArgs   string

	Status string
	Usage  provider.Usage
	// IsInterrupted marks a TURN_DONE whose turn was cut short — by the
	// caller, by a keypress, or to make room for another turn. Such a turn
	// never reaches PLAYBACK_DONE, and what it produced is not a line anybody
	// can count on having been said.
	IsInterrupted bool

	Err error
	// FailureCause is why a FAILED event happened, when the provider put a name
	// to it. It becomes the call's hangup cause; empty means the failure has no
	// word of its own, which is most of them.
	FailureCause provider.FailureCause
}

// Config parameterises one bridged call.
type Config struct {
	// Session is the model configuration. Its audio formats are filled in from
	// the negotiated law and the provider profile.
	Session provider.SessionConfig

	// BargeGuard is how long after the bot starts speaking that speech
	// detection is ignored.
	//
	// This exists because of what happens on a real line rather than in a lab:
	// the bot's own voice returns through the caller's handset or speakerphone,
	// the provider's detector hears speech, and the bot interrupts itself
	// mid-greeting. Zero uses the default; negative disables the guard.
	BargeGuard time.Duration

	// NoInput is how long of a silent caller, after the bot has finished
	// speaking, counts as dead air. Zero uses the default; negative disables
	// the check.
	NoInput time.Duration

	// IsEndingArmed reports whether the call's ending (a transfer or a hangup)
	// is armed and waiting for its closing line to be heard. The orchestrator
	// owns that truth (callActions); the session only asks, and never while
	// holding mu, because arming reads the session's current turn under the
	// actions' own lock. Nil is never armed.
	IsEndingArmed func() bool

	// Logger is the call's logger. The orchestrator has already bound the
	// call's identity to it (callId, aiccCallId, did), so the session adds
	// no key of its own: slog writes a key bound twice twice.
	Logger *slog.Logger
}

// Defaults chosen from field experience rather than taste.
const (
	// defaultBargeGuard matches what the reference implementation settled on
	// after live calls.
	defaultBargeGuard = 800 * time.Millisecond

	// audibleGapSlack is how far past the end of the queued audio a new frame
	// may arrive and still continue it. Two frames covers the jitter of a model
	// streaming at real time; more silence than that is a gap the caller hears.
	audibleGapSlack = 2 * frameDurationMs * time.Millisecond

	// defaultNoInput is long enough not to talk over a caller who is thinking.
	defaultNoInput = 8 * time.Second
)

// Session is one AI call.
//
// Audio moves through it on two pumps that never block each other: caller
// audio goes up as the wire delivers it, and model audio comes down into a
// queue the RTP session drains on its own clock. Nothing here paces anything;
// pacing belongs to the leg that has to meet a deadline.
type Session struct {
	leg   Leg
	model provider.VoiceSession
	log   *slog.Logger
	// providerName labels latency measurements.
	providerName string

	cfg      Config
	uplink   *media.Converter
	downlink *media.Converter
	framer   *framer
	// playBuffer is reused across chunks; only the model-event pump writes it.
	playBuffer []byte

	events chan Event

	// mu guards everything to do with playback. Two goroutines reach it: the
	// model pump queues speech, and the digit pump cuts it off mid-sentence.
	mu sync.Mutex
	// framesQueued counts frames handed to the leg for the current turn. It is
	// how much the caller has heard, which is the one thing a provider cannot
	// work out for itself.
	framesQueued int
	// isBotSpeaking gates barge-in: there is nothing to interrupt otherwise.
	isBotSpeaking bool
	// isResponding is true from a model turn's start to its end, audio or
	// not: the stretch in which asking for another turn collides with it.
	isResponding bool
	// openTurns counts model turns started and not yet ended or interrupted
	// (capped at two). Turns end in the order they start, so an interruption
	// that arrives while another turn is open belongs to the older one: it is
	// stale, and must not take the floor from the turn that replaced it.
	openTurns int
	// isCutOff is true from a barge-in that flushed a response still being
	// generated until the next turn starts: the provider goes on streaming the
	// cancelled response for a moment (up to half a second on some engines),
	// and what arrives in that stretch belongs to a turn the caller has
	// already stopped hearing. Queued, it plays right after the flush as the
	// tail of the answer they interrupted. Every turn begins with
	// ResponseStarted, so that is where it clears. cutOffChunks counts what
	// was dropped, for the log.
	isCutOff     bool
	cutOffChunks int
	// turnOwedUntil is when an asked-for turn that has not started yet stops
	// counting as the bot's floor; zero is none. See askedForATurn.
	turnOwedUntil time.Time
	// audibleSince is when the caller started hearing the bot after the last
	// silence, which is what the barge-in guard window is measured from. It is
	// set by queueFrame when audio is queued on an idle leg, cleared when
	// playback is stopped, and not touched by a turn that follows another with
	// the queue still full: the caller has heard the bot continuously since the
	// first of them, and the line echo the guard exists for is long over.
	// audibleUntil is when the audio queued so far finishes playing, which is
	// how queueFrame tells a queue that drained from one that is being fed in
	// real time. Both are zero while nothing is audible.
	audibleSince time.Time
	audibleUntil time.Time
	// timer measures caller-stopped to reply-on-the-wire, per turn.
	timer turnTimer

	// Two generation counters invalidate watches already in flight. Stopping a
	// timer races with it firing; letting a stale one fire and recognise
	// itself as stale does not.
	//
	// They are separate because they answer different questions. The drain
	// watch asks "is this turn's audio still on its way to the caller?" — only
	// a new turn or a flush changes that. The idle watch asks "has the caller
	// said anything?" — speech changes that. Sharing one counter was a live
	// bug: the caller murmuring over the tail of a goodbye cancelled the
	// drain watch, PLAYBACK_DONE never fired, and every call ended on the
	// grace cap, five silent seconds late.
	drainGeneration uint64
	idleGeneration  uint64
	// turnSeq numbers model turns, so events can say which turn they belong to.
	turnSeq int
	// playbackDone is signalled when a turn's audio has finished generating and
	// the queue should be watched until it drains.
	playbackDone chan playbackMarker
	// afterPlaybackDone, set only by tests, runs right after PLAYBACK_DONE is
	// published and before the dead-air watch starts.
	afterPlaybackDone func()

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// New bridges a dialog to a model.
//
// The audio formats come from the negotiated law and what the provider
// accepts, so a provider that takes G.711 gets the caller's bytes untouched
// and one that does not gets them converted here.
func New(leg Leg, model provider.VoiceSession, profile provider.Profile,
	cfg Config) (*Session, error) {

	if leg == nil || model == nil {
		return nil, errors.New("aicall: a session needs both a leg and a model")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	law := leg.Law()
	input, output := profile.FormatsFor(law)

	uplink, err := media.NewConverter(media.G711Format(law), input)
	if err != nil {
		return nil, fmt.Errorf("aicall: caller audio to provider: %w", err)
	}
	downlink, err := media.NewConverter(output, media.G711Format(law))
	if err != nil {
		return nil, fmt.Errorf("aicall: provider audio to caller: %w", err)
	}

	return &Session{
		leg:          leg,
		model:        model,
		log:          log,
		providerName: profile.Name,
		cfg:          cfg,
		uplink:       uplink,
		downlink:     downlink,
		framer:       newFramer(law),
		playBuffer:   make([]byte, 0, 8*media.FrameSamples),
		events:       make(chan Event, 128),
		playbackDone: make(chan playbackMarker, 8),
		done:         make(chan struct{}),
	}, nil
}

// bargeGuard is how long after speech starts that detection is ignored.
func (s *Session) bargeGuard() time.Duration {
	switch {
	case s.cfg.BargeGuard < 0:
		return 0
	case s.cfg.BargeGuard == 0:
		return defaultBargeGuard
	default:
		return s.cfg.BargeGuard
	}
}

// noInputAfter is how long a silent caller counts as dead air; zero disables.
func (s *Session) noInputAfter() time.Duration {
	switch {
	case s.cfg.NoInput < 0:
		return 0
	case s.cfg.NoInput == 0:
		return defaultNoInput
	default:
		return s.cfg.NoInput
	}
}

// Events yields what happened on the call. The stream ends with a single ENDED
// event and is then closed, so it is safe to range over.
func (s *Session) Events() <-chan Event { return s.events }

// Start configures the model and begins moving audio.
func (s *Session) Start(ctx context.Context) error {
	sessionCfg := s.cfg.Session
	law := s.leg.Law()
	sessionCfg.InputFormat = s.uplink.To()
	sessionCfg.OutputFormat = s.downlink.From()

	if err := s.model.Start(ctx, sessionCfg); err != nil {
		return fmt.Errorf("aicall: start model: %w", err)
	}
	s.log.Info("ai call bridged",
		"law", law.String(),
		"toProvider", sessionCfg.InputFormat.String(),
		"fromProvider", sessionCfg.OutputFormat.String(),
		"isPassthrough", s.uplink.IsPassthrough())

	s.wg.Add(5)
	go s.pumpCallerAudio()
	go s.pumpModelEvents()
	go s.pumpDigits()
	go s.watchLeg()
	go s.watchPlayback()

	// Every publisher is one of those five. Once they have all returned,
	// nothing can emit any more, which is the only point at which closing the
	// stream is safe.
	go func() {
		s.wg.Wait()
		select {
		case s.events <- Event{Type: EventTypeEnded}:
		default:
		}
		close(s.events)
	}()

	s.emit(Event{Type: EventTypeReady})
	return nil
}

// Close ends the call from this side and releases both halves.
func (s *Session) Close(ctx context.Context) {
	s.closeOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		s.turnOwedUntil = time.Time{}
		s.mu.Unlock()
		_ = s.model.Close(ctx)
		s.leg.Stop()
	})
}

// isClosed reports whether Close has been called.
func (s *Session) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Wait blocks until every pump has stopped.
func (s *Session) Wait() { s.wg.Wait() }

//
// Conversation control, for the flow engine.
//

// AnswerTool replies to a tool call and steers the next turn.
func (s *Session) AnswerTool(toolCallID, output, hint string) error {
	if err := s.model.SendToolResult(toolCallID, output, hint); err != nil {
		return err
	}
	s.askedForATurn()
	return nil
}

// SendCue sends the model a text it answers with a turn of its own: a
// dead-air check, a goodbye, a keypress.
func (s *Session) SendCue(text string) error {
	if err := s.model.SendUserText(text); err != nil {
		return err
	}
	s.askedForATurn()
	return nil
}

// askedForATurn records that the model owes a turn which has not started: a
// tool result or a cue was just sent, and the provider's answer begins one
// round trip later (ResponseStarted). In that gap nothing else says the bot
// holds the floor, and a cue sent then is refused ("already has an active
// response") on the Realtime providers.
//
// The claim expires after actionGraceCap so a turn that never comes (a
// provider that answers a result with silence) cannot pin the floor: the
// caller of isHoldingTheFloor has its own backstop as well.
func (s *Session) askedForATurn() {
	s.mu.Lock()
	s.turnOwedUntil = time.Now().Add(actionGraceCap)
	s.mu.Unlock()
}

// Reinstruct replaces the standing instructions, which is how a flow moves the
// conversation between phases.
func (s *Session) Reinstruct(text string) error {
	return s.model.UpdateInstructions(text)
}

// Speak says a line the flow chose, in the words it chose.
//
// The counterpart to Reinstruct: that one changes what the model is working
// towards, this one is the sentence itself. It pre-empts whatever is being
// said and does not queue behind anything — see provider.VoiceSession — so a
// phase whose whole job is one line gets that line out, not a paraphrase of it
// two turns later.
//
// The opening line of a call is not sent through here. It travels in the
// session configuration, because the first turn is asked for while the session
// is being started and there is no mid-call moment to catch.
//
// isClosing says the line ends the call (provider.VoiceSession.SpeakText).
func (s *Session) Speak(text string, isClosing bool) error {
	return s.model.SpeakText(text, isClosing)
}

//
// Audio.
//

// pumpCallerAudio moves the caller's voice to the model.
func (s *Session) pumpCallerAudio() {
	defer s.wg.Done()

	frames := s.leg.Frames()
	converted := make([]byte, 0, media.FrameSamples*8)

	for {
		select {
		case <-s.done:
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			converted = s.uplink.Convert(converted, frame)
			media.PutBytes(frame)

			if err := s.model.SendAudio(converted); err != nil {
				// A model that cannot be fed is a call that cannot continue.
				// Nothing names this one: a socket that stopped taking audio
				// has not said why.
				s.fail("the model stopped accepting audio", err, "")
				return
			}
		}
	}
}

// playAudio converts one chunk of model speech and queues it on the leg.
func (s *Session) playAudio(audio []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isCutOff {
		s.cutOffChunks++
		return
	}
	if !s.isBotSpeaking {
		s.isBotSpeaking = true
	}
	s.timer.onFirstAudio()
	s.playBuffer = s.downlink.Convert(s.playBuffer, audio)
	s.framer.push(s.playBuffer, s.queueFrame)
}

// queueFrame hands one frame to the leg and counts it as heard. The caller
// holds mu.
//
// A frame the leg refuses means the model is producing faster than real time
// by more than the queue can hold — several seconds of audio. That is a
// runaway response, and dropping the overflow beats growing without bound.
//
// This is also where the caller starts hearing the bot. The barge-in guard is
// measured from the first frame queued on an idle leg, not from each turn's
// first chunk: a turn queued behind another's tail is not the start of
// anything the caller notices, and a turn whose audio sits in the queue has
// not started to be heard until it reaches the front. A frame on an empty
// queue is a new start only if the audio queued before it has finished
// playing, because a model streaming in real time finds the queue empty before
// almost every frame without the caller ever hearing silence. The clock is read
// only on an empty queue, which keeps the hot path free of it.
func (s *Session) queueFrame(frame []byte) {
	isIdle := s.leg.Pending() == 0
	if !s.leg.Send(frame) {
		s.log.Warn("dropped model audio: the send queue is full")
		return
	}
	if isIdle {
		now := time.Now()
		if s.audibleSince.IsZero() || now.After(s.audibleUntil.Add(audibleGapSlack)) {
			s.audibleSince = now
			s.audibleUntil = now
		}
	}
	if !s.audibleSince.IsZero() {
		s.audibleUntil = s.audibleUntil.Add(frameDurationMs * time.Millisecond)
	}
	s.framesQueued++
	if totalMs, providerMs, ok := s.timer.onFirstFrame(); ok {
		recordTurnLatency(s.log, s.providerName, totalMs, providerMs)
	}
}

//
// Model events.
//

func (s *Session) pumpModelEvents() {
	defer s.wg.Done()
	// The model going away ends the call: the other pumps have no reason to
	// keep running, and the caller should not be left holding a live line with
	// nothing on the other end.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Close(ctx)
	}()

	for {
		select {
		case <-s.done:
			return
		case event, ok := <-s.model.Events():
			if !ok {
				return
			}
			s.handleModelEvent(event)
		}
	}
}

func (s *Session) handleModelEvent(event provider.Event) {
	switch event.Type {
	case provider.EventTypeAudioDelta:
		s.playAudio(event.Audio)

	case provider.EventTypeSpeechStarted:
		// The caller is talking, so there is no dead air to report whether or
		// not this turns out to be a real interruption.
		s.cancelDeadAirWatch()
		s.mu.Lock()
		s.timer.onSpeechStarted()
		s.mu.Unlock()
		s.bargeIn(provider.InterruptReasonSpeech)

	case provider.EventTypeSpeechStopped:
		s.mu.Lock()
		s.timer.onSpeechStopped()
		s.mu.Unlock()

	case provider.EventTypeInterrupted:
		// The provider confirming a turn was cut short. It may have decided
		// that on its own, so this doubles as a backstop: whatever the reason,
		// the caller must not keep hearing the abandoned answer.
		//
		//
		// A provider that cancels a response and starts another can report the
		// cancellation after the new turn has begun.
		isCurrent := s.endInterruptedTurn()
		//
		// Cancelling is not trimming. A provider that stops on its own knows
		// it stopped, and still has no idea how much of what it produced ever
		// reached the caller — only this side counts frames. When we have
		// already flushed, this is a no-op: nothing was queued, so nothing
		// was heard, so there is nothing to say.
		//
		// "Nothing was queued" is not "nothing was heard": a turn whose audio
		// sat behind an earlier turn's tail and was flushed unplayed has
		// played 0 ms and must be trimmed to 0, or the model's history keeps
		// an answer nobody received. Only a turn with no queued audio at all
		// (already flushed by a barge-in) stays silent.
		if playedMs, hadQueued := s.stopPlayback(isCurrent); hadQueued {
			// Said out loud for the same reason the caller-initiated case is:
			// an interruption that leaves no trace cannot be told apart
			// afterwards from one that never happened. This path reports the
			// provider's own decision, so it names who decided.
			s.log.Info("the provider took the floor back",
				"reason", string(event.InterruptedBy), "playedMs", playedMs)
			s.tellTheModelWhatWasHeard(event.InterruptedBy, playedMs)
		}
		s.emit(Event{Type: EventTypeTurnDone, Status: event.Status,
			Usage: event.Usage, Turn: s.currentTurn(), IsInterrupted: true})

	case provider.EventTypeResponseStarted:
		s.beginTurn()

	case provider.EventTypeResponseDone:
		s.endTurn()
		s.emit(Event{Type: EventTypeTurnDone, Status: event.Status,
			Usage: event.Usage, Turn: s.currentTurn()})

	case provider.EventTypeInputTranscript:
		s.emit(Event{Type: EventTypeCustomerSaid, Text: event.Text, IsFinal: event.IsFinal})

	case provider.EventTypeOutputTranscript:
		// The turn rides along so the line can be judged with the tool calls
		// of its own response (claimWatch).
		s.emit(Event{Type: EventTypeBotSaid, Text: event.Text, IsFinal: event.IsFinal,
			Turn: s.currentTurn()})

	case provider.EventTypeToolCall:
		s.emit(Event{
			Type: EventTypeToolCall, ToolCallID: event.ToolCallID,
			ToolName: event.ToolName, ToolArgs: event.ToolArgs,
			Turn: s.currentTurn(),
		})

	case provider.EventTypeError:
		if event.IsFatal {
			obs.RecordProviderError(s.providerName)
			if event.FailureCause == provider.FailureCauseSessionExpired {
				// Counted as well as, never instead of: this is still a session
				// that ended on an error, and an operator watching that total
				// should not have to know which engines cap a session to read
				// it. The second counter is what separates a limit reached from
				// a fault to fix.
				obs.RecordProviderSessionExpired(s.providerName)
			}
			s.fail(event.Text, event.Err, event.FailureCause)
			return
		}
		s.log.Warn("model reported a recoverable error", "error", event.Err)

	case provider.EventTypeClosed:
		s.log.Info("model session closed")
	}
}

//
// Barge-in.
//

// bargeIn hands the floor back to the caller.
//
// The local queue is flushed first and the provider told second: flushing is
// what the caller actually notices, and it is the only part that cannot be
// done by anyone else. About two frames are already in flight, so the caller
// hears silence within roughly forty milliseconds.
func (s *Session) bargeIn(reason provider.InterruptReason) {
	s.mu.Lock()
	isSpeaking := s.isBotSpeaking
	isGenerating := s.isResponding
	audibleFor := time.Since(s.audibleSince)
	// Generation ending is not the caller's experience ending: the tail of
	// the utterance is still queued and playing after the model has finished
	// producing it. Speech over that tail is as much an interruption as
	// speech over the generation — the boundary is the last frame heard, not
	// the last frame made.
	isAudioInFlight := s.framesQueued > 0 && s.leg.Pending() > 0
	s.mu.Unlock()

	// A keypress also takes the floor from a turn that is still being made and
	// has not spoken yet: one that is only calling a tool, or one waiting on a
	// tool result's answer. Left running, that turn answers what came before
	// the key, and the keypress's own request for a reply is refused as a
	// second response. Speech keeps the narrower gate: the provider's server
	// detection already cancels a turn that has not spoken, and acting on a
	// detection with nothing audible to echo would only duplicate it.
	isKeyOverGeneration := reason == provider.InterruptReasonDTMF && isGenerating
	if !isSpeaking && !isAudioInFlight && !isKeyOverGeneration {
		return
	}

	// A keypress is unambiguous and always takes the floor. Detected speech is
	// not: for the first moments of a turn, what the detector hears is very
	// often the bot's own voice returning down the line, and acting on it makes
	// the bot interrupt itself mid-sentence.
	if reason == provider.InterruptReasonSpeech {
		if guard := s.bargeGuard(); guard > 0 && audibleFor < guard {
			// Info, not Debug: a swallowed interruption is invisible from the
			// outside, and the call log has to be able to tell it from speech
			// that was never detected.
			s.log.Info("ignored speech detected inside the barge-in guard",
				"speakingForMs", audibleFor.Milliseconds(),
				"guardMs", guard.Milliseconds())
			return
		}
	}

	playedMs, hadQueued := s.stopPlayback(true)
	if isGenerating {
		// A response is still open on the provider's side and will go on
		// sending audio until the cancel takes effect. With none open (speech
		// over a finished turn's tail) nothing more is coming, and fencing
		// would only wait for a turn that has nothing to clear it but the next
		// legitimate one.
		s.mu.Lock()
		s.isCutOff = true
		s.mu.Unlock()
	}
	if !hadQueued {
		// Nothing of this turn ever reached the leg, so there is no audio the
		// caller could have heard and nothing in the model's history to trim.
		// Negative is "not known": the provider cancels the response and
		// truncates nothing, rather than aiming a truncate at an item with no
		// audio in it.
		playedMs = -1
	}
	// The provider is told whenever the caller stopped hearing something, not
	// only while it was still producing. Its history is a record of what was
	// said to the caller, and an utterance the caller never heard has to come
	// out of it either way — otherwise the next turn is built on the model
	// believing it said something nobody received.
	//
	// This used to be gated on generation still being in progress, to avoid
	// cancelling a response that had already ended. That protection now lives
	// where the truth is (Realtime.Interrupt knows whether a response is
	// open), because the two are not the same question: speech over the tail
	// of a finished turn still needs the history trimmed.
	s.tellTheModelWhatWasHeard(reason, playedMs)
	// Said out loud here and nowhere else. The line above this one used to be
	// the only place an interruption was mentioned, and it was the *ignored*
	// case — a Debug line for speech that turned out to be echo — so the
	// interruptions that happened left no trace at all and the ones that did
	// not left one.
	s.log.Info("the caller took the floor back",
		"reason", string(reason), "playedMs", playedMs, "wasGenerating", isGenerating)
	obs.RecordBotInterruption(string(reason))
	s.emit(Event{Type: EventTypeBargeIn, Text: string(reason)})
}

// tellTheModelWhatWasHeard reports the interruption and how much of the turn
// reached the caller, so the provider's history matches the conversation that
// actually happened.
func (s *Session) tellTheModelWhatWasHeard(reason provider.InterruptReason, playedMs int) {
	if err := s.model.Interrupt(reason, playedMs); err != nil {
		s.log.Warn("could not tell the model it was interrupted", "error", err)
	}
}

// stopPlayback drops queued speech and reports how much the caller heard, and
// whether the turn had queued any audio at all. Zero milliseconds with
// hadQueued set means the whole turn was flushed unheard (it was queued behind
// an earlier turn's tail); zero with it clear means there was nothing to hear.
//
// endsTheTurn says the flush is for the turn in progress. A stale interruption
// (see openTurns) is about an older turn whose audio was flushed when it was
// cancelled; it leaves the current turn's isResponding alone, because a turn
// that is still being generated is not over. The event carries no turn
// identity, so staleness is worked out from the order turns start and end.
func (s *Session) stopPlayback(endsTheTurn bool) (playedMs int, hadQueued bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cleared := s.leg.ClearTx()
	s.framer.reset()

	// What was queued, less what never made it out of the queue. Anything
	// already handed to the wire counts as heard.
	played := max(s.framesQueued-cleared, 0)
	hadQueued = s.framesQueued > 0
	s.framesQueued = 0
	s.isBotSpeaking = false
	s.audibleSince = time.Time{}
	s.audibleUntil = time.Time{}
	if endsTheTurn {
		s.isResponding = false
	}
	// Nothing is left to drain, and no dead-air timer should run: the caller is
	// already talking.
	s.drainGeneration++
	s.idleGeneration++

	return played * frameDurationMs, hadQueued
}

// endInterruptedTurn accounts for an interruption event and reports whether it
// ended the turn in progress rather than an older, already-replaced one.
func (s *Session) endInterruptedTurn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openTurns = max(s.openTurns-1, 0)
	return s.openTurns == 0
}

func (s *Session) beginTurn() {
	s.mu.Lock()
	s.framesQueued = 0
	s.turnSeq++
	s.isResponding = true
	s.turnOwedUntil = time.Time{}
	s.openTurns = min(s.openTurns+1, 2)
	if s.isCutOff {
		if s.cutOffChunks > 0 {
			s.log.Debug("dropped the audio of a response that was cut off",
				"chunks", s.cutOffChunks)
		}
		s.isCutOff, s.cutOffChunks = false, 0
	}
	// Any pending drain or dead-air watch belongs to the previous turn.
	s.drainGeneration++
	s.idleGeneration++
	s.mu.Unlock()
}

// isHoldingTheFloor reports whether the bot has the floor: a turn is being
// generated, or its tail is still playing to the caller. A turn asked for now
// would collide with it — refused on the Realtime providers, cut off
// mid-sentence on gemini.
func (s *Session) isHoldingTheFloor() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isResponding || s.isBotSpeaking ||
		(!s.turnOwedUntil.IsZero() && time.Now().Before(s.turnOwedUntil)) ||
		(s.framesQueued > 0 && s.leg.Pending() > 0)
}

// queuedPlayout is how long the audio queued for the caller takes to play.
func (s *Session) queuedPlayout() time.Duration {
	return time.Duration(s.leg.Pending()) * frameDurationMs * time.Millisecond
}

// currentTurn reports which model turn is in progress.
func (s *Session) currentTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnSeq
}

// endTurn closes out generation and hands the turn to the playback watcher.
func (s *Session) endTurn() {
	s.mu.Lock()
	// The tail of the last sentence is worth padding out rather than losing.
	s.framer.flush(s.queueFrame)
	s.isBotSpeaking = false
	s.isResponding = false
	s.openTurns = max(s.openTurns-1, 0)
	s.drainGeneration++
	marker := playbackMarker{generation: s.drainGeneration, turn: s.turnSeq}
	s.mu.Unlock()

	select {
	case s.playbackDone <- marker:
	default:
		s.log.Warn("playback watcher is behind; a turn boundary was not tracked")
	}
}

// playbackMarker identifies one turn's handoff to the playback watcher.
type playbackMarker struct {
	generation uint64
	turn       int
}

//
// Playback and dead air.
//

// watchPlayback follows a turn from the end of generation to the end of
// hearing, and then watches for a caller who says nothing at all.
//
// The two are separate events because the model finishing a sentence and the
// caller having heard it are separated by everything still in the send queue.
// Anything that must not cut the bot off mid-word — a transfer, a goodbye —
// has to wait for the second, not the first.
func (s *Session) watchPlayback() {
	defer s.wg.Done()

	var next playbackMarker
	var hasNext bool

	for {
		marker := next
		if hasNext {
			hasNext = false
		} else {
			select {
			case <-s.done:
				return
			case marker = <-s.playbackDone:
			}
		}

		idleGeneration, drained := s.awaitDrained(marker.generation)
		if !drained {
			continue
		}
		s.emit(Event{Type: EventTypePlaybackDone, Turn: marker.turn})
		if s.afterPlaybackDone != nil {
			s.afterPlaybackDone()
		}
		next, hasNext = s.awaitCallerOrDeadAir(idleGeneration)
	}
}

// awaitDrained waits for the send queue to empty, reporting false if the turn
// was superseded — interrupted, or followed by another — while it waited.
//
// It also returns the idle generation the turn's dead-air watch carries,
// read under the same lock that confirms the drain. Speech from that instant
// on cancels the watch: read any later, after PLAYBACK_DONE is published, a
// caller who spoke in between had already moved the counter, the watch took
// the moved value, and dead air was reported over them.
func (s *Session) awaitDrained(generation uint64) (uint64, bool) {
	ticker := time.NewTicker(frameDurationMs * time.Millisecond)
	defer ticker.Stop()

	for {
		if !s.isCurrentDrain(generation) {
			return 0, false
		}
		if s.leg.Pending() == 0 {
			// One more frame interval so the last frame is actually on the
			// wire, not merely off the queue.
			select {
			case <-s.done:
				return 0, false
			case <-ticker.C:
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.idleGeneration, s.drainGeneration == generation
		}
		select {
		case <-s.done:
			return 0, false
		case <-ticker.C:
		}
	}
}

// awaitCallerOrDeadAir reports dead air if the caller stays silent. It returns
// the next turn's marker, if one arrives first, for the caller to handle.
//
// The wait has to watch for that marker as well as for the clock: the bot
// speaking again says there was no dead air to report, and the new turn's
// playback must be followed from the moment it ends, not from whenever a
// timeout nobody is waiting for any more happens to expire. Leaving the marker
// in the channel put seconds of silence in front of every armed transfer.
func (s *Session) awaitCallerOrDeadAir(generation uint64) (playbackMarker, bool) {
	timeout := s.noInputAfter()
	if timeout <= 0 {
		return playbackMarker{}, false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-s.done:
	case marker := <-s.playbackDone:
		return marker, true
	case <-timer.C:
		// A stale timer recognises itself rather than being stopped, which
		// removes the race between cancelling and firing entirely.
		if !s.isCurrentIdle(generation) {
			return playbackMarker{}, false
		}
		s.log.Info("dead air", "afterMs", timeout.Milliseconds())
		s.emit(Event{Type: EventTypeNoInput})
	}
	return playbackMarker{}, false
}

// cancelDeadAirWatch invalidates any timer waiting on the caller. It touches
// only the idle counter: speech says the caller is there, not that a turn's
// audio stopped being on its way to them.
func (s *Session) cancelDeadAirWatch() {
	s.mu.Lock()
	s.idleGeneration++
	s.mu.Unlock()
}

func (s *Session) isCurrentDrain(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drainGeneration == generation
}

func (s *Session) isCurrentIdle(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idleGeneration == generation
}

//
// Keypresses.
//

// pumpDigits turns keypresses into conversation.
//
// A keypress takes the floor immediately — someone pressing a key while the
// bot talks has decided they are done listening — and the digit is put to the
// model as something the caller did, because otherwise it has no way to know
// it happened.
//
// Once the call's ending is armed the closing line is all that is left to say,
// and a keypress cannot change what happens next. It is still recorded and
// reported (the DIGIT event), but it neither cuts the line off nor asks the
// model for a turn: every key interrupted the line and got its own reply, so a
// caller pressing 0 five times heard the goodbye in five fragments and the
// transfer waited for the last full one.
func (s *Session) pumpDigits() {
	defer s.wg.Done()

	digits := s.leg.Digits()
	for {
		select {
		case <-s.done:
			return
		case digit, ok := <-digits:
			if !ok {
				return
			}
			if s.cfg.IsEndingArmed != nil && s.cfg.IsEndingArmed() {
				s.log.Info("a keypress after the ending was armed is only recorded", "digit", digit)
				s.emit(Event{Type: EventTypeDigit, Text: digit})
				continue
			}
			s.bargeIn(provider.InterruptReasonDTMF)
			s.emit(Event{Type: EventTypeDigit, Text: digit})

			if err := s.SendCue(keypressText(digit)); err != nil {
				s.log.Warn("could not report a keypress to the model",
					"digit", digit, "error", err)
			}
		}
	}
}

// keypressText is how a keypress is described to the model. It reads as a
// stage direction rather than as speech, so the model does not answer as if
// the caller had said the number out loud.
func keypressText(digit string) string {
	return fmt.Sprintf("(The caller pressed %s on their keypad.)", digit)
}

//
// Lifecycle.
//

// watchLeg ends the model session when the caller hangs up.
func (s *Session) watchLeg() {
	defer s.wg.Done()

	select {
	case <-s.leg.Stopped():
		s.log.Info("caller leg ended, closing the model session")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Close(ctx)
	case <-s.done:
	}
}

// fail ends the call because the conversation cannot go on.
//
// The cause travels with it when the provider named one: what releases the call
// is the same rescue either way, but "the provider's session ran out of time" is
// a different thing to find in a CDR from "the provider failed".
func (s *Session) fail(reason string, err error, cause provider.FailureCause) {
	s.log.Error("ai call failed", "reason", reason, "error", err, "cause", cause)
	s.emit(Event{Type: EventTypeFailed, Text: reason, Err: err, FailureCause: cause})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.Close(ctx)
}

// emit publishes an event, dropping it once the call is over.
func (s *Session) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.done:
	}
}
