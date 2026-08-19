// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// ListCDRs pages the ledger with filters.
func (s *Server) ListCDRs(w http.ResponseWriter, r *http.Request, params api.ListCDRsParams) {
	filter := store.CDRFilter{
		Status:     stringOr(params.Status),
		DID:        stringOr(params.DID),
		FromNumber: stringOr(params.FromNumber),
		QueueID:    params.QueueID,
		AgentID:    params.AgentID,
		Limit:      intOr(params.Limit, 50),
		Offset:     intOr(params.Offset, 0),
	}
	if params.From != nil {
		filter.From = *params.From
	}
	if params.To != nil {
		filter.To = *params.To
	}

	items, total, err := s.ledger.ListCDRs(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list calls", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// ListMyCDRs pages the caller's own finished calls.
//
// The agent identity comes from the session, never from a parameter: "my
// calls" that took an agentId would be every agent's calls to anyone who can
// type a UUID. The wrap-up attached to each row is the caller's own filing for
// that call, not whatever a colleague wrote.
func (s *Server) ListMyCDRs(w http.ResponseWriter, r *http.Request, params api.ListMyCDRsParams) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	filter := store.CDRFilter{
		AgentID:    &agentID,
		Status:     stringOr(params.Status),
		FromNumber: stringOr(params.FromNumber),
		Limit:      intOr(params.Limit, 50),
		Offset:     intOr(params.Offset, 0),
	}
	if params.From != nil {
		filter.From = *params.From
	}
	if params.To != nil {
		filter.To = *params.To
	}

	items, total, err := s.ledger.ListCDRs(r.Context(), filter)
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot list an agent's calls", "error", err, "agentId", agentID)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list calls", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// ListDispositions serves the after-call-work vocabulary.
func (s *Server) ListDispositions(w http.ResponseWriter, r *http.Request) {
	categories, err := s.ledger.ListDispositions(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot read the disposition vocabulary", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list dispositions", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": categories})
}

// GetCDR returns one finished call with everything it left behind.
func (s *Server) GetCDR(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
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

// ListCallbacks pages the work queue.
func (s *Server) ListCallbacks(w http.ResponseWriter, r *http.Request, params api.ListCallbacksParams) {
	status := ""
	if params.Status != nil {
		status = string(*params.Status)
	}
	items, err := s.ledger.ListCallbacks(r.Context(), status,
		intOr(params.Limit, 50), intOr(params.Offset, 0))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list callbacks", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ClaimCallback marks a callback as being worked by the caller.
func (s *Server) ClaimCallback(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
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

// CompleteCallback closes a callback as kept or dismissed.
func (s *Server) CompleteCallback(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
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
		// A callback belongs on every screen that can act on one.
	}, events.Scope{IsBroadcast: true})
}

//
// Reports.
//

// reportPeriod resolves the from/to window, defaulting to today.
func reportPeriod(from, to *time.Time) (time.Time, time.Time) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.AddDate(0, 0, 1)
	if from != nil {
		start = *from
	}
	if to != nil {
		end = *to
	}
	return start, end
}

func (s *Server) GetReportOverview(w http.ResponseWriter, r *http.Request, params api.GetReportOverviewParams) {
	from, to := reportPeriod(params.From, params.To)
	overview, err := s.ledger.ReportOverview(r.Context(), from, to, params.QueueID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot aggregate", nil)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) GetReportQueues(w http.ResponseWriter, r *http.Request, params api.GetReportQueuesParams) {
	from, to := reportPeriod(params.From, params.To)
	items, err := s.ledger.ReportByQueue(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot aggregate", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) GetReportDaily(w http.ResponseWriter, r *http.Request, params api.GetReportDailyParams) {
	from, to := reportPeriod(params.From, params.To)
	items, err := s.ledger.ReportDaily(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot aggregate", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// intOr bounds an optional count. A missing, negative or absurd value falls
// back to the default, which is what a page size is for.
func intOr(value *int, def int) int {
	if value == nil || *value < 0 || *value > 1_000_000 {
		return def
	}
	return *value
}

// stringOr reads an optional filter, where absent and empty mean the same.
func stringOr[T ~string](value *T) string {
	if value == nil {
		return ""
	}
	return string(*value)
}
