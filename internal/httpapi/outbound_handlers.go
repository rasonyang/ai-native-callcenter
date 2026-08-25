// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// OutboundService places calls on request.
type OutboundService interface {
	Dial(ctx context.Context, agentExtension, destination string,
		userData map[string]string, callcenterName string) (uuid.UUID, error)
	DialAI(ctx context.Context, req outbound.AIDialRequest) (uuid.UUID, error)
}

// DialCall places a click-to-dial call: the agent's own phone rings first, and
// the destination is dialled only once they pick up.
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
	var userData map[string]string
	if req.UserData != nil {
		if err := checkUserData(*req.UserData); err != nil {
			writeError(w, http.StatusBadRequest, CodeUserDataTooLarge, err.Error(),
				map[string]any{"maxKeys": userDataMaxKeys, "maxValueBytes": userDataMaxValueBytes})
			return
		}
		userData = *req.UserData
	}
	// The switch's own name for this agent, so mod_callcenter can be told they
	// are on a call and stop offering them queue calls while they are.
	callID, err := s.outbound.Dial(r.Context(), presence.ExtensionNumber, req.Destination,
		userData, s.agents.CallcenterNameFor(r.Context(), agentID))
	if err != nil {
		writeOutboundError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.DialResponse{CallID: callID})
}

// CreateCall places an AI outbound call.
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

	if req.UserData != nil {
		if err := checkUserData(*req.UserData); err != nil {
			writeError(w, http.StatusBadRequest, CodeUserDataTooLarge, err.Error(),
				map[string]any{"maxKeys": userDataMaxKeys, "maxValueBytes": userDataMaxValueBytes})
			return
		}
	}

	dial := outbound.AIDialRequest{To: req.To}
	if req.UserData != nil {
		dial.UserData = *req.UserData
	}
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

// The bounds are the call's, not this layer's: telephony.MergeUserData holds
// the same two numbers, because every other way business data reaches a call —
// an inbound channel variable, a REFER, a tool the bot calls — goes through
// there and never through here.
//
// What differs is the answer. A request can be refused, so this checks first
// and returns 400 with nothing written; a phone call cannot be refused, so the
// merge drops what will not fit and reports it. Same limits, and the caller
// that has a client to answer to is the one that answers.
const (
	userDataMaxKeys       = telephony.UserDataMaxKeys
	userDataMaxValueBytes = telephony.UserDataMaxValueBytes
)

// checkUserData refuses what will not fit. Refusing is the point: truncating
// business data leaves a screen showing half a customer's details and no sign
// that the other half was ever sent.
//
// Bytes, not characters: the limit is about what is stored and shipped, and a
// Chinese value is three bytes a character where an English one is one.
//
// It counts the keys in the request, which is the resulting call's key count
// only because both endpoints here place a new call. A write against a call
// that already carries data would have to check the total instead, or it would
// answer 201 to a patch the merge then silently trimmed.
func checkUserData(data map[string]string) error {
	if len(data) > userDataMaxKeys {
		return fmt.Errorf("userData has %d keys, at most %d are accepted",
			len(data), userDataMaxKeys)
	}
	for k, v := range data {
		if len(v) > userDataMaxValueBytes {
			return fmt.Errorf("userData[%q] is %d bytes, at most %d are accepted",
				k, len(v), userDataMaxValueBytes)
		}
	}
	return nil
}

func writeOutboundError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, outbound.ErrBadNumber):
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "not a dialable number", nil)
	case errors.Is(err, outbound.ErrNoDefaultOutbound):
		// Its own answer, because the operator can act on it: mark a number as
		// the one calls go out from.
		writeError(w, http.StatusConflict, CodeConflict,
			"no number is marked as the default outbound one", nil)
	case errors.Is(err, outbound.ErrNoOutboundEndpoint):
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown,
			"this deployment has no outbound endpoint configured", nil)
	case errors.Is(err, outbound.ErrUnknownDID):
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "no such DID", nil)
	case errors.Is(err, outbound.ErrFlowless):
		writeError(w, http.StatusConflict, CodeConflict, "the DID has no published flow", nil)
	default:
		writeError(w, http.StatusBadGateway, CodeSwitchDown, "the switch did not take the call", nil)
	}
}
