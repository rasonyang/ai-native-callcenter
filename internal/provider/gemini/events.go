// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// closeReasonLimit is how much of a close reason the protocol carries. Longer
// reasons arrive already cut off, and the bound is repeated here so a log line
// and an error text say the same thing.
const closeReasonLimit = 123

// readLoop is the only goroutine that reads the socket, and the one that emits
// almost everything. Everything a caller-side method decides is recorded under
// the mutex and reported from here: those methods are called from the goroutine
// draining Events, and emitting from one would deadlock the call.
func (s *Session) readLoop() {
	// finish is registered first so that it runs last: readDone closes before
	// the event channel does, and a Close called from the consumer's own
	// goroutine never waits on itself.
	defer s.finish()
	defer close(s.readDone)

	for {
		frame, _, err := s.receive()
		if err != nil {
			// The outcome is recorded before Start is released, because
			// finishStart hands control back to the caller: one that gets it
			// first can Close the session and stamp a clean outcome over the
			// refusal the provider actually gave, and the first outcome wins.
			s.reportLostConnection(err)
			s.finishStart(err)
			return
		}
		if frame == nil {
			continue
		}
		s.handle(frame)
	}
}

// reportLostConnection fails the call for a socket that ended by itself.
//
// There is no reconnect: the provider holds conversation state that cannot be
// rebuilt, so a lost socket ends the session and the call is routed somewhere a
// person can take it. A session that was already ending, or that has already
// been failed for a reason of its own — a goAway, most of all — says nothing
// here, because one broken session is one failure.
func (s *Session) reportLostConnection(err error) {
	if s.hasStopBegun() || s.outcome() != "" || errors.Is(err, net.ErrClosed) {
		return
	}

	var closed *websocket.CloseError
	if errors.As(err, &closed) && isCloseFrameCode(closed.Code) {
		if isNormalCode(closed.Code) {
			// A polite close nobody asked for is still the end of the
			// conversation, and the call has to go somewhere else.
			s.setOutcome(outcomeClosedByServer)
			s.failSession(err, "the provider ended the session")
			return
		}
		// This is the whole error channel: there is no error frame on this
		// protocol, only a code and a sentence that the server truncates.
		s.setOutcome(rejectedOutcome(closed.Code))
		s.failSession(err, fmt.Sprintf("the provider refused the session: %d %s",
			closed.Code, truncateReason(closed.Text)))
		return
	}

	s.setOutcome(outcomeConnectionLost)
	s.failSession(err, "provider connection lost")
}

// failSession reports the one error a session gets: the call cannot continue on
// this provider and has to be routed elsewhere.
func (s *Session) failSession(err error, text string) {
	s.emit(provider.Event{
		Type: provider.EventTypeError, Err: err, IsFatal: true, Text: text})
}

// isNormalCode is the two close codes that mean the other end finished politely.
func isNormalCode(code int) bool {
	return code == websocket.CloseNormalClosure || code == websocket.CloseGoingAway
}

// isCloseFrameCode says the provider actually sent a close frame carrying this
// code. The three it excludes are the library's own words for a socket that
// ended without one — a dropped connection is not a refusal, and reporting it as
// "the provider refused the session: 1006" would send somebody looking for a
// field in the setup that was never the problem.
func isCloseFrameCode(code int) bool {
	switch code {
	case websocket.CloseAbnormalClosure, websocket.CloseNoStatusReceived,
		websocket.CloseTLSHandshake:
		return false
	default:
		return true
	}
}

func truncateReason(reason string) string {
	if len(reason) <= closeReasonLimit {
		return reason
	}
	return reason[:closeReasonLimit]
}

// handle is the whole mapping from this protocol to the call's vocabulary.
//
// A frame carries exactly one of these, and everything else this service sends —
// a bare {}, an empty serverContent, a session resumption handle nobody asked
// for, usage metadata, a key from a version this client has not seen — decodes
// into no fields at all and means nothing to a phone call.
func (s *Session) handle(frame *serverFrame) {
	switch {
	case frame.SetupComplete != nil:
		s.onSetupComplete()
	case frame.ToolCall != nil:
		s.onToolCalls(frame.ToolCall)
	case frame.ToolCallCancellation != nil:
		s.onToolCallsWithdrawn(frame.ToolCallCancellation)
	case frame.GoAway != nil:
		s.onGoAway(frame.GoAway)
	case frame.ServerContent != nil:
		s.onServerContent(frame.ServerContent)
	}
}

// onSetupComplete is the one gate: nothing may be sent before it arrives.
func (s *Session) onSetupComplete() {
	s.finishStart(nil)
	// The caller may speak before the bot has said anything, and nothing is
	// playing yet, so the detector starts listening here.
	s.detector.arm()
	s.emit(provider.Event{Type: provider.EventTypeSessionReady})
}

// onServerContent is a turn's worth of anything: audio, either transcript, and
// the two or three flags that end it.
//
// The order within one frame is the order the call needs it in. Audio first,
// because a frame that carries speech and the transcript of it is the speech
// arriving; the flags last, because they are about what came before them.
func (s *Session) onServerContent(event *serverContent) {
	if event.ModelTurn != nil {
		for _, item := range event.ModelTurn.Parts {
			if item.InlineData == nil || item.InlineData.Data == "" {
				continue
			}
			s.onModelAudio(item.InlineData.Data)
		}
	}
	if event.OutputTranscription != nil && event.OutputTranscription.Text != "" {
		s.onOutputTranscript(event.OutputTranscription.Text)
	}
	if event.InputTranscription != nil && event.InputTranscription.Text != "" {
		s.onInputTranscript(event.InputTranscription.Text)
	}

	// interrupted and turnComplete can arrive in one frame; so, in principle,
	// can generationComplete and turnComplete. Each is handled on its own, in
	// the order a turn passes through them.
	if event.Interrupted {
		s.onInterrupted()
	}
	if event.GenerationComplete {
		s.onGenerationComplete()
	}
	if event.TurnComplete {
		s.onTurnComplete()
	}
}

// onModelAudio is the model speaking.
func (s *Session) onModelAudio(encoded string) {
	s.beginTurn()

	s.mu.Lock()
	isFenced := s.isFenced
	s.mu.Unlock()
	if isFenced {
		// A turn this client stopped. The caller is no longer hearing it, and
		// there is no way to make the model stop producing it.
		s.log.Debug("audio arrived for a turn that was stopped")
		return
	}

	audio, err := base64.StdEncoding.AppendDecode(nil, []byte(encoded))
	if err != nil {
		s.emit(provider.Event{Type: provider.EventTypeError,
			Err: fmt.Errorf("undecodable audio: %w", err)})
		return
	}

	s.mu.Lock()
	s.turnAudioBytes += len(audio)
	s.mu.Unlock()

	s.dog.Signal(provider.WatchAudioArrived)
	s.emit(provider.Event{Type: provider.EventTypeAudioDelta, Audio: audio})
}

// beginTurn opens a turn on the first output of one, and is the only place a
// turn is armed.
//
// There is no frame that announces a turn on this protocol: the first thing the
// model produces IS the turn starting, whether that is a syllable or a function
// call.
func (s *Session) beginTurn() {
	// Whatever the model owed, it has begun paying: this is the first output
	// after a tool result as much as it is the first output of anything else.
	s.isAnswerOwed.Store(false)
	if s.isTurnOpen.Load() {
		return
	}
	// The caller's words, before the answer to them. This protocol never marks
	// the last fragment of a transcript, so the model answering is what says
	// the caller has finished.
	s.flushInputTranscript()

	s.mu.Lock()
	s.isFenced = false
	s.isPreemptedByUs = false
	s.preemptReason = ""
	s.turnAudioBytes = 0
	s.hasToolCall = false
	s.said.Reset()
	s.mu.Unlock()

	s.isTurnOpen.Store(true)
	s.detector.disarm()
	s.dog.Signal(provider.WatchResponseStarted)
	s.emit(provider.Event{Type: provider.EventTypeResponseStarted})
}

// onOutputTranscript is the model's own words, a fragment at a time.
//
// Nothing marks the last one, so the client decides where the line ends: the
// fragments are accumulated and the finished line is announced when the turn
// closes. The running text is reported as it grows, because a consumer that
// wants to show what is being said now should not have to wait for the full
// stop — and because the other clients here report partials the same way.
func (s *Session) onOutputTranscript(fragment string) {
	s.mu.Lock()
	s.said.WriteString(fragment)
	sofar := s.said.String()
	s.mu.Unlock()

	s.emit(provider.Event{Type: provider.EventTypeOutputTranscript, Text: sofar})
}

// flushOutputTranscript announces the model's finished line, once.
func (s *Session) flushOutputTranscript() {
	s.mu.Lock()
	said := s.said.String()
	s.said.Reset()
	s.mu.Unlock()

	if said == "" {
		return
	}
	s.emit(provider.Event{
		Type: provider.EventTypeOutputTranscript, Text: said, IsFinal: true})
}

// onInputTranscript is what the caller said, a fragment at a time and on no
// particular schedule: this protocol guarantees no ordering at all between
// these and anything else.
func (s *Session) onInputTranscript(fragment string) {
	s.mu.Lock()
	s.heard.WriteString(fragment)
	sofar := s.heard.String()
	s.mu.Unlock()

	s.emit(provider.Event{Type: provider.EventTypeInputTranscript, Text: sofar})
}

// flushInputTranscript announces what the caller said, once. It runs when the
// model answers them, and again when a turn ends, for the fragment that arrived
// too late to be either.
func (s *Session) flushInputTranscript() {
	s.mu.Lock()
	heard := s.heard.String()
	s.heard.Reset()
	s.mu.Unlock()

	if heard == "" {
		return
	}
	s.emit(provider.Event{
		Type: provider.EventTypeInputTranscript, Text: heard, IsFinal: true})
}

// onInterrupted is a turn cut short.
//
// Who cut it short has to be said, because a turn this client replaced is not a
// barge-in and logging it as one puts an interruption the caller never made into
// the record of the call. Everything else is the server's own voice detection
// hearing them, which no other message announces — so the caller starting to
// speak is reported from here too.
//
// An interruption with no turn open is the caller talking over the tail of a
// turn that has already finished generating. There is nothing left to close, but
// the call still has to know to flush what it was about to play.
func (s *Session) onInterrupted() {
	s.mu.Lock()
	isOurs := s.isPreemptedByUs
	by := s.preemptReason
	s.isPreemptedByUs = false
	s.preemptReason = ""
	s.mu.Unlock()

	// Anything this client did not do is the server's own detector hearing the
	// caller: there is no other way a turn is stopped here.
	if !isOurs {
		by = provider.InterruptReasonSpeech
	} else if by == "" {
		by = provider.InterruptReasonSystem
	}

	// The model is no longer waiting on anything it asked for: the server
	// discards outstanding calls when a turn is interrupted, and asks again
	// under new ids. An answer it had already been given is discarded with
	// them, so nothing is owed on it either.
	s.withdrawToolCalls("the turn was interrupted")
	s.isAnswerOwed.Store(false)

	wasOpen := s.isTurnOpen.Swap(false)
	s.dog.Signal(provider.WatchResponseEnded)
	s.flushOutputTranscript()

	if !isOurs {
		s.emit(provider.Event{Type: provider.EventTypeSpeechStarted})
	}
	if !wasOpen {
		// Already closed, by generation finishing or by the watchdog. One turn
		// gets one ending.
		return
	}
	s.settlePlayback()
	s.emit(provider.Event{
		Type: provider.EventTypeInterrupted, InterruptedBy: by})
}

// onGenerationComplete is the model having stopped producing, which is the turn
// ending as far as the conversation is concerned.
//
// It is NOT turnComplete: that one trails by the playback the server presumes
// happened, and this application counts the frames the caller actually heard
// itself.
//
// A turn that carried nothing is reported as a turn all the same. Proactive
// audio is permanently enabled on this model — it is allowed to decide that the
// right answer is to say nothing — so silence is a conversation, not a fault,
// and a turn spent calling a function has no audio by definition.
func (s *Session) onGenerationComplete() {
	wasOpen := s.isTurnOpen.Swap(false)
	s.dog.Signal(provider.WatchResponseEnded)
	s.flushOutputTranscript()

	if !wasOpen {
		s.log.Debug("generation finished on a turn that was already closed")
		return
	}

	s.mu.Lock()
	isFenced := s.isFenced
	by := s.preemptReason
	audioBytes := s.turnAudioBytes
	hasToolCall := s.hasToolCall
	s.mu.Unlock()

	s.settlePlayback()

	if isFenced {
		if by == "" {
			by = provider.InterruptReasonSystem
		}
		// The server never heard about this one — there is no cancel — but the
		// caller stopped hearing it when the decision was made.
		s.emit(provider.Event{
			Type: provider.EventTypeInterrupted, InterruptedBy: by})
		return
	}
	if audioBytes == 0 && !hasToolCall {
		s.log.Warn("the model finished a turn without speaking")
	}
	s.emit(provider.Event{Type: provider.EventTypeResponseDone})
}

// onTurnComplete is the server's bookkeeping: the turn is over on its side, and
// it will produce nothing more until it is given something.
//
// It is not an event. It trails the real end of the turn by the playback the
// server presumes happened, so a RESPONSE_DONE here would arrive after the call
// had moved on. What it does mean is that nothing is playing that this client
// knows of, which is when listening for the caller is worth anything.
func (s *Session) onTurnComplete() {
	// A fragment that arrived after the model started answering has nowhere
	// else to go.
	s.flushInputTranscript()
	s.detector.arm()
}

// settlePlayback decides whether the caller is still being spoken to.
//
// A turn that produced audio is still arriving at the caller's ear long after
// the server finished it — the downlink runs about three times faster than real
// time — so listening for speech has to wait for the server to say the turn is
// over. A turn that produced nothing has nothing playing, and waiting would
// leave the caller unheard for as long as the model takes to come back.
func (s *Session) settlePlayback() {
	s.mu.Lock()
	audioBytes := s.turnAudioBytes
	s.mu.Unlock()

	if audioBytes == 0 {
		s.detector.arm()
	}
}

// onToolCalls is the model calling functions — and, as far as the call is
// concerned, spending a whole turn to do it.
//
// No frame brackets a tool call on this protocol: the calls simply arrive, and
// the model then says nothing at all until it is answered. The turn is
// synthesised around them because the call's dead-air timer is armed in exactly
// one place, after a turn ends, and nothing else would arm it; the speech that
// follows the answer is a new turn, which is what the server does too.
func (s *Session) onToolCalls(event *toolCall) {
	if len(event.FunctionCalls) == 0 {
		s.log.Debug("a tool call arrived with no calls in it")
		return
	}

	s.beginTurn()

	order := make([]string, 0, len(event.FunctionCalls))
	pending := make(map[string]bool, len(event.FunctionCalls))
	names := make(map[string]string, len(event.FunctionCalls))
	for _, call := range event.FunctionCalls {
		order = append(order, call.ID)
		pending[call.ID] = true
		names[call.ID] = call.Name
	}
	s.mu.Lock()
	s.hasToolCall = true
	s.toolOrder = order
	s.toolPending = pending
	s.toolNames = names
	s.toolResults = make(map[string]json.RawMessage, len(order))
	s.mu.Unlock()

	for _, call := range event.FunctionCalls {
		arguments := string(call.Args)
		if arguments == "" || arguments == "null" {
			arguments = "{}"
		}
		s.emit(provider.Event{
			Type: provider.EventTypeToolCall, ToolCallID: call.ID,
			ToolName: call.Name, ToolArgs: arguments,
		})
	}

	s.isTurnOpen.Store(false)
	s.dog.Signal(provider.WatchResponseEnded)
	// Nothing is playing while the model waits for an answer, so the caller can
	// be listened to.
	s.detector.arm()
	s.emit(provider.Event{Type: provider.EventTypeResponseDone})
}

// onToolCallsWithdrawn is the server saying it discarded calls it had made.
// Documented, never observed; answering a call it has forgotten achieves
// nothing and the answer would be ignored.
func (s *Session) onToolCallsWithdrawn(event *toolCallCancellation) {
	s.log.Debug("the provider withdrew tool calls", "count", len(event.IDs))
	s.withdrawToolCalls("the provider withdrew them")
}

// withdrawToolCalls forgets everything the model was waiting on. A result that
// arrives afterwards is answered with nothing at all.
func (s *Session) withdrawToolCalls(reason string) {
	s.mu.Lock()
	hadPending := len(s.toolPending) > 0
	s.toolOrder = nil
	s.toolPending = nil
	s.toolNames = nil
	s.toolResults = nil
	s.mu.Unlock()

	if hadPending {
		s.log.Debug("tool calls are no longer answerable", "reason", reason)
	}
}

// onGoAway is the connection's lifetime running out with the caller still on
// the line.
//
// It is fatal at once rather than when the deadline passes: the call has to be
// routed somewhere a person can take it, and every second spent waiting for a
// connection that is going to close is a second of that transfer not happening.
// Nothing is broken — not the network, not the credential, not the model — so it
// has a failure cause and an outcome word of its own.
func (s *Session) onGoAway(event *goAway) {
	s.log.Warn("the provider is about to end the connection", "timeLeft", event.TimeLeft)
	s.setOutcome(outcomeSessionExpired)
	s.beginStop()

	const reason = "the provider is ending the session"
	s.emit(provider.Event{
		Type: provider.EventTypeError, Err: errors.New(reason), Text: reason,
		IsFatal: true, FailureCause: provider.FailureCauseSessionExpired,
	})
}

// onTurnStalled closes out a turn the provider walked away from, or an answer
// to a tool result that never came. The watchdog calls it on its own goroutine,
// having already decided the model owes the caller one of the two.
//
// The tool case has no turn to close — the tool call's own turn ended when the
// calls were handed over — so the turn the model owed is the turn that is
// reported, started and stalled in one breath. That keeps the invariant every
// consumer here relies on: one RESPONSE_STARTED, one ending, never an ending
// with no beginning.
func (s *Session) onTurnStalled(hasAudioArrived bool) {
	wasAnswerOwed := s.isAnswerOwed.Swap(false)
	wasTurnOpen := s.isTurnOpen.Swap(false)
	if !wasTurnOpen && !wasAnswerOwed {
		// The turn ended between the watchdog deciding and this running. One
		// turn gets one ending, and it has had it.
		s.log.Debug("a turn ended while it was being abandoned")
		return
	}

	reason := "the provider never started speaking"
	switch {
	case hasAudioArrived:
		reason = "the provider stopped partway through speaking"
	case !wasTurnOpen:
		reason = "the provider never answered a tool result"
	}
	s.log.Warn("turn abandoned", "reason", reason, "hasAudioArrived", hasAudioArrived)
	if !wasTurnOpen {
		s.emit(provider.Event{Type: provider.EventTypeResponseStarted})
	}
	s.flushOutputTranscript()
	// The server is not going to say this turn is over, so nothing is coming
	// that would start the caller being listened to again.
	s.detector.arm()

	// Not fatal: the session is still usable and the caller heard whatever did
	// arrive. The flow decides what to say next.
	s.emit(provider.Event{Type: provider.EventTypeError,
		Text: reason, Err: errors.New(reason)})
	s.emit(provider.Event{Type: provider.EventTypeResponseDone,
		Status: provider.StatusStalled})
}

//
// Emitting.
//

// emit delivers an event, dropping it only once the session is finished and
// there is nowhere left to put it.
//
// The room in the channel is tried first, on its own. The last events of a
// session — the failure that ended it, and the CLOSED that follows — are
// produced after the socket has been closed, so a single select between the
// channel and the connection being over would choose between two ready cases at
// random and lose about half of them. The consumer is still there and the
// channel still has room; what the connection says is only that nobody is
// obliged to read any more.
func (s *Session) emit(event provider.Event) {
	s.emitMu.RLock()
	defer s.emitMu.RUnlock()
	s.emitLocked(event)
}

// offer delivers an event from a goroutine that must not be made to wait: the
// speech detector runs on the call's own media path, where blocking costs audio
// in both directions.
//
// So it never blocks, on the lock or on the channel, and an event it cannot
// place is dropped. The lock it tries is the shared one, which another emitter
// never holds against it; it fails only while the stream is being closed. What
// it carries — the caller has started or stopped talking — is worth having and
// is not worth a stalled call: it cancels a dead-air timer and starts a
// stopwatch, and both of those are already wrong when the consumer is 256
// events behind.
func (s *Session) offer(event provider.Event) {
	if !s.emitMu.TryRLock() {
		return
	}
	defer s.emitMu.RUnlock()
	if s.isEventsClosed {
		return
	}
	select {
	case s.events <- event:
	default:
	}
}

// finish says the session is over and closes the stream, in that order and
// under one lock. That is what makes CLOSED the last event rather than usually
// the last event: nothing can be placed between the two, and nothing can be
// placed on a channel that is already closed.
func (s *Session) finish() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()

	s.emitLocked(provider.Event{Type: provider.EventTypeClosed})
	s.isEventsClosed = true
	close(s.events)
}

func (s *Session) emitLocked(event provider.Event) {
	if s.isEventsClosed {
		return
	}
	select {
	case s.events <- event:
		return
	default:
	}
	if s.conn == nil {
		return
	}
	select {
	case s.events <- event:
	case <-s.conn.Done():
		// The session is over and the consumer has stopped keeping up.
	}
}

//
// Speech, heard here rather than reported by the provider.
//

// listenForSpeech runs the detector over one frame of caller audio and reports
// what it decided. It is called from SendAudio, on the call's own goroutine.
func (s *Session) listenForSpeech(audio []byte) {
	if s.detector == nil || s.hasStopBegun() {
		return
	}
	switch s.detector.observe(audio) {
	case speechBegan:
		s.offer(provider.Event{Type: provider.EventTypeSpeechStarted})
	case speechEnded:
		s.offer(provider.Event{Type: provider.EventTypeSpeechStopped})
	}
}

// asJSONValue is a tool result in the form this protocol's output field takes.
//
// A result that is JSON goes in as what it is — an object the model can read
// fields out of — and anything else goes in as a string, which is still an
// answer. The hint has already been merged into it by then, because on this
// model the hint is how a phase change reaches the conversation at all.
func asJSONValue(value string) json.RawMessage {
	if json.Valid([]byte(value)) && strings.TrimSpace(value) != "" {
		return json.RawMessage(value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return encoded
}
