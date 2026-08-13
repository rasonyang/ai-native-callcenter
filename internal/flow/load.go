// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
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

// LoadDir reads every .json flow in a directory, keyed by flow id.
func LoadDir(fsys fs.FS, dir string) (map[string]*Spec, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("flow: cannot read %s: %w", dir, err)
	}

	flows := map[string]*Spec{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := path.Join(dir, entry.Name())
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("flow: cannot read %s: %w", name, err)
		}
		spec, err := Load(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if existing, isDuplicate := flows[spec.ID]; isDuplicate {
			return nil, fmt.Errorf("flow: id %q is defined twice (%s and %s)",
				spec.ID, existing.Entry, entry.Name())
		}
		flows[spec.ID] = spec
	}
	return flows, nil
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
		return fmt.Errorf("flow %s is not usable:\n  - %s",
			s.ID, strings.Join(problems, "\n  - "))
	}
	return nil
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
