// SPDX-License-Identifier: Apache-2.0

// Package sipsession mints the credential an agent's phone registers with.
//
// A softphone used to be configured: somebody was told an extension and a
// password, typed both into a browser extension, and the pair outlived every
// reason it existed. This package replaces that with a session. Signing in
// issues a fresh random password, hands the phone the digest of it, and keeps
// only the digest; signing out revokes it and flushes the registration it was
// holding. Nobody ever sees the password, here or anywhere else.
//
// One session per agent, which is what makes "my phone" a single thing: the
// second browser to ask replaces the first, and the first stops ringing.
package sipsession

import (
	"context"
	"crypto/md5" //nolint:gosec // RFC 2617 A1; see a1Hash.
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// ErrNoExtensionBound is an agent with no phone to be. There is nothing to
// register as, so there is no credential to mint — and it is a conflict rather
// than a failure: the request was well formed, the agent's configuration is
// what is not ready.
var ErrNoExtensionBound = errors.New("no extension is bound to this agent")

// Store is the credential table. It holds the digest and never a password.
// *store.SIPSessionStore is it.
type Store interface {
	Get(ctx context.Context, agentID uuid.UUID) (store.SIPSession, error)
	Upsert(ctx context.Context, agentID uuid.UUID, extension, a1Hash string, expiresAt time.Time) (store.SIPSession, error)
	Delete(ctx context.Context, agentID uuid.UUID) (extension string, err error)
	PurgeExpired(ctx context.Context) (int64, error)
}

// Directory answers which phone belongs to an agent. *agents.Service is it.
type Directory interface {
	BoundExtensionFor(ctx context.Context, agentID uuid.UUID) string
}

// Switch is the two things this package asks of FreeSWITCH: end a binding, and
// say whether it can be reached at all.
type Switch interface {
	FlushRegistration(profile, extensionNumber string) error
	IsUp() bool
}

// Config is what a phone is told about the deployment it registers to.
type Config struct {
	// SIPDomain is the digest realm the a1-hash is computed against. It has to
	// be the realm the switch will challenge with, or the hash verifies
	// nothing: the internal profile's challenge-realm=auto_from means that
	// realm is the From-domain the phone sends, which is this value.
	SIPDomain string
	// WSSURL is the WebSocket binding the phone connects to.
	WSSURL string
	// Profile is the sofia profile registrations live on, for the flush.
	Profile string
}

// Issued is a freshly minted credential, as the phone is given it.
type Issued struct {
	SIPDomain string
	WSSURL    string
	Account   string
	A1Hash    string
	ExpiresAt time.Time
}

// Service issues, revokes and sweeps SIP sessions.
type Service struct {
	store  Store
	dir    Directory
	sw     Switch
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
}

// New builds a Service. A nil logger takes the default; a nil clock takes
// time.Now.
func New(store Store, dir Directory, sw Switch, cfg Config, logger *slog.Logger, now func() time.Time) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, dir: dir, sw: sw, cfg: cfg, logger: logger, now: now}
}

// passwordBytes is the entropy behind one phone credential. It matches the
// session token's, because the two are the same kind of thing: a bearer secret
// that is generated, used, and never chosen by a person.
const passwordBytes = 24

// Issue mints the credential for an agent's phone, replacing whatever they
// held before.
//
// The plaintext password exists only as the local variable below. It is not
// returned, not stored, not logged and not derivable from anything that is —
// only its A1 digest leaves this function, which is exactly what a SIP digest
// challenge is answered with and nothing more.
func (s *Service) Issue(ctx context.Context, agentID uuid.UUID, expiresAt time.Time) (Issued, error) {
	extension := s.dir.BoundExtensionFor(ctx, agentID)
	if extension == "" {
		return Issued{}, ErrNoExtensionBound
	}

	// What the new session replaces, read before it is overwritten. Whichever
	// extension it was for: an agent whose phone was rebound still has the old
	// number registered, and that is the binding to end.
	//
	// An unreadable row is not a reason to refuse a sign-in. The worst it
	// costs is a registration left to expire on its own.
	var previous string
	if held, err := s.store.Get(ctx, agentID); err == nil {
		previous = held.Extension
	} else if !errors.Is(err, store.ErrNoSIPSession) {
		s.logger.WarnContext(ctx, "could not read the session being replaced",
			"agentId", agentID, "error", err)
	}

	password, err := newPassword()
	if err != nil {
		return Issued{}, err
	}
	hash := a1Hash(extension, s.cfg.SIPDomain, password)

	if _, err := s.store.Upsert(ctx, agentID, extension, hash, expiresAt); err != nil {
		return Issued{}, fmt.Errorf("store sip session: %w", err)
	}

	// The credential the previous row carried is gone, but the binding it
	// already established is not: the switch keeps ringing that browser until
	// the registration expires by itself, which is minutes of an agent's calls
	// going to a closed tab.
	if previous != "" {
		s.flush(previous, agentID)
	}

	s.logger.InfoContext(ctx, "issued a sip session",
		"agentId", agentID, "extension", extension, "expiresAt", expiresAt)

	return Issued{
		SIPDomain: s.cfg.SIPDomain,
		WSSURL:    s.cfg.WSSURL,
		Account:   extension,
		A1Hash:    hash,
		ExpiresAt: expiresAt,
	}, nil
}

// Revoke ends an agent's session and the registration it was holding.
//
// Idempotent: an agent with no session is already in the state this asks for.
func (s *Service) Revoke(ctx context.Context, agentID uuid.UUID) error {
	extension, err := s.store.Delete(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNoSIPSession) {
			return nil
		}
		return fmt.Errorf("revoke sip session: %w", err)
	}
	if extension != "" {
		s.flush(extension, agentID)
	}
	s.logger.InfoContext(ctx, "revoked a sip session", "agentId", agentID, "extension", extension)
	return nil
}

// PurgeExpired removes sessions past their expiry.
//
// Housekeeping, not enforcement: the directory view already ignores an expired
// row, so a credential stops working at its expiry whether or not this has run.
func (s *Service) PurgeExpired(ctx context.Context) (int64, error) {
	return s.store.PurgeExpired(ctx)
}

// flush ends the switch-side binding, and treats an unreachable switch as a
// warning rather than a failure.
//
// A session that cannot be flushed is still revoked. The alternative — refusing
// to issue a credential because the switch is down — would leave an agent
// unable to sign in during exactly the outage they are needed for, to prevent a
// stale registration that expires on its own in minutes.
func (s *Service) flush(extension string, agentID uuid.UUID) {
	if s.sw == nil || !s.sw.IsUp() {
		s.logger.Warn("could not flush a registration: the switch is not reachable",
			"agentId", agentID, "extension", extension)
		return
	}
	if err := s.sw.FlushRegistration(s.cfg.Profile, extension); err != nil {
		s.logger.Warn("could not flush a registration",
			"agentId", agentID, "extension", extension, "error", err)
	}
}

// newPassword returns a fresh phone password. It is returned to exactly one
// caller, which hashes it and lets it go out of scope.
func newPassword() (string, error) {
	buf := make([]byte, passwordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read sip password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// a1Hash computes RFC 2617's A1: md5(username:realm:password), lower-case hex.
//
// MD5 here is not a choice about storing passwords — it is the digest SIP
// authentication is defined in terms of, and the only value a registrar can
// verify a phone's response against. Nothing in this repository hashes a human
// password this way; account passwords are argon2id (internal/auth). What
// protects this one is that the password behind it is 24 random bytes with a
// lifetime of one web session, never reused and never typed by anybody.
func a1Hash(username, realm, password string) string {
	sum := md5.Sum([]byte(username + ":" + realm + ":" + password)) //nolint:gosec // RFC 2617 A1, not a password store.
	return hex.EncodeToString(sum[:])
}
