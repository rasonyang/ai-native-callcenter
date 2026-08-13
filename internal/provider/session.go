// SPDX-License-Identifier: Apache-2.0

// Package provider is the speech-to-speech abstraction the rest of the
// application talks to. It speaks only in audio frames, transcripts, turn
// boundaries, tool calls, interruption and instructions — nothing about
// recognition, synthesis or any particular vendor's protocol.
package provider

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// FrameInterval is the cadence caller audio is fed at. It matches the
// packetisation on the telephone leg, so audio moves at the rate it arrives
// rather than in bursts.
const FrameInterval = 20 * time.Millisecond

// VoiceSession is one conversation with one speech model.
//
// The surface deliberately mentions no recognition or synthesis concepts. A
// pipeline built from separate components would implement the same methods:
// SendAudio feeds detection and recognition, ToolCall comes from the language
// model, AudioDelta from synthesis, and Interrupt cancels both. Nothing here
// would have to change to accommodate that.
type VoiceSession interface {
	// Start connects and negotiates the session, then asks for the opening
	// turn. It returns once the model is ready to be spoken to.
	Start(ctx context.Context, cfg SessionConfig) error

	// SendAudio forwards caller audio already in the session's input format.
	SendAudio(audio []byte) error

	// SendToolResult answers a tool call. hint steers the next turn: it is
	// what the engine wants said or done next, and it reaches the model
	// alongside the result rather than as a separate instruction.
	//
	// toolCallID identifies the model's function call, not the telephone call.
	SendToolResult(toolCallID, output, hint string) error

	// UpdateInstructions replaces the standing instructions mid-call, which is
	// how flow state and collected facts are re-pinned as a conversation moves
	// between phases.
	UpdateInstructions(text string) error

	// Interrupt handles barge-in. playedMs is how much of the current response
	// the caller actually heard, which some providers need in order to keep
	// their own history honest about what was said.
	//
	// This differs from the design's single-argument sketch: truncation cannot
	// be expressed without the played duration, and the caller is the only
	// party that knows it.
	Interrupt(reason InterruptReason, playedMs int) error

	// Events yields everything the model reports. The consumer must keep up;
	// this is a live conversation and there is nowhere to buffer it.
	Events() <-chan Event

	// Close ends the session. It is idempotent.
	Close(ctx context.Context) error
}

// SessionConfig is everything the model needs before it hears anything.
//
// Some providers freeze parts of this after the first audio frame, so it is
// assembled completely before the session starts rather than adjusted later.
type SessionConfig struct {
	Instructions string
	Voice        string
	// Language informs the prompt and the choice of provider profile. It does
	// not route anything by itself.
	Language string
	Turn     TurnDetection
	Tools    []ToolSpec
	// GreetingCue prompts the opening turn on providers that will not speak
	// into an empty conversation. It is a stage direction, not something the
	// caller said, and it is never recorded as caller speech. Empty uses a
	// default in the session's language.
	GreetingCue string
	// InputFormat and OutputFormat are what this session's audio will be in.
	// The caller converts to and from them.
	InputFormat  media.AudioFormat
	OutputFormat media.AudioFormat
}

// TurnMode is how the end of the caller's turn is decided.
type TurnMode string

const (
	// TurnModeVAD ends a turn after a fixed silence. This is the default for
	// both languages: it is the only mode that meets the latency budget.
	TurnModeVAD TurnMode = "VAD"
	// TurnModeSemantic waits for the utterance to sound complete. It ignores
	// backchannels, at a cost of well over a second of added turn latency on
	// at least one provider, so it is opt-in per flow.
	TurnModeSemantic TurnMode = "SEMANTIC"
	// TurnModeNone leaves turn-taking to the application.
	TurnModeNone TurnMode = "NONE"
)

// TurnDetection configures turn taking.
type TurnDetection struct {
	Mode TurnMode
	// SilenceMs is how long a pause ends a turn. It is the single biggest
	// knob in the response-latency budget.
	SilenceMs int
	// Threshold is the speech-detection sensitivity; zero uses the provider's
	// own default.
	Threshold float64
}

// DefaultTurnDetection is what a flow gets unless it says otherwise.
func DefaultTurnDetection() TurnDetection {
	return TurnDetection{Mode: TurnModeVAD, SilenceMs: 500}
}

// ToolSpec is a function the model may call.
type ToolSpec struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object.
	Parameters json.RawMessage
}

// InterruptReason is why a response was cut short.
type InterruptReason string

const (
	// InterruptReasonSpeech is the caller talking over the model.
	InterruptReasonSpeech InterruptReason = "SPEECH"
	// InterruptReasonDTMF is a keypress, which always interrupts immediately.
	InterruptReasonDTMF InterruptReason = "DTMF"
	// InterruptReasonSystem is the application cutting the model off, for a
	// transfer or a hangup.
	InterruptReasonSystem InterruptReason = "SYSTEM"
)

// EventType is the closed set of things a session reports.
type EventType string

const (
	EventTypeSessionReady     EventType = "SESSION_READY"
	EventTypeAudioDelta       EventType = "AUDIO_DELTA"
	EventTypeInputTranscript  EventType = "INPUT_TRANSCRIPT"
	EventTypeOutputTranscript EventType = "OUTPUT_TRANSCRIPT"
	EventTypeSpeechStarted    EventType = "SPEECH_STARTED"
	EventTypeSpeechStopped    EventType = "SPEECH_STOPPED"
	EventTypeResponseStarted  EventType = "RESPONSE_STARTED"
	EventTypeInterrupted      EventType = "INTERRUPTED"
	EventTypeToolCall         EventType = "TOOL_CALL"
	EventTypeResponseDone     EventType = "RESPONSE_DONE"
	EventTypeSessionWarning   EventType = "SESSION_WARNING"
	EventTypeError            EventType = "ERROR"
	EventTypeClosed           EventType = "CLOSED"
)

// Event is one thing the model reported. Which fields carry meaning depends on
// Type; the rest are zero.
//
// Transcript events are unordered relative to audio: a provider may report
// what it heard before or after it starts answering, and consumers must not
// depend on either.
type Event struct {
	Type EventType

	// Audio carries model speech in the session's output format
	// (EventTypeAudioDelta).
	Audio []byte

	// Text is transcript text (EventTypeInputTranscript,
	// EventTypeOutputTranscript) or a message (EventTypeError,
	// EventTypeSessionWarning).
	Text string
	// IsFinal distinguishes a completed transcript from a partial one.
	IsFinal bool

	// ToolCallID, ToolName and ToolArgs describe a function call
	// (EventTypeToolCall). ToolArgs is a JSON object.
	ToolCallID string
	ToolName   string
	ToolArgs   string

	// Status is the provider's own word for how a response ended
	// (EventTypeResponseDone).
	Status string
	Usage  Usage

	// InterruptedBy says what cut a response short (EventTypeInterrupted).
	InterruptedBy InterruptReason

	// Err is set on EventTypeError. IsFatal means the session cannot continue:
	// provider state is unrecoverable, so the call must be routed elsewhere
	// rather than retried.
	Err     error
	IsFatal bool
}

// Usage is what a turn cost, for budget tracking on providers that cap a
// session by turns or by audio duration rather than by wall-clock time.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}
