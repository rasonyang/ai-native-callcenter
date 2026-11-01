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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	// pending holds the connect-grace watchdogs of taps the switch accepted
	// but that have not dialled back, keyed by the attach's own token. Keyed
	// by token and not by channel because it is the *attachment* that is
	// expected: a re-attach on the same channel is a new expectation, and the
	// one it replaces must not be able to report for it (#312).
	pending map[string]*expectation
	// arrivals holds streams that dialled back before their attach armed an
	// expectation. The switch may dial back while `uuid_audio_stream … start`
	// is still being written (#287), so an arrival is recorded and the later
	// Expect for that token is a no-op rather than a twelve-second wait that
	// reports a live stream as never connected.
	arrivals map[string]time.Time
	// revoked holds tokens whose attachment was retired before its stream
	// arrived, so a late `uuid_audio_stream … start` executing after the stop
	// is refused instead of leaving a stream nothing owns (#312).
	revoked map[string]time.Time
	// gaveUp holds tokens whose connect grace ran out and was reported. The
	// attachment is kept, because a stalled switch may still run its start,
	// but a re-attach of the same call on the same channel may replace it:
	// otherwise a start the switch never ran leaves the leg untranscribed for
	// the rest of the call. Cleared when the stream does arrive, or when the
	// attachment is revoked.
	gaveUp map[string]time.Time

	// onStreamEnded is told when a stream ends gracefully, so the tap can stop
	// tracking an attachment the switch has already finished with. It is
	// always called without s.mu held, because the Tap queries the Server
	// while holding its own lock.
	onStreamEnded func(channelID, token string)
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
	if cfg.Logger == nil {
		return nil, errors.New("streamin: a logger is required")
	}
	return &Server{
		cfg:      cfg,
		log:      cfg.Logger,
		sessions: map[string]*session{},
		used:     map[string]time.Time{},
		pending:  map[string]*expectation{},
		arrivals: map[string]time.Time{},
		revoked:  map[string]time.Time{},
		gaveUp:   map[string]time.Time{},
	}, nil
}

// expectation is one attach's connect-grace watchdog. It carries the key the
// stream will register under and the claim its report would name, so a timer
// that fires can only ever report the attach that armed it.
type expectation struct {
	key   string
	claim Claim
	timer *time.Timer
}

// Token mints the credential the switch presents when it dials back.
//
// It rides in the URL because that is the only carrier available: the module's
// metadata arrives as the first frame, which is *after* the handshake, so it
// can confirm an identity but cannot authenticate one.
func (s *Server) Token(c Claim) string {
	// The trailing field is a nonce, not part of the claim. It makes every
	// minted token unique, so a re-attach on the same channel within the same
	// second gets a token of its own rather than a replay of the one whose
	// stream is being replaced — which is what lets an arrival, an expectation
	// and a revocation belong to one attachment rather than to a channel.
	payload := fmt.Sprintf("%s|%s|%s|%s|%s|%d|%s",
		c.CallID, c.PartyID, c.AgentID, c.Channel, c.Language, c.Expires.Unix(),
		nonce())
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
	if len(parts) != 7 {
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
	token := r.URL.Query().Get("t")
	claim, err := s.verify(token)
	if err != nil {
		// Deliberately terse to the caller and detailed to us: an attacker
		// learns nothing from "unauthorized", and we still get the reason.
		s.log.Warn("transcription ingest refused a connection",
			"error", err, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.claimToken(token) {
		s.log.Warn("transcription ingest refused a replayed token",
			"callId", claim.CallID, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.isRevoked(token) {
		// A `uuid_audio_stream … start` that timed out was written, and a
		// stalled switch runs it late — possibly after the caller's leg is
		// gone and the tap was detached (#302, #312). Refusing it here is
		// what keeps it from becoming a stream nothing owns. This is the cheap
		// answer before the upgrade; serve asks again under the lock that
		// registers the session, which is the answer that counts.
		s.log.Warn("transcription ingest refused a stream whose tap was detached",
			"callId", claim.CallID, "channelId", claim.Channel, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.serve(conn, claim, token)
}

// claimToken spends a token, so a captured URL cannot be replayed.
func (s *Server) claimToken(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneLocked(now)
	if _, spent := s.used[token]; spent {
		return false
	}
	s.used[token] = now
	return true
}

// tokenRetention bounds the spent, arrived, revoked and given-up indexes. A
// token is usable until its claim's Expires, which the Tap stamps from its own
// ttl (a minute in production), so an entry kept far longer than that can no
// longer describe a connection that will be made.
const tokenRetention = 10 * time.Minute

// pruneLocked drops index entries that are too old to matter. The caller holds
// s.mu.
func (s *Server) pruneLocked(now time.Time) {
	for _, index := range []map[string]time.Time{s.used, s.arrivals, s.revoked, s.gaveUp} {
		for token, at := range index {
			if now.Sub(at) > tokenRetention {
				delete(index, token)
			}
		}
	}
}

// isRevoked reports whether this attachment was retired before its stream
// arrived.
func (s *Server) isRevoked(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, gone := s.revoked[token]
	return gone
}

// metadata is the module's first text frame. It confirms identity; it cannot
// establish it, because it arrives after the handshake.
type metadata struct {
	CallID    string `json:"callId"`
	PartyID   string `json:"partyId"`
	AgentID   string `json:"agentId"`
	ChannelID string `json:"channelId"`
}

func (s *Server) serve(conn *websocket.Conn, claim Claim, token string) {
	key := claim.CallID.String() + "|" + claim.Channel

	actor, ok := s.cfg.Transcripts.Lookup(claim.CallID)
	if !ok {
		s.log.Warn("transcription ingest has no transcript actor for this call",
			"callId", claim.CallID, "channelId", claim.Channel)
		s.mu.Lock()
		s.refusedLocked(token)
		s.mu.Unlock()
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
	// Revocation, registration and arrival are one critical section. The check
	// in handle ran before the websocket upgrade, and a detach landing between
	// the two would otherwise revoke a token whose stream then runs anyway,
	// owned by nothing (#312). Recording the arrival under the same lock is
	// what stops a Revoke from falling between "the session is registered"
	// and "its attachment has arrived".
	s.mu.Lock()
	if _, gone := s.revoked[token]; gone {
		s.mu.Unlock()
		s.log.Warn("transcription ingest refused a stream whose tap was detached",
			"callId", claim.CallID, "channelId", claim.Channel, "remote", conn.RemoteAddr().String())
		conn.Close()
		return
	}
	if prev := s.sessions[key]; prev != nil {
		s.refusedLocked(token)
		s.mu.Unlock()
		s.log.Warn("a stream is already open for this channel",
			"callId", claim.CallID, "channelId", claim.Channel)
		conn.Close()
		return
	}
	s.sessions[key] = sess
	s.arrivedLocked(token)
	s.mu.Unlock()
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
		//
		// It names the stream's token, not just its channel: the channel may
		// already carry the next attachment, and only the token says which
		// one this close belongs to.
		s.onStreamEnded(claim.Channel, token)
	}
}

// AttachStreamEndHandler registers who to tell when a tapped stream ends of its
// own accord. The Tap sets this on itself; nothing else has a use for it.
func (s *Server) AttachStreamEndHandler(fn func(channelID, token string)) {
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
//
// It starts when the attach has been sent, which for a command that timed out
// is when the timeout is reported (esl.CommandTimeout later). A switch that
// runs the command even later produces ERROR and then LIVE when the stream
// arrives; that is the honest reading at each moment, and stretching the grace
// would only delay the report for a stream that really never comes.
const connectGrace = 12 * time.Second

// Expect records that a tap's attach was sent and a stream should connect
// shortly. The Tap calls this once the attach command has been *sent*: for a
// success and also for a timeout, because a command written and unanswered is
// a command a stalled switch still runs (#302, #312).
//
// token is the attach's own, minted for its stream URL. Arrival, expectation
// and revocation all belong to that one attachment rather than to the channel,
// which is what makes a re-attach a new stream rather than a continuation of
// the one it replaces.
func (s *Server) Expect(token string, c Claim) {
	key := c.CallID.String() + "|" + c.Channel

	s.mu.Lock()
	if _, gone := s.revoked[token]; gone {
		// The attachment was retired while its command was still in flight
		// (a hangup or a transfer during a stalled `start`). Nothing is
		// expected of it any more, and a state published now would describe
		// a tap that no longer exists.
		delete(s.arrivals, token)
		s.mu.Unlock()
		return
	}
	if _, here := s.arrivals[token]; here {
		// The stream dialled back while the attach command was still in
		// flight, so it is already here (or already over) — not missing.
		// Arming a watchdog now would report it as never connected twelve
		// seconds later (#287), and publishing CONNECTING after the
		// session's own LIVE would move the state backwards.
		delete(s.arrivals, token)
		s.mu.Unlock()
		return
	}
	exp := &expectation{key: key, claim: c}
	exp.timer = time.AfterFunc(connectGrace, func() { s.giveUpOn(token, exp) })
	s.pending[token] = exp
	live := s.sessions[key] != nil
	s.mu.Unlock()

	// Said after the watchdog is armed but outside the lock, because publishing
	// can reserve a sequence block and the media path must never wait on that.
	// An arrival in this window disarms the watchdog, so the report stays
	// honest; the worst case is a CONNECTING that lands a beat after the
	// stream's own LIVE. A stream already live on this channel is not
	// connecting either — a re-attach finds it there and must not move the
	// panel backwards.
	if !live {
		if actor, ok := s.cfg.Transcripts.Lookup(c.CallID); ok {
			actor.State(transcript.StateConnecting, "", nil)
		}
	}
}

// arrivedLocked cancels the expectation: the stream connected. When the stream
// won the race and got here first, it is remembered so the Expect that follows
// is a no-op. A stream that arrives after its grace gave up is no longer one a
// re-attach may replace. The caller holds s.mu.
func (s *Server) arrivedLocked(token string) {
	delete(s.gaveUp, token)
	if s.stopExpectingLocked(token) {
		return
	}
	s.arrivals[token] = time.Now()
}

// Revoke retires an attachment whose stream has not arrived, so that neither
// its watchdog nor a late dial-back can speak for it.
//
// The stop command a detach sends first can be overtaken by a `start` the
// switch had not yet run when it timed out (#302, #312). The token is the only
// thing that still distinguishes that late stream from the next attachment on
// the same channel, so revocation is per token and never per channel.
func (s *Server) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopExpectingLocked(token)
	delete(s.arrivals, token)
	delete(s.gaveUp, token)
	now := time.Now()
	s.pruneLocked(now)
	s.revoked[token] = now
}

// refusedLocked cancels the expectation for a stream that did dial back and was
// turned away here. The caller holds s.mu.
//
// The reason is logged where the decision was made; what this prevents is the
// watchdog reporting the same stream twelve seconds later as one that never
// connected. That is a different fault with a different cause — the switch
// never dialled back at all — and an operator who reads both accounts of one
// event goes looking for a network problem that is not there.
//
// A refusal can beat the attach's `+OK` just as an acceptance can (#287), so
// with nothing yet expected it is recorded as an arrival: the stream did dial
// back, and the Expect that follows must be the same no-op it is for a stream
// that was let in.
func (s *Server) refusedLocked(token string) {
	if s.stopExpectingLocked(token) {
		return
	}
	if _, gone := s.revoked[token]; gone {
		return
	}
	s.arrivals[token] = time.Now()
}

// stopExpectingLocked disarms the token's watchdog and reports whether one was
// armed. The caller holds s.mu.
func (s *Server) stopExpectingLocked(token string) bool {
	exp := s.pending[token]
	if exp == nil {
		return false
	}
	exp.timer.Stop()
	delete(s.pending, token)
	return true
}

// hasGivenUp reports whether this attachment's connect grace ran out without
// its stream arriving, which is what lets a re-attach replace it.
func (s *Server) hasGivenUp(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, gave := s.gaveUp[token]
	return gave
}

// giveUpOn reports a tap whose attach was sent and whose stream never dialled
// back. Silence is the one thing this must not do.
//
// It reports only when the expectation it was armed for is still the live one:
// a timer that was disarmed by the attach's failure or by a detach, or that a
// later attach superseded, has nothing to say. A revocation always disarms the
// expectation first, so a revoked token never gets past that check.
//
// It does not forget or revoke the attachment: a stalled switch may still run
// the start, and that stream must then be owned and move the panel from ERROR
// to LIVE. It only marks the token as given up, so that a re-attach of the
// same leg is allowed to replace it.
func (s *Server) giveUpOn(token string, exp *expectation) {
	s.mu.Lock()
	if s.pending[token] != exp {
		s.mu.Unlock()
		return
	}
	delete(s.pending, token)
	if _, connected := s.sessions[exp.key]; connected {
		s.mu.Unlock()
		return
	}
	s.gaveUp[token] = time.Now()
	s.mu.Unlock()

	s.log.Error("a tap was attached but never connected",
		"callId", exp.claim.CallID, "channelId", exp.claim.Channel, "after", connectGrace)
	if actor, ok := s.cfg.Transcripts.Lookup(exp.claim.CallID); ok {
		actor.State(transcript.StateError, "STREAM_NEVER_CONNECTED", nil)
	}
}

// nonce is 128 random bits as hex; it is a token, not an id.
func nonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
