// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// monitoringCalls records the one eavesdrop asked for.
type monitoringCalls struct {
	stubCalls
	callID, agentID uuid.UUID
	extension, mode string
	err             error
}

func (c *monitoringCalls) Monitor(_ context.Context, callID, agentID uuid.UUID, extension, mode string) error {
	c.callID, c.agentID, c.extension, c.mode = callID, agentID, extension, mode
	return c.err
}

// deskedSupervisor supervises without staffing a queue: they are signed in
// nowhere, and the only phone that is theirs is the one bound in
// configuration.
type deskedSupervisor struct{ dialerPresence }

func (deskedSupervisor) Presence(uuid.UUID) agents.Presence { return agents.Presence{} }

// oneAgentDirectory is an account with a fixed agent identity, so a test can
// ask what happens when a supervisor names themselves.
type oneAgentDirectory struct{ agentID uuid.UUID }

func (d oneAgentDirectory) AgentIDForUser(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return d.agentID, nil
}

func (oneAgentDirectory) QueuesForAgent(*http.Request, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

func monitorAs(t *testing.T, srv *Server, callID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String()+"/monitor", strings.NewReader(body))
	// The agent identity the middleware would have resolved for this account:
	// a supervisor listens in from the phone bound to their own seat, so the
	// directory the server was built with is what supplies it.
	agentID := []uuid.UUID{}
	if srv.agentDir != nil {
		if resolved, err := srv.agentDir.AgentIDForUser(r, uuid.New()); err == nil {
			agentID = append(agentID, resolved)
		}
	}
	r = r.WithContext(contextWithIdentity(r.Context(), auth.Identity{
		UserID: uuid.New(), Role: auth.RoleSupervisor,
	}, agentID...))
	w := httptest.NewRecorder()
	srv.MonitorCall(w, r, callID)
	return w
}

// The phone is the supervisor's own and is never asked for: a supervisor who
// staffs no queue is reached at the extension bound to their account.
func TestASupervisorListensFromTheirBoundPhone(t *testing.T) {
	calls := &monitoringCalls{}
	srv := &Server{calls: calls, agents: deskedSupervisor{}, agentDir: dialerDirectory{}}
	callID, agentID := uuid.New(), uuid.New()

	w := monitorAs(t, srv, callID, `{"mode":"WHISPER","agentId":"`+agentID.String()+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if calls.callID != callID || calls.agentID != agentID || calls.mode != "WHISPER" {
		t.Errorf("monitored %+v, want the call, agent and mode the request named", calls)
	}
	if calls.extension != "1009" {
		t.Errorf("listened from %q, want the phone bound to the account", calls.extension)
	}
}

// A supervisor who has taken a seat on the floor is at that seat, not at the
// desk configuration remembers.
func TestAStaffedSupervisorListensFromTheSeatTheySignedInAt(t *testing.T) {
	calls := &monitoringCalls{}
	srv := &Server{calls: calls, agents: dialerPresence{}, agentDir: dialerDirectory{}}

	w := monitorAs(t, srv, uuid.New(), `{"mode":"LISTEN","agentId":"`+uuid.New().String()+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if calls.extension != "1008" {
		t.Errorf("listened from %q, want the phone presence says they are at", calls.extension)
	}
}

// An account with no agent identity has no phone of its own, and monitoring
// from somebody else's handset is not on offer: it would put a live
// conversation into a stranger's phone.
func TestAnAccountWithNoPhoneCannotListen(t *testing.T) {
	calls := &monitoringCalls{}
	srv := &Server{calls: calls, agents: dialerPresence{}, agentDir: noAgents{}}

	w := monitorAs(t, srv, uuid.New(), `{"mode":"LISTEN","agentId":"`+uuid.New().String()+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("http = %d: %s", w.Code, w.Body)
	}
	if calls.mode != "" {
		t.Error("the switch was asked to raise a phone nobody owns")
	}
}

// The refusals keep their meaning: an agent not on the call is a conflict, a
// mode the switch does not know never reaches it, and listening to yourself is
// a feedback loop rather than supervision.
func TestMonitorRefusals(t *testing.T) {
	calls := &monitoringCalls{err: telephony.ErrNoAgentLeg}
	self := uuid.New()
	srv := &Server{calls: calls, agents: deskedSupervisor{}, agentDir: oneAgentDirectory{agentID: self}}
	other := uuid.New().String()

	w := monitorAs(t, srv, uuid.New(), `{"mode":"LISTEN","agentId":"`+other+`"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("agent not on the call: http = %d, want 409", w.Code)
	}
	calls.err = telephony.ErrCallNotFound
	w = monitorAs(t, srv, uuid.New(), `{"mode":"LISTEN","agentId":"`+other+`"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown call: http = %d, want 404", w.Code)
	}
	calls.err = errors.New("boom")
	w = monitorAs(t, srv, uuid.New(), `{"mode":"SHOUT","agentId":"`+other+`"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown mode: http = %d, want 422", w.Code)
	}
	if calls.mode == "SHOUT" {
		t.Error("an unknown mode reached the switch")
	}
	w = monitorAs(t, srv, uuid.New(), `{"mode":"LISTEN","agentId":"`+self.String()+`"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("monitoring yourself: http = %d, want 409", w.Code)
	}
}
