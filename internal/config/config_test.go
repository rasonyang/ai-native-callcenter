// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
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
	if low, high, err := c.QueuePool(); err != nil || low != 7000 || high != 7999 {
		t.Errorf("QueuePool() = %d, %d, %v — want the documented 7000-7999", low, high, err)
	}
	if c.BotRTPDeadTimeout != 5*time.Second || c.BotFirstMediaTimeout != 30*time.Second {
		t.Errorf("bot media timeouts = %s, %s, want the documented 5s and 30s",
			c.BotRTPDeadTimeout, c.BotFirstMediaTimeout)
	}
	if c.SeedPassword != "aicc@123" {
		t.Errorf("SeedPassword = %q, want the documented aicc@123", c.SeedPassword)
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

func TestBotMediaTimeouts(t *testing.T) {
	t.Setenv("AICC_BOT_RTP_DEAD_TIMEOUT", "0")
	t.Setenv("AICC_BOT_FIRST_MEDIA_TIMEOUT", "45s")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if c.BotRTPDeadTimeout != 0 || c.BotFirstMediaTimeout != 45*time.Second {
		t.Errorf("got %s, %s, want 0 (disabled) and 45s", c.BotRTPDeadTimeout, c.BotFirstMediaTimeout)
	}

	t.Setenv("AICC_BOT_FIRST_MEDIA_TIMEOUT", "-1s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AICC_BOT_FIRST_MEDIA_TIMEOUT") {
		t.Errorf("a negative timeout was accepted: %v", err)
	}
}

func TestBotGreetingMediaWait(t *testing.T) {
	tests := []struct {
		name      string
		value     *string
		wantGated bool
		wantWait  time.Duration
		wantErr   bool
	}{
		{name: "unset greets immediately"},
		{name: "empty is unset", value: ptrTo("")},
		{name: "zero waits for media however long", value: ptrTo("0"), wantGated: true},
		{name: "a grace", value: ptrTo("3s"), wantGated: true, wantWait: 3 * time.Second},
		{name: "a malformed value is refused, not read as unset", value: ptrTo("3 s"), wantErr: true},
		{name: "negative is refused", value: ptrTo("-1s"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value == nil {
				t.Setenv("AICC_BOT_GREETING_MEDIA_WAIT", "")
				os.Unsetenv("AICC_BOT_GREETING_MEDIA_WAIT")
			} else {
				t.Setenv("AICC_BOT_GREETING_MEDIA_WAIT", *tt.value)
			}
			c, err := Load()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "AICC_BOT_GREETING_MEDIA_WAIT") {
					t.Fatalf("Load() error = %v, want one naming the setting", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if c.IsBotGreetingGated != tt.wantGated || c.BotGreetingMediaWait != tt.wantWait {
				t.Errorf("got gated=%v wait=%s, want gated=%v wait=%s",
					c.IsBotGreetingGated, c.BotGreetingMediaWait, tt.wantGated, tt.wantWait)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestParsePeers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw     string
		want    string // the parsed prefixes, space-separated
		wantErr bool
	}{
		{raw: "", want: ""},
		{raw: " , ", want: ""},
		{raw: "192.168.31.55", want: "192.168.31.55/32"},
		{raw: "10.130.0.0/24,127.0.0.1", want: "10.130.0.0/24 127.0.0.1/32"},
		{raw: " 10.130.0.0/24 , 127.0.0.1 ,", want: "10.130.0.0/24 127.0.0.1/32"},
		// Host bits are dropped, not refused: the network is what was meant.
		{raw: "10.130.0.7/24", want: "10.130.0.0/24"},
		{raw: "::ffff:10.0.0.1", want: "10.0.0.1/32"},
		{raw: "fd00::/8", want: "fd00::/8"},
		{raw: "10.130.0.0/33", wantErr: true},
		{raw: "10.130.0.0/", wantErr: true},
		{raw: "192.168.31.300", wantErr: true},
		{raw: "switch.local", wantErr: true},
		{raw: "10.0.0.1-10.0.0.9", wantErr: true},
		{raw: "fe80::1%en0", wantErr: true},
	}
	for _, tt := range tests {
		prefixes, err := parsePeers(tt.raw)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parsePeers(%q) = %v, want an error", tt.raw, prefixes)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePeers(%q) error = %v", tt.raw, err)
			continue
		}
		got := make([]string, len(prefixes))
		for i, p := range prefixes {
			got[i] = p.String()
		}
		if strings.Join(got, " ") != tt.want {
			t.Errorf("parsePeers(%q) = %v, want %s", tt.raw, got, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "bad env",
			cfg:     Config{Env: "staging", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour, ExtensionRange: "1000-1999", QueueRange: "7000-7999"},
			wantErr: "AICC_ENV",
		},
		{
			name:    "empty database url",
			cfg:     Config{Env: "dev", DatabaseMaxConns: 1, SessionTTL: time.Hour, ExtensionRange: "1000-1999", QueueRange: "7000-7999"},
			wantErr: "AICC_DATABASE_URL",
		},
		{
			name:    "zero pool",
			cfg:     Config{Env: "dev", DatabaseURL: "x", SessionTTL: time.Hour, ExtensionRange: "1000-1999", QueueRange: "7000-7999"},
			wantErr: "AICC_DATABASE_MAX_CONNS",
		},
		{
			name:    "short session ttl",
			cfg:     Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Second, ExtensionRange: "1000-1999", QueueRange: "7000-7999"},
			wantErr: "AICC_SESSION_TTL",
		},
		{
			name:    "bad seed",
			cfg:     Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour, Seed: "sample", ExtensionRange: "1000-1999", QueueRange: "7000-7999"},
			wantErr: "AICC_SEED",
		},
		{
			name: "bad extension range",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000..1999", QueueRange: "7000-7999"},
			wantErr: "AICC_EXTENSION_RANGE",
		},
		{
			// Refused rather than quietly swapped end for end: an operator who
			// wrote it backwards meant a range, and guessing which one is how
			// phones end up in a pool nobody chose.
			name: "extension range ends before it starts",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1999-1000", QueueRange: "7000-7999"},
			wantErr: "AICC_EXTENSION_RANGE",
		},
		{
			// The seed hashes it for accounts that must pass the same floor
			// auth.CreateUser enforces.
			name: "short seed password",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "short"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			// Only the demo seed reads it, so nothing else is refused over it.
			name: "short seed password without the demo seed",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", SeedPassword: "short"},
		},
		{
			// A list with a typo in it is refused whole: the rest of it would
			// otherwise be enforced without the entry that named the switch.
			name: "bad bot allowed peers",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999",
				BotAllowedPeers: "10.130.0.0/24, 192.168.31.300"},
			wantErr: "AICC_BOT_ALLOWED_PEERS",
		},
		// The switch expands the seed password into an XML attribute. These
		// would break its directory or be cut short on the way there.
		{
			name: "seed password with ampersand",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc&1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with less than",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc<1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with greater than",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc>1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with double quote",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc\"1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with single quote",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc'1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with space",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc 1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with tab",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc\t1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with newline",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc1234\n"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with control",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc\x001234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			name: "seed password with invalid utf-8",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc\xff1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			// The stack gives the switch this value whether or not the demo
			// seed runs, so the characters are checked either way.
			name: "seed password with an ampersand without the demo seed",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", SeedPassword: "aicc&1234"},
			wantErr: "AICC_SEED_PASSWORD",
		},
		{
			// Punctuation XML carries as-is, and non-ASCII letters, are fine.
			name: "seed password with safe punctuation",
			cfg: Config{Env: "dev", DatabaseURL: "x", DatabaseMaxConns: 1, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo",
				SeedPassword: "aicc@123!#$%*+-=?^_~,.;:/|()[]{}密码"},
		},
		{
			name: "valid",
			cfg: Config{Env: "prod", DatabaseURL: "x", DatabaseMaxConns: 4, SessionTTL: time.Hour,
				ExtensionRange: "1000-1999", QueueRange: "7000-7999", Seed: "demo", SeedPassword: "aicc@123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
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
