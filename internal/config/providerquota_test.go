// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// Empty keeps the profile's default, 0 turns a limit off, a positive number
// replaces it, and anything else is refused at startup (#604).
func TestProviderQuotaSettings(t *testing.T) {
	tests := []struct {
		name, sessions, tokens      string
		wantSessions, wantTokens    int
		wantSessionsSet, wantTokSet bool
		wantErr                     string
	}{
		{name: "unset keeps the profile's"},
		{name: "set", sessions: "120", tokens: "250000", wantSessions: 120, wantTokens: 250000,
			wantSessionsSet: true, wantTokSet: true},
		{name: "zero is off, and set", sessions: "0", tokens: "0", wantSessionsSet: true, wantTokSet: true},
		{name: "negative", sessions: "-1", wantErr: "AICC_PROVIDER_SESSION_RPM"},
		{name: "not a number", tokens: "100k", wantErr: "AICC_PROVIDER_TOKEN_RPM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AICC_PROVIDER_SESSION_RPM", tt.sessions)
			t.Setenv("AICC_PROVIDER_TOKEN_RPM", tt.tokens)
			c, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.ProviderSessionRPM != tt.wantSessions || c.IsProviderSessionRPMSet != tt.wantSessionsSet ||
				c.ProviderTokenRPM != tt.wantTokens || c.IsProviderTokenRPMSet != tt.wantTokSet {
				t.Errorf("sessions %d (set %v), tokens %d (set %v); want %d (%v), %d (%v)",
					c.ProviderSessionRPM, c.IsProviderSessionRPMSet, c.ProviderTokenRPM, c.IsProviderTokenRPMSet,
					tt.wantSessions, tt.wantSessionsSet, tt.wantTokens, tt.wantTokSet)
			}
		})
	}
}
