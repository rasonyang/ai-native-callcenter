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
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/agents"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// deviceStore is the smallest agents.Store that lets the real service run
// behind the real handlers: two agents, each bound to a phone.
type deviceStore struct {
	mu       sync.Mutex
	profiles map[uuid.UUID]agents.Profile
	numbers  map[uuid.UUID]string // extension id -> number
	presence map[uuid.UUID]agents.Presence
}

func (s *deviceStore) LoadPresence(_ context.Context, id uuid.UUID) (agents.Presence, error) {
	return s.presence[id], nil
}
func (s *deviceStore) SavePresence(_ context.Context, id uuid.UUID, p agents.Presence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presence[id] = p
	return nil
}
func (s *deviceStore) LogStateChange(context.Context, uuid.UUID, agents.Presence) error { return nil }
func (s *deviceStore) AgentProfile(_ context.Context, id uuid.UUID) (agents.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[id]
	if !ok {
		return agents.Profile{}, errors.New("no such agent")
	}
	return p, nil
}
func (s *deviceStore) AgentBoundTo(_ context.Context, number string) (uuid.UUID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.profiles {
		if p.ExtensionNumber == number {
			return id, true, nil
		}
	}
	return uuid.Nil, false, nil
}
func (s *deviceStore) Roster(context.Context) ([]agents.RosterEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agents.RosterEntry
	for id, prof := range s.profiles {
		p := s.presence[id]
		out = append(out, agents.RosterEntry{
			AgentID: id, DisplayName: prof.DisplayName, State: p.CurrentState(),
			Extension: p.ExtensionNumber, DefaultExtensionNumber: prof.ExtensionNumber,
		})
	}
	return out, nil
}
func (s *deviceStore) CreateAgent(context.Context, agents.AgentConfig) (agents.AgentConfig, error) {
	return agents.AgentConfig{}, errors.New("unused")
}
func (s *deviceStore) UpdateAgent(_ context.Context, cfg agents.AgentConfig) (agents.AgentConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prof := s.profiles[cfg.AgentID]
	prof.CallcenterName = cfg.CallcenterName
	prof.ExtensionID = cfg.DefaultExtensionID
	prof.ExtensionNumber = ""
	if cfg.DefaultExtensionID != nil {
		prof.ExtensionNumber = s.numbers[*cfg.DefaultExtensionID]
	}
	s.profiles[cfg.AgentID] = prof
	return cfg, nil
}
func (s *deviceStore) DeleteAgent(context.Context, uuid.UUID) error { return nil }

type streamRecorder struct {
	mu   sync.Mutex
	sent []events.Event
}

func (r *streamRecorder) Publish(_ context.Context, ev events.Event, _ events.Scope) events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, ev)
	return ev
}

func (r *streamRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

// lastFor is the latest event addressed to one agent, which is what that
// agent's stream last said.
func (r *streamRecorder) lastFor(agentID uuid.UUID) (events.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.sent) - 1; i >= 0; i-- {
		if r.sent[i].AgentID != nil && *r.sent[i].AgentID == agentID {
			return r.sent[i], true
		}
	}
	return events.Event{}, false
}

type deviceFloor struct {
	srv    *Server
	svc    *agents.Service
	store  *deviceStore
	stream *streamRecorder
	wei    uuid.UUID // bound to 1001
	ben    uuid.UUID // bound to 1002
	weiExt uuid.UUID
}

type noSwitch struct{}

func (noSwitch) AddCallcenterAgent(string) error                      { return nil }
func (noSwitch) SetCallcenterAgentContact(string, string, bool) error { return nil }
func (noSwitch) SetCallcenterAgentStatus(string, string) error        { return nil }
func (noSwitch) SetCallcenterAgentWrapUp(string, int) error           { return nil }
func (noSwitch) SetCallcenterAgentMaxNoAnswer(string, int) error      { return nil }
func (noSwitch) SetCallcenterAgentNoAnswerDelay(string, int) error    { return nil }
func (noSwitch) SetCallcenterAgentRejectDelay(string, int) error      { return nil }
func (noSwitch) SetCallcenterAgentBusyDelay(string, int) error        { return nil }
func (noSwitch) ClearCallcenterAgentHoldOff(string) error             { return nil }
func (noSwitch) IsUp() bool                                           { return false }

func newDeviceFloor(t *testing.T) *deviceFloor {
	t.Helper()
	f := &deviceFloor{
		store: &deviceStore{
			profiles: map[uuid.UUID]agents.Profile{}, numbers: map[uuid.UUID]string{},
			presence: map[uuid.UUID]agents.Presence{},
		},
		stream: &streamRecorder{}, wei: uuid.New(), ben: uuid.New(), weiExt: uuid.New(),
	}
	benExt := uuid.New()
	f.store.numbers[f.weiExt], f.store.numbers[benExt] = "1001", "1002"
	f.store.profiles[f.wei] = agents.Profile{AgentID: f.wei, CallcenterName: "wei", DisplayName: "Wei", ExtensionNumber: "1001", ExtensionID: &f.weiExt}
	f.store.profiles[f.ben] = agents.Profile{AgentID: f.ben, CallcenterName: "ben", DisplayName: "Ben", ExtensionNumber: "1002", ExtensionID: &benExt}
	f.svc = agents.NewService(f.store, noSwitch{}, f.stream)
	f.srv = New(config.Config{}, Deps{Agents: f.svc, AgentDir: staffedAgent{}})
	return f
}

func (f *deviceFloor) as(agentID uuid.UUID, method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(contextWithIdentity(r.Context(),
		auth.Identity{UserID: uuid.New(), Role: auth.RoleAgent}, agentID))
}

func (f *deviceFloor) login(agentID uuid.UUID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.srv.AgentLogin(w, f.as(agentID, http.MethodPost, "/api/v1/agent/login", body))
	return w
}

type phoneView struct {
	IsDeviceRegistered bool    `json:"isDeviceRegistered"`
	DeviceAccount      *string `json:"deviceAccount"`
}

func decodeView(t *testing.T, where string, raw []byte) phoneView {
	t.Helper()
	var v phoneView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v: %s", where, err, raw)
	}
	return v
}

// agree reads the phone through every surface and fails unless they say the
// same thing, and the thing they say is want.
func (f *deviceFloor) agree(t *testing.T, step string, agentID uuid.UUID, want bool) {
	t.Helper()
	views := map[string]phoneView{}

	w := httptest.NewRecorder()
	f.srv.GetAgentPresence(w, f.as(agentID, http.MethodGet, "/api/v1/agent/presence", ""))
	views["GET /agent/presence"] = decodeView(t, "presence", w.Body.Bytes())

	w = httptest.NewRecorder()
	f.srv.GetMe(w, f.as(agentID, http.MethodGet, "/api/v1/auth/me", ""))
	views["GET /auth/me"] = decodeView(t, "me", w.Body.Bytes())

	w = httptest.NewRecorder()
	f.srv.ListAgents(w, f.as(agentID, http.MethodGet, "/api/v1/agents", ""))
	var roster struct {
		Items []struct {
			AgentID      uuid.UUID `json:"agentId"`
			IsRegistered bool      `json:"isRegistered"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &roster); err != nil {
		t.Fatal(err)
	}
	for _, row := range roster.Items {
		if row.AgentID == agentID {
			views["GET /agents"] = phoneView{IsDeviceRegistered: row.IsRegistered}
			if row.IsRegistered {
				ext := f.store.profiles[agentID].ExtensionNumber
				views["GET /agents"] = phoneView{IsDeviceRegistered: true, DeviceAccount: &ext}
			}
		}
	}
	if last, ok := f.stream.lastFor(agentID); ok {
		// The last thing the agent's stream said about their own phone.
		p := last.Payload
		reg, _ := p["isDeviceRegistered"].(bool)
		v := phoneView{IsDeviceRegistered: reg}
		if a, ok := p["deviceAccount"].(string); ok {
			v.DeviceAccount = &a
		}
		views["last SSE payload"] = v
	}

	for name, v := range views {
		if v.IsDeviceRegistered != want {
			t.Errorf("%s: %s says isDeviceRegistered = %v, want %v", step, name, v.IsDeviceRegistered, want)
		}
		if v.IsDeviceRegistered != (v.DeviceAccount != nil) {
			t.Errorf("%s: %s: isDeviceRegistered %v with deviceAccount %v", step, name, v.IsDeviceRegistered, v.DeviceAccount)
		}
	}
}

func TestSignInRefusesAnyRequestThatNamesAnExtension(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"extensionNumber":"1001"}`, // the agent's own
		`{"extensionNumber":"1002"}`, // a colleague's
		`{"foo":1}`,
	} {
		f := newDeviceFloor(t)
		w := f.login(f.wei, body)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", body, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), string(CodeValidationFailed)) {
			t.Errorf("%s: body %s does not carry %s", body, w.Body, CodeValidationFailed)
		}
		if !f.svc.Presence(f.wei).IsLoggedOut() {
			t.Errorf("%s: presence moved", body)
		}
		if n := f.stream.count(); n != 0 {
			t.Errorf("%s: %d events published", body, n)
		}
	}
}

func TestSignInWithAnEmptyBodyOrNoBodyLandsAtTheBoundExtension(t *testing.T) {
	t.Parallel()
	for _, body := range []string{``, `{}`, `null`} {
		f := newDeviceFloor(t)
		w := f.login(f.wei, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status = %d: %s", body, w.Code, w.Body)
		}
		if got := f.svc.Presence(f.wei).ExtensionNumber; got != "1001" {
			t.Errorf("%q: signed in at %q, want the bound 1001", body, got)
		}
	}
}

func TestSignInWithNoBindingIsAConflict(t *testing.T) {
	t.Parallel()
	f := newDeviceFloor(t)
	drifter := uuid.New()
	f.store.profiles[drifter] = agents.Profile{AgentID: drifter, DisplayName: "Dee"}
	if w := f.login(drifter, `{}`); w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409: %s", w.Code, w.Body)
	}
}

func TestRebindingASignedInAgentIsAConflictThatChangesNothing(t *testing.T) {
	t.Parallel()
	f := newDeviceFloor(t)
	if w := f.login(f.wei, ``); w.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %s", w.Code, w.Body)
	}
	told := f.stream.count()
	other := uuid.New()
	f.store.numbers[other] = "1009"

	for name, body := range map[string]string{
		"another extension": fmt.Sprintf(`{"callcenterName":"wei","defaultExtensionId":%q}`, other),
		"cleared":           `{"callcenterName":"wei"}`,
	} {
		w := httptest.NewRecorder()
		f.srv.UpdateAgent(w, f.as(f.wei, http.MethodPut, "/api/v1/agents/"+f.wei.String(), body), f.wei)
		if w.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409: %s", name, w.Code, w.Body)
		}
		if got := f.store.profiles[f.wei]; got.ExtensionNumber != "1001" || *got.ExtensionID != f.weiExt {
			t.Errorf("%s: binding changed: %+v", name, got)
		}
		if p := f.svc.Presence(f.wei); p.IsLoggedOut() || p.ExtensionNumber != "1001" {
			t.Errorf("%s: presence changed: %+v", name, p)
		}
	}
	if f.stream.count() != told {
		t.Error("a refused rebind published an event")
	}

	// Renaming without moving the phone is not a rebind.
	w := httptest.NewRecorder()
	body := fmt.Sprintf(`{"callcenterName":"wei2","defaultExtensionId":%q}`, f.weiExt)
	f.srv.UpdateAgent(w, f.as(f.wei, http.MethodPut, "/api/v1/agents/"+f.wei.String(), body), f.wei)
	if w.Code != http.StatusOK {
		t.Errorf("rename: status = %d, want 200: %s", w.Code, w.Body)
	}
}

// The phone is read through four surfaces; after every step they agree.
func TestEverySurfaceAgreesOnWhetherThePhoneIsRegistered(t *testing.T) {
	t.Parallel()
	f := newDeviceFloor(t)
	ctx := context.Background()

	f.agree(t, "before any signal", f.wei, false)

	f.svc.ObserveDevice(ctx, "1001", agents.SignalRegistered)
	f.agree(t, "registered while signed out", f.wei, true)
	f.agree(t, "a colleague's phone is not theirs", f.ben, false)

	if w := f.login(f.wei, `{}`); w.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %s", w.Code, w.Body)
	}
	f.agree(t, "signed in", f.wei, true)

	f.svc.ObserveDevice(ctx, "1001", agents.SignalUnreachable)
	f.agree(t, "registered but not answering", f.wei, true)

	f.svc.ObserveDevice(ctx, "1001", agents.SignalUnregistered)
	f.agree(t, "unregistered while signed in", f.wei, false)

	w := httptest.NewRecorder()
	f.srv.AgentLogout(w, f.as(f.wei, http.MethodPost, "/api/v1/agent/logout", ""))
	f.agree(t, "signed out, phone gone", f.wei, false)

	f.svc.ObserveDevice(ctx, "1001", agents.SignalRegistered)
	f.agree(t, "registered again while signed out", f.wei, true)
	if got, _ := f.stream.lastFor(f.wei); got.Type != events.TypeDeviceRegistered {
		t.Errorf("last event = %s, want DEVICE_REGISTERED", got.Type)
	}
}
