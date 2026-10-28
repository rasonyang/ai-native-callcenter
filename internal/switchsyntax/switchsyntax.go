// SPDX-License-Identifier: Apache-2.0

// Package switchsyntax names what the switch reads as syntax in the places our
// values travel to: a dial string, the action list of an inline transfer, an
// application's data, a dialplan destination. It is a leaf, so the layer that
// accepts a value (catalog, the API) and the layer that sends it (the adapter
// in internal/telephony) refuse the same things.
//
// Nothing here escapes. Each grammar a value passes through has its own quoting
// rules, and a value usually passes through several, one after the other: an
// escape that is right for one is read again by the next. So a value either
// holds none of these or it does not reach the switch at all.
package switchsyntax

import (
	"strings"
	"unicode"
)

// Unsafe returns the first character or sequence in s that the switch would
// read as syntax rather than as part of the value, and "" when s holds none.
//
// Each refusal has a reason in the switch's own parsers:
//
//   - A line break ends a command on the event socket, and what follows is
//     read as a command of its own. Every other control character and every
//     space goes too: the api argument parser splits on a space outside
//     quotes, and the expansion form ${api args} needs one.
//   - ' opens and closes quoting in every pass of switch_separate_string: the
//     api arguments, an inline transfer's action list and a {…} block. Inside
//     the single-quoted inline transfer it ends the outer quoting.
//   - \ is the escape character of those same passes and of variable
//     expansion.
//   - ^ is the action separator an inline transfer sets with m:^:, and ^^ at
//     the start of any list changes its separator.
//   - , separates the variables of a {…} block and the endpoints of a dial
//     string, and | separates failover endpoints.
//   - { and } delimit a variable block, whose extent is found by counting them
//     before anything else is read; ${ opens an expansion.
//   - $ is variable and api expansion. The switch expands an application's data
//     before it runs it, the bridge of an inline transfer included, so
//     ${name(args)} in a value runs an api command.
//   - :_: is the enterprise originate separator. It is found with strstr over
//     the whole dial string, its variable block included, so no quoting holds
//     it.
//
// = is not refused: a {…} pair is split at its first =, so the rest of the
// value keeps any further one.
func Unsafe(s string) string {
	for _, r := range s {
		switch {
		case unicode.IsControl(r), unicode.IsSpace(r):
			return string(r)
		case strings.ContainsRune(`'\^,|{}$`, r):
			return string(r)
		}
	}
	if strings.Contains(s, enterpriseSeparator) {
		return enterpriseSeparator
	}
	return ""
}

// enterpriseSeparator splits a dial string into legs originated in parallel.
const enterpriseSeparator = ":_:"

// CallsCommand reports whether expanding s would run a command on the switch:
// some ${…} in it is not a plain variable reference such as ${sounds_dir} or
// $${hold_music}.
//
// For a value that is meant to be expanded, which a queue's sounds are: the
// switch expands a sound before it plays it (mod_callcenter's music on hold, a
// playback in the dialplan), and the stored default is $${hold_music}. Naming a
// variable is the feature. ${name(args)} and ${name args} run an api command
// instead, and a sound has no use for that.
func CallsCommand(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '$' || s[i+1] != '{' {
			continue
		}
		j := i + 2
		for j < len(s) && isVariableNameByte(s[j]) {
			j++
		}
		if j == i+2 || j >= len(s) || s[j] != '}' {
			return true
		}
		i = j
	}
	return false
}

// isVariableNameByte is what a channel or global variable's name is made of.
func isVariableNameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return b == '_' || b == '-' || b == '.'
}
