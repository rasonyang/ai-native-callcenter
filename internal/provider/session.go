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
	"errors"
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

	// SendToolResult answers a tool call. What the engine wants said or done
	// next is already part of output: the caller of this method merges it in
	// (MergeHint) so it reaches the model as part of what it just learned.
	//
	// toolCallID identifies the model's function call, not the telephone call.
	SendToolResult(toolCallID, output string) error

	// UpdateInstructions replaces the standing instructions mid-call, which is
	// how flow state and collected facts are re-pinned as a conversation moves
	// between phases.
	UpdateInstructions(text string) error

	// SpeakText makes the bot say something the application has decided on,
	// mid-call. Verbatim where the provider can manage it and as close as it
	// will come otherwise — this is a request for particular words, not a brief
	// to write from. A client may ask its engine for the line more than once
	// to get it said (the Realtime client asks once more when the line's turn
	// called a tool and said nothing); the caller hears it at most once.
	//
	// It PRE-EMPTS: whatever the model is in the middle of saying, this line
	// replaces it, and the caller stops hearing the old one. It does NOT
	// QUEUE: a second call before the first has been spoken replaces it too,
	// because both say what should come next and the older one is by then out
	// of date.
	//
	// It is for mid-call use. The line a call opens with is SessionConfig's,
	// because the opening turn is asked for as part of starting the session.
	//
	// isClosing says the line ends the call — the flow has reached a phase it
	// does not leave, and nothing will be asked of the model after it. A
	// client may steer such a line harder than one the conversation goes on
	// from, because whatever that leaves in the model's history has no later
	// turn to affect (the Realtime client does, on a profile with
	// NeedsDirectedLineInConversation). A client that speaks text outright,
	// or steers every line the same way, ignores it.
	SpeakText(text string, isClosing bool) error

	// Interrupt handles barge-in. playedMs is how much of the current response
	// the caller actually heard, which some providers need in order to keep
	// their own history honest about what was said. Zero is meaningful: the
	// response's audio was queued and none of it reached the caller, so the
	// whole response is to be treated as unheard. The caller invokes Interrupt
	// only when something was playing or queued; a negative value means "not
	// known" and truncates nothing.
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
	// Temperature is the sampling temperature the flow asked for. Nil leaves
	// the provider's own default, which is what every call did before there
	// was a place to say it. A profile that cannot take one
	// (Profile.AcceptsTemperature) ignores it.
	Temperature *float64
	// Language sets the prompt language and the default greeting cue. It never
	// selects the provider, which a deployment chooses once at startup (A1),
	// nor the voice, which the flow owns (A7).
	Language string
	Turn     TurnDetection
	Tools    []ToolSpec
	// OpeningText is what the bot says at the start of the call: not a brief
	// for a greeting but the greeting, in the words the flow chose. Empty
	// leaves the opening to the model, which is what every call did before
	// there was anywhere to put a line.
	//
	// How close to verbatim it lands is the provider's to decide. The client
	// here can only direct a model to repeat a sentence, which is best effort;
	// an engine that speaks text outright will say it as written.
	OpeningText string
	// OpeningGate holds back the request for the opening turn. The session
	// still dials and finishes its handshake at call start; only the ask for
	// the first turn waits until the gate is closed. Nil asks immediately,
	// which is what every call did before there was a gate.
	//
	// The contract every client honours: after the handshake is ready and
	// before it asks for the first turn, wait for the gate, for ctx to end or
	// for the session to close (AwaitOpeningGate does exactly that). Start
	// blocks for the wait, so the caller starts its audio pumps only after the
	// opening turn is requested and no caller audio reaches the provider
	// ahead of it. The wait is not part of any handshake deadline. A gate the
	// caller never closes is the caller's bug: it must close it on every path
	// that ends the call.
	OpeningGate <-chan struct{}
	// InputFormat and OutputFormat are what this session's audio will be in.
	// The caller converts to and from them.
	InputFormat  media.AudioFormat
	OutputFormat media.AudioFormat
}

// AwaitOpeningGate blocks until the opening gate is closed, ctx ends or closed
// (the session's own end signal, nil for none) fires. A nil gate returns at
// once. It returns nil only when the gate opened.
func AwaitOpeningGate(ctx context.Context, gate <-chan struct{}, closed <-chan struct{}) error {
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-closed:
		return errors.New("session closed while waiting to greet")
	}
}

// TurnDetection configures turn taking. Every call ends a caller's turn after
// a fixed silence (server-side VAD): it is the only mode that meets the latency
// budget. Semantic turn detection was removed with no flow field or setting
// asking for it; it comes back with a measured reason.
type TurnDetection struct {
	// SilenceMs is how long a pause ends a turn. It is the single biggest
	// knob in the response-latency budget.
	SilenceMs int
}

// DefaultTurnDetection is what a flow gets unless it says otherwise.
func DefaultTurnDetection() TurnDetection {
	return TurnDetection{SilenceMs: 500}
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

	// InterruptedBy says what cut a response short (EventTypeInterrupted).
	InterruptedBy InterruptReason

	// IsSpokenLine says, on EventTypeResponseStarted, that the turn is a line
	// SpeakText asked for rather than one the model writes on its own: the
	// response this client requested for the line (Realtime), or the engine
	// speaking the committed text (doubao). It is this client's belief, as
	// sure as its knowledge of which request a response answers.
	IsSpokenLine bool
	// IsLineAskedAgain says, with IsSpokenLine, that the turn is the line of
	// the spoken-line turn just before it, asked for once more because that
	// turn called a tool instead of saying it and said nothing (Realtime). It
	// is the same line: whatever marked the first turn (an aside) marks this
	// one too.
	IsLineAskedAgain bool

	// Err is set on EventTypeError. IsFatal means the session cannot continue:
	// provider state is unrecoverable, so the call must be routed elsewhere
	// rather than retried.
	Err     error
	IsFatal bool
}
