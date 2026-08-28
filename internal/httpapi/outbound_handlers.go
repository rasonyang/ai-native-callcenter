// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// OutboundService places calls on request.
type OutboundService interface {
	Dial(ctx context.Context, req outbound.AgentDialRequest) (uuid.UUID, error)
	DialAI(ctx context.Context, req outbound.AIDialRequest) (uuid.UUID, error)
}

// CreateCall places a call: one entry point, told apart by kind.
//
// Both kinds create an OUTBOUND call and both are idempotent by a
// client-minted callId, so what actually differs is who is raised first — the
// customer, to be handed to the bot, or an agent's phone, to be joined to the
// customer once they pick up. That is a parameter, not an endpoint, which is
// what design 04 said before the click-to-dial path grew one of its own.
func (s *Server) CreateCall(w http.ResponseWriter, r *http.Request) {
	var req api.CreateCallRequest
	if !decode(w, r, &req) {
		return
	}
	if !req.Kind.Valid() {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "unknown kind",
			map[string]any{"allowed": []string{
				string(api.CreateCallRequestKindAIOUTBOUND),
				string(api.CreateCallRequestKindAGENTOUTBOUND),
			}})
		return
	}
	if req.UserData != nil {
		if err := checkUserData(*req.UserData); err != nil {
			writeError(w, http.StatusBadRequest, CodeUserDataTooLarge, err.Error(),
				map[string]any{"maxKeys": userDataMaxKeys, "maxValueBytes": userDataMaxValueBytes})
			return
		}
	}

	switch req.Kind {
	case api.CreateCallRequestKindAIOUTBOUND:
		s.createAICall(w, r, req)
	case api.CreateCallRequestKindAGENTOUTBOUND:
		s.createAgentCall(w, r, req)
	}
}

// createAICall originates the customer leg and hands whoever answers to the
// bot running the DID's flow. Placing one is an operations decision, so it
// asks for SUPERVISOR — a role check the router used to make, and which moved
// in here when the two kinds became one route with two answers.
func (s *Server) createAICall(w http.ResponseWriter, r *http.Request, req api.CreateCallRequest) {
	if !s.hasRole(w, r, auth.RoleSupervisor) {
		return
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
			writeDuplicate(w, callID)
			return
		}
		writeOutboundError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.CreateCallResponse{CallID: callID})
}

// createAgentCall places a click-to-dial: an agent's phone is raised first and
// the destination is dialled when that leg answers.
//
// Who may name which phone is the whole of the authorization here. An agent
// dials from the phone they signed in at and may name no other, which is the
// same rule their call-control operations follow — a signed-in identity acts
// as itself. A supervisor and the API key have no phone of their own, so they
// must say which one to raise, and the phone is checked against the switch
// rather than against presence: a system integrating with this places calls
// for agents who are on the floor with a registered phone and never signed
// into this application at all, and presence has nothing to say about them.
func (s *Server) createAgentCall(w http.ResponseWriter, r *http.Request, req api.CreateCallRequest) {
	identity, _ := identityFrom(r.Context())

	var extension, callcenterName string
	switch {
	case req.ExtensionNumber == nil || *req.ExtensionNumber == "":
		// No extension named: the caller means their own, so they had better
		// have one.
		agentID, ok := s.signedInAgent(r, identity)
		if !ok {
			writeError(w, http.StatusBadRequest, CodeValidationFailed,
				"name the extension to dial from", nil)
			return
		}
		presence := s.agents.Presence(agentID)
		if presence.ExtensionNumber == "" {
			writeError(w, http.StatusConflict, CodeConflict, "sign in to a phone first", nil)
			return
		}
		extension = presence.ExtensionNumber
		// The switch's own name for this agent, so mod_callcenter can be told
		// they are on a call and stop offering them queue calls while they are.
		callcenterName = s.agents.CallcenterNameFor(r.Context(), agentID)

	default:
		extension = *req.ExtensionNumber
		if agentID, ok := s.signedInAgent(r, identity); ok &&
			!identity.Role.AtLeast(auth.RoleSupervisor) {
			// An agent naming a phone may only name their own. Rejected
			// rather than quietly redirected: a cockpit that sent the wrong
			// extension has a bug, and dialling from the right one anyway
			// would hide it.
			if s.agents.Presence(agentID).ExtensionNumber != extension {
				writeError(w, http.StatusForbidden, CodeForbidden,
					"an agent dials from their own phone", nil)
				return
			}
		}
		if !s.isPhoneReachable(w, extension) {
			return
		}
		// Somebody may still be signed in at the named phone; if they are,
		// their queues need telling just the same.
		if agentID, ok := s.agents.AgentAtExtension(extension); ok {
			callcenterName = s.agents.CallcenterNameFor(r.Context(), agentID)
		}
	}

	dial := outbound.AgentDialRequest{
		AgentExtension: extension,
		To:             req.To,
		CallcenterName: callcenterName,
	}
	if req.UserData != nil {
		dial.UserData = *req.UserData
	}
	if req.CallID != nil {
		dial.CallID = *req.CallID
	}

	// A dial made to keep a callback is written on the callback before the
	// phone rings, under an id minted here so the two can be joined when the
	// call's CDR lands. The write doubles as the check that the caller holds
	// the callback: nobody else's promise gets a call recorded against it.
	var kept *store.Callback
	if req.CallbackID != nil {
		if s.ledger == nil || isMachine(identity) {
			writeError(w, http.StatusBadRequest, CodeValidationFailed,
				"a callback is kept by a signed-in agent", nil)
			return
		}
		if dial.CallID == uuid.Nil {
			dial.CallID = uuid.New()
		}
		callback, err := s.ledger.MarkCallbackAttempt(r.Context(), *req.CallbackID, dial.CallID, identity.UserID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusConflict, CodeConflict, "claim the callback before calling back", nil)
				return
			}
			writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot note the callback attempt", nil)
			return
		}
		kept = &callback
	}

	callID, err := s.outbound.Dial(r.Context(), dial)
	if err != nil {
		if errors.Is(err, outbound.ErrAlreadyPlaced) {
			writeDuplicate(w, callID)
			return
		}
		writeOutboundError(w, err)
		return
	}
	if kept != nil {
		s.publishCallback(r, events.TypeCallbackUpdated, *kept)
	}
	writeJSON(w, http.StatusCreated, api.CreateCallResponse{CallID: callID})
}

// hasRole answers the role question the router no longer can, because one
// route now serves two operations with two different answers.
func (s *Server) hasRole(w http.ResponseWriter, r *http.Request, want auth.Role) bool {
	identity, ok := identityFrom(r.Context())
	if !ok || !identity.Role.AtLeast(want) {
		writeError(w, http.StatusForbidden, CodeForbidden, "insufficient role",
			map[string]any{"requiredRole": string(want)})
		return false
	}
	return true
}

// signedInAgent resolves the agent profile behind a request, if there is one.
// The API key has no user and therefore never has one.
func (s *Server) signedInAgent(r *http.Request, identity auth.Identity) (uuid.UUID, bool) {
	if isMachine(identity) {
		return uuid.Nil, false
	}
	agentID, err := s.agentDir.AgentIDForUser(r, identity.UserID)
	if err != nil {
		return uuid.Nil, false
	}
	return agentID, true
}

// isPhoneReachable refuses a call at a phone that cannot take it, and says
// which way it cannot.
//
// An extension the switch has never mentioned is refused as unknown rather
// than as unregistered: the two are different mistakes — a typo in an
// integration versus a phone that is switched off — and an operator reading
// the error has to be able to tell them apart.
func (s *Server) isPhoneReachable(w http.ResponseWriter, extension string) bool {
	isRegistered, isInService, isKnown := s.agents.DeviceAtExtension(extension)
	switch {
	case !isKnown:
		writeError(w, http.StatusConflict, CodeConflict, "no such phone", nil)
		return false
	case !isRegistered:
		writeError(w, http.StatusConflict, CodeConflict, "the phone is not registered", nil)
		return false
	case !isInService:
		writeError(w, http.StatusConflict, CodeConflict, "the phone is not answering", nil)
		return false
	}
	return true
}

// writeDuplicate answers a retry that named a call already placed.
func writeDuplicate(w http.ResponseWriter, callID uuid.UUID) {
	isDuplicate := true
	writeJSON(w, http.StatusOK, api.CreateCallResponse{CallID: callID, IsDuplicate: &isDuplicate})
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
