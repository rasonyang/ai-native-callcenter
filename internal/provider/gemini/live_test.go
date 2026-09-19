// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

// This runs against the real service and costs money, so it is skipped unless
// AICC_LIVE_PROVIDER_TEST is set. It exists for the two things no fake can
// answer.
//
// The first is the tool schemas. Every schema in this repository is written the
// way JSON Schema spells a type; this API's type is a protobuf enum, and a setup
// it will not have is refused by closing the socket rather than by saying so. A
// fake accepts anything.
//
// The second is the opening turn. The Live API waits to be spoken to, and what
// makes this model greet from its instructions is an empty turn list — a frame
// with nothing in it, which a fake cannot prove the service understands.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test ./internal/provider/gemini/ -run Live -count=1 -v
func TestLiveGeminiGreetsTheCallerAndSpeaksAtTwentyFourKilohertz(t *testing.T) {
	requireLiveProvider(t)

	session, err := newSession(liveProfile(), slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	cfg := provider.SessionConfig{
		Instructions: "You are Ada, the voice assistant on the NovaNet customer hotline. " +
			"Open the call by greeting the caller in one short English sentence and " +
			"asking how you can help. RESPOND IN ENGLISH.",
		Language: "en",
		Turn:     provider.DefaultTurnDetection(),
		// The schemas a flow actually carries, verbatim from
		// internal/flow/builtin.go: lowercase types, a nested object with no
		// properties of its own, an enum of this deployment's queues.
		Tools:        liveTools(),
		InputFormat:  media.PCM16Format(media.RateProviderIn),
		OutputFormat: media.PCM16Format(media.RateProviderOut),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := session.Start(ctx, cfg); err != nil {
		t.Fatalf("start (the setup was refused, which is what the schemas are about): %v", err)
	}

	var audio []byte
	var said string
	var turns, endings int
	listening := time.After(20 * time.Second)
collect:
	for {
		select {
		case <-listening:
			break collect
		case event, ok := <-session.Events():
			if !ok {
				break collect
			}
			switch event.Type {
			case provider.EventTypeAudioDelta:
				audio = append(audio, event.Audio...)
			case provider.EventTypeOutputTranscript:
				if event.IsFinal {
					said += event.Text
				}
			case provider.EventTypeResponseStarted:
				turns++
			case provider.EventTypeResponseDone, provider.EventTypeInterrupted:
				endings++
				if endings > 0 && len(audio) > 0 {
					break collect
				}
			case provider.EventTypeError:
				t.Logf("provider error (fatal=%v): %v", event.IsFatal, event.Err)
				if event.IsFatal {
					break collect
				}
			}
		}
	}

	t.Logf("the model opened the call with %q in %d bytes (%.2fs at 24 kHz, 16-bit)",
		said, len(audio), float64(len(audio))/(2*float64(media.RateProviderOut)))
	if turns != 1 || endings != 1 {
		t.Errorf("the call had %d turns and %d endings, want one of each", turns, endings)
	}
	if said == "" {
		t.Error("the model said nothing that was transcribed")
	}
	checkTheAudioIsSignedSixteenBit(t, audio)

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := session.outcome(); got != outcomeCleanClose {
		t.Errorf("the session ended as %q, want %q", got, outcomeCleanClose)
	}
	t.Logf("stats: %+v", session.Stats())
}

// The control for the rewrite: the same schema, in the casing the flows write it
// in, sent by hand.
//
// It answers whether the rewrite in geminiSchema is necessary or merely
// harmless, which decides whether a later reader may delete it. It is a raw
// socket rather than a session because the client under test always rewrites —
// that is the point of it — and because a setup that is refused costs one
// handshake and not a single token.
func TestLiveGeminiRefusesASchemaInTheCasingTheFlowsUse(t *testing.T) {
	requireLiveProvider(t)

	headers := http.Header{}
	headers.Set(apiKeyHeader, os.Getenv("GEMINI_API_KEY"))
	conn, err := wsconn.Dial(context.Background(), defaultEndpoint, headers,
		slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		// This one asks the service a question about itself rather than
		// checking anything this client does, and the dial budget it borrows is
		// a phone call's rather than a diagnostic's. A network that cannot meet
		// it has nothing to say about the schemas.
		t.Skipf("could not reach the service to ask: %v", err)
	}
	defer conn.Close()

	setup := map[string]any{"setup": map[string]any{
		"model": wireModel,
		"generationConfig": map[string]any{
			"responseModalities": []string{audioModality}},
		"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{
			"name":        "transfer_to_agent",
			"description": "Transfer the caller to a human queue.",
			"behavior":    behaviorBlocking,
			// As a flow writes it, and as this client does NOT send it.
			"parameters": json.RawMessage(
				`{"type":"object","properties":{"queue":{"type":"string"}},"required":["queue"]}`),
		}}}},
	}}
	data, err := json.Marshal(setup)
	if err != nil {
		t.Fatalf("encode the setup: %v", err)
	}
	if err := conn.Send(data); err != nil {
		t.Fatalf("send the setup: %v", err)
	}

	_, reply, err := conn.Receive()
	if err == nil {
		t.Logf("the service ACCEPTED the flows' own casing and answered %s", reply)
		t.Log("the rewrite in geminiSchema is harmless rather than necessary; " +
			"record that before anyone deletes it")
		return
	}

	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		t.Logf("the service REFUSED the flows' own casing: close %d %q",
			closed.Code, closed.Text)
		return
	}
	t.Logf("the setup was refused with %v", err)
}

// requireLiveProvider skips unless this is a run that may spend money.
func requireLiveProvider(t *testing.T) {
	t.Helper()
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") == "" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run against the real provider")
	}
	if os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("GEMINI_API_KEY is not set")
	}
}

// liveProfile is the deployment values this client would be given, with the
// endpoint left to its default.
func liveProfile() provider.Profile {
	return provider.Profile{
		Name:         "gemini",
		APIKeyEnv:    "GEMINI_API_KEY",
		Voice:        "Kore",
		LinearInput:  media.PCM16Format(media.RateProviderIn),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
	}
}

// liveTools is what a flow hands over, schemas and all.
func liveTools() []provider.ToolSpec {
	return []provider.ToolSpec{
		{
			Name: "transfer_to_agent",
			Description: "Transfer the caller to a human queue. " +
				"Announce the transfer only after the tool returns.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"queue": {"type": "string", "description": "Which queue to transfer to", "enum": ["support","sales"]},
					"reason": {"type": "string", "description": "Why the caller needs a person"},
					"summary": {"type": "string", "description": "What the conversation established, in the caller's language, at most 600 characters"},
					"slots": {"type": "object", "description": "Everything collected so far"}
				},
				"required": ["queue", "reason", "summary"]
			}`),
		},
		{
			Name:        "take_message",
			Description: "Take a message and a callback number.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"message": {"type": "string", "description": "What the caller wants passed on"},
					"callbackNumber": {"type": "string", "description": "Where to call back"}
				},
				"required": ["message"]
			}`),
		},
	}
}

// checkTheAudioIsSignedSixteenBit is what the downlink claims to be, measured.
//
// The frames say audio/pcm;rate=24000 and this application plays them as signed
// 16-bit little-endian at 24 kHz. Read as anything else — floats, or the wrong
// endianness — the same sentence is a signal where a large share of the samples
// sit at the extremes and it sounds like noise on the line. Speech does not do
// that.
func checkTheAudioIsSignedSixteenBit(t *testing.T, audio []byte) {
	t.Helper()

	if len(audio) == 0 {
		t.Fatal("no audio came back, so the model never opened the call")
	}
	if len(audio)%2 != 0 {
		t.Fatalf("%d bytes of audio is not a whole number of 16-bit samples", len(audio))
	}

	var loud, nonZero int
	var energy float64
	samples := len(audio) / 2
	for i := range samples {
		sample := float64(int16(binary.LittleEndian.Uint16(audio[2*i:])))
		if sample != 0 {
			nonZero++
		}
		if math.Abs(sample) > 20000 {
			loud++
		}
		energy += sample * sample
	}
	rms := math.Sqrt(energy / float64(samples))
	loudShare := float64(loud) / float64(samples)
	t.Logf("%d samples, rms %.0f, %.1f%% past ±20000", samples, rms, 100*loudShare)

	if nonZero*4 < samples {
		t.Errorf("%d of %d samples are silent: this is not speech", samples-nonZero, samples)
	}
	if rms < 200 || rms > 20000 {
		t.Errorf("rms %.0f is not a plausible level for speech read as 16-bit", rms)
	}
	if loudShare > 0.05 {
		t.Errorf("%.1f%% of samples are past ±20000: these bytes are probably not int16",
			100*loudShare)
	}
}
