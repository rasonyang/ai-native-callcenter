// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	User auth.Identity `json:"user"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cfg.SessionCookie); err == nil {
		if err := s.auth.Logout(r.Context(), cookie.Value); err != nil {
			slog.ErrorContext(r.Context(), "logout failed", "error", err)
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{User: id})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sseClients": s.hub.SubscriberCount(),
		"oldestSeq":  s.hub.OldestSeq(),
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
