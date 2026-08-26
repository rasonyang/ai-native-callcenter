// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// EndCall ends the whole conversation, for a caller with no leg of their
	// own to leave. Hangup is the agent's operation and takes an agent; this
	// one takes none, because there is nobody on the call to name.
	EndCall(ctx context.Context, callID uuid.UUID) error
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
	// PatchUserData merges business data into a live call and answers with
	// the result and what moved. All of the patch lands or none of it does.
	PatchUserData(callID, agentID uuid.UUID, patch map[string]any) (
		map[string]any, telephony.UserDataChange, error)
}

// PatchUserData attaches business data to a call that is already running.
//
// The order number a backend resolved after the call arrived, the case the
// agent opened while talking: things nobody could have known when the call was
// placed, which is the only reason this exists separately from the userData
// the two dial endpoints already take.
//
// RFC 7386, so null removes a key. That is why the patch's values are
// *string here and plain strings on the way in: a JSON null and an absent key
// are different instructions, and only a pointer can tell them apart.
func (s *Server) PatchUserData(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	var req api.PatchUserDataRequest
	if !decode(w, r, &req) {
		return
	}
	// The shape of the patch is refused here, where refusing costs nothing and
	// the answer is exact: a value too long, or more keys than a call may hold
	// at all. What cannot be judged without the call — whether *this* call has
	// room — is the merge's to refuse, and it does so without writing.
	if err := checkUserDataPatch(req.UserData); err != nil {
		writeError(w, http.StatusBadRequest, CodeUserDataTooLarge, err.Error(),
			map[string]any{"maxKeys": userDataMaxKeys, "maxValueBytes": userDataMaxValueBytes})
		return
	}

	patch := make(map[string]any, len(req.UserData))
	for k, v := range req.UserData {
		if v == nil {
			patch[k] = nil
			continue
		}
		patch[k] = *v
	}

	result, change, err := s.calls.PatchUserData(callID, agentID, patch)
	switch {
	case err == nil:
	case errors.Is(err, telephony.ErrCallNotFound):
		writeError(w, http.StatusNotFound, CodeCallNotFound, "no such call", nil)
		return
	case errors.Is(err, telephony.ErrNotCallParty):
		writeError(w, http.StatusForbidden, CodeNotCallParty, "you are not on this call", nil)
		return
	case errors.Is(err, telephony.ErrUserDataWouldNotFit):
		// wouldNotFit names the keys that were over the line, which is what a
		// client needs to build a retry that works — not every key it sent.
		// Nothing was written either way, and the message says so.
		writeError(w, http.StatusConflict, CodeUserDataTooLarge,
			"this call has no room for all of these keys, and none of them were written",
			map[string]any{"maxKeys": userDataMaxKeys, "wouldNotFit": change.Dropped})
		return
	default:
		slog.ErrorContext(r.Context(), "patching business data failed", "error", err, "callId", callID)
		writeError(w, http.StatusInternalServerError, CodeInternal, "could not attach the business data", nil)
		return
	}

	writeJSON(w, http.StatusOK, api.UserDataResponse{
		UserData:    userDataForWire(result),
		ChangedKeys: emptyIfNil(change.Changed),
		DeletedKeys: emptyIfNil(change.Deleted),
	})
}

// userDataForWire narrows the call's map to the contract's shape. Values are
// strings by contract and by every path that writes them; anything else could
// only come from a future writer that broke that rule, and rendering it with
// %v here would let it out onto the wire looking legitimate.
func userDataForWire(data map[string]any) api.UserData {
	out := make(api.UserData, len(data))
	for k, v := range data {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// emptyIfNil keeps a required array out of the response as [] rather than
// null: the contract says the field is always there, and a client iterating it
// should not have to check.
func emptyIfNil(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}

// checkUserDataPatch applies the same bounds as checkUserData to a patch,
// where a null value is a deletion rather than a value to measure.
func checkUserDataPatch(patch api.UserDataPatch) error {
	if len(patch) > userDataMaxKeys {
		return fmt.Errorf("userData has %d keys, at most %d are accepted",
			len(patch), userDataMaxKeys)
	}
	for k, v := range patch {
		if v == nil {
			continue
		}
		if len(*v) > userDataMaxValueBytes {
			return fmt.Errorf("userData[%q] is %d bytes, at most %d are accepted",
				k, len(*v), userDataMaxValueBytes)
		}
	}
	return nil
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

// HangupCall ends a leg or a call, depending on what the caller has.
//
// An agent has a leg and ends that; a supervisor, an administrator and the API
// key have none, so the only ending available to them is the call's. One verb
// rather than two, because "hang up" is what both are asking for and the
// difference is entirely in what the asker is holding — the same reading that
// makes an agent's dial and a system's dial one POST /calls.
//
// Ending any call rather than only ones the caller placed. The key already
// acts as a supervisor, and a supervisor may end any call on the floor; a
// narrower rule would need every call to record who ordered it, which nothing
// does today. It is worth saying out loud: this widens what a leaked key can
// do from placing calls to ending them.
func (s *Server) HangupCall(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	identity, ok := identityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return
	}
	// An agent is anybody with a leg, so the profile decides this rather than
	// the role: a supervisor who is also staffed as an agent and is on the
	// call leaves their own leg, which is what they meant.
	if _, isAgent := s.signedInAgent(r, identity); isAgent {
		s.callOp(w, r, callID, s.calls.Hangup)
		return
	}
	if !identity.Role.AtLeast(auth.RoleSupervisor) {
		writeError(w, http.StatusForbidden, CodeForbidden, "this account is not an agent", nil)
		return
	}
	s.callResult(w, r, callID, s.calls.EndCall(r.Context(), callID))
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
	s.callResult(w, r, callID, run(r.Context(), callID, agentID))
}

// callResult turns what the switch said into the one answer every call
// operation gives. Split from callOp because not every such operation is an
// agent's: ending a call takes no agent, and it still owes the caller the same
// vocabulary of failures.
func (s *Server) callResult(w http.ResponseWriter, r *http.Request, callID uuid.UUID, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, telephony.ErrCallNotFound):
		writeError(w, http.StatusNotFound, CodeCallNotFound, "no such call", nil)
	case errors.Is(err, telephony.ErrNoAgentLeg), errors.Is(err, telephony.ErrNotCallParty):
		writeError(w, http.StatusForbidden, CodeNotCallParty, "you are not on this call", nil)
	case errors.Is(err, telephony.ErrNoExtensionLeg):
		writeError(w, http.StatusConflict, CodeConflict,
			"this call has no leg at an extension to hang up", nil)
	case errors.Is(err, telephony.ErrNotForCallType):
		writeError(w, http.StatusConflict, CodeOperationNotAllowedForCallType,
			"this is not available on an internal call", nil)
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
