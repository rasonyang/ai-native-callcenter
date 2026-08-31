// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

// SubjectKind is what authenticated a request. There are two kinds and they
// are equals: a person signed in with a browser session, and a system holding
// an API key. Business code reads capabilities, never this — the kind exists
// because a record of who did something has to say which of the two it was,
// and because a handful of operations are genuinely about a session.
type SubjectKind string

const (
	// SubjectUser is a person, authenticated by the session cookie.
	SubjectUser SubjectKind = "USER"
	// SubjectKey is a system, authenticated by a bearer API key.
	SubjectKey SubjectKind = "KEY"
)

// AuthContext is who is making a request and what they may do. It is built
// once, by the authentication middleware, and is the only thing a handler
// asks about authorization: no handler reads a role, and none re-derives an
// agent identity.
//
// Roles have not disappeared from the product — an account still has one, and
// /auth/me still reports it — but a role is now only the map that turns a
// login into scopes (grantedScopes). Nothing downstream of this type branches
// on one.
type AuthContext struct {
	// Kind is USER or KEY.
	Kind SubjectKind

	// SubjectID identifies the subject within its kind: a user id, or a key
	// id. The two are different spaces and must never be mixed in one column
	// — which is why the audit trail records the kind beside the id.
	SubjectID uuid.UUID

	// SubjectName is what to call the subject in a log line or an audit row:
	// a username, or a key's name.
	SubjectName string

	// User is the account behind a browser session. A key has none, and the
	// zero value is the honest answer rather than an invented account. Only
	// the operations that are genuinely about a session read it.
	User auth.Identity

	// AgentID is the agent identity this request acts as, or uuid.Nil when
	// the subject acts as no agent. For a session it is the agent bound to
	// the account, resolved once here rather than thirteen times downstream.
	// For a key it is the agent named in X-AICC-Agent-ID.
	//
	// It is separate from SubjectID on purpose: a supervisor is a subject
	// with no agent, and a key acting for an agent is one subject working as
	// somebody else's identity. Collapsing the two is what made "is this
	// call yours?" ask about the account instead of the agent.
	AgentID uuid.UUID

	// scopes is what this subject may do, sorted and deduplicated.
	scopes []string
}

// Has reports whether the subject holds a scope.
func (a AuthContext) Has(scope string) bool { return slices.Contains(a.scopes, scope) }

// IsAgent reports whether this request acts as an agent identity.
func (a AuthContext) IsAgent() bool { return a.AgentID != uuid.Nil }

// Scopes returns what the subject holds, for the answer /auth/me gives and
// for an audit row. The copy keeps a caller from editing the request's own
// authorization.
func (a AuthContext) Scopes() []string { return slices.Clone(a.scopes) }

type contextKey int

const authKey contextKey = iota

// authFrom returns the AuthContext carried by ctx.
func authFrom(ctx context.Context) (AuthContext, bool) {
	ac, ok := ctx.Value(authKey).(AuthContext)
	return ac, ok
}

// contextWithAuth attaches an AuthContext to ctx.
func contextWithAuth(ctx context.Context, ac AuthContext) context.Context {
	return context.WithValue(ctx, authKey, ac)
}
