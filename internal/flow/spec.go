// SPDX-License-Identifier: Apache-2.0

// Package flow is the declarative call flow: what phase a conversation is in,
// which tools are available there, and what moves it on. The model owns the
// conversation; the flow owns the phase.
package flow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// SpecVersion is the dialect this package reads.
const SpecVersion = "v2"

// Languages a spec may be written in.
const (
	LangZH = "zh"
	LangEN = "en"
	// DefaultLang is used when a call carries no language of its own.
	DefaultLang = LangEN
)

// Text is a caller-facing string in every language the flow supports.
//
// It decodes from either a bare string, meaning the same text everywhere, or
// an object keyed by language. One flow serves every language: business logic
// is never duplicated per language, only wording.
type Text struct {
	ZH string
	EN string
}

func (t *Text) UnmarshalJSON(data []byte) error {
	var plain string
	if err := json.Unmarshal(data, &plain); err == nil {
		t.ZH, t.EN = plain, plain
		return nil
	}
	var byLang struct {
		ZH string `json:"zh"`
		EN string `json:"en"`
	}
	if err := json.Unmarshal(data, &byLang); err != nil {
		return notKeyedByLanguage("text must be a string", data, err)
	}
	t.ZH, t.EN = byLang.ZH, byLang.EN
	return nil
}

// For resolves the text for a language, falling back to the other rather than
// to nothing: a missing translation should leave the caller hearing the wrong
// language, not silence.
func (t Text) For(lang string) string {
	if lang == LangZH {
		if t.ZH != "" {
			return t.ZH
		}
		return t.EN
	}
	if t.EN != "" {
		return t.EN
	}
	return t.ZH
}

func (t Text) IsEmpty() bool { return t.ZH == "" && t.EN == "" }

// TextList is a list of caller-facing strings per language.
type TextList struct {
	ZH []string
	EN []string
}

func (l *TextList) UnmarshalJSON(data []byte) error {
	var flat []string
	if err := json.Unmarshal(data, &flat); err == nil {
		l.ZH, l.EN = flat, flat
		return nil
	}
	var byLang struct {
		ZH []string `json:"zh"`
		EN []string `json:"en"`
	}
	if err := json.Unmarshal(data, &byLang); err != nil {
		return notKeyedByLanguage("text list must be a list", data, err)
	}
	l.ZH, l.EN = byLang.ZH, byLang.EN
	return nil
}

func (l TextList) For(lang string) []string {
	if lang == LangZH {
		if len(l.ZH) > 0 {
			return l.ZH
		}
		return l.EN
	}
	if len(l.EN) > 0 {
		return l.EN
	}
	return l.ZH
}

// EventType is what a transition can fire on.
type EventType string

const (
	// EventTypeToolResult is a tool having run, whatever its outcome.
	EventTypeToolResult EventType = "TOOL_RESULT"
	// EventTypeNoInput is the caller having said nothing.
	EventTypeNoInput EventType = "NO_INPUT"
)

// Operator is a condition test.
type Operator string

const (
	OpEqual              Operator = "EQ"
	OpNotEqual           Operator = "NE"
	OpGreaterThan        Operator = "GT"
	OpLessThan           Operator = "LT"
	OpGreaterThanOrEqual Operator = "GTE"
	OpLessThanOrEqual    Operator = "LTE"
	OpIsNull             Operator = "IS_NULL"
	OpIsNotNull          Operator = "IS_NOT_NULL"
	OpIsEmpty            Operator = "IS_EMPTY"
	OpIsNotEmpty         Operator = "IS_NOT_EMPTY"
	OpIn                 Operator = "IN"
	OpNotIn              Operator = "NOT_IN"
	OpContains           Operator = "CONTAINS"
	OpNotContains        Operator = "NOT_CONTAINS"
)

var knownOperators = map[Operator]bool{
	OpEqual: true, OpNotEqual: true,
	OpGreaterThan: true, OpLessThan: true,
	OpGreaterThanOrEqual: true, OpLessThanOrEqual: true,
	OpIsNull: true, OpIsNotNull: true,
	OpIsEmpty: true, OpIsNotEmpty: true,
	OpIn: true, OpNotIn: true,
	OpContains: true, OpNotContains: true,
}

// Condition is a test over the slot store. Exactly one form applies: a leaf
// test, an All conjunction, or an Any disjunction. A nil condition always
// holds, which is how an unconditional fallback rule is written.
type Condition struct {
	Slot  string      `json:"slot,omitempty"`
	Op    Operator    `json:"op,omitempty"`
	Value any         `json:"value,omitempty"`
	All   []Condition `json:"all,omitempty"`
	Any   []Condition `json:"any,omitempty"`
}

// Transition moves the conversation to another node.
//
// Rules are evaluated in ascending priority, and equal priorities keep their
// declared order. The first whose event and condition both match wins; when
// none does, the conversation stays where it is and the model tries again.
type Transition struct {
	// On is the event this rule reacts to. Empty matches any event.
	On EventType `json:"on,omitempty"`
	// Tool narrows a TOOL_RESULT rule to one tool. Empty matches any. A
	// NO_INPUT rule cannot name one: a silence carries no tool.
	Tool string `json:"tool,omitempty"`
	// Condition must hold; nil always holds.
	Condition *Condition `json:"condition,omitempty"`
	Target    string     `json:"target"`
	Priority  int        `json:"priority,omitempty"`
}

// Node is one phase of a conversation.
type Node struct {
	// Instruction is what the model should be doing in this phase. It may
	// reference collected values as {slots.name}.
	Instruction Text `json:"instruction"`
	// Announce is the phase's own line, spoken when the conversation enters
	// it. Unlike Instruction it is not a brief the model writes from: it is
	// wording the flow owns, said as written.
	//
	// It exists because not every engine can be talked into a turn. A model
	// asked to open with a sentence usually obliges and sometimes improvises,
	// which is fine for a greeting and not fine for a regulated closing line —
	// and a full-duplex engine that takes no text cue at all cannot be asked
	// in the first place. Where the provider can only approximate this the
	// client says so; where it can speak a line verbatim, it does.
	//
	// It renders {slots.name} exactly as Instruction does, and it is optional:
	// most phases leave the wording to the model.
	Announce Text `json:"announce,omitempty"`
	// Tools is the allowlist for this phase. "*" allows everything; empty
	// allows only the flow's always-available tools.
	Tools       []string     `json:"tools,omitempty"`
	Transitions []Transition `json:"transitions,omitempty"`
	// IsTerminal marks a phase the conversation does not leave.
	IsTerminal bool `json:"isTerminal,omitempty"`
	// Lead is the campaign lead the call records on entering this phase
	// (DeclaredLead). Optional, and ignored on a call that is not a campaign
	// call.
	Lead *DeclaredLead `json:"lead,omitempty"`

	// id is filled in from the map key at load time.
	id string
}

// DeclaredLead is a lead a phase records when the call enters it, on a
// campaign call (Runtime.OfferRecordLead), exactly as a record_lead call
// would: the attempt keeps the last lead written, whether the model wrote it
// or a phase did, and its side effects run once when the call ends.
//
// It exists for the flow that classifies the customer and closes in the same
// move: a tool's transition goes straight into a terminal phase whose announce
// is the goodbye, so the model has no later turn in which to call record_lead,
// and asking it to call record_lead in the same response is a request it is
// free to ignore (#352). A phase is entered by the engine, so the lead it
// declares is recorded whatever the model did.
//
// Every entry records it again, so a phase entered twice writes twice. Only a
// shape the loader can check is accepted: Intent is one of LeadIntents as
// written, and every {slots.x} in Notes must be a slot the flow writes.
type DeclaredLead struct {
	// Intent is one of LeadIntents, in capitals.
	Intent string `json:"intent"`
	// Notes is rendered with {slots.x} when the phase is entered, as an
	// announce is, then trimmed and cut to MaxLeadNotesRunes. Optional; a
	// rendering that comes out empty records empty notes.
	Notes Text `json:"notes,omitempty"`

	// problems are what was wrong with the declaration's shape: decoding never
	// fails on it, so that validate can report it with the phase's name, among
	// the flow's other problems, rather than as a document that cannot be
	// parsed. isIntentUnreadable says one of them is about the intent, which
	// validate then does not judge a second time.
	problems           []string
	isIntentUnreadable bool
}

// declaredLeadFields are the keys a lead declaration may have.
var declaredLeadFields = []string{"intent", "notes"}

// UnmarshalJSON reads a lead declaration. It never fails: what is wrong with
// the shape (not an object, a field it does not have, an intent that is not a
// string, notes that are not text) is kept for validate to report.
func (l *DeclaredLead) UnmarshalJSON(data []byte) error {
	*l = DeclaredLead{}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		l.problems = append(l.problems, `must be an object {"intent", "notes"}`)
		l.isIntentUnreadable = true
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(declaredLeadFields, key) {
			l.problems = append(l.problems, fmt.Sprintf("has unknown field %q; a lead has %s",
				key, strings.Join(declaredLeadFields, " and ")))
		}
	}
	if raw, ok := fields["intent"]; ok {
		if err := json.Unmarshal(raw, &l.Intent); err != nil {
			l.problems = append(l.problems, "intent must be a string")
			l.isIntentUnreadable = true
		}
	}
	if raw, ok := fields["notes"]; ok {
		if err := json.Unmarshal(raw, &l.Notes); err != nil {
			l.problems = append(l.problems, "notes must be a string or an object keyed by language")
		}
	}
	return nil
}

// ID is the node's key in the flow.
func (n Node) ID() string { return n.id }

// SuccessTest decides whether a backend response counts as success.
type SuccessTest struct {
	// Path is a dotted path into the response body.
	Path string `json:"path"`
	// Equals is the value that path must hold.
	Equals string `json:"equals"`
}

// HTTPCall is how a declarative tool reaches a backend.
type HTTPCall struct {
	// BaseURL overrides the deployment's AICC_BOT_BACKEND_BASE for this tool:
	// an absolute http or https URL with no user, query or fragment. Absent
	// keeps the deployment's base. Which hosts it may name is the deployment's
	// outbound host policy (AICC_TOOL_ALLOWED_HOSTS), not the flow's.
	BaseURL string `json:"baseUrl,omitempty"`
	// Path is appended to the base URL's path. It starts with a literal "/" and
	// may reference {args.x} (an argument the tool's schema requires) and
	// {slots.y}; every substituted value is escaped into a single path segment.
	// A literal "?query" in it is sent as written and a "#fragment" dropped,
	// as before templates existed (HTTPCall.urlParts).
	Path string `json:"path"`
	// Method defaults to POST, which is what the reference backends expect.
	Method string `json:"method,omitempty"`
	// Query maps a query parameter to a template value ({args.x}, {slots.y}),
	// encoded as a form value. A parameter whose template references an
	// argument the model did not pass is left out.
	Query map[string]string `json:"query,omitempty"`
	// Headers are sent with every call. A value is a literal, or carries
	// {secrets.NAME} references resolved from AICC_TOOL_SECRET_<NAME> when
	// the call is made; a credential header must use one (see secrets.go).
	Headers map[string]string `json:"headers,omitempty"`
	// TimeoutMs bounds the call, from MinToolTimeoutMs to MaxToolTimeoutMs.
	// Absent means DefaultToolTimeoutMs. A pointer, because an explicit 0 is
	// refused rather than read as absent.
	TimeoutMs *int `json:"timeoutMs,omitempty"`
	// Body is a template: string values may reference {args.x} and {slots.y}.
	// Absent sends "{}" for POST, PUT and PATCH, as it always has, and no body
	// for any other method.
	Body map[string]any `json:"body,omitempty"`
	// SuccessWhen decides success; absent means any 2xx counts.
	SuccessWhen *SuccessTest `json:"successWhen,omitempty"`
	// ErrorFrom is the dotted path to the backend's own error: its message
	// when successWhen fails on a 2xx answer, and its error code on a 4xx or
	// 5xx answer (logged and named in the tool's error; the backend's message
	// is never logged or shown to the model).
	ErrorFrom string `json:"errorFrom,omitempty"`
	// Result maps slot names to dotted paths in the response.
	Result map[string]string `json:"result,omitempty"`
	// ModelOnly maps output names to parts of the response that reach the
	// model and the call transcript but are never written to a slot.
	ModelOnly map[string]ModelOnlyField `json:"modelOnly,omitempty"`
}

// ModelOnlyField is one http.modelOnly entry. It is either a dotted path
// (JSON string) whose value passes through as it is, or a projection (JSON
// object {"path", "each"}) that lists the elements of an array, each cut down
// to the fields named in Each.
type ModelOnlyField struct {
	// Path is a dotted path into the response.
	Path string
	// Each maps an output field to a dotted path inside one array element. Set
	// only for a projection.
	Each map[string]string
	// IsProjection tells the object form from the string form.
	IsProjection bool
}

type modelOnlyProjection struct {
	Path string            `json:"path"`
	Each map[string]string `json:"each"`
}

// UnmarshalJSON reads the string form or the strict object form; any other
// JSON type, and any other key, is refused.
func (f *ModelOnlyField) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("modelOnly entry is empty")
	}
	switch trimmed[0] {
	case '"':
		*f = ModelOnlyField{}
		return json.Unmarshal(trimmed, &f.Path)
	case '{':
		var p modelOnlyProjection
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&p); err != nil {
			return fmt.Errorf("modelOnly projection must be {\"path\", \"each\"} with string paths: %w", err)
		}
		*f = ModelOnlyField{Path: p.Path, Each: p.Each, IsProjection: true}
		return nil
	}
	return errors.New("modelOnly entry must be a dotted path (string) or an object {\"path\", \"each\"}")
}

// MarshalJSON writes back the shape that was read.
func (f ModelOnlyField) MarshalJSON() ([]byte, error) {
	if !f.IsProjection {
		return json.Marshal(f.Path)
	}
	return json.Marshal(modelOnlyProjection{Path: f.Path, Each: f.Each})
}

// The bounds of HTTPCall.TimeoutMs. A backend call happens inside a phone call
// with somebody waiting, so the ceiling is ten seconds.
const (
	DefaultToolTimeoutMs = 5000
	MinToolTimeoutMs     = 500
	MaxToolTimeoutMs     = 10000
)

// Timeout is how long the call may take.
func (h *HTTPCall) Timeout() time.Duration {
	if h.TimeoutMs == nil {
		return DefaultToolTimeoutMs * time.Millisecond
	}
	return time.Duration(*h.TimeoutMs) * time.Millisecond
}

// ToolKind is what a flow-declared tool does when the model calls it.
type ToolKind string

const (
	// ToolKindHTTP calls the flow's backend. It is the default, so a tool with
	// no kind is an HTTP tool.
	ToolKindHTTP ToolKind = "http"
	// ToolKindSlot records a fact the caller stated into a declared slot. It
	// makes no call: its only effect is slots.<Slot> = <value>, so the fact
	// outlives the model's own context and can be rendered back into a phase
	// instruction, an announce, an HTTP body or a transition condition.
	ToolKindSlot ToolKind = "slot"
)

// MaxSlotValueLen bounds one recorded value, in characters. A slot tool
// refuses a longer value (the model retries it shorter; half a fact is never
// recorded), while a tool argument kept as <tool>.args.<k> is cut to it,
// because a tool that has already run cannot be refused for what it was
// given. It keeps the pinned instruction bounded.
const MaxSlotValueLen = 256

// Tool is a function the model may call.
type Tool struct {
	// Kind is "http" (the default) or "slot".
	Kind        ToolKind `json:"kind,omitempty"`
	Description Text     `json:"description"`
	// Parameters is a JSON Schema object, passed to the model unchanged. A
	// slot tool takes none of its own: its one argument is "value".
	Parameters json.RawMessage `json:"parameters,omitempty"`
	HTTP       *HTTPCall       `json:"http,omitempty"`
	// Slot is the slot a ToolKindSlot tool writes (slots.<Slot>).
	Slot string `json:"slot,omitempty"`
	// Enum, when set on a slot tool, is the only values it accepts, and is
	// passed to the model as the enum of "value".
	Enum []string `json:"enum,omitempty"`
	// Collect, on an HTTP tool, keeps a list of values across its calls (see
	// Collect). Absent leaves the tool as it was.
	Collect *Collect `json:"collect,omitempty"`

	name string
}

// MaxCollectedValues bounds one collect list. A value beyond it is not added
// (the first MaxCollectedValues are kept) and the engine logs that.
const MaxCollectedValues = 20

// Collect is an opt-in list slot fed by an HTTP tool. Slots otherwise keep
// only the latest tool result, so "the orders looked up in this call" would
// have to be recalled by the model from its history; a collect rule makes the
// engine remember them.
//
// After a call whose result is recorded with ok=1 (and, if When is set, only
// while it holds), Value is rendered ({args.x}, {slots.y}), trimmed, and
// appended to the list unless an equal value is already there. At most
// MaxCollectedValues values are kept, each cut to MaxSlotValueLen characters.
// The slot named by Slot renders ({slots.<Slot>}) as the values joined by
// ", " and is empty while nothing was collected; "<Slot>.count" holds how
// many there are. Both are written before the call's transitions are
// evaluated.
type Collect struct {
	// Slot is the list's name; it may contain dots, as <tool>.<field> does.
	Slot string `json:"slot"`
	// Value is the template for one value.
	Value string `json:"value"`
	// When, if present, must hold against the slots (result.* included) for
	// the value to be collected.
	When *Condition `json:"when,omitempty"`
}

// Name is the tool's key in the flow.
func (t Tool) Name() string { return t.name }

// Global is what applies across every phase.
type Global struct {
	// Persona and Rules become the model's standing instructions.
	Persona Text     `json:"persona"`
	Rules   TextList `json:"rules,omitempty"`
	// Voice is the bot's timbre — part of its character, so it is published
	// with the flow rather than set per deployment.
	//
	// Voice names belong to the provider that answers, and a deployment runs
	// one of those (phase1-decisions A1), so a flow names a voice its own
	// deployment offers. It is deliberately not per language: both providers
	// offer voices that carry Chinese and English equally well, and one bot
	// should not change its voice mid-catalogue. Empty uses the provider's
	// default.
	Voice string `json:"voice,omitempty"`
	// Temperature is the model's sampling temperature, a property of the
	// persona and so published with the flow for the same reason as Voice
	// (A7). It pins the variance of what the model writes freely, such as the
	// summary handed to an agent. Absent leaves the provider's default.
	//
	// The range 0 <= t < 2 is what both engines that take one accept; a
	// provider without the setting (openai, gateway, doubao) ignores it and
	// says so once per process.
	Temperature *float64 `json:"temperature,omitempty"`
	// FallbackTarget is where a conversation goes when it has run too long or
	// lost its way.
	FallbackTarget string `json:"fallbackTarget,omitempty"`
	// ClosingTarget is the flow's own goodbye phase: a terminal node reached
	// when the engine, rather than the model, decides a call is over —
	// repeated silence in a phase where no authored rule can fire on NO_INPUT
	// (see Engine.OnNoInput) and the tool-less conversation wall (see
	// MaxTurnsWithoutTool). It must name a phase with isTerminal true. It is
	// the backstop for a caller asked "anything else?" indefinitely (issue
	// #9), not a replacement for a closing phase that tells the model to hang
	// up on a decline. Empty leaves both guards off.
	ClosingTarget string `json:"closingTarget,omitempty"`
	// MaxTurns bounds tool dispatches for the whole call, guarding against a
	// model that loops.
	MaxTurns int `json:"maxTurns,omitempty"`
	// MaxTurnsWithoutTool bounds tool-less replies to the caller within one
	// phase: the wall against a model that keeps answering — "is there
	// anything else?", "no", "are you sure?" — without ever calling a tool
	// that would end the call. A reply counts when the model finishes a turn
	// that followed caller speech and made no tool call; a tool call and a
	// phase change both start the count over (see Engine.OnBotTurnDone). When
	// the count EXCEEDS this value the call moves to ClosingTarget, once the
	// caller has heard the reply that crossed it. Zero (the default) turns it
	// off, negative is refused at load, and a positive value requires
	// ClosingTarget.
	MaxTurnsWithoutTool int `json:"maxTurnsWithoutTool,omitempty"`
	// MaxDurationSec is the call's time budget, counted from the moment the
	// bot answers (02 §7). At 80% the model is told to wrap up; at 100% the
	// engine ends the call itself: a transfer to the DID's queue when that
	// queue is open, otherwise ClosingTarget (or a plain goodbye when there is
	// none). Absent, or null (the same thing to encoding/json), means
	// DefaultMaxDurationSec, not off; 0 turns the limit off;
	// anything else must lie between MinDurationLimitSec and MaxDurationLimitSec.
	// A pointer, because absent and 0 mean different things.
	MaxDurationSec *int `json:"maxDurationSec,omitempty"`
	// ToolWaitLine replaces the words of the wait line: what the caller hears
	// when an HTTP tool has not returned ToolWaitLineAfter after it started
	// and nothing else is playing (#286). It is said as written, like a
	// phase's announce, and may quote {slots.x}. It must be a statement: a
	// filler phrased as a question gets answered, and the answer talks over
	// the result (#53). A language left empty gets that language's default
	// (DefaultToolWaitLine), not the other language's words.
	ToolWaitLine Text `json:"toolWaitLine,omitempty"`
	// AlwaysAllowedTools are available in every phase — asking for a person,
	// or hanging up, should never be blocked by whatever phase the caller
	// happens to be in.
	AlwaysAllowedTools []string `json:"alwaysAllowedTools,omitempty"`
	// APIBaseEnv names the environment variable holding the backend base URL,
	// so a flow file carries no environment-specific address.
	APIBaseEnv string `json:"apiBaseEnv,omitempty"`
	// Transitions apply in every non-terminal phase, after the node's own.
	Transitions []Transition `json:"transitions,omitempty"`
}

// ToolWaitLineAfter is how long an HTTP tool may run before the caller hears
// the wait line (Global.ToolWaitLine). Fixed, not a setting (#286).
const ToolWaitLineAfter = time.Second

// DefaultToolWaitLine is the wait line of a flow that names none.
var DefaultToolWaitLine = Text{ZH: "请稍等，我查一下。", EN: "One moment, let me check that."}

// MaxTemperatureExclusive is the first value Global.Temperature may not take.
const MaxTemperatureExclusive = 2.0

// The bounds of Global.MaxDurationSec.
const (
	DefaultMaxDurationSec = 900
	// MinDurationLimitSec keeps the wrap-up window (the last 20%) at twelve
	// seconds or more, long enough for one reply.
	MinDurationLimitSec = 60
	MaxDurationLimitSec = 3600
)

// MaxDuration is the call's time budget; zero means the flow turned the
// limit off.
func (g Global) MaxDuration() time.Duration {
	if g.MaxDurationSec == nil {
		return DefaultMaxDurationSec * time.Second
	}
	return time.Duration(*g.MaxDurationSec) * time.Second
}

// Spec is one complete call flow.
type Spec struct {
	ID          string `json:"id"`
	SpecVersion string `json:"specVersion"`
	// Entry names the business entry point this flow serves.
	Entry string `json:"entry,omitempty"`
	// InitialNode is the phase a call starts in.
	InitialNode string          `json:"initialNode"`
	Global      Global          `json:"global"`
	Nodes       map[string]Node `json:"nodes"`
	Tools       map[string]Tool `json:"tools,omitempty"`
}

// Node looks a phase up.
func (s *Spec) Node(id string) (Node, bool) {
	node, ok := s.Nodes[id]
	return node, ok
}

// Lang normalises a call language to one the spec is written in.
func Lang(language string) string {
	if strings.HasPrefix(strings.ToLower(language), LangZH) {
		return LangZH
	}
	return LangEN
}

// notKeyedByLanguage is the refusal for a wording that is neither its plain
// form nor an object keyed by language. The decoder's own error would name an
// internal struct and an offset that is relative to this value alone, so only
// the found kind is reported; a malformed object keeps the decoder's detail.
func notKeyedByLanguage(plain string, data []byte, err error) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return fmt.Errorf("%s or an object keyed by language: %w", plain, err)
	}
	kind := "a number"
	switch {
	case len(trimmed) == 0:
		kind = "empty"
	case trimmed[0] == '"':
		kind = "a string"
	case trimmed[0] == '[':
		kind = "a list"
	case trimmed[0] == 't' || trimmed[0] == 'f':
		kind = "a boolean"
	case trimmed[0] == 'n':
		kind = "null"
	}
	return fmt.Errorf("%s or an object keyed by language, not %s", plain, kind)
}
