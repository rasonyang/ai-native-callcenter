// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/ratemeter"
	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

// A provider's quota is a rate, not a population (#604). The vendors here limit
// how many sessions are opened per minute and how many tokens are spent per
// minute; the one that documents it says there is no limit on how many are held
// at once (docs/design/06-capacity.md section 4). This file is what every client
// shares to make that quota visible: what a refused start was, what a turn cost
// and what the upgrade said about the limits. It knows no protocol event; each
// client decodes its own and hands the numbers here.

// The statuses of a start failure (aicc_provider_dial_failures_total{status})
// that are not an HTTP status. A refused upgrade is its HTTP status as a number
// ("429"), and a socket the provider closed during setup is CLOSE_<code>.
const (
	// StartStatusErrorEvent is a provider error event during setup.
	StartStatusErrorEvent = "ERROR_EVENT"
	// StartStatusTimeout is a setup the provider never confirmed within the
	// budget (wsconn.DialTimeout), or a dial that timed out.
	StartStatusTimeout = "TIMEOUT"
	// StartStatusNetwork is a failure with no answer from the provider to
	// classify: the dial failed without an HTTP response (name resolution, a
	// refused connection, TLS), or the socket broke without a close frame.
	StartStatusNetwork = "NETWORK"
	// StartStatusHTTPOther is an HTTP status outside 100-599, and
	// StartStatusCloseOther a close code outside the protocol's 1000-4999, so
	// that no answer, however odd, mints a series of its own.
	StartStatusHTTPOther  = "HTTP_OTHER"
	StartStatusCloseOther = "CLOSE_OTHER"
)

// StartReason is what a start failure means for the quota.
type StartReason string

const (
	// StartReasonQuotaExceeded is a refusal because the provider's quota is
	// spent: an HTTP 429, or vendor text that says so (isQuotaText).
	StartReasonQuotaExceeded StartReason = "QUOTA_EXCEEDED"
	// StartReasonAuthRejected is a credential the provider refused: HTTP 401
	// or 403, or vendor text about the key (isAuthText).
	StartReasonAuthRejected StartReason = "AUTH_REJECTED"
	// StartReasonOther is everything else.
	StartReasonOther StartReason = "OTHER"
)

// ErrQuotaExceeded matches, with errors.Is, a session start the provider refused
// because its quota is spent. Callers ask this rather than read the provider's
// words, which are vendor text and belong to this package.
var ErrQuotaExceeded = errors.New("provider: quota exceeded")

// StartFailure is a session that could not be started, as a number: the
// bounded status and reason it was counted under, which limit a quota refusal
// spoke for, the request id the vendor's support asks for, and what the
// provider said. Its text is the text of the error it wraps, so a record that
// carried the old error reads the same.
type StartFailure struct {
	Provider string
	Status   string
	Reason   StartReason
	// Limit is which limit a quota refusal spoke for (ratemeter.Limit);
	// empty when Reason is not QUOTA_EXCEEDED (#652).
	Limit     ratemeter.Limit
	RequestID string
	// Detail is the provider's own words: a close frame's text, an error
	// event's code and message, the start of a refused upgrade's body.
	Detail string
	Err    error
}

func (f *StartFailure) Error() string { return f.Err.Error() }
func (f *StartFailure) Unwrap() error { return f.Err }

// Is makes a quota refusal match ErrQuotaExceeded.
func (f *StartFailure) Is(target error) bool {
	return target == ErrQuotaExceeded && f.Reason == StartReasonQuotaExceeded
}

// EventError is a provider's error event as its client decoded it. Each
// client's wire type implements it, so a refusal said in an event is classified
// the same way whatever protocol said it.
type EventError interface {
	error
	EventCode() string
	EventMessage() string
}

// StartTimedOut is the error of a setup the provider never confirmed within its
// budget. It is classified TIMEOUT; its text is what.
func StartTimedOut(what string) error { return startTimeout(what) }

type startTimeout string

func (e startTimeout) Error() string { return string(e) }
func (startTimeout) Timeout() bool   { return true }

// ClassifyStart says what a failed session start was. The first match wins, in
// the order the provider can have said it: an error event, a close frame, a
// refused upgrade; then a timeout, and anything else is NETWORK. limit is the
// limit a quota refusal spoke for, empty for anything else (#652).
func ClassifyStart(err error) (status string, reason StartReason, limit ratemeter.Limit, detail string) {
	var event EventError
	var closed *websocket.CloseError
	var refused *wsconn.HandshakeError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.As(err, &event):
		words := event.EventCode() + " " + event.EventMessage()
		reason = reasonOf(words)
		return StartStatusErrorEvent, reason, limitOf(reason, words), strings.TrimSpace(words)
	case errors.As(err, &closed):
		reason = reasonOf(closed.Text)
		return closeStatus(closed.Code), reason, limitOf(reason, closed.Text), closed.Text
	case errors.As(err, &refused):
		switch {
		case refused.StatusCode == http.StatusTooManyRequests || isQuotaText(refused.Body):
			reason = StartReasonQuotaExceeded
		case refused.StatusCode == http.StatusUnauthorized || refused.StatusCode == http.StatusForbidden ||
			isAuthText(refused.Body):
			reason = StartReasonAuthRejected
		default:
			reason = StartReasonOther
		}
		return httpStatus(refused.StatusCode), reason, limitOf(reason, refused.Body), refused.Body
	case errors.As(err, &timeout) && timeout.Timeout():
		return StartStatusTimeout, StartReasonOther, "", ""
	default:
		return StartStatusNetwork, StartReasonOther, "", ""
	}
}

// quotaWords are how the vendors here say a quota is spent, matched
// case-insensitively anywhere in an error's code and text. They are vendor
// text, not protocol, and the list is kept short on purpose:
//
//   - qwen closes a session it will not serve with 1007 "Allocated quota
//     exceeded, please increase your quota limit." or "Requests rate limit
//     exceeded, please try again later." (production, 2026-11-19);
//   - DashScope's codes are "Throttling", "Throttling.RateQuota" and
//     "Throttling.AllocationQuota";
//   - OpenAI's error codes are "rate_limit_exceeded" and "insufficient_quota";
//   - an HTTP 429's reason phrase is "Too Many Requests";
//   - doubao refuses a session opened too soon after others with
//     "ExceededConcurrentLimit" (its live tests space sessions a minute apart
//     for it).
var quotaWords = []string{
	"quota", "rate limit", "rate_limit", "ratelimit", "throttl", "too many requests", "concurrentlimit",
}

// authWords are how they say the key was refused: OpenAI's "invalid_api_key",
// DashScope's "InvalidApiKey", and the plain words.
var authWords = []string{"api key", "api_key", "apikey", "unauthorized", "authenticat"}

func isQuotaText(text string) bool { return containsAnyFold(text, quotaWords) }
func isAuthText(text string) bool  { return containsAnyFold(text, authWords) }

// tokenLimitWords and requestLimitWords split a quota refusal by the limit it
// spoke for (#652), matched case-insensitively anywhere in the code and text
// like quotaWords. An entry is here only where a vendor document or a
// production log says which limit the words speak for:
//
//   - TOKENS, the tokens spent per period:
//     "allocated quota exceeded, please increase your quota limit" (qwen's
//     close 1007 in production, issue #604, 2026-11-19, and the message of
//     DashScope's Throttling.AllocationQuota) and "throttling.allocationquota",
//     whose cause DashScope gives as "Token consumption per second or minute
//     (TPS/TPM) triggered rate limiting"
//     (https://www.alibabacloud.com/help/en/model-studio/error-code);
//     "tokens per min", the unit OpenAI's rate-limit message names when its
//     tokens-per-minute limit refused ("Rate limit reached for requests or
//     tokens", https://help.openai.com/en/articles/5955604-how-can-i-solve-429-too-many-requests-errors).
//   - REQUESTS, the requests (here, session starts) per period:
//     "requests rate limit exceeded" (qwen's close 1007 in production, issue
//     #604, the 4th start in a second at 3 per 1 s, and the message of
//     DashScope's Throttling.RateQuota), "throttling.ratequota" and
//     "exceeded your request limit" (that code's cause and its other message:
//     "Call frequency (RPS/RPM) triggered rate limiting", same page);
//     "requests per min" (OpenAI: "Rate limit reached for ... on requests
//     per min.", https://developers.openai.com/cookbook/examples/how_to_handle_rate_limits).
//
// Only the per-minute units are named: a per-day limit does not clear in a
// period this platform knows. Everything else that is a quota refusal is
// UNKNOWN, among them, because a document says they are used for more than
// one limit or for neither window:
//
//   - a 429 with no words, and "too many requests", "rate limit",
//     "rate_limit_exceeded" without a unit: OpenAI and DashScope answer 429
//     for their request and their token limits alike (the pages above);
//   - DashScope's bare "Throttling" ("Requests throttling triggered", cause
//     "API calls triggered rate limiting") and Throttling.BurstRate (a spike
//     guard, not a window of the quota);
//   - "insufficient_quota": OpenAI's billing type, a spent credit or spend
//     cap no window clears (https://developers.openai.com/docs/guides/error-codes),
//     and DashScope's alias of Throttling.AllocationQuota, whose second
//     message is OpenAI's billing sentence word for word;
//   - DashScope's other "allocated quota exceeded" texts, request counts per
//     hours, weeks or months and a concurrency bound (same page);
//   - doubao's "ExceededConcurrentLimit": its documented limits are 60
//     session starts and 100,000 tokens a minute per application id
//     (docs/design/doubao-findings.md section 9) and no document names the
//     code; its live tests only saw it refuse a session opened soon after
//     another one closed.
//
// A text naming both limits is UNKNOWN too.
var tokenLimitWords = []string{
	"allocated quota exceeded, please increase your quota limit",
	"throttling.allocationquota",
	"tokens per min",
}
var requestLimitWords = []string{
	"requests rate limit exceeded",
	"throttling.ratequota",
	"exceeded your request limit",
	"requests per min",
}

// limitOf is which limit a quota refusal spoke for: LimitRequests,
// LimitTokens, or LimitUnknown when the words settle neither; empty when the
// failure is not a quota refusal at all.
func limitOf(reason StartReason, text string) ratemeter.Limit {
	if reason != StartReasonQuotaExceeded {
		return ""
	}
	isTokens, isRequests := containsAnyFold(text, tokenLimitWords), containsAnyFold(text, requestLimitWords)
	switch {
	case isTokens && !isRequests:
		return ratemeter.LimitTokens
	case isRequests && !isTokens:
		return ratemeter.LimitRequests
	default:
		return ratemeter.LimitUnknown
	}
}

func containsAnyFold(text string, words []string) bool {
	lower := strings.ToLower(text)
	for _, word := range words {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

func reasonOf(text string) StartReason {
	switch {
	case isQuotaText(text):
		return StartReasonQuotaExceeded
	case isAuthText(text):
		return StartReasonAuthRejected
	default:
		return StartReasonOther
	}
}

func httpStatus(code int) string {
	if code < 100 || code > 599 {
		return StartStatusHTTPOther
	}
	return strconv.Itoa(code)
}

func closeStatus(code int) string {
	if code < 1000 || code > 4999 {
		return StartStatusCloseOther
	}
	return "CLOSE_" + strconv.Itoa(code)
}

// QuotaMeter is the process-wide account of the provider's quota that campaign
// admission reads (internal/provider/ratemeter): a session tells it what its
// turns cost and when the provider said the quota was spent, naming the limit
// that refused (#652). Session starts are counted where a session is made
// (cmd/aicc), before the dial, as the start counter is.
type QuotaMeter interface {
	TokensUsed(n int)
	QuotaRefused(limit ratemeter.Limit)
}

// Option configures a session client beyond its profile.
type Option func(*Options)

// Options are what a client is built with beyond its profile. Exported so that
// a client in a sub-package reads the same ones.
type Options struct {
	// Meter is told what the session spends of the provider's quota. Nil:
	// nobody is (tests, aicc doctor).
	Meter QuotaMeter
}

// WithMeter has the session report its quota to meter.
func WithMeter(meter QuotaMeter) Option { return func(o *Options) { o.Meter = meter } }

// ApplyOptions folds a client's options into one value.
func ApplyOptions(opts []Option) Options {
	var o Options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// The kinds of rate-limit header a provider may answer its upgrade with.
const (
	// RateLimitRequests and RateLimitTokens are the OpenAI-style
	// x-ratelimit-{limit,remaining,reset}-{requests,tokens}.
	RateLimitRequests = "REQUESTS"
	RateLimitTokens   = "TOKENS"
	// RateLimitUnspecified is the unqualified X-RateLimit-Limit/-Remaining/
	// -Reset, whose unit is the vendor's to say.
	RateLimitUnspecified = "UNSPECIFIED"
)

// RateLimit is one rate-limit header family, as the provider wrote it.
type RateLimit struct {
	Kind                    string
	Limit, Remaining, Reset string
}

// String is the family as one log value: "TOKENS limit=100000 remaining=99000 reset=6m0s".
func (l RateLimit) String() string {
	parts := []string{l.Kind}
	for _, kv := range [][2]string{{"limit", l.Limit}, {"remaining", l.Remaining}, {"reset", l.Reset}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	return strings.Join(parts, " ")
}

// RateLimits reads the rate-limit headers of an upgrade response, in a fixed
// order of kinds, skipping the kinds it does not carry. Header names are
// matched case-insensitively (http.Header canonicalises them). None of the
// vendors here was seen sending one; a gateway or another plan may.
func RateLimits(header http.Header) []RateLimit {
	if header == nil {
		return nil
	}
	var out []RateLimit
	for _, family := range []struct{ kind, suffix string }{
		{RateLimitRequests, "-Requests"},
		{RateLimitTokens, "-Tokens"},
		{RateLimitUnspecified, ""},
	} {
		limit := RateLimit{
			Kind:      family.kind,
			Limit:     header.Get("X-Ratelimit-Limit" + family.suffix),
			Remaining: header.Get("X-Ratelimit-Remaining" + family.suffix),
			Reset:     header.Get("X-Ratelimit-Reset" + family.suffix),
		}
		if limit.Limit != "" || limit.Remaining != "" || limit.Reset != "" {
			out = append(out, limit)
		}
	}
	return out
}

// Handshake is what the provider answered a session's upgrade with that
// matters to its quota: the request id its support asks for and any rate-limit
// headers.
type Handshake struct {
	RequestID  string
	RateLimits []RateLimit
}

// LogAttrs are the rate-limit headers as log attributes, none when there were
// none. The request id is the client's to log, under its own name.
func (h Handshake) LogAttrs() []any {
	if len(h.RateLimits) == 0 {
		return nil
	}
	values := make([]string, 0, len(h.RateLimits))
	for _, limit := range h.RateLimits {
		values = append(values, limit.String())
	}
	return []any{"rateLimits", strings.Join(values, "; ")}
}

// HandshakeReporter is a session that can say what the provider answered its
// upgrade with. Optional, and read by aicc doctor only; both clients here
// implement it.
type HandshakeReporter interface {
	Handshake() Handshake
}

var _ HandshakeReporter = (*Realtime)(nil)

// Quota is one session's side of the provider's quota. A client builds one
// and hands it what it decoded; nothing here reads a protocol event.
type Quota struct {
	provider string
	// requestIDHeader is where this protocol's upgrade answer carries the
	// request id: X-Request-Id on the Realtime vendors, X-Tt-Logid on doubao.
	requestIDHeader string
	meter           QuotaMeter
}

// NewQuota builds a session's quota account. meter may be nil.
func NewQuota(provider, requestIDHeader string, meter QuotaMeter) Quota {
	return Quota{provider: provider, requestIDHeader: requestIDHeader, meter: meter}
}

// Upgraded reads what the provider answered an accepted upgrade with and
// publishes any limit it stated. It returns what it read, for the session's
// one log line and HandshakeReporter.
func (q Quota) Upgraded(header http.Header) Handshake {
	h := Handshake{RateLimits: RateLimits(header)}
	if header != nil {
		h.RequestID = header.Get(q.requestIDHeader)
	}
	for _, limit := range h.RateLimits {
		n, errLimit := strconv.ParseInt(limit.Limit, 10, 64)
		left, errLeft := strconv.ParseInt(limit.Remaining, 10, 64)
		obs.SetProviderRateLimit(q.provider, limit.Kind, n, errLimit == nil, left, errLeft == nil)
	}
	return h
}

// Refused turns a session start that failed into a StartFailure: classified,
// counted (aicc_provider_dial_failures_total), logged with the request id, and
// reported to the meter when the provider said its quota is spent. upgrade is
// the accepted upgrade's headers, nil when the dial itself failed (a refused
// upgrade carries its own).
//
// A start the call gave up on is not the provider's failure: with ctx done the
// error is returned as it is and nothing is counted.
func (q Quota) Refused(ctx context.Context, log *slog.Logger, upgrade http.Header, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	status, reason, limit, detail := ClassifyStart(err)
	header := upgrade
	var refused *wsconn.HandshakeError
	if errors.As(err, &refused) {
		header = refused.Header
	}
	failure := &StartFailure{
		Provider: q.provider, Status: status, Reason: reason, Limit: limit, Detail: detail, Err: err,
	}
	if header != nil {
		failure.RequestID = header.Get(q.requestIDHeader)
	}
	obs.RecordProviderDialFailure(q.provider, status, string(reason), string(limit))
	attrs := []any{"status", status, "reason", string(reason), "limit", string(limit),
		"requestId", failure.RequestID, "detail", detail, "error", err}
	attrs = append(attrs, Handshake{RateLimits: RateLimits(header)}.LogAttrs()...)
	if reason == StartReasonQuotaExceeded {
		if q.meter != nil {
			q.meter.QuotaRefused(limit)
		}
		log.Warn("provider refused the session: its quota is spent; campaign admission holds for the refused limit's period", attrs...)
	} else {
		log.Warn("provider session could not be started", attrs...)
	}
	return failure
}

// RefusedMidCall is an error event on a session that had started. One that
// says the quota is spent (a turn refused for a spent token quota) is reported
// to the meter as a refusal, so admission backs off for it as for a refused
// start; it is not a dial failure and is not counted as one. It reports
// whether it was a quota refusal.
func (q Quota) RefusedMidCall(log *slog.Logger, event EventError) bool {
	words := event.EventCode() + " " + event.EventMessage()
	if !isQuotaText(words) {
		return false
	}
	limit := limitOf(StartReasonQuotaExceeded, words)
	if q.meter != nil {
		q.meter.QuotaRefused(limit)
	}
	log.Warn("provider said its quota is spent during a call; campaign admission holds for the refused limit's period",
		"limit", string(limit), "code", event.EventCode(), "message", event.EventMessage())
	return true
}

// Spent counts what one response cost, as the provider reported it: the tokens
// in and out, and the part of a total the vendor did not split between them.
// A negative value is the vendor's error and counts as zero, and a response
// that reported nothing counts nothing. Once per response, never per frame.
func (q Quota) Spent(input, output, total int) {
	input, output, total = max(input, 0), max(output, 0), max(total, 0)
	unsplit := max(total-input-output, 0)
	if input+output+unsplit == 0 {
		return
	}
	obs.RecordProviderTokens(q.provider, int64(input), int64(output), int64(unsplit))
	if q.meter != nil {
		q.meter.TokensUsed(input + output + unsplit)
	}
}
