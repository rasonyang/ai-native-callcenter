// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

func newTestNumberServer(t *testing.T, countryCode string) (*Server, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "testnumbers"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Server{cfg: config.Config{DefaultCountryCode: countryCode}, testNumbers: st.TestNumbers()}, st
}

func addTestNumber(t *testing.T, s *Server, body map[string]string) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/test-numbers", strings.NewReader(string(raw)))
	r = r.WithContext(contextWithAuth(r.Context(), AuthContext{Kind: SubjectUser, SubjectID: uuid.Must(uuid.NewV7()), ActorUserID: uuid.Must(uuid.NewV7())}))
	w := httptest.NewRecorder()
	s.CreateTestNumber(w, r)
	return w.Code, w.Body.Bytes()
}

// One number written three ways is one entry, with the deployment's default
// country code completing the national form; a note rides along; and the list
// and the removal work on the canonical number.
func TestATestNumberIsOneEntryInWhateverFormat(t *testing.T) {
	s, _ := newTestNumberServer(t, "86")
	if code, body := addTestNumber(t, s, map[string]string{"number": "+86 138 0013 8000", "note": "QA desk phone"}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	for _, spelling := range []string{"0086 13800138000", "13800138000"} {
		code, body := addTestNumber(t, s, map[string]string{"number": spelling, "note": "ignored"})
		var e api.TestNumber
		_ = json.Unmarshal(body, &e)
		if code != http.StatusOK || e.Number != "+8613800138000" || e.Note == nil || *e.Note != "QA desk phone" {
			t.Errorf("%q: %d %s, want 200 with the first entry", spelling, code, body)
		}
	}
	if code, _ := addTestNumber(t, s, map[string]string{"number": "not a number"}); code != http.StatusUnprocessableEntity {
		t.Errorf("a bad number: %d, want 422", code)
	}
	if code, _ := addTestNumber(t, s, map[string]string{"number": "13900139000", "note": strings.Repeat("n", 201)}); code != http.StatusUnprocessableEntity {
		t.Errorf("a 201-character note: %d, want 422", code)
	}

	w := httptest.NewRecorder()
	s.ListTestNumbers(w, httptest.NewRequest(http.MethodGet, "/api/v1/test-numbers", nil), api.ListTestNumbersParams{})
	var page api.TestNumberList
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("list = %s, want the one entry", w.Body.String())
	}

	del := func(n string) int {
		w := httptest.NewRecorder()
		s.DeleteTestNumber(w, httptest.NewRequest(http.MethodDelete, "/api/v1/test-numbers/"+url.PathEscape(n), nil), n)
		return w.Code
	}
	if c := del("13800138000"); c != http.StatusUnprocessableEntity {
		t.Errorf("deleting a non-canonical path: %d, want 422", c)
	}
	if c := del("+8613800138000"); c != http.StatusNoContent {
		t.Errorf("deleting: %d, want 204", c)
	}
	if c := del("+8613800138000"); c != http.StatusNotFound {
		t.Errorf("deleting twice: %d, want 404", c)
	}
}

// The hundredth number is accepted and the next new one is refused with 409
// TEST_NUMBER_LIMIT_REACHED; a number already listed is not an error even then.
func TestTheTestNumberListRefusesAHundredAndFirst(t *testing.T) {
	s, _ := newTestNumberServer(t, "86")
	for i := 0; i < store.MaxTestNumbers; i++ {
		if code, body := addTestNumber(t, s, map[string]string{"number": fmt.Sprintf("+86139%08d", i)}); code != http.StatusCreated {
			t.Fatalf("number %d: %d %s", i+1, code, body)
		}
	}
	code, body := addTestNumber(t, s, map[string]string{"number": "+8613999999999"})
	var env struct {
		Error struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	if code != http.StatusConflict || env.Error.Code != "TEST_NUMBER_LIMIT_REACHED" || env.Error.Params["limit"] != float64(store.MaxTestNumbers) {
		t.Errorf("the 101st: %d %s, want 409 TEST_NUMBER_LIMIT_REACHED with the limit", code, body)
	}
	if code, body := addTestNumber(t, s, map[string]string{"number": "+8613900000005"}); code != http.StatusOK {
		t.Errorf("re-adding a listed number at the cap: %d %s, want 200", code, body)
	}
}

// Who may do what, through the real router and real credentials, and what the
// audit trail records.
func TestOnlyScopedCredentialsReachTheTestNumberList(t *testing.T) {
	f := newFixture(t)
	admin := f.seedAdmin()
	_, readKey := f.issueKey(admin, "testnumbers:read")
	_, writeKey := f.issueKey(admin, "testnumbers:write")
	_, dncKey := f.issueKey(admin, "dnc:write", "campaigns:write")

	add := func(c call) reply { c.method, c.path = http.MethodPost, "/test-numbers"; return f.do(c) }
	body := map[string]string{"number": "+8613800138000"}

	if got := add(call{cookie: f.agent.cookie, body: body}); got.status != http.StatusForbidden || got.code != "INSUFFICIENT_SCOPE" {
		t.Errorf("an agent adding: %d %s, want 403 INSUFFICIENT_SCOPE", got.status, got.code)
	}
	if got := add(call{bearer: dncKey, body: body}); got.status != http.StatusForbidden {
		t.Errorf("a key with dnc:write and campaigns:write adding: %d, want 403", got.status)
	}
	if got := add(call{bearer: readKey, body: body}); got.status != http.StatusForbidden {
		t.Errorf("a read-only key adding: %d, want 403", got.status)
	}
	if got := f.do(call{method: http.MethodGet, path: "/test-numbers", bearer: writeKey}); got.status != http.StatusForbidden {
		t.Errorf("a write-only key reading: %d, want 403", got.status)
	}
	if got := add(call{bearer: writeKey, body: body}); got.status != http.StatusCreated {
		t.Fatalf("a write key adding: %d %s", got.status, got.body)
	}
	if got := f.do(call{method: http.MethodGet, path: "/test-numbers", bearer: readKey}); got.status != http.StatusOK {
		t.Errorf("a read key listing: %d", got.status)
	}
	if got := f.do(call{method: http.MethodDelete, path: "/test-numbers/%2B8613800138000", bearer: readKey}); got.status != http.StatusForbidden {
		t.Errorf("a read key removing: %d, want 403", got.status)
	}
	if got := f.do(call{method: http.MethodDelete, path: "/test-numbers/%2B8613800138000", bearer: writeKey}); got.status != http.StatusNoContent {
		t.Errorf("a write key removing: %d, want 204", got.status)
	}
	// A signed-in administrator holds both scopes through the role.
	if got := add(call{cookie: admin, body: body}); got.status != http.StatusCreated {
		t.Errorf("an administrator adding: %d %s", got.status, got.body)
	}

	for _, action := range []string{"POST /api/v1/test-numbers", "DELETE /api/v1/test-numbers/{number}"} {
		var n int
		if err := f.st.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_logs WHERE action = $1 AND subject_kind = 'API_KEY'`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("no audit row for %s by a key", action)
		}
	}
}
