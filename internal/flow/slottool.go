// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A slot tool is how a fact the caller merely said becomes durable. Slots that
// come from a backend response survive truncation of the model's context; a
// budget, a name or an answer to question two does not, unless something
// writes it down. The model calls the tool when the caller states the fact,
// the engine stores it, and every later render of an instruction, an announce
// or an HTTP body sees it.

// slotToolValueArg is the one argument of a slot tool.
const slotToolValueArg = "value"

// slotNamePattern is what a slot tool may be called: one segment, because a
// dotted name would collide with the <tool>.<field> namespace.
var slotNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// engineSlots are written by the engine itself, always present or present from
// the first turn on.
var engineSlots = []string{
	"caller", "entry", "did", "lang", "language", "node", "turns", "turnsWithoutTool",
	"lastTool", "noInput.count",
}

// reservedSlotNames cannot be claimed by a slot tool: the engine's own, and the
// roots of the namespaces it fills.
var reservedSlotNames = append(slices.Clone(engineSlots), "result", "userData", "noInput")

// ModelParameters is the JSON Schema the model is given for the tool. A slot
// tool's is generated, so a flow cannot misdescribe it.
func (t Tool) ModelParameters() json.RawMessage {
	if t.Kind != ToolKindSlot {
		return t.Parameters
	}
	value := map[string]any{"type": "string"}
	if len(t.Enum) > 0 {
		value["enum"] = t.Enum
	}
	schema, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{slotToolValueArg: value},
		"required":   []string{slotToolValueArg},
	})
	if err != nil {
		return nil
	}
	return schema
}

// cleanSlotValue normalises what the model passed to a slot tool: surrounding
// space trimmed and line breaks turned into spaces, so a value rendered into
// an instruction cannot add lines of its own to it. An empty result is not a
// fact. A value over MaxSlotValueLen characters, or outside the tool's enum,
// is refused with the reason, which the model reads and can correct.
func cleanSlotValue(tool Tool, raw any) (string, error) {
	text, ok := raw.(string)
	if !ok {
		if raw == nil {
			return "", fmt.Errorf("value is empty; nothing was recorded")
		}
		text = asString(raw)
	}
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text))
	if text == "" {
		return "", fmt.Errorf("value is empty; nothing was recorded")
	}
	if utf8.RuneCountInString(text) > MaxSlotValueLen {
		return "", fmt.Errorf("value is longer than %d characters; nothing was recorded, "+
			"record a shorter form of it", MaxSlotValueLen)
	}
	if len(tool.Enum) > 0 && !slices.Contains(tool.Enum, text) {
		return "", fmt.Errorf("value %q is not one of %s; nothing was recorded",
			text, strings.Join(tool.Enum, ", "))
	}
	return text, nil
}

// truncateRunes cuts s to at most n characters.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// scalarSlotValue converts a tool argument or business-data value to what may
// be stored as a slot: strings (cut to MaxSlotValueLen), numbers and booleans.
// Objects and arrays are not slots.
func scalarSlotValue(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		return truncateRunes(x, MaxSlotValueLen), true
	case bool, float64, float32, int, int32, int64, uint, uint32, uint64, json.Number:
		return x, true
	default:
		return nil, false
	}
}

// SeedSlots is the one place a call's starting slots are assembled, so
// everything known before the first word lands in the same namespace:
//
//	caller, entry, did   the call's own metadata
//	userData.<k>         business data the call was placed with (read-only;
//	                     the model cannot write it). A nested object is
//	                     flattened with dots, an array is skipped.
//
// A start-of-call lookup (issue #84) adds its mapped fields here too.
func SeedSlots(caller, entry, did string, userData map[string]any) map[string]any {
	slots := map[string]any{"caller": caller, "entry": entry, "did": did}
	seedUserData(slots, "userData", userData)
	return slots
}

func seedUserData(slots map[string]any, prefix string, data map[string]any) {
	for k, v := range data {
		key := prefix + "." + k
		if nested, ok := v.(map[string]any); ok {
			seedUserData(slots, key, nested)
			continue
		}
		if value, ok := scalarSlotValue(v); ok {
			slots[key] = value
		}
	}
}

// recordArgs stores the arguments of a tool call as <tool>.args.<k>, replacing
// the previous call's wholesale so an argument the model left out this time is
// not read from the time before.
func (e *Engine) recordArgs(tool string, args map[string]any) {
	prefix := tool + ".args."
	for key := range e.slots {
		if strings.HasPrefix(key, prefix) {
			delete(e.slots, key)
		}
	}
	for k, v := range args {
		if value, ok := scalarSlotValue(v); ok {
			e.slots[prefix+k] = value
		}
	}
}

// RecordSlot writes a slot tool's value.
func (e *Engine) RecordSlot(slot, value string) {
	e.slots[slot] = value
}

// declaredSlots answers whether a slot name used in a template or a condition
// can ever hold a value in this flow. A name nothing writes renders as empty or
// never matches, which is a typo that would otherwise be heard on a call.
type declaredSlots struct {
	spec *Spec
	// resultKeys is every field any tool's result can carry.
	resultKeys map[string]bool
}

func newDeclaredSlots(s *Spec) declaredSlots {
	d := declaredSlots{spec: s, resultKeys: map[string]bool{"ok": true, "queue": true}}
	for _, tool := range s.Tools {
		if tool.HTTP != nil {
			for key := range tool.HTTP.Result {
				d.resultKeys[key] = true
			}
		}
	}
	return d
}

func (d declaredSlots) has(name string) bool {
	if slices.Contains(engineSlots, name) {
		return true
	}
	if strings.HasPrefix(name, "userData.") && len(name) > len("userData.") {
		return true
	}
	if key, ok := strings.CutPrefix(name, "result."); ok {
		return d.resultKeys[key]
	}
	for _, tool := range d.spec.Tools {
		if tool.Kind == ToolKindSlot && tool.Slot == name {
			return true
		}
		if c := tool.Collect; c != nil && (c.Slot == name || c.Slot+collectCountSuffix == name) {
			return true
		}
	}
	if id, ok := strings.CutSuffix(name, ".visits"); ok {
		_, isNode := d.spec.Nodes[id]
		return isNode
	}
	// <tool>.calls, <tool>.args.<k>, <tool>.<field>
	for _, toolName := range d.toolNames() {
		rest, ok := strings.CutPrefix(name, toolName+".")
		if !ok {
			continue
		}
		return d.toolHas(toolName, rest)
	}
	return false
}

// unwrittenReason completes "references {slots.x}, ..." for a name has()
// refused. A name that a tool hands to the model through http.modelOnly gets
// its own explanation, because the author's next move is different: map it in
// result, rather than hunt for a typo.
func (d declaredSlots) unwrittenReason(name string) string {
	const nothing = "which nothing in this flow writes"
	isModelOnly := func(tool Tool, field string) bool {
		if tool.HTTP == nil {
			return false
		}
		_, ok := tool.HTTP.ModelOnly[field]
		return ok
	}
	if field, ok := strings.CutPrefix(name, "result."); ok {
		for _, toolName := range slices.Sorted(maps.Keys(d.spec.Tools)) {
			if isModelOnly(d.spec.Tools[toolName], field) {
				return fmt.Sprintf("but %q is handed to the model only (http.modelOnly of tool %q) and is never a slot; map it in result to read it", field, toolName)
			}
		}
		return nothing
	}
	for _, toolName := range slices.Sorted(maps.Keys(d.spec.Tools)) {
		if field, ok := strings.CutPrefix(name, toolName+"."); ok && isModelOnly(d.spec.Tools[toolName], field) {
			return fmt.Sprintf("but %q is handed to the model only (http.modelOnly of tool %q) and is never a slot; map it in result to read it", field, toolName)
		}
	}
	return nothing
}

func (d declaredSlots) toolNames() []string {
	names := slices.Collect(maps.Keys(d.spec.Tools))
	return append(names, BuiltinToolNames...)
}

func (d declaredSlots) toolHas(toolName, rest string) bool {
	if rest == "calls" || rest == "ok" {
		return true
	}
	tool, isFlowTool := d.spec.Tools[toolName]
	if arg, ok := strings.CutPrefix(rest, "args."); ok {
		if !isFlowTool {
			return arg != ""
		}
		return slices.Contains(toolArgNames(tool), arg)
	}
	if !isFlowTool {
		return d.resultKeys[rest]
	}
	if tool.HTTP != nil {
		_, ok := tool.HTTP.Result[rest]
		return ok
	}
	return false
}

// toolArgNames lists the arguments a tool declares: the properties of its
// parameter schema, or "value" for a slot tool. A tool that declares no
// properties declares no arguments to read back.
func toolArgNames(tool Tool) []string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.ModelParameters(), &schema); err != nil {
		return nil
	}
	return slices.Collect(maps.Keys(schema.Properties))
}

// collectCountSuffix names the counter that goes with a collect list.
const collectCountSuffix = ".count"

// collect applies a tool's collect rule after its result is recorded. It
// runs only for a successful call, so a refused or failed one adds nothing.
func (e *Engine) collect(toolName string, args, result map[string]any) {
	tool, ok := e.spec.Tools[toolName]
	rule := tool.Collect
	if !ok || rule == nil || asString(result["ok"]) != "1" {
		return
	}
	if rule.When != nil {
		holds, err := evaluate(rule.When, e.slots)
		if err != nil {
			e.log.Error("collect condition could not be evaluated", "tool", toolName, "slot", rule.Slot, "error", err)
			return
		}
		if !holds {
			return
		}
	}
	value := strings.TrimSpace(asString(renderString(rule.Value, args, e.slots)))
	if value == "" {
		return
	}
	if utf8.RuneCountInString(value) > MaxSlotValueLen {
		e.log.Warn("collected value cut to the slot value limit", "tool", toolName, "slot", rule.Slot, "limit", MaxSlotValueLen)
		value = strings.TrimSpace(truncateRunes(value, MaxSlotValueLen))
	}
	values := e.collected[rule.Slot]
	if slices.Contains(values, value) {
		return
	}
	if len(values) >= MaxCollectedValues {
		e.log.Warn("collect list is full; value not added", "tool", toolName, "slot", rule.Slot, "limit", MaxCollectedValues)
		return
	}
	values = append(values, value)
	e.collected[rule.Slot] = values
	e.slots[rule.Slot] = strings.Join(values, ", ")
	e.slots[rule.Slot+collectCountSuffix] = len(values)
}

// collectSlotPattern is what a collect list may be called: dotted segments,
// like the engine's own <tool>.<field> names.
var collectSlotPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)*$`)
