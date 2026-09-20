// SPDX-License-Identifier: Apache-2.0

// Package gemini is a third speech-to-speech client, for a third wire protocol.
//
// The rule this repository holds to — a new engine is a new profile, never a
// second client — is about dialects of one protocol. This is not one of them.
// The session is configured once, in a setup frame that is answered before
// anything else may be sent, and NOTHING in it can be changed afterwards: not
// the instructions, not the tools, not the voice. Turns are the server's to
// declare, twice over, because the model stopping producing and the caller
// having heard it are two different messages seconds apart. There is no cancel
// primitive at all — a turn is stopped by starting another one. Errors are not
// frames: the socket closes with a code and a sentence. And every audio part of
// every frame is speech, several to a frame, arriving about three times faster
// than the caller can hear it. None of that is expressible as values in a
// Profile.
//
// What it is NOT is a second seam. provider.VoiceSession is the only interface
// above this package, and every event it produces is one the existing clients
// already produce; nothing here is cascaded, and no recognition, synthesis or
// language-model concept appears anywhere in it.
//
// # What cannot change mid-call, and what this client does about it
//
// Measured, against the live service: a user-role turn carrying new standing
// instructions is NOT obeyed on its own; the same words inside a tool result's
// output ARE obeyed, and persist; and a phase's own paragraph prefixed to a
// text cue inside one user turn IS obeyed (eighteen attempts, eighteen times).
// So UpdateInstructions writes no frame. It keeps the part of the new
// instructions that the session was not started with — the phase's own words,
// found by taking off the prefix the two share — and the next thing this client
// says to the model carries it: a tool result through its hint, a keypress
// through the turn that reports it.
//
// # Goroutines, and what ends them
//
// There are four, and nothing else. The speech detector has none on purpose: it
// runs on the call's own goroutine inside SendAudio, so a frame of caller audio
// is looked at by the code that already had it in hand.
//
//	goroutine    | owner        | started by         | stops on                 | in-flight work on stop
//	-------------|--------------|--------------------|--------------------------|------------------------
//	read loop    | Session      | Start, after dial  | socket read error/close  | sole closer of Events(); emits CLOSED last
//	pacer        | Session      | Start, once up     | stopping closed          | queued frames discarded, never flushed
//	watchdog     | Session      | Start, after dial  | conn.Done()              | disarms; emits nothing after Done
//	keepalive    | wsconn.Conn  | wsconn.Dial        | conn.Done() / write fail | none
package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

const (
	// eventBuffer is how far the consumer may fall behind. The consumer is a
	// per-call actor forwarding to a paced send queue, so it should never come
	// close — but this engine delivers a whole sentence of audio in a handful of
	// frames, each carrying several parts, so the room is worth having.
	eventBuffer = 256

	// firstAudioTimeout bounds the wait between a turn being announced and the
	// first of it arriving; deltaStallTimeout bounds a gap in the middle of one.
	//
	// Both are generous by the standards of the other clients here, and
	// deliberately: the downlink runs about three times faster than real time,
	// so a gap is a stall rather than a model thinking, but this provider's own
	// sockets were measured stalling a single write for up to 3.6 seconds part
	// way into a session.
	firstAudioTimeout = 4 * time.Second
	deltaStallTimeout = 4 * time.Second

	// closeTimeout bounds the wait for the session to finish ending. Every
	// caller passes a context with no deadline in it, so the bound has to be
	// this client's own.
	closeTimeout = 2 * time.Second
)

// How a session ended, for the one line logged when it does. A call is released
// on the strength of which of these it was, so each is a distinct word rather
// than a flag.
const (
	outcomeCleanClose     = "CLOSED_CLEAN"
	outcomeClosedByServer = "CLOSED_BY_SERVER"
	outcomeConnectionLost = "CONN_LOST"
	// outcomeSessionExpired is the provider announcing the end of the
	// connection while the caller is still on the line. Nothing is broken, which
	// is why it is not CONN_LOST.
	outcomeSessionExpired = "SESSION_EXPIRED"
)

// rejectedOutcome names the close code, because which code it was decides
// nothing here but explains everything afterwards: 1007 is a field in the setup
// this client should not have sent, 1008 is a model or a credential.
func rejectedOutcome(code int) string { return "REJECTED(" + strconv.Itoa(code) + ")" }

// Session is one conversation with the model.
type Session struct {
	profile provider.Profile
	apiKey  string
	log     *slog.Logger

	events chan provider.Event
	conn   *wsconn.Conn

	// ready closes when the provider has accepted the setup, which is the one
	// thing that must happen before anything else may be sent.
	ready     chan struct{}
	readyOnce sync.Once

	// stopping closes when the session has begun ending, whoever decided it.
	// Every send is gated on it, so nothing can be written after the socket has
	// been told the session is over.
	stopping  chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
	// readDone closes when the read loop has ended. It is closed BEFORE the
	// event channel is, so a Close called from the goroutine draining events
	// never waits on itself.
	readDone chan struct{}

	// sendMu orders the transition to stopping against every write: a frame is
	// only written by a holder that has just seen the session still running.
	sendMu sync.Mutex

	// emitMu makes the event channel single-writer without making it
	// single-goroutine. The read loop, the watchdog and the speech detector all
	// have something to say; the channel is closed under this lock, with CLOSED
	// written in the same critical section, so nothing can reach a closed
	// channel and nothing can follow the last event.
	emitMu         sync.Mutex
	isEventsClosed bool

	// isTurnOpen is true between the first output of a turn and it ending,
	// however it ended. The watchdog consults it, and so does everything that
	// has to know whether there is anything to interrupt.
	isTurnOpen atomic.Bool

	// isAnswerOwed is true between a tool result going out and the model
	// producing anything at all in reply.
	//
	// There is no turn open in that window — the tool call's own turn ended when
	// the calls were handed over — and nothing on this protocol announces that
	// the model has taken the answer. So it is a second reason for the watchdog
	// to be watching, and the only one there is for the sixty-four seconds a
	// live caller once spent listening to a model that had stopped answering.
	isAnswerOwed atomic.Bool

	mu         sync.Mutex
	cfg        provider.SessionConfig
	voice      string
	tools      []toolDeclarations
	silenceMs  int
	startError error
	terminal   string
	// baseInstructions is what the session was started with, and the only
	// instructions this model will ever have: they are fixed at setup, so there
	// is no second value to keep. What a phase adds to them is the difference
	// between them and what the flow now wants said.
	baseInstructions string
	// pendingPhase is that difference, waiting for something to carry it.
	pendingPhase string

	// isFenced drops the rest of a turn this client decided to stop. There is no
	// cancel on this protocol, so the model goes on producing and the caller has
	// already stopped hearing it.
	isFenced bool
	// isPreemptedByUs remembers who stopped the turn now ending, which is what
	// keeps an interruption this client made from being logged as the caller's.
	isPreemptedByUs bool
	preemptReason   provider.InterruptReason
	// turnAudioBytes is what the turn has actually carried; hasToolCall says it
	// was a turn spent calling functions instead of speaking.
	turnAudioBytes int
	hasToolCall    bool
	// said and heard accumulate the transcript fragments of a turn. This
	// protocol never marks the last fragment, so the client decides when a line
	// is finished: the model's when its turn closes, the caller's when the model
	// answers them.
	said  strings.Builder
	heard strings.Builder

	// The set of calls the model is waiting on. It goes on only once every one
	// of them has an answer, so they are held here until the set is complete.
	// The names are kept with them: a response names the function as well as
	// the call, and by then the flow has only handed back an id.
	toolOrder   []string
	toolPending map[string]bool
	toolNames   map[string]string
	toolResults map[string]json.RawMessage
	// staleWarned is the ids already reported as answered too late, so a flow
	// that retries does not fill the log with the same line.
	staleWarned map[string]bool

	dog      *provider.Watchdog
	detector *speechDetector

	// Deadlines held as fields so tests need not wait seconds for behaviour that
	// is measured in seconds on a real call.
	firstAudioDeadline time.Duration
	deltaStallDeadline time.Duration
	setupWait          time.Duration
	closeWait          time.Duration

	pacer *pacer
	// ticks is the pacer's cadence. Production fills it with a ticker in Start;
	// a test sets it beforehand and drives the pacer a tick at a time.
	ticks      <-chan time.Time
	paced      chan struct{}
	stopTicker func()
	// uplinkWrite and clock are the same idea as ticks, for the other half of
	// what this uplink measures: a socket write a test can make take four
	// seconds, and a clock it can move without waiting. Both are nil on a call.
	uplinkWrite func([]byte) error
	clock       func() time.Time
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
		readDone: make(chan struct{}),

		firstAudioDeadline: firstAudioTimeout,
		deltaStallDeadline: deltaStallTimeout,
		setupWait:          wsconn.DialTimeout,
		closeWait:          closeTimeout,
	}, nil
}

// Session satisfies the one interface above this package.
var _ provider.VoiceSession = (*Session)(nil)

func (s *Session) Events() <-chan provider.Event { return s.events }

// Start connects, configures the session and waits for the provider to accept
// it, then asks for the opening turn.
//
// This model waits to be spoken to before it says anything, and our bot answers
// the telephone: so something is always asked for. A flow with a line of its own
// asks for that line; a flow without one asks for a turn with nothing in it at
// all, which the model answers by greeting from its instructions.
func (s *Session) Start(ctx context.Context, cfg provider.SessionConfig) error {
	voice := cfg.Voice
	if voice == "" {
		voice = s.profile.Voice
	}
	s.mu.Lock()
	s.cfg = cfg
	s.baseInstructions = cfg.Instructions
	s.voice = voice
	s.tools = buildTools(cfg.Tools)
	s.silenceMs = cfg.Turn.SilenceMs
	s.mu.Unlock()
	s.detector = newSpeechDetector(cfg.Turn.SilenceMs)

	headers := http.Header{}
	headers.Set(apiKeyHeader, s.apiKey)
	for key, value := range s.profile.Headers {
		headers.Set(key, value)
	}

	endpoint := s.profile.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	conn, err := wsconn.Dial(ctx, endpoint, headers, s.log)
	if err != nil {
		return err
	}
	s.conn = conn
	s.log.Info("provider session opening", "endpoint", endpoint)

	s.dog = provider.NewWatchdog(provider.WatchdogConfig{
		FirstAudioDeadline: s.firstAudioDeadline,
		DeltaStallDeadline: s.deltaStallDeadline,
		Done:               conn.Done(),
		IsResponseOpen:     s.isOutputDue,
		OnStall:            s.onTurnStalled,
	})
	go s.readLoop()
	go s.dog.Run()

	if err := s.sendFrameOf(s.buildSetup()); err != nil {
		s.conn.Close()
		return fmt.Errorf("configure the session: %w", err)
	}

	// Nothing may be sent until the setup has been answered. A frame that
	// arrives ahead of setupComplete is not read, and a setup this service will
	// not have is answered by closing the socket rather than by saying so.
	select {
	case <-s.ready:
	case <-ctx.Done():
		s.conn.Close()
		return ctx.Err()
	case <-time.After(s.setupWait):
		s.conn.Close()
		if err := s.failure(); err != nil {
			return fmt.Errorf("session refused: %w", err)
		}
		return errors.New("the provider did not answer the setup")
	}
	if err := s.failure(); err != nil {
		s.conn.Close()
		return fmt.Errorf("session refused: %w", err)
	}

	if err := s.openTheCall(); err != nil {
		s.conn.Close()
		return err
	}

	// A ticker rather than a sleeping loop: its period is absolute, so a tick
	// the pacer was late for is a tick missed rather than a frame queued behind
	// one.
	ticks := s.ticks
	if ticks == nil {
		ticker := time.NewTicker(provider.FrameInterval)
		ticks = ticker.C
		s.stopTicker = ticker.Stop
	}
	s.pacer = newPacer(s, ticks, audioMimeType(s.inputRateHz()))
	go s.pacer.run()
	return nil
}

// openTheCall asks for the first turn.
//
// The flow's own opening line goes out as a line to repeat, exactly as a mid-call
// one does. Its words are NOT reported from here: this model transcribes its own
// speech, so the greeting is announced when it is said, by the read loop, in the
// words that were actually spoken rather than the ones that were asked for.
func (s *Session) openTheCall() error {
	s.mu.Lock()
	opening := s.cfg.OpeningText
	language := s.cfg.Language
	s.mu.Unlock()

	if opening == "" {
		if err := s.sendFrameOf(openingTurn()); err != nil {
			return fmt.Errorf("open the call: %w", err)
		}
		return nil
	}
	if err := s.sendFrameOf(userTurn(provider.SayExactly(opening, language))); err != nil {
		return fmt.Errorf("open the call: %w", err)
	}
	return nil
}

// inputRateHz is the rate the caller's audio will actually be in, which is what
// every uplink frame has to declare.
func (s *Session) inputRateHz() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rate := s.cfg.InputFormat.RateHz; rate > 0 {
		return rate
	}
	return s.profile.LinearInput.RateHz
}

// SendAudio hands one frame of caller audio to the pacer and returns.
//
// It never blocks on the socket. What feeds it is the media path, and a stalled
// write there is audio lost in both directions — and this provider's sockets do
// stall, for seconds at a time, part way into a session.
//
// The frame is copied because it is not ours: the call's converter writes the
// next one into the same buffer, and a queued frame would arrive as whatever
// came after it. The detector reads the caller's own bytes before the copy,
// which is the one place in this client where a frame of audio is looked at
// rather than forwarded.
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

	s.listenForSpeech(audio)
	s.pacer.push(append([]byte(nil), audio...))
	return nil
}

// SendUserText puts something the caller did, but did not say, into the
// conversation and asks the model to respond to it.
//
// It is also what carries a phase change. The instructions cannot be replaced
// mid-session, but words in a user turn are obeyed when the phase's own
// paragraph leads them (measured), so a pending phase goes out in front of the
// cue, in the same turn, once.
func (s *Session) SendUserText(text string) error {
	if text == "" {
		return nil
	}

	s.mu.Lock()
	if phase := s.pendingPhase; phase != "" {
		text = phase + "\n\n" + text
		s.pendingPhase = ""
	}
	s.mu.Unlock()

	s.markPreempted(provider.InterruptReasonSystem)
	return s.sendFrameOf(userTurn(text))
}

// SpeakText says a line the flow chose.
//
// This model cannot be handed words to speak; it can only be told to repeat a
// sentence, which is best effort and the same class of promise the Realtime
// client makes. It PRE-EMPTS: a user turn with turnComplete unconditionally
// stops whatever is being said, measured. It does NOT queue — a second line
// replaces the first, because the server does exactly that and both lines say
// what should come next.
//
// Nothing is emitted here: this is normally called from the goroutine draining
// Events, and emitting would deadlock the call.
func (s *Session) SpeakText(text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	language := s.cfg.Language
	s.mu.Unlock()

	s.markPreempted(provider.InterruptReasonSystem)
	return s.sendFrameOf(userTurn(provider.SayExactly(text, language)))
}

// SendToolResult answers one function call, and sends the set once it is
// complete.
//
// The model is blocked on every call it made — that is what behavior BLOCKING
// buys — so it goes on only when all of them have an answer, and they travel in
// one frame.
//
// A call the server has withdrawn is answered with nothing at all. Measured: an
// interruption while a call is outstanding makes the server discard it and ask
// again under a new id, and a response carrying the old one is ignored. Saying
// so in the log is all there is to do; returning an error would make the flow
// engine report a failure the caller never experienced.
func (s *Session) SendToolResult(toolCallID, output, hint string) error {
	merged := provider.MergeHint(output, hint)

	s.mu.Lock()
	if !s.toolPending[toolCallID] {
		isFirst := !s.staleWarned[toolCallID]
		if s.staleWarned == nil {
			s.staleWarned = map[string]bool{}
		}
		s.staleWarned[toolCallID] = true
		s.mu.Unlock()
		if isFirst {
			s.log.Warn("a tool result arrived for a call the model is no longer waiting on",
				"toolCallId", toolCallID)
		}
		return nil
	}
	delete(s.toolPending, toolCallID)
	s.toolResults[toolCallID] = asJSONValue(merged)
	if len(s.toolPending) > 0 {
		s.mu.Unlock()
		return nil
	}

	responses := make([]functionResponse, 0, len(s.toolOrder))
	for _, callID := range s.toolOrder {
		responses = append(responses, functionResponse{
			ID:       callID,
			Name:     s.toolNames[callID],
			Response: functionOutput{Output: s.toolResults[callID]},
		})
	}
	s.toolOrder = nil
	s.toolResults = nil
	s.mu.Unlock()

	if err := s.sendFrameOf(toolResponseFrame{
		ToolResponse: toolResponseBody{FunctionResponses: responses}}); err != nil {
		return err
	}

	// The model now owes the conversation something — speech, or another call —
	// and until it produces one of them there is no turn for the watchdog to be
	// watching. Measured on a live call: the model answered tool results with
	// more tool calls and then with nothing at all, for a minute, while the
	// caller heard silence and nothing here was waiting on anything.
	s.isAnswerOwed.Store(true)
	s.dog.Signal(provider.WatchResponseStarted)
	return nil
}

// isOutputDue reports whether the model owes the caller anything: a turn it is
// in the middle of, or an answer to a tool result it has not begun. It is what
// the watchdog consults before abandoning either.
func (s *Session) isOutputDue() bool {
	return s.isTurnOpen.Load() || s.isAnswerOwed.Load()
}

// UpdateInstructions takes the new instructions and writes no frame.
//
// It cannot write one: system instructions are fixed at setup on this protocol,
// and a user turn carrying replacement instructions was measured being ignored.
// What IS obeyed is the phase's own words leading a user turn, so the part of
// the new instructions that is new — everything past the prefix it shares with
// the ones the session was started with — is kept until something says it.
//
// Nothing else consumes it: a tool result already carries its phase in the hint
// that travels with the result.
func (s *Session) UpdateInstructions(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pendingPhase = phaseSegment(s.baseInstructions, text)
	s.log.Debug("the phase changed", "hasPhaseText", s.pendingPhase != "")
	return nil
}

// phaseSegment is what a phase added to the standing instructions.
//
// The prefix the two share is the persona and the rules, which the model has had
// since setup and does not need again; what is left is the phase. A text that
// added nothing is carried not at all.
//
// The shared prefix counts only when it is the WHOLE of the original: a flow
// moves between phases by appending to its persona, and instructions that
// diverge part way through a sentence were rewritten rather than extended. What
// the model has not been told is then all of it, and half a sentence is not a
// phase.
//
// The comparison is by rune, not by byte: half the instructions in this
// repository are Chinese, and a prefix that ended inside a character would put a
// broken one at the front of what the model is told.
func phaseSegment(base, updated string) string {
	baseRunes := []rune(base)
	updatedRunes := []rune(updated)

	shared := 0
	for shared < len(baseRunes) && shared < len(updatedRunes) &&
		baseRunes[shared] == updatedRunes[shared] {
		shared++
	}
	if shared < len(baseRunes) {
		return strings.TrimSpace(updated)
	}
	return strings.TrimSpace(string(updatedRunes[shared:]))
}

// Interrupt stops the model talking over the caller.
//
// The caller's own speech needs nothing sent and nothing remembered: the server
// hears them, stops on its own and says so, and that message is where the
// interruption is reported from.
//
// A keypress or an application decision has nowhere to go. This protocol has no
// cancel: the only way to stop a turn is to start another one, and doing that
// from here would put words into the conversation that nobody asked for. So the
// turn is fenced off instead — the rest of its audio is dropped rather than
// played, because the caller has already stopped hearing it — and when the turn
// does end it is reported as the interruption it was rather than as a response
// that ran to completion. If a cue follows, as it does for a keypress, that cue
// pre-empts the turn for real and the server confirms it.
//
// playedMs is logged and nothing more: there is no truncation event on this
// protocol, so how much the caller heard cannot be put into the model's own
// history. What it is for — flushing what the caller was about to hear —
// happens on the call's side either way.
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
		// This turn has already been stopped; asking twice is noise.
		s.mu.Unlock()
		return nil
	}
	s.isFenced = true
	s.isPreemptedByUs = true
	s.preemptReason = reason
	s.mu.Unlock()

	s.log.Debug("stopping the turn", "reason", reason, "playedMs", playedMs)
	return nil
}

// markPreempted records that the turn now open is one this client is replacing,
// so the interruption the server reports is not blamed on the caller. With no
// turn open there is nothing to replace, and a reason already given stands: a
// keypress that is then spoken to the model is still the keypress.
func (s *Session) markPreempted(reason provider.InterruptReason) {
	if !s.isTurnOpen.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isPreemptedByUs {
		return
	}
	s.isPreemptedByUs = true
	s.preemptReason = reason
}

// Close ends the session, once, however many things decide it at the same
// moment.
//
// The order is the whole of it: stopping closes first, so the pacer exits and
// every other send starts refusing, and only then is the socket closed — which
// is what sends the close frame, because on this protocol the WebSocket close IS
// the goodbye. There is no application frame that ends a session.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.beginStop()
		if s.conn == nil {
			s.setOutcome(outcomeCleanClose)
			return
		}
		s.awaitPacer()

		if s.isSocketGone() {
			// There is nothing left to say goodbye to.
			s.setOutcome(outcomeConnectionLost)
		} else {
			s.setOutcome(outcomeCleanClose)
		}
		// One close frame, from here and nowhere else.
		s.conn.Close()
		s.awaitReadLoop(ctx)

		stats := s.Stats()
		s.log.Info("provider session finished", "outcome", s.outcome(),
			"framesSent", stats.FramesSent, "framesDropped", stats.FramesDropped,
			"streamEnds", stats.StreamEnds, "slowWrites", stats.SlowWrites,
			"maxWriteMs", stats.MaxWriteMs,
			"maxBacklogFrames", stats.MaxBacklogFrames)
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
// written at all.
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

// isSocketGone reports whether the read loop has already ended, which is the
// only way there is nothing left to close.
func (s *Session) isSocketGone() bool {
	select {
	case <-s.readDone:
		return true
	default:
		return false
	}
}

// awaitPacer waits for the uplink goroutine to notice. It exits on stopping, so
// this is a handshake rather than a wait.
func (s *Session) awaitPacer() {
	if s.pacer == nil {
		return
	}
	select {
	case <-s.pacer.stopped():
	case <-time.After(time.Second):
		s.log.Warn("the uplink did not stop promptly")
	}
}

// awaitReadLoop waits for the session to finish ending, on this client's own
// bound. Every caller passes a context with no deadline in it, so nothing that
// goes wrong here may hold a phone call open.
func (s *Session) awaitReadLoop(ctx context.Context) {
	timer := time.NewTimer(s.closeWait)
	defer timer.Stop()

	select {
	case <-s.readDone:
	case <-ctx.Done():
	case <-timer.C:
		s.log.Warn("the provider session did not finish ending promptly")
	}
}

// setOutcome records how the session ended. The first answer stands: whatever
// decided it first is what happened, and everything after that is a consequence
// of it.
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

//
// Writing.
//

// sendFrame writes one already-encoded frame, unless the session has begun
// ending. The check and the write are under one lock, which is what makes
// "nothing after the close" true rather than likely.
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

// uplinkFailure is the pacer's last write error, reported to the caller that
// asked for the next frame. The read loop reports the connection itself, so this
// does not report it a second time.
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
