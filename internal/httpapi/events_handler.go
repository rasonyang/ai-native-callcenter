// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// heartbeatInterval keeps proxies and load balancers from closing an idle
// stream, and lets the browser notice a dead connection.
const heartbeatInterval = 15 * time.Second

// StreamEvents serves the ordered event stream for one browser session.
//
// Scope comes from the authenticated identity, never from client parameters;
// ?types= may only narrow what that identity would already receive.
//
// This is the one operation that reads its own parameters rather than taking
// them from the generated wrapper. The wrapper rejects a resume point it
// cannot parse, and an EventSource retries a failed request forever with the
// same header: a mangled Last-Event-ID would become a reconnect loop instead
// of a stream that simply starts fresh. Degrading is the safer failure here,
// so the leniency below is deliberate.
func (s *Server) StreamEvents(w http.ResponseWriter, r *http.Request, _ api.StreamEventsParams) {
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "streaming unsupported", nil)
		return
	}

	// calls:read:own is what the contract asks for to open the stream at all;
	// calls:read:all widens delivery from this subject's own leg events to
	// every call on the floor. The hub is told the resolved answer and never
	// the scope name — a leg event is private to its agent, and which subject
	// that rule exempts is this package's decision to make.
	who := events.Subscriber{
		UserID:        ac.SubjectID,
		SeesEveryCall: ac.Has(api.ScopeCallsReadAll),
		Types:         parseTypes(r.URL.Query().Get("types")),
	}
	// A subject's own call and queue events are scoped to their agent
	// identity; without it the cockpit would receive nothing at all. Somebody
	// with no agent identity needs none — either they see everything, or they
	// have no leg for an event to be about.
	if ac.IsAgent() {
		agentID := ac.AgentID
		who.AgentID = &agentID
		if s.agentDir != nil {
			if queueIDs, err := s.agentDir.QueuesForAgent(r, agentID); err == nil {
				who.QueueIDs = queueIDs
			} else {
				slog.WarnContext(r.Context(), "cannot resolve staffed queues",
					"error", err, "agentId", agentID)
			}
		}
	}

	sub, replay, resetNeeded := s.hub.Subscribe(who, parseLastEventID(r))
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if resetNeeded {
		// The resume point aged out of the ring: tell the client to resync
		// from collection snapshots before tailing again.
		writeEvent(w, events.Event{
			Version:    events.Version,
			Type:       events.TypeSystemReset,
			OccurredAt: time.Now().UTC(),
			Payload:    map[string]any{"oldestSeq": s.hub.OldestSeq()},
		})
		flusher.Flush()
	}
	for _, ev := range replay {
		writeEvent(w, ev)
	}
	flusher.Flush()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-sub.C:
			if !open {
				// Dropped for lagging: end the response so the browser
				// reconnects and resyncs.
				if sub.Dropped {
					slog.WarnContext(ctx, "sse subscriber dropped for lagging", "subjectId", ac.SubjectID, "subjectKind", ac.Kind)
				}
				return
			}
			writeEvent(w, ev)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": hb\n\n")
			flusher.Flush()
		}
	}
}

// writeEvent renders one envelope in SSE framing. The id: line carries the
// global seq, which the browser echoes back as Last-Event-ID on reconnect.
func writeEvent(w http.ResponseWriter, ev events.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		slog.Error("marshal event", "error", err, "type", ev.Type)
		return
	}
	if ev.Seq > 0 {
		fmt.Fprintf(w, "id: %d\n", ev.Seq)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
}

// parseLastEventID is where the client wants the stream to continue. The
// browser echoes the id: line back in the Last-Event-ID header; the query
// parameter exists for clients that cannot set one. Anything unreadable means
// "from now" rather than an error, for the reason StreamEvents gives.
func parseLastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func parseTypes(raw string) []events.Type {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]events.Type, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, events.Type(p))
		}
	}
	return out
}
