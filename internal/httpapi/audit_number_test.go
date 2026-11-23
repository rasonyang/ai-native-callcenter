// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
)

// #580: removing a number from a list records which number it was, and a
// create records the canonical number whatever spelling was posted.
func TestANumberRouteAuditsItsCanonicalTarget(t *testing.T) {
	// Country code 86, so the national spelling canonicalizes too.
	f := newFixtureWithConfig(t, func(c *config.Config) { c.DefaultCountryCode = "86" })
	admin := f.seedAdmin()
	_, dncWrite := f.issueKey(admin, "dnc:write")
	_, testWrite := f.issueKey(admin, "testnumbers:write")

	lastTarget := func(action string) (kind, id string) {
		t.Helper()
		var k, i string
		if err := f.st.Pool.QueryRow(context.Background(),
			`SELECT target_kind, target_id FROM audit_logs WHERE action = $1
			 ORDER BY id DESC LIMIT 1`, action).Scan(&k, &i); err != nil {
			t.Fatalf("no audit row for %s: %v", action, err)
		}
		return k, i
	}
	rows := func(action string) int {
		t.Helper()
		var n int
		if err := f.st.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Three spellings of one number — E.164, the 00 international prefix and
	// the national form — each audited against the canonical +8613800138000.
	for _, raw := range []string{"+86 138 0013 8000", "0086 13800138000", "13800138000"} {
		if got := f.do(call{method: http.MethodPost, path: "/dnc-numbers", bearer: dncWrite,
			body: map[string]string{"number": raw}}); got.status/100 != 2 {
			t.Fatalf("adding %q: %d %s", raw, got.status, got.body)
		}
		if kind, id := lastTarget("POST /api/v1/dnc-numbers"); kind != "dncNumber" || id != "+8613800138000" {
			t.Errorf("POST /dnc-numbers %q audited %s %q, want dncNumber +8613800138000", raw, kind, id)
		}
	}
	for _, raw := range []string{"+86 138 0013 8000", "0086 13800138000", "13800138000"} {
		if got := f.do(call{method: http.MethodPost, path: "/test-numbers", bearer: testWrite,
			body: map[string]string{"number": raw}}); got.status/100 != 2 {
			t.Fatalf("adding %q: %d %s", raw, got.status, got.body)
		}
		if kind, id := lastTarget("POST /api/v1/test-numbers"); kind != "testNumber" || id != "+8613800138000" {
			t.Errorf("POST /test-numbers %q audited %s %q, want testNumber +8613800138000", raw, kind, id)
		}
	}

	// Removing a number names it: the removal is what makes it dialable again.
	for _, action := range []struct{ path, action, kind string }{
		{"/dnc-numbers/%2B8613800138000", "DELETE /api/v1/dnc-numbers/{number}", "dncNumber"},
		{"/test-numbers/%2B8613800138000", "DELETE /api/v1/test-numbers/{number}", "testNumber"},
	} {
		before := rows(action.action)
		if got := f.do(call{method: http.MethodDelete, path: action.path, cookie: admin}); got.status != http.StatusNoContent {
			t.Fatalf("DELETE %s: %d %s", action.path, got.status, got.body)
		}
		if rows(action.action) != before+1 {
			t.Fatalf("%s left no audit row", action.action)
		}
		if kind, id := lastTarget(action.action); kind != action.kind || id != "+8613800138000" {
			t.Errorf("%s audited %s %q, want %s +8613800138000", action.action, kind, id, action.kind)
		}
	}
}

// A refused request changed nothing and writes no row, as before.
func TestARefusedNumberRequestAuditsNothing(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	_, dncWrite := f.issueKey(admin, "dnc:write")
	_, readKey := f.issueKey(admin, "dnc:read")

	rows := func() int {
		t.Helper()
		var n int
		if err := f.st.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	before := rows()
	if got := f.do(call{method: http.MethodPost, path: "/dnc-numbers", bearer: dncWrite,
		body: map[string]string{"number": "not a number"}}); got.status != http.StatusUnprocessableEntity {
		t.Fatalf("a bad number: %d %s, want 422", got.status, got.body)
	}
	if got := f.do(call{method: http.MethodPost, path: "/dnc-numbers", bearer: readKey,
		body: map[string]string{"number": "+8613800138000"}}); got.status != http.StatusForbidden {
		t.Fatalf("a read key adding: %d, want 403", got.status)
	}
	if got := f.do(call{method: http.MethodDelete, path: "/dnc-numbers/%2B8613800138000",
		cookie: admin}); got.status != http.StatusNotFound {
		t.Fatalf("removing a number never added: %d, want 404", got.status)
	}
	if got := f.do(call{method: http.MethodDelete, path: "/dnc-numbers/not-a-number",
		cookie: admin}); got.status != http.StatusUnprocessableEntity {
		t.Fatalf("removing a non-canonical path: %d, want 422", got.status)
	}
	if n := rows(); n != before {
		t.Errorf("%d audit rows written by refused requests, want none", n-before)
	}
}

// Every other audited route keeps deriving its target from the {xxxId}
// parameter; a {number} parameter names the list segment before it.
func TestTargetFromNamesTheRouteParameter(t *testing.T) {
	cases := []struct {
		pattern          string
		keys, values     []string
		wantKind, wantID string
	}{
		{"/api/v1/campaigns/{campaignId}", []string{"campaignId"}, []string{"c1"}, "campaign", "c1"},
		{"/api/v1/flows/{flowId}", []string{"flowId"}, []string{"f1"}, "flow", "f1"},
		{"/api/v1/flows/{flowId}/revisions/{revisionId}",
			[]string{"flowId", "revisionId"}, []string{"f1", "r9"}, "revision", "r9"},
		{"/api/v1/queues/{queueId}/agents/{agentId}",
			[]string{"queueId", "agentId"}, []string{"q1", "a1"}, "agent", "a1"},
		{"/api/v1/dnc-numbers/{number}", []string{"number"}, []string{"+8613800138000"}, "dncNumber", "+8613800138000"},
		{"/api/v1/test-numbers/{number}", []string{"number"}, []string{"+8613800138000"}, "testNumber", "+8613800138000"},
		{"/api/v1/flows/{flowId}", []string{"flowId"}, []string{""}, "flow", ""},
	}
	for _, c := range cases {
		rctx := chi.NewRouteContext()
		rctx.URLParams = chi.RouteParams{Keys: c.keys, Values: c.values}
		rctx.RoutePatterns = []string{c.pattern}
		req := httptest.NewRequest(http.MethodDelete, "/x", nil).
			WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
		kind, id := targetFrom(req)
		if kind != c.wantKind || id != c.wantID {
			t.Errorf("%s → %s %q, want %s %q", c.pattern, kind, id, c.wantKind, c.wantID)
		}
	}
}
