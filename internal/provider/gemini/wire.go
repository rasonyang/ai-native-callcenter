// SPDX-License-Identifier: Apache-2.0

package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The wire, in both directions.
//
// Client frames are text JSON with lowerCamelCase keys, and exactly one of
// setup, clientContent, realtimeInput and toolResponse at the top level. Server
// frames are JSON too, but they arrive with the BINARY opcode and pretty
// printed, so nothing here reads the opcode or the whitespace — a frame is
// whatever its keys say it is.

const (
	// model is the conversation model this client speaks to. It is pinned here
	// rather than taken from the profile for the reason the other non-Realtime
	// client pins its own: what this code knows how to hold a conversation with
	// is this model's lifecycle, and a deployment that could move it with an
	// environment variable could move it to one that answers differently.
	//
	// Never the -extended-thinking variant: on that model turnComplete no longer
	// means the session is idle, blocking tool calls are a hard error, and both
	// of those are load-bearing here.
	model = "gemini-3.8-live"
	// wireModel is the same name in the form the protocol asks for.
	wireModel = "models/" + model

	// defaultEndpoint is where the API lives when a deployment says nothing.
	// The version is part of the path: v1beta is what the reference and the
	// WebSocket tutorial both document, and what the measurements were taken
	// against.
	defaultEndpoint = "wss://generativelanguage.googleapis.com/ws/" +
		"google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

	// apiKeyHeader carries the credential. The documented alternative is a
	// ?key= query parameter, which would put the credential into every URL this
	// process logs; a header cannot be logged by accident.
	apiKeyHeader = "x-goog-api-key"

	// behaviorBlocking makes the model wait for a tool result before it says
	// anything else. It is on EVERY declaration deliberately: this model's
	// default is NON_BLOCKING, which lets it keep talking while a transfer is
	// being arranged, and every tool this application has is a decision the
	// conversation cannot run ahead of.
	behaviorBlocking = "BLOCKING"

	// activityInterrupts is what makes the caller able to talk over the model.
	// It is the documented default and is sent anyway, because barge-in is not
	// something to inherit quietly.
	activityInterrupts = "START_OF_ACTIVITY_INTERRUPTS"

	// audioModality is the only response modality these models have. Text comes
	// back as the transcription of the audio, not instead of it.
	audioModality = "AUDIO"
)

//
// Upstream.
//

// setupFrame is the first frame and the only one of its kind. Nothing else may
// be sent until it has been answered.
type setupFrame struct {
	Setup setupBody `json:"setup"`
}

// setupBody is the whole configuration of the session.
//
// What is NOT here is as measured as what is: thinkingConfig (closed 1007,
// "Thinking level is not supported for this model"), proactivity (permanently
// on, and false is an error), enableAffectiveDialog (removed from the API),
// sessionResumption and contextWindowCompression (this client never resumes —
// a lost socket ends the call), and speechConfig.languageCode (the native-audio
// models refuse to be told a language; the flow's language reaches the model
// through its instructions). responseModalities lives inside generationConfig
// and nowhere else: at the top level the setup is rejected with close 1007,
// "Unknown name \"responseModalities\" at 'setup'".
type setupBody struct {
	Model             string              `json:"model"`
	GenerationConfig  generationConfig    `json:"generationConfig"`
	SystemInstruction *content            `json:"systemInstruction,omitempty"`
	Tools             []toolDeclarations  `json:"tools,omitempty"`
	RealtimeInput     realtimeInputConfig `json:"realtimeInputConfig"`
	// The two transcription configs are empty objects, which is how this
	// protocol says "on, with your defaults". Without the output one there is no
	// record of what the bot said; without the input one, none of what the
	// caller said.
	InputTranscription  emptyObject `json:"inputAudioTranscription"`
	OutputTranscription emptyObject `json:"outputAudioTranscription"`
}

// emptyObject is a field whose presence is the whole message.
type emptyObject struct{}

type generationConfig struct {
	ResponseModalities []string      `json:"responseModalities"`
	SpeechConfig       *speechConfig `json:"speechConfig,omitempty"`
}

type speechConfig struct {
	VoiceConfig voiceConfig `json:"voiceConfig"`
}

type voiceConfig struct {
	PrebuiltVoiceConfig prebuiltVoiceConfig `json:"prebuiltVoiceConfig"`
}

type prebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName"`
}

// content is a turn, or the standing instructions. Only text parts are ever
// sent: this client says things, it does not send pictures.
type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type toolDeclarations struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Behavior    string          `json:"behavior"`
}

type realtimeInputConfig struct {
	AutomaticActivityDetection activityDetection `json:"automaticActivityDetection"`
	ActivityHandling           string            `json:"activityHandling"`
}

// activityDetection configures the server's own voice activity detection, which
// is the only thing that decides turns here. disabled is sent explicitly: its
// default is what this client wants, and a default that matters is worth saying.
type activityDetection struct {
	Disabled bool `json:"disabled"`
	// SilenceDurationMs is the pause that ends the caller's turn. Omitted, the
	// server uses about 800 ms; the flow's own value is what a deployment tuned.
	SilenceDurationMs *int `json:"silenceDurationMs,omitempty"`
}

// clientContentFrame is a turn from this side, appended to the conversation.
//
// With turnComplete it asks for an answer AND pre-empts whatever is being said
// — unconditionally, measured — which is the only interruption primitive this
// protocol has.
type clientContentFrame struct {
	ClientContent clientContentBody `json:"clientContent"`
}

type clientContentBody struct {
	Turns        []content `json:"turns"`
	TurnComplete bool      `json:"turnComplete"`
}

// toolResponseFrame answers function calls. One frame carries the whole set,
// matched to the calls by id.
type toolResponseFrame struct {
	ToolResponse toolResponseBody `json:"toolResponse"`
}

type toolResponseBody struct {
	FunctionResponses []functionResponse `json:"functionResponses"`
}

// functionResponse carries the result under "output", which is the key this API
// reads as the function's return value. Anything else in "response" is treated
// as the output wholesale, so the phase hint that rides along with a result
// would end up indistinguishable from it.
type functionResponse struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response functionOutput `json:"response"`
}

type functionOutput struct {
	Output json.RawMessage `json:"output"`
}

// streamEndFrame says the caller's side has gone quiet, which flushes whatever
// audio the server had cached. It is a fixed frame, so it is a constant.
const streamEndFrame = `{"realtimeInput":{"audioStreamEnd":true}}`

// buildSetup assembles the session. The voice is the flow's, falling back to the
// profile's; with neither, the field is left out and the server picks one.
func (s *Session) buildSetup() setupFrame {
	s.mu.Lock()
	instructions := s.baseInstructions
	voice := s.voice
	tools := s.tools
	silenceMs := s.silenceMs
	s.mu.Unlock()

	body := setupBody{
		Model: wireModel,
		GenerationConfig: generationConfig{
			ResponseModalities: []string{audioModality},
		},
		Tools: tools,
		RealtimeInput: realtimeInputConfig{
			AutomaticActivityDetection: activityDetection{Disabled: false},
			ActivityHandling:           activityInterrupts,
		},
	}
	if voice != "" {
		body.GenerationConfig.SpeechConfig = &speechConfig{
			VoiceConfig: voiceConfig{
				PrebuiltVoiceConfig: prebuiltVoiceConfig{VoiceName: voice}}}
	}
	if instructions != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: instructions}}}
	}
	if silenceMs > 0 {
		hold := silenceMs
		body.RealtimeInput.AutomaticActivityDetection.SilenceDurationMs = &hold
	}
	return setupFrame{Setup: body}
}

// buildTools renders the flow's tools as function declarations.
func buildTools(tools []provider.ToolSpec) []toolDeclarations {
	if len(tools) == 0 {
		return nil
	}
	declarations := make([]functionDeclaration, 0, len(tools))
	for _, tool := range tools {
		declarations = append(declarations, functionDeclaration{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  geminiSchema(tool.Parameters),
			Behavior:    behaviorBlocking,
		})
	}
	return []toolDeclarations{{FunctionDeclarations: declarations}}
}

// geminiSchema is the flow's JSON Schema in the form this API's schema type
// takes.
//
// The difference is one field: "type" here is a protobuf enum, whose JSON names
// are STRING, OBJECT, ARRAY and the rest, while every schema in this repository
// is written the way JSON Schema spells them — lowercase. Proto3 JSON matches
// enum names case-sensitively, so the names are rewritten, recursively, and
// nothing else about the schema is touched: a keyword this API does not know is
// left in place to be refused at setup with a message that names it, which is a
// better failure than a field silently dropped.
//
// Re-encoding sorts the object keys, because a schema is a set of keywords
// rather than a sequence of them.
func geminiSchema(parameters json.RawMessage) json.RawMessage {
	if len(parameters) == 0 {
		return nil
	}
	var schema any
	if err := json.Unmarshal(parameters, &schema); err != nil {
		// A flow's schema is checked for validity at load, so this cannot
		// normally happen; passing the bytes through unchanged leaves the
		// provider to refuse them, which is the honest failure.
		return parameters
	}
	rewritten, err := json.Marshal(upperCaseSchemaTypes(schema))
	if err != nil {
		return parameters
	}
	return rewritten
}

// upperCaseSchemaTypes walks a decoded schema and puts every "type" into the
// casing the enum uses.
func upperCaseSchemaTypes(node any) any {
	object, isObject := node.(map[string]any)
	if !isObject {
		if list, isList := node.([]any); isList {
			for i, item := range list {
				list[i] = upperCaseSchemaTypes(item)
			}
		}
		return node
	}

	for key, value := range object {
		switch key {
		case "type":
			object[key] = upperCaseTypeName(value)
		case "properties", "$defs", "definitions":
			// A map from a name to a schema: the names are the flow's, and only
			// what they point at is a schema.
			object[key] = upperCaseSchemaMap(value)
		case "items", "additionalProperties", "anyOf", "oneOf", "allOf", "prefixItems":
			object[key] = upperCaseSchemaTypes(value)
		}
	}
	return object
}

// upperCaseSchemaMap walks the values of a name-to-schema map, leaving the
// names — which are argument names, not keywords — alone.
func upperCaseSchemaMap(node any) any {
	object, isObject := node.(map[string]any)
	if !isObject {
		return node
	}
	for name, schema := range object {
		object[name] = upperCaseSchemaTypes(schema)
	}
	return object
}

// upperCaseTypeName handles both forms a type takes: one name, or a list of them
// where the first that is not "null" is the one this API can express.
func upperCaseTypeName(value any) any {
	switch typed := value.(type) {
	case string:
		return strings.ToUpper(typed)
	case []any:
		for _, name := range typed {
			text, isText := name.(string)
			if isText && strings.ToLower(text) != "null" {
				return strings.ToUpper(text)
			}
		}
	}
	return value
}

// userTurn is one thing said from this side, asked for as a turn of its own.
func userTurn(text string) clientContentFrame {
	return clientContentFrame{ClientContent: clientContentBody{
		Turns:        []content{{Role: "user", Parts: []part{{Text: text}}}},
		TurnComplete: true,
	}}
}

// openingTurn asks the model to speak first with nothing to answer.
//
// An empty turn list with turnComplete is what makes this model open a call:
// the Live API waits for input before it says anything, and the instructions
// are what it then greets from. The turns field must be present and empty
// rather than absent, which is why it is not omitted.
func openingTurn() clientContentFrame {
	return clientContentFrame{ClientContent: clientContentBody{
		Turns: []content{}, TurnComplete: true}}
}

// audioMimeType is how a frame of caller audio declares its rate. The server
// resamples whatever it is given, so this has to be true rather than convenient.
func audioMimeType(rateHz int) string {
	return fmt.Sprintf("audio/pcm;rate=%d", rateHz)
}

//
// Downstream.
//
// The protocol carries exactly one of these per frame, apart from usageMetadata
// which may accompany any of them. A frame whose keys are none of these — a
// bare {}, an empty serverContent, a session resumption handle nobody asked
// for, usage on its own — is not an event: it is decoded into zero fields and
// ignored.

type serverFrame struct {
	SetupComplete        *emptyObject          `json:"setupComplete"`
	ServerContent        *serverContent        `json:"serverContent"`
	ToolCall             *toolCall             `json:"toolCall"`
	ToolCallCancellation *toolCallCancellation `json:"toolCallCancellation"`
	GoAway               *goAway               `json:"goAway"`
}

// serverContent is the model's half of the conversation and the boundaries of
// its turns.
//
// generationComplete is the model stopping; turnComplete is the server's own
// accounting of the playback it presumes happened, seconds later. An interrupted
// turn gets no generationComplete at all — interrupted, then turnComplete.
type serverContent struct {
	ModelTurn           *modelTurn     `json:"modelTurn"`
	OutputTranscription *transcription `json:"outputTranscription"`
	InputTranscription  *transcription `json:"inputTranscription"`
	GenerationComplete  bool           `json:"generationComplete"`
	TurnComplete        bool           `json:"turnComplete"`
	Interrupted         bool           `json:"interrupted"`
}

// modelTurn carries the audio. One frame may hold several parts, and every one
// of them is speech the caller is owed.
type modelTurn struct {
	Parts []outputPart `json:"parts"`
}

type outputPart struct {
	InlineData *blob `json:"inlineData"`
}

type blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// transcription is a fragment, not a sentence: this protocol streams the words
// as they are said and never marks the last one.
type transcription struct {
	Text string `json:"text"`
}

type toolCall struct {
	FunctionCalls []functionCall `json:"functionCalls"`
}

// functionCall arrives whole — arguments are never streamed — and several may
// arrive in one frame.
type functionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// toolCallCancellation withdraws calls the server has discarded. It is
// documented and was never observed in any measurement; it is handled because
// the alternative is answering a call that no longer exists.
type toolCallCancellation struct {
	IDs []string `json:"ids"`
}

// goAway is the connection's remaining lifetime. There is no reconnect here, so
// it is the end of the session rather than a warning about one.
type goAway struct {
	TimeLeft string `json:"timeLeft"`
}

// sendFrameOf encodes one outbound frame and writes it.
func (s *Session) sendFrameOf(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbound frame: %w", err)
	}
	return s.sendFrame(data)
}

// receive reads and decodes the next frame. A frame that is not JSON is
// reported as nothing at all: one garbled frame is not a reason to end a call.
func (s *Session) receive() (*serverFrame, []byte, error) {
	_, data, err := s.conn.Receive()
	if err != nil {
		return nil, nil, err
	}

	var frame serverFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		s.log.Warn("could not decode a provider frame", "bytes", len(data), "error", err)
		return nil, data, nil
	}
	return &frame, data, nil
}
