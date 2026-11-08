// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// RelayConfig configures the fault-injecting relay.
type RelayConfig struct {
	// Upstream is the real provider's WebSocket URL, without the query: the
	// client's own query (?model=...) is appended per connection.
	Upstream string
	Fault    Fault
	// Ratio is the fraction of sessions that are faulted, 0..1.
	Ratio float64
	Turns TurnRange
	Seed  int64
	// Hold is how long stall-hold keeps the held response.done when neither a
	// barge-in nor anything else releases it.
	Hold time.Duration
	// StallTimeout is how long stall waits for the client's response.cancel
	// before delivering the held response.done as it came.
	StallTimeout time.Duration
	Log          *slog.Logger
}

// errNoActiveResponse is how the real provider refuses a response.cancel that
// arrives with nothing to cancel; the relay hides it because the client never
// asked the provider anything the relay did not forward on its behalf.
const errNoActiveResponse = "no active response"

// refusedWhileBusy is qwen's refusal of response.create during a response.
const refusedWhileBusy = `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Cannot create response while another response is in progress."}}`

// refusedVADTurn is the error behind #197.
const refusedVADTurn = `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Server VAD turn committed but no response was created because a manual response is already in progress."}}`

// cancelRefusalWindow bounds how long after a relayed response.cancel the
// provider's "no active response" refusal is hidden.
const cancelRefusalWindow = 3 * time.Second

// Relay forwards Realtime WebSockets to a real provider, injecting one fault
// per faulted session.
type Relay struct {
	cfg      RelayConfig
	upgrader websocket.Upgrader
	dialer   websocket.Dialer
	sessions atomic.Int64
}

// NewRelay builds a relay, filling defaults for the unset durations.
func NewRelay(cfg RelayConfig) *Relay {
	if cfg.Hold <= 0 {
		cfg.Hold = 20 * time.Second
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 30 * time.Second
	}
	if cfg.Turns.Lo < 1 {
		cfg.Turns = TurnRange{Lo: 2, Hi: 4}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Relay{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
		dialer: websocket.Dialer{HandshakeTimeout: 10 * time.Second},
	}
}

// Handler accepts a client WebSocket on any path.
func (r *Relay) Handler() http.Handler {
	return http.HandlerFunc(r.serve)
}

func (r *Relay) serve(w http.ResponseWriter, req *http.Request) {
	n := int(r.sessions.Add(1))
	log := r.cfg.Log.With("session", n)

	target := r.cfg.Upstream
	if q := req.URL.RawQuery; q != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + q
	}
	// The key is forwarded, never logged.
	header := http.Header{}
	for _, name := range []string{"Authorization", "OpenAI-Beta"} {
		if v := req.Header.Get(name); v != "" {
			header.Set(name, v)
		}
	}
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	up, resp, err := r.dialer.DialContext(ctx, target, header)
	cancel()
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		log.Error("relay upstream dial failed", "err", err, "status", status)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	cli, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		_ = up.Close()
		log.Error("relay client upgrade failed", "err", err)
		return
	}

	s := newRelaySession(r.cfg, n, log, cli, up)
	log.Info("relay session start", "faulted", s.faulted, "fault", s.fault(),
		"turn", s.chosenTurn, "path", req.URL.Path)
	s.run()
	log.Info("relay session end")
}

// writer serialises writes to one WebSocket: gorilla allows a single writer.
type writer struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *writer) write(mt int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.WriteMessage(mt, data)
}

type phase int

const (
	phaseIdle     phase = iota // the chosen turn has not started
	phaseActive                // chosen response created, done not seen
	phaseHeld                  // done seen and withheld
	phaseReleased              // done delivered; the fault is spent
)

type relaySession struct {
	cfg RelayConfig
	n   int
	log *slog.Logger

	cli, up *writer

	faulted    bool
	chosenTurn int

	mu sync.Mutex
	// Conversation tracking.
	sawSessionUpdated bool
	vadPending        bool // speech stopped or committed since the last response.created
	clientCreated     bool // the client sent response.create since the last response.created
	turn              int  // caller turns the VAD answered, seen so far
	spent             bool // the fault has been injected

	// The faulted response.
	phase            phase
	respID           string
	heldDone         []byte
	releaseEarly     string // a release reason that arrived before the done did
	releaseCancelled bool
	gen              int // invalidates a timer once the response is released
	swallowItems     map[string]bool
	swallowDone      bool // unanswered/vad-refused: response.done seen

	// Refusals the relay hides.
	hideCancelRefusalUntil time.Time
	deletesOutstanding     map[string]bool // item ids whose delete has no reply yet
	hideDeleteErrorsUntil  time.Time
}

func newRelaySession(cfg RelayConfig, n int, log *slog.Logger, cli, up *websocket.Conn) *relaySession {
	s := &relaySession{
		cfg: cfg, n: n, log: log,
		cli: &writer{conn: cli}, up: &writer{conn: up},
		swallowItems: map[string]bool{}, deletesOutstanding: map[string]bool{},
	}
	if cfg.Fault != FaultNone {
		s.faulted, s.chosenTurn = decide(cfg.Seed, n, cfg.Ratio, cfg.Turns)
	}
	return s
}

func (s *relaySession) fault() Fault {
	if !s.faulted {
		return FaultNone
	}
	return s.cfg.Fault
}

func (s *relaySession) run() {
	cli, up := s.cli.conn, s.up.conn
	// Pings and pongs cross the relay like everything else.
	for _, pair := range [][2]*websocket.Conn{{cli, up}, {up, cli}} {
		from, to := pair[0], pair[1]
		from.SetPingHandler(func(d string) error {
			return to.WriteControl(websocket.PingMessage, []byte(d), time.Now().Add(time.Second))
		})
		from.SetPongHandler(func(d string) error {
			return to.WriteControl(websocket.PongMessage, []byte(d), time.Now().Add(time.Second))
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.pump(s.cli.conn, s.cli, s.up, s.fromClient)
		s.shutdown()
	}()
	go func() {
		defer wg.Done()
		s.pump(s.up.conn, s.up, s.cli, s.fromUpstream)
		s.shutdown()
	}()
	wg.Wait()
}

// shutdown closes both sides; the pump blocked on the other side then fails
// its read and ends too.
func (s *relaySession) shutdown() {
	s.mu.Lock()
	s.gen++
	s.mu.Unlock()
	_ = s.cli.conn.Close()
	_ = s.up.conn.Close()
}

// pump reads one side and hands text frames to the filter, which returns what
// goes to the peer (dst) and what goes back to the sender's side. Binary
// frames pass untouched.
func (s *relaySession) pump(src *websocket.Conn, srcW, dst *writer, filter func([]byte) (toPeer, toSrc [][]byte)) {
	for {
		mt, data, err := src.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				_ = dst.conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(ce.Code, ce.Text), time.Now().Add(time.Second))
			}
			return
		}
		if mt != websocket.TextMessage {
			if dst.write(mt, data) != nil {
				return
			}
			continue
		}
		toPeer, toSrc := filter(data)
		for _, m := range toPeer {
			if dst.write(websocket.TextMessage, m) != nil {
				return
			}
		}
		if len(toSrc) > 0 {
			// Frames for the sender's own side go through the writer that
			// owns that socket.
			for _, m := range toSrc {
				if srcW.write(websocket.TextMessage, m) != nil {
					return
				}
			}
		}
	}
}

// envelope is the part of a Realtime event the relay reads.
type envelope struct {
	Type       string `json:"type"`
	ResponseID string `json:"response_id"`
	ItemID     string `json:"item_id"`
	Item       struct {
		ID string `json:"id"`
	} `json:"item"`
	Response struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []struct {
			ID string `json:"id"`
		} `json:"output"`
	} `json:"response"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parse(data []byte) envelope {
	var e envelope
	_ = json.Unmarshal(data, &e)
	return e
}

func isAudioDelta(t string) bool {
	return t == "response.audio.delta" || t == "response.output_audio.delta"
}

// fromClient filters a client frame: what goes upstream, what goes back to
// the client.
func (s *relaySession) fromClient(data []byte) (toUp, toClient [][]byte) {
	e := parse(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Type {
	case "response.create":
		if s.fault() == FaultStallHold && (s.phase == phaseActive || s.phase == phaseHeld) {
			s.log.Warn("relay fault: response.create refused while held",
				"fault", FaultStallHold, "turn", s.chosenTurn, "responseId", s.respID)
			return nil, [][]byte{[]byte(refusedWhileBusy)}
		}
		s.clientCreated = true
	case "response.cancel":
		if s.phase == phaseActive || s.phase == phaseHeld {
			s.hideCancelRefusalUntil = time.Now().Add(cancelRefusalWindow)
			if s.fault() == FaultStall {
				// The provider honours the cancel: the client is told its
				// response ended cancelled.
				if m := s.releaseLocked("client-cancel", true); m != nil {
					toClient = append(toClient, m)
				}
			} else {
				s.log.Warn("relay fault: response.cancel not honoured",
					"fault", FaultStallHold, "turn", s.chosenTurn, "responseId", s.respID)
			}
		}
	}
	return [][]byte{data}, toClient
}

// deliverToClient writes frames to the client from outside the pump, which is
// how a release triggered by a timer or by the other direction reaches it.
func (s *relaySession) deliverToClient(msgs ...[]byte) {
	for _, m := range msgs {
		if m != nil {
			_ = s.cli.write(websocket.TextMessage, m)
		}
	}
}

// fromUpstream filters an upstream frame: what goes to the client, what goes
// back upstream.
func (s *relaySession) fromUpstream(data []byte) (toClient, toUp [][]byte) {
	e := parse(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	pass := [][]byte{data}

	switch e.Type {
	case "session.updated":
		s.sawSessionUpdated = true
	case "input_audio_buffer.speech_stopped", "input_audio_buffer.committed":
		s.vadPending = true
	case "input_audio_buffer.speech_started":
		if s.fault() == FaultStallHold && s.phase != phaseIdle && s.phase != phaseReleased {
			// qwen cancelled the response on barge-in on that call: the done
			// follows the speech event, as it does on the wire.
			if m := s.releaseLocked("speech-started", true); m != nil {
				return [][]byte{data, m}, nil
			}
		}
	case "error":
		if s.hideRefusalLocked(e) {
			return nil, nil
		}
	case "response.created":
		return s.onCreatedLocked(e, data)
	}

	if s.faulted {
		switch s.cfg.Fault {
		case FaultStall, FaultStallHold:
			return s.stallLocked(e, data, pass)
		case FaultUnanswered, FaultVADRefused:
			return s.swallowLocked(e, data, pass)
		}
	}
	return pass, nil
}

// hideRefusalLocked reports whether an upstream error answers something the
// relay sent on its own account.
func (s *relaySession) hideRefusalLocked(e envelope) bool {
	if time.Now().Before(s.hideCancelRefusalUntil) &&
		strings.Contains(strings.ToLower(e.Error.Message), errNoActiveResponse) {
		return true
	}
	// Only a refusal of a delete the relay sent is hidden; any other error,
	// including the provider's real complaints about the caller's turn, is
	// the evidence a live round counts and always reaches the client.
	if len(s.deletesOutstanding) > 0 && time.Now().Before(s.hideDeleteErrorsUntil) {
		msg := strings.ToLower(e.Error.Message)
		if strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
			strings.Contains(msg, "unknown item") {
			for id := range s.deletesOutstanding {
				delete(s.deletesOutstanding, id)
				break
			}
			return true
		}
	}
	return false
}

// onCreatedLocked counts the turn and, on the chosen one, starts the fault.
func (s *relaySession) onCreatedLocked(e envelope, data []byte) (toClient, toUp [][]byte) {
	isVAD := s.vadPending && !s.clientCreated
	s.vadPending, s.clientCreated = false, false
	pass := [][]byte{data}
	if !s.faulted || s.spent || !s.sawSessionUpdated {
		return pass, nil
	}
	f := s.cfg.Fault
	if !isVAD {
		return pass, nil
	}
	s.turn++
	if s.turn != s.chosenTurn {
		return pass, nil
	}
	if e.Response.ID == "" {
		// Every later event would match an empty id, so the whole stream
		// would be treated as the faulted response. Wait for a turn that can
		// be told apart.
		s.chosenTurn++
		s.log.Warn("relay: response.created has no response id; fault not injected on this turn",
			"fault", f, "turn", s.turn)
		return pass, nil
	}
	s.spent = true
	s.respID = e.Response.ID
	s.phase = phaseActive
	s.gen++
	switch f {
	case FaultStall, FaultStallHold:
		s.log.Warn("relay fault injected", "fault", f, "turn", s.turn,
			"responseId", s.respID, "effect", "audio dropped, response.done held")
		wait := s.cfg.StallTimeout
		if f == FaultStallHold {
			wait = s.cfg.Hold
		}
		s.armLocked(wait)
		return pass, nil
	default:
		s.log.Warn("relay fault injected", "fault", f, "turn", s.turn,
			"responseId", s.respID, "effect", "response swallowed")
		s.hideCancelRefusalUntil = time.Now().Add(cancelRefusalWindow)
		out := [][]byte{}
		if f == FaultVADRefused {
			out = append(out, []byte(refusedVADTurn))
		}
		return out, [][]byte{[]byte(`{"type":"response.cancel"}`)}
	}
}

// armLocked starts the timer that gives the held done back if nothing else
// does. The generation check makes a timer that lost the race a no-op.
func (s *relaySession) armLocked(d time.Duration) {
	gen := s.gen
	time.AfterFunc(d, func() {
		s.mu.Lock()
		if s.gen != gen {
			s.mu.Unlock()
			return
		}
		cancelled := s.cfg.Fault == FaultStallHold
		m := s.releaseLocked("timeout", cancelled)
		s.mu.Unlock()
		s.deliverToClient(m)
	})
}

// releaseLocked gives the held response.done to the client: rewritten to
// cancelled when cancelled is set, as it came otherwise. If the done has not
// arrived yet it is delivered the moment it does, and nil is returned now.
func (s *relaySession) releaseLocked(reason string, cancelled bool) []byte {
	switch s.phase {
	case phaseActive:
		if s.releaseEarly == "" {
			s.releaseEarly = reason
			s.releaseCancelled = cancelled
		}
		return nil
	case phaseHeld:
		return s.deliverHeldLocked(reason, cancelled)
	}
	return nil
}

func (s *relaySession) deliverHeldLocked(reason string, cancelled bool) []byte {
	m := s.heldDone
	if cancelled {
		m = markCancelled(m)
	}
	s.heldDone = nil
	s.phase = phaseReleased
	s.gen++
	s.log.Warn("relay fault: held response.done delivered", "fault", s.cfg.Fault,
		"turn", s.chosenTurn, "responseId", s.respID, "reason", reason, "asCancelled", cancelled)
	return m
}

// markCancelled rewrites a response.done to status "cancelled".
func markCancelled(raw []byte) []byte {
	var ev map[string]json.RawMessage
	if json.Unmarshal(raw, &ev) != nil {
		return raw
	}
	var resp map[string]json.RawMessage
	if json.Unmarshal(ev["response"], &resp) != nil {
		return raw
	}
	resp["status"] = json.RawMessage(`"cancelled"`)
	if b, err := json.Marshal(resp); err == nil {
		ev["response"] = b
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return raw
	}
	return b
}

// stallLocked handles the faulted response of stall and stall-hold.
func (s *relaySession) stallLocked(e envelope, data []byte, pass [][]byte) ([][]byte, [][]byte) {
	if s.phase != phaseActive || !s.belongs(e) {
		return pass, nil
	}
	switch {
	case isAudioDelta(e.Type):
		return nil, nil
	case e.Type == "response.done":
		s.heldDone = data
		s.phase = phaseHeld
		if s.releaseEarly != "" {
			return [][]byte{s.deliverHeldLocked(s.releaseEarly, s.releaseCancelled)}, nil
		}
		return nil, nil
	}
	return pass, nil
}

// belongs reports whether an event is part of the faulted response.
func (s *relaySession) belongs(e envelope) bool {
	return e.ResponseID == s.respID || e.Response.ID == s.respID
}

// swallowLocked hides every event of the faulted response from the client and
// deletes its items once it is done, so the conversation the provider keeps
// is the one the client believes in.
func (s *relaySession) swallowLocked(e envelope, data []byte, pass [][]byte) ([][]byte, [][]byte) {
	if s.respID == "" {
		return pass, nil
	}
	switch {
	case e.Type == "conversation.item.deleted":
		if s.swallowItems[e.ItemID] || s.swallowItems[e.Item.ID] {
			delete(s.deletesOutstanding, e.ItemID)
			delete(s.deletesOutstanding, e.Item.ID)
			return nil, nil
		}
		return pass, nil
	case e.Type == "conversation.item.created" && s.swallowItems[e.Item.ID]:
		return nil, nil
	case e.ResponseID == s.respID || e.Response.ID == s.respID:
	default:
		return pass, nil
	}
	if e.Type == "response.output_item.added" || e.Type == "response.output_item.done" {
		s.swallowItems[e.Item.ID] = true
	}
	if e.Type != "response.done" {
		return nil, nil
	}
	s.swallowDone = true
	s.phase = phaseReleased
	var del [][]byte
	for _, o := range e.Response.Output {
		s.swallowItems[o.ID] = true
	}
	for id := range s.swallowItems {
		if id == "" {
			continue
		}
		msg, _ := json.Marshal(map[string]string{"type": "conversation.item.delete", "item_id": id})
		del = append(del, msg)
		s.deletesOutstanding[id] = true
	}
	s.hideDeleteErrorsUntil = time.Now().Add(cancelRefusalWindow)
	s.log.Warn("relay fault: swallowed response finished, deleting its items",
		"fault", s.cfg.Fault, "turn", s.chosenTurn, "responseId", s.respID, "items", len(del))
	return nil, del
}
