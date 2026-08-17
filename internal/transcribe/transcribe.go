// SPDX-License-Identifier: Apache-2.0

// Package transcribe recognises speech on a human leg.
//
// It is deliberately not part of internal/provider. That package owns the
// conversational engine and its own contract forbids a recognition concept
// entering it; this one synthesises nothing, holds no dialogue state and
// drives no conversation. The two are siblings joined only by the caller.
//
// Unlike internal/provider, this package holds *two* clients, and that is the
// same rule applied rather than an exception to it: the extension point is the
// wire protocol. OpenAI serves transcription over the Realtime dialect;
// Alibaba serves qwen-audio-3.0-asr-flash-streaming over DashScope's native
// run-task duplex protocol. Those are different grammars, not different field
// names, and no Profile bridges them. A new *engine* on either protocol is a
// new profile; only a new *protocol* earns a third client.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// EventType is what a client reports upward. The seam above never learns which
// vendor produced it.
type EventType string

const (
	// EventPartial is a guess that will be revised. Text is always the whole
	// utterance so far, never a fragment to append: the clients fold three
	// different vendor conventions into this one.
	EventPartial EventType = "PARTIAL"
	// EventFinal is the engine's settled text for one utterance.
	EventFinal EventType = "FINAL"
	// EventSpeechStarted reports the onset of speech, where the engine says so.
	EventSpeechStarted EventType = "SPEECH_STARTED"
	// EventError is a session-level failure. The session is unusable after it.
	EventError EventType = "ERROR"
	// EventClosed is the session ending, for any reason.
	EventClosed EventType = "CLOSED"
)

// Event is one thing the engine said about the audio.
type Event struct {
	Type EventType
	// UtteranceID identifies the utterance across its partials and its final.
	UtteranceID string
	// Text is the full text so far for a partial, or the settled text for a
	// final. Never a fragment.
	Text string
	// Language as reported by the engine; empty when it does not say.
	Language string
	// StartedAtMs and EndedAtMs are stream-relative and optional: DashScope
	// reports them, OpenAI does not. They are enrichment, never the ordering
	// mechanism — two streams' clocks share no origin (D2).
	StartedAtMs int
	EndedAtMs   int
	Err         error
}

// Config is what one recognition session needs to know.
type Config struct {
	// Language hint, lowercase BCP 47. Empty lets the engine decide.
	Language string
	// Hints are domain words worth biasing toward; empty is fine.
	Hints []string
}

// Session recognises one audio stream. It is deliberately smaller than
// provider.VoiceSession: there is no synthesis, no tools and no interruption.
type Session interface {
	// Start opens the session. Audio may be sent once it returns.
	Start(ctx context.Context, cfg Config) error
	// SendAudio submits mono PCM16 at the profile's rate. It must not block on
	// the network: a media path may not stall on a recogniser.
	SendAudio(pcm16 []byte) error
	// Events reports what the engine heard. Closed when the session ends.
	Events() <-chan Event
	// Close ends the session and releases its socket.
	Close(ctx context.Context) error
}

// Profile is one engine reached over one protocol.
type Profile struct {
	// Name is the AICC_TRANSCRIBE_PROVIDER value.
	Name string
	// Endpoint is the websocket URL. On DashScope the workspace id is part of
	// the hostname, so this is deployment configuration and has no useful
	// default.
	Endpoint string
	// Model names the engine.
	Model string
	// SampleRate is the rate the tap must be asked for. The switch resamples,
	// so this is the rate we request rather than one we produce (D15).
	SampleRate int
	// APIKeyEnv names the environment variable holding the credential.
	APIKeyEnv string
	// OwnsEndpointing is true when *we* decide where an utterance ends.
	// OpenAI's transcription model refuses turn detection, so on that path the
	// ingest detects silence and commits; DashScope segments server-side.
	OwnsEndpointing bool
}

// Known provider names.
const (
	ProviderOpenAI = "openai"
	ProviderQwen   = "qwen"
)

// ErrUnknownProvider is returned for an AICC_TRANSCRIBE_PROVIDER we cannot serve.
var ErrUnknownProvider = errors.New("unknown transcription provider")

// Override carries deployment configuration over a profile's defaults.
type Override struct {
	Endpoint string
	Model    string
}

// ProfileFor resolves the deployment's transcription profile, once, at startup.
// An unknown name is a startup failure rather than a surprise on a live call.
func ProfileFor(name string, over Override) (Profile, error) {
	var p Profile
	switch strings.ToLower(strings.TrimSpace(name)) {
	case ProviderOpenAI:
		p = Profile{
			Name:     ProviderOpenAI,
			Endpoint: "wss://api.openai.com/v1/realtime?intent=transcription",
			Model:    "gpt-live-transcribe",
			// Measured: the transcription session refuses anything below
			// 24000, so the tap is asked for 24 kHz and the switch resamples.
			SampleRate: 24000,
			APIKeyEnv:  "OPENAI_API_KEY",
			// Measured: turn_detection is refused, and six seconds of silence
			// produced no final. The boundary is ours to send.
			OwnsEndpointing: true,
		}
	case ProviderQwen:
		p = Profile{
			Name: ProviderQwen,
			// No default: the workspace id is the hostname.
			Endpoint:        "",
			Model:           "qwen-audio-3.0-asr-flash-streaming",
			SampleRate:      16000,
			APIKeyEnv:       "ALIYUN_API_KEY",
			OwnsEndpointing: false,
		}
	default:
		return Profile{}, fmt.Errorf("%w: %q", ErrUnknownProvider, name)
	}

	if over.Endpoint != "" {
		p.Endpoint = over.Endpoint
	}
	if over.Model != "" {
		p.Model = over.Model
	}
	return p, nil
}

// Validate reports configuration that cannot work, at startup rather than on
// the first call.
func (p Profile) Validate() error {
	if p.Endpoint == "" {
		return fmt.Errorf("transcribe: %s needs an endpoint (AICC_TRANSCRIBE_ENDPOINT); "+
			"on qwen the workspace id is part of the hostname and has no default", p.Name)
	}
	if !strings.HasPrefix(p.Endpoint, "ws://") && !strings.HasPrefix(p.Endpoint, "wss://") {
		return fmt.Errorf("transcribe: endpoint must be ws:// or wss://, got %q", p.Endpoint)
	}
	if p.Model == "" {
		return errors.New("transcribe: a model is required")
	}
	return nil
}

// New builds the client for a profile. The protocol, not the vendor, decides
// which one.
func New(p Profile, apiKey string, log Logger) (Session, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	switch p.Name {
	case ProviderQwen:
		return newDashscope(p, apiKey, log), nil
	case ProviderOpenAI:
		return newOpenAIRT(p, apiKey, log), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, p.Name)
	}
}

// Logger is the slice of slog this package uses, so tests need no handler.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}
