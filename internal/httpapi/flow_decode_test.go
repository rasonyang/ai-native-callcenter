// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A spec whose JSON has the wrong shape is the author's mistake, not a storage
// fault: it is told apart from a down database by a 422 that names the field.
func TestASpecOfTheWrongShapeIsRefusedNamingTheField(t *testing.T) {
	t.Parallel()
	const head = `{"id":"probe","specVersion":"v2","initialNode":"welcome",`
	for _, tc := range []struct {
		name  string
		spec  string
		field string
	}{
		{"a number for an instruction", head +
			`"global":{"persona":"A helpful assistant."},"nodes":{"welcome":{"instruction":42}}}`, "text must be a string"},
		{"a number for the initial phase", `{"id":"probe","specVersion":"v2","initialNode":5,` +
			`"global":{"persona":"x"},"nodes":{}}`, "initialNode"},
		{"a malformed modelOnly entry", head +
			`"global":{"persona":"A helpful assistant."},"nodes":{"welcome":{"instruction":"Hi."}},` +
			`"tools":{"look":{"kind":"HTTP","http":{"path":"/x","modelOnly":{"a":3}}}}}`, "modelOnly entry must be"},
		{"a toolWaitLine that is not text", head +
			`"global":{"persona":"A helpful assistant.","toolWaitLine":42},"nodes":{"welcome":{"instruction":"Hi."}}}`, "text must be a string"},
	} {
		for _, op := range []string{"create", "update"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				t.Parallel()
				f := flowStub()
				f.load = true
				s := &Server{flows: f}
				rec := httptest.NewRecorder()
				if op == "create" {
					s.CreateFlow(rec, post(`{"slug":"probe","name":"Probe","spec":`+tc.spec+`}`))
				} else {
					s.UpdateFlowDraft(rec, post(`{"name":"Probe","spec":`+tc.spec+`}`), f.summary.ID)
				}
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
				}
				var body struct {
					Error struct {
						Code   string         `json:"code"`
						Params map[string]any `json:"params"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if body.Error.Code != string(CodeValidationFailed) {
					t.Errorf("code = %q, want VALIDATION_FAILED", body.Error.Code)
				}
				problems, _ := body.Error.Params["problems"].([]any)
				if len(problems) != 1 || !strings.Contains(problems[0].(string), tc.field) {
					t.Errorf("params.problems = %v, want one naming %q", problems, tc.field)
				}
			})
		}
	}
}

// Only a body that is not JSON at all is a 400; a well-formed spec still saves.
func TestATruncatedBodyStaysMalformedAndAValidSpecStillSaves(t *testing.T) {
	t.Parallel()
	f := flowStub()
	f.load = true
	s := &Server{flows: f}

	rec := httptest.NewRecorder()
	s.CreateFlow(rec, post(`{"slug":"probe","name":"Probe","spec":{"id":"pro`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("truncated create = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.UpdateFlowDraft(rec, post(`{"name":"Probe","spec":{"id":"pro`), f.summary.ID)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("truncated update = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.CreateFlow(rec, post(`{"slug":"probe","name":"Probe","spec":`+validSpecBody+`}`))
	if rec.Code != http.StatusCreated {
		t.Errorf("valid create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.UpdateFlowDraft(rec, post(`{"name":"Probe","spec":`+validSpecBody+`}`), f.summary.ID)
	if rec.Code != http.StatusOK {
		t.Errorf("valid update = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}
