// SPDX-License-Identifier: Apache-2.0

// Package callidgate is a CI gate: the key "callId" in a log record, span
// attribute or metric label always carries the domain call id (#439), a
// github.com/google/uuid.UUID or the String() of one. A SIP Call-ID is
// "sipCallId", a switch channel "channelId", a leg "legId" or "partyId", a
// provider tool call "toolCallId".
//
// Unlike uuidgate this gate type-checks: it loads every non-test package of
// the module with golang.org/x/tools/go/packages and fails if one does not
// type-check, because it cannot vouch for code it cannot type. It finds each
// expression whose constant value is "callId" (a named const counts) in these
// positions and requires the value that goes with it to be domain-typed:
//
//   - an argument whose parameter is any (slog's ...any on *slog.Logger and
//     the package functions, the repo's own logger interfaces, With): the
//     next argument is the value, and a trailing "callId" is an offence;
//   - an element of a []any composite literal: the next element;
//   - an argument of append into a []any: the next argument;
//   - the key of a two-parameter log/slog or go.opentelemetry.io/otel/attribute
//     constructor (slog.String, slog.Any, attribute.String, ...), and the
//     attribute.Key("callId").String(v) form: the second argument or v.
//
// Map keys, struct tags, struct field values and format strings are not
// attributes and are never flagged.
//
// Limits: a slice spread into a call (f(attrs...)) is not checked at the
// call; the literals and appends that build the slice are. A key held in a
// non-constant variable, or a pair assembled by other means (a map ranged into
// a slice, a helper returning the pair), is invisible to the gate, and so is a
// slog.Attr struct literal. A wrapper whose parameter is a string rather than
// any is not an attribute position.
package callidgate
