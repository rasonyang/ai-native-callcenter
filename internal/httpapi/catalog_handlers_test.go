// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// recordingCatalog keeps whatever the handler decided to hand the service.
type recordingCatalog struct {
	stubCatalog
	extension catalog.Extension
	queue     catalog.Queue
	did       catalog.DID
}

func (c *recordingCatalog) CreateExtension(_ context.Context, e catalog.Extension) (catalog.Extension, error) {
	c.extension = e
	return e, nil
}

func (c *recordingCatalog) UpdateExtension(_ context.Context, e catalog.Extension) (catalog.Extension, error) {
	c.extension = e
	return e, nil
}

func (c *recordingCatalog) CreateQueue(_ context.Context, q catalog.Queue) (catalog.Queue, error) {
	c.queue = q
	return q, nil
}

func (c *recordingCatalog) CreateDID(_ context.Context, d catalog.DID) (catalog.DID, error) {
	c.did = d
	return d, nil
}

func (c *recordingCatalog) UpdateDID(_ context.Context, d catalog.DID) (catalog.DID, error) {
	c.did = d
	return d, nil
}

func post(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
}

// A boolean the operator left out must arrive as the default the contract
// declares, not as Go's zero value. Nothing downstream can recover the
// difference: the column default never fires, because the insert names the
// column. An extension created this way was disabled, so the switch's
// directory view — which filters on is_enabled — never showed it, the phone
// never registered, and the API reported nothing but 201.
func TestAnOmittedBooleanTakesTheDeclaredDefault(t *testing.T) {
	t.Run("an omitted isEnabled creates an enabled extension", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateExtension(httptest.NewRecorder(), post(`{"number":"1099","password":"secret1"}`))

		if !c.extension.IsEnabled {
			t.Error("the extension was created disabled; the phone would never register")
		}
	})

	t.Run("an explicit false still disables it", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateExtension(httptest.NewRecorder(),
			post(`{"number":"1099","password":"secret1","isEnabled":false}`))

		if c.extension.IsEnabled {
			t.Error("isEnabled:false was ignored; the default overrode the operator")
		}
	})

	t.Run("update reads the same way as create", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}
		id := uuid.New()

		s.UpdateExtension(httptest.NewRecorder(), post(`{"number":"1099"}`), id)
		if !c.extension.IsEnabled {
			t.Error("an update that omitted isEnabled disabled the extension")
		}

		s.UpdateExtension(httptest.NewRecorder(), post(`{"number":"1099","isEnabled":false}`), id)
		if c.extension.IsEnabled {
			t.Error("an update could not disable an extension")
		}
	})

	t.Run("a queue keeps every boolean default it is given", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateQueue(httptest.NewRecorder(), post(`{"name":"support","extNumber":"9100"}`))

		if !c.queue.IsEnabled {
			t.Error("the queue was created disabled")
		}
		if !c.queue.IsRecordingEnabled {
			t.Error("the queue was created without recording; calls would go unrecorded")
		}
		// Not every default is true: this one's column default is false, and
		// seeding it true would be the same bug pointing the other way.
		if c.queue.IsAbandonedResumeAllowed {
			t.Error("isAbandonedResumeAllowed defaulted true, want false")
		}
	})

	t.Run("a number is reachable and recorded unless told otherwise", func(t *testing.T) {
		c := &recordingCatalog{}
		s := &Server{catalog: c}

		s.CreateDID(httptest.NewRecorder(), post(`{"number":"95009"}`))
		if !c.did.IsEnabled || !c.did.IsRecordingEnabled {
			t.Errorf("the number was created disabled or unrecorded: %+v", c.did)
		}

		s.CreateDID(httptest.NewRecorder(),
			post(`{"number":"95009","isEnabled":false,"isRecordingEnabled":false}`))
		if c.did.IsEnabled || c.did.IsRecordingEnabled {
			t.Errorf("an explicit false was overridden: %+v", c.did)
		}
	})
}

// deletingCatalog fails every delete the way PostgreSQL does when the guard
// holds: the referenced row cannot go while somebody points at it.
type deletingCatalog struct {
	stubCatalog
	err error
}

func (c *deletingCatalog) DeleteExtension(context.Context, uuid.UUID) error { return c.err }
func (c *deletingCatalog) DeleteQueue(context.Context, uuid.UUID) error     { return c.err }
func (c *deletingCatalog) DeleteDID(context.Context, uuid.UUID) error       { return c.err }
func (c *deletingCatalog) UnstaffQueue(context.Context, uuid.UUID, uuid.UUID) error {
	return c.err
}

// The database refuses; the operator has to be told what to do about it. A
// refusal reported as "storage down" — the default for an unrecognised
// driver error — teaches them to retry, and retrying will never work.
func TestDeletingAnExtensionAnAgentWorksAtIsRefusedAsAConflict(t *testing.T) {
	// 23001 verbatim from a live delete, because the first version of this
	// test guessed 23503 and passed while the server returned 503: an
	// explicit ON DELETE RESTRICT raises restrict_violation, not
	// foreign_key_violation. 23503 is here too — NO ACTION and the insert
	// side use it — but it is the one that was never the problem.
	for _, code := range []string{"23001", "23503"} {
		c := &deletingCatalog{err: &pgconn.PgError{
			Code:           code,
			ConstraintName: "fk_agents_extensions",
			Message: `update or delete on table "extensions" violates RESTRICT ` +
				`setting of foreign key constraint "fk_agents_extensions" on table "agents"`,
		}}
		s := &Server{catalog: c}
		w := httptest.NewRecorder()

		s.DeleteExtension(w, httptest.NewRequest(http.MethodDelete, "/", nil), uuid.New())

		if w.Code != http.StatusConflict {
			t.Errorf("SQLSTATE %s: http = %d, want 409", code, w.Code)
		}
		checkBindingConflict(t, code, w)
	}
}

func checkBindingConflict(t *testing.T, code string, w *httptest.ResponseRecorder) {
	t.Helper()
	{
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("SQLSTATE %s: body: %v (%s)", code, err, w.Body.String())
		}
		if env.Error.Code != string(CodeExtensionAssignedToAgent) {
			t.Errorf("SQLSTATE %s: code = %q, want EXTENSION_ASSIGNED_TO_AGENT", code, env.Error.Code)
		}
		if !strings.Contains(env.Error.Message, "unbind") {
			t.Errorf("SQLSTATE %s: message does not say what to do about it: %q", code, env.Error.Message)
		}
	}
}

// A delete that fails for any other reason is not this conflict: reporting it
// as one would send the operator looking for a binding that is not there.
func TestAnUnrelatedDeleteFailureIsNotTheBindingConflict(t *testing.T) {
	c := &deletingCatalog{err: &pgconn.PgError{Code: "23001", ConstraintName: "fk_something_else"}}
	s := &Server{catalog: c}
	w := httptest.NewRecorder()

	s.DeleteExtension(w, httptest.NewRequest(http.MethodDelete, "/", nil), uuid.New())

	if w.Code == http.StatusConflict {
		t.Errorf("an unrelated constraint was reported as the agent binding: %s", w.Body.String())
	}
}

// Deleting something that was never there answered 204, and the 404 the
// contract declares for all five catalogue deletes was unreachable code
// (C34). An operator who mistypes an id is told the extension is gone.
func TestDeletingSomethingThatIsNotThereIsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Server, http.ResponseWriter, *http.Request)
	}{
		{"extension", func(s *Server, w http.ResponseWriter, r *http.Request) {
			s.DeleteExtension(w, r, uuid.New())
		}},
		{"queue", func(s *Server, w http.ResponseWriter, r *http.Request) {
			s.DeleteQueue(w, r, uuid.New())
		}},
		{"did", func(s *Server, w http.ResponseWriter, r *http.Request) {
			s.DeleteDID(w, r, uuid.New())
		}},
		{"queue staffing", func(s *Server, w http.ResponseWriter, r *http.Request) {
			s.UnstaffQueue(w, r, uuid.New(), uuid.New())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{catalog: &deletingCatalog{err: catalog.ErrNotFound}}
			w := httptest.NewRecorder()

			tc.call(s, w, httptest.NewRequest(http.MethodDelete, "/", nil))

			if w.Code != http.StatusNotFound {
				t.Errorf("http = %d, want 404 — nothing was removed, so nothing was there (%s)",
					w.Code, w.Body.String())
			}
		})
	}
}
