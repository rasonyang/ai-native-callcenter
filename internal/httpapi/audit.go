// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Auditor records who did what to which thing.
type Auditor interface {
	Audit(ctx context.Context, who store.AuditSubject,
		action, targetKind, targetID string, detail map[string]any, ip string) error
}

// auditSubject is who to record a request against.
//
// actorId keeps its original meaning exactly — the signed-in account, absent
// where there is none — because GET /audit-logs already returns it and
// changing what a shipped field means while keeping its name and type is a
// break no diff tool can see. The subject columns are the new information:
// which credential asked, and which agent identity it acted as.
func auditSubject(r *http.Request) store.AuditSubject {
	ac, ok := authFrom(r.Context())
	if !ok {
		return store.AuditSubject{}
	}
	who := store.AuditSubject{
		Kind: string(ac.Kind),
		Name: ac.SubjectName,
	}
	if ac.SubjectID != uuid.Nil {
		id := ac.SubjectID
		who.ID = &id
	}
	if ac.Kind == SubjectUser && ac.SubjectID != uuid.Nil {
		id := ac.SubjectID
		who.ActorID = &id
	}
	if ac.AgentID != uuid.Nil {
		agentID := ac.AgentID
		who.AgentID = &agentID
	}
	return who
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
		// Credentials never belong in an audit row: auth requests keep their
		// bodies to themselves, and everything else is redacted by field name
		// on the way in.
		var detail map[string]any
		if !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") && r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, auditBodyLimit))
			if err == nil {
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
				if redacted, ok := redactSecrets(body); ok {
					detail = map[string]any{"request": redacted}
				}
			}
		}

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		// Only what happened is audited; a rejected request changed nothing.
		if recorder.status >= 300 {
			return
		}

		action := r.Method + " " + routePattern(r)
		targetKind, targetID := targetFrom(r)
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)

		if err := s.auditor.Audit(r.Context(), auditSubject(r),
			action, targetKind, targetID, detail, ip); err != nil {
			// The action already happened; losing its audit row is worth a
			// loud log line but not a failed response.
			s.logAuditFailure(r, err)
		}
	})
}

// redactSecrets prepares a request body for the audit row, replacing the value
// of any field whose name reads like a secret, at any depth.
//
// By field name rather than by path. The guard above this is a path prefix, and
// a path prefix is the kind of thing that goes stale: it covers /auth/ because
// that is where passwords were when it was written, and it silently failed to
// cover POST /extensions, which carries the SIP registration password of a
// phone. Four rows in the development database hold one in clear text — enough
// to register as that extension and take its calls. A field-name rule covers
// the endpoint nobody has written yet.
//
// A body that is not a JSON object is not stored at all. It cannot be
// inspected, so it cannot be vouched for, and there is no such write endpoint
// in the contract; a malformed body is answered 400 and never reaches an audit
// row anyway.
func redactSecrets(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", false
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return "", false
	}
	redactInto(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// redactedValue is what an audit row says instead of a secret. A marker rather
// than an omission: "this field was sent and we are not keeping it" is a
// different fact from "this field was not sent", and an audit trail should not
// blur them.
const redactedValue = "[redacted]"

func redactInto(node map[string]any) {
	for key, value := range node {
		if isSecretField(key) {
			node[key] = redactedValue
			continue
		}
		switch child := value.(type) {
		case map[string]any:
			redactInto(child)
		case []any:
			for _, item := range child {
				if nested, ok := item.(map[string]any); ok {
					redactInto(nested)
				}
			}
		}
	}
}

// isSecretField reads a field name the way an operator would. Substring rather
// than exact match, so newPassword and apiKeyId are covered without a list of
// every spelling somebody might choose.
func isSecretField(name string) bool {
	lower := strings.ToLower(name)
	for _, word := range []string{"password", "secret", "token", "apikey", "credential"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
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
