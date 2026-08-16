// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

func basicConfig() SessionConfig {
	return SessionConfig{
		Instructions: "You answer the phone for NovaNet.",
		Language:     "en",
		Turn:         DefaultTurnDetection(),
		Tools: []ToolSpec{{
			Name:        "transfer_to_agent",
			Description: "Hand the caller to a person.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"queue":{"type":"string"}}}`),
		}},
		InputFormat:  media.G711Format(media.LawMu),
		OutputFormat: media.G711Format(media.LawMu),
	}
}

//
// Profiles.
//

// Which provider answers is a deployment setting. Nothing about a call — its
// language least of all — may reach into this choice.
func TestProfileIsChosenByNameNotLanguage(t *testing.T) {
	openai, err := ProfileFor(NameOpenAI, Override{})
	if err != nil || openai.Name != NameOpenAI {
		t.Fatalf("ProfileFor(openai) = %q, %v", openai.Name, err)
	}
	qwen, err := ProfileFor(NameQwen, Override{})
	if err != nil || qwen.Name != NameQwen {
		t.Fatalf("ProfileFor(qwen) = %q, %v", qwen.Name, err)
	}
	// Spelling is an operator's input, so it is forgiving about case and space.
	if got, err := ProfileFor("  QWEN ", Override{}); err != nil || got.Name != NameQwen {
		t.Errorf("ProfileFor(\"  QWEN \") = %q, %v", got.Name, err)
	}
	// An unknown name fails at startup rather than on the first call.
	if _, err := ProfileFor("nonesuch", Override{}); err == nil {
		t.Error("an unknown provider name was accepted")
	}
	if _, err := ProfileFor("", Override{}); err == nil {
		t.Error("an empty provider name was accepted")
	}
}

// A deployment that cannot reach the vendor directly — a proxy, a regional
// host, or a gateway that merely speaks the protocol — has to be able to say so.
func TestConnectionDetailsCanBeOverridden(t *testing.T) {
	endpoint, err := ProfileFor(NameOpenAI, Override{Endpoint: "wss://gateway.internal/realtime"})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Endpoint != "wss://gateway.internal/realtime" {
		t.Errorf("endpoint = %q, want the override", endpoint.Endpoint)
	}
	if endpoint.Model != OpenAIProfile().Model {
		t.Errorf("an empty field replaced the model with %q", endpoint.Model)
	}

	model, err := ProfileFor(NameQwen, Override{Model: "qwen-audio-3.0-realtime-flash"})
	if err != nil {
		t.Fatal(err)
	}
	if model.Model != "qwen-audio-3.0-realtime-flash" {
		t.Errorf("model = %q, want the override", model.Model)
	}
	if model.Endpoint != QwenProfile().Endpoint {
		t.Errorf("an empty field replaced the endpoint with %q", model.Endpoint)
	}

	// The model still selects on the connection address, wherever it points.
	if url := model.endpointURL(); !strings.Contains(url, "model=qwen-audio-3.0-realtime-flash") {
		t.Errorf("connection url %q does not carry the overridden model", url)
	}
}

// The whole point of the passthrough path: where the provider takes G.711, the
// call does no conversion at all.
func TestFormatsForFollowTheNegotiatedLaw(t *testing.T) {
	input, output := OpenAIProfile().FormatsFor(media.LawAlaw)
	if input != media.G711Format(media.LawAlaw) || output != media.G711Format(media.LawAlaw) {
		t.Errorf("A-law call got %s in / %s out, want A-law both ways", input, output)
	}

	input, output = QwenProfile().FormatsFor(media.LawMu)
	if input != media.PCM16Format(media.RateProviderIn) {
		t.Errorf("input format = %s, want linear at the provider's input rate", input)
	}
	if output != media.PCM16Format(media.RateProviderOut) {
		t.Errorf("output format = %s, want linear at the provider's output rate", output)
	}
}

//
// Session configuration payloads.
//

func TestSessionUpdateInTheCurrentDialect(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	update := f.awaitMessage("session.update")

	if got := nested(t, update, "session", "instructions"); got != basicConfig().Instructions {
		t.Errorf("instructions = %v", got)
	}
	// G.711 is named as a format in its own right, not as linear audio.
	if got := nested(t, update, "session", "audio", "input", "format", "type"); got != "audio/pcmu" {
		t.Errorf("input format = %v, want audio/pcmu", got)
	}
	if got := nested(t, update, "session", "audio", "output", "format", "type"); got != "audio/pcmu" {
		t.Errorf("output format = %v, want audio/pcmu", got)
	}
	// Companded audio has one rate by definition; stating it would be noise.
	if format, ok := nested(t, update, "session", "audio", "input", "format").(map[string]any); ok {
		if _, present := format["rate"]; present {
			t.Error("a rate was sent alongside a companded format")
		}
	}
	if got := nested(t, update, "session", "audio", "output", "voice"); got != "marin" {
		t.Errorf("voice = %v", got)
	}

	turn := nested(t, update, "session", "audio", "input", "turn_detection").(map[string]any)
	if turn["type"] != "server_vad" || turn["silence_duration_ms"] != float64(500) {
		t.Errorf("turn detection = %v, want server_vad holding 500ms", turn)
	}

	tools := nested(t, update, "session", "tools").([]any)
	if len(tools) != 1 {
		t.Fatalf("sent %d tools", len(tools))
	}
	// The flat shape, which both providers were verified to accept.
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "transfer_to_agent" {
		t.Errorf("tool = %v, want the flat function shape", tool)
	}
	if _, isNested := tool["function"]; isNested {
		t.Error("the tool was sent in the nested shape")
	}
}

func TestSessionUpdateInTheOlderDialect(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.Language = "zh"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	update := f.awaitMessage("session.update")

	if got := nested(t, update, "session", "input_audio_format"); got != "pcm" {
		t.Errorf("input format = %v, want the flat pcm name", got)
	}
	if got := nested(t, update, "session", "voice"); got != "longanqian" {
		t.Errorf("voice = %v", got)
	}
	if _, hasAudioBlock := nested(t, update, "session").(map[string]any)["audio"]; hasAudioBlock {
		t.Error("the newer nested audio block was sent to a provider using the older dialect")
	}
	modalities := nested(t, update, "session", "modalities").([]any)
	if len(modalities) != 2 {
		t.Errorf("modalities = %v", modalities)
	}
}

// Semantic turn taking is forced to a two-second hold on one provider, and any
// value we send is ignored. Sending one anyway would make the configuration
// claim a latency the call will not have.
func TestSemanticTurnDetectionOmitsAHoldTheProviderWouldIgnore(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.Turn = TurnDetection{Mode: TurnModeSemantic, SilenceMs: 500}
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	turn := nested(t, f.awaitMessage("session.update"), "session", "turn_detection").(map[string]any)
	if turn["type"] != "smart_turn" {
		t.Errorf("turn type = %v, want this vendor's semantic mode", turn["type"])
	}
	if _, present := turn["silence_duration_ms"]; present {
		t.Error("a silence hold was sent for a mode that overrides it")
	}
}

func TestSemanticTurnDetectionKeepsTheHoldWhereItIsHonoured(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	cfg := basicConfig()
	cfg.Turn = TurnDetection{Mode: TurnModeSemantic, SilenceMs: 400}
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	turn := nested(t, f.awaitMessage("session.update"),
		"session", "audio", "input", "turn_detection").(map[string]any)
	if turn["type"] != "semantic_vad" {
		t.Errorf("turn type = %v", turn["type"])
	}
	if turn["silence_duration_ms"] != float64(400) {
		t.Errorf("silence hold = %v, want it passed through", turn["silence_duration_ms"])
	}
}

//
// Handshake.
//

func TestStartWaitsForConfirmationThenAsksForTheOpeningTurn(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	awaitEvent(t, session, EventTypeSessionReady)
	f.awaitMessage("response.create")

	// The confirmation must precede the request, or the greeting is generated
	// under the provider's defaults rather than ours.
	sent := typesOf(f.messages())
	if len(sent) < 2 || sent[0] != "session.update" {
		t.Errorf("client sent %v, want the configuration first", sent)
	}
}

// One provider refuses to speak into an empty conversation, so the greeting
// has to be prompted. Verified live: without this the opening turn is rejected
// with "conversation has no messages or no user message" and the caller is met
// with silence.
func TestOpeningTurnIsPromptedWhereTheProviderNeedsIt(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.Language = "zh"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	item := nested(t, f.awaitMessage("conversation.item.create"), "item").(map[string]any)
	if item["role"] != "user" {
		t.Errorf("cue sent with role %v, want user", item["role"])
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "input_text" {
		t.Errorf("cue content type = %v", content["type"])
	}
	if !strings.Contains(content["text"].(string), "问候") {
		t.Errorf("cue is not in the session's language: %v", content["text"])
	}

	// The cue must precede the request, or it does not help.
	sent := typesOf(f.messages())
	cueAt, requestAt := indexOf(sent, "conversation.item.create"), indexOf(sent, "response.create")
	if cueAt < 0 || requestAt < 0 || cueAt > requestAt {
		t.Errorf("client sent %v, want the cue before the request", sent)
	}
}

// Where the provider greets unprompted, injecting a fake user turn would put
// words in the caller's mouth and into the transcript.
func TestNoCueIsSentWhereTheProviderDoesNotNeedOne(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	f.awaitMessage("response.create")
	f.refuteMessage("conversation.item.create")
}

func TestGreetingCueCanBeOverridden(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.GreetingCue = "(the caller is calling about an outage)"
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}

	item := nested(t, f.awaitMessage("conversation.item.create"), "item").(map[string]any)
	content := item["content"].([]any)[0].(map[string]any)
	if content["text"] != cfg.GreetingCue {
		t.Errorf("cue = %v, want the configured one", content["text"])
	}
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

func TestStartFailsWhenTheConfigurationIsRejected(t *testing.T) {
	f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		if message["type"] == "session.update" {
			f.send(map[string]any{"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "code": "invalid_value",
				"message": "voice is not available", "param": "session.voice",
			}})
		}
	})
	session := testSession(t, f, OpenAIProfile())

	err := session.Start(t.Context(), basicConfig())
	if err == nil {
		t.Fatal("Start succeeded against a provider that rejected the session")
	}
	if !strings.Contains(err.Error(), "voice is not available") {
		t.Errorf("error = %v, want the provider's reason", err)
	}
}

// A rejected configuration loses everything — persona, tools and all — so it
// is worth one retry without the optional parts before abandoning the call.
func TestRejectedConfigurationIsRetriedOnceWithoutOptionalFields(t *testing.T) {
	attempts := 0
	f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		if message["type"] != "session.update" {
			return
		}
		attempts++
		if attempts == 1 {
			f.send(map[string]any{"type": "error", "error": map[string]any{
				"code": "invalid_value", "message": "transcription is unsupported",
				"param": "session.update",
			}})
			return
		}
		f.send(map[string]any{"type": "session.updated"})
	})

	profile := OpenAIProfile()
	profile.TranscribeModel = "whisper-1"
	session := testSession(t, f, profile)

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start after retry: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("the configuration was sent %d times, want exactly one retry", attempts)
	}

	updates := 0
	for _, message := range f.messages() {
		if message["type"] != "session.update" {
			continue
		}
		updates++
		input := nested(t, message, "session", "audio", "input").(map[string]any)
		_, hasTranscription := input["transcription"]
		if updates == 1 && !hasTranscription {
			t.Error("the first attempt already omitted the optional field")
		}
		if updates == 2 {
			if hasTranscription {
				t.Error("the retry repeated the field that was rejected")
			}
			// The business persona must survive the retry; losing it would
			// leave the caller talking to a generic assistant.
			if got := nested(t, message, "session", "instructions"); got != basicConfig().Instructions {
				t.Errorf("the retry lost the instructions: %v", got)
			}
			if _, hasTools := nested(t, message, "session").(map[string]any)["tools"]; !hasTools {
				t.Error("the retry lost the tools")
			}
		}
	}
}

//
// Audio and events.
//

func TestSendAudioWireShape(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	frame := []byte{0xFF, 0x00, 0x7F, 0x80}
	if err := session.SendAudio(frame); err != nil {
		t.Fatalf("send audio: %v", err)
	}

	message := f.awaitMessage("input_audio_buffer.append")
	encoded, ok := message["audio"].(string)
	if !ok {
		t.Fatalf("audio field is %T", message["audio"])
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the hand-built JSON produced invalid base64: %v", err)
	}
	if string(decoded) != string(frame) {
		t.Errorf("audio round-tripped as %v, want %v", decoded, frame)
	}
}

func TestEventMapping(t *testing.T) {
	tests := []struct {
		name  string
		wire  map[string]any
		check func(*testing.T, Event)
		want  EventType
	}{
		{
			name: "caller started speaking",
			wire: map[string]any{"type": "input_audio_buffer.speech_started"},
			want: EventTypeSpeechStarted,
		},
		{
			name: "caller stopped speaking",
			wire: map[string]any{"type": "input_audio_buffer.speech_stopped"},
			want: EventTypeSpeechStopped,
		},
		{
			name: "model audio, current event name",
			wire: map[string]any{"type": "response.output_audio.delta",
				"delta": base64.StdEncoding.EncodeToString([]byte("hello"))},
			want: EventTypeAudioDelta,
			check: func(t *testing.T, e Event) {
				if string(e.Audio) != "hello" {
					t.Errorf("audio = %q", e.Audio)
				}
			},
		},
		{
			// The older name is still what some vendors emit.
			name: "model audio, older event name",
			wire: map[string]any{"type": "response.audio.delta",
				"delta": base64.StdEncoding.EncodeToString([]byte("world"))},
			want: EventTypeAudioDelta,
			check: func(t *testing.T, e Event) {
				if string(e.Audio) != "world" {
					t.Errorf("audio = %q", e.Audio)
				}
			},
		},
		{
			name: "what the caller said",
			wire: map[string]any{
				"type":       "conversation.item.input_audio_transcription.completed",
				"transcript": "I need to check my bill",
			},
			want: EventTypeInputTranscript,
			check: func(t *testing.T, e Event) {
				if e.Text != "I need to check my bill" || !e.IsFinal {
					t.Errorf("transcript = %q final=%v", e.Text, e.IsFinal)
				}
			},
		},
		{
			name: "what the model said",
			wire: map[string]any{"type": "response.output_audio_transcript.done",
				"transcript": "Certainly."},
			want: EventTypeOutputTranscript,
			check: func(t *testing.T, e Event) {
				if e.Text != "Certainly." || !e.IsFinal {
					t.Errorf("transcript = %q final=%v", e.Text, e.IsFinal)
				}
			},
		},
		{
			name: "partial output transcript",
			wire: map[string]any{"type": "response.output_audio_transcript.delta",
				"delta": "Cert"},
			want: EventTypeOutputTranscript,
			check: func(t *testing.T, e Event) {
				if e.Text != "Cert" || e.IsFinal {
					t.Errorf("partial transcript = %q final=%v", e.Text, e.IsFinal)
				}
			},
		},
		{
			name: "tool call",
			wire: map[string]any{"type": "response.function_call_arguments.done",
				"call_id": "fc_1", "name": "transfer_to_agent", "arguments": `{"queue":"billing"}`},
			want: EventTypeToolCall,
			check: func(t *testing.T, e Event) {
				if e.ToolCallID != "fc_1" || e.ToolName != "transfer_to_agent" {
					t.Errorf("tool call = %s/%s", e.ToolCallID, e.ToolName)
				}
				if e.ToolArgs != `{"queue":"billing"}` {
					t.Errorf("arguments = %s", e.ToolArgs)
				}
			},
		},
		{
			name: "tool call with no arguments",
			wire: map[string]any{"type": "response.function_call_arguments.done",
				"call_id": "fc_2", "name": "hangup", "arguments": ""},
			want: EventTypeToolCall,
			check: func(t *testing.T, e Event) {
				// An empty string is not valid JSON, and the engine parses it.
				if e.ToolArgs != "{}" {
					t.Errorf("arguments = %q, want an empty object", e.ToolArgs)
				}
			},
		},
		{
			name: "turn finished",
			wire: map[string]any{"type": "response.done", "response": map[string]any{
				"status": "completed",
				"usage":  map[string]any{"input_tokens": 12, "output_tokens": 30, "total_tokens": 42},
			}},
			want: EventTypeResponseDone,
			check: func(t *testing.T, e Event) {
				if e.Status != "completed" {
					t.Errorf("status = %q", e.Status)
				}
				if e.Usage.TotalTokens != 42 {
					t.Errorf("usage = %+v", e.Usage)
				}
			},
		},
		{
			name: "provider error",
			wire: map[string]any{"type": "error", "error": map[string]any{
				"code": "rate_limit_exceeded", "message": "slow down"}},
			want: EventTypeError,
			check: func(t *testing.T, e Event) {
				if e.Text != "slow down" {
					t.Errorf("message = %q", e.Text)
				}
				// Recoverable: the session is still usable.
				if e.IsFatal {
					t.Error("a recoverable error was marked fatal")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProvider(t, acceptSession)
			session := testSession(t, f, OpenAIProfile())
			if err := session.Start(t.Context(), basicConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)

			f.send(tt.wire)
			event := awaitEvent(t, session, tt.want)
			if tt.check != nil {
				tt.check(t, event)
			}
		})
	}
}

func TestUndecodableAudioIsReportedRatherThanPlayed(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.sendRaw(`{"type":"response.output_audio.delta","delta":"not!base64!"}`)

	event := awaitEvent(t, session, EventTypeError)
	if event.IsFatal {
		t.Error("one bad frame killed the session")
	}
}

//
// Barge-in.
//

// Where the provider stops on its own, telling it again would be noise; what
// it does need is how much the caller actually heard.
func TestInterruptOnAProviderThatCancelsItself(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_7", "type": "message"}})
	time.Sleep(50 * time.Millisecond)

	if err := session.Interrupt(InterruptReasonSpeech, 640); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	truncate := f.awaitMessage("conversation.item.truncate")
	if truncate["item_id"] != "item_7" {
		t.Errorf("truncated %v, want the response the caller was hearing", truncate["item_id"])
	}
	if truncate["audio_end_ms"] != float64(640) {
		t.Errorf("audio_end_ms = %v, want what was actually played", truncate["audio_end_ms"])
	}
	f.refuteMessage("response.cancel")
}

// Where it does not, a missed cancel leaves the model talking over the caller.
func TestInterruptOnAProviderThatMustBeTold(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, QwenProfile())

	cfg := basicConfig()
	cfg.InputFormat, cfg.OutputFormat = QwenProfile().FormatsFor(media.LawMu)
	if err := session.Start(t.Context(), cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	if err := session.Interrupt(InterruptReasonDTMF, 0); err != nil {
		t.Fatalf("interrupt: %v", err)
	}

	f.awaitMessage("response.cancel")
}

// The interruption surfaces where the provider confirms it, not from the call
// that requested it. Emitting from Interrupt would deadlock: it is normally
// called from the goroutine draining this very stream.
func TestACancelledTurnIsReportedAsAnInterruption(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})

	event := awaitEvent(t, session, EventTypeInterrupted)
	if event.Status != "cancelled" {
		t.Errorf("status = %q", event.Status)
	}
}

// Interrupting must not block, including when called from the event consumer,
// which is where a barge-in is actually detected.
func TestInterruptDoesNotBlockTheEventConsumer(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	// Drain from one goroutine and interrupt from inside that same loop, which
	// is exactly how the bridge is wired.
	done := make(chan error, 1)
	go func() {
		for event := range session.Events() {
			if event.Type == EventTypeSpeechStarted {
				done <- session.Interrupt(InterruptReasonSpeech, 200)
				return
			}
		}
		done <- errors.New("the stream ended before speech was detected")
	}()

	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interrupt from the consumer goroutine: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupting from the event consumer deadlocked")
	}
}

//
// Tool results and steering.
//

func TestToolResultCarriesTheHintAndAsksForTheNextTurn(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	err := session.SendToolResult("fc_1", `{"ok":1,"balance":"42.10"}`,
		"Tell the caller the balance, then ask if they want to pay now.")
	if err != nil {
		t.Fatalf("send tool result: %v", err)
	}

	item := nested(t, f.awaitMessage("conversation.item.create"), "item").(map[string]any)
	if item["call_id"] != "fc_1" {
		t.Errorf("call_id = %v", item["call_id"])
	}

	var output map[string]any
	if err := json.Unmarshal([]byte(item["output"].(string)), &output); err != nil {
		t.Fatalf("the tool output is not valid JSON: %v", err)
	}
	if output["balance"] != "42.10" {
		t.Errorf("the result lost its own fields: %v", output)
	}
	if !strings.Contains(output["hint"].(string), "ask if they want to pay") {
		t.Errorf("the hint did not reach the model: %v", output["hint"])
	}

	// The model does not speak again until asked.
	f.awaitMessage("response.create")
}

func TestMergeHint(t *testing.T) {
	tests := []struct {
		name   string
		output string
		hint   string
		want   string
	}{
		{"no hint leaves the result alone", `{"ok":1}`, "", `{"ok":1}`},
		{"hint joins a JSON result", `{"ok":1}`, "say hello", `{"hint":"say hello","ok":1}`},
		{"a plain result gets wrapped", `not json`, "say hello",
			`{"hint":"say hello","result":"not json"}`},
		{"a JSON array gets wrapped", `[1,2]`, "say hello",
			`{"hint":"say hello","result":"[1,2]"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeHint(tt.output, tt.hint); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

// Re-sending the whole configuration mid-call would re-assert turn detection,
// which at least one provider rejects once audio is flowing.
func TestUpdateInstructionsSendsOnlyTheInstructions(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	f.awaitMessage("session.update")

	if err := session.UpdateInstructions("You are now confirming the appointment."); err != nil {
		t.Fatalf("update instructions: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		messages := f.messages()
		for _, message := range messages {
			if message["type"] != "session.update" {
				continue
			}
			session := message["session"].(map[string]any)
			if session["instructions"] != "You are now confirming the appointment." {
				continue
			}
			if _, hasAudio := session["audio"]; hasAudio {
				t.Error("the instruction update re-asserted the audio configuration")
			}
			if _, hasTools := session["tools"]; hasTools {
				t.Error("the instruction update re-sent the tools")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the instruction update never arrived")
}

//
// Failure handling.
//

// There is no reconnect: the provider holds conversation state that cannot be
// rebuilt, so the call has to go somewhere a person can take it.
func TestALostConnectionIsFatalAndClosesTheStream(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.hangUp()

	event := awaitEvent(t, session, EventTypeError)
	if !event.IsFatal {
		t.Error("a lost connection was not reported as fatal")
	}
	awaitEvent(t, session, EventTypeClosed)

	select {
	case _, ok := <-session.Events():
		if ok {
			t.Error("events continued after the session closed")
		}
	case <-time.After(time.Second):
		t.Error("the event stream was never closed")
	}
}

func TestSendingAfterCloseFails(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}

	_ = session.Close(t.Context())
	// Close is idempotent; the call teardown path may reach it twice.
	_ = session.Close(t.Context())

	if err := session.SendAudio([]byte{1, 2}); err == nil {
		t.Error("audio was accepted after the session closed")
	}
}

// A provider that accepts a turn and then goes quiet would otherwise leave the
// flow waiting for a completion that never comes, and the caller in silence.
func TestAnAbandonedTurnIsClosedOut(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	session.firstAudioDeadline = 150 * time.Millisecond
	session.deltaStallDeadline = 150 * time.Millisecond

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)

	event := awaitEvent(t, session, EventTypeResponseDone)
	if event.Status != StatusStalled {
		t.Errorf("status = %q, want the turn closed out as stalled", event.Status)
	}
}

func TestATurnThatCompletesDoesNotTripTheWatchdog(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, OpenAIProfile())
	session.firstAudioDeadline = 200 * time.Millisecond
	session.deltaStallDeadline = 200 * time.Millisecond

	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "response.created"})
	f.send(map[string]any{"type": "response.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString([]byte("hi"))})
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})

	event := awaitEvent(t, session, EventTypeResponseDone)
	if event.Status != "completed" {
		t.Fatalf("status = %q", event.Status)
	}

	// Well past both deadlines, nothing further should be invented.
	time.Sleep(400 * time.Millisecond)
	select {
	case event := <-session.Events():
		t.Errorf("the watchdog fired on a completed turn: %+v", event)
	default:
	}
}

func TestMissingCredentialFailsBeforeAnyCall(t *testing.T) {
	profile := OpenAIProfile()
	t.Setenv(profile.APIKeyEnv, "")

	if _, err := New(profile, nil); err == nil {
		t.Error("a session was built with no credential")
	}
}

// A bot names its own voice; the profile's is only the fallback. Both dialects
// carry it, because the deployment that runs either one has bots of its own.
func TestTheSessionsVoiceOverridesTheProfiles(t *testing.T) {
	for _, tt := range []struct {
		name    string
		profile Profile
		path    []string
	}{
		{"GA dialect", OpenAIProfile(), []string{"session", "audio", "output", "voice"}},
		{"older dialect", QwenProfile(), []string{"session", "voice"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := basicConfig()
			cfg.Voice = "cherry"
			client := &Realtime{profile: tt.profile}

			update := client.buildSessionUpdate(cfg, false)
			if got := nested(t, update, tt.path...); got != "cherry" {
				t.Errorf("voice = %v, want the bot's own", got)
			}

			// Naming none leaves the provider's default in place.
			cfg.Voice = ""
			update = client.buildSessionUpdate(cfg, false)
			if got := nested(t, update, tt.path...); got != tt.profile.Voice {
				t.Errorf("voice = %v, want the profile's %q", got, tt.profile.Voice)
			}
		})
	}
}
