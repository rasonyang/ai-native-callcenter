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
	// OutboundEndpoint renders a destination number into a dial string for
	// the AI outbound leg. The loopback form pins the dialplan (/XML)
	// because a loopback b-leg inherits the a-leg's, and an inherited
	// "inline" reads the number as an application name (found live:
	// "Invalid Application 1007").
	// (%s = the number). A trunked deployment sets sofia/gateway/<gw>/%s;
	// the default loops back into the local dialplan.
	OutboundEndpoint string
	// OutboundCallerID is presented on click-to-dial customer legs.
	OutboundCallerID string
	// IsBotEnabled turns the whole AI leg off, for deployments that only
	// route to people.
	IsBotEnabled bool

	// Provider is the speech model this deployment runs, resolved once at
	// startup: qwen inside mainland China, openai elsewhere. A call's
	// language never selects it.
	Provider string
	// ProviderEndpoint and ProviderModel replace the built-in address and
	// model of that provider. The vendor's own values are defaults, not
	// facts: a deployment may reach it through a proxy, on a regional host,
	// or at a server that only speaks the same protocol. Empty keeps the
	// built-in value.
	ProviderEndpoint string
	ProviderModel    string

	// IsTranscriptionEnabled turns live transcription of the human phase off
	// entirely. Off by default: it is a new external dependency, a new
	// listener and new spend, and doing nothing must keep today's behaviour.
	IsTranscriptionEnabled bool
	// StreamAddr is where the tapped audio arrives. Loopback by default,
	// following AICC_METRICS_ADDR: the traffic is unencrypted call audio, and
	// a default that bound every interface would be the mistake nobody
	// notices.
	StreamAddr string
	// StreamPublicURL is what the *switch* dials, which in a container is not
	// what we bind. It carries its own scheme; nothing composes one.
	StreamPublicURL string
	// StreamSecret signs the single-use attach token that is the whole of the
	// authentication on that listener.
	StreamSecret string
	// TranscribeProvider selects the recognition *client*, not merely a
	// profile: the two engines speak different wire protocols. Empty follows
	// AICC_PROVIDER, since one vendor is reachable per deployment.
	TranscribeProvider string
	// TranscribeEndpoint and TranscribeModel override the client's defaults.
	// On qwen the endpoint is effectively required: the workspace id is part
	// of the hostname, so there is no useful default to fall back on.
	TranscribeEndpoint string
	TranscribeModel    string

	// Recordings. Backend FS keeps files where the switch wrote them;
	// S3 uploads them to any S3-compatible store and clears the local spool.
	RecordingBackend string
	RecordingDir     string
	S3Endpoint       string
	S3AccessKey      string
	S3SecretKey      string
	S3Bucket         string
	S3IsSSL          bool

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

		IsTranscriptionEnabled: envBool("AICC_TRANSCRIPTION_ENABLED", false),
		StreamAddr:             env("AICC_STREAM_ADDR", "127.0.0.1:8090"),
		StreamPublicURL:        env("AICC_STREAM_PUBLIC_URL", ""),
		StreamSecret:           env("AICC_STREAM_SECRET", ""),
		TranscribeProvider:     env("AICC_TRANSCRIBE_PROVIDER", ""),
		TranscribeEndpoint:     env("AICC_TRANSCRIBE_ENDPOINT", ""),
		TranscribeModel:        env("AICC_TRANSCRIBE_MODEL", ""),
		Provider:               env("AICC_PROVIDER", "openai"),
		ProviderEndpoint:       env("AICC_PROVIDER_ENDPOINT", ""),
		ProviderModel:          env("AICC_PROVIDER_MODEL", ""),
		OutboundEndpoint:       env("AICC_OUTBOUND_ENDPOINT", "loopback/%s/aicc/XML"),
		OutboundCallerID:       env("AICC_OUTBOUND_CLID", ""),
		RecordingBackend:       env("AICC_RECORDING_BACKEND", "FS"),
		RecordingDir:           env("AICC_RECORDING_DIR", ""),
		S3Endpoint:             env("AICC_S3_ENDPOINT", ""),
		S3AccessKey:            env("AICC_S3_ACCESS_KEY", ""),
		S3SecretKey:            env("AICC_S3_SECRET_KEY", ""),
		S3Bucket:               env("AICC_S3_BUCKET", "aicc-recordings"),
		S3IsSSL:                envBool("AICC_S3_SSL", false),
		SessionTTL:             envDuration("AICC_SESSION_TTL", 12*time.Hour),
		SessionCookie:          env("AICC_SESSION_COOKIE", "aicc_session"),
		SecureCookies:          envBool("AICC_SECURE_COOKIES", false),
		LogLevel:               env("AICC_LOG_LEVEL", "info"),
		LogDir:                 env("AICC_LOG_DIR", "logs"),
		OTLPEndpoint:           env("AICC_OTLP_ENDPOINT", ""),
		ServiceName:            env("AICC_SERVICE_NAME", "aicc"),
		Seed:                   env("AICC_SEED", ""),
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
	if c.IsTranscriptionEnabled {
		if c.StreamPublicURL == "" {
			errs = append(errs, errors.New(
				"AICC_STREAM_PUBLIC_URL must be set when transcription is enabled: "+
					"it is what the switch dials back, and it is not what we bind"))
		} else if !strings.HasPrefix(c.StreamPublicURL, "ws://") &&
			!strings.HasPrefix(c.StreamPublicURL, "wss://") {
			// Caught here rather than at the first call, where it would present
			// as a tap that silently never connects.
			errs = append(errs, fmt.Errorf(
				"AICC_STREAM_PUBLIC_URL must be ws:// or wss://, got %q", c.StreamPublicURL))
		}
		if c.StreamSecret == "" {
			errs = append(errs, errors.New(
				"AICC_STREAM_SECRET must be set when transcription is enabled: "+
					"the token it signs is the only authentication on that listener"))
		}
		if c.TranscribeProviderName() == "qwen" && c.TranscribeEndpoint == "" {
			errs = append(errs, errors.New(
				"AICC_TRANSCRIBE_ENDPOINT must be set for qwen: the workspace id is "+
					"part of the hostname, so there is no default that could work"))
		}
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

// TranscribeProviderName is the recognition client this deployment runs.
// Empty follows the conversational provider, because one vendor is reachable
// per deployment — but the two are separate settings, because a deployment may
// legitimately transcribe with one and converse with the other.
func (c Config) TranscribeProviderName() string {
	if c.TranscribeProvider != "" {
		return c.TranscribeProvider
	}
	return c.Provider
}
