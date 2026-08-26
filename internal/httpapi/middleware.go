// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

type contextKey int

const identityKey contextKey = iota

// csrfHeader must accompany every mutating request. Requiring a custom header
// makes the request non-simple, so a cross-origin caller is stopped by the
// browser's preflight; SameSite=Lax on the cookie is the second layer.
const csrfHeader = "X-AICC-Csrf"

// identityFrom returns the authenticated identity carried by ctx.
func identityFrom(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(identityKey).(auth.Identity)
	return id, ok
}

// contextWithIdentity attaches an authenticated identity to ctx.
func contextWithIdentity(ctx context.Context, id auth.Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// requireSession authenticates the session cookie and rejects unauthenticated
// requests. It also enforces the CSRF header on mutating methods.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Header.Get(csrfHeader) == "" {
				writeError(w, http.StatusForbidden, CodeForbidden,
					"missing "+csrfHeader+" header", nil)
				return
			}
		}

		cookie, err := r.Cookie(s.cfg.SessionCookie)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
			return
		}

		id, err := s.auth.Authenticate(r.Context(), cookie.Value)
		switch {
		case errors.Is(err, auth.ErrSessionExpired):
			s.clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, CodeSessionExpired, "session expired", nil)
			return
		case errors.Is(err, auth.ErrUserSuspended):
			writeError(w, http.StatusForbidden, CodeUserSuspended, "user suspended", nil)
			return
		case err != nil:
			writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot verify session", nil)
			return
		}

		next.ServeHTTP(w, r.WithContext(contextWithIdentity(r.Context(), id)))
	})
}

// requireRole rejects identities below want.
func requireRole(want auth.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := identityFrom(r.Context())
			if !ok || !id.Role.AtLeast(want) {
				writeError(w, http.StatusForbidden, CodeForbidden, "insufficient role", map[string]any{
					"requiredRole": string(want),
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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

// apiKeyHeader carries the shared secret a system authenticates with when it
// has no browser and therefore no session cookie.
const apiKeyHeader = "X-AICC-Api-Key"

// machineUsername names the API key in an audit row. There is one key and no
// per-integrator identity behind it, so this is as specific as the truth gets.
const machineUsername = "api-key"

// machineIdentity is who a request authenticated by the API key is.
//
// SUPERVISOR because the key is the deployment's own credential, held by
// whoever configured the server, and every operation it can reach is one a
// supervisor may perform. It is not ADMIN: configuring the platform is a
// person's job and a leaked key should not be able to rewrite the directory.
//
// UserID stays nil, which is the honest answer — no user did this — and is
// what isMachine reads to keep a made-up user id out of the audit trail.
func machineIdentity() auth.Identity {
	return auth.Identity{
		Username:    machineUsername,
		DisplayName: "API key",
		Role:        auth.RoleSupervisor,
	}
}

// isMachine reports whether an identity is the API key rather than a person.
// A real identity always carries the user's id; only the synthesized one is
// nil, so the check needs no extra field on the wire type /auth/me returns.
func isMachine(id auth.Identity) bool { return id.UserID == uuid.Nil }

// requireSessionOrAPIKey authenticates a request that a system may place
// without signing in, falling back to the ordinary session rules when no key
// is presented.
//
// A presented key is answered on its own terms and never falls through: a
// caller who sent the wrong secret meant to authenticate with it, and letting
// them continue into the session path would answer "no session" to what is
// really a bad credential.
//
// No CSRF header is asked of the key path. That header exists to make a
// cross-origin request non-simple so the browser preflights it, which protects
// a caller whose credential travels automatically — a cookie. This one sends
// its credential deliberately on every request and has no cookie to abuse.
func (s *Server) requireSessionOrAPIKey(next http.Handler) http.Handler {
	session := s.requireSession(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := r.Header.Get(apiKeyHeader)
		if presented == "" {
			session.ServeHTTP(w, r)
			return
		}
		// Configured-empty means the machine path was never opened, and the
		// config loader gives an unset variable exactly that. Checking it
		// before the compare keeps an unconfigured deployment from accepting
		// an empty header as a match.
		if s.cfg.APIKey == "" ||
			subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.APIKey)) != 1 {
			// INVALID_CREDENTIALS rather than SESSION_EXPIRED: the code is
			// what an integration branches on, and told its session expired
			// it would go and refresh one it never had.
			writeError(w, http.StatusUnauthorized, CodeInvalidCredentials, "bad API key", nil)
			return
		}
		next.ServeHTTP(w, r.WithContext(contextWithIdentity(r.Context(), machineIdentity())))
	})
}
