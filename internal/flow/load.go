// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// A flow is configuration that decides how a phone call goes. Everything that
// can be checked is checked here, at load, rather than discovered by a caller:
// a transition to a phase that does not exist should be a startup failure, not
// a conversation that dead-ends.

// Load parses and validates one flow.
func Load(data []byte) (*Spec, error) {
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, &ValidationError{FlowID: idOf(data), Problems: []string{decodeProblem(err)}}
	}
	for id, node := range spec.Nodes {
		node.id = id
		spec.Nodes[id] = node
	}
	for name, tool := range spec.Tools {
		tool.name = name
		spec.Tools[name] = tool
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// decodeProblem words a JSON decoding failure as one problem that names where
// the document went wrong when the decoder knows, and never quotes the body.
func decodeProblem(err error) string {
	var typed *json.UnmarshalTypeError
	var syntax *json.SyntaxError
	switch {
	case errors.As(err, &typed) && typed.Field != "":
		return fmt.Sprintf("cannot parse %s: it is %s, want %s", typed.Field, typed.Value, typed.Type)
	case errors.As(err, &typed):
		return fmt.Sprintf("cannot parse the flow: found %s, want %s", typed.Value, typed.Type)
	case errors.As(err, &syntax):
		return fmt.Sprintf("cannot parse the flow: invalid JSON at byte %d", syntax.Offset)
	}
	// A custom decoder (a wording, a modelOnly entry) names its own field.
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return "cannot parse: " + msg
}

// idOf is the flow's own id when the document has a readable one, for the
// error's heading; empty otherwise.
func idOf(data []byte) string {
	var head struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &head) != nil {
		return ""
	}
	return head.ID
}

// validate rejects anything that would fail mid-call.
func (s *Spec) validate() error {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if s.ID == "" {
		report("the flow has no id")
	}
	if s.SpecVersion != SpecVersion {
		report("specVersion is %q, want %q", s.SpecVersion, SpecVersion)
	}
	if len(s.Nodes) == 0 {
		report("the flow has no phases")
	}
	if s.InitialNode == "" {
		report("initialNode is not set")
	} else if _, ok := s.Nodes[s.InitialNode]; !ok {
		report("initialNode %q is not a phase in this flow", s.InitialNode)
	}
	if s.Global.Persona.IsEmpty() {
		report("global.persona is empty, so the model has no character to adopt")
	}
	if s.Global.FallbackTarget != "" {
		if _, ok := s.Nodes[s.Global.FallbackTarget]; !ok {
			report("global.fallbackTarget %q is not a phase in this flow", s.Global.FallbackTarget)
		}
	}
	if s.Global.ClosingTarget != "" {
		if node, ok := s.Nodes[s.Global.ClosingTarget]; !ok {
			report("global.closingTarget %q is not a phase in this flow", s.Global.ClosingTarget)
		} else if !node.IsTerminal {
			report("global.closingTarget %q must be a terminal phase, the conversation cannot leave it again", s.Global.ClosingTarget)
		}
	}
	if s.Global.MaxTurnsWithoutTool < 0 {
		report("global.maxTurnsWithoutTool is %d; it is a count of turns, 0 to turn the wall off", s.Global.MaxTurnsWithoutTool)
	} else if s.Global.MaxTurnsWithoutTool > 0 && s.Global.ClosingTarget == "" {
		report("global.maxTurnsWithoutTool is set but global.closingTarget is empty, so the wall has nowhere to send the call")
	}
	if d := s.Global.MaxDurationSec; d != nil && *d != 0 && (*d < MinDurationLimitSec || *d > MaxDurationLimitSec) {
		report("global.maxDurationSec is %d; it is a number of seconds from %d to %d, 0 to turn the limit off", *d, MinDurationLimitSec, MaxDurationLimitSec)
	}

	for _, line := range []string{s.Global.ToolWaitLine.ZH, s.Global.ToolWaitLine.EN} {
		if strings.ContainsAny(line, "?？") {
			report("global.toolWaitLine %q is a question; it must be a statement, because a caller answers a question and the answer talks over the result", line)
			break
		}
	}

	if t := s.Global.Temperature; t != nil && (math.IsNaN(*t) || *t < 0 || *t >= MaxTemperatureExclusive) {
		report("global.temperature is %v; it is a sampling temperature from 0 up to but not including %v", *t, MaxTemperatureExclusive)
	}

	// Every tool a phase names must exist, or the model will be offered
	// something that cannot run.
	known := s.knownToolNames()
	for _, name := range s.Global.AlwaysAllowedTools {
		if !slices.Contains(known, name) {
			report("global.alwaysAllowedTools names unknown tool %q", name)
		}
	}
	s.validateTransitions("global", s.Global.Transitions, &problems)

	for id, node := range s.Nodes {
		if node.Instruction.IsEmpty() {
			report("phase %q has no instruction", id)
		}
		for _, name := range node.Tools {
			if name != "*" && !slices.Contains(known, name) {
				report("phase %q allows unknown tool %q", id, name)
			}
		}
		if node.IsTerminal && len(node.Transitions) > 0 {
			report("phase %q is terminal but declares transitions", id)
		}
		s.validateTransitions("phase "+id, node.Transitions, &problems)
		if node.Lead != nil {
			validateDeclaredLead(id, node.Lead, report)
		}
	}

	slotOwners := map[string]string{}
	for name, tool := range s.Tools {
		if tool.Description.IsEmpty() {
			report("tool %q has no description, so the model cannot tell when to use it", name)
		}
		if slices.Contains(BuiltinToolNames, name) {
			report("tool %q is the name of a built-in tool", name)
		}
		switch tool.Kind {
		case "", ToolKindHTTP:
			if tool.Slot != "" || len(tool.Enum) > 0 {
				report("tool %q is an http tool but declares slot or enum, which only a slot tool has", name)
			}
			if tool.HTTP == nil {
				report("tool %q declares no http call", name)
				continue
			}
			if tool.HTTP.Path == "" {
				report("tool %q declares no path", name)
			}
			if len(tool.Parameters) > 0 && !json.Valid(tool.Parameters) {
				report("tool %q has parameters that are not valid JSON", name)
			}
		case ToolKindSlot:
			if tool.Collect != nil {
				report("tool %q is a slot tool and must not declare collect, which only an http tool has", name)
			}
			s.validateSlotTool(name, tool, slotOwners, report)
		default:
			report("tool %q has unknown kind %q; it is %q or %q", name, tool.Kind, ToolKindHTTP, ToolKindSlot)
		}
	}

	// A name nothing writes renders as empty or never matches: a typo the
	// caller would hear instead of the author seeing it.
	declared := newDeclaredSlots(s)
	for id, node := range s.Nodes {
		checkTemplate(declared, "phase "+id+" instruction", node.Instruction, report)
		checkTemplate(declared, "phase "+id+" announce", node.Announce, report)
		if node.Lead != nil {
			checkTemplate(declared, "phase "+id+" lead notes", node.Lead.Notes, report)
		}
		checkConditions(declared, "phase "+id, node.Transitions, report)
	}
	checkTemplate(declared, "global.toolWaitLine", s.Global.ToolWaitLine, report)
	checkConditions(declared, "global", s.Global.Transitions, report)
	for name, tool := range s.Tools {
		if tool.HTTP != nil {
			checkBody(declared, "tool "+name, tool.HTTP.Body, report)
			if tool.Kind != ToolKindSlot {
				validateHTTPCall(declared, name, tool, report)
			}
		}
	}
	s.validateCollects(declared, report)

	if len(problems) > 0 {
		return &ValidationError{FlowID: s.ID, Problems: problems}
	}
	return nil
}

// ValidationError is everything wrong with a flow rather than the first thing.
//
// The list is the point: an author fixing a spec should see the whole report
// at once, not discover it one save at a time. The joined message is what a
// startup log wants; the slice is what an editor puts next to the lines.
type ValidationError struct {
	// FlowID is the spec's own id, which may itself be one of the problems.
	FlowID   string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("flow %s is not usable:\n  - %s",
		e.FlowID, strings.Join(e.Problems, "\n  - "))
}

// Rule is a check a spec must pass beyond being loadable.
//
// It is separate from validate for one reason: validate answers "is this a
// flow", which is the same question everywhere, and a Rule answers "may this
// flow run here", which is not. A deployment applies the rules its own
// installation implies — today exactly one, and only on publish, because that
// is the operation a caller can hear.
type Rule func(*Spec) error

// RequireTerminalAnnounce refuses a flow whose terminal phases have no line of
// their own.
//
// A phase the conversation does not leave exists to say one thing: the
// goodbye, the hand-over script. Every phase before it can leave the wording
// to the model because the model is answering the caller — but a terminal
// phase has nobody to answer. It is reached, it speaks, and the call ends, so
// something has to prompt that turn. Where the engine takes a text cue, the
// phase instruction is that prompt; where it does not, the phase's own line is
// the only way those words exist at all, and without one the caller hears the
// bot simply stop.
//
// It is a publish-time rule and not a load-time one on purpose. The same flow
// is perfectly good on an engine that can be cued, and a spec that loads on one
// installation and not on another would make the dialect a property of the
// host.
func RequireTerminalAnnounce(spec *Spec) error {
	var nodes []string
	for id, node := range spec.Nodes {
		if node.IsTerminal && node.Announce.IsEmpty() {
			nodes = append(nodes, id)
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	slices.Sort(nodes)
	return &MissingAnnounceError{FlowID: spec.ID, Nodes: nodes}
}

// MissingAnnounceError names every terminal phase that would have nothing to
// say. Like ValidationError it reports the whole list rather than the first,
// so an author fixes the flow once.
type MissingAnnounceError struct {
	FlowID string
	Nodes  []string
}

func (e *MissingAnnounceError) Error() string {
	return fmt.Sprintf("flow %s cannot run on this deployment: its speech provider "+
		"cannot be prompted to speak by text, so a phase the call does not leave "+
		"needs an announce of its own; these have none: %s",
		e.FlowID, strings.Join(e.Nodes, ", "))
}

// validateDeclaredLead checks a phase's lead declaration (Node.Lead): its
// shape, and an intent the record_lead tool would accept, written exactly as
// the enum spells it. The tool forgives a model's lower case; a flow is
// written once, by a person, and checked here.
func validateDeclaredLead(id string, lead *DeclaredLead, report func(string, ...any)) {
	for _, problem := range lead.problems {
		report("phase %q lead %s", id, problem)
	}
	if lead.isIntentUnreadable {
		return
	}
	intents := strings.Join(LeadIntents, ", ")
	switch {
	case lead.Intent == "":
		report("phase %q lead declares no intent; it is one of %s", id, intents)
	case slices.Contains(LeadIntents, lead.Intent):
	case slices.Contains(LeadIntents, strings.ToUpper(strings.TrimSpace(lead.Intent))):
		report("phase %q lead intent is %q; write it as %q, in capitals", id, lead.Intent,
			strings.ToUpper(strings.TrimSpace(lead.Intent)))
	default:
		report("phase %q lead intent is %q; it is one of %s", id, lead.Intent, intents)
	}
}

// knownToolNames is everything a phase may legitimately allow: the flow's own
// declarative tools plus the built-ins every flow gets.
func (s *Spec) knownToolNames() []string {
	names := make([]string, 0, len(s.Tools)+len(BuiltinToolNames))
	for name := range s.Tools {
		names = append(names, name)
	}
	return append(names, BuiltinToolNames...)
}

func (s *Spec) validateTransitions(where string, rules []Transition, problems *[]string) {
	for i, rule := range rules {
		if rule.Target == "" {
			*problems = append(*problems,
				fmt.Sprintf("%s transition %d names no target", where, i))
		} else if _, ok := s.Nodes[rule.Target]; !ok {
			*problems = append(*problems,
				fmt.Sprintf("%s transition %d targets %q, which is not a phase in this flow",
					where, i, rule.Target))
		}
		if rule.On != "" && rule.On != EventTypeToolResult && rule.On != EventTypeNoInput {
			*problems = append(*problems,
				fmt.Sprintf("%s transition %d fires on unknown event %q", where, i, rule.On))
		}
		// A silence carries no tool, so such a rule could never fire: it is a
		// rule that says something about silence and does nothing about it.
		if rule.On == EventTypeNoInput && rule.Tool != "" {
			*problems = append(*problems,
				fmt.Sprintf("%s transition %d fires on NO_INPUT but names tool %q, which a silence never carries",
					where, i, rule.Tool))
		}
		if err := validateCondition(rule.Condition); err != nil {
			*problems = append(*problems,
				fmt.Sprintf("%s transition %d: %v", where, i, err))
		}
	}
}

// validateCondition checks the operator set at load time, so a mistyped
// operator is a startup error rather than a rule that silently never fires.
func validateCondition(condition *Condition) error {
	if condition == nil {
		return nil
	}
	if len(condition.All) > 0 || len(condition.Any) > 0 {
		for _, sub := range append(slices.Clone(condition.All), condition.Any...) {
			if err := validateCondition(&sub); err != nil {
				return err
			}
		}
		return nil
	}
	if !knownOperators[condition.Op] {
		return fmt.Errorf("unknown operator %q", condition.Op)
	}
	if condition.Slot == "" {
		return fmt.Errorf("condition with operator %s names no slot", condition.Op)
	}
	return nil
}

// validateSlotTool checks a tool of kind "slot".
func (s *Spec) validateSlotTool(name string, tool Tool, owners map[string]string, report func(string, ...any)) {
	if tool.HTTP != nil {
		report("tool %q is a slot tool and must not declare http: it records a value and calls nothing", name)
	}
	if len(tool.Parameters) > 0 {
		report("tool %q is a slot tool and must not declare parameters: its one argument is %q", name, slotToolValueArg)
	}
	switch {
	case tool.Slot == "":
		report("slot tool %q names no slot", name)
	case !slotNamePattern.MatchString(tool.Slot):
		report("slot tool %q writes slot %q, which is not a single name of letters, digits and underscores", name, tool.Slot)
	case slices.Contains(reservedSlotNames, tool.Slot):
		report("slot tool %q writes slot %q, which the engine owns", name, tool.Slot)
	default:
		if other, taken := owners[tool.Slot]; taken {
			report("slot tools %q and %q both write slot %q", other, name, tool.Slot)
		}
		owners[tool.Slot] = name
	}
	seen := map[string]bool{}
	for _, v := range tool.Enum {
		if strings.TrimSpace(v) == "" || strings.TrimSpace(v) != v || len([]rune(v)) > MaxSlotValueLen {
			report("slot tool %q has enum value %q; it must be non-empty, trimmed and at most %d characters", name, v, MaxSlotValueLen)
		}
		if seen[v] {
			report("slot tool %q lists enum value %q twice", name, v)
		}
		seen[v] = true
	}
}

func checkTemplate(d declaredSlots, where string, text Text, report func(string, ...any)) {
	for _, lang := range []string{text.EN, text.ZH} {
		for _, name := range slotRefs(lang) {
			if !d.has(name) {
				report("%s references {slots.%s}, %s", where, name, d.unwrittenReason(name))
			}
		}
	}
}

func checkBody(d declaredSlots, where string, body map[string]any, report func(string, ...any)) {
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			for _, m := range templateReference.FindAllStringSubmatch(x, -1) {
				if m[1] == "slots" && !d.has(m[2]) {
					report("%s body references {slots.%s}, %s", where, m[2], d.unwrittenReason(m[2]))
				}
			}
		case map[string]any:
			for _, item := range x {
				walk(item)
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(body)
}

func checkConditions(d declaredSlots, where string, rules []Transition, report func(string, ...any)) {
	for i, rule := range rules {
		for _, slot := range conditionSlots(rule.Condition) {
			if !d.has(slot) {
				report("%s transition %d tests slot %q, %s", where, i, slot, d.unwrittenReason(slot))
			}
		}
	}
}

// conditionSlots lists the slots a condition tests, nested ones included.
func conditionSlots(c *Condition) []string {
	if c == nil {
		return nil
	}
	if len(c.All) > 0 || len(c.Any) > 0 {
		var slots []string
		for _, sub := range append(slices.Clone(c.All), c.Any...) {
			slots = append(slots, conditionSlots(&sub)...)
		}
		return slots
	}
	if c.Slot == "" {
		return nil
	}
	return []string{c.Slot}
}

// slotRefs lists the names of the {slots.x} references in a template.
func slotRefs(template string) []string {
	var names []string
	for _, m := range slotReference.FindAllStringSubmatch(template, -1) {
		names = append(names, m[1])
	}
	return names
}

// validateCollects checks every tool's collect rule. Only an http tool may
// have one (a slot tool is reported where it is validated), its slot must not
// be a name something else writes, and what it reads must exist.
func (s *Spec) validateCollects(d declaredSlots, report func(string, ...any)) {
	names := make([]string, 0, len(s.Tools))
	for name, tool := range s.Tools {
		if tool.Collect != nil && tool.Kind != ToolKindSlot {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		rule := s.Tools[name].Collect
		switch {
		case rule.Slot == "":
			report("tool %q collect names no slot", name)
		case !collectSlotPattern.MatchString(rule.Slot):
			report("tool %q collects into slot %q, which is not a name of letters, digits and underscores, with dots between segments", name, rule.Slot)
		default:
			for _, written := range []string{rule.Slot, rule.Slot + collectCountSuffix} {
				if reason := s.collectCollision(d, name, written); reason != "" {
					report("tool %q collects into slot %q, but %q is %s", name, rule.Slot, written, reason)
				}
			}
		}
		if strings.TrimSpace(rule.Value) == "" {
			report("tool %q collect has no value", name)
		}
		args := toolArgNames(s.Tools[name])
		for _, m := range templateReference.FindAllStringSubmatch(rule.Value, -1) {
			switch {
			case m[1] == "args" && !slices.Contains(args, m[2]):
				report("tool %q collect value references {args.%s}, which is not a declared parameter of the tool", name, m[2])
			case m[1] == "slots" && !d.has(m[2]):
				report("tool %q collect value references {slots.%s}, %s", name, m[2], d.unwrittenReason(m[2]))
			}
		}
		if err := validateCondition(rule.When); err != nil {
			report("tool %q collect when: %v", name, err)
		}
		for _, slot := range conditionSlots(rule.When) {
			if !d.has(slot) {
				report("tool %q collect when tests slot %q, %s", name, slot, d.unwrittenReason(slot))
			}
		}
	}
}

// collectCollision says what already owns a slot name a collect list would
// write, or returns an empty string when the name is free.
func (s *Spec) collectCollision(d declaredSlots, owner, written string) string {
	if slices.Contains(engineSlots, written) {
		return "written by the engine"
	}
	for _, root := range []string{"result", "userData", "noInput"} {
		if written == root || strings.HasPrefix(written, root+".") {
			return "in the namespace of " + root + ", which the engine owns"
		}
	}
	if id, ok := strings.CutSuffix(written, ".visits"); ok {
		if _, isNode := s.Nodes[id]; isNode {
			return "the visit counter of phase " + id
		}
	}
	for _, toolName := range d.toolNames() {
		rest, ok := strings.CutPrefix(written, toolName+".")
		if !ok {
			continue
		}
		if rest == "calls" || rest == "ok" || strings.HasPrefix(rest, "args.") {
			return "written by the engine for tool " + toolName
		}
		if tool, isFlowTool := s.Tools[toolName]; isFlowTool {
			if tool.HTTP != nil {
				if _, mapped := tool.HTTP.Result[rest]; mapped {
					return "a result field of tool " + toolName
				}
			}
		} else if d.resultKeys[rest] {
			return "a result field of built-in tool " + toolName
		}
	}
	for name, tool := range s.Tools {
		if tool.Kind == ToolKindSlot && tool.Slot == written {
			return "the slot of slot tool " + name
		}
		if c := tool.Collect; c != nil && name != owner {
			if c.Slot == written || c.Slot+collectCountSuffix == written {
				return "also written by the collect of tool " + name
			}
		}
	}
	return ""
}
