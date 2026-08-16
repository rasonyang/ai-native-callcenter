// SPDX-License-Identifier: Apache-2.0

// Package flow is the declarative call flow: what phase a conversation is in,
// which tools are available there, and what moves it on. The model owns the
// conversation; the flow owns the phase.
package flow

import (
	"encoding/json"
	"fmt"
	"strings"
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
		return fmt.Errorf("text must be a string or an object keyed by language: %w", err)
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
		return fmt.Errorf("text list must be a list or an object keyed by language: %w", err)
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
	// Tool narrows a TOOL_RESULT rule to one tool. Empty matches any.
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
	// Tools is the allowlist for this phase. "*" allows everything; empty
	// allows only the flow's always-available tools.
	Tools       []string     `json:"tools,omitempty"`
	Transitions []Transition `json:"transitions,omitempty"`
	// IsTerminal marks a phase the conversation does not leave.
	IsTerminal bool `json:"isTerminal,omitempty"`

	// id is filled in from the map key at load time.
	id string
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
	Path string `json:"path"`
	// Method defaults to POST, which is what the reference backends expect.
	Method string `json:"method,omitempty"`
	// Body is a template: string values may reference {args.x} and {slots.y}.
	Body map[string]any `json:"body,omitempty"`
	// SuccessWhen decides success; absent means any 2xx counts.
	SuccessWhen *SuccessTest `json:"successWhen,omitempty"`
	// ErrorFrom is the dotted path to the backend's own error message.
	ErrorFrom string `json:"errorFrom,omitempty"`
	// Result maps slot names to dotted paths in the response.
	Result map[string]string `json:"result,omitempty"`
}

// Tool is a function the model may call.
type Tool struct {
	Description Text `json:"description"`
	// Parameters is a JSON Schema object, passed to the model unchanged.
	Parameters json.RawMessage `json:"parameters,omitempty"`
	HTTP       *HTTPCall       `json:"http,omitempty"`

	name string
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
	// FallbackTarget is where a conversation goes when it has run too long or
	// lost its way.
	FallbackTarget string `json:"fallbackTarget,omitempty"`
	// MaxTurns bounds tool dispatches for the whole call, guarding against a
	// model that loops.
	MaxTurns int `json:"maxTurns,omitempty"`
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
