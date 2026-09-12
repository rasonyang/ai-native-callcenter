// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	User auth.Identity `json:"user"`
	// The phone, as the switch currently holds it. A screen asks this to
	// decide whether it may offer to place a call; it is a fact about the
	// device and not about the account, which is why it is answered from the
	// agent service rather than from the session.
	//
	// False and nil for anybody who is not an agent — a supervisor has no
	// phone of their own signed in — and that is the honest answer rather than
	// an omission.
	IsDeviceRegistered bool    `json:"isDeviceRegistered"`
	DeviceAccount      *string `json:"deviceAccount"`
}

// deviceOf answers what the switch holds for this request's agent identity.
func (s *Server) deviceOf(r *http.Request, ac AuthContext) (isRegistered bool, account *string) {
	if s.agents == nil || !ac.IsAgent() {
		return false, nil
	}
	isRegistered, _ = s.agents.DeviceState(ac.AgentID)
	if !isRegistered {
		return false, nil
	}
	// The number the phone registered as is the one bound to the agent: a
	// session is only ever issued for that extension.
	if ext := s.agents.BoundExtensionFor(r.Context(), ac.AgentID); ext != "" {
		account = &ext
	}
	return true, account
}

func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"username and password are required", map[string]any{"field": "username"})
		return
	}

	ip, _ := netip.ParseAddr(clientIP(r))
	token, id, err := s.auth.Login(r.Context(), req.Username, req.Password, r.UserAgent(), ip)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, CodeInvalidCredentials, "invalid credentials", nil)
		return
	case errors.Is(err, auth.ErrUserSuspended):
		writeError(w, http.StatusForbidden, CodeUserSuspended, "user suspended", nil)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "login failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, CodeStorageDown, "cannot issue session", nil)
		return
	}

	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, loginResponse{User: id})
}

func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	// Signing out of the page signs the whole agent out: presence first, then
	// the phone, then the web session.
	//
	// The order is the point, and it was decided by a live run. Revoking the
	// credential flushes the registration, the switch answers with
	// sofia::unregister, and a READY agent losing their phone is moved to
	// NOT_READY with reason DEVICE_LOST — which is true of a phone that
	// crashed and a lie about a person who pressed Sign out. Signing presence
	// out first means the DEVICE_UNREGISTERED that follows finds nobody READY
	// and has nothing to report.
	if ac, ok := authFrom(r.Context()); ok && ac.IsAgent() {
		s.signAgentOut(r, ac.AgentID)
	}
	if cookie, err := r.Cookie(s.cfg.SessionCookie); err == nil {
		if err := s.auth.Logout(r.Context(), cookie.Value); err != nil {
			slog.ErrorContext(r.Context(), "logout failed", "error", err)
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// signAgentOut ends everything a person's sign-out ends, in the order the
// switch needs it: presence, then the phone credential.
//
// Every failure here is logged and continued past. The web session must end
// even when the agent service or the credential store cannot be reached,
// because the alternative is an account that cannot sign out — and both of the
// states left behind expire by themselves.
func (s *Server) signAgentOut(r *http.Request, agentID uuid.UUID) {
	if s.agents != nil {
		// Already signed out is the state this asks for, not a failure: a
		// person who signed out of the cockpit and then out of the page is
		// the ordinary way this happens.
		if _, err := s.agents.Logout(r.Context(), agentID); err != nil &&
			!errors.Is(err, agents.ErrNotLoggedIn) {
			slog.ErrorContext(r.Context(), "could not sign the agent out of presence on logout",
				"agentId", agentID, "error", err)
		}
	}
	if s.sipSessions != nil {
		// Leaving the credential behind would keep a closed tab registered
		// and ringing for whatever is left of the session's lifetime, which
		// is the exact thing one-session-per-agent exists to prevent.
		if err := s.sipSessions.Revoke(r.Context(), agentID); err != nil {
			slog.ErrorContext(r.Context(), "could not revoke the sip session on logout",
				"agentId", agentID, "error", err)
		}
	}
}

func (s *Server) GetMe(w http.ResponseWriter, r *http.Request) {
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	isRegistered, account := s.deviceOf(r, ac)
	writeJSON(w, http.StatusOK, loginResponse{
		User:               ac.User,
		IsDeviceRegistered: isRegistered,
		DeviceAccount:      account,
	})
}

func (s *Server) GetSystemHealth(w http.ResponseWriter, _ *http.Request) {
	// Trunks are read here rather than managed anywhere: a gateway is defined
	// in the switch's own profile configuration, which this application does
	// not write (migration 00021). An operator whose outbound calls are
	// failing needs to know whether the trunk is there, and the switch is the
	// only thing that knows.
	//
	// An empty list when the switch cannot be reached, not an error: the rest
	// of this answer is still true, and a screen that fails wholesale because
	// one card cannot be filled tells the reader less than one that says so.
	trunks := []telephony.Trunk{}
	if s.trunks != nil {
		if found, err := s.trunks.Trunks(); err == nil {
			trunks = found
		} else {
			slog.Warn("cannot read the switch's trunks", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sseClients": s.hub.SubscriberCount(),
		"oldestSeq":  s.hub.OldestSeq(),
		"trunks":     trunks,
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
