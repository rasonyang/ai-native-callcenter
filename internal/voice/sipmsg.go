// SPDX-License-Identifier: Apache-2.0

// Package voice terminates the AI leg: a minimal SIP user agent server, SDP
// negotiation, and an RTP session with jitter handling and DTMF. It speaks to
// FreeSWITCH over a register=false gateway and hands 8 kHz frames to the
// caller; it knows nothing about providers, flows, or the rest of the domain.
package voice

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
)

// The UAS handles exactly this much of SIP. Anything else is answered with a
// method-not-allowed rather than half-implemented.
type method string

const (
	methodInvite  method = "INVITE"
	methodAck     method = "ACK"
	methodBye     method = "BYE"
	methodCancel  method = "CANCEL"
	methodOptions method = "OPTIONS"
	methodInfo    method = "INFO"
)

var knownMethods = map[string]method{
	"INVITE": methodInvite, "ACK": methodAck, "BYE": methodBye,
	"CANCEL": methodCancel, "OPTIONS": methodOptions, "INFO": methodInfo,
}

const allowedMethods = "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO"

// Via, Record-Route and Route carry ordered multiple values that a response
// must echo exactly; every other header is kept as a single value.
var multiValueHeaders = map[string]bool{"via": true, "record-route": true, "route": true}

// Proxies are free to use the compact header forms.
var compactHeaders = map[string]string{
	"v": "via", "i": "call-id", "f": "from", "t": "to", "m": "contact",
}

// sipMessage is a parsed request or response. Header keys are lowercased, so
// lookups are case insensitive as RFC 3261 requires.
type sipMessage struct {
	method     method // empty for responses and unsupported methods
	requestURI string
	statusCode int // zero for requests
	headers    map[string]string
	multi      map[string][]string
	body       string
}

func (m *sipMessage) get(name string) string { return m.headers[strings.ToLower(name)] }

func (m *sipMessage) sipCallID() string  { return m.get("Call-ID") }
func (m *sipMessage) fromHeader() string { return m.get("From") }
func (m *sipMessage) toHeader() string   { return m.get("To") }
func (m *sipMessage) cseq() string       { return m.get("CSeq") }
func (m *sipMessage) contact() string    { return m.get("Contact") }

func (m *sipMessage) vias() []string         { return m.multi["via"] }
func (m *sipMessage) recordRoutes() []string { return m.multi["record-route"] }

// cseqNumber returns the sequence number alone, which is what a response to a
// cancelled INVITE needs in order to be paired with the right transaction.
func (m *sipMessage) cseqNumber() string {
	number, _, _ := strings.Cut(m.cseq(), " ")
	return strings.TrimSpace(number)
}

// customHeaders returns the X-* headers the dialplan attached to the INVITE.
// This is how a call is correlated back to its FreeSWITCH leg, so the original
// value is preserved verbatim and only the key is normalised.
func (m *sipMessage) customHeaders() map[string]string {
	out := map[string]string{}
	for k, v := range m.headers {
		if strings.HasPrefix(k, "x-") {
			out[headerTitleCase(k)] = v
		}
	}
	return out
}

func headerTitleCase(s string) string {
	var b strings.Builder
	upper := true
	for _, ch := range s {
		if upper && ch >= 'a' && ch <= 'z' {
			b.WriteRune(ch - 32)
		} else {
			b.WriteRune(ch)
		}
		isLetter := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		upper = !isLetter
	}
	return b.String()
}

func parseSIP(data []byte) (*sipMessage, error) {
	head, body, _ := strings.Cut(string(data), "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, fmt.Errorf("empty SIP message")
	}

	msg := &sipMessage{headers: map[string]string{}, multi: map[string][]string{}}
	first := lines[0]
	if strings.HasPrefix(first, "SIP/") {
		parts := strings.SplitN(first, " ", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("malformed status line %q", first)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("malformed status code %q", parts[1])
		}
		msg.statusCode = code
	} else {
		parts := strings.SplitN(first, " ", 3)
		msg.method = knownMethods[parts[0]]
		if len(parts) > 1 {
			msg.requestURI = parts[1]
		}
	}

	for _, line := range unfoldHeaders(lines[1:]) {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if full, isCompact := compactHeaders[key]; isCompact {
			key = full
		}
		value = strings.TrimSpace(value)
		if multiValueHeaders[key] {
			msg.multi[key] = append(msg.multi[key], splitHeaderValues(value)...)
		}
		if _, exists := msg.headers[key]; !exists {
			msg.headers[key] = value
		}
	}
	msg.body = strings.TrimSpace(body)
	return msg, nil
}

// unfoldHeaders joins folded header lines. A line that starts with a space or
// a tab continues the header above it (RFC 3261 §7.3.1), and the line break
// with the whitespace after it reads as a single space. Cutting each line at
// its colon without this keeps only the first line of a folded header:
// `Record-Route: <sip:p2;lr>,` then ` <sip:p1;lr>` loses p1 from the route
// set. A continuation with no header above it is dropped.
func unfoldHeaders(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(out) > 0 {
				out[len(out)-1] += " " + strings.TrimLeft(line, " \t")
			}
			continue
		}
		out = append(out, line)
	}
	return out
}

// splitHeaderValues splits on commas that separate header values, ignoring the
// ones inside a URI or a quoted display name.
func splitHeaderValues(value string) []string {
	var parts []string
	var current strings.Builder
	depth, quoted := 0, false
	for _, ch := range value {
		switch {
		case ch == '"':
			quoted = !quoted
		case ch == '<' && !quoted:
			depth++
		case ch == '>' && !quoted:
			depth--
		case ch == ',' && depth == 0 && !quoted:
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}

	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

//
// Response building.
//

// echoRouting reproduces every Via and Record-Route in order. Getting this
// wrong makes the response unroutable back through any proxy in the path.
func echoRouting(msg *sipMessage) string {
	var lines []string
	if vias := msg.vias(); len(vias) > 0 {
		for _, v := range vias {
			lines = append(lines, "Via: "+v)
		}
	} else {
		lines = append(lines, "Via: "+msg.get("Via"))
	}
	for _, r := range msg.recordRoutes() {
		lines = append(lines, "Record-Route: "+r)
	}
	return strings.Join(lines, "\r\n")
}

// buildResponse answers msg. cseqOverride replaces the CSeq line when the
// response belongs to a different transaction than the message that triggered
// it, which is the case for the 487 a CANCEL produces.
func buildResponse(msg *sipMessage, status, toTag, cseqOverride, extraHeaders string, body []byte) []byte {
	to := msg.toHeader()
	if toTag != "" && !strings.Contains(to, "tag=") {
		to += ";tag=" + toTag
	}
	cseq := msg.cseq()
	if cseqOverride != "" {
		cseq = cseqOverride
	}

	head := "SIP/2.0 " + status + "\r\n" +
		echoRouting(msg) + "\r\n" +
		"From: " + msg.fromHeader() + "\r\n" +
		"To: " + to + "\r\n" +
		"Call-ID: " + msg.sipCallID() + "\r\n" +
		"CSeq: " + cseq + "\r\n"
	if extraHeaders != "" {
		head += extraHeaders
		if !strings.HasSuffix(extraHeaders, "\r\n") {
			head += "\r\n"
		}
	}
	head += fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	return append([]byte(head), body...)
}

func build100Trying(invite *sipMessage) []byte {
	return buildResponse(invite, "100 Trying", "", "", "", nil)
}

func build200OKInvite(invite *sipMessage, sdp, localIP string, localPort int, toTag string) []byte {
	extra := fmt.Sprintf("Contact: <sip:aicc@%s:%d>\r\nAllow: %s\r\nContent-Type: application/sdp\r\n",
		localIP, localPort, allowedMethods)
	return buildResponse(invite, "200 OK", toTag, "", extra, []byte(sdp))
}

func build200OK(msg *sipMessage) []byte {
	return buildResponse(msg, "200 OK", "", "", "", nil)
}

func build200OKOptions(options *sipMessage) []byte {
	return buildResponse(options, "200 OK", "", "", "Allow: "+allowedMethods+"\r\n", nil)
}

func buildReject(msg *sipMessage, status, toTag string) []byte {
	return buildResponse(msg, status, toTag, "", "", nil)
}

// build487 terminates the INVITE a CANCEL just cancelled.
//
// The response belongs to the INVITE transaction, so it carries the INVITE's
// CSeq method — answering with the CANCEL's own CSeq leaves the caller waiting
// for a final response that never comes.
func build487(invite *sipMessage, toTag string) []byte {
	return buildResponse(invite, "487 Request Terminated", toTag,
		invite.cseqNumber()+" INVITE", "", nil)
}

var (
	uriInAnglesPattern = regexp.MustCompile(`<(sips?:[^>]+)>`)
	bareURIPattern     = regexp.MustCompile(`(sips?:\S+)`)
)

func extractURI(header string) string {
	if uri, ok := findURI(header); ok {
		return uri
	}
	return "sip:unknown@0.0.0.0"
}

// findURI is extractURI for a caller that needs to know there was no URI,
// rather than be handed a placeholder it might send something to.
func findURI(header string) (string, bool) {
	if m := uriInAnglesPattern.FindStringSubmatch(header); m != nil {
		return m[1], true
	}
	if m := bareURIPattern.FindStringSubmatch(header); m != nil {
		return m[1], true
	}
	return "", false
}

func appendTag(header, tag string) string {
	if strings.Contains(header, "tag=") {
		return header
	}
	return header + ";tag=" + tag
}

// byeReason is why a dialog is being ended, carried on the BYE as RFC 3326
// Reason headers.
//
// Without it a BYE says only that the call is over, and every way a bot leg
// can end looks identical on the wire: the conversation reaching its goodbye,
// the process being restarted, a crash.
//
// The reader these are for is the capture, not the switch (owner directive
// 2026-08-25). A SIP capture — Homer or anything else recording the
// signalling — has the BYE and nothing else: it cannot see this process's
// logs, its ledger or its channel variables. A bare BYE there is
// indistinguishable from a caller hanging up, so "were these three hundred
// calls cut by a deploy?" is a question the capture cannot answer. With the
// Reason it is a search.
//
// Measured on this deployment (2026-08-25), because the far end's behaviour is
// worth knowing before anything is built on it: the headers do go out
// correctly — a SIP trace shows both, above Content-Length — and FreeSWITCH
// does not act on them. The bot leg still hangs up NORMAL_CLEARING, and
// aicc_inbound.lua's warning reads originate_disposition, which reports how
// the dial went and says SUCCESS. What tells a restart from a bot that
// finished, inside this system, is the aicc_bot_finished stamp, and that
// already existed. Nothing here depends on the switch reading Reason.
type byeReason struct {
	// SIPCause and SIPText are the protocol's own answer (503, 480…).
	SIPCause int
	SIPText  string
	// Q850Cause and Q850Text are the telephony answer, which is the one the
	// switch turns into a hangup cause. 41 is temporary failure.
	Q850Cause int
	Q850Text  string
}

// byeReasonRestart is the bot leg ending because this process is going away.
//
// It is a service restart and nothing to do with the caller or the
// conversation. The caller is not left behind either way: the dialplan sets
// continue_on_fail on the bridge for exactly this, and hands them to the
// number's fallback queue — verified end to end on 2026-08-25 by killing the
// process three seconds into a conversation and watching the switch transfer
// the caller to 7001.
var byeReasonRestart = byeReason{
	SIPCause: 503, SIPText: "Service Restart",
	Q850Cause: 41, Q850Text: "Temporary failure",
}

func (r byeReason) headers() string {
	if r.SIPCause == 0 && r.Q850Cause == 0 {
		return ""
	}
	var out strings.Builder
	if r.SIPCause != 0 {
		fmt.Fprintf(&out, "Reason: SIP;cause=%d;text=%q\r\n", r.SIPCause, r.SIPText)
	}
	if r.Q850Cause != 0 {
		fmt.Fprintf(&out, "Reason: Q.850;cause=%d;text=%q\r\n", r.Q850Cause, r.Q850Text)
	}
	return out.String()
}

// buildBye ends a dialog we accepted. From and To swap relative to the INVITE,
// and the request is addressed through the route set, which is the INVITE's
// Record-Route values in the order they arrived.
//
// That order is the one RFC 3261 §12.1.1 defines for our side: a UAS's route
// set is the Record-Route field values as they appear in the request. Only the
// UAC reverses (§12.1.2), because its list came back in the response, which a
// proxy builds by prepending as the request travels — the response's order is
// the request's reversed, so reversing again restores the path outward. Doing
// that here walked a multi-proxy path backwards, from the proxy nearest the
// caller toward the one nearest us; the far end never saw the BYE and the call
// stayed up until a timer cleared it. Nothing in this deployment's topology put
// more than one Record-Route in the request, which is why it took a second
// proxy to notice (issue #250).
//
// Each value is written as its own header. The parser has already split the
// comma-joined ones and joined the folded ones, so however the INVITE grouped
// them the BYE carries one Route header per entry, in order. When the first
// entry is a loose router (`;lr`) the Request-URI is the peer's Contact and
// every entry is a Route. When it is a strict router (no `;lr`), §12.2.1.1
// puts its URI in the Request-URI, the remaining entries in the Route headers,
// and the Contact last among them; routeRequest does both. No peer on this
// path record-routes strictly, but a strict router handed the loose form
// would forward on the Request-URI, the Contact, and skip every entry after it.
//
// Where the BYE is sent is not decided here: with a route set it is the first
// entry's address (Dialog.byeNextHop), whichever form the request took.
//
// Every BYE is a new transaction, so it carries a branch of its own and the
// Max-Forwards every request needs (§8.1.1). The branch used to be the
// Call-ID's first eight characters, which repeats across calls whose Call-IDs
// share a prefix; a transaction-stateful proxy can absorb the second such BYE
// as a retransmission of the first (issue #325).
func buildBye(callID, fromHeader, toHeader, localTag, localIP string,
	localPort int, routeSet []string, remoteContact string, reason byeReason) []byte {

	target := extractURI(fromHeader)
	if remoteContact != "" {
		target = extractURI(remoteContact)
	}
	routing := routeRequest(routeSet, target)
	var routes strings.Builder
	for _, route := range routing.routes {
		routes.WriteString("Route: " + route + "\r\n")
	}

	return fmt.Appendf(nil,
		"BYE %s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP %s:%d;branch=%s\r\n"+
			"Max-Forwards: 70\r\n"+
			"%s"+
			"From: %s\r\n"+
			"To: %s\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: 1 BYE\r\n"+
			"%s"+
			"Content-Length: 0\r\n"+
			"\r\n",
		routing.requestURI, localIP, localPort, newBranch(), routes.String(),
		appendTag(toHeader, localTag), fromHeader, callID, reason.headers())
}

// newBranch is a Via branch for a new client transaction: the RFC 3261 magic
// cookie and 64 random bits, so no two requests this process sends share one.
func newBranch() string { return fmt.Sprintf("z9hG4bK%016x", rand.Uint64()) }

var dtmfSignalPattern = regexp.MustCompile(`(?i)Signal\s*=\s*([0-9A-D#*])`)

// parseInfoDTMF reads a digit out of a SIP INFO body. Both the dtmf-relay form
// and the bare application/dtmf form appear in the wild.
func parseInfoDTMF(msg *sipMessage) string {
	contentType := strings.ToLower(msg.get("Content-Type"))
	switch {
	case strings.Contains(contentType, "dtmf-relay"):
		if m := dtmfSignalPattern.FindStringSubmatch(msg.body); m != nil {
			return strings.ToUpper(m[1])
		}
	case strings.TrimSpace(contentType) == "application/dtmf":
		digit := strings.TrimSpace(msg.body)
		if digit != "" && strings.ContainsRune("0123456789ABCD#*", rune(digit[0])) {
			return strings.ToUpper(digit[:1])
		}
	}
	return ""
}
