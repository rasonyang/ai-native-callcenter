// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// OutboundService places calls on request.
type OutboundService interface {
	Dial(ctx context.Context, agentExtension, destination string) (uuid.UUID, error)
	DialAI(ctx context.Context, req outbound.AIDialRequest) (uuid.UUID, error)
}

// dialRequest is an agent's click-to-dial.
type dialRequest struct {
	Destination string `json:"destination"`
}

// handleDial rings the signed-in agent, then the destination.
func (s *Server) handleDial(w http.ResponseWriter, r *http.Request) {
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

	var req dialRequest
	if !decode(w, r, &req) {
		return
	}
	callID, err := s.outbound.Dial(r.Context(), presence.ExtensionNumber, req.Destination)
	if err != nil {
		writeOutboundError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"callId": callID})
}

// createCallRequest asks the platform to place a call. AI_OUTBOUND is the
// only kind so far: the machinery of an inbound bot call, reversed.
type createCallRequest struct {
	CallID    string `json:"callId"`
	Kind      string `json:"kind"`
	To        string `json:"to"`
	DIDNumber string `json:"did"`
	Language  string `json:"language"`
}

// handleCreateCall places an AI outbound call.
func (s *Server) handleCreateCall(w http.ResponseWriter, r *http.Request) {
	var req createCallRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Kind != "AI_OUTBOUND" {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "kind must be AI_OUTBOUND",
			map[string]any{"allowed": []string{"AI_OUTBOUND"}})
		return
	}

	dial := outbound.AIDialRequest{To: req.To, DIDNumber: req.DIDNumber, Language: req.Language}
	if req.CallID != "" {
		id, err := uuid.Parse(req.CallID)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeValidationFailed, "callId must be a uuid", nil)
			return
		}
		dial.CallID = id
	}

	callID, err := s.outbound.DialAI(r.Context(), dial)
	if err != nil {
		if errors.Is(err, outbound.ErrAlreadyPlaced) {
			// The retry did its job: the call exists. Point at it.
			writeJSON(w, http.StatusOK, map[string]any{"callId": callID, "isDuplicate": true})
			return
		}
		writeOutboundError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"callId": callID})
}

func writeOutboundError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, outbound.ErrBadNumber):
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "not a dialable number", nil)
	case errors.Is(err, outbound.ErrUnknownDID):
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "no such DID", nil)
	case errors.Is(err, outbound.ErrFlowless):
		writeError(w, http.StatusConflict, CodeConflict, "the DID has no published flow", nil)
	default:
		writeError(w, http.StatusBadGateway, CodeSwitchDown, "the switch did not take the call", nil)
	}
}
