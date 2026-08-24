// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("AICC_ENV", "dev")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
	// M0: the dev FreeSWITCH listens on a non-stock ESL port.
	if c.ESLAddr != "127.0.0.1:18021" {
		t.Errorf("ESLAddr = %q, want 127.0.0.1:18021", c.ESLAddr)
	}
	if !c.IsDev() {
		t.Error("IsDev() = false, want true")
	}
	low, high, err := c.ExtensionPool()
	if err != nil || low != 1000 || high != 1999 {
		t.Errorf("ExtensionPool() = %d, %d, %v — want the documented 1000-1999", low, high, err)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("AICC_ENV", "prod")
	t.Setenv("AICC_HTTP_ADDR", ":9999")
	t.Setenv("AICC_SESSION_TTL", "30m")
	t.Setenv("AICC_SECURE_COOKIES", "true")
	t.Setenv("AICC_DATABASE_MAX_CONNS", "42")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q, want :9999", c.HTTPAddr)
	}
	if c.SessionTTL != 30*time.Minute {
		t.Errorf("SessionTTL = %s, want 30m", c.SessionTTL)
	}
	if !c.SecureCookies {
		t.Error("SecureCookies = false, want true")
	}
	if c.DatabaseMaxConns != 42 {
		t.Errorf("DatabaseMaxConns = %d, want 42", c.DatabaseMaxConns)
	}
	if c.IsDev() {
		t.Error("IsDev() = true, want false")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "bad env",
			cfg:     Config{Env: "staging", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour},
			wantErr: "AICC_ENV",
		},
		{
			name:    "empty database url",
			cfg:     Config{Env: "dev", DatabaseMaxConns: 1, SessionTTL: time.Hour},
			wantErr: "AICC_DATABASE_URL",
		},
		{
			name:    "zero pool",
			cfg:     Config{Env: "dev", DatabaseURL: "x", SessionTTL: time.Hour},
			wantErr: "AICC_DATABASE_MAX_CONNS",
		},
		{
			name:    "short session ttl",
			cfg:     Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Second},
			wantErr: "AICC_SESSION_TTL",
		},
		{
			name:    "bad seed",
			cfg:     Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour, Seed: "sample"},
			wantErr: "AICC_SEED",
		},
		{
			name: "bad extension range",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000..1999"},
			wantErr: "AICC_EXTENSION_RANGE",
		},
		{
			// Refused rather than quietly swapped end for end: an operator who
			// wrote it backwards meant a range, and guessing which one is how
			// phones end up in a pool nobody chose.
			name: "extension range ends before it starts",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1999-1000"},
			wantErr: "AICC_EXTENSION_RANGE",
		},
		{
			name: "valid",
			cfg: Config{Env: "prod", DatabaseURL: "x", DatabaseMaxConns: 4, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", Seed: "demo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
