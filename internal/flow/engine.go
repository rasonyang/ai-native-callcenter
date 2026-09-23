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
	// turnsWithoutTool counts tool-less replies to the caller in the current
	// phase (see OnBotTurnDone), mirrored into the slot of the same name so a
	// condition can test it the way it tests turns.
	turnsWithoutTool int
	// isCallerAwaitingReply records a caller utterance with words in it since
	// the last bot turn the wall accounted for: the next completed turn is a
	// reply to the caller.
	isCallerAwaitingReply bool
	// isToolReplyNext records that the last completed turn made a tool call,
	// so the next one answers the tool's result rather than the caller.
	isToolReplyNext bool
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

// Announce is the line this phase speaks on being entered, with collected
// values substituted in. Empty means the phase has nothing of its own to say
// and the model does the talking.
func (e *Engine) Announce() string {
	return e.render(e.current.Announce.For(e.lang))
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
	// A tool just ran, so the conversation is making progress by the engine's
	// own measure: the wall counts replies since this moment, a run of silence
	// is over, and whatever the caller said before the tool call has had its
	// answer (see OnBotTurnDone).
	e.resetTurnsWithoutTool()
	e.isCallerAwaitingReply = false
	e.slots["noInput.count"] = 0

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

// defaultNoInputLimit is how many consecutive silences the engine tolerates
// before supplying its own NO_INPUT rule (see defaultNoInput), when the flow
// declared none of its own for the phase the caller went quiet in. Chosen to
// match the one flow that already had a rule of its own (novanet_support's
// welcome phase, "noInput.count GTE 3"), so a flow adopting ClosingTarget
// gets the behaviour that flow's author already judged reasonable.
const defaultNoInputLimit = 3

// OnNoInput records the caller having said nothing and moves on if a rule
// says to.
//
// noInput.count is consecutive silences, not a lifetime total: any sign of
// the caller making progress resets it — words (OnCallerSpoke), a keypress
// (OnCallerKeyed) or a tool the conversation led to (OnToolResult) — so a bot
// that goes quiet once early in a long call is not one dead-air event away
// from being cut off near the end of it.
func (e *Engine) OnNoInput() string {
	e.slots["noInput.count"] = e.countOf("noInput.count") + 1
	if node := e.fire(EventTypeNoInput, ""); node != "" {
		return node
	}
	return e.defaultNoInput()
}

// defaultNoInput supplies "count >= defaultNoInputLimit -> closingTarget" when
// no rule the author wrote could ever fire on NO_INPUT from the phase the
// caller went silent in — neither the phase's own rules nor the flow-wide ones
// — and the flow named a closing target to send the call to.
//
// This is deliberately narrow: an author who wrote a rule that can fire on
// silence here, however it is conditioned, has said something about silence,
// and the engine respects that rule set exactly as written rather than
// layering a second one behind it. Without a closing target there is nowhere
// to send the call, so re-prompting forever is what the flow gets until it
// names one — which is the behaviour before this existed, not a regression.
func (e *Engine) defaultNoInput() string {
	if e.current.IsTerminal || e.spec.Global.ClosingTarget == "" || e.hasDeclaredNoInputRule() {
		return ""
	}
	if e.countOf("noInput.count") < defaultNoInputLimit {
		return ""
	}
	e.log.Warn("phase declares no NO_INPUT rule; closing the call after repeated silence",
		"node", e.current.id, "count", e.countOf("noInput.count"), "target", e.spec.Global.ClosingTarget)
	return e.enter(e.spec.Global.ClosingTarget)
}

// hasDeclaredNoInputRule reports whether the author wrote any transition that
// fire() would consider for a NO_INPUT event in the current phase — its own
// rules or the flow-wide ones, matched exactly the way fire() matches them, so
// a rule with no `on` (every event) counts and one naming a tool (which a
// silence never carries) does not. It answers "can an authored rule fire on
// silence here", not "does one currently match": a GTE condition that has not
// yet been met still counts, so the default never races ahead of a rule that
// simply has not tripped yet.
func (e *Engine) hasDeclaredNoInputRule() bool {
	for _, rules := range [][]Transition{e.current.Transitions, e.spec.Global.Transitions} {
		for _, rule := range rules {
			if rule.matches(EventTypeNoInput, "") {
				return true
			}
		}
	}
	return false
}

// OnCallerSpoke records one finished caller utterance, by its transcript.
//
// An empty transcript — line noise, echo, a breath the detector took for
// speech — is nothing: it neither ends a run of silence nor gives the bot's
// next turn a caller to answer. Words do both. noInput.count resets (see
// OnNoInput), and the next completed bot turn is marked as a reply to the
// caller, which is what the turns-without-a-tool wall counts (OnBotTurnDone).
func (e *Engine) OnCallerSpoke(transcript string) {
	if strings.TrimSpace(transcript) == "" {
		return
	}
	e.slots["noInput.count"] = 0
	e.isCallerAwaitingReply = true
}

// OnCallerKeyed records a keypress. It ends a run of silence exactly as words
// do. It is not counted as a turn against the wall, which counts replies to
// something the caller said.
func (e *Engine) OnCallerKeyed() {
	e.slots["noInput.count"] = 0
}

// OnBotTurnDone accounts for one bot turn the model has finished producing,
// and reports whether the call now stands past the flow's
// maxTurnsWithoutTool wall (see IsPastTurnsWithoutToolWall).
//
// isToolCall says the turn made a tool call; isInterrupted that it was cut
// short. What counts is one tool-less reply to the caller: a turn that ran to
// completion, made no tool call, and followed a caller utterance with words in
// it since the last turn that was counted. Everything else counts nothing:
//
//   - the opening turn, a dead-air re-prompt and a phase's own line have no
//     caller utterance behind them;
//   - a turn that made a tool call resets the count, because the conversation
//     is making progress by the engine's own measure;
//   - the turn after a tool-call turn answers the tool's result, not the
//     caller. Skipping it is also what keeps a transcript that arrives after
//     the turn it prompted (the Realtime clients deliver the caller's
//     transcript independently of the reply) from being charged to the
//     result's turn;
//   - a turn cut short never finished replying to anybody.
//
// A transcript that arrives late is therefore charged to the next reply, or
// dropped when a tool intervenes, never counted twice: the wall can only be
// late by a turn, never early.
func (e *Engine) OnBotTurnDone(isToolCall, isInterrupted bool) bool {
	switch {
	case isToolCall:
		e.resetTurnsWithoutTool()
		e.isCallerAwaitingReply = false
		e.isToolReplyNext = true
	case isInterrupted:
		// The reply to the tool, if that is what this was, is abandoned; what
		// the caller says next gets a reply that counts.
		e.isToolReplyNext = false
	case e.isToolReplyNext:
		e.isToolReplyNext = false
		e.isCallerAwaitingReply = false
	case e.isCallerAwaitingReply:
		e.isCallerAwaitingReply = false
		if !e.current.IsTerminal && e.spec.Global.MaxTurnsWithoutTool > 0 {
			e.turnsWithoutTool++
			e.slots["turnsWithoutTool"] = e.turnsWithoutTool
		}
	}
	return e.IsPastTurnsWithoutToolWall()
}

// IsPastTurnsWithoutToolWall reports whether the tool-less replies in this
// phase now EXCEED the flow's maxTurnsWithoutTool — the same "more than the
// limit" the sibling maxTurns guard uses — with a closing target to go to and
// a phase the call can still leave.
func (e *Engine) IsPastTurnsWithoutToolWall() bool {
	limit := e.spec.Global.MaxTurnsWithoutTool
	return limit > 0 && e.turnsWithoutTool > limit &&
		e.spec.Global.ClosingTarget != "" && !e.current.IsTerminal
}

// CloseAtTurnsWithoutToolWall moves the call to the flow's closing target if
// it still stands past the wall, returning the new phase, or an empty string
// when nothing is to be done — a tool or a phase change since has reset the
// count, or the call has already reached a terminal phase.
//
// It is separate from OnBotTurnDone because the move must not be made when the
// wall is crossed: that turn's reply is still playing, and the goodbye asked
// for on top of it would either be refused or cut the reply off. The caller
// of the engine makes the move once the caller has heard that reply.
func (e *Engine) CloseAtTurnsWithoutToolWall() string {
	if !e.IsPastTurnsWithoutToolWall() {
		return ""
	}
	e.log.Warn("flow exceeded its turns-without-a-tool wall; closing the call",
		"node", e.current.id, "turns", e.turnsWithoutTool,
		"maxTurnsWithoutTool", e.spec.Global.MaxTurnsWithoutTool,
		"target", e.spec.Global.ClosingTarget)
	return e.enter(e.spec.Global.ClosingTarget)
}

// resetTurnsWithoutTool starts the wall's count over.
func (e *Engine) resetTurnsWithoutTool() {
	e.turnsWithoutTool = 0
	e.slots["turnsWithoutTool"] = 0
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
	// The wall bounds tool-less replies within one phase: a new phase is new
	// progress, whether a tool or a silence moved the call there.
	e.resetTurnsWithoutTool()
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
