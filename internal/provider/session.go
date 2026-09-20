// SPDX-License-Identifier: Apache-2.0

// Package provider is the speech-to-speech client the rest of the application
// talks to. It speaks only in audio frames, transcripts, turn boundaries, tool
// calls, interruption and instructions — nothing about recognition, synthesis
// or any particular vendor's protocol.
//
// There is one wire protocol here, OpenAI Realtime, and one client for it,
// parameterised by a Profile for each vendor's dialect. That protocol is the
// extension point: anything that speaks it — a vendor, a regional host, a
// gateway that composes recognition, a language model and synthesis behind the
// same events — is reached by pointing the endpoint at it (Override), and this
// package does not know or care what is on the other end. Cascaded pipelines
// are built as such a gateway, in their own service; no recognition or
// synthesis concept ever enters this one.
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
// It is the seam between the call actor and the client: aicall drives a
// conversation through these methods and nothing else, and tests stand a fake
// model behind them. It is not a plug-in point for other kinds of engine —
// that job belongs to the wire protocol, see the package comment.
type VoiceSession interface {
	// Start connects and negotiates the session, then asks for the opening
	// turn. It returns once the model is ready to be spoken to.
	Start(ctx context.Context, cfg SessionConfig) error

	// SendAudio forwards caller audio already in the session's input format.
	SendAudio(audio []byte) error

	// SendUserText puts something the caller did, but did not say, into the
	// conversation and asks the model to respond to it. Keypresses are the
	// case that matters: the caller pressed 2, and the model has to know that
	// as surely as if they had said it.
	SendUserText(text string) error

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

	// SpeakText makes the bot say something the application has decided on,
	// mid-call. Verbatim where the provider can manage it and as close as it
	// will come otherwise — this is a request for particular words, not a brief
	// to write from.
	//
	// It PRE-EMPTS: whatever the model is in the middle of saying, this line
	// replaces it, and the caller stops hearing the old one. It does NOT
	// QUEUE: a second call before the first has been spoken replaces it too,
	// because both say what should come next and the older one is by then out
	// of date.
	//
	// It is for mid-call use. The line a call opens with is SessionConfig's,
	// because the opening turn is asked for as part of starting the session.
	SpeakText(text string) error

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
	// OpeningText is what the bot says at the start of the call: not a brief
	// for a greeting but the greeting, in the words the flow chose. Empty
	// leaves the opening to the model, which is what every call did before
	// there was anywhere to put a line.
	//
	// How close to verbatim it lands is the provider's to decide. The client
	// here can only direct a model to repeat a sentence, which is best effort;
	// an engine that speaks text outright will say it as written.
	OpeningText string
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
	// EventTypeOutputTranscript) or a message (EventTypeError).
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
	//
	// FailureCause says why, for a fatal error a client can put a name to. It
	// is empty on every failure that has no name of its own, which is most of
	// them, and the call path treats an empty cause exactly as it always did.
	Err          error
	IsFatal      bool
	FailureCause FailureCause
}

// FailureCause is why a session ended fatally, in this repository's words.
//
// It is here rather than in a client because it leaves the provider layer: the
// call path releases the call with it as the hangup cause, so it goes into a
// CDR that a customer reads and an operator reports on. A vendor's own code is
// the wrong thing to put there — it means nothing outside that vendor's
// documentation, and a deployment that changes engine would find its history
// speaking two languages. A client that knows its engine's code translates it
// into one of these or into nothing at all.
type FailureCause string

const (
	// FailureCauseSessionExpired is the provider ending the session because its
	// own lifetime ran out, with the caller still on the line. Some engines cap
	// how long one session may last however well it is going.
	//
	// It is worth telling apart from every other fatal error because nothing is
	// broken: not the network, not the credential, not the engine. A deployment
	// seeing it has conversations that outlive a limit, which is answered by
	// what the flow is asking of the caller rather than by fixing anything.
	FailureCauseSessionExpired FailureCause = "PROVIDER_SESSION_EXPIRED"
)

// Usage is what a turn cost, for budget tracking on providers that cap a
// session by turns or by audio duration rather than by wall-clock time.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}
