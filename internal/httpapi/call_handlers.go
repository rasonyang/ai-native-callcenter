// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// CallService is the call-control surface used by the API.
type CallService interface {
	Answer(ctx context.Context, callID, agentID uuid.UUID) error
	Hold(ctx context.Context, callID, agentID uuid.UUID) error
	Retrieve(ctx context.Context, callID, agentID uuid.UUID) error
	Hangup(ctx context.Context, callID, agentID uuid.UUID) error
	Transfer(ctx context.Context, callID, agentID uuid.UUID, destination string) error
	CallsForAgent(agentID uuid.UUID) []telephony.Snapshot
	AllCalls() []telephony.Snapshot
}

// handleMyCalls lists the calls the caller is currently a party to.
func (s *Server) handleMyCalls(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.calls.CallsForAgent(agentID)})
}

// handleAllCalls lists every live call, for supervision.
func (s *Server) handleAllCalls(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.calls.AllCalls()})
}

func (s *Server) handleCallAnswer(w http.ResponseWriter, r *http.Request) {
	s.callOp(w, r, func(ctx context.Context, callID, agentID uuid.UUID) error {
		return s.calls.Answer(ctx, callID, agentID)
	})
}

func (s *Server) handleCallHold(w http.ResponseWriter, r *http.Request) {
	s.callOp(w, r, func(ctx context.Context, callID, agentID uuid.UUID) error {
		return s.calls.Hold(ctx, callID, agentID)
	})
}

func (s *Server) handleCallRetrieve(w http.ResponseWriter, r *http.Request) {
	s.callOp(w, r, func(ctx context.Context, callID, agentID uuid.UUID) error {
		return s.calls.Retrieve(ctx, callID, agentID)
	})
}

func (s *Server) handleCallHangup(w http.ResponseWriter, r *http.Request) {
	s.callOp(w, r, func(ctx context.Context, callID, agentID uuid.UUID) error {
		return s.calls.Hangup(ctx, callID, agentID)
	})
}

func (s *Server) handleCallTransfer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.Destination == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"destination is required", map[string]any{"field": "destination"})
		return
	}
	s.callOp(w, r, func(ctx context.Context, callID, agentID uuid.UUID) error {
		return s.calls.Transfer(ctx, callID, agentID, req.Destination)
	})
}

// callOp runs one call operation on behalf of the calling agent.
//
// Every operation acts through the caller's own agent identity, so an agent
// can only control a call they are actually part of: the call id in the path
// is never enough on its own.
func (s *Server) callOp(w http.ResponseWriter, r *http.Request, run func(context.Context, uuid.UUID, uuid.UUID) error) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	callID, err := uuid.Parse(chi.URLParam(r, "callId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "invalid call id",
			map[string]any{"field": "callId"})
		return
	}

	switch err := run(r.Context(), callID, agentID); {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, telephony.ErrCallNotFound):
		writeError(w, http.StatusNotFound, CodeCallNotFound, "no such call", nil)
	case errors.Is(err, telephony.ErrNoAgentLeg), errors.Is(err, telephony.ErrNotCallParty):
		writeError(w, http.StatusForbidden, CodeNotCallParty, "you are not on this call", nil)
	case errors.Is(err, esl.ErrDown):
		writeError(w, http.StatusServiceUnavailable, CodeSwitchDown, "the switch is unreachable", nil)
	default:
		slog.ErrorContext(r.Context(), "call operation failed", "error", err, "callId", callID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "the switch rejected the request", nil)
	}
}

// requireSupervisorRole is a readability alias at the route table.
var requireSupervisorRole = requireRole(auth.RoleSupervisor)
