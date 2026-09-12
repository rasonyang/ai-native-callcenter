// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// SIPSessionStore holds the one credential an agent's phone registers with.
//
// The table has no plaintext column and this type has no plaintext field. What
// is stored is the digest a SIP challenge is answered with, and the password it
// was derived from exists only as a local variable in the service that mints
// it.
type SIPSessionStore struct{ q *queries.Queries }

// SIPSessions returns the session store.
func (s *Store) SIPSessions() *SIPSessionStore { return &SIPSessionStore{q: s.Queries} }

// SIPSession is one agent's phone credential as it is kept.
type SIPSession struct {
	AgentID   uuid.UUID `json:"agentId"`
	Extension string    `json:"extension"`
	// A1Hash is RFC 2617's A1: md5(extension:realm:password), lower-case hex.
	// It is what the switch verifies a digest response against, and it is the
	// only form of the credential that survives being issued.
	A1Hash    string    `json:"a1Hash"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ErrNoSIPSession is the absence of a session, reported rather than invented:
// a caller asking for one has to be able to tell "none" from "a broken read".
var ErrNoSIPSession = errors.New("no sip session")

// Upsert issues a session, replacing whatever the agent held before. One row
// per agent is the rule, so a second sign-in leaves the first browser holding
// a credential the switch no longer accepts.
func (s *SIPSessionStore) Upsert(ctx context.Context, agentID uuid.UUID, extension, a1Hash string, expiresAt time.Time) (SIPSession, error) {
	row, err := s.q.UpsertSIPSession(ctx, queries.UpsertSIPSessionParams{
		AgentID:   agentID,
		Extension: extension,
		A1Hash:    a1Hash,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return SIPSession{}, fmt.Errorf("upsert sip session: %w", err)
	}
	return sipSessionOf(row), nil
}

// Get returns the agent's session, or ErrNoSIPSession.
func (s *SIPSessionStore) Get(ctx context.Context, agentID uuid.UUID) (SIPSession, error) {
	row, err := s.q.GetSIPSession(ctx, agentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SIPSession{}, ErrNoSIPSession
		}
		return SIPSession{}, fmt.Errorf("get sip session: %w", err)
	}
	return sipSessionOf(row), nil
}

// Delete revokes the agent's session and reports the extension it was for, so
// the caller can flush the registration it was holding. ErrNoSIPSession when
// there was nothing to revoke, which is not a failure — revoking is idempotent
// and the caller decides what to do with the absence.
func (s *SIPSessionStore) Delete(ctx context.Context, agentID uuid.UUID) (extension string, err error) {
	extension, err = s.q.DeleteSIPSession(ctx, agentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoSIPSession
		}
		return "", fmt.Errorf("delete sip session: %w", err)
	}
	return extension, nil
}

// PurgeExpired removes sessions past their expiry and reports how many went.
//
// The view already ignores them — a phone stops authenticating the moment its
// session expires, without anything having to run — so this is housekeeping
// rather than enforcement.
func (s *SIPSessionStore) PurgeExpired(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredSIPSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("purge sip sessions: %w", err)
	}
	return n, nil
}

func sipSessionOf(row queries.SipSession) SIPSession {
	return SIPSession{
		AgentID:   row.AgentID,
		Extension: row.Extension,
		A1Hash:    row.A1Hash,
		CreatedAt: row.CreatedAt.Time,
		ExpiresAt: row.ExpiresAt.Time,
	}
}
