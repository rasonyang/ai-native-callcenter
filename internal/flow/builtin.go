// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"context"
	"encoding/json"
	"maps"
)

// The built-in tools every flow gets. They are not HTTP calls: they ask the
// application to do something to the call itself, which no backend can do.
const (
	// ToolTransferToAgent hands the caller to a person.
	ToolTransferToAgent = "transfer_to_agent"
	// ToolTakeMessage records a callback request.
	ToolTakeMessage = "take_message"
	// ToolHangup ends the call.
	ToolHangup = "hangup"
)

// BuiltinToolNames is the closed set, for validating what a phase allows.
var BuiltinToolNames = []string{ToolTransferToAgent, ToolTakeMessage, ToolHangup}

// TransferRequest is the model asking for a person.
//
// Summary and Slots exist so the agent who picks up already knows what the
// caller wants. A transfer that arrives without them makes the caller repeat
// themselves, which is the thing an AI-native call centre is supposed to stop
// happening.
type TransferRequest struct {
	// Queue is where to send the caller.
	Queue string `json:"queue"`
	// Reason is a flow-defined category.
	Reason string `json:"reason"`
	// Summary is what the conversation established, in the caller's language.
	Summary string `json:"summary"`
	// Slots is what the flow collected.
	Slots map[string]any `json:"slots"`
}

// MessageRequest is the model taking a message instead of transferring.
type MessageRequest struct {
	Message        string `json:"message"`
	CallbackNumber string `json:"callbackNumber"`
}

// HangupRequest ends the call.
type HangupRequest struct {
	// IsFarewellSpoken says whether the model has already said goodbye. When it
	// has not, the application gives it the chance before the line drops.
	IsFarewellSpoken bool `json:"isFarewellSpoken"`
}

// Result is what a tool reports back.
//
// A refusal is a result, not an error: when a queue is closed the right
// outcome is the bot explaining that and offering to take a message, not a
// failure the caller hears as a broken system.
type Result struct {
	// IsOK is what the model reads to decide whether the thing happened.
	IsOK bool
	// Fields are merged into the result the model sees and into the slots the
	// flow's conditions read.
	Fields map[string]any
	// Hint steers the next turn. A transition overrides it with the new
	// phase's instruction, because the flow is authoritative on progression.
	Hint string
	// Error explains a refusal or failure, in the caller's language.
	Error string
}

// Failed builds a refusal.
func Failed(reason, hint string) Result {
	return Result{IsOK: false, Error: reason, Hint: hint}
}

// Succeeded builds a success.
func Succeeded(fields map[string]any, hint string) Result {
	return Result{IsOK: true, Fields: fields, Hint: hint}
}

// asToolOutput renders a result as the JSON the model receives.
func (r Result) asToolOutput() string {
	payload := map[string]any{"ok": boolAsFlag(r.IsOK)}
	maps.Copy(payload, r.Fields)
	if r.Error != "" {
		payload["error"] = r.Error
	}
	if r.Hint != "" {
		payload["hint"] = r.Hint
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"ok":"0","error":"the result could not be encoded"}`
	}
	return string(encoded)
}

// boolAsFlag renders success as "1"/"0". Backends in this domain speak that
// dialect, and flow conditions compare against it.
func boolAsFlag(isOK bool) string {
	if isOK {
		return "1"
	}
	return "0"
}

// Actions is what the application does when the model asks for something only
// the call itself can provide.
//
// Every method may refuse. A transfer to a closed queue comes back as a
// refusal with a reason, and the bot explains it and carries on — which is why
// business hours belong to the queue rather than to a branch in the dialplan.
type Actions interface {
	TransferToAgent(ctx context.Context, request TransferRequest) (Result, error)
	TakeMessage(ctx context.Context, request MessageRequest) (Result, error)
	Hangup(ctx context.Context, request HangupRequest) (Result, error)
}

// builtinSchemas describes the built-ins to the model, in the call's language.
//
// queues are the names the deployment actually has. They become the enum of
// transfer_to_agent's queue argument, because a free-form string is a name the
// model has to invent: asked to put a caller through, one wrote
// "customer_service" — a plausible queue that existed nowhere, so the transfer
// was refused and the caller was told an agent would ring back. Design 02 §6
// specified the enum; it was lost in the writing.
func builtinSchemas(lang string, queues []string) map[string]Tool {
	isZH := lang == LangZH

	transferDescription := "Hand the caller to a human agent. Call this as soon as it is " +
		"needed, without announcing it first; say the transfer line afterwards, when the " +
		"result tells you to."
	messageDescription := "Take a message for a callback, for when no agent can take the call."
	hangupDescription := "End the call. Call this as soon as the caller wants to finish, " +
		"without saying goodbye first; say goodbye afterwards, when the result tells you to."
	if isZH {
		transferDescription = "转接人工坐席。需要转人工时立即调用，不要先播报；" +
			"工具返回后再按提示播报转接话术。"
		messageDescription = "记录留言与回电号码，用于无人接听或非营业时间。"
		hangupDescription = "结束通话。用户表示要结束时立即调用，不要先道别；" +
			"工具返回后再道别，道别说完线路自动挂断。"
	}

	return map[string]Tool{
		ToolTransferToAgent: {
			name:        ToolTransferToAgent,
			Description: Text{EN: transferDescription, ZH: transferDescription},
			Parameters:  transferParameters(queues),
		},
		ToolTakeMessage: {
			name:        ToolTakeMessage,
			Description: Text{EN: messageDescription, ZH: messageDescription},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"message": {"type": "string", "description": "What the caller wants passed on"},
					"callbackNumber": {"type": "string", "description": "Where to call back, if different from the caller's number"}
				},
				"required": ["message"]
			}`),
		},
		ToolHangup: {
			name:        ToolHangup,
			Description: Text{EN: hangupDescription, ZH: hangupDescription},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"isFarewellSpoken": {"type": "boolean", "description": "Whether you have already said goodbye"}
				}
			}`),
		},
	}
}

// transferParameters is transfer_to_agent's schema, with the queues this
// deployment has as the enum of the queue argument.
//
// Named queues only when there are some: an enum of nothing is a schema no
// argument can satisfy, and a deployment with no queues configured should let
// the tool run and be refused with a reason, not fail to be called at all.
func transferParameters(queues []string) json.RawMessage {
	queueProperty := `{"type": "string", "description": "Which queue to transfer to"}`
	if len(queues) > 0 {
		encoded, err := json.Marshal(queues)
		if err == nil {
			queueProperty = `{"type": "string", "description": "Which queue to transfer to",` +
				`"enum": ` + string(encoded) + `}`
		}
	}
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"queue": ` + queueProperty + `,
			"reason": {"type": "string", "description": "Why the caller needs a person"},
			"summary": {"type": "string", "description": "What the conversation established, in the caller's language, at most 600 characters"},
			"slots": {"type": "object", "description": "Everything collected so far"}
		},
		"required": ["queue", "reason", "summary"]
	}`)
}
