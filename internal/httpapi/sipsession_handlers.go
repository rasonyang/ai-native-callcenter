// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/sipsession"
)

// The agent's phone credentials: one SIP session per agent, minted by the
// platform rather than configured on the handset.
//
// Nothing here writes the response anywhere but the response. The a1-hash is
// a credential, so it is not logged, not audited and not repeated in an error
// body; the audit trail records that the operation happened and by whom, which
// is the fact worth keeping.

// CreateAgentSIPSession issues the phone credentials for this agent.
func (s *Server) CreateAgentSIPSession(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if s.sipSessions == nil {
		writeError(w, http.StatusNotImplemented, CodeInternal,
			"this deployment issues no phone credentials", nil)
		return
	}

	// The phone is signed in for exactly as long as the person is. A key has
	// no session to follow, so its credential gets the deployment's session
	// lifetime from now — the same window a person would have had.
	expiresAt := ac.SessionExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(s.cfg.SessionTTL)
	}

	issued, err := s.sipSessions.Issue(r.Context(), agentID, expiresAt)
	switch {
	case errors.Is(err, sipsession.ErrNoExtensionBound):
		// CONFLICT, not VALIDATION_FAILED: the request was well formed and
		// there is nothing the caller can put in it to fix this. What is not
		// ready is the agent's configuration.
		writeError(w, http.StatusConflict, CodeConflict,
			"no extension is bound to this agent, so there is nothing to register as", nil)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "could not issue a sip session",
			"agentId", agentID, "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown,
			"cannot issue phone credentials", nil)
		return
	}

	writeJSON(w, http.StatusOK, api.SIPSession{
		SIPDomain: issued.SIPDomain,
		WssURL:    issued.WSSURL,
		Account:   issued.Account,
		A1Hash:    issued.A1Hash,
		ExpiresAt: issued.ExpiresAt.UTC().Truncate(time.Second),
	})
}

// DeleteAgentSIPSession revokes the phone credentials and flushes the
// registration.
//
// Idempotent: an agent with no session is already in the state this asks for,
// and the answer is the same 204.
func (s *Server) DeleteAgentSIPSession(w http.ResponseWriter, r *http.Request) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	if s.sipSessions == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.sipSessions.Revoke(r.Context(), agentID); err != nil {
		slog.ErrorContext(r.Context(), "could not revoke a sip session",
			"agentId", agentID, "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown,
			"cannot revoke phone credentials", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
