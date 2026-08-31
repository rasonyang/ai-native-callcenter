// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// APIKeyStore issues and authenticates the credentials a system integrates
// with.
type APIKeyStore struct{ q *queries.Queries }

// APIKeys returns the key store.
func (s *Store) APIKeys() *APIKeyStore { return &APIKeyStore{q: s.Queries} }

// Key statuses. REVOKED is terminal: there is no path back and no third
// state, so a credential somebody had reason to switch off is one to reissue.
const (
	APIKeyEnabled = "ENABLED"
	APIKeyRevoked = "REVOKED"
)

// ErrKeyAlreadyRevoked is a second revocation of the same key. Reported
// rather than swallowed: an operator who revokes twice is usually looking at
// a stale screen, and telling them the key is already gone is the answer they
// need.
var ErrKeyAlreadyRevoked = errors.New("the key is already revoked")

// APIKey is one integration's credential, as it can be read back. The secret
// is not here and cannot be: only its digest was ever stored.
type APIKey struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	// KeyPrefix is the leading, non-secret part, for telling two keys apart
	// in a list. Nothing is ever looked up by it.
	KeyPrefix  string     `json:"keyPrefix"`
	Status     string     `json:"status"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// keySecretBytes is the entropy behind a key, matching the session token's
// 32 bytes — the two are the same kind of thing and there is no reason for
// one to be weaker.
const keySecretBytes = 32

// keyPrefixLen is how much of the secret is kept in clear for display. Short
// enough to identify nothing on its own, long enough to tell two keys apart
// in a list.
const keyPrefixLen = 8

// Issue mints a key and returns the secret, once.
//
// The secret is generated here and immediately reduced to a SHA-256 digest
// for storage, so this return value is the only time it exists outside the
// caller's hands. Nothing in this package, this database or this application
// can produce it again.
func (s *APIKeyStore) Issue(ctx context.Context, name string, scopes []string,
	createdBy *uuid.UUID) (key APIKey, secret string, err error) {
	raw := make([]byte, keySecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return APIKey{}, "", fmt.Errorf("generate key: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(secret))

	if scopes == nil {
		scopes = []string{}
	}
	row, err := s.q.CreateAPIKey(ctx, queries.CreateAPIKeyParams{
		ID:        uuid.New(),
		Name:      name,
		KeyHash:   digest[:],
		KeyPrefix: secret[:keyPrefixLen],
		Scopes:    scopes,
		CreatedBy: createdBy,
	})
	if err != nil {
		return APIKey{}, "", err
	}
	return apiKeyFrom(row), secret, nil
}

// Authenticate resolves a presented secret into the key that holds it, and
// records the use.
//
// The lookup is by digest, directly (GetAPIKeyByHash), which is the shape
// GetSessionByTokenHash already uses: the digest is the only thing both sides
// can compute, so there is nothing to scan and nothing to compare here. The
// query also inlines the status condition, so a revoked key is not a row that
// came back and was then rejected — it does not come back, and its
// last_used_at is therefore never touched by a request it refused.
func (s *APIKeyStore) Authenticate(ctx context.Context, secret string) (APIKey, error) {
	if secret == "" {
		return APIKey{}, pgx.ErrNoRows
	}
	digest := sha256.Sum256([]byte(secret))
	row, err := s.q.GetAPIKeyByHash(ctx, digest[:])
	if err != nil {
		return APIKey{}, err
	}
	// Written on every authenticated request, with no throttle. One indexed
	// update against a primary key is not worth a cache that would have to be
	// correct across restarts to tell an operator the truth about a key they
	// are deciding whether to revoke.
	if err := s.q.TouchAPIKey(ctx, row.ID); err != nil {
		return APIKey{}, err
	}
	return apiKeyFrom(row), nil
}

// Get returns one key.
func (s *APIKeyStore) Get(ctx context.Context, id uuid.UUID) (APIKey, error) {
	row, err := s.q.GetAPIKey(ctx, id)
	if err != nil {
		return APIKey{}, err
	}
	return apiKeyFrom(row), nil
}

// List returns every key, newest first. Revoked ones stay in the list: they
// are what an audit row from last month refers to.
func (s *APIKeyStore) List(ctx context.Context) ([]APIKey, error) {
	rows, err := s.q.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]APIKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, apiKeyFrom(row))
	}
	return out, nil
}

// Update renames a key or changes what it may do. Nil leaves a field alone.
//
// Status is not here. The only status change is revocation, it is terminal,
// and it has an operation of its own — a PATCH that could also flip a key
// back on would make "revoked" a state rather than an ending.
func (s *APIKeyStore) Update(ctx context.Context, id uuid.UUID, name *string, scopes *[]string) (APIKey, error) {
	arg := queries.UpdateAPIKeyParams{ID: id, Name: name}
	if scopes != nil {
		arg.Scopes = *scopes
		if arg.Scopes == nil {
			arg.Scopes = []string{}
		}
	}
	row, err := s.q.UpdateAPIKey(ctx, arg)
	if err != nil {
		return APIKey{}, err
	}
	return apiKeyFrom(row), nil
}

// Revoke ends a key. Revoking one that is already revoked reports
// ErrKeyAlreadyRevoked rather than moving revoked_at, so the record keeps
// saying when the key actually stopped working.
func (s *APIKeyStore) Revoke(ctx context.Context, id uuid.UUID) (APIKey, error) {
	row, err := s.q.RevokeAPIKey(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Either it does not exist or it is already revoked. Ask which, so
		// the caller can tell a stale screen from a wrong id.
		if _, getErr := s.q.GetAPIKey(ctx, id); getErr == nil {
			return APIKey{}, ErrKeyAlreadyRevoked
		}
		return APIKey{}, pgx.ErrNoRows
	case err != nil:
		return APIKey{}, err
	}
	return apiKeyFrom(row), nil
}

func apiKeyFrom(row queries.ApiKey) APIKey {
	key := APIKey{
		ID:        row.ID,
		Name:      row.Name,
		KeyPrefix: row.KeyPrefix,
		Status:    row.Status,
		Scopes:    row.Scopes,
		CreatedAt: row.CreatedAt.Time,
	}
	if key.Scopes == nil {
		key.Scopes = []string{}
	}
	if row.LastUsedAt.Valid {
		t := row.LastUsedAt.Time
		key.LastUsedAt = &t
	}
	if row.RevokedAt.Valid {
		t := row.RevokedAt.Time
		key.RevokedAt = &t
	}
	return key
}
