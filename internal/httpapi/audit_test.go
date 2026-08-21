// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

type auditRow struct {
	actorID    *uuid.UUID
	action     string
	targetKind string
	targetID   string
	detail     map[string]any
}

type fakeAuditor struct {
	mu   sync.Mutex
	rows []auditRow
}

func (f *fakeAuditor) Audit(_ context.Context, actorID *uuid.UUID,
	action, targetKind, targetID string, detail map[string]any, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, auditRow{actorID, action, targetKind, targetID, detail})
	return nil
}

func (f *fakeAuditor) all() []auditRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auditRow(nil), f.rows...)
}

// trailServer builds a router shaped like the real one — auditTrail wrapping a
// route tree — with a fixed identity standing in for the session.
func trailServer(auditor *fakeAuditor, identity auth.Identity) http.Handler {
	s := &Server{auditor: auditor}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(contextWithIdentity(req.Context(), identity)))
		})
	})
	r.Use(s.auditTrail)
	r.Post("/api/v1/queues/{queueId}/agents", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	r.Post("/api/v1/auth/change-password", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Post("/api/v1/rejected", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	r.Get("/api/v1/read-only", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return r
}

func TestAuditTrail(t *testing.T) {
	userID := uuid.New()
	identity := auth.Identity{UserID: userID, Role: auth.RoleAdmin}

	t.Run("a successful mutation is recorded with actor, action and target", func(t *testing.T) {
		auditor := &fakeAuditor{}
		server := trailServer(auditor, identity)

		queueID := uuid.New()
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/queues/"+queueID.String()+"/agents", strings.NewReader(`{"agentId":"x"}`))
		server.ServeHTTP(httptest.NewRecorder(), req)

		rows := auditor.all()
		if len(rows) != 1 {
			t.Fatalf("recorded %d rows, want 1", len(rows))
		}
		row := rows[0]
		if row.actorID == nil || *row.actorID != userID {
			t.Errorf("actor = %v, want the signed-in user", row.actorID)
		}
		if row.action != "POST /api/v1/queues/{queueId}/agents" {
			t.Errorf("action = %q, want the route pattern", row.action)
		}
		if row.targetKind != "queue" || row.targetID != queueID.String() {
			t.Errorf("target = %s/%s", row.targetKind, row.targetID)
		}
		if request, _ := row.detail["request"].(string); !strings.Contains(request, "agentId") {
			t.Errorf("detail lost the request body: %v", row.detail)
		}
	})

	t.Run("credentials never reach the trail", func(t *testing.T) {
		auditor := &fakeAuditor{}
		server := trailServer(auditor, identity)

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/auth/change-password", strings.NewReader(`{"password":"secret"}`))
		server.ServeHTTP(httptest.NewRecorder(), req)

		rows := auditor.all()
		if len(rows) != 1 {
			t.Fatalf("recorded %d rows", len(rows))
		}
		if rows[0].detail != nil {
			t.Errorf("an auth request's body was recorded: %v", rows[0].detail)
		}
	})

	t.Run("a rejected request is not an action", func(t *testing.T) {
		auditor := &fakeAuditor{}
		server := trailServer(auditor, identity)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/rejected", strings.NewReader(`{}`))
		server.ServeHTTP(httptest.NewRecorder(), req)

		if rows := auditor.all(); len(rows) != 0 {
			t.Errorf("a 400 was audited: %+v", rows)
		}
	})

	t.Run("reads are never audited", func(t *testing.T) {
		auditor := &fakeAuditor{}
		server := trailServer(auditor, identity)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/read-only", nil)
		server.ServeHTTP(httptest.NewRecorder(), req)

		if rows := auditor.all(); len(rows) != 0 {
			t.Errorf("a GET was audited: %+v", rows)
		}
	})

	t.Run("the handler still reads the body the trail already read", func(t *testing.T) {
		auditor := &fakeAuditor{}
		s := &Server{auditor: auditor}
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(contextWithIdentity(req.Context(), identity)))
			})
		})
		r.Use(s.auditTrail)

		var seen string
		r.Post("/api/v1/echo", func(w http.ResponseWriter, req *http.Request) {
			body := make([]byte, 64)
			n, _ := req.Body.Read(body)
			seen = string(body[:n])
			w.WriteHeader(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/echo", strings.NewReader(`{"k":"v"}`))
		r.ServeHTTP(httptest.NewRecorder(), req)

		if seen != `{"k":"v"}` {
			t.Errorf("the handler saw %q — the trail consumed the body", seen)
		}
	})
}

// Every mutating route in the real router must live inside the audited tree.
// This test is what makes coverage structural: adding a POST route outside it
// turns this red, not a review comment.
func TestEveryMutatingRouteIsUnderTheAuditTrail(t *testing.T) {
	server := New(config.Config{Env: "dev"}, Deps{
		Auth: &auth.Service{},
		// Non-nil markers so every conditional route group registers.
		Agents:   stubAgents{},
		Calls:    stubCalls{},
		Catalog:  stubCatalog{},
		Ledger:   stubLedger(t),
		Contacts: stubContacts{},
		Outbound: stubOutbound{},
	})
	router := server.router()

	// The audit trail wraps everything behind the session; the only mutating
	// route outside it is login, which authenticates rather than acts.
	exempt := map[string]bool{
		"POST /api/v1/auth/login": true,
	}

	// Method values for the same method share one code pointer, so the trail
	// can be recognized in a route's middleware chain.
	trail := reflect.ValueOf(server.auditTrail).Pointer()

	walked := map[string]bool{} // route -> is under the trail
	err := chi.Walk(router, func(method, route string, _ http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		if !isMutating(method) {
			return nil
		}
		key := method + " " + strings.ReplaceAll(route, "/*", "")
		for _, mw := range middlewares {
			if reflect.ValueOf(mw).Pointer() == trail {
				walked[key] = true
				return nil
			}
		}
		walked[key] = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A wiring regression that skips whole route groups must not read as
	// "everything covered".
	if len(walked) < 20 {
		t.Fatalf("walk saw only %d mutating routes — a route group did not register", len(walked))
	}

	var uncovered []string
	for key, underTrail := range walked {
		switch {
		case exempt[key] && underTrail:
			t.Errorf("%s is listed as exempt but is audited — drop the stale exemption", key)
		case !exempt[key] && !underTrail:
			uncovered = append(uncovered, key)
		}
	}
	if len(uncovered) > 0 {
		t.Errorf("mutating routes outside the audited tree:\n  %s",
			strings.Join(uncovered, "\n  "))
	}
	for key := range exempt {
		if _, ok := walked[key]; !ok {
			t.Errorf("exempt route %s no longer exists", key)
		}
	}
}

// Stubs: enough presence to make every conditional route group register.
type stubAgents struct{}

func (stubAgents) Login(context.Context, uuid.UUID, string) (agents.Presence, error) {
	return agents.Presence{}, nil
}
func (stubAgents) Logout(context.Context, uuid.UUID) (agents.Presence, error) {
	return agents.Presence{}, nil
}
func (stubAgents) Ready(context.Context, uuid.UUID) (agents.Presence, error) {
	return agents.Presence{}, nil
}
func (stubAgents) NotReady(context.Context, uuid.UUID, agents.Reason) (agents.Presence, error) {
	return agents.Presence{}, nil
}
func (stubAgents) Presence(uuid.UUID) agents.Presence                   { return agents.Presence{} }
func (stubAgents) Roster(context.Context) ([]agents.RosterEntry, error) { return nil, nil }
func (stubAgents) CreateAgent(context.Context, agents.AgentConfig) (agents.AgentConfig, error) {
	return agents.AgentConfig{}, nil
}
func (stubAgents) UpdateAgent(context.Context, agents.AgentConfig) (agents.AgentConfig, error) {
	return agents.AgentConfig{}, nil
}
func (stubAgents) DeleteAgent(context.Context, uuid.UUID) error { return nil }
func (stubAgents) EndWrapUp(context.Context, uuid.UUID) (agents.Presence, error) {
	return agents.Presence{}, nil
}
func (stubAgents) WrapUpCall(uuid.UUID) (uuid.UUID, bool) { return uuid.Nil, false }

type stubCalls struct{}

func (stubCalls) Answer(context.Context, uuid.UUID, uuid.UUID) error   { return nil }
func (stubCalls) Hold(context.Context, uuid.UUID, uuid.UUID) error     { return nil }
func (stubCalls) Retrieve(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (stubCalls) Mute(context.Context, uuid.UUID, uuid.UUID) error     { return nil }
func (stubCalls) Unmute(context.Context, uuid.UUID, uuid.UUID) error   { return nil }
func (stubCalls) Hangup(context.Context, uuid.UUID, uuid.UUID) error   { return nil }
func (stubCalls) SendDTMF(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (stubCalls) Transfer(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (stubCalls) CallsForAgent(uuid.UUID) []telephony.Snapshot { return nil }
func (stubCalls) AllCalls() []telephony.Snapshot               { return nil }
func (stubCalls) WaitingCalls([]uuid.UUID) []telephony.WaitingCall {
	return nil
}
func (stubCalls) AllWaitingCalls() []telephony.WaitingCall { return nil }

type stubCatalog struct{}

func (stubCatalog) Extensions(context.Context) ([]catalog.Extension, error) { return nil, nil }
func (stubCatalog) CreateExtension(context.Context, catalog.Extension) (catalog.Extension, error) {
	return catalog.Extension{}, nil
}
func (stubCatalog) UpdateExtension(context.Context, catalog.Extension) (catalog.Extension, error) {
	return catalog.Extension{}, nil
}
func (stubCatalog) DeleteExtension(context.Context, uuid.UUID) error { return nil }
func (stubCatalog) Queues(context.Context) ([]catalog.Queue, error)  { return nil, nil }
func (stubCatalog) CreateQueue(context.Context, catalog.Queue) (catalog.Queue, error) {
	return catalog.Queue{}, nil
}
func (stubCatalog) UpdateQueue(context.Context, catalog.Queue) (catalog.Queue, error) {
	return catalog.Queue{}, nil
}
func (stubCatalog) DeleteQueue(context.Context, uuid.UUID) error { return nil }
func (stubCatalog) QueueAgents(context.Context, uuid.UUID) ([]catalog.QueueAgent, error) {
	return nil, nil
}
func (stubCatalog) StaffQueue(context.Context, uuid.UUID, uuid.UUID, int, int) error { return nil }
func (stubCatalog) UnstaffQueue(context.Context, uuid.UUID, uuid.UUID) error         { return nil }

func stubLedger(t *testing.T) *store.LedgerStore {
	t.Helper()
	// The walk only needs the route groups to register; nothing is called.
	return &store.LedgerStore{}
}

func (stubCatalog) DIDs(context.Context) ([]catalog.DID, error) { return nil, nil }
func (stubCatalog) CreateDID(context.Context, catalog.DID) (catalog.DID, error) {
	return catalog.DID{}, nil
}
func (stubCatalog) UpdateDID(context.Context, catalog.DID) (catalog.DID, error) {
	return catalog.DID{}, nil
}
func (stubCatalog) DeleteDID(context.Context, uuid.UUID) error { return nil }

type stubContacts struct{}

func (stubContacts) List(context.Context, store.ContactFilter) ([]store.Contact, int64, error) {
	return nil, 0, nil
}
func (stubContacts) Create(context.Context, store.ContactWrite, *uuid.UUID) (store.Contact, error) {
	return store.Contact{}, nil
}
func (stubContacts) Update(context.Context, uuid.UUID, store.ContactWrite, *uuid.UUID) (store.Contact, error) {
	return store.Contact{}, nil
}
func (stubContacts) Delete(context.Context, uuid.UUID) error { return nil }

type stubOutbound struct{}

func (stubOutbound) Dial(context.Context, string, string) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (stubOutbound) DialAI(context.Context, outbound.AIDialRequest) (uuid.UUID, error) {
	return uuid.Nil, nil
}
