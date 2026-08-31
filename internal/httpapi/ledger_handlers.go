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
	items, err := s.ledger.ListDispositions(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot read the disposition vocabulary", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot list dispositions", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetMyDay serves the caller's own numbers for the day.
//
// The agent is the session. Somebody else's day is supervision, and the
// aggregates for that live under /reports with a supervisor's guard on them.
func (s *Server) GetMyDay(w http.ResponseWriter, r *http.Request, params api.GetMyDayParams) {
	agentID, ok := s.agentIDFor(w, r)
	if !ok {
		return
	}
	// Today, unless asked otherwise: from local midnight to now rather than to
	// midnight, because occupancy over a day that has not happened yet is not
	// a number anybody wants to read.
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	to := now
	if params.From != nil {
		from = *params.From
	}
	if params.To != nil {
		to = *params.To
	}

	day, err := s.ledger.ReportAgentDay(r.Context(), agentID, from, to)
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot aggregate an agent's day", "error", err, "agentId", agentID)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot aggregate", nil)
		return
	}
	writeJSON(w, http.StatusOK, day)
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
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	// handled_by has always held a user id and still does. A key working as
	// an agent writes that agent's person, so a promise is kept by somebody a
	// colleague can go and ask — that a key placed the request is the audit
	// trail's business, not this column's (ruling 2).
	if !ac.IsActingForAPerson() {
		writeError(w, http.StatusForbidden, CodeAgentRequired,
			"a callback is claimed by a person; name the agent this key works for", nil)
		return
	}
	callback, err := s.ledger.ClaimCallback(r.Context(), id, ac.ActorUserID)
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

// ReleaseCallback hands a claimed callback back to the pool.
func (s *Server) ReleaseCallback(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if !ac.IsActingForAPerson() {
		writeError(w, http.StatusForbidden, CodeAgentRequired,
			"a callback is released by the person holding it", nil)
		return
	}
	callback, err := s.ledger.ReleaseCallback(r.Context(), id, ac.ActorUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not claimed, or claimed by somebody else: either way it is not
			// this caller's to let go of.
			writeError(w, http.StatusConflict, CodeConflict, "the callback is not yours to release", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot release the callback", nil)
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

	ac, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if !ac.IsActingForAPerson() {
		writeError(w, http.StatusForbidden, CodeAgentRequired,
			"a callback is completed by the person holding it", nil)
		return
	}
	callback, err := s.ledger.HandleCallback(r.Context(), id, req.Status, ac.ActorUserID)
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

// ListCallQueueEvents answers how routing behaved on one call.
//
// The CDR says how a call ended. It cannot say how it got there: it holds one
// queue id where a caller may have crossed several, and one missed reason
// where the caller may have been offered to a dozen agents in turn. Those
// movements have been written since the beginning and read by nothing (C7),
// so the questions they answer had no answer — and one of them is open.
// Seven calls in this database were offered between ten and thirty-three
// times; the worst reads ABANDONED_WAITING, 220 seconds, one agent id.
//
// Supervision rather than an agent's own view, and deliberately so: these
// rows name the colleagues a call was offered to and who did not take it,
// which is exactly the kind of thing an agent's own stream is kept clear of.
func (s *Server) ListCallQueueEvents(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the queue events", nil)
		return
	}
	rows, err := s.ledger.QueueEventsByCall(r.Context(), callID)
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot read a call's queue events",
			"callId", callID, "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the queue events", nil)
		return
	}

	// Empty is an answer, not an absence: a call that never entered a queue
	// has no journey, and saying so is different from failing to look.
	items := make([]api.QueueEvent, 0, len(rows))
	for _, row := range rows {
		items = append(items, api.QueueEvent{
			OccurredAt: row.OccurredAt,
			QueueID:    row.QueueID,
			Event:      api.QueueEventName(row.Event),
			AgentID:    row.AgentID,
			WaitMs:     int32(row.WaitMs),
		})
	}
	writeJSON(w, http.StatusOK, api.QueueEventList{Items: items})
}

// ListAuditLogs pages the audit trail.
//
// The table has been written since the beginning and read by nothing (W4/D5):
// every mutating request that succeeded is in it, recorded by middleware rather
// than by each handler, and until now the only way to see any of it was psql.
// A trail nobody can read is not an answer to "who changed this", it is only a
// promise that the answer exists somewhere.
//
// ADMIN only. The trail names accounts and carries what their requests
// contained, which is a wider view than supervision needs.
func (s *Server) ListAuditLogs(w http.ResponseWriter, r *http.Request, params api.ListAuditLogsParams) {
	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the audit trail", nil)
		return
	}
	filter := store.AuditFilter{
		ActorID:      params.ActorID,
		ActionPrefix: stringOr(params.ActionPrefix),
		Limit:        intOr(params.Limit, 50),
		Offset:       intOr(params.Offset, 0),
	}
	if params.From != nil {
		filter.From = *params.From
	}
	if params.To != nil {
		filter.To = *params.To
	}

	rows, total, err := s.ledger.ListAuditLogs(r.Context(), filter)
	if err != nil {
		slog.ErrorContext(r.Context(), "cannot read the audit trail", "error", err)
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the audit trail", nil)
		return
	}

	items := make([]api.AuditEntry, 0, len(rows))
	for _, row := range rows {
		entry := api.AuditEntry{
			AuditID:    row.ID,
			OccurredAt: row.OccurredAt,
			ActorID:    row.ActorID,
			Action:     row.Action,
			TargetKind: row.TargetKind,
			TargetID:   row.TargetID,
			Detail:     row.Detail,
		}
		// Absent rather than empty where there is nothing to say: a deleted
		// account has no name to resolve, and a request whose peer could not
		// be determined has no address.
		if row.ActorUsername != "" {
			name := row.ActorUsername
			entry.ActorUsername = &name
		}
		if row.IP != "" {
			ip := row.IP
			entry.IP = &ip
		}
		items = append(items, entry)
	}
	writeJSON(w, http.StatusOK, api.AuditEntryList{Items: items, Total: int(total)})
}
