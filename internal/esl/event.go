// SPDX-License-Identifier: Apache-2.0

// Package esl is a minimal FreeSWITCH Event Socket client for inbound mode.
//
// It speaks only what this project needs: authenticate, subscribe to plain
// events, issue api/bgapi commands, and hand normalized events upward. Raw
// FreeSWITCH header names never escape this package's callers boundary — see
// internal/telephony for the mapping layer.
package esl

import (
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Event is one FreeSWITCH event: header names as sent, values URL-decoded.
type Event struct {
	headers map[string]string
	// Body carries the event body when one is present (rare for plain events).
	Body string
}

// NewEvent builds an Event from decoded headers; used by tests and parsing.
func NewEvent(headers map[string]string, body string) *Event {
	return &Event{headers: headers, Body: body}
}

// Get returns a header value, or "" when absent. Lookup is case-insensitive
// because FreeSWITCH is inconsistent across event classes.
func (e *Event) Get(name string) string {
	if e == nil {
		return ""
	}
	if v, ok := e.headers[name]; ok {
		return v
	}
	lower := strings.ToLower(name)
	for k, v := range e.headers {
		if strings.ToLower(k) == lower {
			return v
		}
	}
	return ""
}

// GetFirst returns the first non-empty value among names, allowing documented
// fallback chains such as Caller-ANI then Caller-Caller-ID-Number.
func (e *Event) GetFirst(names ...string) string {
	for _, n := range names {
		if v := e.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// GetInt parses a header as an integer, returning ok=false when absent or
// malformed.
func (e *Event) GetInt(name string) (int64, bool) {
	v := e.Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Name is the event name, e.g. CHANNEL_ANSWER.
func (e *Event) Name() string { return e.Get("Event-Name") }

// Subclass is the CUSTOM event subclass, e.g. sofia::register.
func (e *Event) Subclass() string { return e.Get("Event-Subclass") }

// UniqueID is the channel UUID this event belongs to, when any.
func (e *Event) UniqueID() string { return e.GetFirst("Unique-ID", "Caller-Unique-ID") }

// Variable returns a channel variable, i.e. the variable_<name> header.
func (e *Event) Variable(name string) string { return e.Get("variable_" + name) }

// Timestamp returns the switch-side event time. FreeSWITCH sends microseconds
// since the epoch in Event-Date-Timestamp; a missing value yields the zero time.
func (e *Event) Timestamp() time.Time {
	usec, ok := e.GetInt("Event-Date-Timestamp")
	if !ok {
		return time.Time{}
	}
	return time.UnixMicro(usec).UTC()
}

// Headers returns a copy of all headers, for debugging and tests.
func (e *Event) Headers() map[string]string {
	return maps.Clone(e.headers)
}

// parseEventBody parses the "text/event-plain" payload: one header per line,
// values percent-encoded, an empty line separating an optional body.
func parseEventBody(payload string) *Event {
	headers := make(map[string]string, 64)
	body := ""

	lines := strings.Split(payload, "\n")
	for i, line := range lines {
		if line == "" {
			body = strings.Join(lines[i+1:], "\n")
			break
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		headers[name] = value
	}
	return &Event{headers: headers, Body: body}
}
