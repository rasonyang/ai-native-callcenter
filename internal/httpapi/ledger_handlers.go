// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// handleListCDRs pages the ledger with filters.
func (s *Server) handleListCDRs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.CDRFilter{
		Status:     q.Get("status"),
		DID:        q.Get("did"),
		FromNumber: q.Get("fromNumber"),
		Limit:      intParam(q.Get("limit"), 50),
		Offset:     intParam(q.Get("offset"), 0),
	}
	if from, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		filter.From = from
	}
	if to, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		filter.To = to
	}
	if id, err := uuid.Parse(q.Get("queueId")); err == nil {
		filter.QueueID = &id
	}
	if id, err := uuid.Parse(q.Get("agentId")); err == nil {
		filter.AgentID = &id
	}

	items, total, err := s.ledger.ListCDRs(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list calls", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// handleGetCDR returns one finished call with everything it left behind.
func (s *Server) handleGetCDR(w http.ResponseWriter, r *http.Request) {
	callID, ok := pathID(w, r, "callId")
	if !ok {
		return
	}
	cdr, err := s.ledger.GetCDR(r.Context(), callID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such call", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the call", nil)
		return
	}

	// The detail view wants the whole story in one round trip.
	transcript, err := s.ledger.ListTranscript(r.Context(), callID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the transcript", nil)
		return
	}
	recordings, err := s.ledger.RecordingsByCall(r.Context(), callID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list recordings", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cdr":        cdr,
		"transcript": transcript,
		"recordings": recordings,
	})
}

//
// Callbacks: the loop from a promise to its keeping.
//

// handleListCallbacks pages the work queue.
func (s *Server) handleListCallbacks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, err := s.ledger.ListCallbacks(r.Context(), q.Get("status"),
		intParam(q.Get("limit"), 50), intParam(q.Get("offset"), 0))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list callbacks", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleClaimCallback marks a callback as being worked by the caller.
func (s *Server) handleClaimCallback(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "callbackId")
	if !ok {
		return
	}
	identity, _ := identityFrom(r.Context())

	callback, err := s.ledger.ClaimCallback(r.Context(), id, identity.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either it does not exist or someone claimed it first; to the
			// caller both mean the same thing — it is not theirs to work.
			writeError(w, http.StatusConflict, CodeConflict, "the callback is not open", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot claim the callback", nil)
		return
	}
	s.publishCallback(r, events.TypeCallbackUpdated, callback)
	writeJSON(w, http.StatusOK, callback)
}

// completeCallbackRequest closes a callback.
type completeCallbackRequest struct {
	// Status is DONE or DISMISSED.
	Status string `json:"status"`
}

// handleCompleteCallback closes a callback as kept or dismissed.
func (s *Server) handleCompleteCallback(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "callbackId")
	if !ok {
		return
	}
	var req completeCallbackRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Status != store.CallbackStatusDone && req.Status != store.CallbackStatusDismissed {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "status must be DONE or DISMISSED",
			map[string]any{"allowed": []string{store.CallbackStatusDone, store.CallbackStatusDismissed}})
		return
	}

	identity, _ := identityFrom(r.Context())
	callback, err := s.ledger.HandleCallback(r.Context(), id, req.Status, identity.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, CodeNotFound, "no such callback", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot update the callback", nil)
		return
	}
	s.publishCallback(r, events.TypeCallbackUpdated, callback)
	writeJSON(w, http.StatusOK, callback)
}

// publishCallback announces a callback movement to every signed-in screen.
func (s *Server) publishCallback(r *http.Request, eventType events.Type, callback store.Callback) {
	if s.hub == nil {
		return
	}
	s.hub.Publish(r.Context(), events.Event{
		Type:    eventType,
		CallID:  callback.CallID,
		Payload: map[string]any{"callback": callback},
	}, events.Scope{})
}

//
// Reports.
//

// reportPeriod reads the from/to window, defaulting to today.
func reportPeriod(r *http.Request) (time.Time, time.Time) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	to := from.AddDate(0, 0, 1)

	q := r.URL.Query()
	if parsed, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		from = parsed
	}
	if parsed, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		to = parsed
	}
	return from, to
}

func (s *Server) handleReportOverview(w http.ResponseWriter, r *http.Request) {
	from, to := reportPeriod(r)
	var queueID *uuid.UUID
	if id, err := uuid.Parse(r.URL.Query().Get("queueId")); err == nil {
		queueID = &id
	}
	overview, err := s.ledger.ReportOverview(r.Context(), from, to, queueID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot aggregate", nil)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) handleReportQueues(w http.ResponseWriter, r *http.Request) {
	from, to := reportPeriod(r)
	items, err := s.ledger.ReportByQueue(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot aggregate", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleReportDaily(w http.ResponseWriter, r *http.Request) {
	from, to := reportPeriod(r)
	items, err := s.ledger.ReportDaily(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot aggregate", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func intParam(raw string, def int) int {
	if raw == "" {
		return def
	}
	n := 0
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
		if n > 1_000_000 {
			return def
		}
	}
	return n
}
