// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

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

func (m *fakeMeter) QuotaRefused(limit string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refusals++
}

func (m *fakeMeter) counts() ([]int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.tokens...), m.refusals
}

func meteredSession(t *testing.T, f *fakeDoubao, meter provider.QuotaMeter) *Session {
	t.Helper()
	t.Setenv("DOUBAO_API_KEY", "test-key")
	session, err := newSession(testProfile(f.endpoint()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), provider.WithMeter(meter))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(t.Context()) })
	return session
}

// A refusal on this protocol is a StartFailure like any other, and its request
// id is this protocol's own, the upgrade's X-Tt-Logid.
func TestARefusedSessionIsAStartFailureWithTheLogid(t *testing.T) {
	defer noGoroutinesLeft(t)()

	f := newFakeDoubao(t, func(f *fakeDoubao, message map[string]any) {
		if message["type"] == "session.create" {
			f.send(errorFrame("45000292", "ExceededConcurrentLimit"))
		}
	})
	meter := &fakeMeter{}
	session := meteredSession(t, f, meter)

	err := session.Start(t.Context(), testConfig())
	var failure *provider.StartFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Start = %v, want a StartFailure", err)
	}
	if failure.Status != provider.StartStatusErrorEvent || failure.Reason != provider.StartReasonQuotaExceeded ||
		failure.RequestID != "fake-logid-1" || failure.Provider != "doubao" {
		t.Errorf("failure = %+v", failure)
	}
	if !errors.Is(err, provider.ErrQuotaExceeded) {
		t.Error("the refusal does not match ErrQuotaExceeded")
	}
	if _, refusals := meter.counts(); refusals != 1 {
		t.Errorf("the meter heard %d refusals, want 1", refusals)
	}
}

func TestAnOpenedSessionReportsItsHandshake(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := meteredSession(t, f, nil)
	start(t, session, testConfig())
	if got := session.Handshake(); got.RequestID != "fake-logid-1" || len(got.RateLimits) != 0 {
		t.Errorf("handshake = %+v, want the logid and no rate limits", got)
	}
}

// This protocol reports what a turn cost on its own response.done, seconds
// after the turn, and that is what is counted.
func TestATurnsUsageIsCounted(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	meter := &fakeMeter{}
	session := meteredSession(t, f, meter)
	start(t, session, testConfig())

	f.send(wireResponseDone())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tokens, _ := meter.counts(); len(tokens) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tokens, _ := meter.counts(); fmt.Sprint(tokens) != "[2321]" {
		t.Errorf("the meter heard %v, want [2321]", tokens)
	}
}
