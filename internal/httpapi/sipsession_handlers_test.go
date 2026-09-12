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
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/sipsession"
)

// fakeSIPSessions records what it was asked for and answers what a test set up.
type fakeSIPSessions struct {
	issued    sipsession.Issued
	issueErr  error
	revokeErr error

	issuedFor  []uuid.UUID
	issuedTill []time.Time
	revokedFor []uuid.UUID
}

func (f *fakeSIPSessions) Issue(_ context.Context, agentID uuid.UUID, expiresAt time.Time) (sipsession.Issued, error) {
	f.issuedFor = append(f.issuedFor, agentID)
	f.issuedTill = append(f.issuedTill, expiresAt)
	if f.issueErr != nil {
		return sipsession.Issued{}, f.issueErr
	}
	out := f.issued
	out.ExpiresAt = expiresAt
	return out, nil
}

func (f *fakeSIPSessions) Revoke(_ context.Context, agentID uuid.UUID) error {
	f.revokedFor = append(f.revokedFor, agentID)
	return f.revokeErr
}

func phoneCredential() sipsession.Issued {
	return sipsession.Issued{
		SIPDomain: "aicc.test",
		WSSURL:    "wss://aicc.test:7443/",
		Account:   "1001",
		A1Hash:    "0123456789abcdef0123456789abcdef",
	}
}

// createSession runs the handler as whoever the AuthContext says.
func createSession(t *testing.T, ac AuthContext, phones SIPSessionService) (*httptest.ResponseRecorder, *Server) {
	t.Helper()
	s := &Server{cfg: config.Config{SessionTTL: 12 * time.Hour}, sipSessions: phones}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agent/sip-session", nil)
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.CreateAgentSIPSession(w, r)
	return w, s
}

// agentWithSession is a signed-in agent whose browser session ends at a known
// moment, which is what the phone's credential has to follow.
func agentWithSession(expiresAt time.Time) AuthContext {
	return AuthContext{
		Kind: SubjectUser, SubjectID: uuid.New(), SubjectName: "mina",
		AgentID: uuid.New(), SessionExpiresAt: expiresAt,
		scopes: grantedScopes(auth.RoleAgent),
	}
}

// The five fields are what a phone needs to register, and nothing else is in
// the answer — no password, under that name or any other.
func TestIssuingASessionAnswersEverythingThePhoneNeedsAndNoPassword(t *testing.T) {
	expires := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	phones := &fakeSIPSessions{issued: phoneCredential()}
	ac := agentWithSession(expires)

	w, _ := createSession(t, ac, phones)
	if w.Code != http.StatusOK {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"sipDomain": "aicc.test",
		"wssUrl":    "wss://aicc.test:7443/",
		"account":   "1001",
		"a1Hash":    "0123456789abcdef0123456789abcdef",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
	if _, ok := got["expiresAt"]; !ok {
		t.Error("the phone was not told when its credential stops working")
	}
	if len(got) != len(want)+1 {
		t.Errorf("the answer carries %v, want exactly the five contract fields", keysOf(got))
	}
	// Named explicitly, because the whole design is that this key can never
	// exist: the password is generated, hashed and dropped.
	for _, forbidden := range []string{"password", "secret", "sipPassword"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("the answer carries a %q field", forbidden)
		}
	}
}

// The phone is signed in for exactly as long as the person is.
func TestAPhonesCredentialExpiresWithTheBrowserSession(t *testing.T) {
	expires := time.Now().Add(37 * time.Minute).UTC().Truncate(time.Second)
	phones := &fakeSIPSessions{issued: phoneCredential()}

	w, _ := createSession(t, agentWithSession(expires), phones)
	if w.Code != http.StatusOK {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if len(phones.issuedTill) != 1 || !phones.issuedTill[0].Equal(expires) {
		t.Fatalf("minted until %v, want the session's own expiry %v", phones.issuedTill, expires)
	}

	var got struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Errorf("expiresAt = %v, want %v", got.ExpiresAt, expires)
	}
}

// A key has no session to follow, so its credential gets the deployment's
// session lifetime from now — the same window a person would have had, rather
// than a credential with no end.
func TestAKeyGetsTheDeploymentsSessionLifetime(t *testing.T) {
	phones := &fakeSIPSessions{issued: phoneCredential()}
	ac := AuthContext{
		Kind: SubjectKey, SubjectID: uuid.New(), SubjectName: "crm",
		AgentID: uuid.New(), scopes: grantedScopes(auth.RoleAgent),
	}

	before := time.Now()
	w, _ := createSession(t, ac, phones)
	if w.Code != http.StatusOK {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	got := phones.issuedTill[0]
	if got.Before(before.Add(11*time.Hour)) || got.After(time.Now().Add(13*time.Hour)) {
		t.Errorf("minted until %v, want about 12h from now", got)
	}
}

// An agent with no phone bound is a conflict, not a validation failure: the
// request was well formed and there is nothing to put in it that would help.
func TestAnAgentWithNoPhoneIsToldItIsAConflict(t *testing.T) {
	phones := &fakeSIPSessions{issueErr: sipsession.ErrNoExtensionBound}

	w, _ := createSession(t, agentWithSession(time.Now().Add(time.Hour)), phones)
	if w.Code != http.StatusConflict {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if code := errorCodeOf(t, w); code != string(CodeConflict) {
		t.Errorf("code = %s, want %s", code, CodeConflict)
	}
}

func TestAStorageFailureIsReportedAsStorageDown(t *testing.T) {
	phones := &fakeSIPSessions{issueErr: errors.New("connection refused")}

	w, _ := createSession(t, agentWithSession(time.Now().Add(time.Hour)), phones)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if code := errorCodeOf(t, w); code != string(CodeStorageDown) {
		t.Errorf("code = %s, want %s", code, CodeStorageDown)
	}
	// Whatever went wrong, the answer says nothing a caller could mistake for
	// a credential.
	if len(phones.issuedFor) != 1 {
		t.Errorf("the service was asked %d times", len(phones.issuedFor))
	}
}

// A supervisor is a subject with no agent identity. There is no phone that is
// theirs to sign in, and the refusal says which fact is missing.
func TestASupervisorWithNoAgentIdentityIsRefused(t *testing.T) {
	phones := &fakeSIPSessions{issued: phoneCredential()}
	ac := AuthContext{
		Kind: SubjectUser, SubjectID: uuid.New(), SubjectName: "priya",
		SessionExpiresAt: time.Now().Add(time.Hour),
		scopes:           grantedScopes(auth.RoleSupervisor),
	}

	w, _ := createSession(t, ac, phones)
	if w.Code != http.StatusForbidden {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if code := errorCodeOf(t, w); code != string(CodeAgentRequired) {
		t.Errorf("code = %s, want %s", code, CodeAgentRequired)
	}
	if len(phones.issuedFor) != 0 {
		t.Error("a credential was minted for a subject with no agent identity")
	}
}

func TestRevokingASessionAnswers204(t *testing.T) {
	phones := &fakeSIPSessions{}
	ac := agentWithSession(time.Now().Add(time.Hour))
	s := &Server{cfg: config.Config{SessionTTL: time.Hour}, sipSessions: phones}

	r := httptest.NewRequest(http.MethodDelete, "/api/v1/agent/sip-session", nil)
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.DeleteAgentSIPSession(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a 204 carried a body: %s", w.Body)
	}
	if len(phones.revokedFor) != 1 || phones.revokedFor[0] != ac.AgentID {
		t.Errorf("revoked %v, want the caller's own agent identity", phones.revokedFor)
	}

	// Idempotent: asking again is asking for a state that already holds.
	w2 := httptest.NewRecorder()
	s.DeleteAgentSIPSession(w2, r)
	if w2.Code != http.StatusNoContent {
		t.Errorf("a second revocation = %d, want 204", w2.Code)
	}
}

// Signing out of the browser signs the phone out too. A closed tab that keeps
// its registration keeps being rung, which is the whole failure this feature
// exists to end.
func TestSigningOutRevokesThePhoneToo(t *testing.T) {
	phones := &fakeSIPSessions{}
	ac := agentWithSession(time.Now().Add(time.Hour))
	s := &Server{cfg: config.Config{SessionCookie: "aicc_session"}, sipSessions: phones}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.Logout(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if len(phones.revokedFor) != 1 || phones.revokedFor[0] != ac.AgentID {
		t.Errorf("logout revoked %v, want the signed-in agent's phone", phones.revokedFor)
	}
}

// A phone credential that could not be revoked must not keep somebody signed
// in: the browser session ends either way.
func TestSigningOutSucceedsEvenIfThePhoneCannotBeRevoked(t *testing.T) {
	phones := &fakeSIPSessions{revokeErr: errors.New("storage down")}
	ac := agentWithSession(time.Now().Add(time.Hour))
	s := &Server{cfg: config.Config{SessionCookie: "aicc_session"}, sipSessions: phones}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.Logout(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
}

// /auth/me reports the phone the switch actually holds, not what was issued.
func TestGetMeReportsTheDeviceTheSwitchHolds(t *testing.T) {
	ac := agentWithSession(time.Now().Add(time.Hour))

	t.Run("registered", func(t *testing.T) {
		s := &Server{agents: registeredAgentPhone{}}
		got := getMe(t, s, ac)
		if !got.IsDeviceRegistered {
			t.Error("isDeviceRegistered = false for a phone the switch holds")
		}
		if got.DeviceAccount == nil || *got.DeviceAccount != "1001" {
			t.Errorf("deviceAccount = %v, want the bound extension", got.DeviceAccount)
		}
	})

	t.Run("not registered", func(t *testing.T) {
		s := &Server{agents: stubAgents{}}
		got := getMe(t, s, ac)
		if got.IsDeviceRegistered {
			t.Error("isDeviceRegistered = true for a phone the switch has never seen")
		}
		if got.DeviceAccount != nil {
			t.Errorf("deviceAccount = %v, want nothing rather than an invented number", *got.DeviceAccount)
		}
	})

	t.Run("not an agent", func(t *testing.T) {
		s := &Server{agents: registeredAgentPhone{}}
		supervisor := AuthContext{
			Kind: SubjectUser, SubjectID: uuid.New(), SubjectName: "priya",
			scopes: grantedScopes(auth.RoleSupervisor),
		}
		got := getMe(t, s, supervisor)
		if got.IsDeviceRegistered || got.DeviceAccount != nil {
			t.Error("a subject with no agent identity was given somebody's phone")
		}
	})
}

type meAnswer struct {
	IsDeviceRegistered bool    `json:"isDeviceRegistered"`
	DeviceAccount      *string `json:"deviceAccount"`
}

func getMe(t *testing.T, s *Server, ac AuthContext) meAnswer {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.GetMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	var got meAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// registeredAgentPhone is an agent whose phone the switch holds a registration
// for, at the extension bound to them.
type registeredAgentPhone struct{ stubAgents }

func (registeredAgentPhone) DeviceState(uuid.UUID) (bool, bool) { return true, true }
func (registeredAgentPhone) BoundExtensionFor(context.Context, uuid.UUID) string {
	return "1001"
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The audit trail captures request bodies and never responses, which is why an
// a1-hash cannot reach an audit row today: it exists only in the response to
// POST /agent/sip-session, and that operation has no request body at all.
// Redaction names it anyway, so the rule survives somebody capturing responses.
func TestADigestIsTreatedAsACredentialByTheAuditTrail(t *testing.T) {
	for _, field := range []string{"a1Hash", "a1hash", "A1HASH"} {
		if !isSecretField(field) {
			t.Errorf("%q is not redacted from an audit row", field)
		}
	}
	redacted, ok := redactSecrets([]byte(`{"a1Hash":"0123456789abcdef0123456789abcdef","account":"1001"}`))
	if !ok {
		t.Fatal("a JSON object was not redactable")
	}
	if strings.Contains(redacted, "0123456789abcdef") {
		t.Errorf("the digest survived redaction: %s", redacted)
	}
	if !strings.Contains(redacted, `"account":"1001"`) {
		t.Errorf("redaction ate a field that is not a credential: %s", redacted)
	}
}
