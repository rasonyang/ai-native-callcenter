// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
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
	answer   []telephony.WaitingCall
}

func (c *waitingCalls) WaitingCalls(queueIDs []uuid.UUID) []telephony.WaitingCall {
	c.askedFor = queueIDs
	return c.answer
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

// An empty wrap-up is a legitimate completion: the agent had nothing to file
// and simply wants the next call.
func TestCompletingAnEmptyWrapUpJustGoesReady(t *testing.T) {
	svc := &recordingAgents{}
	srv := New(config.Config{}, Deps{Agents: svc, AgentDir: staffedAgent{}})

	w := httptest.NewRecorder()
	srv.AgentWrapUp(w, agentRequest(http.MethodPost, "/api/v1/agent/wrap-up", `{}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if svc.endedWrapUp != 1 {
		t.Errorf("after-call work ended %d times, want once", svc.endedWrapUp)
	}
}

// Filing needs a call, and the platform is the only thing that may name it.
// Without a call there is nothing to write the disposition onto, and the
// request has to say so rather than quietly dropping what the agent typed.
func TestFilingWithNoFinishedCallIsRefusedAndChangesNothing(t *testing.T) {
	svc := &recordingAgents{hasWrapUp: false}
	srv := New(config.Config{}, Deps{Agents: svc, AgentDir: staffedAgent{}})

	w := httptest.NewRecorder()
	srv.AgentWrapUp(w, agentRequest(http.MethodPost, "/api/v1/agent/wrap-up",
		`{"dispositionCode":"ISSUE_FIXED","note":"replaced the router"}`))

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
		t.Error("presence was changed although the filing was refused; the agent " +
			"would be sent back to ready with their note lost")
	}
}

// The waiting list is the agent's own queues, resolved from their staffing —
// the same rule that decides which queue events reach their event stream.
func TestTheWaitingListIsTheAgentsOwnQueues(t *testing.T) {
	staffed := []uuid.UUID{uuid.New(), uuid.New()}
	calls := &waitingCalls{answer: []telephony.WaitingCall{
		{CallID: uuid.New(), QueueID: staffed[0], QueueName: "support-en", FromNumber: "13800138000"},
	}}
	srv := New(config.Config{}, Deps{
		Agents: &recordingAgents{}, AgentDir: staffedAgent{queues: staffed}, Calls: calls,
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
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].QueueName != "support-en" ||
		body.Items[0].FromNumber != "13800138000" {
		t.Errorf("items = %+v, want the waiting caller with their queue", body.Items)
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
