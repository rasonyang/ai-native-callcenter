// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// The agent's own workspace at the HTTP boundary: which call a wrap-up is
// filed against, and whose queues a waiting list belongs to. Both answers come
// from the platform rather than from the request, which is the property these
// tests exist to hold.

var workspaceAgentID = uuid.New()

// recordingAgents is the presence service with the two wrap-up calls
// observable, so "did the handler end after-call work" is answerable.
type recordingAgents struct {
	stubAgents
	wrapUpCall  uuid.UUID
	hasWrapUp   bool
	endedWrapUp int
}

func (a *recordingAgents) WrapUpCall(uuid.UUID) (uuid.UUID, bool) {
	return a.wrapUpCall, a.hasWrapUp
}

func (a *recordingAgents) EndWrapUp(context.Context, uuid.UUID) (agents.Presence, error) {
	a.endedWrapUp++
	return agents.Presence{State: agents.StateReady}, nil
}

// waitingCalls answers the queue question and records what it was asked.
type waitingCalls struct {
	stubCalls
	askedFor []uuid.UUID
	askedAll bool
	answer   []telephony.WaitingCall
	everyone []telephony.WaitingCall
}

func (c *waitingCalls) WaitingCalls(queueIDs []uuid.UUID) []telephony.WaitingCall {
	c.askedFor = queueIDs
	return c.answer
}

func (c *waitingCalls) AllWaitingCalls() []telephony.WaitingCall {
	c.askedAll = true
	return c.everyone
}

// staffedAgent resolves every session to one agent staffing two queues.
type staffedAgent struct{ queues []uuid.UUID }

func (staffedAgent) AgentIDForUser(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return workspaceAgentID, nil
}
func (s staffedAgent) QueuesForAgent(*http.Request, uuid.UUID) ([]uuid.UUID, error) {
	return s.queues, nil
}

func agentRequest(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(contextWithIdentity(r.Context(),
		auth.Identity{UserID: uuid.New(), Role: auth.RoleAgent}))
}

// Nothing is required any more: the record already exists with a disposition
// the platform filed, and pressing Done without touching anything is an agent
// saying the defaults are right. What must still hold is that there is a call
// to confirm against.
func TestConfirmingWithNoFinishedCallIsRefusedAndChangesNothing(t *testing.T) {
	svc := &recordingAgents{hasWrapUp: false}
	srv := New(config.Config{}, Deps{Agents: svc, AgentDir: staffedAgent{}})

	w := httptest.NewRecorder()
	srv.AgentWrapUp(w, agentRequest(http.MethodPost, "/api/v1/agent/wrap-up", `{}`))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body)
	}
	var body struct{ Error APIError }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != CodeAgentNotInWrapUp {
		t.Errorf("code = %s, want AGENT_NOT_IN_WRAP_UP", body.Error.Code)
	}
	if svc.endedWrapUp != 0 {
		t.Error("presence was changed although there was nothing to confirm")
	}
}

// A cockpit that reloads must find the work still waiting: the state is the
// server's, so refreshing the page is not a way past it.
func TestTheOpenRecordSurvivesAReload(t *testing.T) {
	svc := &recordingAgents{hasWrapUp: false}
	srv := New(config.Config{}, Deps{Agents: svc, AgentDir: staffedAgent{}})

	w := httptest.NewRecorder()
	srv.GetAgentWrapUp(w, agentRequest(http.MethodGet, "/api/v1/agent/wrap-up", ""))

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 when there is nothing to confirm: %s", w.Code, w.Body)
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 carried a body: %s", w.Body)
	}
}

// The waiting list is the agent's own queues, resolved from their staffing —
// the same rule that decides which queue events reach their event stream.
func TestTheWaitingListIsTheAgentsOwnQueues(t *testing.T) {
	staffed := []uuid.UUID{uuid.New(), uuid.New()}
	somebody_elses := uuid.New()
	calls := &waitingCalls{answer: []telephony.WaitingCall{
		{CallID: uuid.New(), QueueID: staffed[0], QueueName: "support-en", FromNumber: "13800138000"},
	}}
	srv := New(config.Config{}, Deps{
		Agents: &recordingAgents{}, AgentDir: staffedAgent{queues: staffed}, Calls: calls,
		Catalog: queueCatalogStub{queues: []catalog.Queue{
			{ID: staffed[0], Name: "support-en", DisplayName: "Support EN", SLAThresholdSec: 20},
			{ID: staffed[1], Name: "billing", DisplayName: "Billing", SLAThresholdSec: 30},
			{ID: somebody_elses, Name: "sales", DisplayName: "Sales", SLAThresholdSec: 45},
		}},
	})

	w := httptest.NewRecorder()
	srv.ListWaitingCalls(w, agentRequest(http.MethodGet, "/api/v1/calls/waiting", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if !slices.Equal(calls.askedFor, staffed) {
		t.Errorf("asked for queues %v, want the agent's staffing %v", calls.askedFor, staffed)
	}
	var body struct {
		Items []struct {
			QueueName  string `json:"queueName"`
			FromNumber string `json:"fromNumber"`
		} `json:"items"`
		Queues []struct {
			QueueID         uuid.UUID `json:"queueId"`
			Name            string    `json:"name"`
			DisplayName     string    `json:"displayName"`
			SLAThresholdSec int       `json:"slaThresholdSec"`
		} `json:"queues"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].QueueName != "support-en" ||
		body.Items[0].FromNumber != "13800138000" {
		t.Errorf("items = %+v, want the waiting caller with their queue", body.Items)
	}
	// Both staffed queues, including the one nobody is waiting in — a quiet
	// line is an answer, and the cockpit cannot tell it from no line at all
	// unless it is named. The queue this agent does not staff stays out.
	if len(body.Queues) != 2 ||
		body.Queues[0].QueueID != staffed[0] || body.Queues[1].QueueID != staffed[1] {
		t.Fatalf("queues = %+v, want exactly the two the agent staffs", body.Queues)
	}
	if body.Queues[1].DisplayName != "Billing" || body.Queues[1].SLAThresholdSec != 30 {
		t.Errorf("queues[1] = %+v, want the queue's own name and answer target", body.Queues[1])
	}
}

// A queue an agent staffs is named even when the catalogue lists others: the
// waiting line is theirs, and so is the list of lines it is drawn from.
type queueCatalogStub struct {
	stubCatalog
	queues []catalog.Queue
}

func (q queueCatalogStub) Queues(context.Context) ([]catalog.Queue, error) { return q.queues, nil }

// The agent's own day is theirs: the endpoint takes no agent, so there is
// nothing to point at a colleague, and the aggregates over somebody else's day
// stay behind the supervisor guard on /reports.
func TestTheDayIsTheCallersOwn(t *testing.T) {
	srv := New(config.Config{Env: "dev"}, Deps{
		Auth: &auth.Service{}, Agents: stubAgents{}, Calls: stubCalls{},
		Catalog: stubCatalog{}, Ledger: stubLedger(t), Contacts: stubContacts{},
		Outbound: stubOutbound{},
	})

	guard := reflect.ValueOf(requireAgentRole).Pointer()
	supervisor := reflect.ValueOf(requireSupervisorRole).Pointer()
	var guarded, supervised bool
	err := chi.Walk(srv.router(), func(method, route string, _ http.Handler,
		middlewares ...func(http.Handler) http.Handler) error {
		if method+" "+route != "GET /api/v1/reports/me" {
			return nil
		}
		for _, mw := range middlewares {
			switch reflect.ValueOf(mw).Pointer() {
			case guard:
				guarded = true
			case supervisor:
				supervised = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !guarded {
		t.Error("GET /reports/me is not behind the agent guard, yet it answers from " +
			"the session's agent identity")
	}
	if supervised {
		t.Error("GET /reports/me is behind the supervisor guard; an agent could not " +
			"read their own day")
	}
}

// The workspace routes are the agent's own, and each is guarded as such: an
// endpoint that answers "my calls" or "my queues" from the session identity is
// only safe while the session is an agent's.
func TestTheAgentWorkspaceRoutesAreAgentOnly(t *testing.T) {
	srv := New(config.Config{Env: "dev"}, Deps{
		Auth: &auth.Service{}, Agents: stubAgents{}, Calls: stubCalls{},
		Catalog: stubCatalog{}, Ledger: stubLedger(t), Contacts: stubContacts{},
		Outbound: stubOutbound{},
	})

	want := map[string]bool{
		"POST /api/v1/agent/wrap-up": false,
		"GET /api/v1/calls/waiting":  false,
		"GET /api/v1/cdrs/mine":      false,
	}
	guard := reflect.ValueOf(requireAgentRole).Pointer()

	err := chi.Walk(srv.router(), func(method, route string, _ http.Handler,
		middlewares ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		if _, watched := want[key]; !watched {
			return nil
		}
		for _, mw := range middlewares {
			if reflect.ValueOf(mw).Pointer() == guard {
				want[key] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for route, guarded := range want {
		if !guarded {
			t.Errorf("%s is not behind the agent guard — it answers from the "+
				"session's agent identity, which a non-agent session does not have", route)
		}
	}
}

// A supervisor works no line, so there is no staffing to scope the waiting
// list to — they watch every queue, which is the same split ListCalls makes.
// Before this they were refused outright for not being an agent, which left
// the one view of who is waiting available only to the people already busy.
func TestTheWaitingListIsEveryQueueForASupervisor(t *testing.T) {
	staffed := []uuid.UUID{uuid.New()}
	calls := &waitingCalls{
		answer: []telephony.WaitingCall{
			{CallID: uuid.New(), QueueID: staffed[0], QueueName: "support-en"},
		},
		everyone: []telephony.WaitingCall{
			{CallID: uuid.New(), QueueID: staffed[0], QueueName: "support-en", FromNumber: "13800138000"},
			{CallID: uuid.New(), QueueID: uuid.New(), QueueName: "support-zh", FromNumber: "18688886669"},
		},
	}
	// A directory that would refuse: a supervisor is nobody's agent.
	srv := New(config.Config{}, Deps{
		Agents: &recordingAgents{}, AgentDir: noAgentDir{}, Calls: calls,
		Catalog: queueCatalogStub{queues: []catalog.Queue{
			{ID: staffed[0], Name: "support-en", DisplayName: "Support EN"},
			{ID: uuid.New(), Name: "support-zh", DisplayName: "Support ZH"},
			{ID: uuid.New(), Name: "sales", DisplayName: "Sales"},
		}},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/calls/waiting", nil)
	r = r.WithContext(contextWithIdentity(r.Context(),
		auth.Identity{UserID: uuid.New(), Role: auth.RoleSupervisor}))
	srv.ListWaitingCalls(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if !calls.askedAll {
		t.Error("the supervisor was scoped to somebody's staffing instead of the whole floor")
	}
	if calls.askedFor != nil {
		t.Errorf("the agent-scoped view was consulted for a supervisor: %v", calls.askedFor)
	}
	var body struct {
		Items []struct {
			QueueName string `json:"queueName"`
		} `json:"items"`
		Queues []struct {
			Name string `json:"name"`
		} `json:"queues"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 2 {
		t.Errorf("items = %+v, want both queues", body.Items)
	}
	// Every queue there is, not only the ones with somebody in them: a
	// supervisor watches the floor, and an empty line is part of it.
	if len(body.Queues) != 3 {
		t.Errorf("queues = %+v, want every queue in the catalogue", body.Queues)
	}
}

// noAgentDir stands for a directory asked about somebody who is not an agent.
type noAgentDir struct{}

func (noAgentDir) AgentIDForUser(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, errors.New("not an agent")
}
func (noAgentDir) QueuesForAgent(*http.Request, uuid.UUID) ([]uuid.UUID, error) {
	return nil, errors.New("not an agent")
}
