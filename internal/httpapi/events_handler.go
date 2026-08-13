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

	"github.com/rasonyang/ai-native-callcenter/internal/auth"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
)

// heartbeatInterval keeps proxies and load balancers from closing an idle
// stream, and lets the browser notice a dead connection.
const heartbeatInterval = 15 * time.Second

// handleEvents serves the ordered event stream for one browser session.
//
// Scope comes from the authenticated identity, never from client parameters;
// ?types= may only narrow what that identity would already receive.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeSessionExpired, "no session", nil)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal, "streaming unsupported", nil)
		return
	}

	who := events.Subscriber{
		UserID:       id.UserID,
		IsSupervisor: id.Role.AtLeast(auth.RoleSupervisor),
		Types:        parseTypes(r.URL.Query().Get("types")),
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
					slog.WarnContext(ctx, "sse subscriber dropped for lagging", "userId", id.UserID)
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
