// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// recordingSwitch is a switch that accepts every command and remembers it.
type recordingSwitch struct{ sent []string }

func (s *recordingSwitch) API(cmd string) (string, error) {
	s.sent = append(s.sent, cmd)
	return "+OK", nil
}
func (s *recordingSwitch) BgAPI(cmd string) (string, error) {
	s.sent = append(s.sent, cmd)
	return "job-1", nil
}
func (s *recordingSwitch) IsUp() bool { return true }

type unstaffedFloor struct{}

func (unstaffedFloor) AgentAtExtension(string) (uuid.UUID, bool)                           { return uuid.Nil, false }
func (unstaffedFloor) AgentByCallcenterName(string) (uuid.UUID, bool)                      { return uuid.Nil, false }
func (unstaffedFloor) BenchForNoAnswer(context.Context, uuid.UUID)                         {}
func (unstaffedFloor) SetOnCall(context.Context, uuid.UUID, bool, uuid.UUID)               {}
func (unstaffedFloor) BeginAfterCallWork(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) {}

type silentHub struct{}

func (silentHub) Publish(_ context.Context, ev events.Event, _ events.Scope) events.Event {
	return ev
}

func transferAs(t *testing.T, srv *Server, callID, agentID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String()+"/transfer",
		strings.NewReader(`{"destination":"2001"}`))
	r = r.WithContext(contextWithAuth(r.Context(), AuthContext{
		Kind: SubjectUser, SubjectID: uuid.Must(uuid.NewV7()), AgentID: agentID,
		scopes: grantedScopes(auth.RoleAgent),
	}))
	w := httptest.NewRecorder()
	srv.TransferCall(w, r, callID)
	return w
}

// wei is on a call with a caller; amy is not. amy, holding wei's call id, is
// told they are not on it and the caller stays where they are; wei's transfer
// then moves the caller (#239). Run against the real coordinator, because the
// check lives there and a stub would only prove the error mapping.
func TestOnlyAnAgentOnTheCallCanTransferItOverHTTP(t *testing.T) {
	t.Parallel()
	sw := &recordingSwitch{}
	registry := telephony.NewRegistry(silentHub{})
	coordinator := telephony.NewCoordinator(registry,
		telephony.NewAdapter(sw, "aicc.test"), unstaffedFloor{}, silentHub{})
	srv := &Server{calls: coordinator}

	callID, wei, amy := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	createdAt := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	if _, err := registry.CreateCall(t.Context(), callID, events.CallTypeInbound, "en", true, createdAt); err != nil {
		t.Fatal(err)
	}
	if err := registry.Do(callID, func(call *telephony.Call) {
		caller := call.AddParty("caller-chan", "13800138000", createdAt)
		caller.State = telephony.PartyTalking
		agent := call.AddParty("wei-chan", "1008", createdAt)
		agent.State = telephony.PartyTalking
		agent.AgentID = &wei
	}); err != nil {
		t.Fatal(err)
	}

	w := transferAs(t, srv, callID, amy)
	if w.Code != http.StatusForbidden {
		t.Fatalf("transfer by an agent not on the call: http = %d, want 403: %s", w.Code, w.Body)
	}
	if got := errorCodeOf(t, w); got != string(CodeNotCallParty) {
		t.Errorf("code = %q, want %q", got, CodeNotCallParty)
	}
	if len(sw.sent) != 0 {
		t.Fatalf("the switch was sent %q for a refused transfer", sw.sent)
	}

	w = transferAs(t, srv, callID, wei)
	if w.Code != http.StatusAccepted {
		t.Fatalf("transfer by the agent on the call: http = %d, want 202: %s", w.Code, w.Body)
	}
	if want := "uuid_transfer caller-chan 2001 XML aicc"; len(sw.sent) != 1 || sw.sent[0] != want {
		t.Errorf("the switch was sent %q, want [%q]", sw.sent, want)
	}
}

// controlAs asks for one call operation as an agent on the call.
func controlAs(t *testing.T, srv *Server, path string, serve func(*Server, http.ResponseWriter, *http.Request, uuid.UUID),
	callID, agentID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls/"+callID.String()+"/"+path,
		strings.NewReader(body))
	r = r.WithContext(contextWithAuth(r.Context(), AuthContext{
		Kind: SubjectUser, SubjectID: uuid.Must(uuid.NewV7()), AgentID: agentID,
		scopes: grantedScopes(auth.RoleAgent),
	}))
	w := httptest.NewRecorder()
	serve(srv, w, r, callID)
	return w
}

// One extension calling another is two people on a line: holding, retrieving
// and transferring it are refused with OPERATION_NOT_ALLOWED_FOR_CALL_TYPE,
// before the switch is touched, and the 409 that carries it is one each
// operation declares (#269). The same requests on an INBOUND call are carried
// out. Run against the real coordinator, which is where the call type is read.
func TestAnInternalCallRefusesHoldRetrieveAndTransferAsTheContractSays(t *testing.T) {
	t.Parallel()
	declared := contractResponses(t)
	operations := []struct {
		operationID, path, body string
		serve                   func(*Server, http.ResponseWriter, *http.Request, uuid.UUID)
	}{
		{"holdCall", "hold", "", (*Server).HoldCall},
		{"retrieveCall", "retrieve", "", (*Server).RetrieveCall},
		{"transferCall", "transfer", `{"destination":"2001"}`, (*Server).TransferCall},
	}
	createdAt := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	t.Run("INTERNAL", func(t *testing.T) {
		t.Parallel()
		sw := &recordingSwitch{}
		registry := telephony.NewRegistry(silentHub{})
		srv := &Server{calls: telephony.NewCoordinator(registry,
			telephony.NewAdapter(sw, "aicc.test"), unstaffedFloor{}, silentHub{})}

		// wei at 1008 called amy at 1009, and both are talking.
		callID, wei, amy := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		if _, err := registry.CreateCall(t.Context(), callID, events.CallTypeInternal, "en", false, createdAt); err != nil {
			t.Fatal(err)
		}
		if err := registry.Do(callID, func(call *telephony.Call) {
			caller := call.AddParty("wei-chan", "1008", createdAt)
			caller.State = telephony.PartyTalking
			caller.AgentID = &wei
			callee := call.AddParty("amy-chan", "1009", createdAt)
			callee.State = telephony.PartyTalking
			callee.AgentID = &amy
		}); err != nil {
			t.Fatal(err)
		}

		for _, op := range operations {
			for _, agentID := range []uuid.UUID{wei, amy} {
				w := controlAs(t, srv, op.path, op.serve, callID, agentID, op.body)
				if w.Code != http.StatusConflict {
					t.Errorf("%s on an internal call: http = %d, want 409: %s", op.operationID, w.Code, w.Body)
					continue
				}
				if got := errorCodeOf(t, w); got != string(CodeOperationNotAllowedForCallType) {
					t.Errorf("%s on an internal call: code = %q, want %q",
						op.operationID, got, CodeOperationNotAllowedForCallType)
				}
				if !slices.Contains(declared[op.operationID], "409") {
					t.Errorf("%s answers 409 and the contract declares only %v",
						op.operationID, declared[op.operationID])
				}
			}
		}
		if len(sw.sent) != 0 {
			t.Errorf("the switch was sent %q for refused operations", sw.sent)
		}
	})

	t.Run("INBOUND", func(t *testing.T) {
		t.Parallel()
		sw := &recordingSwitch{}
		registry := telephony.NewRegistry(silentHub{})
		srv := &Server{calls: telephony.NewCoordinator(registry,
			telephony.NewAdapter(sw, "aicc.test"), unstaffedFloor{}, silentHub{})}

		callID, wei := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		if _, err := registry.CreateCall(t.Context(), callID, events.CallTypeInbound, "en", true, createdAt); err != nil {
			t.Fatal(err)
		}
		if err := registry.Do(callID, func(call *telephony.Call) {
			caller := call.AddParty("caller-chan", "13800138000", createdAt)
			caller.State = telephony.PartyTalking
			agent := call.AddParty("wei-chan", "1008", createdAt)
			agent.State = telephony.PartyTalking
			agent.AgentID = &wei
		}); err != nil {
			t.Fatal(err)
		}

		for _, op := range operations {
			w := controlAs(t, srv, op.path, op.serve, callID, wei, op.body)
			if w.Code != http.StatusAccepted {
				t.Errorf("%s on an inbound call: http = %d, want 202: %s", op.operationID, w.Code, w.Body)
			}
		}
		if len(sw.sent) != len(operations) {
			t.Errorf("the switch was sent %q, want one command per operation", sw.sent)
		}
	})
}
