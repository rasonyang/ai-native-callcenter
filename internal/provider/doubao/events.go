// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// readLoop is the only goroutine that reads the socket, and the only one that
// emits. Everything a caller-side method decides is recorded under the mutex
// and reported from here: those methods are called from the goroutine draining
// Events, and emitting from one would deadlock the call.
func (s *Session) readLoop() {
	// readDone closes before the channel does, so a Close called from the
	// consumer's own goroutine never waits on itself.
	defer close(s.events)
	defer close(s.readDone)

	for {
		event, raw, err := s.receive()
		if err != nil {
			// The outcome is recorded before Start is released, because
			// finishStart hands control back to the caller: one that gets it
			// first can Close the session and stamp a clean outcome over the
			// failure that ended the socket, and the first outcome wins.
			s.reportLostConnection(err)
			s.finishStart(err)
			s.emit(provider.Event{Type: provider.EventTypeClosed})
			return
		}
		if event == nil {
			s.log.Debug("ignored non-text frame", "bytes", len(raw))
			continue
		}
		s.handle(event)
	}
}

// reportLostConnection fails the call for a socket that ended by itself.
//
// There is no reconnect: the provider holds conversation state that cannot be
// rebuilt, so a lost socket ends the session and the call is routed somewhere a
// person can take it. A session that was already ending, or that has already
// been failed for a reason of its own, says nothing here — this provider drops
// the socket in the same millisecond as an error frame, and one broken session
// is one failure.
func (s *Session) reportLostConnection(err error) {
	if s.hasStopBegun() || s.outcome() != "" || isNormalClosure(err) {
		return
	}
	s.setOutcome(outcomeConnectionLost)
	s.emit(provider.Event{
		Type: provider.EventTypeError, Err: err, IsFatal: true,
		Text: "provider connection lost",
	})
}

// handle is the whole mapping from this protocol to the call's vocabulary.
func (s *Session) handle(event *wireEvent) {
	switch event.Type {
	case "session.created":
		// The one gate: nothing may be sent before this arrives.
		s.finishStart(nil)
		s.emit(provider.Event{Type: provider.EventTypeSessionReady})

	// The caller. There is no speech-detection event on this protocol — the
	// transcript starting IS the caller starting, and its completion is the
	// engine deciding they have stopped.
	case "conversation.item.input_audio_transcription.started":
		s.emit(provider.Event{Type: provider.EventTypeSpeechStarted})

	case "conversation.item.input_audio_transcription.delta":
		// Cumulative: each delta is the whole utterance so far, not the next
		// few characters of it.
		s.emit(provider.Event{Type: provider.EventTypeInputTranscript, Text: event.Delta})

	case "conversation.item.input_audio_transcription.completed":
		s.emit(provider.Event{
			Type: provider.EventTypeInputTranscript, Text: event.Text, IsFinal: true})
		s.emit(provider.Event{Type: provider.EventTypeSpeechStopped})

	// The model.
	case "response.output_audio.started":
		s.onTurnStarted(event)

	case "response.output_audio.delta":
		s.onAudioDelta(event)

	case "response.output_text.delta":
		s.emit(provider.Event{Type: provider.EventTypeOutputTranscript, Text: event.Delta})

	case "response.output_text.done":
		s.emit(provider.Event{
			Type: provider.EventTypeOutputTranscript, Text: event.Text, IsFinal: true})

	case "response.output_audio.done":
		s.onTurnFinished()

	case "response.done":
		s.onWireResponseDone(event)

	case "response.function_call_arguments.done":
		s.onToolCalls(event)

	// Acknowledgements. They confirm a frame was accepted and say nothing
	// about the conversation.
	case "response.canceled", "session.updated",
		"input_audio_buffer.committed", "conversation.item.added":
		s.log.Debug("provider acknowledgement", "type", event.Type)

	case "session.closed":
		s.onSessionClosed()

	case "error":
		s.onError(event)

	default:
		s.log.Debug("unmapped provider event", "type", event.Type)
	}
}

// onTurnStarted is a turn beginning, and the only place a turn is armed.
//
// It is also where a turn that was pre-empted is closed out. A line handed to
// the engine replaces whatever is being said, and the response it replaced gets
// no terminal event of ANY kind (measured): if the old turn were not ended
// here it would stay open for the rest of the call, and every later turn would
// be reported as an interruption of it.
func (s *Session) onTurnStarted(event *wireEvent) {
	s.log.Debug("a turn began", "ttsType", event.TTSType, "responseId", event.ResponseID)

	if s.isTurnOpen.Load() {
		s.closeTurn()
		s.emit(provider.Event{
			Type: provider.EventTypeInterrupted, InterruptedBy: provider.InterruptReasonSystem})
	}

	s.mu.Lock()
	s.isFenced = false
	s.isCancelledByUs = false
	s.turnAudioBytes = 0
	spoken := ""
	if event.TTSType == ttsTypeSpokenLine {
		spoken = s.pendingSpeak
		s.pendingSpeak = ""
	}
	s.mu.Unlock()

	s.isTurnOpen.Store(true)
	s.dog.Signal(provider.WatchResponseStarted)
	s.emit(provider.Event{Type: provider.EventTypeResponseStarted})

	// The words of a line this client handed the engine are this client's to
	// report: the provider sends no text events for one.
	if spoken != "" {
		s.emit(provider.Event{
			Type: provider.EventTypeOutputTranscript, Text: spoken, IsFinal: true})
	}
}

// onAudioDelta is the model speaking.
func (s *Session) onAudioDelta(event *wireEvent) {
	if !s.isTurnOpen.Load() {
		// Audio belonging to a turn that has already ended. The caller has
		// stopped hearing it, and playing it would be the tail of an answer
		// they interrupted.
		s.log.Debug("audio arrived with no turn open")
		return
	}
	s.mu.Lock()
	isFenced := s.isFenced
	s.mu.Unlock()
	if isFenced {
		// A cancelled turn goes on producing for up to half a second.
		s.log.Debug("audio arrived for a turn that was cancelled")
		return
	}

	audio, err := base64.StdEncoding.AppendDecode(nil, []byte(event.Delta))
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

// onTurnFinished is a turn ending because it finished.
//
// A turn that finished without carrying a single byte of audio is fatal, and
// deliberately so: this provider accepts a voice it does not have without a
// word of complaint and then says nothing (measured), and the audio format this
// client asks for is undocumented. Both failures look exactly like this, and
// both leave the caller listening to silence, which is worse than a call routed
// to a person.
func (s *Session) onTurnFinished() {
	wasOpen := s.isTurnOpen.Swap(false)
	s.dog.Signal(provider.WatchResponseEnded)

	s.mu.Lock()
	audioBytes := s.turnAudioBytes
	s.mu.Unlock()

	if !wasOpen {
		s.log.Debug("a turn finished that was already closed")
		return
	}
	if audioBytes == 0 {
		reason := "the provider produced no audio; check the voice name"
		s.log.Error("turn carried no audio", "reason", reason)
		s.emit(provider.Event{Type: provider.EventTypeError,
			Err: errors.New(reason), Text: reason, IsFatal: true})
		return
	}
	s.emit(provider.Event{Type: provider.EventTypeResponseDone})
}

// onWireResponseDone is this protocol's response.done, which is not a turn
// ending.
//
// On a turn that ran to completion it trails the audio by four to ten seconds
// and carries only what the turn cost, so it is logged and nothing more: put on
// a RESPONSE_DONE it would arrive long after the call had moved on, and the
// usage would be attributed to whatever turn was current by then.
//
// With a turn still open it is the other thing it means: the turn was cut
// short. The caller talking over the model produces exactly this and nothing
// else — no output_audio.done, a bare response.done in the same millisecond as
// the next transcript starting (measured). Who cut it short has to be said,
// because a turn this client stopped is not a barge-in and logging it as one
// puts an interruption the caller never made into the record of the call.
func (s *Session) onWireResponseDone(event *wireEvent) {
	if usage := usageOf(event); usage != (provider.Usage{}) {
		s.log.Info("turn usage", "totalTokens", usage.TotalTokens,
			"inputTokens", usage.InputTokens, "outputTokens", usage.OutputTokens)
	}
	if !s.isTurnOpen.Load() {
		return
	}

	s.mu.Lock()
	by := provider.InterruptReasonSpeech
	if s.isCancelledByUs {
		by = provider.InterruptReasonSystem
	}
	s.mu.Unlock()

	s.closeTurn()
	s.emit(provider.Event{Type: provider.EventTypeInterrupted, InterruptedBy: by})
}

// onToolCalls is the model calling functions — and, as far as the call is
// concerned, taking a whole turn to do it.
//
// No response event brackets a tool call on this protocol: the calls simply
// arrive. The turn is synthesised here because the call's dead-air timer is
// armed in exactly one place, after a turn ends, and nothing else would arm it:
// a model that goes quiet after a tool result would leave the call in silence
// with no timer running and nobody to notice.
func (s *Session) onToolCalls(event *wireEvent) {
	if len(event.Items) == 0 {
		s.log.Debug("a tool call arrived with no items")
		return
	}

	order := make([]string, 0, len(event.Items))
	pending := make(map[string]bool, len(event.Items))
	for _, item := range event.Items {
		order = append(order, item.CallID)
		pending[item.CallID] = true
	}
	s.mu.Lock()
	s.toolOrder = order
	s.toolPending = pending
	s.toolResults = make(map[string]string, len(order))
	s.mu.Unlock()

	s.emit(provider.Event{Type: provider.EventTypeResponseStarted})
	for _, item := range event.Items {
		arguments := item.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		s.emit(provider.Event{
			Type: provider.EventTypeToolCall, ToolCallID: item.CallID,
			ToolName: item.Name, ToolArgs: arguments,
		})
	}
	s.emit(provider.Event{Type: provider.EventTypeResponseDone})
}

// onSessionClosed is the provider confirming the session has ended — or
// ending it on its own, which is a failure: the conversation cannot be
// rebuilt, so the call has to go somewhere a person can take it.
func (s *Session) onSessionClosed() {
	s.closedOnce.Do(func() { close(s.closed) })

	if s.hasStopBegun() {
		return
	}
	s.setOutcome(outcomeClosedByServer)
	s.beginStop()
	const reason = "the provider ended the session"
	s.emit(provider.Event{Type: provider.EventTypeError,
		Err: errors.New(reason), Text: reason, IsFatal: true})
}

// onError is the provider refusing something.
//
// Before the session is ending it is fatal: there is no recoverable error on
// this protocol — the socket is dropped in the same millisecond — and 4xx and
// 5xx alike end in the call being routed elsewhere. The classification is for
// the log line, where it is the difference between something to fix here and
// something to report to the vendor.
//
// After stopping has begun it is logged and nothing more: a perfectly clean
// close comes with error frames of its own ("the stream is done", "no session
// active"), measured, and failing a call that had already finished would be a
// fault invented by this client.
func (s *Session) onError(event *wireEvent) {
	err := event.Error
	if err == nil {
		err = &wireError{Message: "the provider reported an unspecified error"}
	}

	if s.hasStopBegun() {
		s.log.Debug("provider error after the session began ending",
			"code", err.Code, "message", err.Message)
		return
	}

	if err.Code == codeIdleRelease {
		s.log.Warn("idle release (no interaction for 10 minutes)",
			"code", err.Code, "message", err.Message)
	} else {
		s.log.Error("provider error", "kind", err.kind(),
			"code", err.Code, "message", err.Message)
	}

	s.setOutcome(failedOutcome(err.Code))
	s.finishStart(err)
	s.emit(provider.Event{
		Type: provider.EventTypeError, Err: err, Text: err.Message, IsFatal: true})
}

// closeTurn ends the turn without saying how it ended; the caller says that.
func (s *Session) closeTurn() {
	s.isTurnOpen.Store(false)
	s.dog.Signal(provider.WatchResponseEnded)
}

// onTurnStalled closes out a turn the provider walked away from. The watchdog
// calls it on its own goroutine, having already decided a turn really is open.
func (s *Session) onTurnStalled(hasAudioArrived bool) {
	reason := "the provider never started speaking"
	if hasAudioArrived {
		reason = "the provider stopped partway through speaking"
	}
	s.isTurnOpen.Store(false)
	s.log.Warn("turn abandoned", "reason", reason, "hasAudioArrived", hasAudioArrived)

	// Not fatal: the session is still usable and the caller heard whatever did
	// arrive. The flow decides what to say next.
	s.emit(provider.Event{Type: provider.EventTypeError,
		Text: reason, Err: errors.New(reason)})
	s.emit(provider.Event{Type: provider.EventTypeResponseDone,
		Status: provider.StatusStalled})
}

func usageOf(event *wireEvent) provider.Usage {
	if event.Response == nil || event.Response.Usage == nil {
		return provider.Usage{}
	}
	usage := event.Response.Usage
	return provider.Usage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
	}
}

// emit delivers an event, dropping it only once the session is finished and
// there is nowhere left to put it.
//
// The room in the channel is tried first, on its own. The last events of a
// session — the failure that ended it, and the CLOSED that follows — are
// produced after the socket has been closed, so a single select between the
// channel and the connection being over would choose between two ready cases
// at random and lose about half of them. The consumer is still there and the
// channel still has room; what the connection says is only that nobody is
// obliged to read any more.
func (s *Session) emit(event provider.Event) {
	select {
	case s.events <- event:
		return
	default:
	}
	select {
	case s.events <- event:
	case <-s.conn.Done():
		// The session is over and the consumer has stopped keeping up.
	}
}

func isNormalClosure(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
}
