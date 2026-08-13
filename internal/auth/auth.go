// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// Role is the authorization level of a user.
type Role string

// Roles, ordered by privilege.
const (
	RoleAgent      Role = "AGENT"
	RoleSupervisor Role = "SUPERVISOR"
	RoleAdmin      Role = "ADMIN"
)

var roleRank = map[Role]int{RoleAgent: 1, RoleSupervisor: 2, RoleAdmin: 3}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { _, ok := roleRank[r]; return ok }

// AtLeast reports whether r is at least as privileged as want.
func (r Role) AtLeast(want Role) bool { return roleRank[r] >= roleRank[want] }

// Status values of a user account.
const (
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"
)

// Identity is the authenticated principal carried through a request.
type Identity struct {
	UserID      uuid.UUID `json:"userId"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	Role        Role      `json:"role"`
	Locale      *string   `json:"locale"`
}

// Errors returned by the service. Handlers map them to API error codes.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrSessionExpired     = errors.New("session expired")
	ErrUserSuspended      = errors.New("user suspended")
)

// Service issues and validates sessions.
type Service struct {
	q          *queries.Queries
	sessionTTL time.Duration
}

// NewService builds a Service.
func NewService(q *queries.Queries, sessionTTL time.Duration) *Service {
	return &Service{q: q, sessionTTL: sessionTTL}
}

// Login verifies credentials and issues a session token.
//
// The returned token is the raw cookie value; only its SHA-256 digest is
// stored, so a database leak cannot be replayed as a session.
func (s *Service) Login(ctx context.Context, username, password, userAgent string, ip netip.Addr) (token string, id Identity, err error) {
	user, err := s.q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend comparable time on unknown users so timing does not leak
			// which usernames exist.
			_, _ = VerifyPassword(password, dummyHash)
			return "", Identity{}, ErrInvalidCredentials
		}
		return "", Identity{}, fmt.Errorf("get user: %w", err)
	}

	ok, err := VerifyPassword(password, user.PasswordHash)
	if err != nil || !ok {
		return "", Identity{}, ErrInvalidCredentials
	}
	if user.Status == StatusSuspended {
		return "", Identity{}, ErrUserSuspended
	}

	token, hash, err := newToken()
	if err != nil {
		return "", Identity{}, err
	}

	var ipVal *netip.Addr
	if ip.IsValid() {
		ipVal = &ip
	}
	var uaVal *string
	if userAgent != "" {
		uaVal = &userAgent
	}

	if _, err := s.q.CreateSession(ctx, queries.CreateSessionParams{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    user.ID,
		TokenHash: hash,
		UserAgent: uaVal,
		IP:        ipVal,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(s.sessionTTL), Valid: true},
	}); err != nil {
		return "", Identity{}, fmt.Errorf("create session: %w", err)
	}

	if err := s.q.TouchUserLogin(ctx, user.ID); err != nil {
		return "", Identity{}, fmt.Errorf("touch login: %w", err)
	}

	return token, identityOf(user), nil
}

// Authenticate resolves a session token to its identity.
func (s *Service) Authenticate(ctx context.Context, token string) (Identity, error) {
	row, err := s.q.GetSessionByTokenHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, ErrSessionExpired
		}
		return Identity{}, fmt.Errorf("get session: %w", err)
	}
	if row.User.Status == StatusSuspended {
		return Identity{}, ErrUserSuspended
	}
	return identityOf(row.User), nil
}

// Logout revokes a single session.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.q.DeleteSession(ctx, hashToken(token))
}

// PurgeExpiredSessions deletes sessions past their expiry and reports how many
// rows were removed.
func (s *Service) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	return s.q.DeleteExpiredSessions(ctx)
}

func identityOf(u queries.User) Identity {
	return Identity{
		UserID:      u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        Role(u.Role),
		Locale:      u.Locale,
	}
}

// newToken returns a fresh session token and its stored digest.
func newToken() (token string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("read token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// dummyHash is a valid argon2id hash of a random value, used to equalize the
// cost of the unknown-user path.
const dummyHash = "$argon2id$v=19$m=65536,t=1,p=4$c29tZXNhbHR2YWx1ZXh4$Q2hlY2tzdW1QbGFjZWhvbGRlclZhbHVlMDAwMDAwMDA"
