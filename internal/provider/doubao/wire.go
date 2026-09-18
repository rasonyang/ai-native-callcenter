// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// The wire, in both directions. One JSON event per text frame; audio is base64
// inside the JSON going up and coming down.

// model is the conversation model this protocol is spoken to. It is a protocol
// version rather than a product name — the engine behind it is chosen in the
// console, not here — so it is a constant and the profile's Model is ignored.
const model = "1.2.6.1"

// The audio this client speaks, fixed rather than negotiated.
//
// inputFormat is documented. outputFormat is NOT: the documented "pcm" is
// 32-bit float in [-1,1], measured, and the value that gives signed 16-bit is
// one only the vendor's own demo uses. Every telephone leg in this application
// carries linear 16-bit, so that is the one worth having — and the guard on a
// turn that carried no audio is what catches the day it stops meaning this.
const (
	inputFormat  = "pcm"
	outputFormat = "pcm_s16le"
	inputRateHz  = 16000
	outputRateHz = 24000
)

//
// Upstream.
//

// sessionEvent is session.create and session.update, which carry the same
// body. extension is a sibling of session, not a field of it.
type sessionEvent struct {
	Type      string           `json:"type"`
	EventID   string           `json:"event_id"`
	Session   sessionBody      `json:"session"`
	Extension extensionOptions `json:"extension"`
}

type sessionBody struct {
	Model        string     `json:"model"`
	Instructions string     `json:"instructions"`
	Audio        audioBody  `json:"audio"`
	Tools        []toolBody `json:"tools,omitempty"`
}

type audioBody struct {
	Input  audioInput  `json:"input"`
	Output audioOutput `json:"output"`
}

type audioInput struct {
	Format formatBody `json:"format"`
}

type audioOutput struct {
	Format formatBody `json:"format"`
	Voice  string     `json:"voice"`
}

type formatBody struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

// toolBody is the flat shape this API takes: no nested "function" object.
type toolBody struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// extensionOptions carries the three per-subsystem option objects. They are
// empty and they are required: the session is refused without them.
type extensionOptions struct {
	ASR    struct{} `json:"asr"`
	TTS    struct{} `json:"tts"`
	Dialog struct{} `json:"dialog"`
}

// simpleEvent is every upstream event that is nothing but its own name:
// response.cancel, session.close and the two that declare a hold on the uplink.
type simpleEvent struct {
	Type    string `json:"type"`
	EventID string `json:"event_id"`
}

// speakEvent hands the engine words to say. There is no other way to make it
// speak on this protocol: it answers audio, and nothing else.
type speakEvent struct {
	Type    string `json:"type"`
	EventID string `json:"event_id"`
	Text    string `json:"text"`
}

// toolResultEvent answers a set of function calls. One message carries them
// all, in the order the calls arrived.
type toolResultEvent struct {
	Type    string           `json:"type"`
	EventID string           `json:"event_id"`
	Items   []toolResultItem `json:"items"`
}

type toolResultItem struct {
	CallID  string        `json:"call_id"`
	Role    string        `json:"role"`
	Content []itemContent `json:"content"`
}

type itemContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// buildSession assembles the configuration. session.create and session.update
// take the identical body, and tools are a full overwrite on this API: an
// update that carried only the instructions would silently drop every tool the
// flow has.
func (s *Session) buildSession(eventType, instructions string) sessionEvent {
	s.mu.Lock()
	tools := s.tools
	voice := s.voice
	s.mu.Unlock()

	return sessionEvent{
		Type:    eventType,
		EventID: s.nextEventID(),
		Session: sessionBody{
			Model:        model,
			Instructions: instructions,
			Audio: audioBody{
				Input: audioInput{Format: formatBody{Type: inputFormat, Rate: inputRateHz}},
				Output: audioOutput{
					Format: formatBody{Type: outputFormat, Rate: outputRateHz},
					Voice:  voice,
				},
			},
			Tools: tools,
		},
	}
}

// buildTools renders the flow's tools in the flat shape this API takes.
func buildTools(tools []provider.ToolSpec) []toolBody {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolBody, 0, len(tools))
	for _, tool := range tools {
		parameters := json.RawMessage(`{"type":"object","properties":{}}`)
		if len(tool.Parameters) > 0 {
			parameters = tool.Parameters
		}
		out = append(out, toolBody{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
		})
	}
	return out
}

// nextEventID is the id every upstream event but audio carries. The provider
// echoes it, which is what makes a frame and the answer to it matchable in a
// provider-side trace; audio is exempt because fifty ids a second would bury
// one and there is nothing to match them against.
func (s *Session) nextEventID() string {
	return "evt_" + strconv.FormatUint(s.eventSeq.Add(1), 10)
}

//
// Downstream.
//

// wireEvent is this protocol's event shape.
//
// It is decoded into a struct rather than a map because audio deltas arrive
// tens of times a second per call, and decoding those into maps is measurable
// allocation churn.
type wireEvent struct {
	Type    string `json:"type"`
	EventID string `json:"event_id"`

	ItemID string `json:"item_id"`
	// Delta carries the caller's transcript so far (cumulative), the model's
	// next words, or base64 audio, depending on the event.
	Delta string `json:"delta"`
	Text  string `json:"text"`

	// TTSType says which of the engine's mouths spoke: "chat_tts_text" is a
	// line this client handed it, anything else is the model's own turn.
	TTSType    string `json:"tts_type"`
	QuestionID string `json:"question_id"`
	ResponseID string `json:"response_id"`

	Items    []wireToolCall `json:"items"`
	Response *wireResponse  `json:"response"`
	Error    *wireError     `json:"error"`
}

// wireToolCall is one function call. Arguments are never streamed: the whole
// set arrives at once and may not match the schema that was declared.
type wireToolCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireResponse carries what a turn cost and nothing else — no id, no status.
type wireResponse struct {
	Usage *wireUsage `json:"usage"`
}

type wireUsage struct {
	TotalTokens  int `json:"total_tokens"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// wireError is what the provider refuses with. The code is an eight-digit
// string whose first digit is the only part worth classifying on: 4 is ours to
// fix, 5 is theirs, and neither is recoverable inside a phone call.
type wireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *wireError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return e.Message
}

// kind is how the code reads in a log line.
func (e *wireError) kind() string {
	if e.Code == "" {
		return "unclassified"
	}
	switch e.Code[0] {
	case '4':
		return "client"
	case '5':
		return "server"
	default:
		return "unclassified"
	}
}

// codeIdleRelease is the provider giving up on a session nobody has spoken to
// for ten minutes. It is worth its own words in the log: it says the call was
// silent, not that anything went wrong with it.
const codeIdleRelease = "45000003"

// sendEvent encodes one outbound event and writes it.
func (s *Session) sendEvent(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbound event: %w", err)
	}
	return s.sendFrame(data)
}

// receive reads and decodes the next event.
func (s *Session) receive() (*wireEvent, []byte, error) {
	messageType, data, err := s.conn.Receive()
	if err != nil {
		return nil, nil, err
	}
	if messageType != websocket.TextMessage {
		return nil, data, nil
	}

	var event wireEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, data, fmt.Errorf("decode inbound event: %w", err)
	}
	return &event, data, nil
}
