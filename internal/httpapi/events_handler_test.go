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

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
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

	srv := New(config.Config{SessionCookie: "aicc_session"}, Deps{Hub: hub})

	ctx, cancel := context.WithCancel(contextWithIdentity(context.Background(), id))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	if lastEventID != "" {
		r.Header.Set("Last-Event-ID", lastEventID)
	}
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.apiWrapper().StreamEvents(w, r)
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

	// An agent must not receive a supervisor-only event.
	body := serveEvents(t, hub, auth.Identity{Role: auth.RoleAgent}, "", func() {
		hub.Publish(context.Background(), events.Event{Type: events.TypeQueueCount},
			events.Scope{SupervisorOnly: true})
	})
	if strings.Contains(body, "QUEUE_COUNT") {
		t.Errorf("agent received a supervisor-only event:\n%s", body)
	}

	// The same event reaches a supervisor.
	body = serveEvents(t, hub, auth.Identity{Role: auth.RoleSupervisor}, "", func() {
		hub.Publish(context.Background(), events.Event{Type: events.TypeQueueCount},
			events.Scope{SupervisorOnly: true})
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
