// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/rasonyang/ai-native-callcenter/internal/dayrange"
)

// The outbound ring timeout (AICC_OUTBOUND_RING_TIMEOUT) and the window an
// originated leg may take to be answered. The window is a fixed property of
// the originator, not a setting: the ring timeout is validated against it.
const (
	// OutboundAnswerWindow is how long the originator holds the continuation
	// armed on an originated leg; an answer later than this is not bridged.
	OutboundAnswerWindow = 60 * time.Second
	// DefaultOutboundRingTimeout leaves 15 s between the switch giving up and
	// the window closing, covering the arm delay, retransmissions and a slow
	// trunk. It is also under the 60 s the switch would apply on its own.
	DefaultOutboundRingTimeout = 45 * time.Second
	// MinOutboundRingTimeout keeps a typo from ringing a phone too briefly
	// to be answered.
	MinOutboundRingTimeout = 5 * time.Second
)

// ValidateOutboundRingTimeout checks AICC_OUTBOUND_RING_TIMEOUT: at least
// MinOutboundRingTimeout and strictly below OutboundAnswerWindow, so a leg
// the switch is still ringing can never be answered into a window that has
// already closed.
func ValidateOutboundRingTimeout(d time.Duration) error {
	if d < MinOutboundRingTimeout || d >= OutboundAnswerWindow {
		return fmt.Errorf("AICC_OUTBOUND_RING_TIMEOUT must be at least %s and strictly below the %s answer window, got %s",
			MinOutboundRingTimeout, OutboundAnswerWindow, d)
	}
	return nil
}

// SIPHeader is one SIP header the deployment asks the AI outbound customer
// leg to carry. Name is as written by the operator (case preserved).
type SIPHeader struct {
	Name  string
	Value string
}

// ValidateOutboundEndpoint checks AICC_OUTBOUND_ENDPOINT. Empty is allowed
// (placing an AI outbound call is then refused, as it always was). Otherwise
// the value must be a sofia/ dial string with exactly one %s, the number, and
// no other % verb: loopback cannot carry this path (C47), and a stray verb
// would corrupt every dial string at call time.
func ValidateOutboundEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	const name = "AICC_OUTBOUND_ENDPOINT"
	if !strings.HasPrefix(endpoint, "sofia/") {
		return fmt.Errorf("%s %q must be a sofia/ dial string such as sofia/gateway/<gw>/%%s", name, endpoint)
	}
	if n := strings.Count(endpoint, "%s"); n != 1 {
		return fmt.Errorf("%s %q must contain exactly one %%s for the number, found %d", name, endpoint, n)
	}
	if strings.Contains(strings.Replace(endpoint, "%s", "", 1), "%") {
		return fmt.Errorf("%s %q must not contain a %% verb other than the single %%s", name, endpoint)
	}
	return nil
}

// ParseOutboundSIPHeaders parses AICC_OUTBOUND_SIP_HEADERS: Name=value pairs
// separated by ;, whitespace around names, values and separators trimmed.
// Parsing is strict: every segment must be Name=value, split on the first =,
// so an empty segment or a segment without = is an error. The order is kept.
// An empty (or all-blank) string means no headers.
func ParseOutboundSIPHeaders(raw string) ([]SIPHeader, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	segments := strings.Split(raw, ";")
	headers := make([]SIPHeader, 0, len(segments))
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil, fmt.Errorf("%s: empty segment in %q (a leading, trailing or doubled ';')",
				outboundHeadersVar, raw)
		}
		name, value, ok := strings.Cut(seg, "=")
		if !ok {
			return nil, fmt.Errorf("%s: pair %q is not Name=value", outboundHeadersVar, seg)
		}
		headers = append(headers, SIPHeader{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	if err := ValidateOutboundSIPHeaders(headers); err != nil {
		return nil, err
	}
	return headers, nil
}

const outboundHeadersVar = "AICC_OUTBOUND_SIP_HEADERS"

// ValidateOutboundSIPHeaders holds an already-built slice to the rules the
// parser applies, so a config assembled in code is no looser than one read
// from the environment.
func ValidateOutboundSIPHeaders(headers []SIPHeader) error {
	seen := make(map[string]string, len(headers))
	for _, h := range headers {
		pair := h.Name + "=" + h.Value
		bad := func(format string, args ...any) error {
			return fmt.Errorf("%s: pair %q: %s", outboundHeadersVar, pair, fmt.Sprintf(format, args...))
		}
		lower := strings.ToLower(h.Name)
		switch {
		case !strings.HasPrefix(lower, "x-"):
			return bad("name must start with X-")
		case strings.HasPrefix(lower, "x-aicc-"):
			return bad("the X-AICC- namespace belongs to the application")
		}
		for _, r := range h.Name {
			if !isHeaderTokenRune(r) {
				return bad("name holds %q, which is not allowed in a header name", r)
			}
		}
		if first, dup := seen[lower]; dup {
			return bad("name repeats %q (names are compared case-insensitively)", first)
		}
		seen[lower] = h.Name
		if h.Value == "" {
			return bad("value is empty")
		}
		for _, r := range h.Value {
			switch {
			case unicode.IsControl(r):
				return bad("value holds a control character (%U)", r)
			case r == '{' || r == '}':
				return bad("value must not contain %q (it would break the originate variable block)", r)
			case r == ';':
				return bad("value must not contain ';', the pair separator")
			}
		}
	}
	return nil
}

// isHeaderTokenRune reports whether r may appear in a header name: the RFC
// 3261 token set, less the single quote, which the originate grammar treats
// as quoting.
func isHeaderTokenRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-.!%*_+`~", r)
}

// DefaultOutboundHours are the outer calling hours when AICC_OUTBOUND_HOURS is
// unset: 08:00 to 21:00 in the callee's local time, every day.
const DefaultOutboundHours = "08:00-21:00"

// ParseOutboundHours reads AICC_OUTBOUND_HOURS, "HH:MM-HH:MM". The range may
// cross midnight (for a deployment that calls evenings and nights) but cannot
// be empty.
func ParseOutboundHours(raw string) (dayrange.Range, error) {
	r, err := dayrange.Parse(strings.TrimSpace(raw))
	if err != nil {
		return dayrange.Range{}, fmt.Errorf("AICC_OUTBOUND_HOURS must be HH:MM-HH:MM such as %s, got %q: %w",
			DefaultOutboundHours, raw, err)
	}
	return r, nil
}
