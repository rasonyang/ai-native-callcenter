// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"unicode/utf8"
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

// Hangup reasons, the values hangup's reason argument accepts (#484). The model
// says why it ends the call, in one of these words. The reason is an audit of
// that choice, kept in the log and the call's record; it claims nothing about
// what was said or heard, and nothing but its presence is judged.
const (
	// HangupCallerGoodbye: the caller said goodbye.
	HangupCallerGoodbye = "CALLER_GOODBYE"
	// HangupCallerDeclinedMoreHelp: asked whether they need anything else, the
	// caller said no.
	HangupCallerDeclinedMoreHelp = "CALLER_DECLINED_MORE_HELP"
	// HangupCallerAskedToEnd: the caller asked to end the call, or said this
	// call is a mistake (the wrong number, a call they did not mean to make,
	// someone else they were looking for).
	HangupCallerAskedToEnd = "CALLER_ASKED_TO_END"
	// HangupFlowInstructed: the current phase's instructions say to end the
	// call now (a wrong party, a DO_NOT_CALL, a refusal to talk).
	HangupFlowInstructed = "FLOW_INSTRUCTED"
)

// HangupReasons is the closed set of hangup reasons.
var HangupReasons = []string{HangupCallerGoodbye, HangupCallerDeclinedMoreHelp,
	HangupCallerAskedToEnd, HangupFlowInstructed}

// MaxLeadNotesRunes caps record_lead's notes. A lead note is a sentence or two
// an agent reads before the follow-up call, and it becomes the message of the
// callback a HIGH lead files; a thousand characters holds a full summary in
// Chinese or English while keeping a model's rambling (or a prompt-injected
// caller's) out of the callbacks table. Counted in runes, not bytes, so the
// cap means the same in both languages and never cuts a character in half.
const MaxLeadNotesRunes = 1000

// leadNotes is notes as a lead keeps them, whoever wrote them: trimmed, and
// cut to MaxLeadNotesRunes rather than refused, because a HIGH lead must not
// be lost to wordy notes. isCut says they were cut.
func leadNotes(notes string) (kept string, isCut bool) {
	kept = strings.TrimSpace(notes)
	if utf8.RuneCountInString(kept) <= MaxLeadNotesRunes {
		return kept, false
	}
	return string([]rune(kept)[:MaxLeadNotesRunes]), true
}

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

// HangupRequest ends the call. Whether the farewell is the model's or the
// phase's announce is decided by the flow (a terminal phase with an announce
// speaks it alone), not by the model's claim about itself, so the request
// carries no claim about speech (#441).
type HangupRequest struct {
	// Reason is why the model ends the call: one of HangupReasons, upper-cased
	// and trimmed, once the runtime has judged it (#484). A hangup over an
	// ending already under way, or in a terminal phase, is not judged (the
	// closing paths answer it as they always did), and carries whatever the
	// model sent, possibly nothing.
	Reason string
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

// IsToolOutputOK reports whether a tool output, as Dispatch returns it,
// carries ok "1" (asToolOutput always writes the flag as a string): the tool ran
// and succeeded. Anything else, such as a refusal, is not.
func IsToolOutputOK(output string) bool {
	var payload struct {
		OK string `json:"ok"`
	}
	return json.Unmarshal([]byte(output), &payload) == nil && payload.OK == "1"
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

// EndingState is what an Actions may also report: whether the call's ending is
// already under way. The runtime asks it before judging a hangup's reason
// (#484): a hangup over an ending already armed, or decided by the platform
// (the time limit), is the closing paths' to answer (a repeat, a refusal while
// the closing line is owed, a goodbye over an interruption), exactly as before
// the reason existed. An Actions without it is never under way.
type EndingState interface {
	IsEndingUnderway() bool
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
	// When, not how fast (#484): "as soon as the caller wants to finish" had a
	// model answer the caller's first question and end the call in the same
	// reply. The guards in the runtime and the orchestrator back this text; the
	// text is what keeps a model from needing them.
	//
	// A mistaken call has a sentence of its own, stated as the caller's intent
	// (#554): as the third of five cases, followed by "never in reply to a
	// question: answer, ask whether there is anything else", flash met a wrong
	// number at the greeting with "No problem. What can I help you with today?"
	// in five of eight first sayings, the very sequence that clause prescribes,
	// though the caller was answering the greeting's question, not asking one.
	hangupDescription := "End the call. Call it only when the caller has said goodbye, has " +
		"asked to end the call, or has said they need nothing more after you asked whether " +
		"there is anything else, or when your current instructions tell you to end the call. " +
		"When the caller says this call is a mistake (they have the wrong number, did not mean " +
		"to call, or were looking for someone else), they are asking to end it: call it " +
		"straight away with reason CALLER_ASKED_TO_END, saying nothing first, with no " +
		"reassurance such as \"no problem\" and without asking again how you can help. " +
		"Never call it in the same reply as an answer to the caller, and never in reply to a " +
		"question the caller asked: answer, ask whether there is anything else, and wait for " +
		"their reply. A caller who only asks whether they have the right number is asking " +
		"such a question. Do not say goodbye before calling it; say goodbye afterwards, when " +
		"the result tells you to."
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
		hangupDescription = "结束通话。只在以下情况调用：来电者已经道别；来电者要求结束通话；" +
			"你问过还有没有其他需要，来电者说没有了；或者当前指令要求你结束通话。" +
			"来电者说这通电话打错了（拨错号码、不小心打来，或者要找的是别人），就是要求结束通话：" +
			"直接调用，reason 用 CALLER_ASKED_TO_END，调用前什么都不要说，" +
			"不要先说“没关系”这类的话，也不要再问有什么可以帮您。" +
			"不要在回答来电者的同一次回复里调用，来电者提问时也不要调用：先回答，" +
			"再问还有没有其他需要，等对方回复。来电者只是问自己有没有打对电话，也是提问。" +
			"调用前不要先道别；工具返回后按提示道别，道别说完线路自动挂断。"
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
			Parameters:  hangupParameters(isZH),
		},
	}
}

// hangupParameters is hangup's schema: the one required argument, reason, an
// enum of HangupReasons with each value's meaning, in the call's language.
func hangupParameters(isZH bool) json.RawMessage {
	description := "Why the call ends. CALLER_GOODBYE: the caller said goodbye. " +
		"CALLER_DECLINED_MORE_HELP: you asked whether they need anything else and they said no. " +
		"CALLER_ASKED_TO_END: the caller asked to end the call, or said this call is a mistake " +
		"(the wrong number, a call they did not mean to make, someone else they were looking for). " +
		"FLOW_INSTRUCTED: your current instructions tell you to end the call now."
	if isZH {
		description = "结束通话的原因。CALLER_GOODBYE：来电者道别了。" +
			"CALLER_DECLINED_MORE_HELP：你问过还有没有其他需要，来电者说没有了。" +
			"CALLER_ASKED_TO_END：来电者要求结束通话，或者说这通电话打错了（拨错号码、不小心打来、要找的是别人）。" +
			"FLOW_INSTRUCTED：当前指令要求你现在结束通话。"
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reason": map[string]any{"type": "string", "description": description, "enum": HangupReasons},
		},
		"required": []string{"reason"},
	})
	if err != nil {
		return json.RawMessage(`{"type": "object", "properties": {}}`)
	}
	return schema
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
