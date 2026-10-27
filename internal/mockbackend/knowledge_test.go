// SPDX-License-Identifier: Apache-2.0

package mockbackend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// kbPost calls the aicc-knowledge stand-in and returns the status and body.
func kbPost(t *testing.T, path, bearer string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	New(nil).ServeHTTP(rec, req)
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, out
}

func TestKnowledgeSearchShapes(t *testing.T) {
	t.Parallel()
	code, out := kbPost(t, "/v1/search", "k", map[string]any{"query": "installation time", "language": "EN", "topK": 3})
	items, _ := out["items"].([]any)
	if code != 200 || out["status"] != "HIT" || len(items) != 2 || out["latencyMs"] == nil {
		t.Fatalf("EN HIT: %d %v", code, out)
	}
	if code, out = kbPost(t, "/v1/search", "k", map[string]any{"query": "安装要多久", "language": "ZH"}); out["status"] != "HIT" {
		t.Fatalf("ZH HIT: %d %v", code, out)
	}
	// The index is the request's language: a Chinese question on the EN index misses.
	if _, out = kbPost(t, "/v1/search", "k", map[string]any{"query": "安装要多久", "language": "EN"}); out["status"] != "NO_MATCH" {
		t.Fatalf("cross-language: %v", out)
	}
	if code, out = kbPost(t, "/v1/search", "k", map[string]any{"query": "unrelated", "language": "EN"}); code != 200 ||
		out["status"] != "NO_MATCH" || len(out["items"].([]any)) != 0 {
		t.Fatalf("NO_MATCH: %d %v", code, out)
	}
}

func TestKnowledgeErrorsAreTheEnvelope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		bearer string
		body   map[string]any
		status int
		code   string
	}{
		{"no key", "", map[string]any{"query": "x", "language": "EN"}, 401, "UNAUTHORIZED"},
		{"lower-case language", "k", map[string]any{"query": "x", "language": "en"}, 422, "VALIDATION_FAILED"},
		{"index down", "k", map[string]any{"query": "kb-down", "language": "EN"}, 503, "INDEX_UNAVAILABLE"},
	} {
		status, out := kbPost(t, "/v1/search", tc.bearer, tc.body)
		e, _ := out["error"].(map[string]any)
		if status != tc.status || e["code"] != tc.code || e["params"] == nil || e["message"] == nil {
			t.Errorf("%s: %d %v", tc.name, status, out)
		}
	}
}

func TestKnowledgePlanLookup(t *testing.T) {
	t.Parallel()
	_, out := kbPost(t, "/v1/facts/plans/lookup", "k", map[string]any{"key": map[string]any{"plan": "Plan 59 Plus"}})
	row, _ := out["row"].(map[string]any)
	if out["status"] != "FOUND" || row["monthly_fee"] != float64(59) || row["name_zh"] != "59元Plus套餐" ||
		row["speed_mbps"] != nil || out["validFrom"] != "2025-01-01" {
		t.Fatalf("FOUND: %v", out)
	}
	_, out = kbPost(t, "/v1/facts/plans/lookup", "k", map[string]any{"key": map[string]any{"plan": "Plan 79"}})
	if out["status"] != "NOT_FOUND" || out["row"] != nil {
		t.Fatalf("NOT_FOUND: %v", out)
	}
	if status, _ := kbPost(t, "/v1/facts/plans/lookup", "k", map[string]any{"key": map[string]any{"plan": "Basic"}, "at": "2026-12-01"}); status != 422 {
		t.Fatalf("at must be refused, got %d", status)
	}
}
