// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// CallService is the call-control surface used by the API.
type CallService interface {
	Answer(ctx context.Context, callID, agentID uuid.UUID) error
	Hold(ctx context.Context, callID, agentID uuid.UUID) error
	Retrieve(ctx context.Context, callID, agentID uuid.UUID) error
	Mute(ctx context.Context, callID, agentID uuid.UUID) error
	Unmute(ctx context.Context, callID, agentID uuid.UUID) error
	Hangup(ctx context.Context, callID, agentID uuid.UUID) error
	Transfer(ctx context.Context, callID, agentID uuid.UUID, destination string) error
	SendDTMF(ctx context.Context, callID, agentID uuid.UUID, digits string) error
	CallsForAgent(agentID uuid.UUID) []telephony.Snapshot
	AllCalls() []telephony.Snapshot
	// WaitingCalls is the queue's own view: who has joined and not yet
	// reached anybody. It cannot be derived from the calls above — a caller
	// on hold music in a queue looks like any other live call.
	WaitingCalls(queueIDs []uuid.UUID) []telephony.WaitingCall
	// AllWaitingCalls is the same view across every queue, for whoever
	// watches the whole floor rather than working one line of it.
	AllWaitingCalls() []telephony.WaitingCall
}

// ListMyCalls lists the calls the caller is currently a party to.
func (s *Server) ListMyCalls(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.calls.CallsForAgent(agentID)})
}

// ListWaitingCalls lists the callers waiting in the queues this agent staffs,
// or every queue for a supervisor.
//
// An agent's queues come from their staffing, not from the request: they work
// the line they are on, and the same rule already decides which queue events
// reach their event stream. A supervisor works no line and watches all of
// them, which is the same split ListCalls already makes.
func (s *Server) ListWaitingCalls(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return
	}
	if id.Role.AtLeast(auth.RoleSupervisor) {
		writeJSON(w, http.StatusOK, map[string]any{"items": s.calls.AllWaitingCalls()})
		return
	}

	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	queueIDs, err := s.agentDir.QueuesForAgent(r, agentID)
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot resolve staffed queues", "error", err, "agentId", agentID)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read your queues", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.calls.WaitingCalls(queueIDs)})
}

// ListCalls lists every live call, for supervision.
func (s *Server) ListCalls(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.calls.AllCalls()})
}

//
// The control operations themselves. Each one is the contract operation, and
// each acts on the call id the wrapper already parsed.
//

func (s *Server) AnswerCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	s.callOp(w, r, callID, s.calls.Answer)
}

func (s *Server) HoldCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	s.callOp(w, r, callID, s.calls.Hold)
}

func (s *Server) RetrieveCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	s.callOp(w, r, callID, s.calls.Retrieve)
}

func (s *Server) MuteCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	s.callOp(w, r, callID, s.calls.Mute)
}

func (s *Server) UnmuteCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	s.callOp(w, r, callID, s.calls.Unmute)
}

func (s *Server) HangupCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	s.callOp(w, r, callID, s.calls.Hangup)
}

func (s *Server) SendCallDTMF(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	var req api.DTMFRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.Digits == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"digits are required", map[string]any{"field": "digits"})
		return
	}
	s.callOp(w, r, callID, func(ctx context.Context, callID, agentID uuid.UUID) error {
		return s.calls.SendDTMF(ctx, callID, agentID, req.Digits)
	})
}

func (s *Server) TransferCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	var req api.TransferRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil ||
		req.Destination == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"destination is required", map[string]any{"field": "destination"})
		return
	}
	s.callOp(w, r, callID, func(ctx context.Context, callID, agentID uuid.UUID) error {
		return s.calls.Transfer(ctx, callID, agentID, req.Destination)
	})
}

// callOp runs one call operation on behalf of the calling agent. The call id
// arrives already parsed by the generated wrapper.
//
// Every operation acts through the caller's own agent identity, so an agent
// can only control a call they are actually part of: the call id in the path
// is never enough on its own.
func (s *Server) callOp(w http.ResponseWriter, r *http.Request, callID uuid.UUID, run func(context.Context, uuid.UUID, uuid.UUID) error) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}

	switch err := run(r.Context(), callID, agentID); {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, telephony.ErrCallNotFound):
		writeError(w, http.StatusNotFound, CodeCallNotFound, "no such call", nil)
	case errors.Is(err, telephony.ErrNoAgentLeg), errors.Is(err, telephony.ErrNotCallParty):
		writeError(w, http.StatusForbidden, CodeNotCallParty, "you are not on this call", nil)
	case errors.Is(err, telephony.ErrInvalidDTMF):
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"digits must be 0-9, A-D, * or #", map[string]any{"field": "digits"})
	case errors.Is(err, esl.ErrDown):
		writeError(w, http.StatusServiceUnavailable, CodeSwitchDown, "the switch is unreachable", nil)
	default:
		slog.ErrorContext(r.Context(), "call operation failed", "error", err, "callId", callID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "the switch rejected the request", nil)
	}
}

// requireSupervisorRole is a readability alias at the route table.
var requireSupervisorRole = requireRole(auth.RoleSupervisor)
