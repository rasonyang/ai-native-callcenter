// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

// csrfHeader must accompany a mutating request made with the session cookie.
// Requiring a custom header makes the request non-simple, so a cross-origin
// caller is stopped by the browser's preflight; SameSite=Lax on the cookie is
// the second layer.
//
// It is asked of the cookie and only of the cookie. That header exists to
// protect a credential the browser attaches by itself; a key is presented
// deliberately on every request and has no cookie to abuse. Which operations
// want it is not decided here — the contract says so operation by operation
// (NeedsCSRF), and this server obeys the contract.
const csrfHeader = "X-AICC-Csrf"

// actAsAgentHeader names the agent a key is working as. A key belongs to a
// system, not to a person, so the agent it acts for is a fact about the
// request rather than about the credential.
//
// A session may never send it. An account is already bound to at most one
// agent identity, so the header could only ever mean "act as somebody else",
// and that is not a capability this product has.
const actAsAgentHeader = "X-AICC-Agent-ID"

// bearerPrefix is the one credential presentation an API key uses. There is
// no X-AICC-Api-Key header any more: a bearer token is what every HTTP client
// already knows how to send, and the contract declares it as http/bearer.
const bearerPrefix = "Bearer "

// KeySubject is an authenticated API key: who it is and what it may do.
// Scopes come from the key's own grant, written down when it was issued —
// a key's capabilities are never derived from a role.
type KeySubject struct {
	KeyID   uuid.UUID
	Name    string
	Scopes  []string
	AgentID uuid.UUID
}

// KeyAuthenticator resolves a presented secret into the key that holds it,
// and records that the key was used. Any error means the credential does not
// authenticate; the caller is told INVALID_CREDENTIALS and nothing more,
// because "revoked" and "never existed" are the same answer to whoever is
// holding a secret they should not have.
//
// Nil until commit ④ installs the store-backed implementation. A deployment
// without one simply has no keys, and says so with the same 401.
type KeyAuthenticator interface {
	AuthenticateKey(ctx context.Context, secret string) (KeySubject, error)
}

// authenticate resolves the request's credential into an AuthContext.
//
// One middleware for both kinds. The old pair — requireSession, and a
// requireSessionOrAPIKey bolted onto the two routes a machine was allowed to
// reach — encoded "which credentials may reach this route" in the routing
// table, where the contract could not see it. It is the contract's answer
// now (enforceContract), and this function's only job is to say who is
// asking.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret, ok := bearerSecret(r); ok {
			ac, done := s.authenticateKey(w, r, secret)
			if done {
				return
			}
			next.ServeHTTP(w, r.WithContext(contextWithAuth(r.Context(), ac)))
			return
		}
		ac, done := s.authenticateSession(w, r)
		if done {
			return
		}
		next.ServeHTTP(w, r.WithContext(contextWithAuth(r.Context(), ac)))
	})
}

// bearerSecret returns the secret presented in the Authorization header.
//
// A presented bearer is answered on its own terms and never falls back to the
// cookie: a caller who sent a key meant to authenticate with it, and letting
// them continue into the session path would answer "no session" to what is
// really a bad credential.
func bearerSecret(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) <= len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(bearerPrefix):]), true
}

// authenticateSession authenticates the browser session cookie. done reports
// that the response has already been written.
func (s *Server) authenticateSession(w http.ResponseWriter, r *http.Request) (ac AuthContext, done bool) {
	cookie, err := r.Cookie(s.cfg.SessionCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return AuthContext{}, true
	}

	id, err := s.auth.Authenticate(r.Context(), cookie.Value)
	switch {
	case errors.Is(err, auth.ErrSessionExpired):
		s.clearSessionCookie(w)
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "session expired", nil)
		return AuthContext{}, true
	case errors.Is(err, auth.ErrUserSuspended):
		writeError(w, http.StatusForbidden, CodeUserSuspended, "user suspended", nil)
		return AuthContext{}, true
	case err != nil:
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot verify session", nil)
		return AuthContext{}, true
	}

	// A person acts as themselves. The header is a key's way of naming the
	// agent it works for; a session sending it is asking to be somebody else,
	// and it is refused loudly rather than ignored. Ignoring it is what the
	// baseline did, and a silently discarded authorization header is the kind
	// of thing that is harmless until the day something reads it.
	if r.Header.Get(actAsAgentHeader) != "" {
		writeError(w, http.StatusForbidden, CodeAgentImpersonationNotAllowed,
			"a signed-in account acts as itself; "+actAsAgentHeader+" belongs to an API key",
			map[string]any{"header": actAsAgentHeader})
		return AuthContext{}, true
	}

	ac = AuthContext{
		Kind:        SubjectUser,
		SubjectID:   id.UserID,
		SubjectName: id.Username,
		User:        id,
		ActorUserID: id.UserID,
		scopes:      grantedScopes(id.Role),
	}
	// The agent identity behind the account, resolved once. Thirteen call
	// sites used to look it up for themselves; a handler now reads a field.
	// An account with no agent keeps uuid.Nil, which is not an error here —
	// only an operation that acts as an agent refuses it, and it says
	// AGENT_REQUIRED when it does.
	if s.agentDir != nil {
		if agentID, err := s.agentDir.AgentIDForUser(r, id.UserID); err == nil {
			ac.AgentID = agentID
		}
	}
	return ac, false
}

// authenticateKey authenticates a bearer API key and resolves the agent it
// is acting for.
func (s *Server) authenticateKey(w http.ResponseWriter, r *http.Request, secret string) (ac AuthContext, done bool) {
	if s.keys == nil || secret == "" {
		writeError(w, http.StatusUnauthorized, CodeInvalidCredentials, "bad API key", nil)
		return AuthContext{}, true
	}
	subject, err := s.keys.AuthenticateKey(r.Context(), secret)
	if err != nil {
		// INVALID_CREDENTIALS rather than SESSION_EXPIRED: the code is what
		// an integration branches on, and told its session expired it would
		// go and refresh one it never had.
		writeError(w, http.StatusUnauthorized, CodeInvalidCredentials, "bad API key", nil)
		return AuthContext{}, true
	}

	ac = AuthContext{
		Kind:        SubjectKey,
		SubjectID:   subject.KeyID,
		SubjectName: subject.Name,
		AgentID:     subject.AgentID,
		scopes:      subject.Scopes,
	}
	if raw := r.Header.Get(actAsAgentHeader); raw != "" {
		agentID, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeValidationFailed,
				actAsAgentHeader+" is not an identifier", map[string]any{"field": actAsAgentHeader})
			return AuthContext{}, true
		}
		ac.AgentID = agentID
	}
	// The person behind the agent the key is working as. Resolved here so the
	// handlers that record who did something read a field rather than each
	// asking the directory in their own way.
	if ac.AgentID != uuid.Nil && s.agentDir != nil {
		if userID, err := s.agentDir.UserIDForAgent(r, ac.AgentID); err == nil {
			ac.ActorUserID = userID
		}
	}
	return ac, false
}

// enforceContract is the authorization gate, and it reads the contract.
//
// It runs inside the generated wrapper, which is the first moment chi has
// resolved the route pattern — so the operation can be looked up by what it
// is rather than by a guard somebody remembered to place beside it. Every
// rule it applies (which credentials reach this operation, which scopes each
// must carry, whether the cookie needs a CSRF header) comes from
// docs/openapi.json, generated into api.OperationSecurityByRoute.
//
// That is the whole point of the change: the contract is the product, so the
// contract is what the server obeys. A route whose authorization lives in
// server.go is a rule the published API cannot state.
func (s *Server) enforceContract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want, ok := api.SecurityForRoute(contractRoute(r))
		if !ok {
			// Fail closed. A route with no contract entry is a routing table
			// that has drifted from the contract, and guessing is how a hole
			// opens quietly. TestEveryRouteIsInTheContract makes this
			// unreachable; this is what happens if it ever is not.
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"this route declares no security in the contract", nil)
			return
		}
		if want.IsAnonymous {
			next.ServeHTTP(w, r)
			return
		}

		ac, ok := authFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
			return
		}

		need := want.SessionScopes
		if ac.Kind == SubjectKey {
			need = want.KeyScopes
		}
		if need == nil {
			// The credential has no alternative on this operation at all.
			// 401, not 403: what is missing is a credential this operation
			// takes, not permission. And it is said in words a machine can
			// act on — "session expired" would send an integration off to
			// refresh a session it never had.
			writeError(w, http.StatusUnauthorized, CodeInvalidCredentials,
				"this operation does not take "+credentialName(ac.Kind), nil)
			return
		}

		if want.NeedsCSRF && ac.Kind == SubjectUser && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, CodeForbidden,
				"missing "+csrfHeader+" header", nil)
			return
		}

		for _, scope := range need {
			if !ac.Has(scope) {
				// INSUFFICIENT_SCOPE, not FORBIDDEN: it names what to fix.
				// A caller told only "forbidden" cannot tell a missing
				// capability from a rule about this particular row.
				writeError(w, http.StatusForbidden, CodeInsufficientScope,
					"this credential does not hold "+scope,
					map[string]any{"requiredScope": scope})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// contractRoute is the contract's key for the route chi matched: the pattern
// with the server's own /api/v1 prefix removed, because the contract's paths
// are relative to it.
func contractRoute(r *http.Request) (method, path string) {
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	return r.Method, strings.TrimPrefix(pattern, apiPrefix)
}

// apiPrefix is where the contract's paths are mounted.
const apiPrefix = "/api/v1"

func credentialName(kind SubjectKind) string {
	if kind == SubjectKey {
		return "an API key"
	}
	return "a browser session"
}

// clientIP extracts the peer address, preferring the proxy header only when
// the deployment is configured to trust one.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// requireAgent resolves the agent identity a request acts as, writing the
// refusal itself when there is none.
//
// This replaced agentIDFor, which asked the database on every call site. The
// answer is in the AuthContext now; what is left is the refusal, and it says
// AGENT_REQUIRED rather than FORBIDDEN — the caller is not being denied a
// capability, they are being told this operation works through an agent
// identity and the credential has none.
func requireAgent(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	ac, ok := authFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return uuid.Nil, false
	}
	if !ac.IsAgent() {
		writeError(w, http.StatusForbidden, CodeAgentRequired,
			"this operation acts through an agent identity and this credential has none", nil)
		return uuid.Nil, false
	}
	return ac.AgentID, true
}

// mustAuth is for the handlers that always run behind authentication. The
// second return is kept so a caller can still refuse rather than panic on a
// context that somehow carries nothing.
func mustAuth(w http.ResponseWriter, r *http.Request) (AuthContext, bool) {
	ac, ok := authFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return AuthContext{}, false
	}
	return ac, true
}
