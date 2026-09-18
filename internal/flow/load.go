// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"fmt"
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
		return nil, fmt.Errorf("flow: cannot parse: %w", err)
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
	}

	for name, tool := range s.Tools {
		if tool.Description.IsEmpty() {
			report("tool %q has no description, so the model cannot tell when to use it", name)
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
	}

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
