// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// OpenAI's Realtime transcription session. A different grammar from DashScope,
// not a different spelling: session.update, input_audio_buffer.append with
// base64 audio, and deltas that are *increments* to accumulate rather than the
// cumulative text DashScope sends.
//
// Two measured constraints shape this client (design 08, B.2):
//   - the session refuses any rate below 24000
//   - it refuses turn_detection, and silence alone never produces a final,
//     so the utterance boundary is ours to send
const (
	oaKeepalive   = 15 * time.Second
	oaReadTimeout = 45 * time.Second
	oaWriteWait   = 5 * time.Second
	oaDialTimeout = 5 * time.Second
)

type oaEvent struct {
	Type       string `json:"type"`
	ItemID     string `json:"item_id"`
	Delta      string `json:"delta"`
	Text       string `json:"text"`
	Stash      string `json:"stash"`
	Transcript string `json:"transcript"`
	Language   string `json:"language"`
	Error      *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
}

type openairt struct {
	profile Profile
	apiKey  string
	log     Logger

	conn   *websocket.Conn
	events chan Event
	writes sync.Mutex

	closeOnce sync.Once
	done      chan struct{}

	// accumulated holds each in-flight utterance's text. OpenAI sends deltas
	// to append, so the cumulative text the seam promises is assembled here.
	mu          sync.Mutex
	accumulated map[string]string
}

func newOpenAIRT(p Profile, apiKey string, log Logger) *openairt {
	return &openairt{
		profile:     p,
		apiKey:      apiKey,
		log:         log,
		events:      make(chan Event, 32),
		done:        make(chan struct{}),
		accumulated: map[string]string{},
	}
}

func (o *openairt) Events() <-chan Event { return o.events }

func (o *openairt) Start(ctx context.Context, cfg Config) error {
	dialCtx, cancel := context.WithTimeout(ctx, oaDialTimeout)
	defer cancel()

	dialer := websocket.Dialer{HandshakeTimeout: oaDialTimeout}
	conn, resp, err := dialer.DialContext(dialCtx, o.profile.Endpoint, http.Header{
		"Authorization": {"Bearer " + o.apiKey},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("transcribe: dial %s: %w (HTTP %s)", o.profile.Name, err, resp.Status)
		}
		return fmt.Errorf("transcribe: dial %s: %w", o.profile.Name, err)
	}
	o.conn = conn
	_ = conn.SetReadDeadline(time.Now().Add(oaReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(oaReadTimeout))
	})

	transcription := map[string]any{"model": o.profile.Model}
	if cfg.Language != "" {
		transcription["language"] = cfg.Language
	}
	if len(cfg.Hints) > 0 {
		transcription["prompt"] = strings.Join(cfg.Hints, ", ")
	}

	update := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{
				"input": map[string]any{
					"format":        map[string]any{"type": "audio/pcm", "rate": o.profile.SampleRate},
					"transcription": transcription,
					// Measured: this model rejects turn detection outright, so
					// it must be explicitly null rather than merely omitted.
					"turn_detection": nil,
				},
			},
		},
	}
	if err := o.writeJSON(update); err != nil {
		conn.Close()
		return fmt.Errorf("transcribe: session.update: %w", err)
	}

	go o.readLoop()
	go o.keepalive()
	return nil
}

func (o *openairt) writeJSON(v any) error {
	o.writes.Lock()
	defer o.writes.Unlock()
	_ = o.conn.SetWriteDeadline(time.Now().Add(oaWriteWait))
	return o.conn.WriteJSON(v)
}

func (o *openairt) SendAudio(pcm16 []byte) error {
	select {
	case <-o.done:
		return errors.New("transcribe: session closed")
	default:
	}
	return o.writeJSON(map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm16),
	})
}

// Commit closes the current utterance. This client owns endpointing because
// the model refuses to: the ingest watches for silence and calls this, and
// without it a final never arrives at all.
func (o *openairt) Commit() error {
	select {
	case <-o.done:
		return errors.New("transcribe: session closed")
	default:
	}
	return o.writeJSON(map[string]any{"type": "input_audio_buffer.commit"})
}

func (o *openairt) keepalive() {
	t := time.NewTicker(oaKeepalive)
	defer t.Stop()
	for {
		select {
		case <-o.done:
			return
		case <-t.C:
			o.writes.Lock()
			_ = o.conn.SetWriteDeadline(time.Now().Add(oaWriteWait))
			err := o.conn.WriteMessage(websocket.PingMessage, nil)
			o.writes.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (o *openairt) readLoop() {
	defer o.finish()
	for {
		mt, data, err := o.conn.ReadMessage()
		if err != nil {
			select {
			case <-o.done:
			default:
				o.emit(Event{Type: EventError, Err: fmt.Errorf("transcribe: read: %w", err)})
			}
			return
		}
		_ = o.conn.SetReadDeadline(time.Now().Add(oaReadTimeout))
		if mt != websocket.TextMessage {
			continue
		}

		var ev oaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			o.log.Warn("transcribe: undecodable frame", "provider", o.profile.Name, "error", err)
			continue
		}

		switch {
		case ev.Type == "error":
			msg := "unknown"
			if ev.Error != nil {
				msg = fmt.Sprintf("%s/%s: %s", ev.Error.Type, ev.Error.Code, ev.Error.Message)
			}
			o.emit(Event{Type: EventError, Err: errors.New("transcribe: " + msg)})
			return

		case ev.Type == "input_audio_buffer.speech_started":
			o.emit(Event{Type: EventSpeechStarted, UtteranceID: ev.ItemID})

		case strings.HasSuffix(ev.Type, "input_audio_transcription.delta"):
			// A delta is an increment. Accumulate, and publish the whole text
			// so far, because the seam promises cumulative text.
			o.mu.Lock()
			o.accumulated[ev.ItemID] += ev.Delta
			text := o.accumulated[ev.ItemID]
			o.mu.Unlock()
			if strings.TrimSpace(text) != "" {
				o.emit(Event{Type: EventPartial, UtteranceID: ev.ItemID, Text: text})
			}

		case strings.HasSuffix(ev.Type, "input_audio_transcription.text"):
			// The dialect's other spelling: text is a confirmed prefix and
			// stash a revisable suffix. Concatenated, never surfaced apart.
			text := ev.Text + ev.Stash
			if strings.TrimSpace(text) != "" {
				o.emit(Event{Type: EventPartial, UtteranceID: ev.ItemID, Text: text})
			}

		case strings.HasSuffix(ev.Type, "input_audio_transcription.completed"):
			o.mu.Lock()
			delete(o.accumulated, ev.ItemID)
			o.mu.Unlock()
			// The final is a corrected rewrite, not the concatenation of the
			// deltas, so it replaces rather than extends.
			if strings.TrimSpace(ev.Transcript) == "" {
				continue // an empty final takes no seq (see dashscope.go)
			}
			o.emit(Event{Type: EventFinal, UtteranceID: ev.ItemID,
				Text: ev.Transcript, Language: ev.Language})

		case strings.HasSuffix(ev.Type, "input_audio_transcription.failed"):
			o.emit(Event{Type: EventError, Err: errors.New("transcribe: transcription failed")})
		}
	}
}

func (o *openairt) emit(ev Event) {
	select {
	case o.events <- ev:
	case <-o.done:
	default:
		o.log.Warn("transcribe: event dropped, consumer is not reading",
			"provider", o.profile.Name, "type", ev.Type)
	}
}

func (o *openairt) Close(ctx context.Context) error {
	var err error
	o.closeOnce.Do(func() {
		close(o.done)
		if o.conn != nil {
			err = o.conn.Close()
		}
	})
	return err
}

func (o *openairt) finish() {
	o.emit(Event{Type: EventClosed})
	close(o.events)
}
