// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// fakeChat answers from a function and records every request. It stands in for
// the model API: no test in this package reaches a real one.
type fakeChat struct {
	mu       sync.Mutex
	requests []ChatRequest
	answer   func(n int, req ChatRequest) (ChatResponse, error)
}

func (f *fakeChat) Complete(_ context.Context, req ChatRequest) (ChatResponse, error) {
	f.mu.Lock()
	n := len(f.requests)
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return f.answer(n, req)
}

// queue answers from a fixed list, repeating the last answer when it runs out.
func queue(answers ...ChatResponse) *fakeChat {
	return &fakeChat{answer: func(n int, _ ChatRequest) (ChatResponse, error) {
		return answers[min(n, len(answers)-1)], nil
	}}
}

func text(s string) ChatResponse {
	return ChatResponse{Content: s, PromptTokens: 100, CompletionTokens: 10}
}

func tool(name, args string) ChatResponse {
	return ChatResponse{ToolCalls: []ChatToolCall{{ID: "c-" + name, Name: name, Arguments: args}}, PromptTokens: 100, CompletionTokens: 10}
}

func earlyCollections(t *testing.T) *flow.Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(seedFlows, "early_collections.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := flow.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func scenarioOf(t *testing.T, body string) Scenario {
	t.Helper()
	file, err := ParseScenarios([]byte(`{"scenarios":[` + body + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	return file.Scenarios[0]
}

const promiseScenario = `{"name":"promise","lang":"en","caller":"92223333",
	"callerPersona":{"goal":"say you will pay on Friday","persona":"You are the account owner."},
	"model":{"phase":"promised","ending":"none","visits":["promised"],"avoids":["need_transfer"]}}`

// The happy path: the bot model is shown the real instructions and tools, its
// tool calls move the real engine, and the run is judged on the outcome.
func TestTier2PromiseCall(t *testing.T) {
	backend := MockBackends()
	bot := queue(
		tool("bill_lookup", `{}`), text("You have a bill of 45.60 overdue."),
		tool("promise_record", `{"pay_date":"Friday"}`),
	)
	caller := queue(text("Yes, it is me."), text("I will pay on Friday."), text(callerHangup))

	reports := RunModel(context.Background(), earlyCollections(t), backend,
		ModelConfig{Bot: bot, Caller: caller, BotModel: "bot-m", CallerModel: "caller-m", Runs: 1},
		[]Scenario{scenarioOf(t, promiseScenario)})

	r := reports[0]
	if !r.Succeeded() {
		t.Fatalf("scenario failed: %+v", r)
	}
	run := r.Runs[0]
	if got := strings.Join(run.Path, ">"); got != "greet>remind>promised" {
		t.Errorf("path = %s", got)
	}
	// promised is a terminal phase: the platform ends the call after its
	// announce, so the model never calls hangup.
	if run.Ending != "none" {
		t.Errorf("ending = %s", run.Ending)
	}

	first := bot.requests[0]
	if first.Model != "bot-m" {
		t.Errorf("bot model = %q", first.Model)
	}
	system := first.Messages[0].Content
	for _, want := range []string{"NovaNet Broadband billing-reminder", "asked whether you are speaking with the account holder"} {
		if !strings.Contains(system, want) {
			t.Errorf("the bot's system prompt lacks %q", want)
		}
	}
	// The platform's own rules come from the runtime, not from this package.
	if !strings.Contains(system, "instructions") {
		t.Errorf("the system prompt carries no platform rule: %s", system)
	}
	var names []string
	for _, tl := range first.Tools {
		names = append(names, tl.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "bill_lookup") || !strings.Contains(strings.Join(names, ","), "transfer_to_agent") {
		t.Errorf("tools offered = %v", names)
	}
	// After the move the model is re-instructed with the new phase and has read
	// the tool result as the runtime returned it.
	last := bot.requests[len(bot.requests)-1]
	if !strings.Contains(strings.Join(run.Log, "\n"), "payment by Friday") {
		t.Errorf("the promised phase's announce does not carry the date: %v", run.Log)
	}
	var sawResult bool
	for _, m := range last.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, `"amount":"45.60"`) {
			sawResult = true
		}
	}
	if !sawResult {
		t.Error("the model never saw bill_lookup's result as the runtime returned it")
	}
	// The caller model sees the bot's announce and its replies, not the flow.
	if got := caller.requests[0].Messages[1].Content; !strings.Contains(got, "Hello, this is NovaNet") {
		t.Errorf("the caller was told %q first", got)
	}
	if strings.Contains(caller.requests[0].Messages[0].Content, "billing-reminder") {
		t.Error("the caller model was shown the flow's persona")
	}
}

func TestTier2PassRate(t *testing.T) {
	backend := MockBackends()
	spec := earlyCollections(t)
	sc := scenarioOf(t, `{"name":"coin","caller":"92223333","callerPersona":{"goal":"g"},
		"model":{"ending":"hangup","passRate":0.5}}`)

	// The bot hangs up in runs 1 and 3 and just talks in run 2.
	run := 0
	bot := &fakeChat{answer: func(_ int, req ChatRequest) (ChatResponse, error) {
		run++
		if run == 2 {
			return text("Just chatting."), nil
		}
		return tool("hangup", `{}`), nil
	}}
	caller := queue(text("hi"))
	cfg := ModelConfig{Bot: bot, Caller: caller, Runs: 3, MaxCallerTurns: 1}
	r := RunModel(context.Background(), spec, backend, cfg, []Scenario{sc})[0]

	if len(r.Runs) != 3 {
		t.Fatalf("runs = %d", len(r.Runs))
	}
	if r.Passed != 2 || !r.Succeeded() {
		t.Errorf("passed %d of 3, succeeded=%v; want 2 and true at a 50%% bar: %+v", r.Passed, r.Succeeded(), r.Runs)
	}

	sc.Model.PassRate = 0.9
	r = RunModel(context.Background(), spec, backend, ModelConfig{Bot: queue(text("chat")), Caller: caller, Runs: 2, MaxCallerTurns: 1}, []Scenario{sc})[0]
	if r.Succeeded() || r.Passed != 0 {
		t.Errorf("a bot that never hangs up passed: %+v", r)
	}
}

func TestTier2TurnCapEndsARunawayCall(t *testing.T) {
	backend := MockBackends()
	sc := scenarioOf(t, `{"name":"endless","caller":"92223333","callerPersona":{"goal":"g","maxTurns":3},"model":{}}`)
	bot, caller := queue(text("Anything else?")), queue(text("Well..."))

	r := RunModel(context.Background(), earlyCollections(t), backend,
		ModelConfig{Bot: bot, Caller: caller, Runs: 1, MaxCallerTurns: 50}, []Scenario{sc})[0]

	if r.Succeeded() || !strings.Contains(r.Runs[0].Failure, "still going after 3 caller turns") {
		t.Fatalf("failure = %q", r.Runs[0].Failure)
	}
	if len(caller.requests) != 3 {
		t.Errorf("the caller model was asked %d times, want 3", len(caller.requests))
	}
}

func TestTier2ToolRoundCap(t *testing.T) {
	backend := MockBackends()
	sc := scenarioOf(t, `{"name":"loop","caller":"10000000","callerPersona":{"goal":"g"},"model":{"passRate":1}}`)
	// An allowed tool called forever without answering: transfer is refused
	// and the call stays put.
	caller := queue(text("hello"))
	spec := earlyCollections(t)

	sc.Actions = map[string]ActionResult{"transfer_to_agent": {IsOK: boolPtr(false), Error: "closed"}}
	bot := queue(tool("transfer_to_agent", `{"queue":"q","reason":"r","summary":"s"}`))
	r := RunModel(context.Background(), spec, backend, ModelConfig{Bot: bot, Caller: caller, Runs: 1}, []Scenario{sc})[0]

	if r.Succeeded() || !strings.Contains(r.Runs[0].Failure, "tool calls for 6 model rounds") {
		t.Fatalf("failure = %q", r.Runs[0].Failure)
	}
	if len(bot.requests) != maxToolRounds {
		t.Errorf("the bot model was called %d times in one turn, want %d", len(bot.requests), maxToolRounds)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestTier2SilenceReachesTheNoInputRule(t *testing.T) {
	backend := MockBackends()
	sc := scenarioOf(t, `{"name":"silent","caller":"92223333","callerPersona":{"goal":"say nothing","behaviour":"silent"},
		"model":{"phase":"finish","ending":"none"}}`)
	r := RunModel(context.Background(), earlyCollections(t), backend,
		ModelConfig{Bot: queue(text("Hello?")), Caller: queue(text(callerSilence)), Runs: 1}, []Scenario{sc})[0]
	if !r.Succeeded() {
		t.Fatalf("%+v", r.Runs[0])
	}
}

func TestTier2BudgetStopsTheInvocation(t *testing.T) {
	backend := MockBackends()
	sc1 := scenarioOf(t, `{"name":"first","caller":"92223333","callerPersona":{"goal":"g"},"model":{}}`)
	sc2 := scenarioOf(t, `{"name":"second","caller":"92223333","callerPersona":{"goal":"g"},"model":{}}`)
	inner := queue(text("blah"))
	budget := NewBudget(inner, 5, 0)

	reports := RunModel(context.Background(), earlyCollections(t), backend,
		ModelConfig{Bot: budget, Caller: budget, Runs: 3}, []Scenario{sc1, sc2})

	if calls, _ := budget.Usage(); calls != 5 {
		t.Errorf("model calls = %d, want exactly the cap of 5", calls)
	}
	if !strings.Contains(reports[0].Error, "budget") || reports[0].Succeeded() {
		t.Errorf("first = %+v", reports[0])
	}
	if !reports[1].Skipped || len(reports[1].Runs) != 0 {
		t.Errorf("the second scenario ran after the budget was spent: %+v", reports[1])
	}
}

func TestTier2TokenBudget(t *testing.T) {
	inner := queue(ChatResponse{Content: "x", PromptTokens: 80, CompletionTokens: 30})
	b := NewBudget(inner, 0, 100)
	if _, err := b.Complete(context.Background(), ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Complete(context.Background(), ChatRequest{}); !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget after 110 tokens against a cap of 100", err)
	}
}

func TestTier2APIErrorStopsTheInvocation(t *testing.T) {
	backend := MockBackends()
	sc := scenarioOf(t, `{"name":"a","caller":"92223333","callerPersona":{"goal":"g"},"model":{}}`)
	sc2 := scenarioOf(t, `{"name":"b","caller":"92223333","callerPersona":{"goal":"g"},"model":{}}`)
	broken := &fakeChat{answer: func(int, ChatRequest) (ChatResponse, error) {
		return ChatResponse{}, errors.New("HTTP 401: invalid api key")
	}}
	reports := RunModel(context.Background(), earlyCollections(t), backend,
		ModelConfig{Bot: broken, Caller: queue(text("hi")), Runs: 3}, []Scenario{sc, sc2})

	if len(reports[0].Runs) != 1 || !strings.Contains(reports[0].Error, "invalid api key") {
		t.Errorf("first = %+v", reports[0])
	}
	if !reports[1].Skipped {
		t.Errorf("a model API error must not be followed by more requests: %+v", reports[1])
	}
	if len(broken.requests) != 1 {
		t.Errorf("requests after the failure: %d", len(broken.requests))
	}
}

func TestJudgeNamesWhatWentWrong(t *testing.T) {
	backend := MockBackends()
	sc := scenarioOf(t, `{"name":"wrong","caller":"92223333","callerPersona":{"goal":"g"},
		"model":{"phase":"finish_transfer","ending":"transfer_to_agent","transfer":{"queue":"billing"},"visits":["disputed"],"avoids":["remind"]}}`)
	bot := queue(tool("bill_lookup", `{}`), text("ok"), tool("hangup", `{}`), text("bye"))
	r := RunModel(context.Background(), earlyCollections(t), backend,
		ModelConfig{Bot: bot, Caller: queue(text("hi"), text("bye")), Runs: 1}, []Scenario{sc})[0]
	for _, want := range []string{`ended in phase "finish_declined", want "finish_transfer"`, `ended with "hangup"`, "no transfer succeeded",
		`never entered "disputed"`, `entered "remind", which it must not`} {
		if !strings.Contains(r.Runs[0].Failure, want) {
			t.Errorf("failure lacks %q: %s", want, r.Runs[0].Failure)
		}
	}
}

func TestModelScenariosAreSkippedByTier1AndValidated(t *testing.T) {
	backend := MockBackends()
	sc := scenarioOf(t, promiseScenario)
	if got := Run(context.Background(), earlyCollections(t), backend, []Scenario{sc}); len(got) != 0 {
		t.Errorf("tier 1 replayed a model-only scenario: %+v", got)
	}
	for name, body := range map[string]string{
		"nothing to run":        `{"name":"a"}`,
		"persona without model": `{"name":"a","callerPersona":{"goal":"g"}}`,
		"model without persona": `{"name":"a","steps":[{"noInput":true}],"model":{}}`,
		"no goal":               `{"name":"a","callerPersona":{},"model":{}}`,
		"bad pass rate":         `{"name":"a","callerPersona":{"goal":"g"},"model":{"passRate":2}}`,
	} {
		if _, err := ParseScenarios([]byte(`{"scenarios":[` + body + `]}`)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHTTPChatWire(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":null,"tool_calls":[
			{"id":"call_9","type":"function","function":{"name":"hangup","arguments":"{}"}}]}}],
			"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	}))
	defer server.Close()

	chat := &HTTPChat{BaseURL: server.URL + "/compatible-mode/v1/", APIKey: "k"}
	resp, err := chat.Complete(context.Background(), ChatRequest{
		Model: "qwen-plus",
		Messages: []ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "assistant", ToolCalls: []ChatToolCall{{ID: "c1", Name: "x", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "c1", Content: "{}"},
		},
		Tools: []ChatTool{{Name: "hangup", Description: "d"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/compatible-mode/v1/chat/completions" || gotAuth != "Bearer k" {
		t.Errorf("path %q auth %q", gotPath, gotAuth)
	}
	if gotBody["model"] != "qwen-plus" || len(gotBody["tools"].([]any)) != 1 {
		t.Errorf("body = %v", gotBody)
	}
	messages := gotBody["messages"].([]any)
	if messages[1].(map[string]any)["tool_calls"] == nil || messages[2].(map[string]any)["tool_call_id"] != "c1" {
		t.Errorf("messages = %v", messages)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_9" || resp.PromptTokens != 12 || resp.CompletionTokens != 3 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestHTTPChatErrorsAreNotRetried(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer server.Close()
	_, err := (&HTTPChat{BaseURL: server.URL, APIKey: "k"}).Complete(context.Background(), ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") || hits != 1 {
		t.Fatalf("err = %v, hits = %d", err, hits)
	}
}

func TestHTTPChatRejectsMalformedAnswers(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   `oops`,
		"no choices": `{"choices":[]}`,
		"api error":  `{"error":{"message":"bad model"}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		if _, err := (&HTTPChat{BaseURL: server.URL, APIKey: "k"}).Complete(context.Background(), ChatRequest{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		server.Close()
	}
}

// A response that spoke and recorded the caller's question in a phase where
// the caller speaks next is not followed by another model round, as on a call
// (the results go out without a turn): the next thing the bot model is asked
// for is its reply to the caller. A response that only called the tool still
// gets its round.
func TestTier2ASpokenSlotCallInACallerNextPhaseGetsNoFollowUpRound(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(testdataFlows, "renewal_offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := flow.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	scenario := scenarioOf(t, `{"name":"question","lang":"en",
		"callerPersona":{"goal":"ask about the price, then decline"},
		"model":{"phase":"close_decline","visits":["after_first_question"]}}`)
	caller := queue(text("Does the price change?"), text("No thanks."), text(callerHangup))

	spoke := tool("record_question", `{"value":"does the price change"}`)
	spoke.Content = "It stays at fifty yuan a month."
	bot := queue(spoke, tool("classify", `{"value":"decline"}`))
	reports := RunModel(context.Background(), spec, MockBackends(),
		ModelConfig{Bot: bot, Caller: caller, BotModel: "b", CallerModel: "c", Runs: 1},
		[]Scenario{scenario})
	if !reports[0].Succeeded() {
		t.Fatalf("scenario failed: %+v", reports[0])
	}
	if got := len(bot.requests); got != 2 {
		t.Errorf("the bot model was asked %d times, want 2: the question, then the caller's decision", got)
	}

	silent := tool("record_question", `{"value":"does the price change"}`)
	bot = queue(silent, text("It stays at fifty yuan."), tool("classify", `{"value":"decline"}`))
	caller = queue(text("Does the price change?"), text("No thanks."), text(callerHangup))
	RunModel(context.Background(), spec, MockBackends(),
		ModelConfig{Bot: bot, Caller: caller, BotModel: "b", CallerModel: "c", Runs: 1},
		[]Scenario{scenario})
	if got := len(bot.requests); got != 3 {
		t.Errorf("a response that only called the tool: the bot model was asked %d times, want 3", got)
	}
}
