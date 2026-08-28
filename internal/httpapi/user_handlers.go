// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// Accounts, as an administrator manages them.
//
// Creating one is the only place three things are written together: the
// account, the ACD identity of somebody who takes calls, and the phone they
// take them at. Until this existed the only way to make an account was the
// `aicc useradd` command, which is why the administration screens could
// configure a call centre but not staff one.

// AccountService is the account surface the API writes through.
type AccountService interface {
	List(ctx context.Context) ([]store.Account, error)
	Get(ctx context.Context, userID uuid.UUID) (store.Account, error)
	Provision(ctx context.Context, in store.NewAccount) (store.Account, error)
	EnsureAgentIdentity(ctx context.Context, userID uuid.UUID, in store.NewAccount) (store.Account, error)
	Update(ctx context.Context, userID uuid.UUID, in store.Changes) (store.Account, error)
}

// usernamePattern is the contract's, restated where it is enforced: the
// generated server binds parameters but does not validate bodies.
//
// Narrower than the column on purpose. An agent's switch-side name is derived
// from the username, and mod_callcenter cannot hold a space, an @ or a quote —
// so the characters are refused at the door rather than mangled behind it.
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ListUsers serves every account with what belongs to it.
func (s *Server) ListUsers(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot list accounts", nil)
		return
	}
	rows, err := s.accounts.List(r.Context())
	if err != nil {
		s.writeAccountError(w, r, err)
		return
	}
	items := make([]api.User, 0, len(rows))
	for _, row := range rows {
		items = append(items, accountOut(row))
	}
	writeJSON(w, http.StatusOK, api.UserList{Items: items})
}

// CreateUser provisions an account, and for a call-taking role its identity
// and phone with it.
func (s *Server) CreateUser(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil || s.auth == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot create accounts", nil)
		return
	}
	var in api.UserCreate
	if !decode(w, r, &in) {
		return
	}
	username := strings.TrimSpace(in.Username)
	if !usernamePattern.MatchString(username) || len(username) > 64 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a username is letters, digits, dot, underscore and hyphen",
			map[string]any{"field": "username", "rule": "USERNAME_CHARSET"})
		return
	}
	if !auth.Role(in.Role).Valid() {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"unknown role", map[string]any{"field": "role", "rule": "UNKNOWN_ROLE"})
		return
	}
	if len(in.Password) < 8 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a password is at least 8 characters", map[string]any{"field": "password", "rule": "PASSWORD_MIN_8"})
		return
	}

	// Hashing is deliberately slow, so it happens before the transaction opens
	// rather than while it holds the extension pool's lock.
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		slog.ErrorContext(r.Context(), "hash password", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "cannot create the account", nil)
		return
	}

	sipPassword, err := newSIPPassword()
	if err != nil {
		slog.ErrorContext(r.Context(), "generate sip password", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "cannot create the account", nil)
		return
	}

	displayName := strings.TrimSpace(deref(in.DisplayName))
	if displayName == "" {
		displayName = username
	}
	low, high, err := s.cfg.ExtensionPool()
	if err != nil {
		// Refused at startup, so reaching here means the config changed under
		// a running process.
		slog.ErrorContext(r.Context(), "extension range unusable", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "cannot allocate a phone", nil)
		return
	}

	account, err := s.accounts.Provision(r.Context(), store.NewAccount{
		Username:       username,
		PasswordHash:   hash,
		DisplayName:    displayName,
		Role:           string(in.Role),
		Locale:         in.Locale,
		CallcenterName: callcenterNameFor(username),
		SIPPassword:    sipPassword,
		ExtensionLow:   low,
		ExtensionHigh:  high,
	})
	if err != nil {
		s.writeAccountError(w, r, err)
		return
	}
	s.mirrorNewAgent(r.Context(), account)
	writeJSON(w, http.StatusCreated, accountOut(account))
}

// UpdateUser rewrites an account, provisioning what a promotion now needs.
func (s *Server) UpdateUser(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	if s.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot edit accounts", nil)
		return
	}
	var in api.UserUpdate
	if !decode(w, r, &in) {
		return
	}
	username := strings.TrimSpace(in.Username)
	if !usernamePattern.MatchString(username) || len(username) > 64 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a username is letters, digits, dot, underscore and hyphen",
			map[string]any{"field": "username", "rule": "USERNAME_CHARSET"})
		return
	}
	if !auth.Role(in.Role).Valid() {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"unknown role", map[string]any{"field": "role", "rule": "UNKNOWN_ROLE"})
		return
	}
	if in.Status != api.UserStatusACTIVE && in.Status != api.UserStatusSUSPENDED {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"unknown status", map[string]any{"field": "status", "rule": "UNKNOWN_STATUS"})
		return
	}
	displayName := strings.TrimSpace(deref(in.DisplayName))
	if displayName == "" {
		displayName = username
	}

	account, err := s.accounts.Update(r.Context(), userID, store.Changes{
		Username:    username,
		DisplayName: displayName,
		Role:        string(in.Role),
		Status:      string(in.Status),
		Locale:      in.Locale,
	})
	if err != nil {
		s.writeAccountError(w, r, err)
		return
	}

	// A promotion into a call-taking role needs the identity and phone the
	// account never had. The reverse has no counterpart: demotion leaves the
	// agent row alone, because the person's state history and filed wrap-ups
	// hang off it.
	if account.AgentID == nil && (account.Role == "AGENT" || account.Role == "SUPERVISOR") {
		account, err = s.provisionPhoneFor(r.Context(), account)
		if err != nil {
			s.writeAccountError(w, r, err)
			return
		}
		s.mirrorNewAgent(r.Context(), account)
	}
	writeJSON(w, http.StatusOK, accountOut(account))
}

// ResetUserPassword sets somebody else's password and ends their sessions.
func (s *Server) ResetUserPassword(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	if s.auth == nil || s.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot set passwords", nil)
		return
	}
	var in api.PasswordReset
	if !decode(w, r, &in) {
		return
	}
	if len(in.Password) < 8 {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a password is at least 8 characters", map[string]any{"field": "password", "rule": "PASSWORD_MIN_8"})
		return
	}
	// Asked for first, so resetting an account that is not there is a 404
	// rather than a success nobody can see the effect of.
	if _, err := s.accounts.Get(r.Context(), userID); err != nil {
		s.writeAccountError(w, r, err)
		return
	}
	if err := s.auth.SetPassword(r.Context(), userID, in.Password); err != nil {
		s.writeAccountError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// provisionPhoneFor gives a promoted account the identity and phone it lacks.
func (s *Server) provisionPhoneFor(ctx context.Context, account store.Account) (store.Account, error) {
	sipPassword, err := newSIPPassword()
	if err != nil {
		return store.Account{}, err
	}
	low, high, err := s.cfg.ExtensionPool()
	if err != nil {
		return store.Account{}, err
	}
	return s.accounts.EnsureAgentIdentity(ctx, account.UserID, store.NewAccount{
		DisplayName:    account.DisplayName,
		CallcenterName: callcenterNameFor(account.Username),
		SIPPassword:    sipPassword,
		ExtensionLow:   low,
		ExtensionHigh:  high,
	})
}

// mirrorNewAgent tells the switch about an agent that was written straight to
// the database.
//
// Provisioning writes three rows in one transaction rather than going through
// the agents service, so the mirror the service would have done has to happen
// here. Without it the agent exists in the product and not in mod_callcenter,
// and nothing says so until a queue fails to offer them a call.
func (s *Server) mirrorNewAgent(ctx context.Context, account store.Account) {
	if s.agents == nil || account.AgentID == nil {
		return
	}
	s.agents.MirrorAgent(ctx, *account.AgentID)
}

func accountOut(a store.Account) api.User {
	out := api.User{
		UserID:      a.UserID,
		Username:    a.Username,
		DisplayName: a.DisplayName,
		Role:        api.Role(a.Role),
		Status:      api.UserStatus(a.Status),
	}
	// Absent rather than empty: an administrator has no agent identity at all,
	// and an empty string would read as one that is merely unnamed.
	if a.AgentID != nil {
		id := *a.AgentID
		out.AgentID = &id
	}
	if a.CallcenterName != "" {
		name := a.CallcenterName
		out.CallcenterName = &name
	}
	if a.ExtensionNumber != "" {
		number := a.ExtensionNumber
		out.ExtensionNumber = &number
	}
	return out
}

// callcenterNameFor derives the switch-side name from the login name.
//
// Stamped once and never renamed: mod_callcenter's tiers, the switch's own
// records and every historical row know an agent by this string, so renaming
// the account must not rename it underneath them.
func callcenterNameFor(username string) string { return "agent-" + username }

// newSIPPassword mints a phone credential nobody has to invent.
//
// It is never returned by any write: an operator reads it back through the
// extension's own reveal endpoint, which is ADMIN-only and audited (D5). Long
// enough that it is not worth guessing, and unpadded base32 so it survives
// being typed into a phone's configuration by hand.
func newSIPPassword() (string, error) {
	var raw [15]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]), nil
}

func (s *Server) writeAccountError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrUsernameTaken):
		writeError(w, http.StatusConflict, CodeConflict, "that username is already taken", nil)
	case errors.Is(err, store.ErrNoSuchAccount):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such account", nil)
	case errors.Is(err, store.ErrLastAdmin):
		// Its own code, because the operator can act on it: promote somebody
		// else first. A generic conflict reads as "try again".
		writeError(w, http.StatusConflict, CodeLastAdmin,
			"the last active administrator cannot be demoted or suspended", nil)
	case errors.Is(err, catalog.ErrPoolExhausted):
		// Also its own: nothing collided, the deployment has run out of
		// numbers and the answer is a wider AICC_EXTENSION_RANGE.
		writeError(w, http.StatusConflict, CodeExtensionPoolExhausted,
			"every number in the extension range is taken", nil)
	case errors.Is(err, auth.ErrValidation):
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed, err.Error(), nil)
	default:
		slog.ErrorContext(r.Context(), "account request failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot complete the change", nil)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// RevealExtensionPassword answers what a phone was given, and records that it
// was asked.
//
// The password is stored in clear (D4), so this endpoint is the whole of the
// exposure and the whole of the control. It is audited from the handler rather
// than by the middleware: that middleware guarantees coverage of *mutations*,
// structurally, and a read that discloses a credential is a different category
// of event — the handler that discloses is the one that knows it did.
func (s *Server) RevealExtensionPassword(w http.ResponseWriter, r *http.Request, extensionID uuid.UUID) {
	if s.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot read the phone", nil)
		return
	}
	password, err := s.catalog.ExtensionPassword(r.Context(), extensionID)
	if err != nil {
		s.writeCatalogError(w, r, err)
		return
	}

	// Recorded before the answer leaves: a disclosure whose record failed is
	// still a disclosure, and the log line says so loudly.
	if s.auditor != nil {
		var actorID *uuid.UUID
		if identity, ok := identityFrom(r.Context()); ok {
			id := identity.UserID
			actorID = &id
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if err := s.auditor.Audit(r.Context(), actorID,
			"GET "+routePattern(r), "extension", extensionID.String(), nil, ip); err != nil {
			s.logAuditFailure(r, err)
		}
	}
	writeJSON(w, http.StatusOK, api.ExtensionSecret{Password: password})
}
