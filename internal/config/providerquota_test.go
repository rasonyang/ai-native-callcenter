// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The six quota settings, cleared for each case so a developer's own .env or
// environment cannot decide it.
var providerQuotaKeys = []string{
	"AICC_PROVIDER_SESSION_RPM", "AICC_PROVIDER_SESSION_LIMIT", "AICC_PROVIDER_SESSION_PERIOD",
	"AICC_PROVIDER_TOKEN_RPM", "AICC_PROVIDER_TOKEN_LIMIT", "AICC_PROVIDER_TOKEN_PERIOD",
}

// The RPM form keeps its v0.9.0 meaning: empty keeps the profile's default, 0
// turns a limit off, a positive number replaces it over a minute, and anything
// else is refused at startup (#604).
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
			for _, k := range providerQuotaKeys {
				t.Setenv(k, "")
			}
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
			isSessionsSet := c.ProviderSessionLimitFrom != ""
			isTokSet := c.ProviderTokenLimitFrom != ""
			if c.ProviderSessionLimit != tt.wantSessions || isSessionsSet != tt.wantSessionsSet ||
				c.ProviderTokenLimit != tt.wantTokens || isTokSet != tt.wantTokSet {
				t.Errorf("sessions %d (set %v), tokens %d (set %v); want %d (%v), %d (%v)",
					c.ProviderSessionLimit, isSessionsSet, c.ProviderTokenLimit, isTokSet,
					tt.wantSessions, tt.wantSessionsSet, tt.wantTokens, tt.wantTokSet)
			}
			// The RPM form is the same limit over a minute, named as given.
			if isSessionsSet && (c.ProviderSessionPeriod != time.Minute || c.ProviderSessionLimitFrom != "AICC_PROVIDER_SESSION_RPM") {
				t.Errorf("session limit over %s from %s, want 60s from AICC_PROVIDER_SESSION_RPM",
					c.ProviderSessionPeriod, c.ProviderSessionLimitFrom)
			}
			if isTokSet && (c.ProviderTokenPeriod != time.Minute || c.ProviderTokenLimitFrom != "AICC_PROVIDER_TOKEN_RPM") {
				t.Errorf("token limit over %s from %s, want 60s from AICC_PROVIDER_TOKEN_RPM",
					c.ProviderTokenPeriod, c.ProviderTokenLimitFrom)
			}
		})
	}
}

// A limit is a count over a period the vendor chooses (#644). The new form
// takes both; a limit without a period is over 60s; and every combination that
// cannot be meant stops startup with a message naming the setting.
func TestProviderQuotaLimitsOverAPeriod(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string

		wantSession, wantToken         int
		wantSessionPer, wantTokenPer   time.Duration
		wantSessionFrom, wantTokenFrom string
		wantErrs                       []string
	}{
		{name: "the raised quota of #644",
			env: map[string]string{
				"AICC_PROVIDER_SESSION_LIMIT": "3", "AICC_PROVIDER_SESSION_PERIOD": "1s",
				"AICC_PROVIDER_TOKEN_LIMIT": "60000", "AICC_PROVIDER_TOKEN_PERIOD": "12s"},
			wantSession: 3, wantSessionPer: time.Second, wantSessionFrom: "AICC_PROVIDER_SESSION_LIMIT",
			wantToken: 60000, wantTokenPer: 12 * time.Second, wantTokenFrom: "AICC_PROVIDER_TOKEN_LIMIT"},
		{name: "a limit without its period is over 60s",
			env:         map[string]string{"AICC_PROVIDER_SESSION_LIMIT": "90"},
			wantSession: 90, wantSessionPer: time.Minute, wantSessionFrom: "AICC_PROVIDER_SESSION_LIMIT"},
		{name: "a limit of 0 is off whatever the period",
			env:       map[string]string{"AICC_PROVIDER_TOKEN_LIMIT": "0", "AICC_PROVIDER_TOKEN_PERIOD": "12s"},
			wantToken: 0, wantTokenPer: 12 * time.Second, wantTokenFrom: "AICC_PROVIDER_TOKEN_LIMIT"},
		{name: "one limit in each form",
			env:         map[string]string{"AICC_PROVIDER_SESSION_RPM": "180", "AICC_PROVIDER_TOKEN_LIMIT": "60000", "AICC_PROVIDER_TOKEN_PERIOD": "12s"},
			wantSession: 180, wantSessionPer: time.Minute, wantSessionFrom: "AICC_PROVIDER_SESSION_RPM",
			wantToken: 60000, wantTokenPer: 12 * time.Second, wantTokenFrom: "AICC_PROVIDER_TOKEN_LIMIT"},

		{name: "both forms of the session limit",
			env:      map[string]string{"AICC_PROVIDER_SESSION_RPM": "180", "AICC_PROVIDER_SESSION_LIMIT": "3", "AICC_PROVIDER_SESSION_PERIOD": "1s"},
			wantErrs: []string{"AICC_PROVIDER_SESSION_RPM and AICC_PROVIDER_SESSION_LIMIT are both set"}},
		{name: "both forms of the token limit, one of them off",
			env:      map[string]string{"AICC_PROVIDER_TOKEN_RPM": "0", "AICC_PROVIDER_TOKEN_LIMIT": "60000"},
			wantErrs: []string{"AICC_PROVIDER_TOKEN_RPM and AICC_PROVIDER_TOKEN_LIMIT are both set"}},
		{name: "a period without its limit",
			env:      map[string]string{"AICC_PROVIDER_SESSION_PERIOD": "1s"},
			wantErrs: []string{"AICC_PROVIDER_SESSION_PERIOD is set without AICC_PROVIDER_SESSION_LIMIT"}},
		{name: "a period beside the RPM form",
			env:      map[string]string{"AICC_PROVIDER_TOKEN_RPM": "300000", "AICC_PROVIDER_TOKEN_PERIOD": "12s"},
			wantErrs: []string{"AICC_PROVIDER_TOKEN_PERIOD is set without AICC_PROVIDER_TOKEN_LIMIT"}},
		{name: "a zero period",
			env:      map[string]string{"AICC_PROVIDER_SESSION_LIMIT": "3", "AICC_PROVIDER_SESSION_PERIOD": "0s"},
			wantErrs: []string{"AICC_PROVIDER_SESSION_PERIOD must be a positive duration"}},
		{name: "a negative period",
			env:      map[string]string{"AICC_PROVIDER_TOKEN_LIMIT": "60000", "AICC_PROVIDER_TOKEN_PERIOD": "-12s"},
			wantErrs: []string{"AICC_PROVIDER_TOKEN_PERIOD must be a positive duration"}},
		{name: "a period that is not a duration",
			env:      map[string]string{"AICC_PROVIDER_SESSION_LIMIT": "3", "AICC_PROVIDER_SESSION_PERIOD": "1 second"},
			wantErrs: []string{`AICC_PROVIDER_SESSION_PERIOD="1 second" is not a duration`}},
		{name: "a bare number is not a duration",
			env:      map[string]string{"AICC_PROVIDER_TOKEN_LIMIT": "60000", "AICC_PROVIDER_TOKEN_PERIOD": "12"},
			wantErrs: []string{`AICC_PROVIDER_TOKEN_PERIOD="12" is not a duration`}},
		{name: "a malformed limit",
			env:      map[string]string{"AICC_PROVIDER_SESSION_LIMIT": "three", "AICC_PROVIDER_SESSION_PERIOD": "1s"},
			wantErrs: []string{`AICC_PROVIDER_SESSION_LIMIT="three" is not a whole number`}},
		{name: "every mistake is named at once",
			env: map[string]string{
				"AICC_PROVIDER_SESSION_RPM": "180", "AICC_PROVIDER_SESSION_LIMIT": "3",
				"AICC_PROVIDER_TOKEN_PERIOD": "12s"},
			wantErrs: []string{"AICC_PROVIDER_SESSION_RPM and AICC_PROVIDER_SESSION_LIMIT are both set",
				"AICC_PROVIDER_TOKEN_PERIOD is set without AICC_PROVIDER_TOKEN_LIMIT"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range providerQuotaKeys {
				t.Setenv(k, tt.env[k])
			}
			c, err := Load()
			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("Load accepted %v", tt.env)
				}
				for _, want := range tt.wantErrs {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v\nwant it to say %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.ProviderSessionLimit != tt.wantSession || c.ProviderSessionLimitFrom != tt.wantSessionFrom ||
				(tt.wantSessionFrom != "" && c.ProviderSessionPeriod != tt.wantSessionPer) {
				t.Errorf("session limit %d per %s from %q, want %d per %s from %q",
					c.ProviderSessionLimit, c.ProviderSessionPeriod, c.ProviderSessionLimitFrom,
					tt.wantSession, tt.wantSessionPer, tt.wantSessionFrom)
			}
			if c.ProviderTokenLimit != tt.wantToken || c.ProviderTokenLimitFrom != tt.wantTokenFrom ||
				(tt.wantTokenFrom != "" && c.ProviderTokenPeriod != tt.wantTokenPer) {
				t.Errorf("token limit %d per %s from %q, want %d per %s from %q",
					c.ProviderTokenLimit, c.ProviderTokenPeriod, c.ProviderTokenLimitFrom,
					tt.wantToken, tt.wantTokenPer, tt.wantTokenFrom)
			}
		})
	}
}

// .env.example is the registry of every setting with its real default: the
// six quota settings are each listed, empty, because the default is the
// provider profile's.
func TestTheQuotaSettingsAreInTheRegistry(t *testing.T) {
	raw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range providerQuotaKeys {
		if !regexp.MustCompile(`(?m)^#?` + k + `=$`).Match(raw) {
			t.Errorf(".env.example does not list %s= with its empty default", k)
		}
	}
}
