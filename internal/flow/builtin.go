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
	// ToolRecordLead records what a campaign call concluded about the
	// customer. It exists only on campaign calls (Runtime.OfferRecordLead).
	ToolRecordLead = "record_lead"
)

// Lead intents, the values record_lead's intent argument accepts. They are the
// campaign contract enum (campaign.LeadIntent), spelled out here because the
// flow package does not import the campaign domain.
const (
	LeadHigh      = "HIGH"
	LeadMedium    = "MEDIUM"
	LeadLow       = "LOW"
	LeadNone      = "NONE"
	LeadDoNotCall = "DO_NOT_CALL"
)

// LeadIntents is the closed set of record_lead intents.
var LeadIntents = []string{LeadHigh, LeadMedium, LeadLow, LeadNone, LeadDoNotCall}

// MaxLeadNotesRunes caps record_lead's notes. A lead note is a sentence or two
// an agent reads before the follow-up call, and it becomes the message of the
// callback a HIGH lead files; a thousand characters holds a full summary in
// Chinese or English while keeping a model's rambling (or a prompt-injected
// caller's) out of the callbacks table. Counted in runes, not bytes, so the
// cap means the same in both languages and never cuts a character in half.
const MaxLeadNotesRunes = 1000

// BuiltinToolNames is the closed set, for validating what a phase allows.
var BuiltinToolNames = []string{ToolTransferToAgent, ToolTakeMessage, ToolHangup, ToolRecordLead}

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
	// FromPhase is the phase the call was in when the transfer was asked
	// for, before any move its result causes: where the call goes back to if
	// the transfer never happens (Engine.ReturnTo). The runtime fills it; it
	// is not a tool argument.
	FromPhase string `json:"-"`
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

// LeadRequest is the model recording the stance of a campaign customer. Intent
// is one of LeadIntents and Notes is within MaxLeadNotesRunes by the time the
// application sees it; the runtime has judged both.
type LeadRequest struct {
	Intent string
	Notes  string
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
	// flow's conditions read. Nothing else may be copied into slots.
	Fields map[string]any
	// ModelOnly is merged into the result the model sees and into the
	// transcript, and is never written to a slot: the passages a knowledge
	// base returned must not outlive the turn or be rendered into a later
	// instruction (http.modelOnly).
	ModelOnly map[string]any
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

// TranscriptOutput is a tool output as the transcript records it: without the
// hint. The hint is instruction text for the model (a phase's instruction, a
// line to say exactly), which is not the tool's answer and must not reach the
// CDR or anyone reading it. An output that is not a JSON object, or has no
// hint, is returned unchanged.
func TranscriptOutput(output string) string {
	var asObject map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &asObject); err != nil || asObject == nil {
		return output
	}
	if _, ok := asObject["hint"]; !ok {
		return output
	}
	delete(asObject, "hint")
	stripped, err := json.Marshal(asObject)
	if err != nil {
		return output
	}
	return string(stripped)
}

// asToolOutput renders a result as the JSON the model receives.
func (r Result) asToolOutput() string {
	payload := map[string]any{"ok": boolAsFlag(r.IsOK)}
	maps.Copy(payload, r.Fields)
	maps.Copy(payload, r.ModelOnly)
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
	// RecordLead stores the lead of a campaign call, overwriting an earlier
	// one. It is only reached on a call that offers record_lead.
	RecordLead(ctx context.Context, request LeadRequest) (Result, error)
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
	leadDescription := "Record what this call concluded about the customer. Call it as soon as " +
		"their stance is clear and again whenever it changes; the last call counts. Intent: HIGH = " +
		"wants the offer or a follow-up from a person, MEDIUM = interested but undecided, LOW = " +
		"barely interested, NONE = no interest, DO_NOT_CALL = asks not to be called again. Notes: " +
		"what they said, in one or two sentences. Never tell the customer you are recording this."
	if isZH {
		leadDescription = "记录本次通话对客户的判断。客户态度明确后立即调用，态度变化时再次调用，以最后一次为准。" +
			"intent：HIGH=想要优惠或希望人工跟进，MEDIUM=有兴趣但未决定，LOW=兴趣不大，" +
			"NONE=没有意向，DO_NOT_CALL=要求不要再来电。notes：客户的原话要点，一两句话。" +
			"不要告诉客户你在记录。"
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
					"callbackNumber": {"type": "string", "description": "Only when the caller gives another number to be called back on: its digits, nothing else (for example 13800138000). Leave it empty when they want to be called back on the number they are calling from; never write words that describe a number."}
				},
				"required": ["message"]
			}`),
		},
		ToolRecordLead: {
			name:        ToolRecordLead,
			Description: Text{EN: leadDescription, ZH: leadDescription},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"intent": {"type": "string", "description": "The customer's stance", "enum": ["HIGH", "MEDIUM", "LOW", "NONE", "DO_NOT_CALL"]},
					"notes": {"type": "string", "description": "What the customer said, one or two sentences", "maxLength": 1000}
				},
				"required": ["intent"]
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
