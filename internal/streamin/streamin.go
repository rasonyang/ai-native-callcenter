// SPDX-License-Identifier: Apache-2.0

// Package streamin receives the audio the switch taps from an agent's leg.
//
// It is a listener of its own rather than a route under /api/v1, for four
// reasons that are all about what the API surface is for: every API route sits
// behind a session cookie and a CSRF header, which FreeSWITCH cannot present;
// request routes carry a 30-second timeout, which a media stream must not; the
// project already separates an unauthenticated operational listener by address;
// and a path carrying fifty binary frames a second per call has no business
// behind the audit trail and tracing that wrap the API.
//
// The stream is stereo by construction: left is the agent's microphone, right
// is the customer. That is measured, not assumed — a two-tone loopback and then
// a live agent leg both read the same way.
package streamin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
	"github.com/rasonyang/ai-native-callcenter/internal/transcript"
)

// Logger is the slice of slog this package uses.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Transcripts hands out the per-call actor that owns transcript order.
type Transcripts interface {
	Lookup(callID uuid.UUID) (*transcript.Actor, bool)
}

// SessionFactory builds one recognition session; swapped out in tests.
type SessionFactory func() (transcribe.Session, error)

// Config wires the ingest.
type Config struct {
	// Addr is the listen address. Bound to the private interface, because the
	// traffic is unencrypted call audio and the mitigation for not encrypting
	// it is that it is not reachable.
	Addr string
	// Secret keys the attach token.
	Secret []byte
	// TokenTTL bounds how long a minted token stays usable.
	TokenTTL time.Duration
	// Profile is the transcription profile this deployment runs.
	Profile transcribe.Profile
	// NewSession builds a recognition session per speaker.
	NewSession SessionFactory
	// Transcripts resolves the call's transcript actor.
	Transcripts Transcripts
	Logger      Logger
}

// Server accepts tapped audio and turns it into transcript lines.
type Server struct {
	cfg  Config
	log  Logger
	srv  *http.Server
	addr string // what was actually bound, which a :0 port only knows afterwards

	mu       sync.Mutex
	sessions map[string]*session
	// used records spent tokens: one token, one connection.
	used map[string]time.Time
	// pending holds taps the switch accepted but that have not dialled back.
	pending map[string]*time.Timer

	// onStreamEnded is told when a stream ends gracefully, so the tap can stop
	// tracking an attachment the switch has already finished with.
	onStreamEnded func(callID uuid.UUID, channelID string)
}

// Claim is what a token asserts and the metadata frame must agree with.
type Claim struct {
	CallID  uuid.UUID
	PartyID uuid.UUID
	AgentID uuid.UUID
	Channel string
	// Language is the call's own, so the recogniser is told what to expect.
	// It rides in the signed token because it must be known before the first
	// frame: the module's metadata arrives after the session has to start.
	Language string
	Expires  time.Time
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 1024,
	// The switch is not a browser and sends no meaningful Origin.
	CheckOrigin: func(*http.Request) bool { return true },
}

// New prepares the ingest. Nothing listens until Start.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("streamin: an address is required")
	}
	if len(cfg.Secret) == 0 {
		return nil, errors.New("streamin: a secret is required to sign attach tokens")
	}
	if cfg.NewSession == nil || cfg.Transcripts == nil {
		return nil, errors.New("streamin: a session factory and a transcript registry are required")
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 60 * time.Second
	}
	if cfg.Logger == nil {
		return nil, errors.New("streamin: a logger is required")
	}
	return &Server{
		cfg:      cfg,
		log:      cfg.Logger,
		sessions: map[string]*session{},
		used:     map[string]time.Time{},
		pending:  map[string]*time.Timer{},
	}, nil
}

// Token mints the credential the switch presents when it dials back.
//
// It rides in the URL because that is the only carrier available: the module's
// metadata arrives as the first frame, which is *after* the handshake, so it
// can confirm an identity but cannot authenticate one.
func (s *Server) Token(c Claim) string {
	payload := fmt.Sprintf("%s|%s|%s|%s|%s|%d",
		c.CallID, c.PartyID, c.AgentID, c.Channel, c.Language, c.Expires.Unix())
	mac := hmac.New(sha256.New, s.cfg.Secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks a token and reports what it claims.
func (s *Server) verify(token string) (Claim, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Claim{}, errors.New("malformed token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Claim{}, errors.New("malformed token body")
	}
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return Claim{}, errors.New("malformed token signature")
	}
	mac := hmac.New(sha256.New, s.cfg.Secret)
	mac.Write(raw)
	if !hmac.Equal(mac.Sum(nil), want) {
		return Claim{}, errors.New("bad signature")
	}

	parts := strings.Split(string(raw), "|")
	if len(parts) != 6 {
		return Claim{}, errors.New("malformed claim")
	}
	var c Claim
	if c.CallID, err = uuid.Parse(parts[0]); err != nil {
		return Claim{}, errors.New("bad call id")
	}
	c.PartyID, _ = uuid.Parse(parts[1])
	c.AgentID, _ = uuid.Parse(parts[2])
	c.Channel = parts[3]
	c.Language = parts[4]
	unix, err := strconv.ParseInt(parts[5], 10, 64)
	if err != nil {
		return Claim{}, errors.New("bad expiry")
	}
	c.Expires = time.Unix(unix, 0)
	if time.Now().After(c.Expires) {
		return Claim{}, errors.New("token expired")
	}
	return c, nil
}

// Start binds the listener.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", s.handle)
	s.srv = &http.Server{Addr: s.cfg.Addr, Handler: mux}

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("streamin: listen %s: %w", s.cfg.Addr, err)
	}
	s.addr = ln.Addr().String()
	s.log.Info("transcription ingest listening", "addr", s.cfg.Addr)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("transcription ingest stopped", "error", err)
		}
	}()
	return nil
}

// Addr reports the bound address. With a :0 port this is the only way to learn
// which one, so it is what a caller dials back.
func (s *Server) Addr() string { return s.addr }

// Stop closes the listener and every session on it.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	live := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		live = append(live, sess)
	}
	s.mu.Unlock()
	for _, sess := range live {
		sess.close(ctx)
	}
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	claim, err := s.verify(r.URL.Query().Get("t"))
	if err != nil {
		// Deliberately terse to the caller and detailed to us: an attacker
		// learns nothing from "unauthorized", and we still get the reason.
		s.log.Warn("transcription ingest refused a connection",
			"error", err, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.claimToken(r.URL.Query().Get("t")) {
		s.log.Warn("transcription ingest refused a replayed token",
			"callId", claim.CallID, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.serve(conn, claim)
}

// claimToken spends a token, so a captured URL cannot be replayed.
func (s *Server) claimToken(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for t, at := range s.used {
		if now.Sub(at) > 10*time.Minute {
			delete(s.used, t)
		}
	}
	if _, spent := s.used[token]; spent {
		return false
	}
	s.used[token] = now
	return true
}

// metadata is the module's first text frame. It confirms identity; it cannot
// establish it, because it arrives after the handshake.
type metadata struct {
	CallID    string `json:"callId"`
	PartyID   string `json:"partyId"`
	AgentID   string `json:"agentId"`
	ChannelID string `json:"channelId"`
}

func (s *Server) serve(conn *websocket.Conn, claim Claim) {
	actor, ok := s.cfg.Transcripts.Lookup(claim.CallID)
	if !ok {
		s.log.Warn("transcription ingest has no transcript actor for this call",
			"callId", claim.CallID)
		conn.Close()
		return
	}

	sess := &session{
		claim: claim,
		conn:  conn,
		actor: actor,
		log:   s.log,
		cfg:   s.cfg,
	}
	key := claim.CallID.String() + "|" + claim.Channel
	s.mu.Lock()
	if prev := s.sessions[key]; prev != nil {
		s.mu.Unlock()
		s.log.Warn("a stream is already open for this channel", "callId", claim.CallID)
		conn.Close()
		return
	}
	s.sessions[key] = sess
	s.mu.Unlock()
	s.arrived(key)
	// The switch dialling back is the only observable moment between "the
	// switch accepted the attach" and "audio is being recognised", and without
	// it attach-to-LIVE is a latency stage with no measurement anywhere.
	s.log.Info("tapped stream connected",
		"callId", claim.CallID, "channelId", claim.Channel)

	defer func() {
		s.mu.Lock()
		delete(s.sessions, key)
		s.mu.Unlock()
	}()

	if sess.run() && s.onStreamEnded != nil {
		// The module closed the stream itself, which it does when the channel
		// goes away. Telling the tap now is what keeps the teardown quiet: the
		// Detach that follows a hangup would otherwise issue a stop against a
		// channel FreeSWITCH has already destroyed, and mod_audio_stream logs
		// that at ERR — once per tapped call, which buries a real one.
		s.onStreamEnded(claim.CallID, claim.Channel)
	}
}

// AttachStreamEndHandler registers who to tell when a tapped stream ends of its
// own accord. The Tap sets this on itself; nothing else has a use for it.
func (s *Server) AttachStreamEndHandler(fn func(callID uuid.UUID, channelID string)) {
	s.onStreamEnded = fn
}

// checkMetadata reports whether the frame agrees with what the token claimed.
// A mismatch is a bug or an attack; either way the audio is not what it says.
func checkMetadata(raw []byte, claim Claim) error {
	var m metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("undecodable metadata: %w", err)
	}
	if m.CallID != "" && !strings.EqualFold(m.CallID, claim.CallID.String()) {
		return fmt.Errorf("metadata call %s does not match the token's %s", m.CallID, claim.CallID)
	}
	if m.ChannelID != "" && m.ChannelID != claim.Channel {
		return fmt.Errorf("metadata channel %s does not match the token's %s", m.ChannelID, claim.Channel)
	}
	return nil
}

// speakerFor maps a stereo channel to who is on it.
//
// Measured on a live agent leg, not inferred from the flag names: left is the
// read stream, which on the agent's own channel is the agent's microphone;
// right is the write stream, which is what the agent hears, which is the
// customer.
func speakerFor(channel int) string {
	if channel == 0 {
		return store.SpeakerHumanAgent
	}
	return store.SpeakerCustomer
}

// connectGrace is how long a tap has to actually connect before we stop
// believing it will.
//
// `uuid_audio_stream … start` returns +OK before the socket exists — the
// connect is asynchronous — so a successful attach is not evidence that audio
// is coming. Without this the panel sits at "Connecting…" for the life of a
// call whose stream never arrived, which is the same silence as a call nobody
// is transcribing and is reported no differently.
const connectGrace = 12 * time.Second

// Expect records that a tap was attached and should connect shortly. The Tap
// calls this after the switch accepts the command.
func (s *Server) Expect(c Claim) {
	// Said here rather than inferred anywhere: the switch has accepted the
	// attach and a stream is expected, which is precisely "connecting". A
	// client that has to guess this from the absence of a state cannot tell it
	// from a call nobody is transcribing at all.
	if actor, ok := s.cfg.Transcripts.Lookup(c.CallID); ok {
		actor.State(transcript.StateConnecting, "", nil)
	}

	key := c.CallID.String() + "|" + c.Channel
	timer := time.AfterFunc(connectGrace, func() { s.giveUpOn(key, c) })

	s.mu.Lock()
	if prev := s.pending[key]; prev != nil {
		prev.Stop()
	}
	s.pending[key] = timer
	s.mu.Unlock()
}

// arrived cancels the expectation: the stream connected.
func (s *Server) arrived(key string) {
	s.mu.Lock()
	if timer := s.pending[key]; timer != nil {
		timer.Stop()
		delete(s.pending, key)
	}
	s.mu.Unlock()
}

// giveUpOn reports a tap that was accepted by the switch and never dialled
// back. Silence is the one thing this must not do.
func (s *Server) giveUpOn(key string, c Claim) {
	s.mu.Lock()
	_, connected := s.sessions[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if connected {
		return
	}

	s.log.Error("a tap was attached but never connected",
		"callId", c.CallID, "channelId", c.Channel, "after", connectGrace)
	if actor, ok := s.cfg.Transcripts.Lookup(c.CallID); ok {
		actor.State("ERROR", "STREAM_NEVER_CONNECTED", nil)
	}
}
