// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

//
// #357 through the orchestrator: the turn a slot tool's result owed is
// cancelled by the phase's line, and a hangup it finishes before the cancel
// lands must not end the call. The real Realtime client talks to a scripted
// endpoint, because the defect is in what the client hands out.
//

// scriptedRealtime is a Realtime endpoint the test drives frame by frame.
type scriptedRealtime struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	conn     *websocket.Conn
	writeMu  sync.Mutex
	received []map[string]any
	// onMessage runs on the server's goroutine for every client frame.
	onMessage func(s *scriptedRealtime, message map[string]any)
}

func newScriptedRealtime(t *testing.T) *scriptedRealtime {
	t.Helper()
	s := &scriptedRealtime{t: t}
	upgrader := websocket.Upgrader{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conn = conn
		s.mu.Unlock()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var message map[string]any
			if json.Unmarshal(data, &message) != nil {
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, message)
			hook := s.onMessage
			s.mu.Unlock()
			if message["type"] == "session.update" {
				s.send(map[string]any{"type": "session.updated"})
			}
			if hook != nil {
				hook(s, message)
			}
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *scriptedRealtime) send(event map[string]any) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	data, _ := json.Marshal(event)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

func (s *scriptedRealtime) frames(messageType string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, message := range s.received {
		if message["type"] == messageType {
			out = append(out, message)
		}
	}
	return out
}

func (s *scriptedRealtime) await(t *testing.T, messageType string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.frames(messageType); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%d %q frames, want %d", len(s.frames(messageType)), messageType, n)
	return nil
}

// instructionsOf is the direction a response.create carries, "" for none.
func instructionsOf(request map[string]any) string {
	response, _ := request["response"].(map[string]any)
	text, _ := response["instructions"].(string)
	return text
}

// cancelledTurnFlow records the caller's answer with a slot tool and moves to
// a phase that is either a non-terminal one with a line, inviting hangup on
// the caller's next words, or a terminal one with a line.
func cancelledTurnFlow(isTerminalTarget bool) string {
	target := `"booked": {
			"announce": "Done, you are booked in.",
			"instruction": "Say nothing until the caller replies, then call hangup.",
			"isFlowHangupAllowed": true,
			"tools": ["hangup"]
		}`
	if isTerminalTarget {
		target = `"booked": {
			"announce": "Done, you are booked in.",
			"instruction": "The line hangs up after the farewell.",
			"tools": [], "isTerminal": true
		}`
	}
	return `{
	"id": "cancelled-turn-test",
	"specVersion": "v2",
	"initialNode": "offer",
	"global": {"persona": "You call customers.", "alwaysAllowedTools": ["hangup"]},
	"tools": {
		"confirm": {"kind": "slot", "slot": "answer", "description": "Record the caller's agreement."}
	},
	"nodes": {
		"offer": {
			"instruction": "Ask whether they agree, then record the answer.",
			"tools": ["confirm"],
			"transitions": [{"on": "TOOL_RESULT", "tool": "confirm", "target": "booked"}]
		},
		` + target + `
	}
}`
}

// startCancelledTurnCall runs the drive loop with the real Realtime client on
// the scripted endpoint, and has the model call the slot tool in its first
// response (R1), left open.
func startCancelledTurnCall(t *testing.T, flowJSON string) (*scriptedRealtime, *claimsHarness) {
	t.Helper()
	s := newScriptedRealtime(t)
	profile := provider.OpenAIProfile()
	profile.Endpoint = "ws" + strings.TrimPrefix(s.server.URL, "http")
	t.Setenv(profile.APIKeyEnv, "test-key")
	real, err := provider.New(profile, nil)
	if err != nil {
		t.Fatalf("new realtime client: %v", err)
	}
	h := newClaimsHarnessModel(t, profile, flowJSON, "en", "",
		Config{BargeGuard: -1, NoInput: -1},
		func(*fakeModel) provider.VoiceSession { return real }, nil)
	return s, h
}

// callSlotToolInR1 has the endpoint create R1 and finish the slot tool's call
// in it, then wait for the orchestrator to answer it.
func callSlotToolInR1(t *testing.T, s *scriptedRealtime) {
	t.Helper()
	s.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "r1"}})
	s.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_confirm", "type": "function_call", "call_id": "fc_confirm"}})
	s.send(map[string]any{"type": "response.function_call_arguments.done",
		"item_id": "item_confirm", "call_id": "fc_confirm", "name": "confirm",
		"arguments": `{"value":"yes, go ahead"}`})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range s.frames("conversation.item.create") {
			if it, _ := item["item"].(map[string]any); it["type"] == "function_call_output" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the slot tool was never answered")
}

// The result's turn (B) is cancelled for the phase's line and calls hangup
// before the cancel lands. The call is not dispatched: nothing is armed, the
// call stays in the phase with the line, the item is taken out of the
// conversation, and the line is asked for.
func TestAHangupInTheTurnThePhasesLineCancelsDoesNotEndTheCall(t *testing.T) {
	s, h := startCancelledTurnCall(t, cancelledTurnFlow(false))
	var once sync.Once
	s.mu.Lock()
	s.onMessage = func(s *scriptedRealtime, message map[string]any) {
		switch message["type"] {
		case "response.create":
			// The first request is the opening, which R1 stands in for.
			switch len(s.frames("response.create")) {
			case 2: // B, the turn the slot tool's result owed
				s.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "b"}})
			case 3: // the line
				s.send(map[string]any{"type": "response.created", "response": map[string]any{"id": "line"}})
				s.send(map[string]any{"type": "response.done",
					"response": map[string]any{"id": "line", "status": "completed"}})
			}
		case "response.cancel":
			once.Do(func() {
				// B finishes a hangup in the round trip, and only then is
				// the cancel answered.
				s.send(map[string]any{"type": "response.output_item.added",
					"item": map[string]any{"id": "item_hangup", "type": "function_call", "call_id": "fc_hangup"}})
				s.send(map[string]any{"type": "response.function_call_arguments.done",
					"item_id": "item_hangup", "call_id": "fc_hangup", "name": "hangup",
					"arguments": `{"reason":"FLOW_INSTRUCTED"}`})
				s.send(map[string]any{"type": "response.done", "response": map[string]any{
					"id": "b", "status": "cancelled",
					"output": []any{map[string]any{"id": "item_hangup", "type": "function_call", "call_id": "fc_hangup"}}}})
			})
		}
	}
	s.mu.Unlock()

	callSlotToolInR1(t, s)
	s.send(map[string]any{"type": "response.done",
		"response": map[string]any{"id": "r1", "status": "completed",
			"output": []any{map[string]any{"id": "item_confirm", "type": "function_call", "call_id": "fc_confirm"}}}})

	s.await(t, "response.cancel", 1)
	deleted := s.await(t, "conversation.item.delete", 1)
	if deleted[0]["item_id"] != "item_hangup" {
		t.Errorf("deleted %v, want item_hangup", deleted[0]["item_id"])
	}
	requests := s.await(t, "response.create", 3)
	if !strings.Contains(instructionsOf(requests[2]), "Done, you are booked in.") {
		t.Errorf("the second request is not the phase's line: %q", instructionsOf(requests[2]))
	}
	time.Sleep(200 * time.Millisecond)
	if h.actions.isArmed() {
		t.Error("the hangup of a cancelled turn armed the ending")
	}
	if got := h.runtime.Engine().NodeID(); got != "booked" {
		t.Errorf("phase = %q, want the call to stay in booked", got)
	}
	select {
	case <-h.done:
		t.Error("the call ended")
	default:
	}
}

// A terminal target carries its line in the tool's answer: no turn is asked
// for by SpeakText and so no cancel is sent, as before.
func TestATerminalPhasesLineRidesTheToolAnswerAndCancelsNothing(t *testing.T) {
	s, _ := startCancelledTurnCall(t, cancelledTurnFlow(true))
	callSlotToolInR1(t, s)

	var output string
	for _, item := range s.frames("conversation.item.create") {
		if it, _ := item["item"].(map[string]any); it["type"] == "function_call_output" {
			output, _ = it["output"].(string)
		}
	}
	if !strings.Contains(output, "Done, you are booked in.") {
		t.Errorf("the tool's answer does not carry the line: %q", output)
	}
	s.send(map[string]any{"type": "response.done",
		"response": map[string]any{"id": "r1", "status": "completed"}})
	time.Sleep(200 * time.Millisecond)
	if got := s.frames("response.cancel"); len(got) != 0 {
		t.Errorf("%d cancels sent for a line that rides the answer", len(got))
	}
}
