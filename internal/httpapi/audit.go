// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Auditor records who did what to which thing.
type Auditor interface {
	Audit(ctx context.Context, actorID *uuid.UUID,
		action, targetKind, targetID string, detail map[string]any, ip string) error
}

// auditBodyLimit bounds how much of a request lands in the audit detail.
const auditBodyLimit = 4 << 10

// auditTrail records every mutating request that succeeded.
//
// It is a middleware rather than a call in each handler so that coverage is
// structural: a new POST route is audited the moment it exists, not when
// someone remembers. The action is the route pattern itself — stable,
// greppable, and impossible to let drift from the routing table.
func (s *Server) auditTrail(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auditor == nil || !isMutating(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		// The body is read for the audit detail and handed back untouched.
		// Credentials never belong in an audit row, so auth requests keep
		// their bodies to themselves.
		var detail map[string]any
		if !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") && r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, auditBodyLimit))
			if err == nil {
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
				if len(body) > 0 {
					detail = map[string]any{"request": string(body)}
				}
			}
		}

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		// Only what happened is audited; a rejected request changed nothing.
		if recorder.status >= 300 {
			return
		}

		var actorID *uuid.UUID
		if identity, ok := identityFrom(r.Context()); ok {
			id := identity.UserID
			actorID = &id
		}

		action := r.Method + " " + routePattern(r)
		targetKind, targetID := targetFrom(r)
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)

		if err := s.auditor.Audit(r.Context(), actorID, action, targetKind, targetID, detail, ip); err != nil {
			// The action already happened; losing its audit row is worth a
			// loud log line but not a failed response.
			s.logAuditFailure(r, err)
		}
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// routePattern is the chi template that matched, e.g. /api/v1/queues/{queueId}.
func routePattern(r *http.Request) string {
	if ctx := chi.RouteContext(r.Context()); ctx != nil {
		if pattern := ctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return r.URL.Path
}

// targetFrom derives what the request acted on from its path parameters: the
// last {xxxId} parameter names the kind, its value the instance.
func targetFrom(r *http.Request) (kind, id string) {
	ctx := chi.RouteContext(r.Context())
	if ctx == nil {
		return "", ""
	}
	params := ctx.URLParams
	for i := len(params.Keys) - 1; i >= 0; i-- {
		key := params.Keys[i]
		if strings.HasSuffix(key, "Id") {
			return strings.TrimSuffix(key, "Id"), params.Values[i]
		}
	}
	return "", ""
}

// statusRecorder remembers what the handler answered.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush keeps SSE working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logAuditFailure is split out so the middleware body stays readable.
func (s *Server) logAuditFailure(r *http.Request, err error) {
	slog.ErrorContext(r.Context(), "audit write failed",
		"method", r.Method, "path", r.URL.Path, "error", err)
}
