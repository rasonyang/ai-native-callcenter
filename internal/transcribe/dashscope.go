// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DashScope's native duplex protocol: run-task / task-started /
// result-generated / finish-task, a header+payload envelope carrying a
// task_id, and audio as raw binary frames rather than base64 in JSON.
//
// Everything here was measured against the live service (design 08, B.3b)
// rather than taken from the reference documentation, which describes the
// OpenAI-compatible interface this model is *not* served on.

const (
	dsKeepalive   = 15 * time.Second
	dsReadTimeout = 45 * time.Second
	dsWriteWait   = 5 * time.Second
	dsDialTimeout = 5 * time.Second
	// dsStartTimeout bounds the wait for task-started.
	//
	// Measured at ~5s against the Beijing MaaS host on 2026-08-18, which is
	// most of this budget — it is a bound on a service that is slow, not on
	// one that is broken, so it is generous rather than tight. A task that
	// never starts is an error state, and the alternative to waiting is what
	// this replaced: streaming audio into a task that does not exist yet.
	dsStartTimeout = 20 * time.Second
	// dsSentenceSilence is how long a pause must be before the server closes a
	// sentence. The documented default is 1300ms; a call transcript wants to
	// keep up with the conversation rather than lag a beat behind it.
	dsSentenceSilence = 800
)

type dsHeader struct {
	Action       string         `json:"action,omitempty"`
	TaskID       string         `json:"task_id"`
	Streaming    string         `json:"streaming,omitempty"`
	Event        string         `json:"event,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"`
	ErrorMessage string         `json:"error_message,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

type dsRunTask struct {
	Header  dsHeader `json:"header"`
	Payload struct {
		TaskGroup  string         `json:"task_group"`
		Task       string         `json:"task"`
		Function   string         `json:"function"`
		Model      string         `json:"model"`
		Parameters map[string]any `json:"parameters"`
		Input      map[string]any `json:"input"`
	} `json:"payload"`
}

type dsSentence struct {
	BeginTime     *int   `json:"begin_time"`
	EndTime       *int   `json:"end_time"`
	Text          string `json:"text"`
	SentenceBegin bool   `json:"sentence_begin"`
	SentenceEnd   bool   `json:"sentence_end"`
	SentenceID    int    `json:"sentence_id"`
	Heartbeat     bool   `json:"heartbeat"`
}

type dsEnvelope struct {
	Header  dsHeader `json:"header"`
	Payload struct {
		Output struct {
			Sentence *dsSentence `json:"sentence"`
		} `json:"output"`
	} `json:"payload"`
}

type dashscope struct {
	profile Profile
	apiKey  string
	log     Logger

	conn   *websocket.Conn
	taskID string

	events chan Event
	// writes serialises sends: audio comes from the media path while control
	// frames come from Start and Close, and a websocket permits one writer.
	writes sync.Mutex

	closeOnce sync.Once
	done      chan struct{}
	// failed marks a task-failed, after which the connection is documented as
	// closed and not reusable — so nothing else is attempted on it.
	failed bool
	mu     sync.Mutex

	// started closes when task-started arrives; startFail carries a task-failed
	// that beat it. Start blocks on one or the other, because audio sent
	// before the task exists is discarded by the service and nothing says so.
	started   chan struct{}
	startOnce sync.Once
	startFail error

	// wire is a raw frame log for diagnosis; nil unless AICC_TRANSCRIBE_WIRE
	// names a directory. Every text frame in both directions is written
	// verbatim, because a summary of a protocol you are debugging is a summary
	// written by the assumption you are testing.
	wire   *os.File
	audioN int
}

func newDashscope(p Profile, apiKey string, log Logger) *dashscope {
	return &dashscope{
		profile: p,
		apiKey:  apiKey,
		log:     log,
		events:  make(chan Event, 32),
		done:    make(chan struct{}),
		started: make(chan struct{}),
	}
}

func newTaskID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *dashscope) Events() <-chan Event { return d.events }

// ErrTaskNeverStarted reports that the recogniser accepted the connection and
// never started the task. Distinct from a dial or a protocol failure: the
// socket is fine and the service simply has not answered, which is the case
// that used to be indistinguishable from a healthy session.
var ErrTaskNeverStarted = errors.New("transcribe: the recognition task never started")

// markStarted releases Start. err non-nil means the task failed instead.
func (d *dashscope) markStarted(err error) {
	d.startOnce.Do(func() {
		d.startFail = err
		close(d.started)
	})
}

func (d *dashscope) Start(ctx context.Context, cfg Config) error {
	dialCtx, cancel := context.WithTimeout(ctx, dsDialTimeout)
	defer cancel()

	dialer := websocket.Dialer{HandshakeTimeout: dsDialTimeout}
	conn, resp, err := dialer.DialContext(dialCtx, d.profile.Endpoint, http.Header{
		"Authorization": {"Bearer " + d.apiKey},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("transcribe: dial %s: %w (HTTP %s)", d.profile.Name, err, resp.Status)
		}
		return fmt.Errorf("transcribe: dial %s: %w", d.profile.Name, err)
	}
	d.conn = conn
	d.taskID = newTaskID()
	d.openWire()
	_ = conn.SetReadDeadline(time.Now().Add(dsReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(dsReadTimeout))
	})

	params := map[string]any{
		"format":                       "pcm",
		"sample_rate":                  d.profile.SampleRate,
		"semantic_punctuation_enabled": true,
		"max_sentence_silence":         dsSentenceSilence,
		// A call has long silences by nature — one party listens while the
		// other talks — and without this the service may close an idle task.
		"heartbeat": true,
	}
	if cfg.Language != "" {
		params["language_hints"] = []string{cfg.Language}
	}
	if len(cfg.Hints) > 0 {
		vocabulary := make(map[string]int, len(cfg.Hints))
		for _, h := range cfg.Hints {
			vocabulary[h] = 4
		}
		params["vocabulary"] = vocabulary
	}

	var run dsRunTask
	run.Header = dsHeader{Action: "run-task", TaskID: d.taskID, Streaming: "duplex"}
	run.Payload.TaskGroup, run.Payload.Task, run.Payload.Function = "audio", "asr", "recognition"
	run.Payload.Model = d.profile.Model
	run.Payload.Parameters = params
	run.Payload.Input = map[string]any{}

	if err := d.writeJSON(&run); err != nil {
		conn.Close()
		return fmt.Errorf("transcribe: run-task: %w", err)
	}

	go d.readLoop()
	go d.keepalive()

	// Block until the service says the task exists. Audio written before
	// task-started is discarded by DashScope, and nothing reports it: the
	// socket stays open, no error arrives, and the session looks healthy while
	// every frame is thrown away. Measured live on 2026-08-18 — 245 frames,
	// 4.9 seconds, no result ever returned.
	//
	// This is also what makes LIVE mean the recogniser is running rather than
	// "we wrote run-task", which is why a total failure read green.
	select {
	case <-d.started:
		if d.startFail != nil {
			_ = d.Close(ctx)
			return d.startFail
		}
	case <-time.After(dsStartTimeout):
		_ = d.Close(ctx)
		return fmt.Errorf("%w after %s", ErrTaskNeverStarted, dsStartTimeout)
	case <-ctx.Done():
		_ = d.Close(ctx)
		return ctx.Err()
	}
	return nil
}

// openWire starts a raw frame log when AICC_TRANSCRIBE_WIRE names a directory.
func (d *dashscope) openWire() {
	dir := os.Getenv("AICC_TRANSCRIBE_WIRE")
	if dir == "" {
		return
	}
	f, err := os.Create(filepath.Join(dir, "ds-"+d.taskID[:8]+".log"))
	if err != nil {
		d.log.Warn("transcribe: cannot open the wire log", "error", err)
		return
	}
	d.wire = f
}

func (d *dashscope) traceWire(dir string, b []byte) {
	if d.wire == nil {
		return
	}
	fmt.Fprintf(d.wire, "%s %s %s\n", time.Now().Format("15:04:05.000"), dir, b)
}

func (d *dashscope) writeJSON(v any) error {
	if d.wire != nil {
		if b, err := json.Marshal(v); err == nil {
			d.traceWire("OUT", b)
		}
	}
	d.writes.Lock()
	defer d.writes.Unlock()
	_ = d.conn.SetWriteDeadline(time.Now().Add(dsWriteWait))
	return d.conn.WriteJSON(v)
}

// SendAudio writes one chunk as a raw binary frame. There is no base64 and no
// JSON envelope on this protocol, which is why the per-frame path here does no
// encoding at all.
func (d *dashscope) SendAudio(pcm16 []byte) error {
	select {
	case <-d.done:
		return errors.New("transcribe: session closed")
	default:
	}
	d.mu.Lock()
	failed := d.failed
	d.mu.Unlock()
	if failed {
		return errors.New("transcribe: session failed")
	}

	d.writes.Lock()
	defer d.writes.Unlock()
	if d.wire != nil {
		d.audioN++
		if d.audioN%50 == 1 {
			d.traceWire("OUT", []byte(fmt.Sprintf("<audio frame #%d, %d bytes>", d.audioN, len(pcm16))))
		}
	}
	_ = d.conn.SetWriteDeadline(time.Now().Add(dsWriteWait))
	return d.conn.WriteMessage(websocket.BinaryMessage, pcm16)
}

func (d *dashscope) keepalive() {
	t := time.NewTicker(dsKeepalive)
	defer t.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-t.C:
			d.writes.Lock()
			_ = d.conn.SetWriteDeadline(time.Now().Add(dsWriteWait))
			err := d.conn.WriteMessage(websocket.PingMessage, nil)
			d.writes.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (d *dashscope) readLoop() {
	// A read loop that ends without task-started releases Start with the
	// reason rather than leaving it to time out on a socket that is gone.
	defer d.markStarted(ErrTaskNeverStarted)
	defer d.finish(nil)
	for {
		mt, data, err := d.conn.ReadMessage()
		if err != nil {
			select {
			case <-d.done: // an expected close
			default:
				d.emit(Event{Type: EventError, Err: fmt.Errorf("transcribe: read: %w", err)})
			}
			return
		}
		_ = d.conn.SetReadDeadline(time.Now().Add(dsReadTimeout))
		if mt != websocket.TextMessage {
			d.traceWire("IN", []byte(fmt.Sprintf("<binary frame, %d bytes>", len(data))))
			continue
		}
		d.traceWire("IN", data)

		var env dsEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			d.log.Warn("transcribe: undecodable frame", "provider", d.profile.Name, "error", err)
			continue
		}

		switch env.Header.Event {
		case "task-started":
			d.log.Debug("transcribe: task started", "provider", d.profile.Name)
			d.markStarted(nil)
		case "result-generated":
			d.onSentence(env.Payload.Output.Sentence)
		case "task-finished":
			return
		case "task-failed":
			d.mu.Lock()
			d.failed = true
			d.mu.Unlock()
			failure := fmt.Errorf("transcribe: %s: %s",
				env.Header.ErrorCode, env.Header.ErrorMessage)
			// Releases Start when the failure beat task-started; a no-op
			// afterwards, when the task had already begun.
			d.markStarted(failure)
			d.emit(Event{Type: EventError, Err: failure})
			return
		}
	}
}

func (d *dashscope) onSentence(s *dsSentence) {
	if s == nil {
		return
	}
	// A heartbeat keeps an idle task alive and carries no speech. It is
	// documented as skippable and arrives with sentence_id 0.
	if s.Heartbeat {
		return
	}
	// An empty final is real — leading silence produces one — and it must not
	// reach the transcript actor, because the actor allocates a seq for what it
	// accepts and a seq spent on silence puts a permanent hole between an
	// agent's snapshot and their live tail. Dropped here as well as there: the
	// actor is the last guard, this is the first.
	if s.Text == "" {
		return
	}

	ev := Event{
		// sentence_id is monotonic from 1 within a task, which is exactly the
		// identity a partial and its final need to share.
		UtteranceID: fmt.Sprintf("%s-%d", d.taskID[:8], s.SentenceID),
		Text:        s.Text,
	}
	if s.BeginTime != nil {
		ev.StartedAtMs = *s.BeginTime
	}
	if s.EndTime != nil {
		ev.EndedAtMs = *s.EndTime
	}
	if s.SentenceEnd {
		ev.Type = EventFinal
	} else {
		ev.Type = EventPartial
	}
	d.emit(ev)
}

// emit never blocks the read loop. A consumer that has stopped reading is a
// bug upstream, and stalling the socket would turn it into a dead session.
func (d *dashscope) emit(ev Event) {
	select {
	case d.events <- ev:
	case <-d.done:
	default:
		d.log.Warn("transcribe: event dropped, consumer is not reading",
			"provider", d.profile.Name, "type", ev.Type)
	}
}

func (d *dashscope) Close(ctx context.Context) error {
	var err error
	d.closeOnce.Do(func() {
		d.mu.Lock()
		failed := d.failed
		d.mu.Unlock()

		// finish-task asks the server for any last result. A failed task
		// documents its connection as unusable, so nothing is sent on it.
		if !failed && d.conn != nil {
			_ = d.writeJSON(map[string]any{
				"header":  dsHeader{Action: "finish-task", TaskID: d.taskID, Streaming: "duplex"},
				"payload": map[string]any{"input": map[string]any{}},
			})
		}
		close(d.done)
		if d.conn != nil {
			err = d.conn.Close()
		}
	})
	return err
}

// finish closes the event channel exactly once, so a consumer ranging over it
// terminates however the session ended.
func (d *dashscope) finish(err error) {
	if err != nil {
		d.emit(Event{Type: EventError, Err: err})
	}
	d.emit(Event{Type: EventClosed})
	close(d.events)
}
