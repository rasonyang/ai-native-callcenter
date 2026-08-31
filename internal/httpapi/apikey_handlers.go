// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/docs"
	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// KeyService is the key management surface used by the API. It is the same
// object the authentication middleware talks to through KeyAuthenticator, so
// a deployment either has keys or does not — there is no state where a key
// can be issued and then fails to authenticate.
type KeyService interface {
	KeyAuthenticator

	Issue(ctx context.Context, name string, scopes []string, createdBy *uuid.UUID) (store.APIKey, string, error)
	Get(ctx context.Context, id uuid.UUID) (store.APIKey, error)
	List(ctx context.Context) ([]store.APIKey, error)
	Update(ctx context.Context, id uuid.UUID, name *string, scopes *[]string) (store.APIKey, error)
	Revoke(ctx context.Context, id uuid.UUID) (store.APIKey, error)
}

// GetOpenAPI serves the contract, to anyone.
//
// Unauthenticated on purpose: the description of an interface is not a secret,
// every endpoint in it enforces its own authorization, and requiring a
// credential to read it would mean an integrator has to authenticate before
// they can learn how to authenticate.
func (s *Server) GetOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(docs.Contract)
}

// ListAPIKeys returns every key, revoked ones included: a revoked key is what
// an audit row from last month refers to, and a list that hid them would
// leave that row pointing at nothing.
func (s *Server) ListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.keys.List(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot list api keys", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list the keys", nil)
		return
	}
	items := make([]api.APIKey, 0, len(keys))
	for _, key := range keys {
		items = append(items, apiKeyResponse(key))
	}
	writeJSON(w, http.StatusOK, api.APIKeyList{Items: items})
}

// CreateAPIKey issues a key and returns its secret — the only time the secret
// is ever returned by anything.
func (s *Server) CreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req api.APIKeyWrite
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a key needs a name saying what it is for", map[string]any{"field": "name"})
		return
	}
	scopes, ok := checkedScopes(w, req.Scopes)
	if !ok {
		return
	}

	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	var createdBy *uuid.UUID
	if ac.Kind == SubjectUser {
		id := ac.SubjectID
		createdBy = &id
	}

	key, secret, err := s.keys.Issue(r.Context(), name, scopes, createdBy)
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot issue an api key", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot issue the key", nil)
		return
	}
	out := apiKeyResponse(key)
	writeJSON(w, http.StatusCreated, api.APIKeyCreated{
		ID:         out.ID,
		Name:       out.Name,
		KeyPrefix:  out.KeyPrefix,
		Status:     out.Status,
		Scopes:     out.Scopes,
		CreatedAt:  out.CreatedAt,
		LastUsedAt: out.LastUsedAt,
		RevokedAt:  out.RevokedAt,
		Secret:     secret,
	})
}

// GetAPIKey returns one key. Never its secret: there is none to return, only
// a digest was stored.
func (s *Server) GetAPIKey(w http.ResponseWriter, r *http.Request, keyID uuid.UUID) {
	key, err := s.keys.Get(r.Context(), keyID)
	if err != nil {
		writeKeyLookupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiKeyResponse(key))
}

// UpdateAPIKey renames a key or changes what it may do.
//
// Status is not something this can set. The only status change is revocation,
// which is terminal and has an operation of its own — a PATCH that could also
// switch a key back on would make "revoked" a state rather than an ending.
func (s *Server) UpdateAPIKey(w http.ResponseWriter, r *http.Request, keyID uuid.UUID) {
	var req api.APIKeyUpdate
	if !decode(w, r, &req) {
		return
	}
	var name *string
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
				"a key needs a name saying what it is for", map[string]any{"field": "name"})
			return
		}
		name = &trimmed
	}
	var scopes *[]string
	if req.Scopes != nil {
		checked, ok := checkedScopes(w, *req.Scopes)
		if !ok {
			return
		}
		scopes = &checked
	}

	key, err := s.keys.Update(r.Context(), keyID, name, scopes)
	if err != nil {
		writeKeyLookupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiKeyResponse(key))
}

// RevokeAPIKey ends a key, for good.
func (s *Server) RevokeAPIKey(w http.ResponseWriter, r *http.Request, keyID uuid.UUID) {
	key, err := s.keys.Revoke(r.Context(), keyID)
	switch {
	case errors.Is(err, store.ErrKeyAlreadyRevoked):
		// Said out loud rather than answered 200: an operator revoking twice
		// is usually looking at a stale screen, and a silent success would
		// let them believe they had just stopped something.
		writeError(w, http.StatusConflict, CodeConflict, "the key is already revoked", nil)
		return
	case err != nil:
		writeKeyLookupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiKeyResponse(key))
}

// checkedScopes refuses a name that is not in the vocabulary rather than
// ignoring it.
//
// An unknown scope stored as written would give the operator a key they
// believe has a capability it does not, and it would fail later, somewhere
// else, for a reason nobody can connect to this form. The vocabulary is
// generated from the contract's x-scopes, so this cannot drift from what the
// operations actually ask for.
func checkedScopes(w http.ResponseWriter, scopes []string) ([]string, bool) {
	var unknown []string
	for _, scope := range scopes {
		if !api.IsScope(scope) {
			unknown = append(unknown, scope)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"no such scope: "+strings.Join(unknown, ", "),
			map[string]any{"field": "scopes", "unknown": unknown, "allowed": api.AllScopes})
		return nil, false
	}
	out := slices.Clone(scopes)
	slices.Sort(out)
	return slices.Compact(out), true
}

func writeKeyLookupError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such key", nil)
		return
	}
	slog.ErrorContext(r.Context(), "api key request failed", "error", err)
	writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the key", nil)
}

// apiKeyResponse maps the store's key onto the contract's, explicitly. The
// two shapes agree today; the mapping is here so that a column added to the
// table does not appear on the wire because nobody stopped it.
func apiKeyResponse(key store.APIKey) api.APIKey {
	return api.APIKey{
		ID:         key.ID,
		Name:       key.Name,
		KeyPrefix:  key.KeyPrefix,
		Status:     api.APIKeyStatus(key.Status),
		Scopes:     key.Scopes,
		CreatedAt:  key.CreatedAt,
		LastUsedAt: key.LastUsedAt,
		RevokedAt:  key.RevokedAt,
	}
}

// APIKeys adapts the store's key store to the shape this package needs.
//
// The seam is here rather than in the store because KeySubject is an
// authorization idea — who is asking and what they may do — and the store's
// job ends at the row. It is one type conversion and no logic, which is the
// point: there is nowhere for a rule to hide in it.
type APIKeys struct{ *store.APIKeyStore }

// AuthenticateKey resolves a presented secret into the subject holding it.
func (k APIKeys) AuthenticateKey(ctx context.Context, secret string) (KeySubject, error) {
	key, err := k.Authenticate(ctx, secret)
	if err != nil {
		return KeySubject{}, err
	}
	// AgentID stays zero: a key belongs to a system, not to a person, and the
	// agent it works for is a fact about the request (X-AICC-Agent-ID), not
	// about the credential. Binding one here would make a key that can only
	// ever act for the same agent, which is a role by another name.
	return KeySubject{KeyID: key.ID, Name: key.Name, Scopes: key.Scopes}, nil
}

// The adapter is the whole KeyService: a compile error here means the store
// and the API have drifted apart.
var _ KeyService = APIKeys{}
