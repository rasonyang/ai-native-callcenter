// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Engine tracks one call's progress through a flow.
//
// The division of labour is the whole point: the model owns the conversation —
// what to say, how to say it, when to ask again — and the engine owns the
// phase. It never writes dialogue. It decides which tools are available now,
// what the model should be trying to accomplish, and when that has changed.
//
// One call, one engine, driven from that call's own goroutine.
type Engine struct {
	spec *Spec
	lang string
	log  *slog.Logger

	current Node
	slots   map[string]any
	turns   int
}

// NewEngine starts a call at the flow's initial phase. baseSlots seeds
// call metadata such as the caller's number.
func NewEngine(spec *Spec, lang string, baseSlots map[string]any, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		spec:  spec,
		lang:  Lang(lang),
		log:   log.With("flowId", spec.ID),
		slots: map[string]any{},
	}
	maps.Copy(e.slots, baseSlots)
	e.slots["lang"] = e.lang
	e.enter(spec.InitialNode)
	return e
}

// Spec is the flow being run.
func (e *Engine) Spec() *Spec { return e.spec }

// Lang is the call's language.
func (e *Engine) Lang() string { return e.lang }

// NodeID is the current phase.
func (e *Engine) NodeID() string { return e.current.id }

// IsTerminal reports whether the conversation has reached a phase it does not
// leave.
func (e *Engine) IsTerminal() bool { return e.current.IsTerminal }

// Slots exposes what has been collected, for templates and diagnostics.
func (e *Engine) Slots() map[string]any {
	return maps.Clone(e.slots)
}

// Slot reads one collected value.
func (e *Engine) Slot(name string) (any, bool) {
	value, ok := e.slots[name]
	return value, ok
}

// IsToolAllowed reports whether a tool may be used in the current phase.
//
// The allowlist is what keeps a model from running ahead of itself — offering
// to book an appointment before it has identified the caller. Tools the flow
// marks as always available are exempt, because a caller asking for a person
// should never be told the current phase does not permit it.
func (e *Engine) IsToolAllowed(name string) bool {
	if slices.Contains(e.spec.Global.AlwaysAllowedTools, name) {
		return true
	}
	return slices.Contains(e.current.Tools, "*") || slices.Contains(e.current.Tools, name)
}

// AllowedTools lists what the current phase permits, for diagnostics.
func (e *Engine) AllowedTools() []string {
	return append(slices.Clone(e.spec.Global.AlwaysAllowedTools), e.current.Tools...)
}

// Instruction is what the model should be doing now, with collected values
// substituted in.
func (e *Engine) Instruction() string {
	return e.render(e.current.Instruction.For(e.lang))
}

// OnToolResult records a tool having run and moves the conversation on if a
// rule says to. It returns the new phase, or an empty string for staying put.
//
// Staying put is the common case and is deliberate: when nothing matches, the
// model is left in the same phase with the same instruction, which reads to the
// caller as being asked again rather than as an error.
func (e *Engine) OnToolResult(tool string, result map[string]any) string {
	e.turns++
	e.slots["turns"] = e.turns
	e.slots["lastTool"] = tool

	// The transient view is replaced wholesale so a condition on result.x
	// cannot match a value left over from an earlier tool.
	for key := range e.slots {
		if strings.HasPrefix(key, "result.") {
			delete(e.slots, key)
		}
	}
	for key, value := range result {
		e.slots["result."+key] = value
		e.slots[tool+"."+key] = value
	}
	e.slots[tool+".calls"] = e.countOf(tool+".calls") + 1

	if node := e.guardRunawayLoop(); node != "" {
		return node
	}
	return e.fire(EventTypeToolResult, tool)
}

// OnNoInput records the caller having said nothing and moves on if a rule
// says to.
func (e *Engine) OnNoInput() string {
	e.slots["noInput.count"] = e.countOf("noInput.count") + 1
	return e.fire(EventTypeNoInput, "")
}

// guardRunawayLoop sends a call that has made too many tool calls to the
// fallback phase. A model that loops would otherwise keep a caller on the line
// indefinitely, being asked the same question.
func (e *Engine) guardRunawayLoop() string {
	target := e.spec.Global.FallbackTarget
	switch {
	case e.spec.Global.MaxTurns <= 0, e.turns <= e.spec.Global.MaxTurns,
		target == "", target == e.current.id, e.current.IsTerminal:
		return ""
	}

	e.log.Warn("flow exceeded its turn limit, falling back",
		"turns", e.turns, "maxTurns", e.spec.Global.MaxTurns, "target", target)
	return e.enter(target)
}

// fire evaluates the current phase's rules, then the flow-wide ones.
//
// Node rules come first so a phase can override a cross-cutting rule; within
// each set, lower priority wins and equal priorities keep their declared order.
func (e *Engine) fire(event EventType, tool string) string {
	if e.current.IsTerminal {
		return ""
	}

	rules := append(sortedRules(e.current.Transitions), sortedRules(e.spec.Global.Transitions)...)
	for _, rule := range rules {
		if !rule.matches(event, tool) {
			continue
		}
		ok, err := evaluate(rule.Condition, e.slots)
		if err != nil {
			// Loading validates the operators, so this is a slot holding an
			// unexpected shape rather than a malformed flow. Skipping the rule
			// keeps the call going.
			e.log.Error("transition condition could not be evaluated",
				"node", e.current.id, "target", rule.Target, "error", err)
			continue
		}
		if !ok {
			continue
		}
		if rule.Target == e.current.id {
			return "" // an explicit instruction to stay
		}
		return e.enter(rule.Target)
	}
	return ""
}

func (r Transition) matches(event EventType, tool string) bool {
	if r.On != "" && r.On != event {
		return false
	}
	return r.Tool == "" || r.Tool == tool
}

// sortedRules orders by priority while keeping declaration order within a
// priority, so a flow author can read the file top to bottom.
func sortedRules(rules []Transition) []Transition {
	ordered := slices.Clone(rules)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Priority < ordered[j].Priority
	})
	return ordered
}

func (e *Engine) enter(nodeID string) string {
	node, ok := e.spec.Node(nodeID)
	if !ok {
		// Loading rejects unknown targets, so reaching this means the spec was
		// mutated after loading. Staying put keeps the call alive.
		e.log.Error("flow refers to a phase that does not exist", "node", nodeID)
		return ""
	}

	e.current = node
	e.slots["node"] = nodeID
	e.slots[nodeID+".visits"] = e.countOf(nodeID+".visits") + 1
	e.log.Info("flow entered phase", "node", nodeID, "visits", e.slots[nodeID+".visits"])
	return nodeID
}

func (e *Engine) countOf(slot string) int {
	if n, ok := asNumber(e.slots[slot]); ok {
		return int(n)
	}
	return 0
}

var slotReference = regexp.MustCompile(`\{slots\.([A-Za-z0-9_.\-]+)\}`)

// render substitutes {slots.name} references. A missing slot renders as
// nothing, so a half-filled instruction reads as an incomplete sentence rather
// than as a template someone forgot to fill.
func (e *Engine) render(template string) string {
	if !strings.Contains(template, "{slots.") {
		return template
	}
	return slotReference.ReplaceAllStringFunc(template, func(match string) string {
		name := slotReference.FindStringSubmatch(match)[1]
		return asString(e.slots[name])
	})
}
