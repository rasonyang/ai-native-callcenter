// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// stubKeys is the key store the authentication middleware talks to. It is a
// stand-in for what commit ④ installs; the seam is what these tests exercise,
// because the rules under test — a bearer is answered on its own terms, a key
// never falls through to the session path, a key cannot reach an operation the
// contract gives it no alternative on — are the middleware's and not the
// store's.
type stubKeys struct {
	secret  string
	subject KeySubject
}

func (k stubKeys) AuthenticateKey(_ context.Context, presented string) (KeySubject, error) {
	if k.secret == "" || presented != k.secret {
		return KeySubject{}, errors.New("no such key")
	}
	return k.subject, nil
}

// aKey is a key holding exactly the scopes named, under a name a reader of the
// audit trail can recognise.
func aKey(secret string, scopes ...string) stubKeys {
	return stubKeys{secret: secret, subject: KeySubject{
		KeyID: uuid.New(), Name: "crm-integration", Scopes: scopes,
	}}
}

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

func keyedServer(t *testing.T, keys KeyAuthenticator) (http.Handler, *keyedDialer) {
	t.Helper()
	dialer := &keyedDialer{}
	srv := New(config.Config{SessionCookie: "aicc_session"}, Deps{
		Outbound: dialer,
		Agents:   dialerPresence{},
		AgentDir: dialerDirectory{},
		Keys:     keys,
	})
	return srv.router(), dialer
}

func machineRequest(key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(
		`{"kind":"AGENT_OUTBOUND","to":"13912345678","extensionNumber":"1009"}`))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
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
	router, dialer := keyedServer(t, aKey("s3cret", api.ScopeCallsCreate))
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
	router, dialer := keyedServer(t, aKey("s3cret", api.ScopeCallsCreate))
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

// A deployment that has issued no keys — or has no key store at all — answers
// every bearer the same 401. Nothing about "no keys exist" may read as "any
// key will do".
func TestAnUnconfiguredKeyOpensNothing(t *testing.T) {
	router, dialer := keyedServer(t, nil)
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
	router, dialer := keyedServer(t, aKey("s3cret", api.ScopeCallsCreate))

	// No credential at all: authentication refuses before the CSRF rule is
	// ever reached, because the CSRF header protects a session and there is
	// none to protect.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, machineRequest(""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("http = %d: %s — with no cookie there is no session", w.Code, w.Body)
	}
	if code := errorCodeOf(t, w); code != string(CodeSessionExpired) {
		t.Errorf("code = %q, want SESSION_EXPIRED — for a browser, the session is the problem", code)
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
	srv := New(config.Config{SessionCookie: "aicc_session"}, Deps{
		Outbound: dialer,
		Agents:   dialerPresence{},
		AgentDir: dialerDirectory{},
		Auditor:  recorder,
		Keys:     aKey("s3cret", api.ScopeCallsCreate),
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
	if !strings.Contains(string(detail), "crm-integration") {
		t.Errorf("detail = %s, want it to name the key — otherwise the row is "+
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

// endingCalls records what was ended and how.
type endingCalls struct {
	stubCalls
	endedCall uuid.UUID
	agentLeg  uuid.UUID
}

func (c *endingCalls) EndCall(_ context.Context, callID uuid.UUID) error {
	c.endedCall = callID
	return nil
}

func (c *endingCalls) Hangup(_ context.Context, callID, _ uuid.UUID) error {
	c.agentLeg = callID
	return nil
}

// hangupAs sends a hangup with whatever credential the caller supplies, through
// the real routing table so the route's own middleware is under test too.
func hangupAs(t *testing.T, callID uuid.UUID, dir AgentDirectory,
	decorate func(*http.Request)) (*httptest.ResponseRecorder, *endingCalls) {
	t.Helper()
	calls := &endingCalls{}
	srv := New(config.Config{SessionCookie: "aicc_session"}, Deps{
		Calls:    calls,
		Agents:   dialerPresence{},
		AgentDir: dir,
		Outbound: &keyedDialer{},
		Keys:     aKey("s3cret", api.ScopeCallsControl),
	})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String()+"/hangup", nil)
	decorate(r)
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)
	return w, calls
}

// noAgents is a directory where nobody has an agent profile, which is what a
// supervisor account and the API key both look like.
type noAgents struct{}

func (noAgents) AgentIDForUser(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, errors.New("not an agent")
}
func (noAgents) QueuesForAgent(*http.Request, uuid.UUID) ([]uuid.UUID, error) { return nil, nil }

// A system that dials must be able to stop what it started. Nothing else can:
// the call it placed carries no agent id, so every agent is NOT_CALL_PARTY on
// it, and before this the only way to end one was fs_cli (measured live
// 2026-08-26 while verifying third-party dialling).
func TestASystemCanEndTheCallItPlaced(t *testing.T) {
	callID := uuid.New()
	w, calls := hangupAs(t, callID, noAgents{}, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer s3cret")
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if calls.endedCall != callID {
		t.Errorf("ended %v, want the call named in the path", calls.endedCall)
	}
	if calls.agentLeg != uuid.Nil {
		t.Error("an agent's leg was hung up on behalf of a caller who has none")
	}
}

// The same rule reaches the supervisor, whose refusal was the same defect wearing
// different clothes: hangup asked for an agent profile, and supervision is not
// staffed on the floor.
func TestASupervisorCanEndACallTheyAreNotOn(t *testing.T) {
	callID := uuid.New()
	calls := &endingCalls{}
	srv := &Server{calls: calls, agents: dialerPresence{}, agentDir: noAgents{}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String()+"/hangup", nil)
	r = r.WithContext(contextWithAuth(r.Context(), AuthContext{
		Kind: SubjectUser, SubjectID: uuid.New(),
		scopes: grantedScopes(auth.RoleSupervisor),
	}))
	w := httptest.NewRecorder()
	srv.HangupCall(w, r, callID)

	if w.Code != http.StatusAccepted {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if calls.endedCall != callID {
		t.Errorf("ended %v, want the call named in the path", calls.endedCall)
	}
	if calls.agentLeg != uuid.Nil {
		t.Error("an agent's leg was hung up on behalf of a caller who has none")
	}
}

// An agent still leaves their own leg rather than ending the conversation. They
// have one to leave, and a caller handed back to a queue is still on a call —
// widening this to end the call would drop the customer every time an agent
// stepped out.
func TestAnAgentStillEndsOnlyTheirOwnLeg(t *testing.T) {
	callID := uuid.New()
	calls := &endingCalls{}
	srv := &Server{calls: calls, agents: dialerPresence{}, agentDir: dialerDirectory{}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String()+"/hangup", nil)
	r = r.WithContext(contextWithAuth(r.Context(), AuthContext{
		Kind: SubjectUser, SubjectID: uuid.New(), AgentID: uuid.New(),
		scopes: grantedScopes(auth.RoleAgent),
	}))
	w := httptest.NewRecorder()
	srv.HangupCall(w, r, callID)

	if w.Code != http.StatusAccepted {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if calls.agentLeg != callID {
		t.Errorf("hung up %v, want the agent's own leg on this call", calls.agentLeg)
	}
	if calls.endedCall != uuid.Nil {
		t.Error("an agent stepping out ended the whole conversation")
	}
}

// A machine is never told to refresh a session it cannot have.
//
// An API key reaching a session-only endpoint used to be answered
// SESSION_EXPIRED, which to an integration reads as "log in again" — and it
// has nothing to log in with, so it retries that forever. The boundary itself
// is the point of the 401 and does not move: a key that can place calls must
// not reach the configuration saying where call records are sent.
func TestAKeyIsNotToldToRefreshASessionItCannotHave(t *testing.T) {
	// Logging out is the operation the contract gives no bearer alternative:
	// a key has no session to end. It is the standing example of "this
	// credential does not reach this operation", which is a different answer
	// from "you lack a scope".
	srv := New(config.Config{SessionCookie: "aicc_session"}, Deps{
		Keys: aKey("the-key", api.AllScopes...),
	})

	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r.Header.Set("Authorization", "Bearer the-key")
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body)
	}
	if code := errorCodeOf(t, w); code != string(CodeInvalidCredentials) {
		t.Errorf("code = %q, want INVALID_CREDENTIALS — told SESSION_EXPIRED an "+
			"integration goes and refreshes a session it never had", code)
	}
	// Holding every scope in the vocabulary changes nothing: this is not a
	// missing capability, it is the wrong kind of credential.
	if strings.Contains(w.Body.String(), string(CodeInsufficientScope)) {
		t.Errorf("body = %s, want it not to blame a scope", w.Body)
	}

	// A browser with no cookie is still told its session is the problem,
	// because for a browser it is.
	plain := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	plain.Header.Set(csrfHeader, "1")
	w = httptest.NewRecorder()
	srv.router().ServeHTTP(w, plain)
	if code := errorCodeOf(t, w); code != string(CodeSessionExpired) {
		t.Errorf("code = %q, want SESSION_EXPIRED for a browser", code)
	}
}
