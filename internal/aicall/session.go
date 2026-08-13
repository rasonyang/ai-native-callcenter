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
	// EventTypeCallerSaid and EventTypeBotSaid are transcript events.
	EventTypeCallerSaid EventType = "CALLER_SAID"
	EventTypeBotSaid    EventType = "BOT_SAID"
	// EventTypeToolCall is the model asking for something to be done.
	EventTypeToolCall EventType = "TOOL_CALL"
	// EventTypeTurnDone closes a turn, whether it completed or was cut short.
	EventTypeTurnDone EventType = "TURN_DONE"
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

	// Text is transcript text, a digit, or a failure message.
	Text    string
	IsFinal bool

	ToolCallID string
	ToolName   string
	ToolArgs   string

	Status string
	Usage  provider.Usage

	Err error
}

// Config parameterises one bridged call.
type Config struct {
	// Session is the model configuration. Its audio formats are filled in from
	// the negotiated law and the provider profile.
	Session provider.SessionConfig
	Logger  *slog.Logger
}

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
		leg:        leg,
		model:      model,
		log:        log.With("callId", leg.ID()),
		cfg:        cfg,
		uplink:     uplink,
		downlink:   downlink,
		framer:     newFramer(law),
		playBuffer: make([]byte, 0, 8*media.FrameSamples),
		events:     make(chan Event, 128),
		done:       make(chan struct{}),
	}, nil
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

	s.wg.Add(4)
	go s.pumpCallerAudio()
	go s.pumpModelEvents()
	go s.pumpDigits()
	go s.watchLeg()

	// Every publisher is one of those four. Once they have all returned,
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
		_ = s.model.Close(ctx)
		s.leg.Stop()
	})
}

// Wait blocks until every pump has stopped.
func (s *Session) Wait() { s.wg.Wait() }

//
// Conversation control, for the flow engine.
//

// AnswerTool replies to a tool call and steers the next turn.
func (s *Session) AnswerTool(toolCallID, output, hint string) error {
	return s.model.SendToolResult(toolCallID, output, hint)
}

// Reinstruct replaces the standing instructions, which is how a flow moves the
// conversation between phases.
func (s *Session) Reinstruct(text string) error {
	return s.model.UpdateInstructions(text)
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
				s.fail("the model stopped accepting audio", err)
				return
			}
		}
	}
}

// playAudio converts one chunk of model speech and queues it on the leg.
func (s *Session) playAudio(audio []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.isBotSpeaking = true
	s.playBuffer = s.downlink.Convert(s.playBuffer, audio)
	s.framer.push(s.playBuffer, s.queueFrame)
}

// queueFrame hands one frame to the leg and counts it as heard. The caller
// holds mu.
//
// A frame the leg refuses means the model is producing faster than real time
// by more than the queue can hold — several seconds of audio. That is a
// runaway response, and dropping the overflow beats growing without bound.
func (s *Session) queueFrame(frame []byte) {
	if !s.leg.Send(frame) {
		s.log.Warn("dropped model audio: the send queue is full")
		return
	}
	s.framesQueued++
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
		s.bargeIn(provider.InterruptReasonSpeech)

	case provider.EventTypeInterrupted:
		// The provider confirming a turn was cut short. It may have decided
		// that on its own, so this doubles as a backstop: whatever the reason,
		// the caller must not keep hearing the abandoned answer.
		s.stopPlayback()
		s.emit(Event{Type: EventTypeTurnDone, Status: event.Status, Usage: event.Usage})

	case provider.EventTypeResponseStarted:
		s.beginTurn()

	case provider.EventTypeResponseDone:
		s.endTurn()
		s.emit(Event{Type: EventTypeTurnDone, Status: event.Status, Usage: event.Usage})

	case provider.EventTypeInputTranscript:
		s.emit(Event{Type: EventTypeCallerSaid, Text: event.Text, IsFinal: event.IsFinal})

	case provider.EventTypeOutputTranscript:
		s.emit(Event{Type: EventTypeBotSaid, Text: event.Text, IsFinal: event.IsFinal})

	case provider.EventTypeToolCall:
		s.emit(Event{
			Type: EventTypeToolCall, ToolCallID: event.ToolCallID,
			ToolName: event.ToolName, ToolArgs: event.ToolArgs,
		})

	case provider.EventTypeError:
		if event.IsFatal {
			s.fail(event.Text, event.Err)
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
	s.mu.Unlock()
	if !isSpeaking {
		return
	}

	playedMs := s.stopPlayback()
	if err := s.model.Interrupt(reason, playedMs); err != nil {
		s.log.Warn("could not tell the model it was interrupted", "error", err)
	}
	s.emit(Event{Type: EventTypeBargeIn, Text: string(reason)})
}

// stopPlayback drops queued speech and reports how much the caller heard.
func (s *Session) stopPlayback() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cleared := s.leg.ClearTx()
	s.framer.reset()

	// What was queued, less what never made it out of the queue. Anything
	// already handed to the wire counts as heard.
	played := max(s.framesQueued-cleared, 0)
	s.framesQueued = 0
	s.isBotSpeaking = false

	return played * frameDurationMs
}

func (s *Session) beginTurn() {
	s.mu.Lock()
	s.framesQueued = 0
	s.mu.Unlock()
}

func (s *Session) endTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The tail of the last sentence is worth padding out rather than losing.
	s.framer.flush(s.queueFrame)
	s.isBotSpeaking = false
}

//
// Keypresses.
//

// pumpDigits turns keypresses into conversation.
//
// A keypress always takes the floor immediately — someone pressing a key while
// the bot talks has decided they are done listening — and the digit is put to
// the model as something the caller did, because otherwise it has no way to
// know it happened.
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
			s.bargeIn(provider.InterruptReasonDTMF)
			s.emit(Event{Type: EventTypeDigit, Text: digit})

			if err := s.model.SendUserText(keypressText(digit)); err != nil {
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
func (s *Session) fail(reason string, err error) {
	s.log.Error("ai call failed", "reason", reason, "error", err)
	s.emit(Event{Type: EventTypeFailed, Text: reason, Err: err})

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
