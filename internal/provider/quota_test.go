// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/provider/wsconn"
)

// The refusal qwen answered 1117 production calls with on 2026-11-19, as the
// start error read then. It is the one shape this classification exists for.
const qwenQuotaText = "Allocated quota exceeded, please increase your quota limit."

func TestAStartFailureIsClassifiedByWhatTheProviderSaid(t *testing.T) {
	t.Parallel()
	refused := func(status int, body string) error {
		return fmt.Errorf("x: %w", &wsconn.HandshakeError{
			Endpoint: "wss://p.example", StatusCode: status, Body: body, Err: websocket.ErrBadHandshake})
	}
	closed := func(code int, text string) error {
		return fmt.Errorf("session rejected: %w", &websocket.CloseError{Code: code, Text: text})
	}
	event := func(code, message string) error {
		return fmt.Errorf("session rejected: %w", &wireError{Code: code, Message: message})
	}
	tests := []struct {
		name   string
		err    error
		status string
		reason StartReason
	}{
		{"qwen's quota close", closed(1007, qwenQuotaText), "CLOSE_1007", StartReasonQuotaExceeded},
		{"a close about the key", closed(1008, "Unauthorized"), "CLOSE_1008", StartReasonAuthRejected},
		{"a close about something else", closed(1011, "internal error"), "CLOSE_1011", StartReasonOther},
		{"a close code outside the protocol", closed(5001, ""), StartStatusCloseOther, StartReasonOther},
		{"a 429", refused(http.StatusTooManyRequests, ""), "429", StartReasonQuotaExceeded},
		{"a 403 whose body says throttling", refused(http.StatusForbidden, `{"code":"Throttling.RateQuota"}`), "403", StartReasonQuotaExceeded},
		{"a 401", refused(http.StatusUnauthorized, `{"code":"InvalidApiKey"}`), "401", StartReasonAuthRejected},
		{"a 403", refused(http.StatusForbidden, ""), "403", StartReasonAuthRejected},
		{"a 503", refused(http.StatusServiceUnavailable, "upstream down"), "503", StartReasonOther},
		{"a status outside HTTP", refused(999, ""), StartStatusHTTPOther, StartReasonOther},
		{"openai's rate limit event", event("rate_limit_exceeded", "slow down"), StartStatusErrorEvent, StartReasonQuotaExceeded},
		{"openai's spent credit", event("insufficient_quota", "You exceeded your current quota"), StartStatusErrorEvent, StartReasonQuotaExceeded},
		{"openai's bad key", event("invalid_api_key", "Incorrect API key provided"), StartStatusErrorEvent, StartReasonAuthRejected},
		{"dashscope's throttling code", event("Throttling", "Requests throttling triggered"), StartStatusErrorEvent, StartReasonQuotaExceeded},
		{"an event about the configuration", event("invalid_value", "bad voice"), StartStatusErrorEvent, StartReasonOther},
		{"a setup never confirmed", StartTimedOut("provider did not confirm the session configuration"), StartStatusTimeout, StartReasonOther},
		{"a dial that timed out", fmt.Errorf("dial: %w", context.DeadlineExceeded), StartStatusTimeout, StartReasonOther},
		{"a refused connection", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), StartStatusNetwork, StartReasonOther},
		{"a socket that broke", fmt.Errorf("session rejected: %w", io.ErrUnexpectedEOF), StartStatusNetwork, StartReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, _ := ClassifyStart(tt.err)
			if status != tt.status || reason != tt.reason {
				t.Errorf("ClassifyStart = %s/%s, want %s/%s", status, reason, tt.status, tt.reason)
			}
		})
	}
}

// The matcher is vendor text, so it is pinned word by word: case does not
// matter, and nothing that merely mentions a limit or a key counts.
func TestTheQuotaMatcherIsVendorTextCaseInsensitive(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		qwenQuotaText, "QUOTA", "Rate limit reached for requests", "rate_limit_exceeded",
		"RateLimitExceeded", "Throttling.AllocationQuota", "429 Too Many Requests", "ExceededConcurrentLimit",
	} {
		if !isQuotaText(text) {
			t.Errorf("%q was not read as a spent quota", text)
		}
	}
	for _, text := range []string{"", "invalid payload data", "the session limit of 10 minutes was reached", "rate"} {
		if isQuotaText(text) {
			t.Errorf("%q was read as a spent quota", text)
		}
	}
}

func TestRateLimitHeadersAreReadCaseInsensitivelyAndOnlyWhenPresent(t *testing.T) {
	t.Parallel()
	if got := RateLimits(http.Header{"X-Request-Id": {"r"}}); len(got) != 0 {
		t.Errorf("no rate-limit header read as %v", got)
	}
	header := http.Header{}
	// Set canonicalises, as a received header would be; the names below are
	// the lower-case forms a vendor writes.
	header.Set("x-ratelimit-limit-requests", "5000")
	header.Set("x-ratelimit-remaining-requests", "4999")
	header.Set("x-ratelimit-reset-requests", "12ms")
	header.Set("x-ratelimit-limit-tokens", "100000")
	header.Set("X-RATELIMIT-REMAINING", "7")
	got := RateLimits(header)
	want := []RateLimit{
		{Kind: RateLimitRequests, Limit: "5000", Remaining: "4999", Reset: "12ms"},
		{Kind: RateLimitTokens, Limit: "100000"},
		{Kind: RateLimitUnspecified, Remaining: "7"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("RateLimits = %v, want %v", got, want)
	}
	attrs := Handshake{RateLimits: got}.LogAttrs()
	if len(attrs) != 2 || attrs[1] != "REQUESTS limit=5000 remaining=4999 reset=12ms; TOKENS limit=100000; UNSPECIFIED remaining=7" {
		t.Errorf("log attributes = %v", attrs)
	}
}

// fakeMeter records what a session reported to the process-wide meter.
type fakeMeter struct {
	mu       sync.Mutex
	tokens   []int
	refusals int
}

func (m *fakeMeter) TokensUsed(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = append(m.tokens, n)
}

func (m *fakeMeter) QuotaRefused() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refusals++
}

func (m *fakeMeter) counts() (tokens []int, refusals int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.tokens...), m.refusals
}

// upgradeServer accepts the upgrade with the given headers and hands the
// socket to serve on the server's own goroutine.
func upgradeServer(t *testing.T, header http.Header, serve func(conn *websocket.Conn)) string {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, header)
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func meteredSession(t *testing.T, endpoint string, meter QuotaMeter) *Realtime {
	t.Helper()
	profile := QwenProfile()
	t.Setenv(profile.APIKeyEnv, "test-key")
	profile.Endpoint = endpoint
	session, err := New(profile, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMeter(meter))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session
}

// The production refusal: the upgrade is accepted, the session.update is read,
// and the provider closes with 1007 and its quota text. Start says so as a
// number, keeps the request id, tells the meter once, and its text is the text
// production logged.
func TestQwensQuotaCloseIsAStartFailureTheMeterHears(t *testing.T) {
	endpoint := upgradeServer(t, http.Header{"X-Request-Id": {"req-1007"}}, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInvalidFramePayloadData, qwenQuotaText),
			time.Now().Add(time.Second))
		_, _, _ = conn.ReadMessage()
	})
	meter := &fakeMeter{}
	session := meteredSession(t, endpoint, meter)

	err := session.Start(t.Context(), basicConfig())
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Start = %v, want a quota refusal", err)
	}
	var failure *StartFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Start = %T, want a StartFailure", err)
	}
	if failure.Status != "CLOSE_1007" || failure.Reason != StartReasonQuotaExceeded ||
		failure.RequestID != "req-1007" || failure.Detail != qwenQuotaText || failure.Provider != NameQwen {
		t.Errorf("failure = %+v", failure)
	}
	want := "session rejected: websocket: close 1007 (invalid payload data): " + qwenQuotaText
	if err.Error() != want {
		t.Errorf("text = %q, want the text production logged, %q", err.Error(), want)
	}
	if _, refusals := meter.counts(); refusals != 1 {
		t.Errorf("the meter heard %d refusals, want 1", refusals)
	}
	if got := session.Handshake().RequestID; got != "req-1007" {
		t.Errorf("handshake request id = %q", got)
	}
}

// A refused upgrade carries its own request id, and a 429 is a quota refusal
// whatever the body says.
func TestA429UpgradeIsAQuotaRefusalWithItsRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-429")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	meter := &fakeMeter{}
	session := meteredSession(t, "ws"+strings.TrimPrefix(server.URL, "http"), meter)

	err := session.Start(t.Context(), basicConfig())
	var failure *StartFailure
	if !errors.As(err, &failure) || !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Start = %v, want a quota StartFailure", err)
	}
	if failure.Status != "429" || failure.RequestID != "req-429" {
		t.Errorf("failure = %+v", failure)
	}
	if !strings.Contains(err.Error(), "http 429") {
		t.Errorf("text = %q, want the status in it as before", err)
	}
	if _, refusals := meter.counts(); refusals != 1 {
		t.Errorf("the meter heard %d refusals, want 1", refusals)
	}
}

// A refused key is a failure, counted under its own reason, and is not a quota
// the dialer should back off for.
func TestARefusedKeyIsNotAQuotaRefusal(t *testing.T) {
	endpoint := upgradeServer(t, nil, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{
			"code": "invalid_api_key", "message": "Incorrect API key provided"}})
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(3000, ""),
			time.Now().Add(time.Second))
		_, _, _ = conn.ReadMessage()
	})
	meter := &fakeMeter{}
	session := meteredSession(t, endpoint, meter)

	err := session.Start(t.Context(), basicConfig())
	var failure *StartFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Start = %v, want a StartFailure", err)
	}
	if failure.Status != StartStatusErrorEvent || failure.Reason != StartReasonAuthRejected {
		t.Errorf("failure = %s/%s, want ERROR_EVENT/AUTH_REJECTED", failure.Status, failure.Reason)
	}
	if errors.Is(err, ErrQuotaExceeded) {
		t.Error("a refused key matched ErrQuotaExceeded")
	}
	if _, refusals := meter.counts(); refusals != 0 {
		t.Errorf("the meter heard %d refusals for a refused key", refusals)
	}
}

// A start the call gave up on (the caller hung up during setup) is nobody's
// refusal: the context's error comes back and nothing is counted.
func TestAStartTheCallGaveUpOnIsNotAFailure(t *testing.T) {
	release := make(chan struct{})
	endpoint := upgradeServer(t, nil, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
		<-release
	})
	t.Cleanup(func() { close(release) })
	meter := &fakeMeter{}
	session := meteredSession(t, endpoint, meter)

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- session.Start(ctx, basicConfig()) }()
	cancel()
	err := <-errc
	// Wherever the cancel lands (before the dial, during it, or in the wait
	// for the configuration to be accepted) the error is the call's own.
	var failure *StartFailure
	if err == nil || errors.As(err, &failure) {
		t.Fatalf("Start = %v, want the call's own error and no StartFailure", err)
	}
	if _, refusals := meter.counts(); refusals != 0 {
		t.Errorf("the meter heard %d refusals", refusals)
	}
}

// After the start, an error event that says the quota is spent is still a
// refusal the meter hears, and the session goes on as it always did.
func TestAQuotaErrorMidCallIsHeardByTheMeter(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	meter := &fakeMeter{}
	profile := OpenAIProfile()
	t.Setenv(profile.APIKeyEnv, "test-key")
	profile.Endpoint = f.endpoint()
	session, err := New(profile, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMeter(meter))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	f.send(map[string]any{"type": "error", "error": map[string]any{
		"code": "invalid_value", "message": "bad voice"}})
	awaitEvent(t, session, EventTypeError)
	f.send(map[string]any{"type": "error", "error": map[string]any{
		"code": "rate_limit_exceeded", "message": "Rate limit reached for tokens per min"}})
	if e := awaitEvent(t, session, EventTypeError); e.IsFatal {
		t.Error("a mid-call rate limit was made fatal")
	}
	if _, refusals := meter.counts(); refusals != 1 {
		t.Errorf("the meter heard %d refusals, want 1 (the rate limit, not the bad voice)", refusals)
	}
}

// What the upgrade was answered with is kept for aicc doctor.
func TestAnAcceptedUpgradeKeepsItsRequestIDAndRateLimits(t *testing.T) {
	endpoint := upgradeServer(t, http.Header{
		"X-Request-Id":             {"req-ok"},
		"X-Ratelimit-Limit-Tokens": {"100000"},
	}, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	session := meteredSession(t, endpoint, nil)
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := session.Handshake()
	if got.RequestID != "req-ok" || len(got.RateLimits) != 1 || got.RateLimits[0].Limit != "100000" ||
		got.RateLimits[0].Kind != RateLimitTokens {
		t.Errorf("handshake = %+v", got)
	}
}
