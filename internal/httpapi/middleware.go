// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

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
