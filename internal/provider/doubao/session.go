// SPDX-License-Identifier: Apache-2.0

// Package doubao is a second speech-to-speech client, for an engine whose wire
// protocol is not the Realtime one.
//
// The rule this repository has held to — a new engine is a new profile, never a
// second client — is about dialects of one protocol. This is a different
// protocol, and the difference is lifecycle rather than vocabulary: the session
// is bootstrapped with session.create and confirmed before anything may be
// sent; there is no way to ask for a turn, because the engine answers audio and
// nothing else; no event announces the caller starting or stopping speaking,
// only the transcript of it; a tool result is an item with role "tool" rather
// than a function-call output; the session ends with a handshake; and the
// uplink is a clock and a keepalive at once — too fast or too slow is an error,
// and a silence nobody declared stops the engine answering at all. None of that
// is expressible as values in a Profile.
//
// What it is NOT is a second seam. provider.VoiceSession is the only interface
// above this package, and every event it produces is one the existing client
// already produces; nothing here is cascaded, and no recognition, synthesis or
// language-model concept appears anywhere in it.
//
// Goroutines, and what ends them. There are four, and nothing else:
//
//	goroutine    | owner        | started by         | stops on                 | in-flight work on stop
//	-------------|--------------|--------------------|--------------------------|------------------------
//	read loop    | Session      | Start, after dial  | socket read error/close  | sole closer of Events(); emits CLOSED last
//	pacer        | Session      | Start, once up     | stopping closed          | queued frames discarded, never flushed
//	watchdog     | Session      | Start, after dial  | conn.Done()              | disarms; emits nothing after Done
//	keepalive    | wsconn.Conn  | wsconn.Dial        | conn.Done() / write fail | none
package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

const (
	// eventBuffer is how far the consumer may fall behind. The consumer is a
	// per-call actor forwarding to a paced send queue, so it should never come
	// close.
	eventBuffer = 256
	// firstAudioTimeout bounds the wait between a turn being announced and the
	// first of it arriving; deltaStallTimeout bounds a gap in the middle of one.
	firstAudioTimeout = 3 * time.Second
	deltaStallTimeout = 2 * time.Second
	// closeTimeout bounds the close handshake. Every caller passes a context
	// with no deadline in it, so the bound has to be this client's own.
	closeTimeout = 2 * time.Second
	// ttsTypeSpokenLine marks a turn that is speaking words this client handed
	// the engine rather than words the model wrote.
	ttsTypeSpokenLine = "chat_tts_text"
)

// How a session ended, for the one line logged when it does. A call is
// released on the strength of which of these it was, so each is a distinct
// word rather than a flag.
const (
	outcomeCleanClose     = "CLOSED_CLEAN"
	outcomeCloseTimeout   = "CLOSED_TIMEOUT"
	outcomeClosedByServer = "CLOSED_BY_SERVER"
	outcomeConnectionLost = "CONN_LOST"
)

// failedOutcome names the provider's own code, because which code it was
// decides nothing here but explains everything afterwards.
func failedOutcome(code string) string { return "FAILED(" + code + ")" }

// ErrTextCueUnsupported is what this engine does with a line of text that is
// not meant to be spoken: nothing at all.
//
// Measured, twice: a lone item with role "user" is accepted without an
// acknowledgement and dropped, with or without a buffer commit. There is no
// frame on this protocol that makes the model take a turn from text, so a
// keypress cannot be put into the conversation the way it can elsewhere.
// Saying so is better than sending a frame that changes nothing and returning
// success.
var ErrTextCueUnsupported = errors.New(
	"doubao: this engine takes no text cue; a turn cannot be prompted with text")

// Session is one conversation with the engine.
type Session struct {
	profile provider.Profile
	apiKey  string
	log     *slog.Logger

	events chan provider.Event
	conn   *wsconn.Conn

	// ready closes when the provider has created the session, which is the one
	// thing that must happen before anything else may be sent.
	ready     chan struct{}
	readyOnce sync.Once

	// stopping closes when the session has begun ending, whoever decided it.
	// Every send is gated on it, so nothing can be written after the provider
	// has been told the session is over.
	stopping  chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
	// closed closes when the provider confirms the session has ended.
	closed     chan struct{}
	closedOnce sync.Once
	// readDone closes when the read loop has ended. It is closed BEFORE the
	// event channel is, so a Close called from the goroutine draining events
	// never waits on itself.
	readDone chan struct{}

	// sendMu orders the transition to stopping against every write: a frame is
	// only written by a holder that has just seen the session still running.
	sendMu   sync.Mutex
	eventSeq atomic.Uint64

	// isTurnOpen is true between a turn being announced and it ending, however
	// it ended. The watchdog consults it, and so does everything that has to
	// know whether there is anything to interrupt.
	isTurnOpen atomic.Bool

	mu           sync.Mutex
	cfg          provider.SessionConfig
	instructions string
	voice        string
	tools        []toolBody
	startError   error
	logid        string
	terminal     string
	// isFenced drops audio from a turn that was cancelled: deltas go on
	// arriving for up to half a second after the cancel was acknowledged, and
	// the caller has already stopped hearing that turn.
	isFenced bool
	// isCancelledByUs remembers who stopped the turn now ending, which is what
	// keeps an interruption this client made from being logged as the caller's.
	isCancelledByUs bool
	// turnAudioBytes is what the turn has actually carried, for the guard on a
	// turn that carried none.
	turnAudioBytes int
	// pendingSpeak is the line this client handed the engine and is waiting to
	// hear it start. There is at most one: a second commit kills the first.
	pendingSpeak string
	// The set of calls the model is waiting on. It goes on only once every one
	// of them has an answer, so they are held here until the set is complete.
	toolOrder   []string
	toolPending map[string]bool
	toolResults map[string]string

	dog *provider.Watchdog
	// Deadlines held as fields so tests need not wait seconds for behaviour
	// that is measured in seconds on a real call.
	firstAudioDeadline time.Duration
	deltaStallDeadline time.Duration
	closeWait          time.Duration

	pacer *pacer
	// ticks is the pacer's cadence. Production fills it with a ticker in Start;
	// a test sets it beforehand and drives the pacer a tick at a time.
	ticks      <-chan time.Time
	paced      chan struct{}
	stopTicker func()
}

// New builds a session for a profile. The credential is read from the
// environment named by the profile, so a missing key fails here rather than
// mid-call.
func New(profile provider.Profile, log *slog.Logger) (provider.VoiceSession, error) {
	session, err := newSession(profile, log)
	if err != nil {
		return nil, err
	}
	return session, nil
}

// newSession is New without the interface, for this package and its tests.
func newSession(profile provider.Profile, log *slog.Logger) (*Session, error) {
	apiKey := os.Getenv(profile.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("provider %s: %s is not set", profile.Name, profile.APIKeyEnv)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Session{
		profile: profile,
		apiKey:  apiKey,
		log:     log.With("provider", profile.Name, "model", model),

		events:   make(chan provider.Event, eventBuffer),
		ready:    make(chan struct{}),
		stopping: make(chan struct{}),
		closed:   make(chan struct{}),
		readDone: make(chan struct{}),

		firstAudioDeadline: firstAudioTimeout,
		deltaStallDeadline: deltaStallTimeout,
		closeWait:          closeTimeout,
	}, nil
}

// Session satisfies the one interface above this package.
var _ provider.VoiceSession = (*Session)(nil)

func (s *Session) Events() <-chan provider.Event { return s.events }

// Start connects, creates the session and waits for the provider to confirm
// it, then says the opening line if the flow gave one.
//
// There is no opening turn to ask for: this engine speaks when it is given
// words or when it hears the caller, and a call that names no opening line
// simply waits for whoever rang to say something.
func (s *Session) Start(ctx context.Context, cfg provider.SessionConfig) error {
	voice := cfg.Voice
	if voice == "" {
		voice = s.profile.Voice
	}
	s.mu.Lock()
	s.cfg = cfg
	s.instructions = cfg.Instructions
	s.voice = voice
	s.tools = buildTools(cfg.Tools)
	s.mu.Unlock()

	headers := http.Header{}
	headers.Set("X-Api-Key", s.apiKey)
	for key, value := range s.profile.Headers {
		headers.Set(key, value)
	}

	conn, err := wsconn.Dial(ctx, s.profile.Endpoint, headers, s.log)
	if err != nil {
		return err
	}
	s.conn = conn
	// The session's identity is in the upgrade response, not in a frame. It is
	// what a support ticket is answered from, so it is logged once and carried
	// by every line this session writes afterwards.
	if logid := conn.Header().Get("X-Tt-Logid"); logid != "" {
		s.mu.Lock()
		s.logid = logid
		s.mu.Unlock()
		s.log = s.log.With("logid", logid)
	}
	s.log.Info("provider session opening", "endpoint", s.profile.Endpoint)

	s.dog = provider.NewWatchdog(provider.WatchdogConfig{
		FirstAudioDeadline: s.firstAudioDeadline,
		DeltaStallDeadline: s.deltaStallDeadline,
		Done:               conn.Done(),
		IsResponseOpen:     s.isTurnOpen.Load,
		OnStall:            s.onTurnStalled,
	})
	go s.readLoop()
	go s.dog.Run()

	if err := s.sendEvent(s.buildSession("session.create", cfg.Instructions)); err != nil {
		s.conn.Close()
		return fmt.Errorf("create session: %w", err)
	}

	// Nothing may be sent until the session exists: a frame that arrives ahead
	// of session.created is answered with an error and the socket is dropped.
	select {
	case <-s.ready:
	case <-ctx.Done():
		s.conn.Close()
		return ctx.Err()
	case <-time.After(wsconn.DialTimeout):
		s.conn.Close()
		if err := s.failure(); err != nil {
			return fmt.Errorf("session refused: %w", err)
		}
		return errors.New("the provider did not create the session")
	}
	if err := s.failure(); err != nil {
		s.conn.Close()
		return fmt.Errorf("session refused: %w", err)
	}

	// The opening line goes out as a line to speak, the same frame a mid-call
	// one uses. Its words are reported when the engine starts saying them, from
	// the read loop: reporting them here would block Start behind a consumer
	// that has not started draining yet.
	if cfg.OpeningText != "" {
		s.mu.Lock()
		s.pendingSpeak = cfg.OpeningText
		s.mu.Unlock()
		if err := s.sendEvent(speakEvent{
			Type: "speech_text_buffer.commit", EventID: s.nextEventID(),
			Text: cfg.OpeningText,
		}); err != nil {
			s.conn.Close()
			return fmt.Errorf("open the call: %w", err)
		}
	}

	// A ticker rather than a sleeping loop: its period is absolute, and a tick
	// the pacer was late for is dropped instead of queued. That is exactly what
	// this provider needs — a client that fell behind and then caught up in a
	// burst is a pacing error to it, the same as one that fell silent.
	ticks := s.ticks
	if ticks == nil {
		ticker := time.NewTicker(provider.FrameInterval)
		ticks = ticker.C
		s.stopTicker = ticker.Stop
	}
	s.pacer = newPacer(s, ticks)
	go s.pacer.run()
	return nil
}

// SendAudio hands one frame of caller audio to the pacer and returns.
//
// It never blocks on the socket. What feeds it is the media path, and a stalled
// write there is audio lost in both directions; what the provider wants is one
// frame every 20 ms, which is the pacer's business rather than the caller's.
//
// The frame is copied because it is not ours: the call's converter writes the
// next one into the same buffer, and a queued frame would arrive as whatever
// came after it.
func (s *Session) SendAudio(audio []byte) error {
	if err := s.uplinkFailure(); err != nil {
		return err
	}
	select {
	case <-s.stopping:
		return wsconn.ErrSessionClosed
	default:
	}
	if s.pacer == nil {
		return wsconn.ErrSessionClosed
	}
	s.pacer.push(append([]byte(nil), audio...))
	return nil
}

// SendUserText refuses. See ErrTextCueUnsupported: there is no frame on this
// protocol that would do it, and both callers log and carry on.
func (s *Session) SendUserText(string) error { return ErrTextCueUnsupported }

// SpeakText says a line the flow chose.
//
// This engine speaks text outright — there is no "repeat after me" about it —
// which also means the words are this client's to report: the provider sends no
// transcript for a line it was handed. The read loop announces them when the
// turn carrying them starts.
//
// It PRE-EMPTS whatever is being said, measured, and it does NOT queue: a
// second commit before the first has been spoken kills the first outright, so
// there is at most one line waiting and the newer one is it. Nothing is emitted
// here — this is normally called from the goroutine draining Events, and
// emitting would deadlock it.
func (s *Session) SpeakText(text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	s.pendingSpeak = text
	s.mu.Unlock()

	return s.sendEvent(speakEvent{
		Type: "speech_text_buffer.commit", EventID: s.nextEventID(), Text: text})
}

// SendToolResult answers one function call, and sends the set once it is
// complete.
//
// The model goes on only when every call it made has an answer, and this
// protocol carries them in one message rather than one each. The engine is
// answered in the order it asked, whatever order the results arrive in.
func (s *Session) SendToolResult(toolCallID, output, hint string) error {
	s.mu.Lock()
	if !s.toolPending[toolCallID] {
		_, isAnswered := s.toolResults[toolCallID]
		s.mu.Unlock()
		if isAnswered {
			return fmt.Errorf("doubao: tool call %q has already been answered", toolCallID)
		}
		return fmt.Errorf("doubao: no tool call %q is waiting for a result", toolCallID)
	}
	delete(s.toolPending, toolCallID)
	s.toolResults[toolCallID] = provider.MergeHint(output, hint)
	if len(s.toolPending) > 0 {
		s.mu.Unlock()
		return nil
	}

	items := make([]toolResultItem, 0, len(s.toolOrder))
	for _, callID := range s.toolOrder {
		items = append(items, toolResultItem{
			CallID:  callID,
			Role:    "tool",
			Content: []itemContent{{Type: "input_text", Text: s.toolResults[callID]}},
		})
	}
	s.toolOrder = nil
	s.toolResults = nil
	s.mu.Unlock()

	return s.sendEvent(toolResultEvent{
		Type: "conversation.item.create", EventID: s.nextEventID(), Items: items})
}

// UpdateInstructions replaces the standing instructions.
//
// The whole session body goes with them. Tools are a full overwrite on this
// API, so an update carrying only the instructions would silently take every
// tool the flow has with it.
func (s *Session) UpdateInstructions(text string) error {
	s.mu.Lock()
	s.instructions = text
	s.mu.Unlock()
	return s.sendEvent(s.buildSession("session.update", text))
}

// Interrupt stops the model talking over the caller.
//
// The caller's own speech needs nothing sent: this engine hears them and stops
// on its own, and the turn ends with a bare response.done. A keypress or an
// application decision has to be said out loud, and only while there is
// something to stop — but the cancel does not take effect at once. Audio from
// the cancelled turn goes on arriving for up to half a second afterwards
// (measured), so the same decision also fences the rest of it off.
//
// playedMs is logged and nothing more: this protocol has no truncation event,
// so how much the caller heard cannot be put into the engine's own history the
// way it can elsewhere. What it is for — flushing what the caller was about to
// hear — happens on the call's side either way.
//
// No event is emitted here, for the reason SpeakText gives.
func (s *Session) Interrupt(reason provider.InterruptReason, playedMs int) error {
	if reason == provider.InterruptReasonSpeech {
		return nil
	}
	if !s.isTurnOpen.Load() {
		return nil
	}

	s.mu.Lock()
	if s.isFenced {
		// This turn has already been cancelled; asking twice is noise.
		s.mu.Unlock()
		return nil
	}
	s.isFenced = true
	s.isCancelledByUs = true
	s.mu.Unlock()

	s.log.Debug("stopping the turn", "reason", reason, "playedMs", playedMs)
	return s.sendEvent(simpleEvent{Type: "response.cancel", EventID: s.nextEventID()})
}

// Close ends the session, once, however many things decide it at the same
// moment.
//
// The order is the whole of it: stopping closes first, so the pacer exits and
// every other send starts refusing, and only then is the provider told. A frame
// written after session.close is answered with an error on a session that no
// longer exists.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.beginStop()
		if s.conn == nil {
			s.setOutcome(outcomeCleanClose)
			return
		}
		s.awaitPacer()

		if s.isSocketGone() {
			// There is no session left to close politely.
			s.setOutcome(outcomeConnectionLost)
		} else if err := s.sendSessionClose(); err != nil {
			s.log.Debug("could not ask the provider to end the session", "error", err)
		}
		s.awaitClosed(ctx)

		s.conn.Close()
		// The logid is already on this logger, put there when the socket was
		// opened: it is what a support ticket is answered from, so every line
		// of the session carries it rather than only this one.
		stats := s.Stats()
		s.log.Info("provider session finished", "outcome", s.outcome(),
			"framesSent", stats.FramesSent, "framesDropped", stats.FramesDropped,
			"mutes", stats.Mutes, "unmutes", stats.Unmutes)
	})
	return nil
}

// Stats is what the uplink did.
func (s *Session) Stats() Stats {
	if s.pacer == nil {
		return Stats{}
	}
	return s.pacer.statistics()
}

//
// Stopping.
//

// beginStop is the one transition, owned by one Once: after it no frame can be
// written by anything but the close itself.
func (s *Session) beginStop() {
	s.stopOnce.Do(func() {
		s.sendMu.Lock()
		close(s.stopping)
		s.sendMu.Unlock()
	})
}

func (s *Session) hasStopBegun() bool {
	select {
	case <-s.stopping:
		return true
	default:
		return false
	}
}

// isSocketGone reports whether there is anything left to say goodbye to: the
// read loop has ended, or the provider has already ended the session itself.
func (s *Session) isSocketGone() bool {
	select {
	case <-s.readDone:
		return true
	default:
	}
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// sendSessionClose writes the one frame that may follow the stop transition.
// It takes the same lock every other send does, so it cannot overtake one.
func (s *Session) sendSessionClose() error {
	data, err := json.Marshal(simpleEvent{
		Type: "session.close", EventID: s.nextEventID()})
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.conn.Send(data)
}

// awaitPacer waits for the uplink goroutine to notice. It exits on stopping, so
// this is a handshake rather than a wait.
func (s *Session) awaitPacer() {
	if s.pacer == nil {
		return
	}
	select {
	case <-s.pacer.stopped:
	case <-time.After(time.Second):
		s.log.Warn("the uplink did not stop promptly")
	}
}

// awaitClosed waits for the provider to confirm, on this client's own bound.
// Every caller passes a context with no deadline in it, so a provider that
// stops answering must not be able to hold the call open.
func (s *Session) awaitClosed(ctx context.Context) {
	timer := time.NewTimer(s.closeWait)
	defer timer.Stop()

	select {
	case <-s.closed:
	case <-s.readDone:
	case <-ctx.Done():
	case <-timer.C:
	}

	select {
	case <-s.closed:
		s.setOutcome(outcomeCleanClose)
	default:
		s.setOutcome(outcomeCloseTimeout)
	}
}

// setOutcome records how the session ended. The first answer stands: whatever
// decided it first is what happened, and everything after that is a
// consequence of it.
func (s *Session) setOutcome(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal == "" {
		s.terminal = outcome
	}
}

func (s *Session) outcome() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *Session) logID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logid
}

//
// Writing.
//

// sendFrame writes one already-encoded frame, unless the session has begun
// ending. The check and the write are under one lock, which is what makes
// "nothing after session.close" true rather than likely.
func (s *Session) sendFrame(data []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	select {
	case <-s.stopping:
		return wsconn.ErrSessionClosed
	default:
	}
	if s.conn == nil {
		return wsconn.ErrSessionClosed
	}
	return s.conn.Send(data)
}

// simpleFrame encodes one of the events that is nothing but its own name.
func (s *Session) simpleFrame(eventType string) []byte {
	data, err := json.Marshal(simpleEvent{Type: eventType, EventID: s.nextEventID()})
	if err != nil {
		// simpleEvent is two strings; this cannot happen.
		s.log.Error("could not encode an event", "type", eventType, "error", err)
	}
	return data
}

// uplinkFailure is the pacer's last write error, reported to the caller that
// asked for the next frame. The read loop reports the connection itself, so
// this does not report it a second time.
func (s *Session) uplinkFailure() error {
	if s.pacer == nil {
		return nil
	}
	return s.pacer.err()
}

// failure is why the session could not be started, if it could not.
func (s *Session) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startError
}

// finishStart releases Start, recording the first failure if there was one.
func (s *Session) finishStart(err error) {
	s.readyOnce.Do(func() {
		if err != nil {
			s.mu.Lock()
			s.startError = err
			s.mu.Unlock()
		}
		close(s.ready)
	})
}
