// SPDX-License-Identifier: Apache-2.0

package mockprovider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Scenario mode makes the mock a scripted model instead of a metronome.
//
// The default mode produces a turn on a timer and never asks for a tool, so a
// run cannot reach a path that only a model's tool call opens (record_lead, a
// conversation the bot ends itself) without a real, paid, non-deterministic
// provider. A scenario is a list of scripts; each session plays one, a step at
// a time: speak, request a tool, wait for its result, end.
//
// What the wire gives a script to select on. The application sends the
// provider no call identifier: the URL carries the model, the headers the
// bearer key, and session.update the standing instructions, the tools, the
// voice and the audio formats. The instructions are the one per-session value
// that differs between calls, because they are the flow's persona and phase
// text, so a script selects on a substring of them (Script.InstructionsContain).
// A script with no such substring is the default; a session that no script
// claims gets silence and is counted (Stats.Unmatched).

// Step operations. SCREAMING_SNAKE, like every enum in the repository.
const (
	// OpSpeak plays one turn of DurationSec seconds of audio.
	OpSpeak = "SPEAK"
	// OpCallTool plays one turn that requests the tool ToolName with Args.
	OpCallTool = "CALL_TOOL"
	// OpAwaitToolResult waits for the application to answer the most recent
	// CALL_TOOL, up to TimeoutSec (default DefaultAwaitTimeoutSec).
	OpAwaitToolResult = "AWAIT_TOOL_RESULT"
	// OpEndSession closes the WebSocket with a normal closure (1000). On the
	// Realtime profiles the provider has no "session over" event: the socket
	// closing is the end, and the application's session treats the closed
	// event stream as the model going away.
	OpEndSession = "END_SESSION"
)

// DefaultAwaitTimeoutSec bounds OpAwaitToolResult when a step names none.
const DefaultAwaitTimeoutSec = 10

// Scenario is the parsed content of a scenario file.
type Scenario struct {
	Scripts []Script `json:"scripts"`
}

// Script is what one session plays.
type Script struct {
	Name string `json:"name"`
	// InstructionsContain selects the script for a session whose standing
	// instructions contain it. Empty makes this the default script.
	InstructionsContain string `json:"instructionsContain"`
	Steps               []Step `json:"steps"`
}

// Step is one move of a script. Which fields apply depends on Op.
type Step struct {
	Op          string          `json:"op"`
	DurationSec float64         `json:"durationSec,omitempty"`
	ToolName    string          `json:"toolName,omitempty"`
	Args        json.RawMessage `json:"args,omitempty"`
	TimeoutSec  float64         `json:"timeoutSec,omitempty"`
}

// LoadScenario reads and validates a scenario file.
func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseScenario(data)
}

// ParseScenario validates a scenario document. Unknown fields are errors: a
// misspelt "durationSec" would otherwise play as a silent zero.
func ParseScenario(data []byte) (*Scenario, error) {
	var sc Scenario
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sc); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	if len(sc.Scripts) == 0 {
		return nil, errors.New("scenario: no scripts")
	}
	defaults := 0
	for i := range sc.Scripts {
		script := &sc.Scripts[i]
		if script.InstructionsContain == "" {
			defaults++
		}
		if err := script.validate(); err != nil {
			return nil, fmt.Errorf("scenario: script %d (%q): %w", i, script.Name, err)
		}
	}
	if defaults > 1 {
		return nil, errors.New("scenario: more than one default script (empty instructionsContain)")
	}
	return &sc, nil
}

// Pick is the script a session with these standing instructions plays: the
// first script whose InstructionsContain they contain, else the default one,
// else nil (the session stays silent). Exported so that a harness can check,
// before a run, which script each of its calls will get.
func (sc *Scenario) Pick(instructions string) *Script {
	var fallback *Script
	for i := range sc.Scripts {
		script := &sc.Scripts[i]
		if script.InstructionsContain == "" {
			fallback = script
			continue
		}
		if strings.Contains(instructions, script.InstructionsContain) {
			return script
		}
	}
	return fallback
}

func (script *Script) validate() error {
	if len(script.Steps) == 0 {
		return errors.New("no steps")
	}
	isToolOpen := false
	for i, step := range script.Steps {
		switch step.Op {
		case OpSpeak:
			if step.DurationSec <= 0 {
				return fmt.Errorf("step %d: SPEAK needs durationSec > 0", i)
			}
		case OpCallTool:
			if step.ToolName == "" {
				return fmt.Errorf("step %d: CALL_TOOL needs toolName", i)
			}
			if len(step.Args) > 0 {
				var object map[string]any
				if err := json.Unmarshal(step.Args, &object); err != nil || object == nil {
					return fmt.Errorf("step %d: args must be a JSON object", i)
				}
			}
			isToolOpen = true
		case OpAwaitToolResult:
			if !isToolOpen {
				return fmt.Errorf("step %d: AWAIT_TOOL_RESULT with no CALL_TOOL before it", i)
			}
			if step.TimeoutSec < 0 {
				return fmt.Errorf("step %d: timeoutSec must not be negative", i)
			}
			isToolOpen = false
		case OpEndSession:
			if i != len(script.Steps)-1 {
				return fmt.Errorf("step %d: END_SESSION must be the last step", i)
			}
		default:
			return fmt.Errorf("step %d: unknown op %q", i, step.Op)
		}
	}
	return nil
}

// scenarioState is a session's script bookkeeping. instructions and isStarted
// belong to the read loop; results is the hand-off to the script goroutine.
type scenarioState struct {
	instructions string
	isStarted    bool
	// results carries, in arrival order, every function_call_output the
	// application sends (its call id) and every response.create after the
	// script started (isCreate).
	results chan scnSignal
}

type scnSignal struct {
	callID string
	// isCreate marks a response.create: the application asking for the turn
	// a tool result is owed.
	isCreate bool
}

func (s *session) signal(sig scnSignal) {
	select {
	case s.scn.results <- sig:
	default:
	}
}

// DefaultResponseGrace bounds how long AWAIT_TOOL_RESULT waits, after the
// function_call_output, for the response.create that normally follows it.
const DefaultResponseGrace = time.Second

// observe is called for every inbound event in scenario mode, ahead of the
// ordinary dispatch. It reports whether the event is consumed.
func (s *session) observe(eventType string, data []byte) (isConsumed bool) {
	switch eventType {
	case "session.update":
		var update struct {
			Session struct {
				Instructions string `json:"instructions"`
			} `json:"session"`
		}
		if json.Unmarshal(data, &update) == nil {
			s.scn.instructions = update.Session.Instructions
		}
		return false // the ordinary handler still acknowledges it

	case "response.create":
		// The first one is the application asking for the opening turn, which
		// is when a script begins. Every later one asks for the turn a tool
		// result is owed; the script, not the application, decides what the
		// model says next.
		if !s.scn.isStarted {
			s.scn.isStarted = true
			go s.runScript(s.scn.instructions)
		} else {
			s.signal(scnSignal{isCreate: true})
		}
		return true

	case "conversation.item.create":
		var create struct {
			Item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			} `json:"item"`
		}
		if json.Unmarshal(data, &create) == nil && create.Item.Type == "function_call_output" {
			s.server.toolResults.Add(1)
			s.signal(scnSignal{callID: create.Item.CallID})
		}
		return true

	case "response.cancel":
		// Scripted turns are atomic; a cancel has nothing to stop.
		return true
	}
	return false
}

func (s *session) runScript(instructions string) {
	script := s.server.cfg.Scenario.Pick(instructions)
	if script == nil {
		s.server.unmatched.Add(1)
		s.log.Warn("mockprovider: no script claims this session; it stays silent")
		return
	}
	s.log.Info("mockprovider: playing script", "script", script.Name)

	lastCallID := ""
	calls := 0
	for _, step := range script.Steps {
		switch step.Op {
		case OpSpeak:
			if !s.scriptedTurn(secondsOf(step.DurationSec), nil) {
				return
			}
		case OpCallTool:
			calls++
			lastCallID = fmt.Sprintf("call_scn_%d", calls)
			call := &scriptedCall{id: lastCallID, name: step.ToolName, args: string(step.Args)}
			if call.args == "" {
				call.args = "{}"
			}
			if !s.scriptedTurn(0, call) {
				return
			}
			s.server.toolCalls.Add(1)
		case OpAwaitToolResult:
			timeout := step.TimeoutSec
			if timeout == 0 {
				timeout = DefaultAwaitTimeoutSec
			}
			if !s.awaitResult(lastCallID, secondsOf(timeout)) {
				s.server.scriptsFailed.Add(1)
				s.log.Warn("mockprovider: no tool result in time", "script", script.Name, "toolCallId", lastCallID)
				s.closeWith(websocket.CloseInternalServerErr, "tool result missing")
				return
			}
		case OpEndSession:
			s.server.scriptsDone.Add(1)
			s.closeWith(websocket.CloseNormalClosure, "scripted end")
			return
		}
	}
	// A script without END_SESSION leaves the session open and quiet.
	s.server.scriptsDone.Add(1)
}

func secondsOf(sec float64) time.Duration {
	return time.Duration(sec * float64(time.Second))
}

type scriptedCall struct{ id, name, args string }

// scriptedTurn plays one response: speech of the given length, or a tool call.
// It reports false when the session ended under it.
func (s *session) scriptedTurn(speech time.Duration, call *scriptedCall) bool {
	cfg := s.server.cfg
	s.mu.Lock()
	s.responded++
	id := fmt.Sprintf("resp_%d", s.responded)
	s.mu.Unlock()

	s.send(map[string]any{"type": "response.created", "response": map[string]any{"id": id}})

	if call != nil {
		itemID := "item_" + id
		item := map[string]any{"id": itemID, "type": "function_call", "call_id": call.id, "name": call.name}
		s.send(map[string]any{"type": "response.output_item.added", "item": item})
		s.send(map[string]any{
			"type": "response.function_call_arguments.done", "item_id": itemID,
			"call_id": call.id, "name": call.name, "arguments": call.args,
		})
		s.send(map[string]any{"type": "response.output_item.done", "item": item})
		return s.send(map[string]any{
			"type": "response.done",
			"response": map[string]any{
				"id": id, "status": "completed", "output": []any{item},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		}) == nil
	}

	s.send(map[string]any{
		"type": "response.output_item.added",
		"item": map[string]any{"id": "item_" + id, "type": "message"},
	})
	if !s.wait(cfg.FirstAudio, nil) {
		return false
	}
	encoded := base64.StdEncoding.EncodeToString(deltaFrame(cfg.DeltaAudio))
	deltas := int(speech / cfg.DeltaAudio)
	if deltas < 1 {
		deltas = 1
	}
	for range deltas {
		if err := s.send(map[string]any{"type": "response.output_audio.delta", "delta": encoded}); err != nil {
			return false
		}
		s.server.audioSec.Add(cfg.DeltaAudio.Milliseconds())
		if cfg.DeltaPace > 0 && !s.wait(cfg.DeltaPace, nil) {
			return false
		}
	}
	s.send(map[string]any{"type": "response.output_audio_transcript.done", "transcript": "mock turn"})
	err := s.send(map[string]any{
		"type": "response.done",
		"response": map[string]any{
			"id": id, "status": "completed",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		},
	})
	s.server.turns.Add(1)
	return err == nil
}

// awaitResult waits for the application to finish answering callID: the
// function_call_output, then the response.create the client sends after it
// (realtime.go SendToolResult writes the two as separate frames). Returning
// on the first frame would let a script close the socket between the two
// writes and fail the client's second one with a close-sent error. A client
// that sends no response.create (the turn rides on one already asked for)
// costs one grace period, counted and logged. It reports false when the
// output itself does not arrive in time.
func (s *session) awaitResult(callID string, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	isOutput := false
	var grace <-chan time.Time
	for {
		select {
		case got := <-s.scn.results:
			switch {
			case !isOutput && !got.isCreate && got.callID == callID:
				isOutput = true
				g := time.NewTimer(s.server.cfg.ResponseGrace)
				defer g.Stop()
				grace = g.C
			case isOutput && got.isCreate:
				return true
			}
		case <-grace:
			s.server.resultsWithoutCreate.Add(1)
			s.log.Info("mockprovider: no response.create followed the tool result; continuing",
				"toolCallId", callID)
			return true
		case <-timer.C:
			return false
		case <-s.done:
			return false
		}
	}
}

// closeWith ends the session with a WebSocket close frame.
func (s *session) closeWith(code int, text string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, text), time.Now().Add(2*time.Second))
}
