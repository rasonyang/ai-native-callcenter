// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// stubFlows answers whatever a case needs and remembers what it was handed.
type stubFlows struct {
	summary store.FlowSummary
	detail  store.FlowDetail
	err     error

	createdSlug string
	createdName string
	draft       []byte
	publishNote string
	published   bool
}

func (f *stubFlows) List(context.Context) ([]store.FlowSummary, error) {
	return []store.FlowSummary{f.summary}, f.err
}

func (f *stubFlows) Summary(context.Context, uuid.UUID) (store.FlowSummary, error) {
	return f.summary, nil
}

func (f *stubFlows) Detail(context.Context, uuid.UUID) (store.FlowDetail, error) {
	return f.detail, f.err
}

func (f *stubFlows) Create(_ context.Context, slug, name string, draft []byte) (uuid.UUID, error) {
	f.createdSlug, f.createdName, f.draft = slug, name, draft
	return f.summary.ID, f.err
}

func (f *stubFlows) UpdateDraft(_ context.Context, _ uuid.UUID, name string, draft []byte) error {
	f.createdName, f.draft = name, draft
	return f.err
}

func (f *stubFlows) Publish(_ context.Context, _ uuid.UUID, note string) error {
	f.publishNote, f.published = note, true
	return f.err
}

const validSpecBody = `{"id":"probe","specVersion":"v2","initialNode":"welcome",` +
	`"global":{"persona":"A helpful assistant."},` +
	`"nodes":{"welcome":{"instruction":"Greet the caller."}}}`

func flowStub() *stubFlows {
	return &stubFlows{summary: store.FlowSummary{
		ID: uuid.New(), Slug: "probe", Name: "Probe",
		HasUnpublishedChanges: true,
		CreatedAt:             time.Now(), UpdatedAt: time.Now(),
	}}
}

// Publishing without saying why is a choice, not a malformed request. An
// EventSource-shaped mistake here would be a Publish button that cannot be
// pressed without inventing a note for the operator.
func TestPublishingNeedsNoBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"no body at all", "", ""},
		{"an empty object", `{}`, ""},
		{"a note", `{"note":"  seasonal greeting  "}`, "seasonal greeting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := flowStub()
			s := &Server{flows: f}
			rec := httptest.NewRecorder()

			s.PublishFlow(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)), f.summary.ID)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if !f.published {
				t.Fatal("nothing was published")
			}
			if f.publishNote != tc.want {
				t.Errorf("note = %q, want %q", f.publishNote, tc.want)
			}
		})
	}
}

// An author fixing a spec should see the whole report, not discover it one
// save at a time — so the refusal carries every problem the loader found.
func TestARefusedSpecCarriesEveryProblem(t *testing.T) {
	f := flowStub()
	f.err = &flow.ValidationError{FlowID: "probe", Problems: []string{
		"global.persona is empty, so the model has no character to adopt",
		`initialNode "missing" is not a phase in this flow`,
	}}
	s := &Server{flows: f}
	rec := httptest.NewRecorder()

	s.CreateFlow(rec, post(`{"slug":"probe","name":"Probe","spec":`+validSpecBody+`}`))

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
	if len(problems) != 2 {
		t.Fatalf("params.problems = %v, want both problems — one at a time is a "+
			"conversation with the editor, not a report", body.Error.Params["problems"])
	}
}

// Each failure the operator can act on says something different: a slug in use
// is a name to change, a missing flow is a stale link, a storage fault is
// neither and must not be dressed up as either.
func TestFlowFailuresAreToldApart(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a slug already taken", &pgconn.PgError{Code: "23505"}, http.StatusConflict},
		{"a flow that is gone", store.ErrFlowNotFound, http.StatusNotFound},
		{"storage down", context.DeadlineExceeded, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{flows: flowStub()}
			rec := httptest.NewRecorder()
			s.writeFlowError(rec, httptest.NewRequest(http.MethodGet, "/", nil), tc.err)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// The slug is the identity automation updates a flow by. One with a space or a
// capital in it is not a thing `aicc flowadd -slug` can name back.
func TestASlugMustBeOneAutomationCanName(t *testing.T) {
	for _, tc := range []struct {
		slug string
		want int
	}{
		{"novanet_support", http.StatusCreated},
		{"support-en", http.StatusCreated},
		{"NovaNet", http.StatusUnprocessableEntity},
		{"nova net", http.StatusUnprocessableEntity},
		{"", http.StatusUnprocessableEntity},
		{"-leading", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			f := flowStub()
			s := &Server{flows: f}
			rec := httptest.NewRecorder()
			body, _ := json.Marshal(map[string]any{
				"slug": tc.slug, "name": "Probe", "spec": json.RawMessage(validSpecBody),
			})
			s.CreateFlow(rec, post(string(body)))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Creating stores a draft and stops there. A flow that answered its number the
// moment it was typed would make saving a deployment.
func TestCreatingAFlowDoesNotPublishIt(t *testing.T) {
	f := flowStub()
	s := &Server{flows: f}
	rec := httptest.NewRecorder()

	s.CreateFlow(rec, post(`{"slug":"probe","name":"Probe","spec":`+validSpecBody+`}`))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if f.published {
		t.Error("creating a flow published it")
	}
	// What was stored is a spec the loader accepts, re-encoded from the
	// decoded document rather than passed through by accident.
	if _, err := flow.Load(f.draft); err != nil {
		t.Errorf("the stored draft is not loadable: %v", err)
	}
}
