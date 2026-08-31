// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/api"
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
		auth.Identity{UserID: uuid.New(), Role: auth.RoleAgent}, uuid.New()))
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

// The agent's own day is theirs, and the contract is where that is written
// down now: /reports/me asks for the own-scoped capability, while the
// aggregates over everybody's day ask for the floor-wide ones.
//
// This used to walk the routing table looking for a role guard. There are no
// role guards any more — an operation's authorization is a property of the
// operation, not of where somebody mounted it — so the assertion moved to the
// place that actually decides.
func TestTheDayIsTheCallersOwn(t *testing.T) {
	mine, ok := api.SecurityForRoute(http.MethodGet, "/reports/me")
	if !ok {
		t.Fatal("GET /reports/me is not in the contract")
	}
	if !slices.Contains(mine.SessionScopes, api.ScopeHistoryReadOwn) {
		t.Errorf("scopes = %v, want history:read:own — it answers from the "+
			"caller's own agent identity", mine.SessionScopes)
	}
	for _, wider := range []string{api.ScopeHistoryReadAll, api.ScopeReportsRead} {
		if slices.Contains(mine.SessionScopes, wider) {
			t.Errorf("scopes = %v, want no %s — an agent could not read their own day",
				mine.SessionScopes, wider)
		}
	}
}

// The workspace operations answer "my calls", "my queues", "my day" from the
// subject's own agent identity, so each asks for an own-scoped capability and
// none of them asks for a floor-wide one. An operation that answered from the
// caller's agent identity while demanding the floor-wide scope would be a
// screen only supervisors could open to see nothing.
func TestTheAgentWorkspaceOperationsAskForTheOwnScopedCapability(t *testing.T) {
	want := map[string]string{
		"POST /agent/wrap-up": api.ScopeAgentAct,
		"GET /calls/waiting":  api.ScopeCallsReadOwn,
		"GET /cdrs/mine":      api.ScopeHistoryReadOwn,
	}
	for route, scope := range want {
		method, path, _ := strings.Cut(route, " ")
		sec, ok := api.SecurityForRoute(method, path)
		if !ok {
			t.Errorf("%s is not in the contract", route)
			continue
		}
		if !slices.Contains(sec.SessionScopes, scope) {
			t.Errorf("%s asks for %v, want %s", route, sec.SessionScopes, scope)
		}
	}
}

// Every route this server mounts is an operation the contract declares.
//
// enforceContract fails closed on a route it cannot find, which is the right
// behaviour and a terrible way to discover the problem — in production, as a
// 500, on the one endpoint somebody added without touching the contract. This
// is where it is discovered instead. The four exclusions are the ops listener
// and the SPA, which are not the API and say so in the contract's own
// description; every one of them is served from a different handler entirely.
func TestEveryRouteIsInTheContract(t *testing.T) {
	srv := New(config.Config{Env: "dev"}, Deps{
		Auth: &auth.Service{}, Agents: stubAgents{}, Calls: stubCalls{},
		Catalog: stubCatalog{}, Ledger: stubLedger(t), Contacts: stubContacts{},
		Outbound: stubOutbound{}, Keys: stubKeys{},
	})

	var undeclared []string
	err := chi.Walk(srv.router(), func(method, route string, _ http.Handler,
		_ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, apiPrefix) {
			return nil
		}
		path := strings.TrimSuffix(strings.TrimPrefix(route, apiPrefix), "/")
		if path == "" {
			return nil
		}
		if _, ok := api.SecurityForRoute(method, path); !ok {
			undeclared = append(undeclared, method+" "+path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(undeclared) > 0 {
		slices.Sort(undeclared)
		t.Errorf("mounted and not declared in the contract: %s\n"+
			"Authorization is read from the contract, so such a route has none: "+
			"it fails closed with a 500 and nobody learns why until it is live.",
			strings.Join(undeclared, ", "))
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

func (staffedAgent) UserIDForAgent(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}

func (noAgentDir) UserIDForAgent(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, errors.New("not an agent")
}
