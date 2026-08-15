// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// The server implements the generated contract interface: an operation added
// to docs/openapi.json fails this build until the server grows its method.
// This is the compile-time half of spec-first; `make api-check` is the other.
var _ api.ServerInterface = (*Server)(nil)

// apiWrapper mounts contract operations on the route table with the generated
// parameter binding in front. Routes migrate to it group by group; the ones
// still parsing their own parameters keep their handle* registrations.
func (s *Server) apiWrapper() *api.ServerInterfaceWrapper {
	return &api.ServerInterfaceWrapper{
		Handler:          s,
		ErrorHandlerFunc: writeParamError,
	}
}

// writeParamError translates generated binding failures into the standard
// envelope, so a wrapper-mounted route rejects a bad identifier exactly like
// a hand-parsed one: 400 VALIDATION_FAILED naming the field.
func writeParamError(w http.ResponseWriter, _ *http.Request, err error) {
	field := ""
	var invalidFormat *api.InvalidParamFormatError
	var requiredParam *api.RequiredParamError
	var requiredHeader *api.RequiredHeaderError
	var unmarshaling *api.UnmarshalingParamError
	var tooMany *api.TooManyValuesForParamError
	switch {
	case errors.As(err, &invalidFormat):
		field = invalidFormat.ParamName
	case errors.As(err, &requiredParam):
		field = requiredParam.ParamName
	case errors.As(err, &requiredHeader):
		field = requiredHeader.ParamName
	case errors.As(err, &unmarshaling):
		field = unmarshaling.ParamName
	case errors.As(err, &tooMany):
		field = tooMany.ParamName
	}
	var params map[string]any
	if field != "" {
		params = map[string]any{"field": field}
	}
	writeError(w, http.StatusBadRequest, CodeValidationFailed, "invalid identifier", params)
}

//
// Auth, identity and system.
//

func (s *Server) Login(w http.ResponseWriter, r *http.Request)  { s.handleLogin(w, r) }
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) { s.handleLogout(w, r) }
func (s *Server) GetMe(w http.ResponseWriter, r *http.Request)  { s.handleMe(w, r) }

func (s *Server) GetSystemHealth(w http.ResponseWriter, r *http.Request) { s.handleHealth(w, r) }

func (s *Server) StreamEvents(w http.ResponseWriter, r *http.Request, _ api.StreamEventsParams) {
	s.handleEvents(w, r)
}

//
// Agent presence.
//

func (s *Server) GetAgentPresence(w http.ResponseWriter, r *http.Request) {
	s.handleAgentPresence(w, r)
}
func (s *Server) AgentLogin(w http.ResponseWriter, r *http.Request)    { s.handleAgentLogin(w, r) }
func (s *Server) AgentLogout(w http.ResponseWriter, r *http.Request)   { s.handleAgentLogout(w, r) }
func (s *Server) AgentReady(w http.ResponseWriter, r *http.Request)    { s.handleAgentReady(w, r) }
func (s *Server) AgentNotReady(w http.ResponseWriter, r *http.Request) { s.handleAgentNotReady(w, r) }
func (s *Server) ListAgents(w http.ResponseWriter, r *http.Request)    { s.handleAgentRoster(w, r) }

func (s *Server) ForceLogoutAgent(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleAgentForceLogout(w, r)
}

func (s *Server) CreateAgent(w http.ResponseWriter, r *http.Request) { s.handleCreateAgent(w, r) }

func (s *Server) UpdateAgent(w http.ResponseWriter, r *http.Request, agentID uuid.UUID) {
	s.handleUpdateAgent(w, r, agentID)
}

func (s *Server) DeleteAgent(w http.ResponseWriter, r *http.Request, agentID uuid.UUID) {
	s.handleDeleteAgent(w, r, agentID)
}

func (s *Server) ListUsers(w http.ResponseWriter, r *http.Request) { s.handleListUsers(w, r) }

//
// Live calls and call control. These run behind the generated wrapper: the
// call id arrives parsed, and the request and response bodies are the
// generated contract types.
//

func (s *Server) ListMyCalls(w http.ResponseWriter, r *http.Request) { s.handleMyCalls(w, r) }
func (s *Server) ListCalls(w http.ResponseWriter, r *http.Request)   { s.handleAllCalls(w, r) }

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

//
// Dialing out.
//

func (s *Server) DialCall(w http.ResponseWriter, r *http.Request) {
	identity, _ := identityFrom(r.Context())
	agentID, err := s.agentDir.AgentIDForUser(r, identity.UserID)
	if err != nil {
		writeError(w, http.StatusForbidden, CodeForbidden, "no agent profile", nil)
		return
	}
	presence := s.agents.Presence(agentID)
	if presence.ExtensionNumber == "" {
		writeError(w, http.StatusConflict, CodeConflict, "sign in to a phone first", nil)
		return
	}

	var req api.DialRequest
	if !decode(w, r, &req) {
		return
	}
	callID, err := s.outbound.Dial(r.Context(), presence.ExtensionNumber, req.Destination)
	if err != nil {
		writeOutboundError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.DialResponse{CallID: callID})
}

func (s *Server) CreateCall(w http.ResponseWriter, r *http.Request) {
	var req api.CreateCallRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Kind != api.CreateCallRequestKindAIOUTBOUND {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "kind must be AI_OUTBOUND",
			map[string]any{"allowed": []string{string(api.CreateCallRequestKindAIOUTBOUND)}})
		return
	}

	dial := outbound.AIDialRequest{To: req.To}
	if req.DID != nil {
		dial.DIDNumber = *req.DID
	}
	if req.Language != nil {
		dial.Language = *req.Language
	}
	if req.CallID != nil {
		dial.CallID = *req.CallID
	}

	callID, err := s.outbound.DialAI(r.Context(), dial)
	if err != nil {
		if errors.Is(err, outbound.ErrAlreadyPlaced) {
			// The retry did its job: the call exists. Point at it.
			isDuplicate := true
			writeJSON(w, http.StatusOK, api.CreateCallResponse{CallID: callID, IsDuplicate: &isDuplicate})
			return
		}
		writeOutboundError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.CreateCallResponse{CallID: callID})
}

//
// Catalog. Still on the hand-parsed handlers; the wrapper takes over as each
// group migrates to the generated types.
//

func (s *Server) ListExtensions(w http.ResponseWriter, r *http.Request) { s.handleListExtensions(w, r) }
func (s *Server) CreateExtension(w http.ResponseWriter, r *http.Request) {
	s.handleCreateExtension(w, r)
}

func (s *Server) UpdateExtension(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleUpdateExtension(w, r)
}

func (s *Server) DeleteExtension(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleDeleteExtension(w, r)
}

func (s *Server) ListQueues(w http.ResponseWriter, r *http.Request)  { s.handleListQueues(w, r) }
func (s *Server) CreateQueue(w http.ResponseWriter, r *http.Request) { s.handleCreateQueue(w, r) }

func (s *Server) UpdateQueue(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleUpdateQueue(w, r)
}

func (s *Server) DeleteQueue(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleDeleteQueue(w, r)
}

func (s *Server) ListQueueAgents(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleListQueueAgents(w, r)
}

func (s *Server) StaffQueue(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleStaffQueue(w, r)
}

func (s *Server) UnstaffQueue(w http.ResponseWriter, r *http.Request, _ uuid.UUID, _ uuid.UUID) {
	s.handleUnstaffQueue(w, r)
}

func (s *Server) ListDIDs(w http.ResponseWriter, r *http.Request)  { s.handleListDIDs(w, r) }
func (s *Server) CreateDID(w http.ResponseWriter, r *http.Request) { s.handleCreateDID(w, r) }

func (s *Server) UpdateDID(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleUpdateDID(w, r)
}

func (s *Server) DeleteDID(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleDeleteDID(w, r)
}

//
// Ledger, recordings, reports and callbacks.
//

func (s *Server) ListCDRs(w http.ResponseWriter, r *http.Request, _ api.ListCDRsParams) {
	s.handleListCDRs(w, r)
}

func (s *Server) GetCDR(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleGetCDR(w, r)
}

func (s *Server) ListCallRecordings(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleCallRecordings(w, r)
}

func (s *Server) ListCallReviews(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleCallReviews(w, r)
}

func (s *Server) GetRecordingAudio(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleRecordingAudio(w, r)
}

func (s *Server) CreateRecordingReview(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleCreateReview(w, r)
}

func (s *Server) GetReportOverview(w http.ResponseWriter, r *http.Request, _ api.GetReportOverviewParams) {
	s.handleReportOverview(w, r)
}

func (s *Server) GetReportQueues(w http.ResponseWriter, r *http.Request, _ api.GetReportQueuesParams) {
	s.handleReportQueues(w, r)
}

func (s *Server) GetReportDaily(w http.ResponseWriter, r *http.Request, _ api.GetReportDailyParams) {
	s.handleReportDaily(w, r)
}

func (s *Server) ListCallbacks(w http.ResponseWriter, r *http.Request, _ api.ListCallbacksParams) {
	s.handleListCallbacks(w, r)
}

func (s *Server) ClaimCallback(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleClaimCallback(w, r)
}

func (s *Server) CompleteCallback(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	s.handleCompleteCallback(w, r)
}
