// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Watchdogs on a response. A provider that accepts a turn and then goes quiet
// is indistinguishable from a broken call to the person listening.
const (
	// firstAudioTimeout bounds the wait between asking for a response and
	// hearing the first of it.
	firstAudioTimeout = 3 * time.Second
	// deltaStallTimeout bounds a gap in the middle of a response. Audio has
	// already arrived, so the right answer is to finish with what there is.
	deltaStallTimeout = 2 * time.Second
	// eventBuffer is how far the consumer may fall behind. The consumer is a
	// per-call actor forwarding to a paced send queue, so it should never come
	// close.
	eventBuffer = 256
)

// Realtime is a session against any provider speaking the realtime protocol.
// Vendor differences live entirely in its Profile.
type Realtime struct {
	profile Profile
	apiKey  string
	log     *slog.Logger

	events chan Event
	conn   *transport

	// ready closes when the provider has accepted the session configuration.
	ready     chan struct{}
	readyOnce sync.Once
	// startErr records why the handshake failed, if it did.
	startErr atomic[error]

	closeOnce sync.Once

	mu sync.Mutex
	// cfg is fixed at Start. Some providers freeze turn detection after the
	// first audio frame, so nothing here is adjusted mid-call except the
	// instructions.
	cfg          SessionConfig
	instructions string
	// responseItemID identifies the assistant's current audio item, which is
	// what truncation refers to.
	responseItemID string
	// isSessionRetried guards the one-shot retry of a rejected configuration.
	isSessionRetried bool

	// watch carries response-progress signals to the watchdog.
	watch chan watchSignal
	// Watchdog deadlines, held as fields so tests need not wait seconds for
	// behaviour that is measured in seconds on a real call.
	firstAudioDeadline time.Duration
	deltaStallDeadline time.Duration
}

// watchSignal tells the watchdog where a response has got to.
type watchSignal uint8

const (
	watchResponseStarted watchSignal = iota
	watchAudioArrived
	watchResponseEnded
)

// atomic is a tiny typed holder; sync/atomic's generic Pointer would need a
// pointer indirection for an interface value.
type atomic[T any] struct {
	mu sync.Mutex
	v  T
}

func (a *atomic[T]) set(v T) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomic[T]) get() T  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// New builds a session for a profile. The credential is read from the
// environment named by the profile, so a missing key fails here rather than
// mid-call.
func New(profile Profile, log *slog.Logger) (*Realtime, error) {
	apiKey := os.Getenv(profile.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("provider %s: %s is not set", profile.Name, profile.APIKeyEnv)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Realtime{
		profile: profile,
		apiKey:  apiKey,
		log:     log.With("provider", profile.Name, "model", profile.Model),
		events:  make(chan Event, eventBuffer),
		ready:   make(chan struct{}),
		watch:   make(chan watchSignal, 16),

		firstAudioDeadline: firstAudioTimeout,
		deltaStallDeadline: deltaStallTimeout,
	}, nil
}

// Profile reports the vendor configuration in use.
func (r *Realtime) Profile() Profile { return r.profile }

func (r *Realtime) Events() <-chan Event { return r.events }

// Start connects, configures the session, and asks for the opening turn.
func (r *Realtime) Start(ctx context.Context, cfg SessionConfig) error {
	r.mu.Lock()
	r.cfg = cfg
	r.instructions = cfg.Instructions
	r.mu.Unlock()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+r.apiKey)
	for key, value := range r.profile.Headers {
		headers.Set(key, value)
	}

	conn, err := dial(ctx, r.profile.endpointURL(), headers, r.log)
	if err != nil {
		return err
	}
	r.conn = conn
	go r.readLoop()
	go r.watchdog()

	if err := r.conn.send(r.buildSessionUpdate(cfg, false)); err != nil {
		r.conn.close()
		return fmt.Errorf("configure session: %w", err)
	}

	// The configuration must be accepted before any audio is sent, because on
	// at least one provider the turn-detection settings freeze at the first
	// frame.
	select {
	case <-r.ready:
	case <-ctx.Done():
		r.conn.close()
		return ctx.Err()
	case <-time.After(dialTimeout):
		r.conn.close()
		if err := r.startErr.get(); err != nil {
			return fmt.Errorf("session rejected: %w", err)
		}
		return errors.New("provider did not confirm the session configuration")
	}
	if err := r.startErr.get(); err != nil {
		r.conn.close()
		return fmt.Errorf("session rejected: %w", err)
	}

	// The opening turn is the flow's first node speaking; the caller is
	// already on the line waiting to be greeted.
	if r.profile.NeedsCueForFirstTurn {
		if err := r.conn.send(map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{
				"type": "message", "role": "user",
				"content": []map[string]any{{"type": "input_text", "text": greetingCue(cfg)}},
			},
		}); err != nil {
			r.conn.close()
			return fmt.Errorf("prompt opening turn: %w", err)
		}
	}
	if err := r.conn.send(map[string]any{"type": "response.create"}); err != nil {
		r.conn.close()
		return fmt.Errorf("request opening turn: %w", err)
	}
	return nil
}

// greetingCue is the stage direction that gets a provider talking first.
func greetingCue(cfg SessionConfig) string {
	if cfg.GreetingCue != "" {
		return cfg.GreetingCue
	}
	if strings.HasPrefix(strings.ToLower(cfg.Language), "zh") {
		return "（电话已接通，请按照你的指示开始问候来电者。）"
	}
	return "(The call has connected. Greet the caller as instructed.)"
}

// SendAudio forwards one chunk of caller audio.
//
// The JSON is assembled directly around the base64 rather than marshalled from
// a struct: this runs fifty times a second per call in each direction, and
// marshalling would copy every frame twice more for no benefit. Base64's
// alphabet needs no JSON escaping, so this is safe as well as cheap.
func (r *Realtime) SendAudio(audio []byte) error {
	const prefix = `{"type":"input_audio_buffer.append","audio":"`

	message := make([]byte, 0, len(prefix)+base64.StdEncoding.EncodedLen(len(audio))+2)
	message = append(message, prefix...)
	message = base64.StdEncoding.AppendEncode(message, audio)
	message = append(message, '"', '}')

	return r.conn.sendRaw(message)
}

// SendToolResult answers a tool call and steers what happens next.
//
// The hint travels inside the result rather than as a separate instruction
// update: the model reads it as part of what it just learned, which is what
// makes it act on it immediately instead of at some later turn.
func (r *Realtime) SendToolResult(toolCallID, output, hint string) error {
	if err := r.conn.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": toolCallID,
			"output":  mergeHint(output, hint),
		},
	}); err != nil {
		return err
	}
	return r.conn.send(map[string]any{"type": "response.create"})
}

// mergeHint folds steering into a tool result. A result that is already a JSON
// object gains a field; anything else is wrapped so the shape stays predictable.
func mergeHint(output, hint string) string {
	if hint == "" {
		return output
	}
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &asObject); err == nil && asObject != nil {
		hintJSON, _ := json.Marshal(hint)
		asObject["hint"] = hintJSON
		if merged, err := json.Marshal(asObject); err == nil {
			return string(merged)
		}
	}
	merged, err := json.Marshal(map[string]string{"result": output, "hint": hint})
	if err != nil {
		return output
	}
	return string(merged)
}

// UpdateInstructions replaces the standing instructions.
//
// Only the instructions are sent. Re-sending the whole configuration would
// re-assert turn detection, which at least one provider treats as an error
// once audio has started flowing.
func (r *Realtime) UpdateInstructions(text string) error {
	r.mu.Lock()
	r.instructions = text
	r.mu.Unlock()

	session := map[string]any{"instructions": text}
	if r.profile.Style == styleGA {
		session["type"] = "realtime"
	}
	return r.conn.send(map[string]any{"type": "session.update", "session": session})
}

// Interrupt stops the model talking over the caller.
//
// Which side is responsible differs by vendor: one cancels on its own as soon
// as it hears speech and only needs to be told how much was actually heard;
// the other does nothing until asked. Both are normalised here so the call
// only ever sees a single interruption.
func (r *Realtime) Interrupt(reason InterruptReason, playedMs int) error {
	r.mu.Lock()
	itemID := r.responseItemID
	r.mu.Unlock()

	if !r.profile.CancelsResponseItself {
		if err := r.conn.send(map[string]any{"type": "response.cancel"}); err != nil {
			return err
		}
	}

	// Telling the provider how much was heard keeps its conversation history
	// honest: without it the model believes the caller heard a sentence that
	// was cut off after three words.
	if itemID != "" && playedMs > 0 {
		if err := r.conn.send(map[string]any{
			"type":          "conversation.item.truncate",
			"item_id":       itemID,
			"content_index": 0,
			"audio_end_ms":  playedMs,
		}); err != nil {
			return err
		}
	}

	r.emit(Event{Type: EventTypeInterrupted, InterruptedBy: reason})
	return nil
}

// Close ends the session.
func (r *Realtime) Close(_ context.Context) error {
	r.closeOnce.Do(func() {
		if r.conn != nil {
			r.conn.close()
		}
	})
	return nil
}

//
// Session payloads.
//

// buildSessionUpdate assembles the configuration in this profile's dialect.
// isReduced drops the optional fields, for the one retry after a rejection.
func (r *Realtime) buildSessionUpdate(cfg SessionConfig, isReduced bool) map[string]any {
	session := map[string]any{"instructions": cfg.Instructions}

	if tools := r.buildTools(cfg.Tools); len(tools) > 0 {
		session["tools"] = tools
	}
	voice := cfg.Voice
	if voice == "" {
		voice = r.profile.Voice
	}

	if r.profile.Style == styleGA {
		inputName, inputRate := r.profile.formatName(cfg.InputFormat)
		outputName, outputRate := r.profile.formatName(cfg.OutputFormat)

		input := map[string]any{
			"format":         audioFormatPayload(inputName, inputRate),
			"turn_detection": r.buildTurnDetection(cfg.Turn),
		}
		if r.profile.TranscribeModel != "" && !isReduced {
			input["transcription"] = map[string]any{"model": r.profile.TranscribeModel}
		}
		output := map[string]any{"format": audioFormatPayload(outputName, outputRate)}
		if voice != "" {
			output["voice"] = voice
		}

		session["type"] = "realtime"
		session["audio"] = map[string]any{"input": input, "output": output}
		return map[string]any{"type": "session.update", "session": session}
	}

	inputName, _ := r.profile.formatName(cfg.InputFormat)
	outputName, _ := r.profile.formatName(cfg.OutputFormat)
	session["modalities"] = []string{"text", "audio"}
	session["input_audio_format"] = inputName
	session["output_audio_format"] = outputName
	session["turn_detection"] = r.buildTurnDetection(cfg.Turn)
	if voice != "" {
		session["voice"] = voice
	}
	return map[string]any{"type": "session.update", "session": session}
}

func audioFormatPayload(name string, rateHz int) map[string]any {
	format := map[string]any{"type": name}
	if rateHz > 0 {
		format["rate"] = rateHz
	}
	return format
}

// buildTurnDetection maps the declarative setting onto the vendor's names.
func (r *Realtime) buildTurnDetection(turn TurnDetection) any {
	switch turn.Mode {
	case TurnModeNone:
		return nil
	case TurnModeSemantic:
		detection := map[string]any{"type": r.profile.SemanticTurnType}
		// Where the vendor forces its own hold, sending a different one would
		// only make the configuration lie about what the call will do.
		if r.profile.SemanticTurnSilenceMs == 0 && turn.SilenceMs > 0 {
			detection["silence_duration_ms"] = turn.SilenceMs
		}
		return detection
	default:
		detection := map[string]any{"type": "server_vad"}
		if turn.SilenceMs > 0 {
			detection["silence_duration_ms"] = turn.SilenceMs
		}
		if turn.Threshold > 0 {
			detection["threshold"] = turn.Threshold
		}
		return detection
	}
}

// buildTools renders tool specifications in the flat shape both providers were
// verified to accept.
func (r *Realtime) buildTools(tools []ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := json.RawMessage(`{"type":"object","properties":{}}`)
		if len(tool.Parameters) > 0 {
			parameters = tool.Parameters
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  parameters,
		})
	}
	return out
}

//
// Inbound events.
//

func (r *Realtime) readLoop() {
	defer close(r.events)

	for {
		event, raw, err := r.conn.receive()
		if err != nil {
			// There is no reconnect: the provider holds conversation state
			// that cannot be rebuilt, so a lost socket ends the session and
			// the call is routed somewhere a person can take it.
			r.finishStart(err)
			if !isNormalClosure(err) {
				r.emit(Event{
					Type: EventTypeError, Err: err, IsFatal: true,
					Text: "provider connection lost",
				})
			}
			r.emit(Event{Type: EventTypeClosed})
			return
		}
		if event == nil {
			r.log.Debug("ignored non-text frame", "bytes", len(raw))
			continue
		}
		r.handle(event)
	}
}

func (r *Realtime) handle(event *wireEvent) {
	switch event.Type {
	case "session.created":
		// Informational: the provider's defaults, before ours are applied.
		return

	case "session.updated":
		r.finishStart(nil)
		r.emit(Event{Type: EventTypeSessionReady})

	case "input_audio_buffer.speech_started":
		r.emit(Event{Type: EventTypeSpeechStarted})

	case "input_audio_buffer.speech_stopped":
		r.emit(Event{Type: EventTypeSpeechStopped})

	case "response.created":
		r.signal(watchResponseStarted)
		r.emit(Event{Type: EventTypeResponseStarted})

	case "response.output_item.added":
		if event.Item != nil && event.Item.ID != "" {
			r.mu.Lock()
			r.responseItemID = event.Item.ID
			r.mu.Unlock()
		}

	// Both the current and the older names for model audio.
	case "response.audio.delta", "response.output_audio.delta":
		audio, err := base64.StdEncoding.AppendDecode(nil, []byte(event.Delta))
		if err != nil {
			r.emit(Event{Type: EventTypeError, Err: fmt.Errorf("undecodable audio: %w", err)})
			return
		}
		r.signal(watchAudioArrived)
		r.emit(Event{Type: EventTypeAudioDelta, Audio: audio})

	case "response.audio_transcript.delta", "response.output_audio_transcript.delta":
		r.emit(Event{Type: EventTypeOutputTranscript, Text: event.Delta})

	case "response.audio_transcript.done", "response.output_audio_transcript.done":
		r.emit(Event{Type: EventTypeOutputTranscript, Text: event.Transcript, IsFinal: true})

	case "conversation.item.input_audio_transcription.delta":
		r.emit(Event{Type: EventTypeInputTranscript, Text: event.Delta})

	case "conversation.item.input_audio_transcription.completed":
		r.emit(Event{Type: EventTypeInputTranscript, Text: event.Transcript, IsFinal: true})

	case "response.function_call_arguments.done":
		arguments := event.Arguments
		if arguments == "" {
			arguments = "{}"
		}
		r.emit(Event{
			Type: EventTypeToolCall, ToolCallID: event.CallID,
			ToolName: event.Name, ToolArgs: arguments,
		})

	case "response.done":
		r.handleResponseDone(event)

	case "error":
		r.handleError(event)

	default:
		r.log.Debug("unmapped provider event", "type", event.Type)
	}
}

func (r *Realtime) handleResponseDone(event *wireEvent) {
	r.signal(watchResponseEnded)

	out := Event{Type: EventTypeResponseDone}
	if event.Response != nil {
		out.Status = event.Response.Status
		if usage := event.Response.Usage; usage != nil {
			out.Usage = Usage{
				InputTokens:  usage.InputTokens,
				OutputTokens: usage.OutputTokens,
				TotalTokens:  usage.TotalTokens,
			}
		}
	}
	// A cancelled response is the tail of a barge-in that was already
	// reported, so it is not surfaced as a second interruption.
	r.emit(out)
}

func (r *Realtime) handleError(event *wireEvent) {
	if event.Error == nil {
		r.emit(Event{Type: EventTypeError, Err: errors.New("provider reported an unspecified error")})
		return
	}
	err := event.Error

	// A rejected configuration takes the whole session with it — persona,
	// tools and all — so it is worth one retry without the optional parts
	// before giving up on the call.
	if err.Param == "session.update" && r.canRetrySession() {
		r.log.Warn("session configuration rejected, retrying without optional fields",
			"code", err.Code, "message", err.Message)
		r.mu.Lock()
		cfg := r.cfg
		r.mu.Unlock()
		if sendErr := r.conn.send(r.buildSessionUpdate(cfg, true)); sendErr == nil {
			return
		}
	}

	r.finishStart(err)
	r.emit(Event{Type: EventTypeError, Err: err, Text: err.Message})
}

// canRetrySession allows exactly one reduced retry, before the session is up.
func (r *Realtime) canRetrySession() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.ready:
		return false
	default:
	}
	if r.isSessionRetried {
		return false
	}
	r.isSessionRetried = true
	return true
}

// finishStart releases Start, recording the first failure if there was one.
func (r *Realtime) finishStart(err error) {
	r.readyOnce.Do(func() {
		if err != nil {
			r.startErr.set(err)
		}
		close(r.ready)
	})
}

// StatusStalled is the status on a response the provider abandoned partway.
// It is synthesised here, not reported by any provider.
const StatusStalled = "STALLED"

// watchdog ends a turn the provider has silently abandoned.
//
// A model that accepts a turn and then stops is indistinguishable, to the
// person on the phone, from a call that has died — and the flow engine would
// wait for a completion that is never coming. Rather than hang, the turn is
// closed out with what actually arrived.
func (r *Realtime) watchdog() {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	hasAudioArrived := false
	isWaiting := false

	arm := func(d time.Duration) {
		if isWaiting && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
		isWaiting = true
	}
	disarm := func() {
		if isWaiting && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		isWaiting = false
	}

	for {
		select {
		case <-r.conn.done:
			return

		case signal := <-r.watch:
			switch signal {
			case watchResponseStarted:
				hasAudioArrived = false
				arm(r.firstAudioDeadline)
			case watchAudioArrived:
				hasAudioArrived = true
				arm(r.deltaStallDeadline)
			case watchResponseEnded:
				disarm()
			}

		case <-timer.C:
			isWaiting = false
			reason := "the provider never started speaking"
			if hasAudioArrived {
				reason = "the provider stopped partway through speaking"
			}
			r.log.Warn("response abandoned", "reason", reason,
				"hasAudioArrived", hasAudioArrived)
			// Not fatal: the session is still usable, and the caller has heard
			// whatever did arrive. The flow decides what to say next.
			r.emit(Event{Type: EventTypeError, Text: reason,
				Err: errors.New(reason)})
			r.emit(Event{Type: EventTypeResponseDone, Status: StatusStalled})
		}
	}
}

// signal notifies the watchdog without ever blocking the read loop.
func (r *Realtime) signal(s watchSignal) {
	select {
	case r.watch <- s:
	case <-r.conn.done:
	default:
		// The watchdog is momentarily behind; a missed progress signal only
		// costs a spurious timeout, never a wrong one.
	}
}

// emit delivers an event, dropping it only once the session is finished.
func (r *Realtime) emit(event Event) {
	select {
	case r.events <- event:
	case <-r.conn.done:
		// The session is over; nobody is reading any more.
	}
}

func isNormalClosure(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway)
}
