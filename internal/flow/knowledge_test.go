// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A flow that reads from aicc-knowledge: a fake of the service checks every
// request the templates send against the service's contract and answers each
// case, and the tests drive the real Runtime and Backend against it.

const (
	kbAPIKey      = "kb-key-7c1e9a40d2b84f6e"
	kbMessage     = "MESSAGE-FROM-BACKEND-must-not-travel"
	kbSearchPath  = "/v1/search"
	kbFactsPath   = "/v1/facts/plans/lookup"
	kbSearchBlock = `{
	  "baseUrl": "BASE", "path": "/v1/search", "timeoutMs": TIMEOUT,
	  "headers": {"Authorization": "Bearer {secrets.KB_API_KEY}"},
	  "body": {"query": "{args.query}", "language": "{slots.language}", "topK": 3, "timeoutMs": 2500},
	  "errorFrom": "error.code",
	  "result": {"kbStatus": "status"},
	  "modelOnly": {
	    "status": "status",
	    "items": {"path": "items", "each": {"answer": "answer", "sourceRef": "sourceRef"}}
	  }
	}`
	kbFactsBlock = `{
	  "baseUrl": "BASE", "path": "/v1/facts/plans/lookup", "timeoutMs": 3000,
	  "headers": {"Authorization": "Bearer {secrets.KB_API_KEY}"},
	  "body": {"key": {"plan": "{args.plan}"}},
	  "errorFrom": "error.code",
	  "modelOnly": {"status": "status", "row": "row", "validFrom": "validFrom", "validTo": "validTo"}
	}`
)

func kbFlow(baseURL string, searchTimeoutMs int) string {
	search := strings.NewReplacer("BASE", baseURL, "TIMEOUT", strconv.Itoa(searchTimeoutMs)).Replace(kbSearchBlock)
	facts := strings.ReplaceAll(kbFactsBlock, "BASE", baseURL)
	return `{
	  "id": "knowledge", "specVersion": "v2", "initialNode": "main",
	  "global": {"persona": "You answer questions."},
	  "nodes": {"main": {"instruction": "Help the caller.", "tools": ["*"]}},
	  "tools": {
	    "search_kb": {
	      "description": "Search the FAQ.",
	      "parameters": {"type":"object","properties":{"query":{"type":"string"}},"required":["query"]},
	      "http": ` + search + `
	    },
	    "lookup_plan": {
	      "description": "Look up a plan.",
	      "parameters": {"type":"object","properties":{"plan":{"type":"string","enum":["Basic","Plus"]}},"required":["plan"]},
	      "http": ` + facts + `
	    }
	  }
	}`
}

// kbAnswer is what the fake answers one request with.
type kbAnswer struct {
	status int
	body   string
	// block holds the request until its context ends, so that the client's
	// own bound is what ends it.
	block bool
	// pad appends that many filler bytes after body.
	pad int
}

func kbErr(status int, code string, params string) kbAnswer {
	if params == "" {
		params = "{}"
	}
	return kbAnswer{status: status, body: `{"error":{"code":"` + code + `","message":"` + kbMessage + `","params":` + params + `}}`}
}

// newKBFake serves both operations, checking each request against the
// contract before answering.
func newKBFake(t *testing.T, answer kbAnswer) *testBackend {
	t.Helper()
	var server *testBackend
	server = newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		check := func(ok bool, format string, args ...any) {
			if !ok {
				t.Errorf("contract violation on %s: %s", r.URL.Path, fmt.Sprintf(format, args...))
			}
		}
		check(r.Method == http.MethodPost, "method %s", r.Method)
		check(r.Header.Get("Content-Type") == "application/json", "Content-Type %q", r.Header.Get("Content-Type"))
		check(r.Header.Get("Authorization") == "Bearer "+kbAPIKey, "Authorization %q", r.Header.Get("Authorization"))
		// testBackend has already consumed the body and recorded the request.
		requests := server.received()
		recordedBody := requests[len(requests)-1].body
		var body map[string]any
		check(json.Unmarshal(recordedBody, &body) == nil, "body is not a JSON object: %s", recordedBody)
		switch r.URL.Path {
		case kbSearchPath:
			checkSearchRequest(check, body)
		case kbFactsPath:
			checkFactsRequest(check, body)
		default:
			check(false, "unknown path")
		}
		if answer.block {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_, _ = io.WriteString(w, answer.body)
		if answer.pad > 0 {
			_, _ = io.WriteString(w, strings.Repeat("x", answer.pad))
		}
	})
	return server
}

func isInteger(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func checkSearchRequest(check func(bool, string, ...any), body map[string]any) {
	for key, value := range body {
		switch key {
		case "query":
			s, ok := value.(string)
			check(ok && s != "" && len([]rune(s)) <= 1000, "query %v", value)
		case "language":
			check(value == "EN" || value == "ZH", "language %v", value)
		case "topK":
			n, ok := isInteger(value)
			check(ok && n >= 1 && n <= 3, "topK %v", value)
		case "timeoutMs":
			n, ok := isInteger(value)
			check(ok && n >= 1 && n <= 60000, "timeoutMs %v", value)
		case "scope":
			scope, ok := value.(map[string]any)
			check(ok, "scope %v", value)
			for _, v := range scope {
				_, isString := v.(string)
				check(isString, "scope value %v", v)
			}
		default:
			check(false, "unexpected field %q", key)
		}
	}
	_, hasQuery := body["query"]
	_, hasLanguage := body["language"]
	check(hasQuery && hasLanguage, "query and language are required")
}

func checkFactsRequest(check func(bool, string, ...any), body map[string]any) {
	_, hasAt := body["at"]
	check(!hasAt, "a fact request must not carry at")
	key, ok := body["key"].(map[string]any)
	check(ok && len(key) > 0, "key %v", body["key"])
	check(len(body) == 1, "unexpected fields in %v", body)
}

func kbRuntime(t *testing.T, server *testBackend, lang string, searchTimeoutMs int) (*Runtime, *logCapture) {
	t.Helper()
	spec, err := Load([]byte(kbFlow(server.URL, searchTimeoutMs)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logs := &logCapture{}
	backend := NewBackend("", WithSecrets(secretsFrom(map[string]string{"AICC_TOOL_SECRET_KB_API_KEY": kbAPIKey})))
	runtime := NewRuntime(NewEngine(spec, lang, map[string]any{"caller": "13800138000"}, logs.logger()),
		&fakeActions{}, backend, nil, logs.logger())
	return runtime, logs
}

// kbCall dispatches one tool call and checks what must hold for every case:
// the API key is nowhere a reader could find it, and the output is a JSON
// object.
func kbCall(t *testing.T, r *Runtime, logs *logCapture, tool, args string) (map[string]any, string) {
	t.Helper()
	output, _ := r.Dispatch(t.Context(), tool, args)
	for where, text := range map[string]string{
		"the log": logs.String(), "the tool output": output, "the slots": fmt.Sprint(r.engine.Slots()),
	} {
		if strings.Contains(text, kbAPIKey) {
			t.Errorf("%s contains the API key:\n%s", where, text)
		}
		if strings.Contains(text, kbMessage) {
			t.Errorf("%s carries the backend's message:\n%s", where, text)
		}
	}
	return decodeOutput(t, output), output
}

const kbHit = `{"status":"HIT","latencyMs":41,"items":[
  {"id":"faq-1","question":"Q-ONE-text","answer":"A-ONE: Plan 59 costs 59 per month.","sourceRef":"tariff.docx#p3","score":0.91},
  {"id":"faq-2","question":"Q-TWO-text","answer":"A-TWO: Plan 59 Plus costs 79.","sourceRef":"tariff.docx#p4","score":0.74},
  {"id":"faq-3","question":"Q-THREE-text","answer":"A-THREE: Prices exclude tax.","sourceRef":"policy.docx#p1","score":0.52}]}`

// assertNoModelOnlySlots fails when anything a modelOnly mapping produced is
// in the slots; only what result maps (kbStatus) may be there.
func assertNoModelOnlySlots(t *testing.T, r *Runtime, tool string) {
	t.Helper()
	slots := r.engine.Slots()
	for _, name := range []string{"status", "items", "row", "validFrom", "validTo", "answer", "sourceRef"} {
		for _, key := range []string{"result." + name, tool + "." + name} {
			if _, found := slots[key]; found {
				t.Errorf("slot %q holds a model-only value: %v", key, slots[key])
			}
		}
	}
	dump := fmt.Sprint(slots)
	for _, leaked := range []string{"A-ONE", "A-TWO", "A-THREE", "tariff.docx", "59.00", "plans.xlsx"} {
		if strings.Contains(dump, leaked) {
			t.Errorf("the slots hold %q: %s", leaked, dump)
		}
	}
}

// A HIT reaches the model with status and each item's answer and sourceRef,
// and nothing else of the items; no slot holds any of it. A NO_MATCH is a
// success with an empty list.
func TestKnowledgeSearchAnswersReachTheModelNotTheSlots(t *testing.T) {
	t.Parallel()
	t.Run("hit", func(t *testing.T) {
		t.Parallel()
		server := newKBFake(t, kbAnswer{status: 200, body: kbHit})
		r, logs := kbRuntime(t, server, "en", 3000)
		out, raw := kbCall(t, r, logs, "search_kb", `{"query":"how much is plan 59"}`)

		if out["ok"] != "1" || out["status"] != "HIT" {
			t.Fatalf("output = %s", raw)
		}
		items, _ := out["items"].([]any)
		if len(items) != 3 {
			t.Fatalf("items = %v, want 3", out["items"])
		}
		for i, item := range items {
			object, _ := item.(map[string]any)
			keys := make([]string, 0, len(object))
			for k := range object {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, []string{"answer", "sourceRef"}) {
				t.Errorf("item %d has fields %v, want answer and sourceRef only", i, keys)
			}
		}
		if first, _ := items[0].(map[string]any); first["answer"] != "A-ONE: Plan 59 costs 59 per month." || first["sourceRef"] != "tariff.docx#p3" {
			t.Errorf("first item = %v", first)
		}
		for _, withheld := range []string{"score", "0.91", "faq-1", "Q-ONE-text", "latencyMs"} {
			if strings.Contains(raw, withheld) {
				t.Errorf("the output carries %q: %s", withheld, raw)
			}
		}
		assertNoModelOnlySlots(t, r, "search_kb")
		if got, _ := r.engine.Slot("result.kbStatus"); got != "HIT" {
			t.Errorf("result.kbStatus = %v, want HIT (the mapped result still reaches the slots)", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		t.Parallel()
		server := newKBFake(t, kbAnswer{status: 200, body: `{"status":"NO_MATCH","items":[],"latencyMs":12}`})
		r, logs := kbRuntime(t, server, "en", 3000)
		out, raw := kbCall(t, r, logs, "search_kb", `{"query":"do you sell cars"}`)
		items, isList := out["items"].([]any)
		if out["ok"] != "1" || out["status"] != "NO_MATCH" || !isList || len(items) != 0 {
			t.Errorf("output = %s", raw)
		}
		assertNoModelOnlySlots(t, r, "search_kb")
	})
}

// Every failure the service or the network can produce is a failure the model
// reads (ok=0), the log names the service's error code, and neither the
// backend's message nor the key travels.
func TestKnowledgeSearchFailuresNameTheBackendCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		answer     kbAnswer
		timeoutMs  int
		wantStatus int
		wantCode   string
	}{
		{"401 UNAUTHORIZED", kbErr(401, "UNAUTHORIZED", ""), 3000, 401, "UNAUTHORIZED"},
		{"422 UNKNOWN_SCOPE_KEY", kbErr(422, "UNKNOWN_SCOPE_KEY", `{"key":"region"}`), 3000, 422, "UNKNOWN_SCOPE_KEY"},
		{"503 INDEX_UNAVAILABLE", kbErr(503, "INDEX_UNAVAILABLE", ""), 3000, 503, "INDEX_UNAVAILABLE"},
		{"503 UPSTREAM_UNAVAILABLE", kbErr(503, "UPSTREAM_UNAVAILABLE", ""), 3000, 503, "UPSTREAM_UNAVAILABLE"},
		{"504 UPSTREAM_TIMEOUT", kbErr(504, "UPSTREAM_TIMEOUT", ""), 3000, 504, "UPSTREAM_TIMEOUT"},
		{"an error with no envelope", kbAnswer{status: 502, body: "<html>bad gateway</html>"}, 3000, 502, ""},
		{"slower than the tool timeout", kbAnswer{block: true}, MinToolTimeoutMs, 0, ""},
		{"oversized", kbAnswer{status: 200, body: `{"status":"HIT","items":[],"pad":"`, pad: maxResponseBytes + 4096}, 3000, 0, ""},
		{"not JSON", kbAnswer{status: 200, body: "this is not json"}, 3000, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newKBFake(t, tc.answer)
			r, logs := kbRuntime(t, server, "en", tc.timeoutMs)
			out, raw := kbCall(t, r, logs, "search_kb", `{"query":"how much is plan 59"}`)

			if out["ok"] != "0" {
				t.Fatalf("a failure reached the model as a success: %s", raw)
			}
			for _, field := range []string{"status", "items"} {
				if _, present := out[field]; present {
					t.Errorf("a failed call carries %q: %s", field, raw)
				}
			}
			assertNoModelOnlySlots(t, r, "search_kb")
			text := logs.String()
			switch {
			case tc.wantCode != "":
				if !strings.Contains(text, `"backendError":"`+tc.wantCode+`"`) ||
					!strings.Contains(text, `"msg":"http tool answered with an error status"`) ||
					!strings.Contains(text, `"status":`+strconv.Itoa(tc.wantStatus)) {
					t.Errorf("the log does not name %s:\n%s", tc.wantCode, text)
				}
				if want := fmt.Sprintf("returned %d (%s)", tc.wantStatus, tc.wantCode); !strings.Contains(raw, want) {
					t.Errorf("the error = %s, want it to say %q", raw, want)
				}
			case tc.wantStatus != 0:
				if !strings.Contains(text, `"backendError":""`) {
					t.Errorf("the log lacks the error status line:\n%s", text)
				}
				if !strings.Contains(raw, "returned 502") || strings.Contains(raw, "returned 502 (") {
					t.Errorf("output = %s, want an error with no code", raw)
				}
			}
			if tc.name == "slower than the tool timeout" && !strings.Contains(raw, "did not answer within 500 ms") {
				t.Errorf("output = %s, want it to say the bound", raw)
			}
		})
	}
}

// The fact lookup: a row, a miss, and the table-level failures.
func TestKnowledgeFactLookup(t *testing.T) {
	t.Parallel()
	t.Run("found", func(t *testing.T) {
		t.Parallel()
		server := newKBFake(t, kbAnswer{status: 200, body: `{"status":"FOUND","row":{"plan":"Basic","price":"59.00"},
			"sourceRef":"plans.xlsx#r2","validFrom":"2026-10-01","validTo":null}`})
		r, logs := kbRuntime(t, server, "zh", 3000)
		out, raw := kbCall(t, r, logs, "lookup_plan", `{"plan":"Basic"}`)
		row, _ := out["row"].(map[string]any)
		if out["ok"] != "1" || out["status"] != "FOUND" || row["price"] != "59.00" ||
			out["validFrom"] != "2026-10-01" {
			t.Fatalf("output = %s", raw)
		}
		if validTo, present := out["validTo"]; !present || validTo != nil {
			t.Errorf("validTo = %v (present %v), want an explicit null", validTo, present)
		}
		if strings.Contains(raw, "plans.xlsx") {
			t.Errorf("sourceRef was not mapped but reached the model: %s", raw)
		}
		assertNoModelOnlySlots(t, r, "lookup_plan")
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		server := newKBFake(t, kbAnswer{status: 200, body: `{"status":"NOT_FOUND","row":null,"sourceRef":null,"validFrom":null,"validTo":null}`})
		r, logs := kbRuntime(t, server, "en", 3000)
		out, raw := kbCall(t, r, logs, "lookup_plan", `{"plan":"Plus"}`)
		if out["ok"] != "1" || out["status"] != "NOT_FOUND" || out["row"] != nil {
			t.Errorf("output = %s", raw)
		}
		assertNoModelOnlySlots(t, r, "lookup_plan")
	})

	for _, tc := range []struct {
		name   string
		answer kbAnswer
		status int
		code   string
	}{
		{"404 FACT_TABLE_NOT_FOUND", kbErr(404, "FACT_TABLE_NOT_FOUND", `{"table":"plans"}`), 404, "FACT_TABLE_NOT_FOUND"},
		{"503 FACT_TABLE_UNAVAILABLE", kbErr(503, "FACT_TABLE_UNAVAILABLE", ""), 503, "FACT_TABLE_UNAVAILABLE"},
		{"422 VALIDATION_FAILED with params.missing", kbErr(422, "VALIDATION_FAILED", `{"missing":["plan"]}`), 422, "VALIDATION_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newKBFake(t, tc.answer)
			r, logs := kbRuntime(t, server, "en", 3000)
			out, raw := kbCall(t, r, logs, "lookup_plan", `{"plan":"Basic"}`)
			if out["ok"] != "0" {
				t.Fatalf("a failure reached the model as a success: %s", raw)
			}
			if !strings.Contains(logs.String(), `"backendError":"`+tc.code+`"`) {
				t.Errorf("the log does not name %s:\n%s", tc.code, logs.String())
			}
			if strings.Contains(raw, "missing") {
				t.Errorf("the backend's params reached the model: %s", raw)
			}
			assertNoModelOnlySlots(t, r, "lookup_plan")
		})
	}
}

// The language sent is the call's, whatever the model passes.
func TestKnowledgeLanguageComesFromTheCallNotTheModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ lang, modelSays, want string }{
		{"en", "ZH", "EN"},
		{"en", "zh", "EN"},
		{"zh", "EN", "ZH"},
		{"zh", "", "ZH"},
		{"zh-CN", "EN", "ZH"},
	} {
		t.Run(tc.lang+" call, model says "+tc.modelSays, func(t *testing.T) {
			t.Parallel()
			server := newKBFake(t, kbAnswer{status: 200, body: `{"status":"NO_MATCH","items":[],"latencyMs":1}`})
			r, logs := kbRuntime(t, server, tc.lang, 3000)
			args := `{"query":"hello"`
			if tc.modelSays != "" {
				args += `,"language":"` + tc.modelSays + `"`
			}
			kbCall(t, r, logs, "search_kb", args+`}`)

			got := server.received()
			if len(got) != 1 {
				t.Fatalf("backend received %d requests", len(got))
			}
			var body map[string]any
			if err := json.Unmarshal(got[0].body, &body); err != nil {
				t.Fatal(err)
			}
			if body["language"] != tc.want {
				t.Errorf("language sent = %v, want %s", body["language"], tc.want)
			}
			if lang, _ := r.engine.Slot("lang"); lang != Lang(tc.lang) {
				t.Errorf("slots.lang = %v, want the unchanged %q", lang, Lang(tc.lang))
			}
		})
	}
}

// A fact request never carries at, and the body carries the typed key.
func TestKnowledgeFactRequestCarriesTheKeyAndNoAt(t *testing.T) {
	t.Parallel()
	server := newKBFake(t, kbAnswer{status: 200, body: `{"status":"NOT_FOUND","row":null}`})
	r, logs := kbRuntime(t, server, "en", 3000)
	kbCall(t, r, logs, "lookup_plan", `{"plan":"Basic","at":"2027-01-01"}`)
	got := server.received()
	if len(got) != 1 || string(got[0].body) != `{"key":{"plan":"Basic"}}` {
		t.Fatalf("requests = %+v", got)
	}
}

//
// Load-time rules.
//

const kbParams = `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`

func TestModelOnlyIsValidatedAtLoad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, block, want string }{
		{"a name that is ok", `{"path":"/x","modelOnly":{"ok":"status"}}`, `modelOnly output "ok"`},
		{"a name that is error", `{"path":"/x","modelOnly":{"error":"status"}}`, `modelOnly output "error"`},
		{"a name that is hint", `{"path":"/x","modelOnly":{"hint":"status"}}`, `modelOnly output "hint"`},
		{"an empty name", `{"path":"/x","modelOnly":{"":"status"}}`, "an output with no name"},
		{"also a result key", `{"path":"/x","result":{"status":"s"},"modelOnly":{"status":"status"}}`, "is also a key of result"},
		{"an empty path", `{"path":"/x","modelOnly":{"a":""}}`, `output "a" has an empty path`},
		{"a projection with no path", `{"path":"/x","modelOnly":{"a":{"each":{"b":"c"}}}}`, `output "a" has an empty path`},
		{"a projection with no each", `{"path":"/x","modelOnly":{"a":{"path":"items"}}}`, `lists no fields in each`},
		{"a projection with an empty each", `{"path":"/x","modelOnly":{"a":{"path":"items","each":{}}}}`, `lists no fields in each`},
		{"an empty each path", `{"path":"/x","modelOnly":{"a":{"path":"items","each":{"b":""}}}}`, `each field "b" has an empty path`},
		{"an unknown key", `{"path":"/x","modelOnly":{"a":{"path":"items","each":{"b":"c"},"limit":3}}}`, "cannot parse"},
		{"a number", `{"path":"/x","modelOnly":{"a":3}}`, "cannot parse"},
		{"an array", `{"path":"/x","modelOnly":{"a":["items"]}}`, "cannot parse"},
		{"null", `{"path":"/x","modelOnly":{"a":null}}`, "cannot parse"},
		{"a non-string each path", `{"path":"/x","modelOnly":{"a":{"path":"items","each":{"b":4}}}}`, "cannot parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(toolFlowJSON(kbParams, tc.block)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	spec := toolFlow(t, kbParams, `{"path":"/x","result":{"k":"status"},"modelOnly":{
		"status":"status","items":{"path":"items","each":{"answer":"answer"}}}}`)
	if len(spec.Tools["lookup"].HTTP.ModelOnly) != 2 {
		t.Errorf("a valid block did not load: %+v", spec.Tools["lookup"].HTTP)
	}
}

// A flow is stored and re-marshalled; both forms must come back as written.
func TestModelOnlyRoundTrips(t *testing.T) {
	t.Parallel()
	const block = `{"path":"/x","modelOnly":{"items":{"path":"items","each":{"answer":"answer"}},"status":"status"}}`
	spec := toolFlow(t, kbParams, block)
	encoded, err := json.Marshal(spec.Tools["lookup"].HTTP)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(block), &want); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got["modelOnly"]) != fmt.Sprint(want["modelOnly"]) {
		t.Errorf("modelOnly came back as %v, want %v", got["modelOnly"], want["modelOnly"])
	}
}

// A condition or a template that reads a model-only name as a slot is refused
// with the reason, not as a typo.
func TestAModelOnlyNameIsNotASlot(t *testing.T) {
	t.Parallel()
	base := func(extra string) string {
		return strings.Replace(toolFlowJSON(kbParams, `{"path":"/x","result":{"k":"status"},"modelOnly":{"status":"status"}}`),
			`"nodes": {"main": {"instruction": "Help the caller.", "tools": ["*"]}}`, extra, 1)
	}
	for _, tc := range []struct{ name, nodes string }{
		{"a condition on result.status", `"nodes": {"main": {"instruction": "Help.", "tools": ["*"], "transitions": [
			{"on": "TOOL_RESULT", "tool": "lookup", "condition": {"slot": "result.status", "op": "EQ", "value": "HIT"}, "target": "done"}]},
			"done": {"instruction": "Bye.", "isTerminal": true}}`},
		{"a condition on lookup.status", `"nodes": {"main": {"instruction": "Help.", "tools": ["*"], "transitions": [
			{"on": "TOOL_RESULT", "tool": "lookup", "condition": {"slot": "lookup.status", "op": "EQ", "value": "HIT"}, "target": "done"}]},
			"done": {"instruction": "Bye.", "isTerminal": true}}`},
		{"an instruction", `"nodes": {"main": {"instruction": "Status was {slots.result.status}.", "tools": ["*"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(base(tc.nodes)))
			if err == nil || !strings.Contains(err.Error(), "handed to the model only") ||
				!strings.Contains(err.Error(), "map it in result to read it") {
				t.Errorf("err = %v, want the model-only explanation", err)
			}
		})
	}
	// The mapped name is readable.
	ok := base(`"nodes": {"main": {"instruction": "Help.", "tools": ["*"], "transitions": [
		{"on": "TOOL_RESULT", "tool": "lookup", "condition": {"slot": "result.k", "op": "EQ", "value": "HIT"}, "target": "done"}]},
		"done": {"instruction": "Bye.", "isTerminal": true}}`)
	if _, err := Load([]byte(ok)); err != nil {
		t.Errorf("a condition on the result-mapped name: %v", err)
	}
}

// slots.language belongs to the engine.
func TestTheLanguageSlotIsReserved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ lang, want string }{{"en", "EN"}, {"zh", "ZH"}, {"zh-CN", "ZH"}, {"", "EN"}} {
		engine := testEngineFor(t, testFlow, tc.lang)
		if got, _ := engine.Slot("language"); got != tc.want {
			t.Errorf("lang %q: slots.language = %v, want %s", tc.lang, got, tc.want)
		}
	}

	slotTool := strings.Replace(toolFlowJSON(kbParams, `{"path":"/x"}`), `"tools": {"lookup": {`,
		`"tools": {"say": {"kind":"slot","slot":"language","description":"d"}, "lookup": {`, 1)
	if _, err := Load([]byte(slotTool)); err == nil || !strings.Contains(err.Error(), `writes slot "language", which the engine owns`) {
		t.Errorf("a slot tool writing language: %v", err)
	}
	collect := strings.Replace(toolFlowJSON(kbParams, `{"path":"/x"}`), `"http":`,
		`"collect": {"slot": "language", "value": "{args.q}"}, "http":`, 1)
	if _, err := Load([]byte(collect)); err == nil || !strings.Contains(err.Error(), "written by the engine") {
		t.Errorf("a collect list into language: %v", err)
	}
	// Reading it is fine.
	toolFlow(t, kbParams, `{"path":"/x","body":{"language":"{slots.language}"}}`)
}

func TestBackendErrorCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, errorFrom, body, want string }{
		{"a string", "error.code", `{"error":{"code":"INDEX_UNAVAILABLE"}}`, "INDEX_UNAVAILABLE"},
		{"no errorFrom", "", `{"error":{"code":"X"}}`, ""},
		{"not JSON", "error.code", `<html>`, ""},
		{"an array body", "error.code", `[1]`, ""},
		{"path missing", "error.code", `{"error":{}}`, ""},
		{"an object at the path", "error.code", `{"error":{"code":{"a":1}}}`, ""},
		{"null at the path", "error.code", `{"error":{"code":null}}`, ""},
		{"a number", "error.code", `{"error":{"code":42}}`, "42"},
		{"control characters", "error.code", `{"error":{"code":"A\nB\u0000C\u001b[31mD"}}`, "ABC[31mD"},
		{"cut to 128", "error.code", `{"error":{"code":"` + strings.Repeat("é", 300) + `"}}`, strings.Repeat("é", 128)},
	} {
		if got := backendErrorCode(tc.errorFrom, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: backendErrorCode = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A projection skips elements that are not objects and leaves out a field that
// does not resolve; a path that is missing or not an array yields nothing.
func TestModelOnlyExtraction(t *testing.T) {
	t.Parallel()
	response := map[string]any{
		"items":  []any{map[string]any{"a": "1", "b": "2", "c": map[string]any{"d": "deep"}}, "text", 3.0, map[string]any{"a": "x"}},
		"scalar": "s",
	}
	got, ok := ModelOnlyField{Path: "items", IsProjection: true, Each: map[string]string{"a": "a", "d": "c.d", "gone": "zzz"}}.extract(response)
	want := `[map[a:1 d:deep] map[a:x]]`
	if !ok || fmt.Sprint(got) != want {
		t.Errorf("projection = %v (%v), want %s", got, ok, want)
	}
	for name, field := range map[string]ModelOnlyField{
		"missing path":           {Path: "nope"},
		"projection of a scalar": {Path: "scalar", IsProjection: true, Each: map[string]string{"a": "a"}},
		"projection of missing":  {Path: "nope", IsProjection: true, Each: map[string]string{"a": "a"}},
	} {
		if v, ok := field.extract(response); ok {
			t.Errorf("%s: extracted %v, want nothing", name, v)
		}
	}
	if v, ok := (ModelOnlyField{Path: "scalar"}).extract(response); !ok || v != "s" {
		t.Errorf("string form = %v, %v", v, ok)
	}
}
