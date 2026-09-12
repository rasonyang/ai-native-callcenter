// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
)

// A phone is a precondition for READY, and the refusal has to be legible to a
// client that is not a browser: a named code it can branch on, at a status
// that says "the world is not in a state where this can be done" rather than
// "your request was wrong". Nothing about the request was wrong.
type phonelessAgents struct {
	stubAgents
	presence agents.Presence
}

func (a phonelessAgents) Ready(context.Context, uuid.UUID) (agents.Presence, error) {
	return a.presence, agents.ErrDeviceNotRegistered
}

func (a phonelessAgents) Presence(uuid.UUID) agents.Presence { return a.presence }

func TestReadyWithoutARegisteredPhoneIsRefusedByName(t *testing.T) {
	svc := phonelessAgents{presence: agents.Presence{
		State: agents.StateNotReady, Reason: agents.ReasonLogin, ExtensionNumber: "1001",
	}}
	srv := New(config.Config{}, Deps{Agents: svc, AgentDir: staffedAgent{}})

	w := httptest.NewRecorder()
	srv.AgentReady(w, agentRequest(http.MethodPost, "/api/v1/agent/ready", ""))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body)
	}
	var body struct{ Error APIError }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != CodeDeviceNotRegistered {
		t.Errorf("code = %s, want DEVICE_NOT_REGISTERED — a client cannot tell "+
			"this from any other conflict without it", body.Error.Code)
	}
}

// The phone is part of presence on the wire, and deviceAccount is null rather
// than absent when there is no registration: a screen showing an extension
// number has to be able to say whether that number can actually ring.
func TestPresenceStatesWhoseRegistrationTheSwitchHolds(t *testing.T) {
	for _, tt := range []struct {
		name           string
		presence       agents.Presence
		wantRegistered bool
		wantAccount    string
	}{
		{
			name: "a registered phone names the account it is held for",
			presence: agents.Presence{
				State: agents.StateReady, ExtensionNumber: "1001",
				IsRegistered: true, IsDeviceInService: true,
			},
			wantRegistered: true,
			wantAccount:    "1001",
		},
		{
			name: "no registration, so no account",
			presence: agents.Presence{
				State: agents.StateNotReady, Reason: agents.ReasonDeviceLost,
				ExtensionNumber: "1001",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := phonelessAgents{presence: tt.presence}
			srv := New(config.Config{}, Deps{Agents: svc, AgentDir: staffedAgent{}})

			w := httptest.NewRecorder()
			srv.GetAgentPresence(w, agentRequest(http.MethodGet, "/api/v1/agent/presence", ""))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
			}

			var got api.Presence
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.IsDeviceRegistered != tt.wantRegistered {
				t.Errorf("isDeviceRegistered = %v, want %v",
					got.IsDeviceRegistered, tt.wantRegistered)
			}
			switch {
			case tt.wantAccount == "":
				if got.DeviceAccount != nil {
					t.Errorf("deviceAccount = %q, want null", *got.DeviceAccount)
				}
			case got.DeviceAccount == nil || *got.DeviceAccount != tt.wantAccount:
				t.Errorf("deviceAccount = %v, want %q", got.DeviceAccount, tt.wantAccount)
			}

			// Null, not absent. The generated type cannot tell the two apart,
			// so the raw body is what answers it.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"isDeviceRegistered", "deviceAccount"} {
				if _, ok := raw[field]; !ok {
					t.Errorf("%s is missing from the body; the contract requires it", field)
				}
			}
		})
	}
}
