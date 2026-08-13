// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// ErrUserExists reports a username collision.
var ErrUserExists = errors.New("username already taken")

// NewUser describes an account to create.
type NewUser struct {
	Username    string
	Password    string
	DisplayName string
	Role        Role
	Locale      *string
}

// CreateUser creates an account with a hashed password.
func (s *Service) CreateUser(ctx context.Context, in NewUser) (Identity, error) {
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" {
		return Identity{}, fmt.Errorf("%w: username is required", ErrValidation)
	}
	if len(in.Password) < 8 {
		return Identity{}, fmt.Errorf("%w: password must be at least 8 characters", ErrValidation)
	}
	if !in.Role.Valid() {
		return Identity{}, fmt.Errorf("%w: unknown role %q", ErrValidation, in.Role)
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}

	if _, err := s.q.GetUserByUsername(ctx, in.Username); err == nil {
		return Identity{}, ErrUserExists
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, fmt.Errorf("check username: %w", err)
	}

	hash, err := HashPassword(in.Password)
	if err != nil {
		return Identity{}, err
	}

	user, err := s.q.CreateUser(ctx, queries.CreateUserParams{
		ID:           uuid.Must(uuid.NewV7()),
		Username:     in.Username,
		PasswordHash: hash,
		DisplayName:  in.DisplayName,
		Role:         string(in.Role),
		Status:       StatusActive,
		Locale:       in.Locale,
	})
	if err != nil {
		return Identity{}, fmt.Errorf("create user: %w", err)
	}
	return identityOf(user), nil
}

// SetPassword replaces a user's password and revokes their sessions, so a
// password change ends any session opened with the old one.
func (s *Service) SetPassword(ctx context.Context, userID uuid.UUID, password string) error {
	if len(password) < 8 {
		return fmt.Errorf("%w: password must be at least 8 characters", ErrValidation)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.q.UpdateUserPassword(ctx, queries.UpdateUserPasswordParams{
		ID:           userID,
		PasswordHash: hash,
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	return s.q.DeleteSessionsForUser(ctx, userID)
}

// ListUsers returns all accounts.
func (s *Service) ListUsers(ctx context.Context) ([]Identity, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	out := make([]Identity, 0, len(rows))
	for _, u := range rows {
		out = append(out, identityOf(u))
	}
	return out, nil
}

// ErrValidation reports invalid input.
var ErrValidation = errors.New("validation failed")
