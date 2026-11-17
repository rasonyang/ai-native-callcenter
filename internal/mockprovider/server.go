// SPDX-License-Identifier: Apache-2.0

// Package mockprovider is a stand-in voice provider for load testing.
//
// It speaks the Realtime protocol over a WebSocket, which means the
// application reaches it the way it reaches any provider — through
// AICC_PROVIDER_ENDPOINT — and runs its real client against it: the same
// handshake, the same JSON, the same base64 audio path. Nothing in the
// application knows it is under test.
//
// That is deliberately different from the in-process fake design 06 §7
// imagined. A fake VoiceSession would have skipped exactly the layers a load
// test is meant to stress, and the endpoint override already exists as a
// documented extension point. What both designs share is the point of it: two
// hundred concurrent conversations with no wide-area network and no bill.
package mockprovider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// Config shapes the conversation every session has. The defaults describe a
// provider that behaves well: it answers quickly and says something short.
type Config struct {
	// FirstAudio is the wait between accepting a turn and the first audio of
	// it. The application's own watchdog gives up at three seconds.
	FirstAudio time.Duration
	// TurnAudio is how much speech a turn is worth.
	TurnAudio time.Duration
	// TurnEvery is the gap between one turn ending and the next beginning —
	// the caller's share of the conversation.
	TurnEvery time.Duration
	// SpeechBefore is how long the caller is heard speaking before a turn.
	// Zero skips the speech events entirely.
	SpeechBefore time.Duration

	// DeltaAudio is how much audio rides in one delta, and DeltaPace how long
	// the provider waits between them. Real providers deliver a turn far
	// faster than it plays, which is what fills the application's send queue,
	// so the default is a burst rather than real time.
	DeltaAudio time.Duration
	DeltaPace  time.Duration

	// Scenario switches the server to scripted sessions (scenario.go). Nil is
	// the default mode: a turn on a timer, never a tool.
	Scenario *Scenario
	// ResponseGrace is how long AWAIT_TOOL_RESULT waits for the
	// response.create that follows a tool result before continuing without
	// it. Default DefaultResponseGrace.
	ResponseGrace time.Duration

	Log *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.ResponseGrace <= 0 {
		c.ResponseGrace = DefaultResponseGrace
	}
	if c.FirstAudio <= 0 {
		c.FirstAudio = 400 * time.Millisecond
	}
	if c.TurnAudio <= 0 {
		c.TurnAudio = 4 * time.Second
	}
	if c.TurnEvery <= 0 {
		c.TurnEvery = 6 * time.Second
	}
	if c.DeltaAudio <= 0 {
		c.DeltaAudio = 100 * time.Millisecond
	}
	if c.DeltaPace < 0 {
		c.DeltaPace = 0
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Server accepts Realtime sessions. One instance serves every call.
type Server struct {
	cfg      Config
	upgrader websocket.Upgrader

	sessions atomic.Int64
	peak     atomic.Int64
	total    atomic.Int64
	turns    atomic.Int64
	cut      atomic.Int64 // turns that ended without a completion
	audioSec atomic.Int64 // milliseconds of audio sent, summed

	// Scenario mode.
	toolCalls     atomic.Int64 // tool requests sent
	toolResults   atomic.Int64 // tool results received
	scriptsDone   atomic.Int64
	scriptsFailed atomic.Int64
	unmatched     atomic.Int64 // sessions no script claimed
	// resultsWithoutCreate counts awaited results with no response.create
	// after them inside the grace period.
	resultsWithoutCreate atomic.Int64

	// meters are the uplink meters of the most recent sessions, oldest first.
	metersMu sync.Mutex
	meters   []*uplinkMeter
}

// New prepares a server. The audio it speaks is built once here and shared:
// two hundred sessions saying the same thing need one copy of it.
func New(cfg Config) *Server {
	cfg.applyDefaults()
	return &Server{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
	}
}

// Stats reports what the server has served.
type Stats struct {
	Live     int64   `json:"live"`
	Peak     int64   `json:"peak"`
	Total    int64   `json:"total"`
	Turns    int64   `json:"turns"`
	Cut      int64   `json:"cut"`
	AudioSec float64 `json:"audioSec"`

	// Scenario mode only.
	ToolCalls     int64 `json:"toolCalls"`
	ToolResults   int64 `json:"toolResults"`
	ScriptsDone   int64 `json:"scriptsDone"`
	ScriptsFailed int64 `json:"scriptsFailed"`
	Unmatched     int64 `json:"unmatched"`
	// ResultsWithoutCreate counts tool results the script continued past
	// after the grace period, with no response.create following.
	ResultsWithoutCreate int64 `json:"resultsWithoutCreate"`

	// Sessions is what each of the most recent sessions heard of the
	// application's uplink; served only by GET StatsPath?sessions=1, never by
	// Stats, which a load run polls.
	Sessions []SessionStat `json:"sessions,omitempty"`
}

// SessionStats is the uplink record of the most recent sessions (at most
// maxSessionStats), oldest first.
func (s *Server) SessionStats() []SessionStat {
	s.metersMu.Lock()
	meters := append([]*uplinkMeter(nil), s.meters...)
	s.metersMu.Unlock()
	out := make([]SessionStat, 0, len(meters))
	for _, m := range meters {
		out = append(out, m.snapshot())
	}
	return out
}

func (s *Server) addMeter(m *uplinkMeter) {
	s.metersMu.Lock()
	defer s.metersMu.Unlock()
	if len(s.meters) >= maxSessionStats {
		s.meters = append(s.meters[:0], s.meters[1:]...)
	}
	s.meters = append(s.meters, m)
}

func (s *Server) Stats() Stats {
	return Stats{
		Live:     s.sessions.Load(),
		Peak:     s.peak.Load(),
		Total:    s.total.Load(),
		Turns:    s.turns.Load(),
		Cut:      s.cut.Load(),
		AudioSec: float64(s.audioSec.Load()) / 1000,

		ToolCalls:     s.toolCalls.Load(),
		ToolResults:   s.toolResults.Load(),
		ScriptsDone:   s.scriptsDone.Load(),
		ScriptsFailed: s.scriptsFailed.Load(),
		Unmatched:     s.unmatched.Load(),

		ResultsWithoutCreate: s.resultsWithoutCreate.Load(),
	}
}

// StatsPath is where Handler serves the counters as JSON. Real providers have
// no such path, so a session can never be mistaken for a stats request.
const StatsPath = "/_stats"

// Handler is the server as the application reaches it, plus a read-only
// GET StatsPath that reports Stats as JSON, so a test outside the process can
// ask how many sessions were ever opened.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+StatsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		stats := s.Stats()
		if r.URL.Query().Get("sessions") == "1" {
			stats.Sessions = s.SessionStats()
		}
		_ = json.NewEncoder(w).Encode(stats)
	})
	mux.Handle("/", s)
	return mux
}

// ServeHTTP upgrades any path to a session. Real providers select the model
// from the query string; this one answers for whatever it is asked.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.cfg.Log.Warn("mockprovider: upgrade failed", "error", err)
		return
	}
	live := s.sessions.Add(1)
	seq := s.total.Add(1)
	for {
		peak := s.peak.Load()
		if live <= peak || s.peak.CompareAndSwap(peak, live) {
			break
		}
	}
	defer s.sessions.Add(-1)

	sess := newSession(s, conn)
	sess.meter = newUplinkMeter(seq)
	s.addMeter(sess.meter)
	defer sess.meter.close()
	sess.run()
}

// session is one conversation.
type session struct {
	server *Server
	conn   *websocket.Conn
	log    *slog.Logger

	writeMu sync.Mutex

	mu        sync.Mutex
	responded int
	// cancelTurn stops the turn in flight, if any.
	cancelTurn chan struct{}

	done chan struct{}

	// scn is set in scenario mode only.
	scn *scenarioState

	// meter measures the application's uplink.
	meter *uplinkMeter
}

func newSession(server *Server, conn *websocket.Conn) *session {
	s := &session{
		server: server,
		conn:   conn,
		log:    server.cfg.Log,
		done:   make(chan struct{}),
	}
	if server.cfg.Scenario != nil {
		s.scn = &scenarioState{results: make(chan scnSignal, 16)}
	}
	return s
}

func (s *session) run() {
	defer s.conn.Close()
	defer close(s.done)

	s.send(map[string]any{"type": "session.created", "session": map[string]any{"id": "mock"}})

	// The caller drives the first turn; after that the provider drives them,
	// the way server-side turn detection does.
	if s.scn == nil {
		go s.converse()
	}

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &event) != nil {
			continue
		}
		switch event.Type {
		case "input_audio_buffer.append":
			s.meter.noteAppend(data)
		case "session.update":
			s.meter.noteSessionUpdate(data)
		case "response.create":
			s.meter.noteResponseCreate()
		}
		if s.scn != nil && s.observe(event.Type, data) {
			continue
		}
		switch event.Type {
		case "session.update":
			s.send(map[string]any{"type": "session.updated", "session": map[string]any{"id": "mock"}})
		case "response.create":
			go s.respond()
		case "response.cancel":
			s.stopTurn()
		default:
			// input_audio_buffer.append, conversation.item.*: the uplink is
			// read and dropped. Counting it would be the only reason to keep
			// it, and the socket read already did the work that costs.
		}
	}
}

// converse produces a turn every TurnEvery, the way a provider with its own
// turn detection does once the caller stops speaking.
func (s *session) converse() {
	ticker := time.NewTicker(s.server.cfg.TurnEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.respond()
		}
	}
}

// respond plays one turn: speech detected, a response created, audio, done.
func (s *session) respond() {
	cfg := s.server.cfg

	s.mu.Lock()
	if s.cancelTurn != nil {
		s.mu.Unlock() // a turn is already in flight
		return
	}
	cancel := make(chan struct{})
	s.cancelTurn = cancel
	s.responded++
	id := fmt.Sprintf("resp_%d", s.responded)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.cancelTurn == cancel {
			s.cancelTurn = nil
		}
		s.mu.Unlock()
	}()

	if cfg.SpeechBefore > 0 {
		s.send(map[string]any{"type": "input_audio_buffer.speech_started"})
		if !s.wait(cfg.SpeechBefore, cancel) {
			s.server.cut.Add(1)
			return
		}
		s.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	}

	s.send(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
	s.send(map[string]any{
		"type": "response.output_item.added",
		"item": map[string]any{"id": "item_" + id, "type": "message"},
	})

	if !s.wait(cfg.FirstAudio, cancel) {
		s.server.cut.Add(1)
		s.log.Warn("mockprovider: turn cut before any audio", "response", id)
		return
	}

	frame := deltaFrame(cfg.DeltaAudio)
	encoded := base64.StdEncoding.EncodeToString(frame)
	deltas := int(cfg.TurnAudio / cfg.DeltaAudio)
	if deltas < 1 {
		deltas = 1
	}
	for range deltas {
		if err := s.send(map[string]any{
			"type": "response.output_audio.delta", "delta": encoded,
		}); err != nil {
			s.server.cut.Add(1)
			s.log.Warn("mockprovider: write failed mid-turn", "response", id, "error", err)
			return
		}
		s.server.audioSec.Add(cfg.DeltaAudio.Milliseconds())
		if cfg.DeltaPace > 0 && !s.wait(cfg.DeltaPace, cancel) {
			s.server.cut.Add(1)
			return
		}
		select {
		case <-cancel:
			s.send(map[string]any{
				"type":     "response.done",
				"response": map[string]any{"id": id, "status": "cancelled"},
			})
			return
		default:
		}
	}

	s.send(map[string]any{
		"type": "response.output_audio_transcript.done", "transcript": "mock turn",
	})
	s.send(map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"id": id, "status": "completed",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		},
	})
	s.server.turns.Add(1)
}

// stopTurn ends the turn in flight. A provider that ignored a cancel would
// leave the model talking over the caller, which is the bug this exercises.
func (s *session) stopTurn() {
	s.mu.Lock()
	cancel := s.cancelTurn
	s.mu.Unlock()
	if cancel != nil {
		select {
		case <-cancel:
		default:
			close(cancel)
		}
	}
}

// wait sleeps unless the turn is cancelled or the session ends first.
func (s *session) wait(d time.Duration, cancel chan struct{}) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-cancel:
		return false
	case <-s.done:
		return false
	}
}

func (s *session) send(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

// deltaFrame is one delta's worth of µ-law audio: a 440 Hz tone, so anyone
// listening to a load test hears something recognisable rather than silence
// or noise. Built once per size and shared.
var deltaFrames sync.Map // time.Duration -> []byte

func deltaFrame(d time.Duration) []byte {
	if cached, ok := deltaFrames.Load(d); ok {
		return cached.([]byte)
	}
	samples := int(d.Seconds() * float64(media.RateTelephone))
	pcm := make([]int16, samples)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(media.RateTelephone)))
	}
	converter, err := media.NewConverter(
		media.PCM16Format(media.RateTelephone), media.G711Format(media.LawMu))
	if err != nil {
		panic(err) // both formats are constants; this cannot fail
	}
	frame := converter.Convert(make([]byte, 0, samples), media.PCM16ToBytes(nil, pcm))
	deltaFrames.Store(d, frame)
	return frame
}
