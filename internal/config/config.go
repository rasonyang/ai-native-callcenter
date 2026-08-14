// SPDX-License-Identifier: Apache-2.0

// Package config loads runtime configuration from the environment.
//
// Every setting is an AICC_* environment variable; a .env file in the working
// directory is loaded first, and real environment variables always win over it.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete runtime configuration of the application.
type Config struct {
	Env         string // "dev" | "prod"; affects logging format and error verbosity
	HTTPAddr    string // main listener, e.g. ":8080"
	MetricsAddr string // separate unauthenticated listener for /metrics, /healthz, /readyz

	DatabaseURL      string
	DatabaseMaxConns int32

	ESLAddr     string
	ESLPassword string
	// SwitchDomain must match FreeSWITCH own $${domain}: the Lua handler
	// renders queue names with it and mod_callcenter matches them literally.
	SwitchDomain string
	// SIPProfile is the sofia profile agents register to.
	SIPProfile string

	// The AI voice leg. The SIP port is the target of the switch's bot
	// gateway; the RTP range sits clear of the switch's own.
	BotSIPHost string
	BotSIPPort int
	// BotAdvertiseIP overrides route probing in SDP answers; empty probes.
	BotAdvertiseIP string
	BotRTPPortLow  int
	BotRTPPortHigh int
	BotMaxCalls    int
	// BotBackendBase is the base URL flows' declarative HTTP tools call.
	BotBackendBase string
	// IsBotEnabled turns the whole AI leg off, for deployments that only
	// route to people.
	IsBotEnabled bool

	SessionTTL    time.Duration
	SessionCookie string
	SecureCookies bool

	LogLevel string
	// LogDir receives one log file per process start, named by start time,
	// so any run can be analysed after the fact. Empty disables the file.
	LogDir       string
	OTLPEndpoint string // empty disables trace export
	ServiceName  string

	Seed string // "" | "demo" | "fresh"
}

// Load reads configuration from .env (if present) and the environment.
func Load() (Config, error) {
	loadDotEnv(".env")

	c := Config{
		Env:              env("AICC_ENV", "dev"),
		HTTPAddr:         env("AICC_HTTP_ADDR", ":8080"),
		MetricsAddr:      env("AICC_METRICS_ADDR", "127.0.0.1:9090"),
		DatabaseURL:      env("AICC_DATABASE_URL", "postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable"),
		DatabaseMaxConns: int32(envInt("AICC_DATABASE_MAX_CONNS", 10)),
		ESLAddr:          env("AICC_ESL_ADDR", "127.0.0.1:18021"),
		ESLPassword:      env("AICC_ESL_PASSWORD", "ClueCon"),
		SwitchDomain:     env("AICC_SWITCH_DOMAIN", "127.0.0.1"),
		SIPProfile:       env("AICC_SIP_PROFILE", "internal"),
		BotSIPHost:       env("AICC_BOT_SIP_HOST", "0.0.0.0"),
		BotSIPPort:       envInt("AICC_BOT_SIP_PORT", 6060),
		BotAdvertiseIP:   env("AICC_BOT_ADVERTISE_IP", ""),
		BotRTPPortLow:    envInt("AICC_BOT_RTP_PORT_LOW", 40000),
		BotRTPPortHigh:   envInt("AICC_BOT_RTP_PORT_HIGH", 40999),
		BotMaxCalls:      envInt("AICC_BOT_MAX_CALLS", 220),
		BotBackendBase:   env("AICC_BOT_BACKEND_BASE", ""),
		IsBotEnabled:     envBool("AICC_BOT_ENABLED", true),
		SessionTTL:       envDuration("AICC_SESSION_TTL", 12*time.Hour),
		SessionCookie:    env("AICC_SESSION_COOKIE", "aicc_session"),
		SecureCookies:    envBool("AICC_SECURE_COOKIES", false),
		LogLevel:         env("AICC_LOG_LEVEL", "info"),
		LogDir:           env("AICC_LOG_DIR", "logs"),
		OTLPEndpoint:     env("AICC_OTLP_ENDPOINT", ""),
		ServiceName:      env("AICC_SERVICE_NAME", "aicc"),
		Seed:             env("AICC_SEED", ""),
	}

	return c, c.validate()
}

func (c Config) validate() error {
	var errs []error
	if c.Env != "dev" && c.Env != "prod" {
		errs = append(errs, fmt.Errorf("AICC_ENV must be dev or prod, got %q", c.Env))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("AICC_DATABASE_URL must not be empty"))
	}
	if c.DatabaseMaxConns < 1 {
		errs = append(errs, fmt.Errorf("AICC_DATABASE_MAX_CONNS must be >= 1, got %d", c.DatabaseMaxConns))
	}
	if c.SessionTTL < time.Minute {
		errs = append(errs, fmt.Errorf("AICC_SESSION_TTL must be >= 1m, got %s", c.SessionTTL))
	}
	switch c.Seed {
	case "", "demo", "fresh":
	default:
		errs = append(errs, fmt.Errorf("AICC_SEED must be empty, demo or fresh, got %q", c.Seed))
	}
	return errors.Join(errs...)
}

// IsDev reports whether the process runs in development mode.
func (c Config) IsDev() bool { return c.Env == "dev" }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// loadDotEnv reads KEY=VALUE lines from path, without overriding existing
// environment variables. A missing file is not an error.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}
