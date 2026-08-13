// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

// AgentDirectory resolves the agent behind a signed-in user.
type AgentDirectory interface {
	AgentIDForUser(r *http.Request, userID uuid.UUID) (uuid.UUID, error)
}

type presenceResponse struct {
	State           agents.State        `json:"state"`
	Reason          agents.Reason       `json:"reason,omitempty"`
	Availability    agents.Availability `json:"availability"`
	ExtensionNumber string              `json:"extensionNumber,omitempty"`
	EnteredAt       string              `json:"enteredAt"`
	WrapUpEndsAt    *string             `json:"wrapUpEndsAt,omitempty"`
}

func presenceOf(p agents.Presence) presenceResponse {
	out := presenceResponse{
		State:           p.CurrentState(),
		Reason:          p.Reason,
		Availability:    p.Availability(),
		ExtensionNumber: p.ExtensionNumber,
		EnteredAt:       p.EnteredAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if !p.WrapUpEndsAt.IsZero() {
		ends := p.WrapUpEndsAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		out.WrapUpEndsAt = &ends
	}
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

func (s *Server) handleAgentLogin(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	var req struct {
		ExtensionNumber string `json:"extensionNumber"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.ExtensionNumber == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"extensionNumber is required", map[string]any{"field": "extensionNumber"})
		return
	}

	p, err := s.agents.Login(r.Context(), agentID, req.ExtensionNumber)
	s.writePresence(w, r, p, err)
}

func (s *Server) handleAgentLogout(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	p, err := s.agents.Logout(r.Context(), agentID)
	s.writePresence(w, r, p, err)
}

func (s *Server) handleAgentReady(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	p, err := s.agents.Ready(r.Context(), agentID)
	s.writePresence(w, r, p, err)
}

func (s *Server) handleAgentNotReady(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleAgentPresence(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, presenceOf(s.agents.Presence(agentID)))
}

func (s *Server) handleAgentRoster(w http.ResponseWriter, r *http.Request) {
	rows, err := s.agents.Roster(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "roster failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot read the roster", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// handleAgentForceLogout lets a supervisor sign somebody else out, which is
// how an abandoned phone stops absorbing calls.
func (s *Server) handleAgentForceLogout(w http.ResponseWriter, r *http.Request) {
	agentID, err := uuid.Parse(chi.URLParam(r, "agentId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "invalid agent id",
			map[string]any{"field": "agentId"})
		return
	}
	p, err := s.agents.Logout(r.Context(), agentID)
	s.writePresence(w, r, p, err)
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
