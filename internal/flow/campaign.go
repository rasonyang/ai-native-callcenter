// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"slices"
	"strings"
)

// CanTransfer reports whether a conversation following spec can hand the
// caller to a person.
//
// The only way a flow reaches a person is the built-in tool
// transfer_to_agent, so the question is whether any phase offers it: a phase
// that allows it by name, a phase that allows everything ("*"), or a flow
// that lists it among the tools every phase has. Reachability is not
// analysed: a phase no transition leads to still counts, because a flow with
// a way to a person written into it is not one an unattended campaign should
// run, whether or not today's transitions reach it.
//
// Two transfers are deliberately not this function's business, because they
// do not depend on what the flow offers the model but on the call: the
// session-limit hand-over to the DID's queue (Global.MaxDurationSec) and the
// rescue of a caller to the DID's fallback queue when the provider fails. A
// campaign call must switch both off at runtime (design 10 section 10).
func CanTransfer(spec *Spec) bool {
	if spec == nil {
		return false
	}
	if slices.Contains(spec.Global.AlwaysAllowedTools, ToolTransferToAgent) {
		return true
	}
	for _, node := range spec.Nodes {
		if slices.Contains(node.Tools, "*") || slices.Contains(node.Tools, ToolTransferToAgent) {
			return true
		}
	}
	return false
}

// userDataPrefix is how business data a call was placed with appears in the
// slot store (see SeedSlots).
const userDataPrefix = "userData."

// UserDataNames lists the business-data names the flow reads: every
// userData.<name> its instructions, announces, transition conditions and HTTP
// bodies refer to. A flow does not declare its inputs anywhere else, so this
// is the vocabulary a campaign contact's slots may use. The result is sorted
// and free of duplicates.
func UserDataNames(spec *Spec) []string {
	if spec == nil {
		return nil
	}
	found := map[string]struct{}{}
	addSlot := func(slot string) {
		if name, ok := strings.CutPrefix(slot, userDataPrefix); ok && name != "" {
			found[name] = struct{}{}
		}
	}
	addText := func(text Text) {
		for _, s := range []string{text.EN, text.ZH} {
			for _, name := range slotRefs(s) {
				addSlot(name)
			}
		}
	}
	var addCondition func(c *Condition)
	addCondition = func(c *Condition) {
		if c == nil {
			return
		}
		addSlot(c.Slot)
		for i := range c.All {
			addCondition(&c.All[i])
		}
		for i := range c.Any {
			addCondition(&c.Any[i])
		}
	}
	addRules := func(rules []Transition) {
		for _, rule := range rules {
			addCondition(rule.Condition)
		}
	}
	var walkBody func(v any)
	walkBody = func(v any) {
		switch x := v.(type) {
		case string:
			for _, m := range templateReference.FindAllStringSubmatch(x, -1) {
				if m[1] == "slots" {
					addSlot(m[2])
				}
			}
		case map[string]any:
			for _, item := range x {
				walkBody(item)
			}
		case []any:
			for _, item := range x {
				walkBody(item)
			}
		}
	}

	addRules(spec.Global.Transitions)
	for _, node := range spec.Nodes {
		addText(node.Instruction)
		addText(node.Announce)
		addRules(node.Transitions)
	}
	for _, tool := range spec.Tools {
		if tool.HTTP != nil {
			walkBody(tool.HTTP.Body)
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
