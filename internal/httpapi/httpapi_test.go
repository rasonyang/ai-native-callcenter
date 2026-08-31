// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

func TestRoleAtLeast(t *testing.T) {
	tests := []struct {
		name string
		have auth.Role
		want auth.Role
		ok   bool
	}{
		{"agent meets agent", auth.RoleAgent, auth.RoleAgent, true},
		{"agent below supervisor", auth.RoleAgent, auth.RoleSupervisor, false},
		{"agent below admin", auth.RoleAgent, auth.RoleAdmin, false},
		{"supervisor meets agent", auth.RoleSupervisor, auth.RoleAgent, true},
		{"supervisor below admin", auth.RoleSupervisor, auth.RoleAdmin, false},
		{"admin meets everything", auth.RoleAdmin, auth.RoleSupervisor, true},
		{"unknown role meets nothing", auth.Role("GUEST"), auth.RoleAgent, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.have.AtLeast(tt.want); got != tt.ok {
				t.Errorf("%s.AtLeast(%s) = %v, want %v", tt.have, tt.want, got, tt.ok)
			}
		})
	}
}

// contextWithIdentity builds the AuthContext the authentication middleware
// would have built for a signed-in account, so a handler test exercises the
// handler rather than the door.
//
// The optional agent id is what the middleware resolves from the account's
// agent binding: pass one where the account takes calls, and leave it out
// where it does not — an administrator, or a supervisor who is not staffed.
// It is a parameter rather than a lookup because these tests call handlers
// directly and never pass through authentication.
func contextWithIdentity(ctx context.Context, id auth.Identity, agentID ...uuid.UUID) context.Context {
	ac := AuthContext{
		Kind:        SubjectUser,
		SubjectID:   id.UserID,
		SubjectName: id.Username,
		User:        id,
		scopes:      grantedScopes(id.Role),
	}
	if len(agentID) > 0 {
		ac.AgentID = agentID[0]
	}
	return contextWithAuth(ctx, ac)
}

// The contract decides who may reach an operation, and it is read at the
// route rather than guessed: a route the contract does not declare is refused
// rather than let through, and a subject missing the scope is told which one.
func TestTheContractDecidesWhoReachesAnOperation(t *testing.T) {
	reached := false
	handler := (&Server{}).enforceContract(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))

	tests := []struct {
		name    string
		pattern string
		method  string
		auth    *AuthContext
		want    int
		code    ErrorCode
	}{
		{
			name: "a route the contract does not declare fails closed",
			// Not in the contract at any method: guessing here is how a hole
			// opens quietly.
			pattern: "/api/v1/not-in-the-contract", method: http.MethodGet,
			auth: &AuthContext{Kind: SubjectUser, scopes: api.AllScopes},
			want: http.StatusInternalServerError, code: CodeInternal,
		},
		{
			name:    "an anonymous operation needs nothing",
			pattern: "/api/v1/auth/login", method: http.MethodPost,
			auth: nil, want: http.StatusOK,
		},
		{
			name:    "a subject without the scope is told which one",
			pattern: "/api/v1/cdrs", method: http.MethodGet,
			auth: &AuthContext{Kind: SubjectUser, scopes: grantedScopes(auth.RoleAgent)},
			want: http.StatusForbidden, code: CodeInsufficientScope,
		},
		{
			name:    "a subject with it passes",
			pattern: "/api/v1/cdrs", method: http.MethodGet,
			auth: &AuthContext{Kind: SubjectUser, scopes: grantedScopes(auth.RoleSupervisor)},
			want: http.StatusOK,
		},
		{
			name:    "a key cannot reach an operation with no bearer alternative",
			pattern: "/api/v1/auth/logout", method: http.MethodPost,
			auth: &AuthContext{Kind: SubjectKey, scopes: api.AllScopes},
			want: http.StatusUnauthorized, code: CodeInvalidCredentials,
		},
		{
			name:    "a mutating session request without the CSRF header is refused",
			pattern: "/api/v1/auth/logout", method: http.MethodPost,
			auth: &AuthContext{Kind: SubjectUser, scopes: api.AllScopes},
			want: http.StatusForbidden, code: CodeForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached = false
			r := httptest.NewRequest(tt.method, tt.pattern, nil)
			rctx := chi.NewRouteContext()
			rctx.RoutePatterns = []string{tt.pattern}
			ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
			if tt.auth != nil {
				ctx = contextWithAuth(ctx, *tt.auth)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r.WithContext(ctx))

			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body)
			}
			if tt.code != "" {
				if code := errorCodeOf(t, w); code != string(tt.code) {
					t.Errorf("code = %q, want %q", code, tt.code)
				}
			}
			if (w.Code == http.StatusOK) != reached {
				t.Errorf("reached the handler = %v on status %d", reached, w.Code)
			}
		})
	}
}

func TestParseLastEventID(t *testing.T) {
	tests := []struct {
		name   string
		header string
		query  string
		want   int64
	}{
		{"absent", "", "", 0},
		{"header wins", "42", "7", 42},
		{"query fallback", "", "7", 7},
		{"garbage", "not-a-number", "", 0},
		{"whitespace tolerated", " 15 ", "", 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := "/api/v1/events"
			if tt.query != "" {
				target += "?lastEventId=" + tt.query
			}
			r := httptest.NewRequest(http.MethodGet, target, nil)
			if tt.header != "" {
				r.Header.Set("Last-Event-ID", tt.header)
			}
			if got := parseLastEventID(r); got != tt.want {
				t.Errorf("parseLastEventID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseTypes(t *testing.T) {
	got := parseTypes("PARTY_RINGING, AGENT_READY ,")
	if len(got) != 2 || got[0] != "PARTY_RINGING" || got[1] != "AGENT_READY" {
		t.Errorf("parseTypes() = %v, want [PARTY_RINGING AGENT_READY]", got)
	}
	if parseTypes("") != nil {
		t.Error("parseTypes(\"\") should be nil")
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		remote string
		want   string
	}{
		{"192.168.1.5:51234", "192.168.1.5"},
		{"[::1]:8080", "::1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		if got := clientIP(r); got != tt.want {
			t.Errorf("clientIP(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}
