// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeActions records what the model asked the application to do.
type fakeActions struct {
	mu        sync.Mutex
	transfers []TransferRequest
	messages  []MessageRequest
	hangups   []HangupRequest
	// refuseTransfer makes transfers come back refused, as a closed queue would.
	refuseTransfer bool
}

func (a *fakeActions) TransferToAgent(_ context.Context, request TransferRequest) (Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.transfers = append(a.transfers, request)
	if a.refuseTransfer {
		return Failed("QUEUE_CLOSED",
			"The queue is closed. Explain that and offer to take a message."), nil
	}
	return Succeeded(map[string]any{"queue": request.Queue},
		"Tell the caller you are connecting them."), nil
}

func (a *fakeActions) TakeMessage(_ context.Context, request MessageRequest) (Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, request)
	return Succeeded(nil, "Confirm the message has been taken."), nil
}

func (a *fakeActions) Hangup(_ context.Context, request HangupRequest) (Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hangups = append(a.hangups, request)
	return Succeeded(nil, "Say goodbye now."), nil
}

func testRuntime(t *testing.T, actions *fakeActions, backendURL string) *Runtime {
	t.Helper()
	engine := NewEngine(loadTestFlow(t), "en",
		map[string]any{"caller": "13800138000"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return NewRuntime(engine, actions, NewBackend(backendURL),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func decodeOutput(t *testing.T, output string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("tool output is not JSON: %v\n%s", err, output)
	}
	return decoded
}

//
// Offering tools to the model.
//

func TestOnlyReferencedToolsAreOffered(t *testing.T) {
	r := testRuntime(t, &fakeActions{}, "")

	var names []string
	for _, tool := range r.Tools() {
		names = append(names, tool.Name)
	}

	for _, want := range []string{"lookup_account", "take_message", "transfer_to_agent", "hangup"} {
		if !contains(names, want) {
			t.Errorf("tool %q is missing from %v", want, names)
		}
	}
	// The flow never references a fourth built-in, so nothing else appears.
	if len(names) != 4 {
		t.Errorf("offered %v, want exactly the referenced four", names)
	}
}

func TestToolDescriptionsFollowTheCallLanguage(t *testing.T) {
	engine := NewEngine(loadTestFlow(t), "zh", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := NewRuntime(engine, &fakeActions{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	for _, tool := range r.Tools() {
		if tool.Name == ToolTransferToAgent && !strings.Contains(tool.Description, "转接") {
			t.Errorf("transfer description is not in the call language: %q", tool.Description)
		}
		if tool.Name == "lookup_account" && !strings.Contains(tool.Description, "查询") {
			t.Errorf("lookup description is not in the call language: %q", tool.Description)
		}
	}
}

func TestInstructionsCarryPersonaRulesAndPhase(t *testing.T) {
	r := testRuntime(t, &fakeActions{}, "")

	instructions := r.Instructions()
	for _, want := range []string{
		"You answer for NovaNet billing.", // persona
		"Keep replies short.",             // rules
		"Current phase [welcome]",         // phase label
		"Greet and ask",                   // phase instruction
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions are missing %q:\n%s", want, instructions)
		}
	}
}

//
// Dispatch.
//

// A tool used outside its phase is steered, not failed: the model gets the
// current phase's instruction and keeps talking to the caller.
func TestAToolOutsideItsPhaseIsRefusedWithSteering(t *testing.T) {
	r := testRuntime(t, &fakeActions{}, "")

	output, moved := r.Dispatch(t.Context(), "take_message", `{"message":"hi"}`)
	if moved != "" {
		t.Fatalf("a refused tool moved the phase to %q", moved)
	}

	decoded := decodeOutput(t, output)
	if decoded["ok"] != "0" {
		t.Errorf("ok = %v, want a refusal", decoded["ok"])
	}
	if hint, _ := decoded["hint"].(string); !strings.Contains(hint, "Greet and ask") {
		t.Errorf("the refusal does not steer back to the phase: %v", decoded["hint"])
	}
}

// When a transition fires, the new phase's instruction replaces whatever hint
// the tool produced — the flow is authoritative on progression.
func TestATransitionOverridesTheHint(t *testing.T) {
	actions := &fakeActions{}
	r := testRuntime(t, actions, "")

	output, moved := r.Dispatch(t.Context(), ToolTransferToAgent,
		`{"queue":"billing","reason":"BILLING_DISPUTE","summary":"wants a refund"}`)
	if moved != "handoff" {
		t.Fatalf("moved to %q, want handoff", moved)
	}

	decoded := decodeOutput(t, output)
	hint, _ := decoded["hint"].(string)
	if !strings.Contains(hint, "Announce the transfer") {
		t.Errorf("hint = %q, want the new phase's instruction", hint)
	}
	if strings.Contains(hint, "connecting them") {
		t.Errorf("the tool's own hint survived a transition: %q", hint)
	}

	if len(actions.transfers) != 1 || actions.transfers[0].Summary != "wants a refund" {
		t.Errorf("transfers = %+v", actions.transfers)
	}
}

// A refused transfer is a conversation, not an error. The bot explains and
// carries on — this is the AI-native answer to business hours.
func TestARefusedTransferKeepsTheConversationGoing(t *testing.T) {
	actions := &fakeActions{refuseTransfer: true}
	r := testRuntime(t, actions, "")

	output, moved := r.Dispatch(t.Context(), ToolTransferToAgent,
		`{"queue":"billing","reason":"AFTER_HOURS","summary":"needs help"}`)

	decoded := decodeOutput(t, output)
	if decoded["ok"] != "0" {
		t.Errorf("ok = %v", decoded["ok"])
	}
	if decoded["error"] != "QUEUE_CLOSED" {
		t.Errorf("error = %v, want the refusal reason", decoded["error"])
	}
	if hint, _ := decoded["hint"].(string); !strings.Contains(hint, "take a message") {
		t.Errorf("hint = %q, want the model told what to offer instead", hint)
	}
	// The flow-wide transfer rule conditions on result.ok, so a refusal holds
	// the phase and the model handles it in conversation.
	if moved != "" {
		t.Errorf("a refused transfer moved the phase to %q", moved)
	}
}

func TestABackendFailureBecomesARecoveryHint(t *testing.T) {
	r := testRuntime(t, &fakeActions{}, "http://127.0.0.1:1") // nothing listens

	output, moved := r.Dispatch(t.Context(), "lookup_account", `{"phone":"13800138000"}`)
	if moved != "" {
		t.Fatalf("a failed tool moved the phase to %q", moved)
	}

	decoded := decodeOutput(t, output)
	if decoded["ok"] != "0" {
		t.Errorf("ok = %v", decoded["ok"])
	}
	if hint, _ := decoded["hint"].(string); !strings.Contains(hint, "Tell the caller") {
		t.Errorf("hint = %q, want the model told to explain", hint)
	}
}

func TestAnUnknownToolIsRefused(t *testing.T) {
	r := testRuntime(t, &fakeActions{}, "")

	// The phase guard fires first for a tool no phase allows.
	output, _ := r.Dispatch(t.Context(), "mystery", `{}`)
	if decoded := decodeOutput(t, output); decoded["ok"] != "0" {
		t.Errorf("ok = %v", decoded["ok"])
	}
}

//
// The declarative HTTP runner, against a real server.
//

func TestHTTPToolEndToEnd(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/account/lookup" {
			t.Errorf("path = %q", req.URL.Path)
		}
		body, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(body, &captured)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"retCode": "000000",
			"data": map[string]any{
				"found": "1", "name": "Alice", "balance": "42.10",
				"secret": "must not leak",
			},
		})
	}))
	defer server.Close()

	r := testRuntime(t, &fakeActions{}, server.URL)
	output, moved := r.Dispatch(t.Context(), "lookup_account", `{"phone":"13800138000"}`)

	// The template filled from both args and slots.
	if captured["phoneNumber"] != "13800138000" {
		t.Errorf("request body phoneNumber = %v", captured["phoneNumber"])
	}
	if captured["callerId"] != "13800138000" {
		t.Errorf("request body callerId = %v, want the slot value", captured["callerId"])
	}

	// The mapped fields moved the flow and reached the model.
	if moved != "report" {
		t.Fatalf("moved to %q, want report", moved)
	}
	decoded := decodeOutput(t, output)
	if decoded["ok"] != "1" || decoded["name"] != "Alice" {
		t.Errorf("output = %v", decoded)
	}
	// Only the mapped fields are exposed: the rest of the backend's response
	// has no business inside a conversation.
	if _, leaked := decoded["secret"]; leaked {
		t.Error("an unmapped backend field leaked into the model's view")
	}
}

func TestHTTPToolBackendRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"retCode": "100001", "retMsg": "account is locked",
		})
	}))
	defer server.Close()

	r := testRuntime(t, &fakeActions{}, server.URL)
	output, moved := r.Dispatch(t.Context(), "lookup_account", `{"phone":"1"}`)

	if moved != "" {
		t.Fatalf("a rejection moved the phase to %q", moved)
	}
	decoded := decodeOutput(t, output)
	if decoded["ok"] != "0" || decoded["error"] != "account is locked" {
		t.Errorf("output = %v, want the backend's own message", decoded)
	}
}

func TestTemplateKeepsTypesForLoneReferences(t *testing.T) {
	rendered := renderTemplate(
		map[string]any{
			"count":  "{args.count}",
			"label":  "order {args.count} of {args.total}",
			"nested": map[string]any{"phone": "{slots.caller}"},
		},
		map[string]any{"count": float64(3), "total": float64(9)},
		map[string]any{"caller": "1001"},
	)

	if rendered["count"] != float64(3) {
		t.Errorf("a lone reference lost its type: %T %v", rendered["count"], rendered["count"])
	}
	if rendered["label"] != "order 3 of 9" {
		t.Errorf("label = %v", rendered["label"])
	}
	nested := rendered["nested"].(map[string]any)
	if nested["phone"] != "1001" {
		t.Errorf("nested phone = %v", nested["phone"])
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
