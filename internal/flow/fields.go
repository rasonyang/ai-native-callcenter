// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// A key the spec does not declare is a mistake the decoder cannot see:
// encoding/json drops it without a word, so a phase with "anounce" loads, and
// publishes, as a phase with no line at all. Every such key is found here, by
// walking the document against the Go types it decodes into and their json
// tags. The types are the vocabulary, so a field added to the spec later is
// known here the moment it has a tag, and nothing needs to be listed twice.
//
// What is not a field is not checked: the keys of a map are names the author
// chooses (phases, tools, slots, headers, query parameters, modelOnly
// outputs), a tool's parameters are a JSON Schema the model receives as
// written, and a body or a condition's value is data.

// UnknownField is a key in a flow document that the spec does not declare.
type UnknownField struct {
	// Where names the object that holds the key, in the words the flow's
	// other problems use: `the flow`, `global`, `phase "close"`,
	// `global transition 0`, `tool "lookup" http successWhen`.
	Where string
	// Field is the key as written.
	Field string
	// Suggestion is the declared field the key is a near miss of, when
	// exactly one is close (see suggestField); empty otherwise.
	Suggestion string

	// noun and known name the object's vocabulary when it is short enough to
	// list in the sentence (objectNouns).
	noun  string
	known []string
}

// String is the problem as a sentence, the way every load problem is worded:
// `phase "close" has unknown field "anounce"; did you mean "announce"?`.
func (u UnknownField) String() string {
	problem := fmt.Sprintf("%s has unknown field %q", u.Where, u.Field)
	switch {
	case u.Suggestion != "":
		return problem + fmt.Sprintf("; did you mean %q?", u.Suggestion)
	case u.noun != "":
		return problem + "; " + u.noun + " has " + joinWithAnd(u.known)
	}
	return problem
}

// objectShapes maps a type that reads itself (UnmarshalJSON) and whose Go
// fields are not its JSON keys to the struct its object form decodes into.
// The decoders use the same structs, so the two cannot drift apart.
var objectShapes = map[reflect.Type]reflect.Type{
	reflect.TypeFor[Text]():           reflect.TypeFor[textByLang](),
	reflect.TypeFor[TextList]():       reflect.TypeFor[textListByLang](),
	reflect.TypeFor[ModelOnlyField](): reflect.TypeFor[modelOnlyProjection](),
}

// objectNouns names the objects whose whole vocabulary a problem lists when it
// has no field to suggest. global and an http block are left out: a dozen
// names and more do not make a sentence, and a near miss is still suggested
// there. A type missing from this map is checked all the same.
var objectNouns = map[reflect.Type]string{
	reflect.TypeFor[Spec]():                "a flow",
	reflect.TypeFor[Node]():                "a phase",
	reflect.TypeFor[Transition]():          "a transition",
	reflect.TypeFor[Condition]():           "a condition",
	reflect.TypeFor[Tool]():                "a tool",
	reflect.TypeFor[Collect]():             "collect",
	reflect.TypeFor[SuccessTest]():         "successWhen",
	reflect.TypeFor[DeclaredLead]():        "a lead",
	reflect.TypeFor[modelOnlyProjection](): "a projection",
	reflect.TypeFor[textByLang]():          "a text keyed by language",
	reflect.TypeFor[textListByLang]():      "a text list keyed by language",
}

// entryNames is how a problem names one entry of a collection field: a phase
// rather than nodes "close", a transition rather than transitions 0.
var entryNames = map[string]string{
	"nodes":       "phase",
	"tools":       "tool",
	"transitions": "transition",
}

// rootWhere names the document itself.
const rootWhere = "the flow"

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// unknownFields lists every key in data that the spec does not declare, in a
// stable order. A document that is not JSON has none: the decoder reports it.
func unknownFields(data []byte) []UnknownField {
	var doc any
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	var found []UnknownField
	walkFields(doc, reflect.TypeFor[Spec](), rootWhere, &found)
	return found
}

// walkFields checks value, which decodes into t, and everything inside it. A
// value of the wrong JSON kind is skipped: the decoder or validate reports it.
func walkFields(value any, t reflect.Type, where string, found *[]UnknownField) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if shape, ok := objectShapes[t]; ok {
		t = shape
	}
	if t.Kind() != reflect.Struct {
		return
	}
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	fields := jsonFields(t)
	for _, key := range slices.Sorted(maps.Keys(object)) {
		field, ok := fields[key]
		if !ok {
			unknown := UnknownField{Where: where, Field: key,
				Suggestion: suggestField(key, slices.Collect(maps.Keys(fields)))}
			if noun, ok := objectNouns[t]; ok {
				unknown.noun, unknown.known = noun, declaredOrder(t)
			}
			*found = append(*found, unknown)
			continue
		}
		walkField(object[key], field.Type, where, key, found)
	}
}

// walkField checks one field's value: an object directly, a map or a list
// entry by entry, each named as the problems name it.
func walkField(value any, t reflect.Type, parent, key string, found *[]UnknownField) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	base := fieldWhere(parent, key)
	if entry, ok := entryNames[key]; ok {
		base = entry
		if parent != rootWhere {
			base = parent + " " + entry
		}
	}
	switch {
	case t == rawMessageType:
	case t.Kind() == reflect.Map:
		entries, _ := value.(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			walkFields(entries[name], t.Elem(), base+" "+strconv.Quote(name), found)
		}
	case t.Kind() == reflect.Slice:
		items, _ := value.([]any)
		for i, item := range items {
			walkFields(item, t.Elem(), base+" "+strconv.Itoa(i), found)
		}
	default:
		walkFields(value, t, fieldWhere(parent, key), found)
	}
}

// fieldWhere names a field of the object at parent: by its key at the top,
// global.<key> as the loader's other problems write it, and parent and key
// otherwise (phase "close" announce, tool "lookup" http).
func fieldWhere(parent, key string) string {
	switch parent {
	case rootWhere:
		return key
	case "global":
		return "global." + key
	}
	return parent + " " + key
}

// jsonFields is the vocabulary of a struct: every key encoding/json would
// decode into it, by the name its tag gives.
func jsonFields(t reflect.Type) map[string]reflect.StructField {
	fields := map[string]reflect.StructField{}
	for _, field := range reflect.VisibleFields(t) {
		name, ok := jsonName(field)
		if !ok {
			continue
		}
		fields[name] = field
	}
	return fields
}

// declaredOrder is the struct's vocabulary in the order the spec declares it,
// for a problem to list.
func declaredOrder(t reflect.Type) []string {
	var names []string
	for _, field := range reflect.VisibleFields(t) {
		if name, ok := jsonName(field); ok {
			names = append(names, name)
		}
	}
	return names
}

// jsonName is the key a struct field decodes from, and whether it decodes from
// one at all: unexported fields, fields tagged "-" and embedded structs (whose
// fields stand in for them) do not.
func jsonName(field reflect.StructField) (string, bool) {
	if !field.IsExported() || field.Anonymous && field.Type.Kind() == reflect.Struct {
		return "", false
	}
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	switch name {
	case "-":
		return "", false
	case "":
		return field.Name, true
	}
	return name, true
}

// suggestField is the one declared field a key is a near miss of, or empty.
//
// A key that differs only in case is the nearest there is. Otherwise a field
// is close when the key is within a small edit distance of it, adjacent
// letters swapped counting as one edit, and the key is suggested only when
// exactly one field is close: two candidates would be a guess.
func suggestField(key string, known []string) string {
	lower := strings.ToLower(key)
	var close []string
	for _, field := range known {
		if strings.EqualFold(key, field) {
			return field
		}
		if editDistance(lower, strings.ToLower(field)) <= closeEnough(field) {
			close = append(close, field)
		}
	}
	if len(close) == 1 {
		return close[0]
	}
	return ""
}

// closeEnough is how many edits still read as a misspelling of field. A short
// name has few letters to spare: "on" is not a misspelling of "op".
func closeEnough(field string) int {
	switch n := len(field); {
	case n <= 2:
		return 0
	case n <= 5:
		return 1
	}
	return 2
}

// editDistance counts the insertions, deletions, substitutions and swaps of
// adjacent letters that turn a into b (optimal string alignment).
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}

// joinWithAnd lists names as a sentence does: "a, b and c".
func joinWithAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
