// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/outbound"
)

// erringOutbound answers every placement with one error, and the callId the
// service would hand back with it.
type erringOutbound struct {
	callID uuid.UUID
	err    error
}

func (o erringOutbound) Dial(context.Context, outbound.AgentDialRequest) (uuid.UUID, error) {
	return o.callID, o.err
}

func (o erringOutbound) DialAI(context.Context, outbound.AIDialRequest) (uuid.UUID, error) {
	return o.callID, o.err
}

func placeWith(t *testing.T, svc OutboundService, ac AuthContext, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{outbound: svc, agents: dialerPresence{}, agentDir: dialerDirectory{}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	r = r.WithContext(contextWithAuth(r.Context(), ac))
	w := httptest.NewRecorder()
	s.CreateCall(w, r)
	return w
}

func supervisorSubject() AuthContext {
	return AuthContext{
		Kind: SubjectUser, SubjectID: uuid.Must(uuid.NewV7()), SubjectName: "sam",
		scopes: grantedScopes(auth.RoleSupervisor),
	}
}

// A placement the switch did not confirm in time answers 202 with the callId,
// for both kinds: the customer (or the agent's phone) may still ring, so it is
// never reported as SWITCH_DOWN, which invites a second call under a new id
// (#221). The body is the 201 body: no isDuplicate.
func TestAnUnconfirmedPlacementAnswers202WithTheCallID(t *testing.T) {
	t.Parallel()
	callID := uuid.Must(uuid.NewV7())
	unconfirmed := fmt.Errorf("%w: %w", outbound.ErrPlacementUnconfirmed,
		fmt.Errorf("%w: bgapi originate …", esl.ErrTimeout))
	svc := erringOutbound{callID: callID, err: unconfirmed}

	for _, c := range []struct {
		name string
		ac   AuthContext
		body string
	}{
		{"an AI call", supervisorSubject(), `{"kind":"AI_OUTBOUND","to":"18688886660","did":"95099","callId":"` + callID.String() + `"}`},
		{"a click-to-dial", agentSubject(), `{"kind":"AGENT_OUTBOUND","to":"18688886660","callId":"` + callID.String() + `"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := placeWith(t, svc, c.ac, c.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("http = %d, want 202: %s", w.Code, w.Body)
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["callId"] != callID.String() {
				t.Errorf("callId = %v, want %s", got["callId"], callID)
			}
			if _, has := got["isDuplicate"]; has || len(got) != 1 {
				t.Errorf("body = %s, want the 201 body: the callId alone", w.Body)
			}
		})
	}
}

// The retry with the same callId while the placement is unconfirmed is the
// service's ErrAlreadyPlaced, and answers 200 isDuplicate.
func TestARetryOfAnUnconfirmedPlacementIsADuplicate(t *testing.T) {
	t.Parallel()
	callID := uuid.Must(uuid.NewV7())
	w := placeWith(t, erringOutbound{callID: callID, err: outbound.ErrAlreadyPlaced}, supervisorSubject(),
		`{"kind":"AI_OUTBOUND","to":"18688886660","did":"95099","callId":"`+callID.String()+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("http = %d, want 200: %s", w.Code, w.Body)
	}
	var got struct {
		CallID      uuid.UUID `json:"callId"`
		IsDuplicate bool      `json:"isDuplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CallID != callID || !got.IsDuplicate {
		t.Errorf("body = %s", w.Body)
	}
}

// A link that is down is still SWITCH_DOWN, as 503 like every call operation
// answers it; an error nobody classified stays 502. Neither is 202: those
// placements reached no switch that could run them later.
func TestASwitchThatIsDownIsStillSwitchDown(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		err      error
		wantHTTP int
	}{
		{"the link is down", esl.ErrDown, http.StatusServiceUnavailable},
		{"wrapped", fmt.Errorf("originate: %w", esl.ErrDown), http.StatusServiceUnavailable},
		{"anything else", errors.New("write command: broken pipe"), http.StatusBadGateway},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, body := range []string{
				`{"kind":"AI_OUTBOUND","to":"18688886660","did":"95099"}`,
				`{"kind":"AGENT_OUTBOUND","to":"18688886660","extensionNumber":"1009"}`,
			} {
				w := placeWith(t, erringOutbound{err: c.err}, supervisorSubject(), body)
				if w.Code != c.wantHTTP {
					t.Fatalf("%s: http = %d, want %d: %s", body, w.Code, c.wantHTTP, w.Body)
				}
				if code := errorCodeOf(t, w); code != string(CodeSwitchDown) {
					t.Errorf("%s: code = %q, want SWITCH_DOWN", body, code)
				}
			}
		})
	}
}
