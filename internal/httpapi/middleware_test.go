// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// keyedDialer answers a click-to-dial and remembers it happened, so a test can
// tell "the request was authenticated and refused later" from "it never got
// past the door".
type keyedDialer struct {
	stubOutbound
	placed bool
}

func (d *keyedDialer) Dial(context.Context, outbound.AgentDialRequest) (uuid.UUID, error) {
	d.placed = true
	return uuid.New(), nil
}

func keyedServer(t *testing.T, key string) (http.Handler, *keyedDialer) {
	t.Helper()
	dialer := &keyedDialer{}
	srv := New(config.Config{APIKey: key, SessionCookie: "aicc_session"}, Deps{
		Outbound: dialer,
		Agents:   dialerPresence{},
		AgentDir: dialerDirectory{},
	})
	return srv.router(), dialer
}

func machineRequest(key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(
		`{"kind":"AGENT_OUTBOUND","to":"13912345678","extensionNumber":"1009"}`))
	if key != "" {
		r.Header.Set(apiKeyHeader, key)
	}
	return r
}

// The requirement the key exists for: a system with no browser, no cookie and
// no CSRF header places a call and is served.
//
// The missing CSRF header is the point rather than an oversight. That header
// makes a cross-origin request non-simple so the browser preflights it, which
// protects a caller whose credential travels automatically; this one sends its
// credential deliberately and has no cookie to abuse. Demanding it anyway
// would be a ritual every integration would satisfy with a constant.
func TestAKeyedRequestNeedsNoSessionAndNoCsrfHeader(t *testing.T) {
	router, dialer := keyedServer(t, "s3cret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, machineRequest("s3cret"))

	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if !dialer.placed {
		t.Error("the call was answered but never placed")
	}
}

// A wrong key is answered as a wrong key and never falls through to the
// session rules. Falling through would answer "no session" to what is really a
// bad credential, and send an integrator looking for a login problem they do
// not have.
func TestAWrongKeyIsRefusedRatherThanTreatedAsNoSession(t *testing.T) {
	router, dialer := keyedServer(t, "s3cret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, machineRequest("wrong"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	// The code is what an integration branches on: told its session expired it
	// would go and refresh one it never had.
	if code := errorCodeOf(t, w); code != string(CodeInvalidCredentials) {
		t.Errorf("code = %q, want INVALID_CREDENTIALS", code)
	}
	if !strings.Contains(w.Body.String(), "bad API key") {
		t.Errorf("message = %s, want it to name the key", w.Body)
	}
	if dialer.placed {
		t.Error("a call went out on a wrong key")
	}
}

// A deployment that never named a key has the machine path closed, and the
// config loader gives an unset variable the empty string — so an empty header
// must not compare equal to an empty setting and let everybody in.
func TestAnUnconfiguredKeyOpensNothing(t *testing.T) {
	router, dialer := keyedServer(t, "")
	for _, presented := range []string{"anything", " "} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, machineRequest(presented))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("key %q returned %d, want 401", presented, w.Code)
		}
	}
	if dialer.placed {
		t.Error("a call went out on a deployment with no key configured")
	}
}

// Presenting no key at all is the browser's request, and it still meets the
// session rules — including the CSRF header, which a browser does have to
// send.
func TestWithoutAKeyTheSessionRulesStillApply(t *testing.T) {
	router, dialer := keyedServer(t, "s3cret")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, machineRequest(""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("http = %d: %s — a mutating request with no CSRF header is refused", w.Code, w.Body)
	}

	r := machineRequest("")
	r.Header.Set(csrfHeader, "1")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("http = %d: %s — and with no cookie there is no session", w.Code, w.Body)
	}

	if dialer.placed {
		t.Error("a call went out unauthenticated")
	}
}

// The key is the deployment's own credential, not a person's, so the audit row
// carries no user id — a made-up one would look like somebody to a reader
// chasing who placed a call. It says what it was instead.
func TestTheKeyIsAuditedAsItselfRatherThanAsAUser(t *testing.T) {
	recorder := &recordingAuditor{}
	dialer := &keyedDialer{}
	srv := New(config.Config{APIKey: "s3cret", SessionCookie: "aicc_session"}, Deps{
		Outbound: dialer,
		Agents:   dialerPresence{},
		AgentDir: dialerDirectory{},
		Auditor:  recorder,
	})
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, machineRequest("s3cret"))
	if w.Code != http.StatusCreated {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}

	if recorder.actorID != nil {
		t.Errorf("actor = %v, want null — no user placed this call", recorder.actorID)
	}
	detail, err := json.Marshal(recorder.detail)
	if err != nil {
		t.Fatalf("encode detail: %v", err)
	}
	if !strings.Contains(string(detail), machineUsername) {
		t.Errorf("detail = %s, want it to name the API key — otherwise the row is "+
			"indistinguishable from one with no actor at all", detail)
	}
}

type recordingAuditor struct {
	actorID *uuid.UUID
	detail  map[string]any
}

func (a *recordingAuditor) Audit(_ context.Context, actorID *uuid.UUID, _, _, _ string,
	detail map[string]any, _ string) error {
	a.actorID, a.detail = actorID, detail
	return nil
}
