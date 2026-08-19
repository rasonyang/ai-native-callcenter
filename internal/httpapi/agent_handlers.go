// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// AgentDirectory resolves the agent behind a signed-in user.
type AgentDirectory interface {
	AgentIDForUser(r *http.Request, userID uuid.UUID) (uuid.UUID, error)
	// QueuesForAgent lists the queues the agent staffs. The event stream needs
	// it to decide which queue-scoped events reach this subscriber.
	QueuesForAgent(r *http.Request, agentID uuid.UUID) ([]uuid.UUID, error)
}

// presenceOf renders presence on the contract type.
//
// Timestamps are truncated to the second: presence moves in whole seconds and
// a screen renders it that way, so the sub-second digits would be noise on
// every event.
func presenceOf(p agents.Presence) api.Presence {
	out := api.Presence{
		State:        api.AgentState(p.CurrentState()),
		Availability: api.Availability(p.Availability()),
		EnteredAt:    p.EnteredAt.UTC().Truncate(time.Second),
	}
	if p.Reason != "" {
		reason := api.NotReadyReason(p.Reason)
		out.Reason = &reason
	}
	if p.ExtensionNumber != "" {
		out.ExtensionNumber = &p.ExtensionNumber
	}
	out.WrapUpCallID = p.WrapUpCallID
	return out
}

// agentIDFor resolves the caller's own agent identity.
func (s *Server) agentIDFor(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := identityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return uuid.Nil, false
	}
	agentID, err := s.agentDir.AgentIDForUser(r, id.UserID)
	if err != nil {
		writeError(w, http.StatusForbidden, CodeForbidden, "this account is not an agent", nil)
		return uuid.Nil, false
	}
	return agentID, true
}

func (s *Server) AgentLogin(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	// The body is optional: an agent signs in at the extension configuration
	// bound to them, and only names one to override it.
	var req struct {
		ExtensionNumber string `json:"extensionNumber"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
			return
		}
	}

	p, err := s.agents.Login(r.Context(), agentID, req.ExtensionNumber)
	s.writePresence(w, r, p, err)
}

func (s *Server) AgentLogout(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	p, err := s.agents.Logout(r.Context(), agentID)
	s.writePresence(w, r, p, err)
}

func (s *Server) AgentReady(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	p, err := s.agents.Ready(r.Context(), agentID)
	s.writePresence(w, r, p, err)
}

func (s *Server) AgentNotReady(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason agents.Reason `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"reason is required", map[string]any{"field": "reason"})
		return
	}
	if req.Reason == "" {
		req.Reason = agents.ReasonBreak
	}

	p, err := s.agents.NotReady(r.Context(), agentID, req.Reason)
	s.writePresence(w, r, p, err)
}

// AgentWrapUp files the after-call work for the call the agent just finished
// and returns them to ready.
//
// The call comes from the platform, never from the request: an agent files
// against the call they were on, and letting a client name one would let any
// agent write a disposition onto any call. The filing is also accepted after
// they have moved on — an agent still typing when something else took them out
// of after-call work has not forfeited what they typed — so the last wrapped
// call stays addressable until the next one begins.
//
// The disposition is required, and this is where that is enforced: it is what
// makes a call reportable, and a screen is not a place to keep a rule.
func (s *Server) AgentWrapUp(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	var req api.WrapUpRequest
	if !decode(w, r, &req) {
		return
	}
	code := strings.TrimSpace(req.DispositionCode)
	if code == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"a disposition is required", map[string]any{"field": "dispositionCode"})
		return
	}

	callID, hasCall := s.agents.WrapUpCall(agentID)
	if !hasCall {
		writeError(w, http.StatusConflict, CodeAgentNotInWrapUp,
			"there is no finished call to file this against", nil)
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot file the wrap-up", nil)
		return
	}

	note := ""
	if req.Note != nil {
		note = strings.TrimSpace(*req.Note)
	}
	if _, err := s.ledger.FileWrapUp(r.Context(), callID, agentID, code, note); err != nil {
		if errors.Is(err, store.ErrUnknownDisposition) {
			writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
				"no such disposition", map[string]any{"field": "dispositionCode"})
			return
		}
		slog.ErrorContext(r.Context(), "wrap-up not recorded",
			"error", err, "callId", callID, "agentId", agentID)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot file the wrap-up", nil)
		return
	}

	p, err := s.agents.EndWrapUp(r.Context(), agentID)
	s.writePresence(w, r, p, err)
}

func (s *Server) GetAgentPresence(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, presenceOf(s.agents.Presence(agentID)))
}

func (s *Server) ListAgents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.agents.Roster(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "roster failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot read the roster", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// ForceLogoutAgent lets a supervisor sign somebody else out, which is how an
// abandoned phone stops absorbing calls.
func (s *Server) ForceLogoutAgent(w http.ResponseWriter, r *http.Request, agentID uuid.UUID) {
	p, err := s.agents.Logout(r.Context(), agentID)
	s.writePresence(w, r, p, err)
}

//
// Agent configuration. Administration owns the agent↔extension binding: it is
// static, so it is edited here rather than chosen at sign-in.
//

// CreateAgent gives an account an agent identity.
func (s *Server) CreateAgent(w http.ResponseWriter, r *http.Request) {
	var in api.AgentWrite
	if !decode(w, r, &in) {
		return
	}
	if in.UserID == nil {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"userId is required", map[string]any{"field": "userId"})
		return
	}
	cfg, err := s.agents.CreateAgent(r.Context(), agentConfigFrom(uuid.Nil, *in.UserID, in))
	s.writeAgentConfig(w, r, cfg, err, http.StatusCreated)
}

// UpdateAgent rewrites one agent's configuration.
func (s *Server) UpdateAgent(w http.ResponseWriter, r *http.Request, agentID uuid.UUID) {
	var in api.AgentWrite
	if !decode(w, r, &in) {
		return
	}
	// The account behind an agent identity never changes; only what the
	// switch needs to reach them does.
	cfg, err := s.agents.UpdateAgent(r.Context(), agentConfigFrom(agentID, uuid.Nil, in))
	s.writeAgentConfig(w, r, cfg, err, http.StatusOK)
}

// DeleteAgent removes an agent identity, leaving the account alone.
func (s *Server) DeleteAgent(w http.ResponseWriter, r *http.Request, agentID uuid.UUID) {
	if err := s.agents.DeleteAgent(r.Context(), agentID); err != nil {
		s.writeAgentConfigError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func agentConfigFrom(agentID, userID uuid.UUID, in api.AgentWrite) agents.AgentConfig {
	cfg := agents.AgentConfig{
		AgentID:            agentID,
		UserID:             userID,
		CallcenterName:     in.CallcenterName,
		DefaultExtensionID: in.DefaultExtensionID,
	}
	if in.IsAutoAnswer != nil {
		cfg.IsAutoAnswer = *in.IsAutoAnswer
	}
	return cfg
}

func (s *Server) writeAgentConfig(
	w http.ResponseWriter, r *http.Request, cfg agents.AgentConfig, err error, status int,
) {
	if err != nil {
		s.writeAgentConfigError(w, r, err)
		return
	}
	writeJSON(w, status, api.Agent{
		AgentID:            cfg.AgentID,
		UserID:             cfg.UserID,
		CallcenterName:     cfg.CallcenterName,
		IsAutoAnswer:       cfg.IsAutoAnswer,
		DefaultExtensionID: cfg.DefaultExtensionID,
	})
}

// writeAgentConfigError maps configuration failures onto the API vocabulary. A
// unique violation means the operator bound a phone somebody already has, or
// gave an account a second agent identity.
func (s *Server) writeAgentConfigError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agents.ErrValidation):
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed, err.Error(), nil)
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, CodeNotFound, "no such agent", nil)
	case isUniqueViolation(err):
		writeError(w, http.StatusConflict, CodeConflict,
			"that extension or account already has an agent", nil)
	default:
		slog.ErrorContext(r.Context(), "agent configuration failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot save the agent", nil)
	}
}

// handleListUsers lists accounts so an agent identity can be attached to one.
func (s *Server) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.auth.ListUsers(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list users failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot list accounts", nil)
		return
	}
	items := make([]api.User, 0, len(users))
	for _, u := range users {
		items = append(items, api.User{
			UserID:      u.UserID,
			Username:    u.Username,
			DisplayName: u.DisplayName,
			Role:        api.Role(u.Role),
		})
	}
	writeJSON(w, http.StatusOK, api.UserList{Items: items})
}

// writePresence maps service errors onto the API error vocabulary.
func (s *Server) writePresence(w http.ResponseWriter, r *http.Request, p agents.Presence, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, presenceOf(p))
	case errors.Is(err, agents.ErrUnknownAgent):
		writeError(w, http.StatusForbidden, CodeForbidden, "this account is not an agent", nil)
	case errors.Is(err, agents.ErrExtensionInUse):
		writeError(w, http.StatusConflict, CodeExtensionInUse,
			"another agent is signed in at that extension", nil)
	case errors.Is(err, agents.ErrNoExtensionBound):
		writeError(w, http.StatusConflict, CodeConflict,
			"no extension is bound to this agent", nil)
	case errors.Is(err, agents.ErrAlreadyLoggedIn):
		writeError(w, http.StatusConflict, CodeAgentAlreadyLoggedIn, "already signed in", nil)
	case errors.Is(err, agents.ErrNotLoggedIn):
		writeError(w, http.StatusConflict, CodeAgentNotLoggedIn, "not signed in", nil)
	case errors.Is(err, agents.ErrUnknownReason):
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"unknown not-ready reason", map[string]any{"field": "reason"})
	case errors.Is(err, agents.ErrStorage):
		slog.ErrorContext(r.Context(), "agent state not recorded", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown,
			"cannot record the change", nil)
	default:
		slog.ErrorContext(r.Context(), "agent request failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "unexpected error", nil)
	}
}

// requireAgentRole is a readability alias at the route table.
var requireAgentRole = requireRole(auth.RoleAgent)
