// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type stubReserver struct{ n int64 }

func (s *stubReserver) ReserveSeqBlock(context.Context, string, int64) (int64, error) {
	s.n += 100_000
	return s.n, nil
}

// serveEvents runs the stream through the generated wrapper until it produces
// the wanted number of
// data frames or the deadline passes, and returns the raw response body.
func serveEvents(t *testing.T, hub *events.Hub, id auth.Identity, lastEventID string, publish func()) string {
	t.Helper()
	return serveEventsWith(t, hub, id, nil, lastEventID, publish)
}

// serveEventsAs is serveEvents for a session that resolves to an agent, which
// is what decides whether an agent-scoped event reaches it at all.
func serveEventsAs(t *testing.T, hub *events.Hub, id auth.Identity, dir AgentDirectory, publish func()) string {
	t.Helper()
	return serveEventsWith(t, hub, id, dir, "", publish)
}

func serveEventsWith(t *testing.T, hub *events.Hub, id auth.Identity, dir AgentDirectory,
	lastEventID string, publish func()) string {
	t.Helper()

	srv := New(config.Config{SessionCookie: "aicc_session"}, Deps{Hub: hub, AgentDir: dir})

	// The agent identity the authentication middleware would have resolved
	// from this account's binding — the same directory the real one asks.
	agentID := []uuid.UUID{}
	if dir != nil {
		if resolved, err := dir.AgentIDForUser(
			httptest.NewRequest(http.MethodGet, "/", nil), id.UserID); err == nil {
			agentID = append(agentID, resolved)
		}
	}
	ctx, cancel := context.WithCancel(contextWithIdentity(context.Background(), id, agentID...))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	if lastEventID != "" {
		r.Header.Set("Last-Event-ID", lastEventID)
	}
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Called the way it is mounted: by hand, outside the generated
		// wrapper, because an EventSource must not be answered with a
		// rejected parameter.
		srv.StreamEvents(w, r, api.StreamEventsParams{})
	}()

	// Give the handler time to subscribe before publishing, then to write.
	time.Sleep(50 * time.Millisecond)
	if publish != nil {
		publish()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream did not return after the request context was cancelled")
	}
	return w.Body.String()
}

func TestEventStreamHeadersAndFraming(t *testing.T) {
	hub := events.NewHub(events.NewSequence(&stubReserver{}, "events"))
	callID := uuid.New()

	body := serveEvents(t, hub, auth.Identity{Role: auth.RoleSupervisor}, "", func() {
		hub.Publish(context.Background(), events.Event{
			Type:     events.TypePartyRinging,
			CallID:   &callID,
			CallType: events.CallTypeInbound,
			UserData: map[string]any{"ticketId": "T-42"},
		}, events.Scope{})
	})

	for _, want := range []string{
		"event: PARTY_RINGING",
		`"type":"PARTY_RINGING"`,
		`"callType":"INBOUND"`,
		`"ticketId":"T-42"`,
		"id: ",
		"data: ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q\ngot:\n%s", want, body)
		}
	}
}

func TestEventStreamScopesByIdentity(t *testing.T) {
	hub := events.NewHub(events.NewSequence(&stubReserver{}, "events"))

	// An event whose audience nobody set reaches supervisors and no agent.
	body := serveEvents(t, hub, auth.Identity{Role: auth.RoleAgent}, "", func() {
		hub.Publish(context.Background(), events.Event{Type: events.TypeQueueCount},
			events.Scope{})
	})
	if strings.Contains(body, "QUEUE_COUNT") {
		t.Errorf("agent received an event addressed to nobody:\n%s", body)
	}

	// The same event reaches a supervisor.
	body = serveEvents(t, hub, auth.Identity{Role: auth.RoleSupervisor}, "", func() {
		hub.Publish(context.Background(), events.Event{Type: events.TypeQueueCount},
			events.Scope{})
	})
	if !strings.Contains(body, "QUEUE_COUNT") {
		t.Errorf("supervisor did not receive the event:\n%s", body)
	}
}

func TestEventStreamEmitsResetWhenResumePointIsGone(t *testing.T) {
	hub := events.NewHub(events.NewSequence(&stubReserver{}, "events"))

	body := serveEvents(t, hub, auth.Identity{Role: auth.RoleSupervisor}, "999999", nil)
	if !strings.Contains(body, "event: SYSTEM_RESET") {
		t.Errorf("stream did not tell the client to resync:\n%s", body)
	}
	if !strings.Contains(body, `"oldestSeq"`) {
		t.Errorf("SYSTEM_RESET lacks oldestSeq:\n%s", body)
	}
}

func TestEventStreamReplaysAfterLastEventID(t *testing.T) {
	hub := events.NewHub(events.NewSequence(&stubReserver{}, "events"))
	ctx := context.Background()

	first := hub.Publish(ctx, events.Event{Type: events.TypeAgentReady}, events.Scope{})
	hub.Publish(ctx, events.Event{Type: events.TypeAgentNotReady}, events.Scope{})

	body := serveEvents(t, hub, auth.Identity{Role: auth.RoleSupervisor},
		strconv.FormatInt(first.Seq, 10), nil)

	if strings.Contains(body, "AGENT_READY") {
		t.Errorf("replayed an event the client already had:\n%s", body)
	}
	if !strings.Contains(body, "AGENT_NOT_READY") {
		t.Errorf("did not replay the missed event:\n%s", body)
	}
}

// The stream and the REST handler must answer the same question the same way.
// mayReadTranscript lets a supervisor read any call and an agent only the ones
// they are on; this is that rule on the stream, end to end through the real hub
// rather than through the scope predicate alone.
//
// The bot phase is the case that was wrong: with no agent on the call the
// transcript actor publishes an empty scope, which used to reach every agent
// in the building.
func TestTranscriptOnTheStreamMatchesWhoMayReadItOverREST(t *testing.T) {
	onTheCall, elsewhere := uuid.New(), uuid.New()
	botPhase := events.Event{Type: events.TypeCallTranscript,
		Payload: map[string]any{"text": "thanks for calling", "speaker": "BOT"}}
	humanPhase := events.Event{Type: events.TypeCallTranscript,
		Payload: map[string]any{"text": "my card number is", "speaker": "CUSTOMER"}}

	for _, tc := range []struct {
		name      string
		role      auth.Role
		agentID   *uuid.UUID
		wantBot   bool
		wantHuman bool
	}{
		{name: "the agent on the call", role: auth.RoleAgent, agentID: &onTheCall,
			wantBot: false, wantHuman: true},
		{name: "an agent on another call", role: auth.RoleAgent, agentID: &elsewhere,
			wantBot: false, wantHuman: false},
		{name: "a supervisor", role: auth.RoleSupervisor,
			wantBot: true, wantHuman: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := events.NewHub(events.NewSequence(&stubReserver{}, "events"))
			id := auth.Identity{Role: tc.role}
			var dir AgentDirectory
			if tc.agentID != nil {
				dir = fixedAgent{*tc.agentID}
			}
			body := serveEventsAs(t, hub, id, dir, func() {
				hub.Publish(context.Background(), botPhase, events.Scope{})
				hub.Publish(context.Background(), humanPhase,
					events.Scope{AgentIDs: []uuid.UUID{onTheCall}})
			})
			if got := strings.Contains(body, "thanks for calling"); got != tc.wantBot {
				t.Errorf("saw the bot phase = %v, want %v", got, tc.wantBot)
			}
			if got := strings.Contains(body, "my card number is"); got != tc.wantHuman {
				t.Errorf("saw the human phase = %v, want %v", got, tc.wantHuman)
			}
		})
	}
}

// fixedAgent resolves every session to one agent, which is what the real
// directory does per user.
type fixedAgent struct{ id uuid.UUID }

func (f fixedAgent) AgentIDForUser(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return f.id, nil
}
func (fixedAgent) QueuesForAgent(*http.Request, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// A callback is one of the few things that is genuinely everybody's: it
// appears on every screen that can act on one. Under a default-deny hub that
// has to be said, and this is what says it — the scope literal at the publish
// site, not the hub's willingness to deliver.
func TestACallbackReachesAnAgentWhoIsOnNothing(t *testing.T) {
	hub := events.NewHub(events.NewSequence(&stubReserver{}, "events"))
	srv := New(config.Config{SessionCookie: "aicc_session"}, Deps{Hub: hub})

	agentID := uuid.New()
	sub, _, _ := hub.Subscribe(events.Subscriber{UserID: uuid.New(), AgentID: &agentID}, 0)
	defer sub.Close()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.publishCallback(r, events.TypeCallbackCreated, store.Callback{})

	select {
	case ev := <-sub.C:
		if ev.Type != events.TypeCallbackCreated {
			t.Errorf("received %s, want CALLBACK_CREATED", ev.Type)
		}
	case <-time.After(time.Second):
		t.Error("an agent on no call never saw the callback")
	}
}

func (f fixedAgent) UserIDForAgent(*http.Request, uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), nil
}
