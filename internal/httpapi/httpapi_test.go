// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
)

func TestRoleAtLeast(t *testing.T) {
	tests := []struct {
		name string
		have auth.Role
		want auth.Role
		ok   bool
	}{
		{"agent meets agent", auth.RoleAgent, auth.RoleAgent, true},
		{"agent below supervisor", auth.RoleAgent, auth.RoleSupervisor, false},
		{"agent below admin", auth.RoleAgent, auth.RoleAdmin, false},
		{"supervisor meets agent", auth.RoleSupervisor, auth.RoleAgent, true},
		{"supervisor below admin", auth.RoleSupervisor, auth.RoleAdmin, false},
		{"admin meets everything", auth.RoleAdmin, auth.RoleSupervisor, true},
		{"unknown role meets nothing", auth.Role("GUEST"), auth.RoleAgent, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.have.AtLeast(tt.want); got != tt.ok {
				t.Errorf("%s.AtLeast(%s) = %v, want %v", tt.have, tt.want, got, tt.ok)
			}
		})
	}
}

func TestRequireRoleRejectsAndPasses(t *testing.T) {
	handler := requireRole(auth.RoleSupervisor)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	tests := []struct {
		name     string
		identity *auth.Identity
		want     int
	}{
		{"no identity", nil, http.StatusForbidden},
		{"agent rejected", &auth.Identity{Role: auth.RoleAgent}, http.StatusForbidden},
		{"supervisor allowed", &auth.Identity{Role: auth.RoleSupervisor}, http.StatusOK},
		{"admin allowed", &auth.Identity{Role: auth.RoleAdmin}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.identity != nil {
				ctx := contextWithIdentity(r.Context(), *tt.identity)
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestParseLastEventID(t *testing.T) {
	tests := []struct {
		name   string
		header string
		query  string
		want   int64
	}{
		{"absent", "", "", 0},
		{"header wins", "42", "7", 42},
		{"query fallback", "", "7", 7},
		{"garbage", "not-a-number", "", 0},
		{"whitespace tolerated", " 15 ", "", 15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := "/api/v1/events"
			if tt.query != "" {
				target += "?lastEventId=" + tt.query
			}
			r := httptest.NewRequest(http.MethodGet, target, nil)
			if tt.header != "" {
				r.Header.Set("Last-Event-ID", tt.header)
			}
			if got := parseLastEventID(r); got != tt.want {
				t.Errorf("parseLastEventID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseTypes(t *testing.T) {
	got := parseTypes("PARTY_RINGING, AGENT_READY ,")
	if len(got) != 2 || got[0] != "PARTY_RINGING" || got[1] != "AGENT_READY" {
		t.Errorf("parseTypes() = %v, want [PARTY_RINGING AGENT_READY]", got)
	}
	if parseTypes("") != nil {
		t.Error("parseTypes(\"\") should be nil")
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		remote string
		want   string
	}{
		{"192.168.1.5:51234", "192.168.1.5"},
		{"[::1]:8080", "::1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		if got := clientIP(r); got != tt.want {
			t.Errorf("clientIP(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}
