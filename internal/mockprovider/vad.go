// SPDX-License-Identifier: Apache-2.0

package mockprovider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// VAD mode makes the mock hear the caller the way a provider with server-side
// turn detection does (#596): a frame above silence on the uplink starts
// speech, which cancels the response in progress the way qwen's or the
// gateway's server does (response.done, status cancelled); a run of quiet
// frames ends it, which commits the utterance, sends its scripted transcript
// and answers it with a reply of the mock's own — a line of audio, or a tool
// call, like any response a model makes.
//
// What the reply carries is scripted per utterance, so a run of cases decides
// what each burst of the callee's turns into. The utterance list is one
// queue for the whole server: burst n of the run, whichever session it is on,
// plays utterance n. The driver dials its cases in order, the same ordering
// the simulator's n-th-INVITE behaviours already rely on; bursts past the
// list's end replay the last utterance, the way a callee who keeps saying
// "喂" would.
//
// The mode is off unless configured (-vad); nothing else about the server
// changes.
type VAD struct {
	// Utterances is the queue of replies, consumed in arrival order.
	Utterances []VADUtterance `json:"utterances"`
	// HangoverMs is the quiet time that ends a burst of speech. Short enough
	// to answer promptly, long enough not to split a burst's own pacing.
	HangoverMs float64 `json:"hangoverMs,omitempty"`

	mu   sync.Mutex
	next int
}

// VADUtterance is the reply one detected burst earns.
type VADUtterance struct {
	// Text is the caller's words as the mock's transcriber reports them
	// (conversation.item.input_audio_transcription.completed).
	Text string `json:"text"`
	// SpeakSec is how much audio the reply speaks, the model's improvised
	// answer to the caller. Zero speaks nothing.
	SpeakSec float64 `json:"speakSec,omitempty"`
	// ToolName, when set, makes the reply call this tool, like a response
	// whose turn moves the call (a function_call output item).
	ToolName string          `json:"toolName,omitempty"`
	ToolArgs json.RawMessage `json:"toolArgs,omitempty"`
}

// DefaultVADHangoverMs ends speech after this much quiet when the file says
// nothing. 300 ms, matching the application's own end-of-greeting measure.
const DefaultVADHangoverMs = 300

// LoadVAD reads and validates a VAD file.
func LoadVAD(path string) (*VAD, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseVAD(data)
}

// ParseVAD validates a VAD document. Unknown fields are errors, as in the
// scenario file.
func ParseVAD(data []byte) (*VAD, error) {
	var vad struct {
		Utterances []struct {
			Text     string          `json:"text"`
			SpeakSec *float64        `json:"speakSec"`
			ToolName string          `json:"toolName"`
			ToolArgs json.RawMessage `json:"toolArgs"`
		} `json:"utterances"`
		HangoverMs *float64 `json:"hangoverMs"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vad); err != nil {
		return nil, fmt.Errorf("vad file: %w", err)
	}
	if len(vad.Utterances) == 0 {
		return nil, fmt.Errorf("vad file: utterances is empty")
	}
	out := &VAD{HangoverMs: DefaultVADHangoverMs}
	if vad.HangoverMs != nil {
		out.HangoverMs = *vad.HangoverMs
	}
	if out.HangoverMs <= 0 || out.HangoverMs > 5000 {
		return nil, fmt.Errorf("vad file: hangoverMs %v: want 0 < h <= 5000", out.HangoverMs)
	}
	for i, u := range vad.Utterances {
		speak := 1.0
		if u.SpeakSec != nil {
			speak = *u.SpeakSec
		}
		if u.Text == "" {
			return nil, fmt.Errorf("vad file: utterance %d: text is empty", i)
		}
		if speak < 0 || speak > 60 {
			return nil, fmt.Errorf("vad file: utterance %d: speakSec %v out of range", i, speak)
		}
		args := u.ToolArgs
		if u.ToolName != "" && len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		out.Utterances = append(out.Utterances, VADUtterance{
			Text: u.Text, SpeakSec: speak, ToolName: u.ToolName, ToolArgs: args,
		})
	}
	return out, nil
}

// take returns the utterance the next detected burst plays, consuming it.
// The list is shared across sessions: one queue for the run.
func (v *VAD) take() VADUtterance {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.next >= len(v.Utterances) {
		return v.Utterances[len(v.Utterances)-1]
	}
	u := v.Utterances[v.next]
	v.next++
	return u
}

// vadState is one session's speech detection. Appends feed it frame levels;
// a timer ends the burst when the uplink goes quiet, since nothing may append
// during silence.
type vadState struct {
	s *session

	mu       sync.Mutex
	speaking bool
	itemID   string
	seq      int
	timer    *time.Timer
}

func newVADState(s *session) *vadState {
	return &vadState{s: s}
}

// noteAppend measures one input_audio_buffer.append against the silence
// threshold, as the meter does.
func (v *vadState) noteAppend(data []byte) {
	var event struct {
		Audio string `json:"audio"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(event.Audio)
	if err != nil || len(raw) == 0 {
		return
	}
	format := v.s.meter.formatOf()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, level := range frameLevels(format, raw) {
		if level <= uplinkSilenceRMS {
			continue
		}
		if !v.speaking {
			v.speaking = true
			v.seq++
			v.itemID = fmt.Sprintf("vad_item_%d", v.seq)
			go v.speechStarted(v.itemID)
		}
		v.armLocked()
	}
}

// noteSessionUpdate learns the input format, as the meter does.
func (v *vadState) noteSessionUpdate(data []byte) { v.s.meter.noteSessionUpdate(data) }

// close stops the hangover timer; the session's end ends any open burst.
func (v *vadState) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.timer != nil {
		v.timer.Stop()
	}
}

func (v *vadState) armLocked() {
	d := time.Duration(v.s.server.cfg.VAD.HangoverMs) * time.Millisecond
	if v.timer == nil {
		v.timer = time.AfterFunc(d, v.speechEnded)
		return
	}
	v.timer.Reset(d)
}

// speechStarted reports the burst's start and cancels the turn in flight, as
// a server's VAD does mid-response.
func (v *vadState) speechStarted(itemID string) {
	v.s.mu.Lock()
	hasTurn := v.s.cancelTurn != nil
	v.s.mu.Unlock()
	v.s.send(map[string]any{
		"type": "input_audio_buffer.speech_started", "item_id": itemID,
	})
	v.s.server.vadStarts.Add(1)
	v.s.meter.noteVADStart(hasTurn)
	if hasTurn {
		v.s.server.vadCancels.Add(1)
		v.s.stopTurn()
	}
}

// speechEnded fires at the hangover: the burst is over, so the utterance is
// stopped, committed, transcribed — and answered with the next scripted
// utterance, as the provider's own turn detection would.
func (v *vadState) speechEnded() {
	v.mu.Lock()
	if !v.speaking {
		return
	}
	v.speaking = false
	itemID := v.itemID
	v.mu.Unlock()

	utt := v.s.server.cfg.VAD.take()
	v.s.send(map[string]any{"type": "input_audio_buffer.speech_stopped", "item_id": itemID})
	v.s.send(map[string]any{"type": "input_audio_buffer.committed", "item_id": itemID})
	v.s.send(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": itemID, "transcript": utt.Text, "status": "completed",
	})
	v.s.server.vadUtterances.Add(1)
	v.s.meter.noteVADText(utt.Text)
	go v.s.vadReply(utt)
}

// vadReply plays the scripted answer to a burst: a response of its own with
// the utterance's audio and, when it names one, its tool call — cancellable
// like any turn, so a second burst inside it is a cut of the reply.
func (s *session) vadReply(u VADUtterance) {
	cfg := s.server.cfg

	// The burst that earned this reply cancelled the turn in flight; its
	// respond() clears cancelTurn as it settles, a few milliseconds out — but
	// the application may ask the next line first, so the floor can be held
	// for a whole turn. Give the reply the floor once it is free rather than
	// dropping it outright; only the session's end gives up on it.
	for {
		s.mu.Lock()
		free := s.cancelTurn == nil
		s.mu.Unlock()
		if free {
			break
		}
		select {
		case <-s.done:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	s.mu.Lock()
	if s.cancelTurn != nil {
		s.mu.Unlock()
		return
	}
	cancel := make(chan struct{})
	s.cancelTurn = cancel
	s.cancelIsReply = true
	s.responded++
	id := fmt.Sprintf("resp_%d", s.responded)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.cancelTurn == cancel {
			s.cancelTurn = nil
			s.cancelIsReply = false
		}
		s.mu.Unlock()
	}()

	s.send(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})
	s.send(map[string]any{
		"type": "response.output_item.added",
		"item": map[string]any{"id": "item_" + id, "type": "message"},
	})

	cancelled := func() bool {
		s.send(map[string]any{
			"type":     "response.done",
			"response": map[string]any{"id": id, "status": "cancelled"},
		})
		return true
	}
	isCancelled := func() bool {
		select {
		case <-cancel:
			return true
		default:
			return false
		}
	}

	// The scripted tool call goes out before the audio: the application may
	// cut a reply over a greeting to say the opening again, and a call made
	// only at the turn's end would be muted with it — an outcome the case
	// cannot tell apart from the call never being made.
	var outputs []any
	if u.ToolName != "" {
		callID := fmt.Sprintf("call_%s", id)
		item := map[string]any{
			"id": "item_" + id + "_call", "type": "function_call",
			"call_id": callID, "name": u.ToolName,
		}
		s.send(map[string]any{"type": "response.output_item.added", "item": item})
		s.send(map[string]any{
			"type": "response.function_call_arguments.done", "item_id": item["id"],
			"call_id": callID, "name": u.ToolName, "arguments": string(u.ToolArgs),
		})
		s.send(map[string]any{"type": "response.output_item.done", "item": item})
		outputs = append(outputs, item)
		s.server.toolCalls.Add(1)
	}

	speech := time.Duration(u.SpeakSec * float64(time.Second))
	if !s.wait(cfg.FirstAudio, cancel) {
		if isCancelled() {
			cancelled()
		}
		return
	}
	encoded := base64.StdEncoding.EncodeToString(s.turnFrame(cfg.DeltaAudio))
	deltas := int(speech / cfg.DeltaAudio)
	for range deltas {
		if err := s.send(map[string]any{
			"type": "response.output_audio.delta", "delta": encoded,
		}); err != nil {
			return
		}
		s.server.audioSec.Add(cfg.DeltaAudio.Milliseconds())
		if cfg.DeltaPace > 0 && !s.wait(cfg.DeltaPace, cancel) {
			if isCancelled() {
				cancelled()
			}
			return
		}
		if isCancelled() {
			cancelled()
			return
		}
	}

	s.send(map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"id": id, "status": "completed", "output": outputs,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		},
	})
	s.server.turns.Add(1)
	s.server.vadReplies.Add(1)
}
